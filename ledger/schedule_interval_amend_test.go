package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createIntervalSchedule registers one active plan for the interval
// amendment tests: alice pays bob 1000 cents daily, with a memo, an
// explicit 10-cent fee, and an end time far in the future, so the
// amendment tests can prove every other field survives untouched.
func createIntervalSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
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

func TestAmendTransferScheduleIntervalValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleInterval("sched-unknown", 48*time.Hour); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createIntervalSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createIntervalSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleInterval("sched-1", 48*time.Hour); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleInterval("sched-1", 48*time.Hour); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Non-positive intervals reuse the creation gate, and every
	// rejected amendment leaves the plan exactly as it was.
	for _, bad := range []time.Duration{0, -time.Hour} {
		if _, err := l.AmendTransferScheduleInterval("sched-1", bad); !errors.Is(err, ErrScheduleBadInterval) {
			t.Errorf("interval %v: err = %v, want ErrScheduleBadInterval", bad, err)
		}
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current interval is a no-op, not a change.
	if got, err := l.AmendTransferScheduleInterval("sched-1", 24*time.Hour); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-interval amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only Interval changes — NextRunAt in particular
	// is not recomputed.
	amended, err := l.AmendTransferScheduleInterval("sched-1", 48*time.Hour)
	if err != nil {
		t.Fatalf("AmendTransferScheduleInterval active: %v", err)
	}
	if amended.Interval != 48*time.Hour {
		t.Errorf("amended interval = %v, want 48h", amended.Interval)
	}
	if !amended.NextRunAt.Equal(orig.NextRunAt) {
		t.Errorf("amended next_run_at = %v, want %v (unchanged)", amended.NextRunAt, orig.NextRunAt)
	}
	amended.Interval = orig.Interval
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except interval", amended, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the interval
	// moves.
	l4 := New()
	orig4 := createIntervalSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleInterval("sched-1", 72*time.Hour)
	if err != nil {
		t.Fatalf("AmendTransferScheduleInterval paused: %v", err)
	}
	if paused.Interval != 72*time.Hour || paused.Status != SchedulePaused {
		t.Errorf("amended paused = interval %v status %q, want 72h paused", paused.Interval, paused.Status)
	}
	paused.Interval = orig4.Interval
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except interval", paused, orig4)
	}
}

func TestAmendTransferScheduleIntervalNextAdvance(t *testing.T) {
	// The new interval governs the advancement after the next
	// release, not the release moment itself: NextRunAt is
	// untouched by the amendment, then moves by the new interval
	// when the due run fires.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createIntervalSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleInterval("sched-1", 48*time.Hour); err != nil {
		t.Fatalf("AmendTransferScheduleInterval: %v", err)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); !got.NextRunAt.Equal(now) {
		t.Fatalf("next_run_at after amend = %v, want %v (not recomputed)", got.NextRunAt, now)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); !got.NextRunAt.Equal(now.Add(48 * time.Hour)) {
		t.Errorf("next_run_at after firing = %v, want %v (new 48h interval, not 24h)", got.NextRunAt, now.Add(48*time.Hour))
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000 (amount unchanged by interval amend)", got)
	}
}

func TestAmendTransferScheduleIntervalPausedResumeAdvance(t *testing.T) {
	// A paused plan's revision waits with the plan and governs the
	// first run after resume.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createIntervalSchedule(t, l, "sched-1", now)
	if _, err := l.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	if _, err := l.AmendTransferScheduleInterval("sched-1", 48*time.Hour); err != nil {
		t.Fatalf("AmendTransferScheduleInterval paused: %v", err)
	}
	if _, err := l.ResumeTransferSchedule("sched-1"); err != nil {
		t.Fatalf("ResumeTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); !got.NextRunAt.Equal(now.Add(48 * time.Hour)) {
		t.Errorf("next_run_at after resume firing = %v, want %v", got.NextRunAt, now.Add(48*time.Hour))
	}
}

func TestAmendTransferScheduleIntervalBusinessDayAdvance(t *testing.T) {
	// Business-day cadence converts the NEW interval to days at
	// fire time: 24h -> 72h moves the advancement from 1 business
	// day (Friday -> Monday) to 3 (Friday -> Wednesday).
	l := New()
	fundAccount(t, l, "payer", 10000000, "USD")
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:                 "sched-bd",
		From:               "payer",
		To:                 "payee",
		AmountCents:        1000,
		Currency:           "USD",
		Interval:           24 * time.Hour,
		NextRunAt:          bdFriday,
		BusinessDayCadence: true,
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if _, err := l.AmendTransferScheduleInterval("sched-bd", 72*time.Hour); err != nil {
		t.Fatalf("AmendTransferScheduleInterval: %v", err)
	}
	if got, _ := l.GetTransferSchedule("sched-bd"); !got.NextRunAt.Equal(bdFriday) {
		t.Fatalf("next_run_at after amend = %v, want %v (not recomputed)", got.NextRunAt, bdFriday)
	}
	runs := l.SweepDue(bdFriday.Add(2 * time.Hour))
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	want := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	if got, _ := l.GetTransferSchedule("sched-bd"); !got.NextRunAt.Equal(want) {
		t.Errorf("next_run_at after firing = %v, want %v (T+3 business days under new 72h interval)", got.NextRunAt, want)
	}
}

func TestAmendTransferScheduleIntervalEndsAtCompletion(t *testing.T) {
	// EndsAt completion is judged on the advanced NextRunAt: with
	// the new 48h interval the post-firing NextRunAt lands past
	// EndsAt (T+36h) and the plan completes, while an unamended
	// control plan advancing 24h stays active.
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
		EndsAt:      now.Add(36 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if _, err := l.AmendTransferScheduleInterval("sched-1", 48*time.Hour); err != nil {
		t.Fatalf("AmendTransferScheduleInterval: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.Status != ScheduleCompleted {
		t.Errorf("status after firing = %q, want completed (advanced NextRunAt T+48h past EndsAt T+36h)", got.Status)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); !got.NextRunAt.Equal(now.Add(48 * time.Hour)) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, now.Add(48*time.Hour))
	}

	control := New()
	fundAccount(t, control, "alice", 100000, "USD")
	if _, _, err := control.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(36 * time.Hour),
	}); err != nil {
		t.Fatalf("control create: %v", err)
	}
	if runs := control.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("control runs = %+v, want one fired run", runs)
	}
	if got, _ := control.GetTransferSchedule("sched-1"); got.Status != ScheduleActive {
		t.Errorf("control status = %q, want active (T+24h within EndsAt)", got.Status)
	}
}

func TestAmendTransferScheduleIntervalRunHistoryUnchanged(t *testing.T) {
	// Amending after a run fired leaves the run history, the
	// post-firing NextRunAt, and every other field untouched.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now.Add(-time.Minute),
		EndsAt:      now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	orig, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	runsBefore, err := l.ListScheduleRuns("sched-1", 0)
	if err != nil {
		t.Fatalf("ListScheduleRuns: %v", err)
	}
	amended, err := l.AmendTransferScheduleInterval("sched-1", 2*time.Hour)
	if err != nil {
		t.Fatalf("AmendTransferScheduleInterval: %v", err)
	}
	if amended.Interval != 2*time.Hour {
		t.Errorf("amended interval = %v, want 2h", amended.Interval)
	}
	amended.Interval = orig.Interval
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except interval", amended, orig)
	}
	runsAfter, err := l.ListScheduleRuns("sched-1", 0)
	if err != nil {
		t.Fatalf("ListScheduleRuns after: %v", err)
	}
	if !reflect.DeepEqual(runsAfter, runsBefore) {
		t.Errorf("run history after amend = %+v, want %+v", runsAfter, runsBefore)
	}
}

func TestAmendTransferScheduleIntervalAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createIntervalSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleInterval("sched-1", 24*time.Hour); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleIntervalCtx(ctx, "sched-1", 48*time.Hour); err != nil {
		t.Fatalf("AmendTransferScheduleIntervalCtx: %v", err)
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
	if after.Interval != 48*time.Hour {
		t.Errorf("restored interval = %v, want 48h", after.Interval)
	}
	after.Interval = before.Interval
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except interval", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_interval_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_interval"] != "24h0m0s" ||
		d["interval"] != "48h0m0s" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 24h0m0s->48h0m0s USD run_seq 0", d)
	}
}
