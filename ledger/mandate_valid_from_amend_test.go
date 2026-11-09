package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// futureStartMandateFixture creates a ledger with a funded payer and
// one active mandate whose validity starts an hour in the future:
// payer "cust" holds 100000 USD, payee "merchant", cap 5000 USD per
// debit, per-day frequency, valid for 30 days from the future start.
func futureStartMandateFixture(t *testing.T) (*Ledger, Mandate) {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	m, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, m
}

func TestAmendMandateValidFromValidation(t *testing.T) {
	if _, err := New().AmendMandateValidFrom("m-unknown", time.Now().Add(-time.Hour)); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	// The new start must be strictly before the live end: equal to
	// or after the end is refused, and every refusal leaves the
	// mandate exactly as it was.
	for _, start := range []time.Time{m.ValidUntil, m.ValidUntil.Add(time.Hour)} {
		if _, err := l.AmendMandateValidFrom("m-sub", start); !errors.Is(err, ErrMandateInvalidWindow) {
			t.Errorf("start %v: err = %v, want ErrMandateInvalidWindow", start, err)
		}
	}
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
	// Amending to the current start is a no-op, not a change.
	if got, err := l.AmendMandateValidFrom("m-sub", m.ValidFrom); err != nil || got != m {
		t.Errorf("same-start amendment = %+v, %v, want %+v, nil", got, err, m)
	}
	// A legal revision changes the validity start and nothing else.
	amended, err := l.AmendMandateValidFrom("m-sub", m.ValidFrom.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("AmendMandateValidFrom: %v", err)
	}
	if !amended.ValidFrom.Equal(m.ValidFrom.Add(-24 * time.Hour)) {
		t.Errorf("amended valid_from = %v, want %v", amended.ValidFrom, m.ValidFrom.Add(-24*time.Hour))
	}
	amended.ValidFrom = m.ValidFrom
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except valid_from", amended, m)
	}
}

func TestAmendMandateValidFromTerminalStates(t *testing.T) {
	newStart := time.Now().Add(-2 * time.Hour)
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandateValidFrom("m-sub", newStart); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandateValidFrom("m-sub", newStart); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: stored expired reads as a terminal state, not a
	// lazy one.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandateValidFrom("m-sub", newStart); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("swept-expired mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Lapsed but unswept: still expired, never resurrected by an
	// amendment.
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
	if _, err := l4.AmendMandateValidFrom("m-lapsed", now.Add(-31*24*time.Hour)); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandateValidFromHasDebits(t *testing.T) {
	l, m := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200}); err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	// One posted debit closes the revision window: moving the start
	// would restate the window that debit was pulled under.
	if _, err := l.AmendMandateValidFrom("m-sub", m.ValidFrom.Add(-24*time.Hour)); !errors.Is(err, ErrMandateHasDebits) {
		t.Errorf("debited mandate: err = %v, want ErrMandateHasDebits", err)
	}
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendment = %+v ok=%v, want %+v", got, ok, m)
	}
}

func TestAmendMandateValidFromEarlyDebit(t *testing.T) {
	// A debit before the original start is rejected as not yet
	// active; the rejection books nothing, so the same debit ID is
	// reusable after the start moves earlier.
	l, m := futureStartMandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("debit before valid_from: err = %v, want ErrMandateNotActive", err)
	}
	if _, err := l.AmendMandateValidFrom("m-sub", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("AmendMandateValidFrom: %v", err)
	}
	receipt, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000})
	if err != nil {
		t.Fatalf("debit after earlier start: %v", err)
	}
	if receipt.Entry.AmountCents != 1000 || receipt.Entry.DebitAccount != "merchant" {
		t.Errorf("receipt = %+v, want 1000 pulled to merchant", receipt.Entry)
	}
	if got, _ := l.GetMandate("m-sub"); got != m {
		// Only ValidFrom moved; restore it for the comparison.
		got.ValidFrom = m.ValidFrom
		if !reflect.DeepEqual(got, m) {
			t.Errorf("mandate after amendment = %+v, want %+v except valid_from", got, m)
		}
	}
}

func TestAmendMandateValidFromDelayedDebit(t *testing.T) {
	// Pushing the start out keeps a debit before the new start
	// refused: the window is a gate on future pulls.
	l, _ := mandateFixture(t)
	if _, err := l.AmendMandateValidFrom("m-sub", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("AmendMandateValidFrom: %v", err)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("debit before delayed valid_from: err = %v, want ErrMandateNotActive", err)
	}
}

func TestAmendMandateValidFromZeroStart(t *testing.T) {
	// A zero start passes the creation window gate and means
	// effective since the beginning of time: any debit inside the
	// window pulls.
	l, m := futureStartMandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("debit before valid_from: err = %v, want ErrMandateNotActive", err)
	}
	amended, err := l.AmendMandateValidFrom("m-sub", time.Time{})
	if err != nil {
		t.Fatalf("AmendMandateValidFrom zero: %v", err)
	}
	if !amended.ValidFrom.IsZero() {
		t.Errorf("amended valid_from = %v, want zero", amended.ValidFrom)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000}); err != nil {
		t.Fatalf("debit after zero start: %v", err)
	}
	amended.ValidFrom = m.ValidFrom
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except valid_from", amended, m)
	}
}

func TestAmendMandateValidFromAuditAndSnapshot(t *testing.T) {
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
	// The no-op revision emits nothing.
	if _, err := l.AmendMandateValidFrom("m-sub", before.ValidFrom); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	newStart := before.ValidFrom.Add(-24 * time.Hour)
	if _, err := l.AmendMandateValidFromCtx(ctx, "m-sub", newStart); err != nil {
		t.Fatalf("AmendMandateValidFromCtx: %v", err)
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
		t.Fatalf("restored mandate missing")
	}
	if !after.ValidFrom.Equal(newStart) {
		t.Errorf("restored valid_from = %v, want %v", after.ValidFrom, newStart)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare times by instant, not by struct equality.
	if after.ID != before.ID || after.Payer != before.Payer || after.Payee != before.Payee ||
		after.Currency != before.Currency || after.MaxAmountCents != before.MaxAmountCents ||
		after.Frequency != before.Frequency || after.Status != before.Status {
		t.Errorf("restored mandate = %+v, want %+v except valid_from", after, before)
	}
	if after.ValidUntil.UnixNano() != before.ValidUntil.UnixNano() ||
		after.CreatedAt.UnixNano() != before.CreatedAt.UnixNano() {
		t.Errorf("restored mandate times = %v/%v, want %v/%v",
			after.ValidUntil, after.CreatedAt, before.ValidUntil, before.CreatedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["mandate_valid_from_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" ||
		d["previous_valid_from"] != before.ValidFrom.UTC().Format(time.RFC3339) ||
		d["valid_from"] != newStart.UTC().Format(time.RFC3339) ||
		d["currency"] != "USD" ||
		d["max_amount_cents"] != float64(5000) {
		t.Errorf("event details = %+v, want m-sub start %v->%v USD cap 5000", d, before.ValidFrom, newStart)
	}
}
