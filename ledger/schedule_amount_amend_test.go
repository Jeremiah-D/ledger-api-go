package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createAmountSchedule registers one active plan for the amount
// amendment tests: alice pays bob 1000 cents hourly, with a memo,
// an explicit 10-cent fee, and an end time far in the future, so the
// amendment tests can prove every other field survives untouched.
func createAmountSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
	t.Helper()
	sched, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          id,
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		FeeCents:    10,
		FeeAccount:  "fees",
		Memo:        "rent",
		Interval:    24 * time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule %s: %v", id, err)
	}
	return sched
}

func TestAmendTransferScheduleAmountValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleAmount("sched-unknown", 2000); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createAmountSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createAmountSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleAmount("sched-1", 2000); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("cancelled schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Completed: one firing advances NextRunAt past EndsAt.
	l3 := New()
	fundAccount(t, l3, "alice", 100000, "USD")
	s := testSchedule(now)
	s.ID = "sched-1"
	s.Interval = time.Hour
	s.NextRunAt = now.Add(-time.Minute)
	s.EndsAt = s.NextRunAt.Add(30 * time.Minute)
	if _, _, err := l3.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if runs := l3.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	if got, _ := l3.GetTransferSchedule("sched-1"); got.Status != ScheduleCompleted {
		t.Fatalf("status after firing past ends_at = %q, want completed", got.Status)
	}
	if _, err := l3.AmendTransferScheduleAmount("sched-1", 2000); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Non-positive amounts reuse the creation gate, and every
	// rejected amendment leaves the plan exactly as it was.
	for _, bad := range []int64{0, -5} {
		if _, err := l.AmendTransferScheduleAmount("sched-1", bad); !errors.Is(err, ErrScheduleNonPositiveAmount) {
			t.Errorf("amount %d: err = %v, want ErrScheduleNonPositiveAmount", bad, err)
		}
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current amount is a no-op, not a change.
	if got, err := l.AmendTransferScheduleAmount("sched-1", 1000); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-amount amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only AmountCents changes.
	amended, err := l.AmendTransferScheduleAmount("sched-1", 2500)
	if err != nil {
		t.Fatalf("AmendTransferScheduleAmount active: %v", err)
	}
	if amended.AmountCents != 2500 {
		t.Errorf("amended amount = %d, want 2500", amended.AmountCents)
	}
	amended.AmountCents = orig.AmountCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except amount", amended, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan and governs the first run after
	// resume — and again only the amount moves.
	l4 := New()
	orig4 := createAmountSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleAmount("sched-1", 3000)
	if err != nil {
		t.Fatalf("AmendTransferScheduleAmount paused: %v", err)
	}
	if paused.AmountCents != 3000 || paused.Status != SchedulePaused {
		t.Errorf("amended paused = amount %d status %q, want 3000 paused", paused.AmountCents, paused.Status)
	}
	paused.AmountCents = orig4.AmountCents
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except amount", paused, orig4)
	}
}

func TestAmendTransferScheduleAmountNextFire(t *testing.T) {
	// The next successful firing posts the amended amount: the
	// sweep snapshot is taken at fire time, so no firing-path
	// change is needed, and the already-fired lineage is untouched.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createAmountSchedule(t, l, "sched-1", now.Add(-time.Minute))
	if _, err := l.AmendTransferScheduleAmount("sched-1", 2500); err != nil {
		t.Fatalf("AmendTransferScheduleAmount: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("bob", "USD"); got != 2500 {
		t.Errorf("bob = %d, want 2500 (amended amount, not 1000)", got)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.RunSeq != 1 {
		t.Errorf("run_seq = %d, want 1", got.RunSeq)
	}
}

func TestAmendTransferScheduleAmountFailedRunRetry(t *testing.T) {
	// A failed run posts nothing, so its retry reads the amended
	// amount and books exactly once — no double-posting risk, and
	// the same "<ID>/run/<seq>" key is reused.
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	createAmountSchedule(t, l, "sched-1", now.Add(-time.Minute))
	l.Freeze("alice")
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if _, err := l.AmendTransferScheduleAmount("sched-1", 3000); err != nil {
		t.Fatalf("AmendTransferScheduleAmount after failure: %v", err)
	}
	l.Unfreeze("alice")
	runs = l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("retry runs = %+v, want one fired run", runs)
	}
	if runs[0].RunSeq != 1 {
		t.Errorf("retried run_seq = %d, want 1 (same run retried)", runs[0].RunSeq)
	}
	if got := l.BalanceIn("bob", "USD"); got != 3000 {
		t.Errorf("bob = %d, want 3000 (amended amount, booked exactly once)", got)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.RunSeq != 1 {
		t.Errorf("run_seq = %d, want 1", got.RunSeq)
	}
}

func TestAmendTransferScheduleAmountAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createAmountSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleAmount("sched-1", 1000); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleAmountCtx(ctx, "sched-1", 2500); err != nil {
		t.Fatalf("AmendTransferScheduleAmountCtx: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, err := restored.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("restored schedule missing: %v", err)
	}
	if after.AmountCents != 2500 {
		t.Errorf("restored amount = %d, want 2500", after.AmountCents)
	}
	after.AmountCents = before.AmountCents
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except amount", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_amount_cents"] != float64(1000) ||
		d["amount_cents"] != float64(2500) ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 1000->2500 USD run_seq 0", d)
	}
}
