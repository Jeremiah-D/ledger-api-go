package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Schedule-sweeper interval runtime revision tests (LG-317). They
// reuse the audit_sink_test.go waiter (waitForSink), mustAuditLog
// from ledger_audit_test.go, and seedBalance from
// ledger_transfer_test.go.

// retunedScheduleSweepEvents stops the sweeper, closes the audit log
// (which drains the async writer), and returns every
// schedule_sweep_interval_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedScheduleSweepEvents(t *testing.T, sw *ScheduleSweeper, al *AuditLog) []AuditEvent {
	t.Helper()
	sw.Stop()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "schedule_sweep_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startScheduleRetuneTestSweeper starts a sweeper on a ledger with
// one due transfer schedule (50ms plan cadence) and an audit log
// attached. The plan fires on the first sweep; its NextRunAt then
// advances past now, so later ticks sweep an empty table unless a
// test uses a shorter plan interval.
func startScheduleRetuneTestSweeper(t *testing.T, interval time.Duration) (*Ledger, *ScheduleSweeper, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	seedBalance(t, l, "payer", 10_000_000)
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "retune-swept",
		From:        "payer",
		To:          "payee",
		AmountCents: 100,
		Currency:    "USD",
		Interval:    50 * time.Millisecond,
		NextRunAt:   time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sw := StartScheduleSweeper(ctx, l, interval, nil)
	if sw == nil {
		cancel()
		t.Fatalf("sweeper not started")
	}
	return l, sw, al, cancel
}

// waitForScheduleTicks waits until the sweeper has completed at least
// n ticks.
func waitForScheduleTicks(t *testing.T, sw *ScheduleSweeper, n uint64, timeout time.Duration) {
	t.Helper()
	waitForSink(t, timeout, "ticks", func() bool { return sw.Ticks() >= n })
}

func TestSetScheduleSweepIntervalInvalidRejectedLiveUnchanged(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Second, -time.Millisecond} {
		if prev, err := sw.SetInterval(d); !errors.Is(err, ErrInvalidScheduleSweepInterval) {
			t.Errorf("SetInterval(%v): err = %v, want ErrInvalidScheduleSweepInterval", d, err)
		} else if prev != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): returned %v, want the live 500ms", d, prev)
		}
		if got := sw.Interval(); got != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): live interval moved to %v, want unchanged 500ms", d, got)
		}
	}
	if events := retunedScheduleSweepEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("rejected revisions wrote %d audit events, want 0", len(events))
	}
}

func TestSetScheduleSweepIntervalReturnsPreviousValue(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartScheduleSweeper(ctx, l, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()

	if got := sw.Interval(); got != 500*time.Millisecond {
		t.Fatalf("Interval() = %v, want construction-time 500ms", got)
	}
	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := sw.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v, want 10ms after the revision", got)
	}
	// The getter follows a second revision the same way.
	prev, err = sw.SetInterval(250 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 10ms", prev)
	}
	if got := sw.Interval(); got != 250*time.Millisecond {
		t.Fatalf("Interval() = %v, want 250ms after the second revision", got)
	}
}

func TestSetScheduleSweepIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	// One real revision, as a positive control: it must audit.
	if _, err := sw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	// A same-value revision is a no-op: same return, no new audit.
	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("same-value revision returned %v, want the live 10ms", prev)
	}
	if events := retunedScheduleSweepEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-op", len(events))
	}
}

func TestSetScheduleSweepIntervalNextTickUsesNewInterval(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForScheduleTicks(t, sw, 1, 3*time.Second)
	if got := sw.Ticks(); got != 1 {
		t.Fatalf("ticks after first wait = %d, want exactly 1 on the 500ms cadence", got)
	}

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := sw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := sw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitForScheduleTicks(t, sw, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetScheduleSweepIntervalInFlightSweepNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	seedBalance(t, l, "payer", 10_000_000)
	// A plan whose 10ms cadence is shorter than the 15ms sweep
	// rhythm: it is due on every tick, so each tick fires exactly
	// one run and RunSeq tracks the tick count.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "retune-busy",
		From:        "payer",
		To:          "payee",
		AmountCents: 100,
		Currency:    "USD",
		Interval:    10 * time.Millisecond,
		NextRunAt:   time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartScheduleSweeper(ctx, l, 15*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the sweeper fire a couple of runs on the old cadence.
	waitForScheduleTicks(t, sw, 2, 3*time.Second)

	// Revise mid-stream: the run loop only selects between sweeps,
	// so a sweep already in progress runs to completion and no run
	// is dropped or duplicated by the revision.
	if _, err := sw.SetInterval(500 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval mid-sweep: %v", err)
	}
	waitForScheduleTicks(t, sw, 3, 3*time.Second)

	// Every tick fired exactly one run: FiredTotal tracks RunSeq
	// with no gap and no duplicate.
	got, err := l.GetTransferSchedule("retune-busy")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if got.RunSeq != sw.FiredTotal() {
		t.Fatalf("RunSeq = %d, FiredTotal = %d: want equal — a revision must neither drop nor duplicate a run", got.RunSeq, sw.FiredTotal())
	}
	if got := sw.FailedTotal(); got != 0 {
		t.Fatalf("FailedTotal = %d, want 0", got)
	}
	// The revision never touched the plan's own cadence.
	if got.Interval != 10*time.Millisecond {
		t.Fatalf("plan Interval = %v after the revision, want unchanged 10ms", got.Interval)
	}

	// The next tick follows the loosened 500ms cadence: nothing
	// within 200ms of the last tick, then a tick arrives.
	time.Sleep(200 * time.Millisecond)
	if n := sw.Ticks(); n != 3 {
		t.Fatalf("ticks = %d within 200ms of the last tick, want still 3 on the loosened 500ms rhythm", n)
	}
	waitForScheduleTicks(t, sw, 4, 3*time.Second)
}

func TestSetScheduleSweepIntervalConcurrentRevisionAndSweepRaceFree(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the
	// sweeper ticks underneath: -race must stay silent, and the
	// sweeper must keep ticking on some positive interval.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = sw.SetInterval(d)
				_ = sw.Interval()
			}
		}(g)
	}
	wg.Wait()
	if got := sw.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	waitForScheduleTicks(t, sw, 1, 3*time.Second)
	if n := sw.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the sweeper still ticking")
	}
}

func TestSetScheduleSweepIntervalCtxActorAttribution(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetIntervalCtx(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedScheduleSweepEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_sweep_interval_ms"]; got != float64(500) {
		t.Fatalf("first previous_sweep_interval_ms = %v, want 500", got)
	}
	if got := events[0].Details["current_sweep_interval_ms"]; got != float64(10) {
		t.Fatalf("first current_sweep_interval_ms = %v, want 10", got)
	}
	if events[1].Actor != "SetInterval" {
		t.Fatalf("second event Actor = %q, want SetInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_sweep_interval_ms"]; got != float64(10) {
		t.Fatalf("second previous_sweep_interval_ms = %v, want 10", got)
	}
	if got := events[1].Details["current_sweep_interval_ms"]; got != float64(250) {
		t.Fatalf("second current_sweep_interval_ms = %v, want 250", got)
	}
}

func TestSetScheduleSweepIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	events := retunedScheduleSweepEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetInterval" {
		t.Errorf("Actor = %q, want SetInterval", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_sweep_interval_ms"]; got != float64(500) {
		t.Errorf("previous_sweep_interval_ms = %v, want 500", got)
	}
	if got := ev.Details["current_sweep_interval_ms"]; got != float64(10) {
		t.Errorf("current_sweep_interval_ms = %v, want 10", got)
	}
	// Only the two numeric ms fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current sweep interval ms", ev.Details)
	}
}

func TestSetScheduleSweepIntervalNoRestartContinuousTicks(t *testing.T) {
	_, sw, al, cancel := startScheduleRetuneTestSweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the sweeper tick on the original cadence first.
	waitForScheduleTicks(t, sw, 2, 3*time.Second)
	ticksBefore := sw.Ticks()
	firedBefore := sw.FiredTotal()
	if firedBefore == 0 {
		t.Fatalf("FiredTotal = 0 after %d ticks, want the due schedule firing", ticksBefore)
	}

	// Revise the cadence: the same goroutine keeps running — the
	// counters never reset and the ticks keep accumulating on the
	// new interval instead of restarting from zero.
	if _, err := sw.SetInterval(30 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForScheduleTicks(t, sw, ticksBefore+3, 3*time.Second)
	if got := sw.Ticks(); got < ticksBefore+3 {
		t.Fatalf("Ticks() = %d, want >= %d: ticks must continue, not restart", got, ticksBefore+3)
	}
	if got := sw.FiredTotal(); got < firedBefore {
		t.Fatalf("FiredTotal() = %d, want >= %d: counters must keep accumulating across the revision", got, firedBefore)
	}
	if got := sw.FailedTotal(); got != 0 {
		t.Fatalf("FailedTotal() = %d, want 0", got)
	}
}

func TestSetScheduleSweepIntervalRevisionLeavesPlansUntouched(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	seedBalance(t, l, "payer", 10_000_000)
	// A plan that is not due for an hour: no sweep fires it, so any
	// change to its fields after a revision could only come from
	// the revision itself.
	nextRun := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "retune-untouched",
		From:        "payer",
		To:          "payee",
		AmountCents: 100,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   nextRun,
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	before, err := l.GetTransferSchedule("retune-untouched")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartScheduleSweeper(ctx, l, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	if _, err := sw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForScheduleTicks(t, sw, 2, 3*time.Second)

	after, err := l.GetTransferSchedule("retune-untouched")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if after.Interval != before.Interval || !after.NextRunAt.Equal(before.NextRunAt) ||
		after.RunSeq != before.RunSeq || after.Status != before.Status {
		t.Fatalf("plan changed across the revision:\nbefore = %+v\nafter  = %+v", before, after)
	}
	if runs, err := l.ListScheduleRuns("retune-untouched", 0); err != nil {
		t.Fatalf("ListScheduleRuns: %v", err)
	} else if len(runs) != 0 {
		t.Fatalf("run history = %d entries, want 0: the revision must not fire a run", len(runs))
	}
	if got := sw.FiredTotal(); got != 0 {
		t.Fatalf("FiredTotal = %d, want 0: no plan was due", got)
	}
}
