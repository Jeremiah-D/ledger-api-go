package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendMandateMaxAmountValidation(t *testing.T) {
	if _, err := New().AmendMandateMaxAmount("m-unknown", 5000); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	for _, cap := range []int64{0, -100} {
		if _, err := l.AmendMandateMaxAmount("m-sub", cap); !errors.Is(err, ErrMandateNonPositiveMaxAmount) {
			t.Errorf("cap %d: err = %v, want ErrMandateNonPositiveMaxAmount", cap, err)
		}
	}
	// Every rejected amendment leaves the mandate exactly as it was.
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
	// A legal amendment changes the cap and nothing else.
	amended, err := l.AmendMandateMaxAmount("m-sub", 8000)
	if err != nil {
		t.Fatalf("AmendMandateMaxAmount: %v", err)
	}
	if amended.MaxAmountCents != 8000 {
		t.Errorf("amended cap = %d, want 8000", amended.MaxAmountCents)
	}
	amended.MaxAmountCents = m.MaxAmountCents
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except the cap", amended, m)
	}
}

func TestAmendMandateMaxAmountTerminalStates(t *testing.T) {
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandateMaxAmount("m-sub", 8000); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandateMaxAmount("m-sub", 8000); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: stored expired reads as a terminal state, not a
	// lazy one.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandateMaxAmount("m-sub", 8000); !errors.Is(err, ErrMandateNotActive) {
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
	if _, err := l4.AmendMandateMaxAmount("m-lapsed", 8000); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandateMaxAmountEffectiveNextDebit(t *testing.T) {
	// Raising the cap unblocks a debit the old cap rejected.
	l, _ := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-over", AmountCents: 6000}); !errors.Is(err, ErrMandateAmountExceeded) {
		t.Fatalf("debit above old cap: err = %v, want ErrMandateAmountExceeded", err)
	}
	if _, err := l.AmendMandateMaxAmount("m-sub", 8000); err != nil {
		t.Fatalf("AmendMandateMaxAmount: %v", err)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-raised", AmountCents: 6000}); err != nil {
		t.Fatalf("debit under raised cap: %v", err)
	}

	// Lowering the cap blocks a debit the old cap allowed, while the
	// debit already journaled under the old cap stays journaled.
	l2, _ := mandateFixture(t)
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-old", AmountCents: 4000}); err != nil {
		t.Fatalf("debit under old cap: %v", err)
	}
	if _, err := l2.AmendMandateMaxAmount("m-sub", 3000); err != nil {
		t.Fatalf("AmendMandateMaxAmount: %v", err)
	}
	// Next-day booking so the per-day frequency window is free and
	// only the amount gate can fire.
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-new", AmountCents: 4000, CreatedAt: time.Now().Add(24 * time.Hour)}); !errors.Is(err, ErrMandateAmountExceeded) {
		t.Fatalf("debit above lowered cap: err = %v, want ErrMandateAmountExceeded", err)
	}
	debits, _, err := l2.MandateDebits("m-sub", "", 10)
	if err != nil {
		t.Fatalf("MandateDebits: %v", err)
	}
	if len(debits) != 1 || debits[0].ID != "d-old" || debits[0].AmountCents != 4000 {
		t.Errorf("debits after lowered cap = %+v, want only the original 4000 debit", debits)
	}
}

func TestAmendMandateMaxAmountAuditAndSnapshot(t *testing.T) {
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
	if _, err := l.AmendMandateMaxAmountCtx(ctx, "m-sub", 8000); err != nil {
		t.Fatalf("AmendMandateMaxAmount: %v", err)
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
	if after.MaxAmountCents != 8000 {
		t.Errorf("restored cap = %d, want 8000", after.MaxAmountCents)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare times by instant, not by struct equality.
	if after.ID != before.ID || after.Payer != before.Payer || after.Payee != before.Payee ||
		after.Currency != before.Currency || after.Frequency != before.Frequency ||
		after.Status != before.Status {
		t.Errorf("restored mandate = %+v, want %+v except the cap", after, before)
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
	events := auditEventsByOp(t, dir)["mandate_max_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_max_amount_cents"] != float64(5000) ||
		d["max_amount_cents"] != float64(8000) || d["currency"] != "USD" ||
		d["frequency"] != MandateFrequencyPerDay {
		t.Errorf("event details = %+v, want m-sub 5000->8000 USD per-day", d)
	}
}
