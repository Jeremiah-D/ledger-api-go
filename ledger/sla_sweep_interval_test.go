package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Settlement-SLA-sweeper interval runtime revision tests (LG-322).
// They reuse mustAuditLog from ledger_audit_test.go, seedPayoutLedger
// / approveSubmitPayout from ledger_payout_test.go,
// draftPayoutWithRail from ledger_sla_test.go, and waitFor from
// ledger_hold_sweep_test.go. The fednow rail SLA is 1ms so a ticking
// sweeper breaches a submitted payout's watch in real time.

// retunedSLASweepEvents stops the sweeper, closes the audit log
// (which drains the async writer), and returns every
// sla_sweep_interval_retuned event recorded in it. The ledger must
// not be used after this call.
func retunedSLASweepEvents(t *testing.T, sw *SLASweeper, al *AuditLog) []AuditEvent {
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
		if ev.Op == "sla_sweep_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startRetuneTestSLASweeper starts an SLA sweeper on a ledger with
// an audit log attached. The fednow rail SLA is 1ms, so a submitted
// payout's watch breaches on the next tick in real time.
func startRetuneTestSLASweeper(t *testing.T, interval time.Duration) (*Ledger, *SLASweeper, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	if err := l.SetSLAConfig("fednow", time.Millisecond); err != nil {
		t.Fatalf("SetSLAConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sw := StartSLASweeper(ctx, l, interval, nil)
	if sw == nil {
		cancel()
		t.Fatalf("sweeper not started")
	}
	return l, sw, al, cancel
}

// submitFednowPayout drafts, approves, and submits a fednow-rail
// payout, opening an SLA watch under its id.
func submitFednowPayout(t *testing.T, l *Ledger, id string) {
	t.Helper()
	draftPayoutWithRail(t, l, id, 100_000, "fednow")
	approveSubmitPayout(t, l, id, "bob")
}

// waitForWatchBreached waits until the named watch has breached.
func waitForWatchBreached(t *testing.T, l *Ledger, id string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "watch "+id+" to breach", func() bool {
		w, ok := l.SLAWatch(id)
		return ok && w.Breached
	})
}

// waitForSLATicks waits until the SLA sweeper has completed at
// least n ticks.
func waitForSLATicks(t *testing.T, sw *SLASweeper, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "ticks", func() bool { return sw.Ticks() >= n })
}

func TestSetSlaSweepIntervalInvalidRejectedLiveUnchanged(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Second, -time.Millisecond} {
		if prev, err := sw.SetInterval(d); !errors.Is(err, ErrInvalidSlaSweepInterval) {
			t.Errorf("SetInterval(%v): err = %v, want ErrInvalidSlaSweepInterval", d, err)
		} else if prev != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): returned %v, want the live 500ms", d, prev)
		}
		if got := sw.Interval(); got != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): live interval moved to %v, want unchanged 500ms", d, got)
		}
	}
	if events := retunedSLASweepEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("rejected revisions wrote %d audit events, want 0", len(events))
	}
}

func TestSetSlaSweepIntervalReturnsPreviousValue(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartSLASweeper(ctx, l, 500*time.Millisecond, nil)
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

func TestSetSlaSweepIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
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
	if events := retunedSLASweepEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-op", len(events))
	}
}

func TestSetSlaSweepIntervalNextTickUsesNewInterval(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForSLATicks(t, sw, 1, 3*time.Second)
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
	waitForSLATicks(t, sw, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetSlaSweepIntervalInFlightSweepNotInterrupted(t *testing.T) {
	l, sw, al, cancel := startRetuneTestSLASweeper(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// A watch already overdue: the in-flight ticks breach it on
	// the original cadence.
	submitFednowPayout(t, l, "po-sla-1")
	waitForWatchBreached(t, l, "po-sla-1", 3*time.Second)
	if got := sw.BreachedTotal(); got != 1 {
		t.Fatalf("BreachedTotal = %d, want 1 after the first breach", got)
	}

	// Revise mid-stream while the sweeper keeps ticking: the
	// revision must not interrupt, drop, or double-count sweeps.
	if _, err := sw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval mid-sweep: %v", err)
	}
	// A second payout submitted after the revision must still be
	// breached by a later tick — the sweeper keeps working on the
	// new cadence — and the first breach is never counted twice.
	submitFednowPayout(t, l, "po-sla-2")
	waitForWatchBreached(t, l, "po-sla-2", 3*time.Second)
	if got := sw.BreachedTotal(); got != 2 {
		t.Fatalf("BreachedTotal = %d, want 2: each overdue watch breaches exactly once", got)
	}
	if w, _ := l.SLAWatch("po-sla-1"); !w.Breached {
		t.Fatalf("first watch Breached = false, want the sticky breach to hold")
	}
}

func TestSetSlaSweepIntervalRevisionLeavesWatchDeadlineUntouched(t *testing.T) {
	l, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The sweeper interval decides only how often the watch table
	// is evaluated; a watch's Deadline still decides when it
	// breaches. Revise before the first tick fires.
	submitFednowPayout(t, l, "po-sla-3")
	before, ok := l.SLAWatch("po-sla-3")
	if !ok {
		t.Fatal("no SLA watch opened on submitted payout")
	}
	if _, err := sw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	after, _ := l.SLAWatch("po-sla-3")
	if !after.Deadline.Equal(before.Deadline) {
		t.Fatalf("Deadline moved %v -> %v, want unchanged (only the evaluation cadence moves)", before.Deadline, after.Deadline)
	}
	if after.Breached {
		t.Fatalf("watch Breached = true before any tick, want still unbreached")
	}
}

func TestSetSlaSweepIntervalConcurrentRevisionAndSweepRaceFree(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 10*time.Millisecond)
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
	waitForSLATicks(t, sw, 1, 3*time.Second)
	if n := sw.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the sweeper still ticking")
	}
}

func TestSetSlaSweepIntervalCtxActorAttribution(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetIntervalCtx(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedSLASweepEvents(t, sw, al)
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

func TestSetSlaSweepIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSLASweeper(t, 500*time.Millisecond)
	defer cancel()

	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	events := retunedSLASweepEvents(t, sw, al)
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

func TestSetSlaSweepIntervalNoRestartContinuousTicks(t *testing.T) {
	l, sw, al, cancel := startRetuneTestSLASweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the sweeper tick on the original cadence first, breaching
	// one overdue watch along the way.
	submitFednowPayout(t, l, "po-sla-4")
	waitForWatchBreached(t, l, "po-sla-4", 3*time.Second)
	ticksBefore := sw.Ticks()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first breach, want the sweeper already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the breach count is untouched.
	if _, err := sw.SetInterval(50 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForSLATicks(t, sw, ticksBefore+2, 3*time.Second)
	if got := sw.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := sw.BreachedTotal(); got != 1 {
		t.Fatalf("BreachedTotal = %d after the revision, want still 1", got)
	}
	if got := sw.Interval(); got != 50*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 50ms", got)
	}
}
