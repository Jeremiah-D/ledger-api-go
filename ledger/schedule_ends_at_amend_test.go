package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createEndsAtSchedule registers one active plan for the end-time
// amendment tests: alice pays bob 1000 cents daily, with a memo, an
// explicit 10-cent fee, and an end time 30 days out, so the amendment
// tests can prove every other field survives untouched.
func createEndsAtSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
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

func TestAmendTransferScheduleEndsAtValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleEndsAt("sched-unknown", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createEndsAtSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createEndsAtSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleEndsAt("sched-1", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleEndsAt("sched-1", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// A non-zero end time at or before the live NextRunAt reuses the
	// creation gate, and every rejected amendment leaves the plan
	// exactly as it was.
	for _, bad := range []time.Time{now, now.Add(-time.Hour)} {
		if _, err := l.AmendTransferScheduleEndsAt("sched-1", bad); !errors.Is(err, ErrScheduleBadEndsAt) {
			t.Errorf("ends_at %v: err = %v, want ErrScheduleBadEndsAt", bad, err)
		}
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current end time is a no-op, not a change.
	if got, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(30*24*time.Hour)); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-ends_at amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only EndsAt changes — NextRunAt in particular is
	// not recomputed.
	amended, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(60*24*time.Hour))
	if err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt active: %v", err)
	}
	if !amended.EndsAt.Equal(now.Add(60 * 24 * time.Hour)) {
		t.Errorf("amended ends_at = %v, want %v", amended.EndsAt, now.Add(60*24*time.Hour))
	}
	if !amended.NextRunAt.Equal(orig.NextRunAt) {
		t.Errorf("amended next_run_at = %v, want %v (unchanged)", amended.NextRunAt, orig.NextRunAt)
	}
	amended.EndsAt = orig.EndsAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except ends_at", amended, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the end time moves.
	l4 := New()
	orig4 := createEndsAtSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleEndsAt("sched-1", now.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt paused: %v", err)
	}
	if !paused.EndsAt.Equal(now.Add(90*24*time.Hour)) || paused.Status != SchedulePaused {
		t.Errorf("amended paused = ends_at %v status %q, want %v paused", paused.EndsAt, paused.Status, now.Add(90*24*time.Hour))
	}
	paused.EndsAt = orig4.EndsAt
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except ends_at", paused, orig4)
	}
}

func TestAmendTransferScheduleEndsAtGateUsesLiveNextRunAt(t *testing.T) {
	// The gate is judged against the NextRunAt at amendment time,
	// not the creation-time one: after one firing advances NextRunAt
	// to T+24h, an end time of T+12h — after the creation NextRunAt
	// but before the live one — is rejected, while T+25h is accepted.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createEndsAtSchedule(t, l, "sched-1", now)
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if !live.NextRunAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("live next_run_at = %v, want %v", live.NextRunAt, now.Add(24*time.Hour))
	}
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(12*time.Hour)); !errors.Is(err, ErrScheduleBadEndsAt) {
		t.Errorf("ends_at before live next_run_at: err = %v, want ErrScheduleBadEndsAt", err)
	}
	if got, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(25*time.Hour)); err != nil || !got.EndsAt.Equal(now.Add(25*time.Hour)) {
		t.Errorf("ends_at after live next_run_at = %+v, %v, want T+25h, nil", got, err)
	}
}

func TestAmendTransferScheduleEndsAtCompletion(t *testing.T) {
	// Completion is still judged on the advanced NextRunAt: with the
	// end time pulled in to T+30m, the post-firing NextRunAt (T+1h)
	// lands past it and the plan completes, while an unamended
	// control plan with a far end time stays active.
	now := time.Now().UTC().Truncate(time.Second)
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(30*time.Minute)); err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.Status != ScheduleCompleted {
		t.Errorf("status after firing = %q, want completed (advanced NextRunAt T+1h past EndsAt T+30m)", got.Status)
	}

	control := New()
	fundAccount(t, control, "alice", 100000, "USD")
	if _, _, err := control.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("control create: %v", err)
	}
	if runs := control.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("control runs = %+v, want one fired run", runs)
	}
	if got, _ := control.GetTransferSchedule("sched-1"); got.Status != ScheduleActive {
		t.Errorf("control status = %q, want active (T+1h within EndsAt)", got.Status)
	}
}

func TestAmendTransferScheduleEndsAtClearToOpenEnded(t *testing.T) {
	// Clearing the cutoff to zero makes the plan open-ended: a firing
	// whose advanced NextRunAt would have passed the old end time
	// leaves the plan active instead of completing it, and already-
	// fired runs are untouched.
	now := time.Now().UTC().Truncate(time.Second)
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	cleared, err := l.AmendTransferScheduleEndsAt("sched-1", time.Time{})
	if err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt clear: %v", err)
	}
	if !cleared.EndsAt.IsZero() {
		t.Errorf("cleared ends_at = %v, want zero", cleared.EndsAt)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	got, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if got.Status != ScheduleActive {
		t.Errorf("status after firing past old ends_at = %q, want active (cutoff cleared)", got.Status)
	}
	if !got.NextRunAt.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, now.Add(24*time.Hour))
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 1 {
		t.Errorf("run history = %d runs, %v, want 1", len(runs), err)
	}
	// Clearing an already open-ended plan is a no-op, not a change.
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", time.Time{}); err != nil {
		t.Errorf("re-clearing open-ended plan: err = %v, want nil", err)
	}
}

func TestAmendTransferScheduleEndsAtAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createEndsAtSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(30*24*time.Hour)); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	newEndsAt := now.Add(60 * 24 * time.Hour)
	if _, err := l.AmendTransferScheduleEndsAtCtx(ctx, "sched-1", newEndsAt); err != nil {
		t.Fatalf("AmendTransferScheduleEndsAtCtx: %v", err)
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
	if !after.EndsAt.Equal(newEndsAt) {
		t.Errorf("restored ends_at = %v, want %v", after.EndsAt, newEndsAt)
	}
	after.EndsAt = before.EndsAt
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except ends_at", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_ends_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_ends_at"] != now.Add(30*24*time.Hour).UTC().Format(time.RFC3339) ||
		d["ends_at"] != newEndsAt.UTC().Format(time.RFC3339) ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 %v->%v USD run_seq 0", d, now.Add(30*24*time.Hour), newEndsAt)
	}
}

func TestAmendTransferScheduleEndsAtClearedSnapshotRoundTrip(t *testing.T) {
	// A cleared (zero) end time round-trips through snapshots as
	// zero, not as a resurrected cutoff.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createEndsAtSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", time.Time{}); err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt clear: %v", err)
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
	if !after.EndsAt.IsZero() {
		t.Errorf("restored ends_at = %v, want zero", after.EndsAt)
	}
}
