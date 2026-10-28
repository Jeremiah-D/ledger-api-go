package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// mandateWideFixture creates a ledger with a funded payer and one
// active mandate whose validity window spans all of 2026–2027, so
// tests can book debits at fixed calendar dates inside it: payer
// "cust" holds 100000 USD, payee "merchant", cap 5000 USD per
// debit, the given frequency.
func mandateWideFixture(t *testing.T, frequency string) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      frequency,
		ValidFrom:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidUntil:     time.Date(2027, 12, 31, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l
}

func TestAmendMandateFrequencyValidation(t *testing.T) {
	if _, err := New().AmendMandateFrequency("m-unknown", MandateFrequencyPerWeek); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	for _, freq := range []string{"per-year", "", "Per-Day", "weekly"} {
		if _, err := l.AmendMandateFrequency("m-sub", freq); !errors.Is(err, ErrInvalidMandateFrequency) {
			t.Errorf("frequency %q: err = %v, want ErrInvalidMandateFrequency", freq, err)
		}
	}
	// Every rejected amendment leaves the mandate exactly as it was.
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
	// A legal amendment changes the frequency and nothing else.
	amended, err := l.AmendMandateFrequency("m-sub", MandateFrequencyPerWeek)
	if err != nil {
		t.Fatalf("AmendMandateFrequency: %v", err)
	}
	if amended.Frequency != MandateFrequencyPerWeek {
		t.Errorf("amended frequency = %q, want per-week", amended.Frequency)
	}
	amended.Frequency = m.Frequency
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except the frequency", amended, m)
	}
}

func TestAmendMandateFrequencyTerminalStates(t *testing.T) {
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandateFrequency("m-sub", MandateFrequencyPerWeek); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandateFrequency("m-sub", MandateFrequencyPerWeek); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: stored expired reads as a terminal state, not a
	// lazy one.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandateFrequency("m-sub", MandateFrequencyPerWeek); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("swept-expired mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	if _, _, err := l4.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l4.CreateMandate(Mandate{
		ID:             "m-lapsed",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-30 * 24 * time.Hour),
		ValidUntil:     now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate lapsed: %v", err)
	}
	if _, err := l4.AmendMandateFrequency("m-lapsed", MandateFrequencyPerWeek); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandateFrequencyEffectiveNextDebit(t *testing.T) {
	oct5 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	oct20 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	// Tightening: per-day to per-month widens the window, so a
	// debit earlier in October that the old frequency would have
	// let pass now blocks the next pull.
	control := mandateWideFixture(t, MandateFrequencyPerDay)
	if _, err := control.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, CreatedAt: oct5}); err != nil {
		t.Fatalf("control first debit: %v", err)
	}
	if _, err := control.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1000, CreatedAt: oct20}); err != nil {
		t.Fatalf("control second debit under per-day: %v", err)
	}
	l := mandateWideFixture(t, MandateFrequencyPerDay)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, CreatedAt: oct5}); err != nil {
		t.Fatalf("first debit: %v", err)
	}
	if _, err := l.AmendMandateFrequency("m-sub", MandateFrequencyPerMonth); err != nil {
		t.Fatalf("AmendMandateFrequency tighten: %v", err)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1000, CreatedAt: oct20}); !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Fatalf("debit after tightening: err = %v, want ErrMandateFrequencyExceeded", err)
	}
	debits, _, err := l.MandateDebits("m-sub", "", 10)
	if err != nil {
		t.Fatalf("MandateDebits: %v", err)
	}
	if len(debits) != 1 || debits[0].ID != "d-1" {
		t.Errorf("debits after tightening = %+v, want only d-1", debits)
	}

	// Loosening: per-month to per-day narrows the window, so a
	// debit earlier in October but on a different day no longer
	// blocks a pull today. The pre-amendment attempt proves the
	// old frequency blocked it; the failed attempt books nothing,
	// so the same debit ID is reusable after the amendment.
	l2 := mandateWideFixture(t, MandateFrequencyPerMonth)
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, CreatedAt: oct5}); err != nil {
		t.Fatalf("first debit: %v", err)
	}
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1000, CreatedAt: oct20}); !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Fatalf("debit under old per-month: err = %v, want ErrMandateFrequencyExceeded", err)
	}
	if _, err := l2.AmendMandateFrequency("m-sub", MandateFrequencyPerDay); err != nil {
		t.Fatalf("AmendMandateFrequency loosen: %v", err)
	}
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1000, CreatedAt: oct20}); err != nil {
		t.Fatalf("debit after loosening: %v", err)
	}
	// The debit journaled under the old frequency stays journaled
	// even though the new window would have treated it the same.
	debits2, _, err := l2.MandateDebits("m-sub", "", 10)
	if err != nil {
		t.Fatalf("MandateDebits: %v", err)
	}
	if len(debits2) != 2 || debits2[0].ID != "d-1" || debits2[1].ID != "d-2" {
		t.Errorf("debits after loosening = %+v, want d-1 then d-2", debits2)
	}
}

func TestAmendMandateFrequencyAuditAndSnapshot(t *testing.T) {
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
	before, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendMandateFrequencyCtx(ctx, "m-sub", MandateFrequencyPerMonth); err != nil {
		t.Fatalf("AmendMandateFrequency: %v", err)
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
	if after.Frequency != MandateFrequencyPerMonth {
		t.Errorf("restored frequency = %q, want per-month", after.Frequency)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare times by instant, not by struct equality.
	if after.ID != before.ID || after.Payer != before.Payer || after.Payee != before.Payee ||
		after.Currency != before.Currency || after.MaxAmountCents != before.MaxAmountCents ||
		after.Status != before.Status {
		t.Errorf("restored mandate = %+v, want %+v except the frequency", after, before)
	}
	if after.ValidFrom.UnixNano() != before.ValidFrom.UnixNano() ||
		after.ValidUntil.UnixNano() != before.ValidUntil.UnixNano() ||
		after.CreatedAt.UnixNano() != before.CreatedAt.UnixNano() {
		t.Errorf("restored mandate times = %v/%v/%v, want %v/%v/%v",
			after.ValidFrom, after.ValidUntil, after.CreatedAt,
			before.ValidFrom, before.ValidUntil, before.CreatedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["mandate_frequency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_frequency"] != MandateFrequencyPerDay ||
		d["frequency"] != MandateFrequencyPerMonth || d["currency"] != "USD" ||
		d["max_amount_cents"] != float64(5000) {
		t.Errorf("event details = %+v, want m-sub per-day->per-month USD cap 5000", d)
	}
}
