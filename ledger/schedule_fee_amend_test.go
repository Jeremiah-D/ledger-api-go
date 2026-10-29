package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createFeeSchedule registers one active plan for the fee amendment
// tests: alice pays bob 1000 cents hourly, with an explicit 10-cent
// fee to the fees account, so the amendment tests can prove every
// other field survives untouched.
func createFeeSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
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
		Interval:    time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule %s: %v", id, err)
	}
	return sched
}

func TestAmendTransferScheduleFeeValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleFee("sched-unknown", 25); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFeeSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFeeSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleFee("sched-1", 25); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleFee("sched-1", 25); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// The creation fee gate is reused: a negative fee is rejected,
	// and a positive fee on a plan with no fee account is rejected
	// (the account itself is never changed by this amendment), and
	// every rejected amendment leaves the plan exactly as it was.
	if _, err := l.AmendTransferScheduleFee("sched-1", -1); !errors.Is(err, ErrScheduleBadFee) {
		t.Errorf("negative fee: err = %v, want ErrScheduleBadFee", err)
	}
	lNoFee := New()
	noFeeOrig, _, err := lNoFee.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now,
	})
	if err != nil {
		t.Fatalf("create no-fee schedule: %v", err)
	}
	if _, err := lNoFee.AmendTransferScheduleFee("sched-1", 10); !errors.Is(err, ErrScheduleBadFee) {
		t.Errorf("positive fee without fee account: err = %v, want ErrScheduleBadFee", err)
	}
	if got, err := lNoFee.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, noFeeOrig) {
		t.Errorf("schedule after rejected positive fee = %+v, %v, want %+v", got, err, noFeeOrig)
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current fee is a no-op, not a change.
	if got, err := l.AmendTransferScheduleFee("sched-1", 10); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-fee amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only FeeCents changes — FeeAccount and SkipFee
	// in particular are untouched.
	amended, err := l.AmendTransferScheduleFee("sched-1", 25)
	if err != nil {
		t.Fatalf("AmendTransferScheduleFee active: %v", err)
	}
	if amended.FeeCents != 25 {
		t.Errorf("amended fee = %d, want 25", amended.FeeCents)
	}
	if amended.FeeAccount != "fees" || amended.SkipFee {
		t.Errorf("amended fee account/skip = %q/%v, want fees/false (unchanged)", amended.FeeAccount, amended.SkipFee)
	}
	amended.FeeCents = orig.FeeCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except fee", amended, orig)
	}

	// Amending to zero is legal and keeps the fee account, so a
	// later positive fee still has an account to charge.
	cleared, err := l.AmendTransferScheduleFee("sched-1", 0)
	if err != nil {
		t.Fatalf("AmendTransferScheduleFee clear: %v", err)
	}
	if cleared.FeeCents != 0 || cleared.FeeAccount != "fees" {
		t.Errorf("cleared fee = %d account %q, want 0 fees", cleared.FeeCents, cleared.FeeAccount)
	}
	if back, err := l.AmendTransferScheduleFee("sched-1", 5); err != nil || back.FeeCents != 5 {
		t.Errorf("fee after clearing = %+v, %v, want 5, nil", back, err)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the fee moves.
	l4 := New()
	orig4 := createFeeSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleFee("sched-1", 30)
	if err != nil {
		t.Fatalf("AmendTransferScheduleFee paused: %v", err)
	}
	if paused.FeeCents != 30 || paused.Status != SchedulePaused {
		t.Errorf("amended paused = fee %d status %q, want 30 paused", paused.FeeCents, paused.Status)
	}
	paused.FeeCents = orig4.FeeCents
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except fee", paused, orig4)
	}
}

func TestAmendTransferScheduleFeeNextRelease(t *testing.T) {
	// The new fee governs the next release with no firing-path
	// change: the first run books its original 10-cent fee, the
	// amendment itself posts nothing, and the second run books the
	// amended 25-cent fee — the already-released run's fee leg is
	// never rewritten.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createFeeSchedule(t, l, "sched-1", now)
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("fees", "USD"); got != 10 {
		t.Fatalf("fees after first run = %d, want 10", got)
	}
	if _, err := l.AmendTransferScheduleFee("sched-1", 25); err != nil {
		t.Fatalf("AmendTransferScheduleFee: %v", err)
	}
	if got := l.BalanceIn("fees", "USD"); got != 10 {
		t.Fatalf("fees after amendment = %d, want 10 (amendment posts nothing)", got)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("fees", "USD"); got != 35 {
		t.Errorf("fees after second run = %d, want 35 (10 original + 25 amended)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-2*1000-35 {
		t.Errorf("alice after two runs = %d, want %d", got, 100000-2*1000-35)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 {
		t.Errorf("run history = %d runs, %v, want 2", len(runs), err)
	}
}

func TestAmendTransferScheduleFeeAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFeeSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleFee("sched-1", 10); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleFeeCtx(ctx, "sched-1", 25); err != nil {
		t.Fatalf("AmendTransferScheduleFeeCtx: %v", err)
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
	if after.FeeCents != 25 {
		t.Errorf("restored fee = %d, want 25", after.FeeCents)
	}
	after.FeeCents = before.FeeCents
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except fee", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_fee_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_fee_cents"] != float64(10) ||
		d["fee_cents"] != float64(25) ||
		d["fee_account"] != "fees" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 10->25 fees USD run_seq 0", d)
	}
}

func TestAmendTransferScheduleFeeClearedSnapshotRoundTrip(t *testing.T) {
	// A fee cleared to zero round-trips through snapshots as zero,
	// not as a resurrected charge.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleFee("sched-1", 0); err != nil {
		t.Fatalf("AmendTransferScheduleFee clear: %v", err)
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
	if after.FeeCents != 0 {
		t.Errorf("restored fee = %d, want zero", after.FeeCents)
	}
	if after.FeeAccount != "fees" {
		t.Errorf("restored fee account = %q, want fees (unchanged)", after.FeeAccount)
	}
}
