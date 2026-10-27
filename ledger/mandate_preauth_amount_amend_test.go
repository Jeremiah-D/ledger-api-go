package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendPreAuthAmountSuccess(t *testing.T) {
	l, _ := preauthFixture(t)
	p := authorizeOne(t, l, "pa-1")
	versionBefore := l.Version()
	got, err := l.AmendPreAuthAmount("m-sub", "pa-1", 3500)
	if err != nil {
		t.Fatalf("AmendPreAuthAmount: %v", err)
	}
	// The amendment moves the amount and nothing else.
	if got.AmountCents != 3500 {
		t.Errorf("amount_cents = %d, want 3500", got.AmountCents)
	}
	if got.Currency != "USD" || got.Status != PreAuthStatusActive ||
		!got.ExpiresAt.Equal(p.ExpiresAt) || got.IdempotencyKey != p.IdempotencyKey {
		t.Errorf("amended preauth = %+v, want USD active with original expiry/key", got)
	}
	if got.ID != "pa-1" || got.MandateID != "m-sub" || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("amended preauth = %+v, want pa-1/m-sub with original created_at", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The stored row carries the new amount.
	stored, ok := l.GetPreAuth("pa-1")
	if !ok || stored.AmountCents != 3500 || stored.Status != PreAuthStatusActive {
		t.Errorf("stored preauth = %+v,%v, want active with amount 3500", stored, ok)
	}
	// Capture settles the live amount: the amended reservation
	// settles at 3500, not the originally authorized 2000.
	receipt, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"})
	if err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if receipt.AmountCents != 3500 || receipt.Entry.AmountCents != 3500 {
		t.Errorf("capture receipt = %+v, want 3500", receipt)
	}
}

func TestAmendPreAuthAmountRejections(t *testing.T) {
	// Unknown pre-authorization, and a pre-authorization under a
	// different mandate than the route's.
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	if _, err := l.AmendPreAuthAmount("m-sub", "nope", 3000); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("unknown preauth: err = %v, want ErrPreAuthNotFound", err)
	}
	if _, err := l.AmendPreAuthAmount("other-mandate", "pa-1", 3000); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("wrong mandate: err = %v, want ErrPreAuthNotFound", err)
	}

	// Non-positive amounts and amounts above the mandate cap
	// (m-sub caps at 5000) all fail, and the original amount is
	// untouched — including the at-cap amount, which succeeds.
	for _, tc := range []struct {
		name   string
		amount int64
		want   error
	}{
		{"zero", 0, ErrPreAuthNonPositiveAmount},
		{"negative", -5, ErrPreAuthNonPositiveAmount},
		{"over cap", 5001, ErrMandateAmountExceeded},
	} {
		ll, _ := preauthFixture(t)
		authorizeOne(t, ll, "pa-1")
		if _, err := ll.AmendPreAuthAmount("m-sub", "pa-1", tc.amount); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		stored, _ := ll.GetPreAuth("pa-1")
		if stored.AmountCents != 2000 {
			t.Errorf("%s: amount moved to %d after rejected amendment, want 2000", tc.name, stored.AmountCents)
		}
	}
	lc, _ := preauthFixture(t)
	authorizeOne(t, lc, "pa-1")
	if _, err := lc.AmendPreAuthAmount("m-sub", "pa-1", 5000); err != nil {
		t.Errorf("at-cap amount: err = %v, want nil", err)
	}

	// Captured and released pre-authorizations are terminal.
	lcap, _ := preauthFixture(t)
	authorizeOne(t, lcap, "pa-1")
	if _, err := lcap.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if _, err := lcap.AmendPreAuthAmount("m-sub", "pa-1", 3000); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("captured: err = %v, want ErrPreAuthNotActive", err)
	}
	lrel, _ := preauthFixture(t)
	authorizeOne(t, lrel, "pa-1")
	if _, err := lrel.ReleasePreAuth("m-sub", "pa-1"); err != nil {
		t.Fatalf("ReleasePreAuth: %v", err)
	}
	if _, err := lrel.AmendPreAuthAmount("m-sub", "pa-1", 3000); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("released: err = %v, want ErrPreAuthNotActive", err)
	}

	// Expired pre-authorizations: born expired, and swept expired.
	now := time.Now()
	le, _ := preauthFixture(t)
	if _, _, err := le.AuthorizePreAuth(PreAuth{
		ID: "pa-born-expired", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD",
		ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AuthorizePreAuth born expired: %v", err)
	}
	if _, err := le.AmendPreAuthAmount("m-sub", "pa-born-expired", 3000); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("born expired: err = %v, want ErrPreAuthExpired", err)
	}
	ls, _ := preauthFixture(t)
	authorizeOne(t, ls, "pa-1")
	if got := ls.ExpirePreAuthsAt(now.Add(2 * time.Hour)); got != 1 {
		t.Fatalf("ExpirePreAuthsAt: expired = %d, want 1", got)
	}
	if _, err := ls.AmendPreAuthAmount("m-sub", "pa-1", 3000); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("swept expired: err = %v, want ErrPreAuthExpired", err)
	}
}

func TestAmendPreAuthAmountAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	// Rebuild the mandate fixture on the audited ledger.
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-sub", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, _, err := l.AuthorizePreAuth(PreAuth{
		ID: "pa-1", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alice")
	if _, err := l.AmendPreAuthAmountCtx(ctx, "m-sub", "pa-1", 3500); err != nil {
		t.Fatalf("AmendPreAuthAmountCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["preauth_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_amount_cents"] != float64(2000) ||
		d["amount_cents"] != float64(3500) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want m-sub 2000->3500 USD", d)
	}
}
