package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTransferScheduleNextRunAtValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleNextRunAt("sched-unknown", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createEndsAtSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createEndsAtSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleNextRunAt("sched-1", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleNextRunAt("sched-1", now.Add(48*time.Hour)); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// The creation gates, reused: a zero next run time can never
	// fire, and a next run past the live end time could never fire
	// either — it would complete the plan on sight. Every rejected
	// amendment leaves the plan exactly as it was.
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", time.Time{}); !errors.Is(err, ErrScheduleBadNextRunAt) {
		t.Errorf("zero next_run_at: err = %v, want ErrScheduleBadNextRunAt", err)
	}
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(31*24*time.Hour)); !errors.Is(err, ErrScheduleBadEndsAt) {
		t.Errorf("next_run_at past ends_at: err = %v, want ErrScheduleBadEndsAt", err)
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current next run time is a no-op, not a change.
	if got, err := l.AmendTransferScheduleNextRunAt("sched-1", now); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-next_run_at amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// A next run exactly at the end time is legal: the gate rejects
	// only a time *past* the cutoff, and a run due exactly at EndsAt
	// still fires (completion needs NextRunAt past EndsAt).
	if got, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(30*24*time.Hour)); err != nil || !got.NextRunAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("next_run_at at ends_at = %+v, %v, want T+30d, nil", got, err)
	}
	// Restore the original next run time for the field-isolation check.
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now); err != nil {
		t.Fatalf("restore next_run_at: %v", err)
	}

	// Active plan: only NextRunAt changes — EndsAt in particular is
	// not recomputed, and the run sequence is untouched.
	amended, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAt active: %v", err)
	}
	if !amended.NextRunAt.Equal(now.Add(48 * time.Hour)) {
		t.Errorf("amended next_run_at = %v, want %v", amended.NextRunAt, now.Add(48*time.Hour))
	}
	if !amended.EndsAt.Equal(orig.EndsAt) {
		t.Errorf("amended ends_at = %v, want %v (unchanged)", amended.EndsAt, orig.EndsAt)
	}
	amended.NextRunAt = orig.NextRunAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except next_run_at", amended, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the next run
	// time moves.
	l4 := New()
	orig4 := createEndsAtSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleNextRunAt("sched-1", now.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAt paused: %v", err)
	}
	if !paused.NextRunAt.Equal(now.Add(72*time.Hour)) || paused.Status != SchedulePaused {
		t.Errorf("amended paused = next_run_at %v status %q, want %v paused", paused.NextRunAt, paused.Status, now.Add(72*time.Hour))
	}
	paused.NextRunAt = orig4.NextRunAt
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except next_run_at", paused, orig4)
	}
}

func TestAmendTransferScheduleNextRunAtGateUsesLiveEndsAt(t *testing.T) {
	// The gate is judged against the EndsAt at amendment time,
	// not the creation-time one: after the cutoff is pulled in to
	// T+30m (LG-255), a next run of T+1h — inside the creation
	// cutoff but past the live one — is rejected, while T+15m is
	// accepted. Clearing the cutoff to zero (open-ended) accepts
	// any non-zero next run time.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createEndsAtSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", now.Add(30*time.Minute)); err != nil {
		t.Fatalf("AmendTransferScheduleEndsAt: %v", err)
	}
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(time.Hour)); !errors.Is(err, ErrScheduleBadEndsAt) {
		t.Errorf("next_run_at past live ends_at: err = %v, want ErrScheduleBadEndsAt", err)
	}
	if got, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(15*time.Minute)); err != nil || !got.NextRunAt.Equal(now.Add(15*time.Minute)) {
		t.Errorf("next_run_at within live ends_at = %+v, %v, want T+15m, nil", got, err)
	}
	if _, err := l.AmendTransferScheduleEndsAt("sched-1", time.Time{}); err != nil {
		t.Fatalf("clear ends_at: %v", err)
	}
	if got, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(365*24*time.Hour)); err != nil || !got.NextRunAt.Equal(now.Add(365*24*time.Hour)) {
		t.Errorf("next_run_at open-ended = %+v, %v, want T+365d, nil", got, err)
	}
}

func TestAmendTransferScheduleNextRunAtSweepReadsLiveTime(t *testing.T) {
	// A moved-up next run fires on the next sweep, once — missed
	// nominal runs count as skipped, never backfilled. A moved-out
	// next run delays firing until the new time, with no sweep in
	// between firing anything.
	now := time.Now().UTC().Truncate(time.Second)
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.ID = "sched-1"
	s.Interval = 24 * time.Hour
	s.NextRunAt = now.Add(24 * time.Hour)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAt past: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("past runs = %+v, want one fired run", runs)
	}
	if runs[0].SkippedRuns != 2 {
		t.Errorf("skipped_runs = %d, want 2 (nominals T-48h and T-24h missed, no backfill)", runs[0].SkippedRuns)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); !got.NextRunAt.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("advanced next_run_at = %v, want %v", got.NextRunAt, now.Add(24*time.Hour))
	}
	if bal := l.BalanceIn("alice", "USD"); bal != 100000-1000 {
		t.Errorf("alice balance = %d, want %d (exactly one firing)", bal, 100000-1000)
	}

	l2 := New()
	fundAccount(t, l2, "alice", 100000, "USD")
	s2 := testSchedule(now)
	s2.ID = "sched-1"
	s2.Interval = 24 * time.Hour
	s2.NextRunAt = now
	if _, _, err := l2.CreateTransferSchedule(s2); err != nil {
		t.Fatalf("create future: %v", err)
	}
	if _, err := l2.AmendTransferScheduleNextRunAt("sched-1", now.Add(24*time.Hour)); err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAt future: %v", err)
	}
	if runs := l2.SweepDue(now); len(runs) != 0 {
		t.Fatalf("sweep at old time runs = %+v, want none (firing delayed)", runs)
	}
	if runs := l2.SweepDue(now.Add(24 * time.Hour)); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep at new time runs = %+v, want one fired run", runs)
	}
}

func TestAmendTransferScheduleNextRunAtPausedWaitsForResume(t *testing.T) {
	// A paused plan's revision waits: no sweep fires while paused,
	// and the first run after resume fires at the amended time.
	now := time.Now().UTC().Truncate(time.Second)
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.ID = "sched-1"
	s.Interval = 24 * time.Hour
	s.NextRunAt = now.Add(24 * time.Hour)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := l.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now.Add(-time.Hour)); err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAt paused: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 0 {
		t.Fatalf("sweep while paused runs = %+v, want none", runs)
	}
	if _, err := l.ResumeTransferSchedule("sched-1"); err != nil {
		t.Fatalf("ResumeTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep after resume runs = %+v, want one fired run at amended time", runs)
	}
}

func TestAmendTransferScheduleNextRunAtAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createEndsAtSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleNextRunAt("sched-1", now); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	newNextRunAt := now.Add(48 * time.Hour)
	if _, err := l.AmendTransferScheduleNextRunAtCtx(ctx, "sched-1", newNextRunAt); err != nil {
		t.Fatalf("AmendTransferScheduleNextRunAtCtx: %v", err)
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
	if !after.NextRunAt.Equal(newNextRunAt) {
		t.Errorf("restored next_run_at = %v, want %v", after.NextRunAt, newNextRunAt)
	}
	after.NextRunAt = before.NextRunAt
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except next_run_at", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_next_run_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_next_run_at"] != now.UTC().Format(time.RFC3339) ||
		d["next_run_at"] != newNextRunAt.UTC().Format(time.RFC3339) ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 %v->%v USD run_seq 0", d, now, newNextRunAt)
	}
}
