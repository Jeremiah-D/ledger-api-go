package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func int64Ptr(v int64) *int64        { return &v }
func stringPtr(v string) *string     { return &v }
func timePtr(v time.Time) *time.Time { return &v }

// Amending the amount before acceptance changes what the debtor pays:
// the payout posts at the new amount, not the issued one.
func TestAmendRfPAmountThenAcceptPaysNewAmount(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	v0 := l.Version()
	testRfP(ctx, t, l, "rfp-1")

	amended, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{AmountCents: int64Ptr(7500)}, "merchant")
	if err != nil {
		t.Fatalf("AmendRfP: %v", err)
	}
	if amended.AmountCents != 7500 {
		t.Fatalf("amended amount = %d, want 7500", amended.AmountCents)
	}
	if amended.Status != RfPStatusIssued || amended.ID != "rfp-1" || amended.Creditor != "merchant" || amended.Debtor != "customer" {
		t.Fatalf("amend touched identity/status: %+v", amended)
	}
	if l.Version() != v0 {
		t.Fatal("AmendRfP bumped the version")
	}

	receipt, err := l.AcceptRfP(ctx, "rfp-1", "customer")
	if err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	paid := false
	for _, e := range receipt.Entries {
		if e.AmountCents == 7500 {
			paid = true
		}
	}
	if !paid {
		t.Fatalf("no payout entry of 7500 in receipt: %+v", receipt.Entries)
	}
	if l.BalanceIn("customer", "USD") != 2500 || l.BalanceIn("merchant", "USD") != 7500 {
		t.Fatalf("balances: customer=%d merchant=%d, want 2500/7500", l.BalanceIn("customer", "USD"), l.BalanceIn("merchant", "USD"))
	}
}

// A currency amendment to another valid code takes effect; a
// lowercase code is rejected, exactly like at creation —
// normalizeCurrency folds nothing, it validates 3 uppercase letters.
func TestAmendRfPCurrencyChanges(t *testing.T) {
	ctx := context.Background()
	l := New()
	testRfP(ctx, t, l, "rfp-1")

	amended, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{Currency: stringPtr("EUR")}, "")
	if err != nil {
		t.Fatalf("AmendRfP: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Fatalf("amended currency = %q, want EUR", amended.Currency)
	}
	if got, _ := l.GetRfP("rfp-1"); got.Currency != "EUR" {
		t.Fatalf("stored currency = %q, want EUR", got.Currency)
	}
	if _, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{Currency: stringPtr("eur")}, ""); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("AmendRfP(lowercase currency): got %v, want ErrInvalidCurrency", err)
	}
	if got, _ := l.GetRfP("rfp-1"); got.Currency != "EUR" {
		t.Fatalf("live currency moved on rejected amend: %q, want EUR", got.Currency)
	}
}

// Extending the expiry lets the debtor accept after the original
// deadline: without the amendment the same accept would fail expired.
func TestAmendRfPExpiryExtensionAcceptAfterOldExpiry(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	now := time.Now()
	if _, _, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-short", Creditor: "merchant", Debtor: "customer",
		AmountCents: 5000, Currency: "USD", ExpiresAt: now.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}
	if _, err := l.AmendRfP(ctx, "rfp-short", RfPAmendment{ExpiresAt: timePtr(now.Add(time.Hour))}, ""); err != nil {
		t.Fatalf("AmendRfP: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := l.AcceptRfP(ctx, "rfp-short", "customer"); err != nil {
		t.Fatalf("AcceptRfP after old expiry: %v, want success after extension", err)
	}
}

// A paid or rejected request no longer accepts amendments: the terms
// the debtor answered to are the terms that stand.
func TestAmendRfPPaidAndRejected(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	testRfP(ctx, t, l, "rfp-paid")
	if _, err := l.AcceptRfP(ctx, "rfp-paid", "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	if _, err := l.AmendRfP(ctx, "rfp-paid", RfPAmendment{AmountCents: int64Ptr(1)}, ""); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("AmendRfP(paid): got %v, want ErrRfPNotIssued", err)
	}

	testRfP(ctx, t, l, "rfp-rejected")
	if _, err := l.RejectRfP(ctx, "rfp-rejected", "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}
	if _, err := l.AmendRfP(ctx, "rfp-rejected", RfPAmendment{AmountCents: int64Ptr(1)}, ""); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("AmendRfP(rejected): got %v, want ErrRfPNotIssued", err)
	}
}

// Amending a lazily expired request fails with ErrRfPExpired and
// persists the expiry: a later accept reports ErrRfPNotIssued, not a
// second lazy expiry.
func TestAmendRfPLazyExpiredPersists(t *testing.T) {
	ctx := context.Background()
	l := New()
	now := time.Now()
	if _, _, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-late", Creditor: "merchant", Debtor: "customer",
		AmountCents: 5000, Currency: "USD", ExpiresAt: now.Add(200 * time.Millisecond),
	}); err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := l.AmendRfP(ctx, "rfp-late", RfPAmendment{AmountCents: int64Ptr(1)}, ""); !errors.Is(err, ErrRfPExpired) {
		t.Fatalf("AmendRfP(expired): got %v, want ErrRfPExpired", err)
	}
	if got, _ := l.GetRfP("rfp-late"); got.Status != RfPStatusExpired {
		t.Fatalf("status after failed amend = %q, want expired (persisted)", got.Status)
	}
	if _, err := l.AcceptRfP(ctx, "rfp-late", "customer"); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("AcceptRfP after persisted expiry: got %v, want ErrRfPNotIssued", err)
	}
}

func TestAmendRfPNotFound(t *testing.T) {
	ctx := context.Background()
	l := New()
	if _, err := l.AmendRfP(ctx, "nope", RfPAmendment{AmountCents: int64Ptr(1)}, ""); !errors.Is(err, ErrRfPNotFound) {
		t.Fatalf("AmendRfP(unknown): got %v, want ErrRfPNotFound", err)
	}
}

// An invalid merged candidate is rejected and the live record is
// untouched: a non-positive amount, a past expiry, or an empty expiry.
func TestAmendRfPInvalidLeavesLiveUntouched(t *testing.T) {
	ctx := context.Background()
	l := New()
	orig := testRfP(ctx, t, l, "rfp-1")

	for _, tc := range []struct {
		name  string
		patch RfPAmendment
		want  error
	}{
		{"zero amount", RfPAmendment{AmountCents: int64Ptr(0)}, ErrRfPNonPositiveAmount},
		{"negative amount", RfPAmendment{AmountCents: int64Ptr(-5)}, ErrRfPNonPositiveAmount},
		{"past expiry", RfPAmendment{ExpiresAt: timePtr(time.Now().Add(-time.Hour))}, ErrRfPExpiryNotFuture},
		{"zero expiry", RfPAmendment{ExpiresAt: timePtr(time.Time{})}, ErrRfPExpiryRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.AmendRfP(ctx, "rfp-1", tc.patch, ""); !errors.Is(err, tc.want) {
				t.Fatalf("AmendRfP: got %v, want %v", err, tc.want)
			}
			got, _ := l.GetRfP("rfp-1")
			if got.AmountCents != orig.AmountCents || got.Currency != orig.Currency || !got.ExpiresAt.Equal(orig.ExpiresAt) {
				t.Fatalf("live record moved on rejected amend: %+v, want %+v", got, orig)
			}
		})
	}
}

// Amending to the current terms is a no-op: the record is returned
// unchanged and no audit event fires.
func TestAmendRfPSameValueNoop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	orig := testRfP(ctx, t, l, "rfp-1")

	same, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{
		AmountCents: int64Ptr(orig.AmountCents),
		Currency:    stringPtr(orig.Currency),
		ExpiresAt:   timePtr(orig.ExpiresAt),
	}, "")
	if err != nil {
		t.Fatalf("AmendRfP same-value: %v", err)
	}
	if same != orig {
		t.Fatalf("no-op amend returned %+v, want original %+v", same, orig)
	}
	// An empty patch is a no-op too.
	if _, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{}, ""); err != nil {
		t.Fatalf("AmendRfP empty patch: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := auditEventsByOp(t, dir)["rfp_amended"]; len(got) != 0 {
		t.Fatalf("no-op amend audited %d rfp_amended events", len(got))
	}
}

// The amendment is audited as rfp_amended with the previous and new
// terms, attributed to the operator carried by ctx.
func TestAmendRfPAuditDetails(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alan")
	l := New(WithAuditLog(al))
	orig := testRfP(ctx, t, l, "rfp-1")
	newExpiry := time.Now().Add(2 * time.Hour)

	amended, err := l.AmendRfP(ctx, "rfp-1", RfPAmendment{
		AmountCents: int64Ptr(7500),
		Currency:    stringPtr("EUR"),
		ExpiresAt:   timePtr(newExpiry),
	}, "")
	if err != nil {
		t.Fatalf("AmendRfP: %v", err)
	}
	if amended.AmountCents != 7500 || amended.Currency != "EUR" || !amended.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("amended = %+v, want 7500/EUR/new expiry", amended)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	evs := auditEventsByOp(t, dir)["rfp_amended"]
	if len(evs) != 1 {
		t.Fatalf("rfp_amended events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Actor != "ops-alan" {
		t.Fatalf("rfp_amended actor = %q, want ops-alan", ev.Actor)
	}
	d := ev.Details
	if d["previous_amount_cents"] != float64(5000) || d["amount_cents"] != float64(7500) {
		t.Fatalf("amount details = %+v, want previous 5000 / current 7500", d)
	}
	if d["previous_currency"] != "USD" || d["currency"] != "EUR" {
		t.Fatalf("currency details = %+v, want previous USD / current EUR", d)
	}
	prevExp, _ := d["previous_expires_at"].(string)
	curExp, _ := d["expires_at"].(string)
	if prevExp == "" || curExp == "" || prevExp == curExp {
		t.Fatalf("expiry details = previous %q / current %q, want distinct RFC3339 values", prevExp, curExp)
	}
	if prevExp != orig.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Fatalf("previous_expires_at = %q, want %q", prevExp, orig.ExpiresAt.UTC().Format(time.RFC3339))
	}
}
