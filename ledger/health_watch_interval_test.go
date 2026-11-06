package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Balance-health-watch interval runtime revision tests (LG-327).
// They reuse mustAuditLog from ledger_audit_test.go, postSimple /
// TestStartHealthWatchTicksAndFinds helpers from
// ledger_health_watch_test.go, and waitFor from
// ledger_hold_sweep_test.go.

// startRetuneTestHealthWatch starts a health watch on a ledger with
// an audit log attached. The interval is long by default so only
// direct hw.tick() calls drive scans unless the test waits on real
// ticks.
func startRetuneTestHealthWatch(t *testing.T, interval time.Duration) (*Ledger, *HealthWatch, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))

	ctx, cancel := context.WithCancel(context.Background())
	hw := StartHealthWatch(ctx, l, interval, nil)
	if hw == nil {
		t.Fatal("StartHealthWatch returned nil for positive interval")
	}
	return l, hw, al, cancel
}

// waitForHealthTicks waits until the watch has completed at least n
// ticks, or fails the test on timeout.
func waitForHealthTicks(t *testing.T, hw *HealthWatch, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "health watch ticks", func() bool { return hw.Ticks() >= n })
}

// retunedHealthWatchIntervalEvents stops the watch, closes the audit
// log (which drains the async writer), and returns every
// health_watch_interval_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedHealthWatchIntervalEvents(t *testing.T, hw *HealthWatch, al *AuditLog) []AuditEvent {
	t.Helper()
	hw.Stop()
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
		if ev.Op == "health_watch_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetHealthWatchIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := hw.SetInterval(d)
		if !errors.Is(err, ErrInvalidHealthWatchInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidHealthWatchInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := hw.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := hw.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidHealthWatchInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidHealthWatchInterval)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time rule where a
	// non-positive interval disables the watchdog: zero here would
	// stop the whole worker, and a runtime revision never silently
	// disables it.
	if events := retunedHealthWatchIntervalEvents(t, hw, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetHealthWatchIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()

	prev, err := hw.SetInterval(time.Hour)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 1h", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := hw.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if prev, err := hw.SetInterval(30 * time.Minute); err != nil || prev != 30*time.Minute {
		t.Fatalf("same-value SetInterval(30m) = (%v, %v), want (30m, nil)", prev, err)
	}
	if events := retunedHealthWatchIntervalEvents(t, hw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetHealthWatchIntervalReturnsPreviousValue(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, 500*time.Millisecond)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	prev, err := hw.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := hw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 20ms", got)
	}
}

func TestSetHealthWatchIntervalNextTickUsesNewInterval(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, 500*time.Millisecond)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForHealthTicks(t, hw, 1, 3*time.Second)
	if got := hw.Ticks(); got != 1 {
		t.Fatalf("ticks after first wait = %d, want exactly 1 on the 500ms cadence", got)
	}

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := hw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := hw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitForHealthTicks(t, hw, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetHealthWatchIntervalInFlightScanNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()
	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.EnableOverdraftProtection("equity")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hw := StartHealthWatch(ctx, l, 10*time.Millisecond, nil)
	if hw == nil {
		t.Fatal("StartHealthWatch returned nil for positive interval")
	}
	defer hw.Stop()

	// Hold the ledger lock so the first tick blocks inside
	// RunHealthCheck (which takes l.mu) — a scan is observably in
	// flight while the ticker keeps firing underneath.
	l.mu.Lock()
	// Let the ticker fire while the scan is blocked: the tick
	// channel (buffered 1) now holds a due scan the loop has not
	// consumed yet.
	time.Sleep(50 * time.Millisecond)

	// Revise mid-scan from another goroutine: the setter's audit
	// emission needs l.mu.RLock, so it waits with the scan until
	// the test releases the lock — the revision itself must not be
	// cut short, and neither must the in-flight scan.
	type result struct {
		prev time.Duration
		err  error
	}
	done := make(chan result, 1)
	go func() {
		prev, err := hw.SetInterval(500 * time.Millisecond)
		done <- result{prev, err}
	}()
	l.mu.Unlock()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("SetInterval mid-scan: %v", r.err)
		}
		if r.prev != 10*time.Millisecond {
			t.Fatalf("SetInterval mid-scan returned %v, want 10ms", r.prev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SetInterval mid-scan never returned")
	}

	// The in-flight scan completes (tick 1), and the tick that fired
	// during the revision runs too (tick 2) — either straight off
	// the ticker channel or via the retune branch's drain. Either
	// way both land well inside the new 500ms cadence.
	waitForHealthTicks(t, hw, 2, 3*time.Second)
	if got := hw.Ticks(); got != 2 {
		t.Fatalf("ticks = %d after the revision, want exactly 2 (in-flight scan + drained tick)", got)
	}
	if got := hw.ViolationsTotal(); got == 0 {
		t.Fatal("ViolationsTotal = 0 after the in-flight scan, want the negative-balance violation recorded")
	}
	// The next tick follows the loosened 500ms cadence: nothing
	// within 200ms of the drained tick, then a tick arrives.
	time.Sleep(200 * time.Millisecond)
	if n := hw.Ticks(); n != 2 {
		t.Fatalf("ticks = %d within 200ms of the drained tick, want still 2 on the loosened 500ms rhythm", n)
	}
	waitForHealthTicks(t, hw, 3, 3*time.Second)
}

func TestSetHealthWatchIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, 10*time.Millisecond)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the watch
	// ticks underneath, plus direct tick() calls from another
	// goroutine: -race must stay silent, and the watch must keep
	// ticking on some positive interval.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = hw.SetInterval(d)
				_ = hw.Interval()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			hw.tick()
		}
	}()
	wg.Wait()
	if got := hw.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	waitForHealthTicks(t, hw, 1, 3*time.Second)
	if n := hw.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the watch still ticking")
	}
}

func TestSetHealthWatchIntervalCtxActorAttribution(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := hw.SetIntervalCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := hw.SetInterval(2 * time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedHealthWatchIntervalEvents(t, hw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_watch_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Fatalf("first previous_watch_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := events[0].Details["current_watch_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("first current_watch_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if events[1].Actor != "SetInterval" {
		t.Fatalf("second event Actor = %q, want SetInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_watch_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("second previous_watch_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if got := events[1].Details["current_watch_interval_ms"]; got != float64(2*time.Hour/time.Millisecond) {
		t.Fatalf("second current_watch_interval_ms = %v, want %v", got, float64(2*time.Hour/time.Millisecond))
	}
}

func TestSetHealthWatchIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()

	prev, err := hw.SetInterval(30 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("returned previous interval = %v, want 1h", prev)
	}
	events := retunedHealthWatchIntervalEvents(t, hw, al)
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
	if got := ev.Details["previous_watch_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Errorf("previous_watch_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := ev.Details["current_watch_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Errorf("current_watch_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	// Only the two numeric ms fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current watch interval ms", ev.Details)
	}
}

func TestSetHealthWatchIntervalNoRestartContinuousTicks(t *testing.T) {
	l, hw, al, cancel := startRetuneTestHealthWatch(t, 20*time.Millisecond)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	// A standing violation, so every scan records one.
	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.EnableOverdraftProtection("equity")
	waitFor(t, 3*time.Second, "violation", func() bool { return hw.ViolationsTotal() >= 1 })
	ticksBefore := hw.Ticks()
	violationsBefore := hw.ViolationsTotal()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first violation, want the watch already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow, violations keep accumulating — no dedup,
	// every scan re-reports the standing violation) and the
	// counters are never reset.
	if _, err := hw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForHealthTicks(t, hw, ticksBefore+2, 3*time.Second)
	if got := hw.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := hw.ViolationsTotal(); got < violationsBefore+2 {
		t.Fatalf("violations = %d after the revision, want at least %d (two more scans of the standing violation)", got, violationsBefore+2)
	}
	if got := hw.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 10ms", got)
	}
}

func TestSetHealthWatchIntervalGetterFollowsLiveValue(t *testing.T) {
	_, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	if got := hw.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v at start, want 1h", got)
	}
	if prev, err := hw.SetInterval(30 * time.Minute); err != nil || prev != time.Hour {
		t.Fatalf("SetInterval(30m) = (%v, %v), want (1h, nil)", prev, err)
	}
	if got := hw.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after tightening, want 30m", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := hw.SetInterval(0); !errors.Is(err, ErrInvalidHealthWatchInterval) {
		t.Fatalf("SetInterval(0): err = %v, want ErrInvalidHealthWatchInterval", err)
	}
	if got := hw.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after rejected revision, want still 30m", got)
	}
}

// TestSetHealthWatchIntervalDoesNotTouchInvariants verifies the
// revision moves only the scan cadence: the three invariant checks,
// the rotation cursor, the journal version, and the violation
// counters are untouched — a standing negative balance on a protected
// account is still found on every scan after the revision, with the
// counters continuing from where they were.
func TestSetHealthWatchIntervalDoesNotTouchInvariants(t *testing.T) {
	l, hw, al, cancel := startRetuneTestHealthWatch(t, time.Hour)
	defer cancel()
	defer hw.Stop()
	defer func() { _ = al.Close() }()

	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.EnableOverdraftProtection("equity")

	// One scan before the revision establishes the baseline: the
	// violation is found and the rotation cursor has advanced.
	hw.tick()
	if got := hw.ViolationsTotal(); got != 1 {
		t.Fatalf("violations = %d before the revision, want 1", got)
	}
	if got := l.HealthViolationCount(); got != 1 {
		t.Fatalf("ledger violations = %d before the revision, want 1", got)
	}
	cursorBefore := l.healthCursor.Load()
	versionBefore := l.version

	if _, err := hw.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	// The revision itself moves nothing but the cadence.
	if got := l.healthCursor.Load(); got != cursorBefore {
		t.Fatalf("rotation cursor moved %d -> %d, want unchanged (revision must not reset the window)", cursorBefore, got)
	}
	if got := l.version; got != versionBefore {
		t.Fatalf("version moved %d -> %d, want unchanged", versionBefore, got)
	}
	if got := hw.ViolationsTotal(); got != 1 {
		t.Fatalf("violations = %d right after the revision, want still 1", got)
	}

	// Scans after the revision keep finding the same violation,
	// and the counters continue from where they were.
	hw.tick()
	hw.tick()
	if got := hw.ViolationsTotal(); got != 3 {
		t.Fatalf("violations = %d after two more scans, want 3", got)
	}
	if got := l.HealthViolationCount(); got != 3 {
		t.Fatalf("ledger violations = %d after two more scans, want 3", got)
	}
}
