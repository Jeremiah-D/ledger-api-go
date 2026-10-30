package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// principalMemo returns the memo on the principal journal entry of
// the named transfer (the principal entry's ID is the transfer ID).
func principalMemo(t *testing.T, l *Ledger, transferID string) string {
	t.Helper()
	for _, e := range l.Entries() {
		if e.ID == transferID {
			return e.Memo
		}
	}
	t.Fatalf("no principal entry for transfer %q", transferID)
	return ""
}

func TestAmendTransferScheduleMemoValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleMemo("sched-unknown", "x"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFeeSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFeeSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleMemo("sched-1", "x"); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleMemo("sched-1", "x"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Overlong memo: the creation gate reused — ErrMemoTooLong and
	// the live memo untouched.
	overlong := strings.Repeat("m", 256)
	if _, err := l.AmendTransferScheduleMemo("sched-1", overlong); !errors.Is(err, ErrMemoTooLong) {
		t.Errorf("overlong memo: err = %v, want ErrMemoTooLong", err)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.Memo != "rent" {
		t.Errorf("memo after rejected amendment = %q, want rent (unchanged)", got.Memo)
	}

	// Amending to the current memo is a no-op, not a change.
	if got, err := l.AmendTransferScheduleMemo("sched-1", "rent"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-memo amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only Memo changes — legs, amount, currency, fee
	// amount, fee account, interval, next run time, end time, and run
	// sequence are untouched.
	amended, err := l.AmendTransferScheduleMemo("sched-1", "utilities")
	if err != nil {
		t.Fatalf("AmendTransferScheduleMemo active: %v", err)
	}
	if amended.Memo != "utilities" {
		t.Errorf("amended memo = %q, want utilities", amended.Memo)
	}
	amended.Memo = orig.Memo
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except memo", amended, orig)
	}

	// Empty string is legal and clears the memo; the no-op judgement
	// is by equality, so clearing an already-clear plan is a no-op.
	cleared, err := l.AmendTransferScheduleMemo("sched-1", "")
	if err != nil {
		t.Fatalf("AmendTransferScheduleMemo clear: %v", err)
	}
	if cleared.Memo != "" {
		t.Errorf("cleared memo = %q, want empty", cleared.Memo)
	}
	cleared.Memo = orig.Memo
	if !reflect.DeepEqual(cleared, orig) {
		t.Errorf("cleared schedule = %+v, want %+v except memo", cleared, orig)
	}
	if got, err := l.AmendTransferScheduleMemo("sched-1", ""); err != nil || got.Memo != "" {
		t.Errorf("clear-on-clear amendment = %+v, %v, want empty memo, nil", got, err)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the memo moves.
	l4 := New()
	orig4 := createFeeSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleMemo("sched-1", "utilities")
	if err != nil {
		t.Fatalf("AmendTransferScheduleMemo paused: %v", err)
	}
	if paused.Memo != "utilities" || paused.Status != SchedulePaused {
		t.Errorf("amended paused = memo %q status %q, want utilities paused", paused.Memo, paused.Status)
	}
	paused.Memo = orig4.Memo
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except memo", paused, orig4)
	}
}

func TestAmendTransferScheduleMemoNextFiring(t *testing.T) {
	// The new memo governs the next successful firing with no
	// firing-path change: the first run books the original memo, the
	// amendment itself posts nothing, the second run books the amended
	// memo on its principal entry, and the first run's journaled
	// entry is never rewritten.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createFeeSchedule(t, l, "sched-1", now)
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	if got := principalMemo(t, l, "sched-1/run/1"); got != "rent" {
		t.Errorf("first run principal memo = %q, want rent", got)
	}
	if _, err := l.AmendTransferScheduleMemo("sched-1", "utilities"); err != nil {
		t.Fatalf("AmendTransferScheduleMemo: %v", err)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-1000-10 {
		t.Fatalf("alice after amendment = %d, want %d (amendment posts nothing)", got, 100000-1000-10)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := principalMemo(t, l, "sched-1/run/2"); got != "utilities" {
		t.Errorf("second run principal memo = %q, want utilities", got)
	}
	if got := principalMemo(t, l, "sched-1/run/1"); got != "rent" {
		t.Errorf("first run principal memo after amendment = %q, want rent (never rewritten)", got)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 {
		t.Errorf("run history = %d runs, %v, want 2", len(runs), err)
	}
}

func TestAmendTransferScheduleMemoFailedRunRetry(t *testing.T) {
	// A failed run posted nothing, so its retry reads the amended
	// memo: a frozen payer fails the first run, unfreezing plus a memo
	// amendment later, the retry books exactly once, with the new
	// memo, under the same "<ID>/run/<seq>" idempotency key.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createFeeSchedule(t, l, "sched-1", now)
	l.Freeze("alice")
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("first sweep runs = %+v, want one failed run", runs)
	}
	if _, err := l.AmendTransferScheduleMemo("sched-1", "utilities"); err != nil {
		t.Fatalf("AmendTransferScheduleMemo: %v", err)
	}
	l.Unfreeze("alice")
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("retry sweep runs = %+v, want one fired run", runs)
	}
	if runs[0].TransferID != "sched-1/run/1" {
		t.Errorf("retry transfer id = %q, want sched-1/run/1 (same run)", runs[0].TransferID)
	}
	if got := principalMemo(t, l, "sched-1/run/1"); got != "utilities" {
		t.Errorf("retried run principal memo = %q, want utilities", got)
	}
}

func TestAmendTransferScheduleMemoAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFeeSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleMemo("sched-1", "rent"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleMemoCtx(ctx, "sched-1", "utilities"); err != nil {
		t.Fatalf("AmendTransferScheduleMemoCtx: %v", err)
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
	if after.Memo != "utilities" {
		t.Errorf("restored memo = %q, want utilities", after.Memo)
	}
	after.Memo = before.Memo
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except memo", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_memo_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_memo"] != "rent" ||
		d["memo"] != "utilities" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 rent->utilities USD run_seq 0", d)
	}
}

func TestAmendTransferScheduleMemoClearedRoundTrip(t *testing.T) {
	// A memo cleared back to "" round-trips through snapshots as "",
	// not as a resurrected memo: amend, clear, then export and restore.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleMemo("sched-1", "utilities"); err != nil {
		t.Fatalf("AmendTransferScheduleMemo: %v", err)
	}
	if _, err := l.AmendTransferScheduleMemo("sched-1", ""); err != nil {
		t.Fatalf("AmendTransferScheduleMemo clear: %v", err)
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
	if after.Memo != "" {
		t.Errorf("restored memo = %q, want empty", after.Memo)
	}
	if after.FeeCents != 10 || after.FeeAccount != "fees" {
		t.Errorf("restored fee/account = %d/%q, want 10/fees (untouched)", after.FeeCents, after.FeeAccount)
	}
}
