package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendMandateValidUntilValidation(t *testing.T) {
	if _, err := New().AmendMandateValidUntil("m-unknown", time.Now().Add(24*time.Hour)); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	// Extension only: an equal, earlier, or zero end is refused,
	// and every refusal leaves the mandate exactly as it was.
	for _, end := range []time.Time{m.ValidUntil, m.ValidUntil.Add(-time.Hour), time.Time{}} {
		if _, err := l.AmendMandateValidUntil("m-sub", end); !errors.Is(err, ErrMandateInvalidWindow) {
			t.Errorf("end %v: err = %v, want ErrMandateInvalidWindow", end, err)
		}
	}
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
	// A legal extension changes the validity end and nothing else.
	extended, err := l.AmendMandateValidUntil("m-sub", m.ValidUntil.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("AmendMandateValidUntil: %v", err)
	}
	if !extended.ValidUntil.Equal(m.ValidUntil.Add(30 * 24 * time.Hour)) {
		t.Errorf("amended valid_until = %v, want %v", extended.ValidUntil, m.ValidUntil.Add(30*24*time.Hour))
	}
	extended.ValidUntil = m.ValidUntil
	if extended != m {
		t.Errorf("amended mandate = %+v, want %+v except valid_until", extended, m)
	}
}

func TestAmendMandateValidUntilTerminalStates(t *testing.T) {
	newEnd := time.Now().Add(60 * 24 * time.Hour)
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandateValidUntil("m-sub", newEnd); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandateValidUntil("m-sub", newEnd); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: stored expired reads as a terminal state, not a
	// lazy one.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandateValidUntil("m-sub", newEnd); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("swept-expired mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Lapsed but unswept: still expired, never resurrected by an
	// extension.
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
	if _, err := l4.AmendMandateValidUntil("m-lapsed", now.Add(30*24*time.Hour)); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandateValidUntilEffectiveNextDebit(t *testing.T) {
	l := New()
	now := time.Now()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidUntil:     now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	// A debit inside the original window books before the amendment
	// and stays booked after it.
	beforeAmend := now.Add(-time.Hour)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-0", AmountCents: 1000, CreatedAt: beforeAmend}); err != nil {
		t.Fatalf("pre-amendment debit: %v", err)
	}
	// A debit past the old end is rejected as expired; the rejection
	// books nothing, so the same debit ID is reusable after the
	// extension.
	pastOldEnd := now.Add(48 * time.Hour)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, CreatedAt: pastOldEnd}); !errors.Is(err, ErrMandateExpired) {
		t.Fatalf("debit past old end: err = %v, want ErrMandateExpired", err)
	}
	if _, err := l.AmendMandateValidUntil("m-sub", now.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("AmendMandateValidUntil: %v", err)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, CreatedAt: pastOldEnd}); err != nil {
		t.Fatalf("debit in extended window: %v", err)
	}
	debits, _, err := l.MandateDebits("m-sub", "", 10)
	if err != nil {
		t.Fatalf("MandateDebits: %v", err)
	}
	if len(debits) != 2 || debits[0].ID != "d-0" || debits[1].ID != "d-1" {
		t.Errorf("debits after extension = %+v, want d-0 then d-1", debits)
	}
}

func TestAmendMandateValidUntilAuditAndSnapshot(t *testing.T) {
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
	newEnd := before.ValidUntil.Add(30 * 24 * time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendMandateValidUntilCtx(ctx, "m-sub", newEnd); err != nil {
		t.Fatalf("AmendMandateValidUntilCtx: %v", err)
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
	if !after.ValidUntil.Equal(newEnd) {
		t.Errorf("restored valid_until = %v, want %v", after.ValidUntil, newEnd)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare times by instant, not by struct equality.
	if after.ID != before.ID || after.Payer != before.Payer || after.Payee != before.Payee ||
		after.Currency != before.Currency || after.MaxAmountCents != before.MaxAmountCents ||
		after.Frequency != before.Frequency || after.Status != before.Status {
		t.Errorf("restored mandate = %+v, want %+v except valid_until", after, before)
	}
	if after.ValidFrom.UnixNano() != before.ValidFrom.UnixNano() ||
		after.CreatedAt.UnixNano() != before.CreatedAt.UnixNano() {
		t.Errorf("restored mandate times = %v/%v, want %v/%v",
			after.ValidFrom, after.CreatedAt, before.ValidFrom, before.CreatedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["mandate_valid_until_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" ||
		d["previous_valid_until"] != before.ValidUntil.UTC().Format(time.RFC3339) ||
		d["valid_until"] != newEnd.UTC().Format(time.RFC3339) ||
		d["currency"] != "USD" || d["max_amount_cents"] != float64(5000) {
		t.Errorf("event details = %+v, want m-sub %v->%v USD cap 5000",
			d, before.ValidUntil.UTC().Format(time.RFC3339), newEnd.UTC().Format(time.RFC3339))
	}
}
