package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendPreAuthCurrencySuccess(t *testing.T) {
	l, _ := preauthFixture(t)
	p := authorizeOne(t, l, "pa-1")
	// Fund the payer in EUR as well: the capture below settles in
	// EUR, and the debit path guards the payer's EUR balance.
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust-eur", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "EUR"}); err != nil {
		t.Fatalf("fund EUR Post: %v", err)
	}
	// The mandate moves first (LG-367); the reservation is then
	// brought in line with the mandate's new currency.
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	versionBefore := l.Version()
	got, err := l.AmendPreAuthCurrency("m-sub", "pa-1", "EUR")
	if err != nil {
		t.Fatalf("AmendPreAuthCurrency: %v", err)
	}
	// The amendment moves the currency and nothing else.
	if got.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", got.Currency)
	}
	if got.AmountCents != 2000 || got.Status != PreAuthStatusActive ||
		!got.ExpiresAt.Equal(p.ExpiresAt) || got.IdempotencyKey != p.IdempotencyKey {
		t.Errorf("amended preauth = %+v, want 2000 active with original expiry/key", got)
	}
	if got.ID != "pa-1" || got.MandateID != "m-sub" || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("amended preauth = %+v, want pa-1/m-sub with original created_at", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The stored row carries the new currency.
	stored, ok := l.GetPreAuth("pa-1")
	if !ok || stored.Currency != "EUR" || stored.Status != PreAuthStatusActive {
		t.Errorf("stored preauth = %+v,%v, want active with currency EUR", stored, ok)
	}
	// Capture settles the live currency: the amended reservation
	// settles in EUR, not the originally authorized USD.
	receipt, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"})
	if err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if receipt.AmountCents != 2000 || receipt.Entry.AmountCents != 2000 || receipt.Entry.Currency != "EUR" {
		t.Errorf("capture receipt = %+v, want 2000 EUR", receipt)
	}
}

func TestAmendPreAuthCurrencyRejections(t *testing.T) {
	// Unknown pre-authorization, and a pre-authorization under a
	// different mandate than the route's.
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	if _, err := l.AmendPreAuthCurrency("m-sub", "nope", "EUR"); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("unknown preauth: err = %v, want ErrPreAuthNotFound", err)
	}
	if _, err := l.AmendPreAuthCurrency("other-mandate", "pa-1", "EUR"); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("wrong mandate: err = %v, want ErrPreAuthNotFound", err)
	}

	// Empty and malformed codes, and a currency that does not match
	// the mandate's live currency (m-sub is USD here), all fail,
	// and the original currency is untouched.
	for _, tc := range []struct {
		name string
		code string
		want error
	}{
		{"empty", "", ErrInvalidCurrency},
		{"lowercase", "eur", ErrInvalidCurrency},
		{"short", "EU", ErrInvalidCurrency},
		{"long", "EURO", ErrInvalidCurrency},
		{"mismatch", "EUR", ErrMandateCurrencyMismatch},
		{"mismatch GBP", "GBP", ErrMandateCurrencyMismatch},
	} {
		ll, _ := preauthFixture(t)
		authorizeOne(t, ll, "pa-1")
		if _, err := ll.AmendPreAuthCurrency("m-sub", "pa-1", tc.code); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		stored, _ := ll.GetPreAuth("pa-1")
		if stored.Currency != "USD" {
			t.Errorf("%s: currency moved to %q after rejected amendment, want USD", tc.name, stored.Currency)
		}
	}

	// Captured and released pre-authorizations are terminal.
	lcap, _ := preauthFixture(t)
	authorizeOne(t, lcap, "pa-1")
	if _, err := lcap.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if _, err := lcap.AmendPreAuthCurrency("m-sub", "pa-1", "EUR"); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("captured: err = %v, want ErrPreAuthNotActive", err)
	}
	lrel, _ := preauthFixture(t)
	authorizeOne(t, lrel, "pa-1")
	if _, err := lrel.ReleasePreAuth("m-sub", "pa-1"); err != nil {
		t.Fatalf("ReleasePreAuth: %v", err)
	}
	if _, err := lrel.AmendPreAuthCurrency("m-sub", "pa-1", "EUR"); !errors.Is(err, ErrPreAuthNotActive) {
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
	if _, err := le.AmendPreAuthCurrency("m-sub", "pa-born-expired", "EUR"); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("born expired: err = %v, want ErrPreAuthExpired", err)
	}
	ls, _ := preauthFixture(t)
	authorizeOne(t, ls, "pa-1")
	if got := ls.ExpirePreAuthsAt(now.Add(2 * time.Hour)); got != 1 {
		t.Fatalf("ExpirePreAuthsAt: expired = %d, want 1", got)
	}
	if _, err := ls.AmendPreAuthCurrency("m-sub", "pa-1", "EUR"); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("swept expired: err = %v, want ErrPreAuthExpired", err)
	}
}

func TestAmendPreAuthCurrencyNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
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
	got, err := l.AmendPreAuthCurrency("m-sub", "pa-1", "USD")
	if err != nil {
		t.Fatalf("AmendPreAuthCurrency same value: %v", err)
	}
	if got.Currency != "USD" || got.Status != PreAuthStatusActive {
		t.Errorf("no-op preauth = %+v, want USD active", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := auditEventsByOp(t, dir)["preauth_currency_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 for a no-op", len(events))
	}
}

func TestAmendPreAuthCurrencyAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
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
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alice")
	if _, err := l.AmendPreAuthCurrencyCtx(ctx, "m-sub", "pa-1", "EUR"); err != nil {
		t.Fatalf("AmendPreAuthCurrencyCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["preauth_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_currency"] != "USD" ||
		d["currency"] != "EUR" || d["amount_cents"] != float64(2000) {
		t.Errorf("event details = %+v, want m-sub USD->EUR 2000", d)
	}
}
