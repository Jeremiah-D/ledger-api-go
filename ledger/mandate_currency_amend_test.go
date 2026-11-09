package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendMandateCurrencyValidation(t *testing.T) {
	if _, err := New().AmendMandateCurrency("m-unknown", "EUR"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	// An empty code is a malformed revision, not a request for the
	// default currency; illegal codes reuse the creation gate.
	if _, err := l.AmendMandateCurrency("m-sub", ""); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("empty currency: err = %v, want ErrInvalidCurrency", err)
	}
	for _, bad := range []string{"usd", "US", "USDD", "U$D", "EURO"} {
		if _, err := l.AmendMandateCurrency("m-sub", bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("illegal currency %q: err = %v, want ErrInvalidCurrency", bad, err)
		}
	}
	// Every rejected amendment leaves the mandate exactly as it was.
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
}

func TestAmendMandateCurrencyOnlyCurrencyChanges(t *testing.T) {
	l, m := mandateFixture(t)
	amended, err := l.AmendMandateCurrency("m-sub", "EUR")
	if err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want EUR", amended.Currency)
	}
	// Change ONLY the Currency field; everything else is untouched.
	amended.Currency = m.Currency
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except the currency", amended, m)
	}
	if amended.Payer != "cust" || amended.Payee != "merchant" ||
		amended.MaxAmountCents != 5000 || amended.Frequency != MandateFrequencyPerDay ||
		!amended.ValidFrom.Equal(m.ValidFrom) || !amended.ValidUntil.Equal(m.ValidUntil) ||
		amended.Status != MandateStatusActive {
		t.Errorf("amended mandate fields = %+v, want payer/payee/cap/frequency/window/status untouched", amended)
	}
}

func TestAmendMandateCurrencySameValueNoOp(t *testing.T) {
	l, m := mandateFixture(t)
	same, err := l.AmendMandateCurrency("m-sub", "USD")
	if err != nil {
		t.Fatalf("AmendMandateCurrency same currency: %v", err)
	}
	if same != m {
		t.Errorf("same-currency amend = %+v, want %+v unchanged", same, m)
	}
	// A revision out and back is two real revisions, not a no-op.
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency to EUR: %v", err)
	}
	if back, err := l.AmendMandateCurrency("m-sub", "USD"); err != nil {
		t.Fatalf("AmendMandateCurrency back to USD: %v", err)
	} else if back.Currency != "USD" || back != m {
		t.Errorf("amend back to USD = %+v, want %+v", back, m)
	}
}

func TestAmendMandateCurrencyTerminalStates(t *testing.T) {
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandateCurrency("m-sub", "EUR"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: the stored status is "expired" (the validity
	// end itself is still in the future, so the lazy-expiry
	// predicate does not fire) — the LG-243 reading, not a lazy
	// expiry.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandateCurrency("m-sub", "EUR"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("swept-expired mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	if _, _, err := l4.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l4.CreateMandate(Mandate{
		ID:             "m-old",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-48 * time.Hour),
		ValidUntil:     now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate lapsed: %v", err)
	}
	if _, err := l4.AmendMandateCurrency("m-old", "EUR"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandateCurrencyDebitFollowsNewCurrency(t *testing.T) {
	l, _ := mandateFixture(t)
	// One debit under the original currency books in USD.
	before, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200, Memo: "subscription"})
	if err != nil {
		t.Fatalf("DebitWithMandate before amend: %v", err)
	}
	if before.Entry.Currency != "USD" {
		t.Errorf("debit before amend currency = %q, want USD", before.Entry.Currency)
	}
	// Amend the currency; the revision is off-journal, and the next
	// pull — booked next day so the per-day frequency window is
	// free — settles in the new currency.
	versionBefore := l.Version()
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: amendment is off-journal")
	}
	after, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1200, Currency: "EUR", Memo: "subscription", CreatedAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("DebitWithMandate after amend: %v", err)
	}
	if after.Entry.Currency != "EUR" || after.Entry.AmountCents != 1200 {
		t.Errorf("debit after amend = %+v, want 1200 EUR", after.Entry)
	}
	if after.Entry.DebitAccount != "merchant" || after.Entry.CreditAccount != "cust" {
		t.Errorf("debit after amend legs = %q/%q, want merchant/cust", after.Entry.DebitAccount, after.Entry.CreditAccount)
	}
}

func TestAmendMandateCurrencyOldCurrencyDebitRejected(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	entriesBefore := len(l.Entries())
	versionBefore := l.Version()
	// A pull in the retired currency fails at the mandate gate and
	// books nothing — the currency check runs before the frequency
	// gate, so no window state matters here.
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-old", AmountCents: 1200, Currency: "USD", Memo: "subscription"}); !errors.Is(err, ErrMandateCurrencyMismatch) {
		t.Errorf("debit in old currency: err = %v, want ErrMandateCurrencyMismatch", err)
	}
	if got := len(l.Entries()); got != entriesBefore {
		t.Errorf("entries after rejected debit = %d, want %d (nothing booked)", got, entriesBefore)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved on a rejected debit")
	}
	if got, ok := l.GetMandate("m-sub"); !ok || got.Currency != "EUR" {
		t.Errorf("mandate after rejected debit = %+v ok=%v, want currency EUR", got, ok)
	}
}

func TestAmendMandateCurrencyPostedDebitKeepsCurrency(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200, Memo: "subscription"}); err != nil {
		t.Fatalf("DebitWithMandate before amend: %v", err)
	}
	if _, err := l.AmendMandateCurrency("m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	// The revision is not FX and not a restatement: the posted debit
	// keeps its booked currency, legs, and amount.
	var posted *JournalEntry
	for _, e := range l.Entries() {
		if e.ID == "d-1" {
			e := e
			posted = &e
		}
	}
	if posted == nil {
		t.Fatal("posted debit d-1 missing from entries")
	}
	if posted.Currency != "USD" || posted.AmountCents != 1200 ||
		posted.DebitAccount != "merchant" || posted.CreditAccount != "cust" {
		t.Errorf("posted debit after amend = %+v, want 1200 USD merchant/cust", *posted)
	}
}

func TestAmendMandateCurrencyAuditAndSnapshot(t *testing.T) {
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
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendMandateCurrencyCtx(ctx, "m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency: %v", err)
	}
	// The same-value no-op writes no second event.
	if _, err := l.AmendMandateCurrencyCtx(ctx, "m-sub", "EUR"); err != nil {
		t.Fatalf("AmendMandateCurrency same currency: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetMandate("m-sub")
	if !ok {
		t.Fatal("restored mandate missing")
	}
	if after.Currency != "EUR" || after.Payer != "cust" || after.Payee != "merchant" || after.Status != MandateStatusActive {
		t.Errorf("restored mandate = %+v, want currency EUR, payer cust, payee merchant, active", after)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["mandate_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (same-value no-op writes none)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_currency"] != "USD" ||
		d["currency"] != "EUR" || d["payer"] != "cust" || d["payee"] != "merchant" {
		t.Errorf("event details = %+v, want m-sub USD->EUR cust/merchant", d)
	}
}
