package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// FX triangular-consistency watch interval runtime revision tests
// (LG-326).
// They reuse mustAuditLog from ledger_audit_test.go, mustSetRate
// from ledger_fx_test.go, startRetuneTestFXConsistencyWatch /
// setInconsistentTriangle from ledger_fx_consistency_threshold_test.go,
// and waitFor from ledger_hold_sweep_test.go.

// retunedFXConsistencyWatchIntervalEvents stops the watch, closes the
// audit log (which drains the async writer), and returns every
// fx_consistency_watch_interval_retuned event recorded in it. The
// ledger must not be used after this call.
func retunedFXConsistencyWatchIntervalEvents(t *testing.T, w *FXConsistencyWatch, al *AuditLog) []AuditEvent {
	t.Helper()
	w.Stop()
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
		if ev.Op == "fx_consistency_watch_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// waitForFXConsistencyTicks waits until the watch has completed at
// least n ticks, or fails the test on timeout.
func waitForFXConsistencyTicks(t *testing.T, w *FXConsistencyWatch, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "fx consistency watch ticks", func() bool { return w.Ticks() >= n })
}

func TestSetFXConsistencyWatchIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := w.SetInterval(d)
		if !errors.Is(err, ErrInvalidFXConsistencyWatchInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidFXConsistencyWatchInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := w.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := w.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidFXConsistencyWatchInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidFXConsistencyWatchInterval)", prev, err)
	}
	// Zero is rejected here, unlike LG-325's threshold: zero would
	// stop the whole worker, and a runtime revision never silently
	// disables it.
	if events := retunedFXConsistencyWatchIntervalEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetFXConsistencyWatchIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	prev, err := w.SetInterval(time.Hour)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 1h", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := w.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if prev, err := w.SetInterval(30 * time.Minute); err != nil || prev != 30*time.Minute {
		t.Fatalf("same-value SetInterval(30m) = (%v, %v), want (30m, nil)", prev, err)
	}
	if events := retunedFXConsistencyWatchIntervalEvents(t, w, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetFXConsistencyWatchIntervalReturnsPreviousValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 500*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	prev, err := w.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := w.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 20ms", got)
	}
}

func TestSetFXConsistencyWatchIntervalNextTickUsesNewInterval(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 500*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForFXConsistencyTicks(t, w, 1, 3*time.Second)
	if got := w.Ticks(); got != 1 {
		t.Fatalf("ticks after first wait = %d, want exactly 1 on the 500ms cadence", got)
	}

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := w.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := w.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitForFXConsistencyTicks(t, w, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetFXConsistencyWatchIntervalInFlightTickNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()
	setInconsistentTriangle(t, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := StartFXConsistencyWatch(ctx, l, 10*time.Millisecond, 50, nil)
	if w == nil {
		t.Fatal("StartFXConsistencyWatch returned nil for positive interval and threshold")
	}
	defer w.Stop()

	// Wait until the watch is mid-flight: it has alerted once on
	// the live ticker.
	waitFor(t, 3*time.Second, "consistency alert", func() bool { return w.Total() >= 1 })

	// Revise from a racing goroutine while the ticker keeps
	// firing: the in-flight scan must not be cut short, the
	// revision must land, and the watch must keep ticking on the
	// same goroutine (no restart).
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := w.SetInterval(50 * time.Millisecond); err != nil {
			t.Errorf("SetInterval(50ms) mid-flight: %v", err)
		}
	}()
	<-done
	if got := w.Interval(); got != 50*time.Millisecond {
		t.Fatalf("Interval() = %v after mid-flight revision, want 50ms", got)
	}
	ticksAfter := w.Ticks()
	waitFor(t, 3*time.Second, "consistency ticks after revision", func() bool { return w.Ticks() >= ticksAfter+2 })
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after mid-flight revision, want still 1 (already-alerted cycle not re-reported)", got)
	}
}

func TestSetFXConsistencyWatchIntervalDoesNotSwallowFiredTick(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 10*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// Let at least two ticks land on the 10ms cadence.
	waitForFXConsistencyTicks(t, w, 1, 3*time.Second)
	time.Sleep(15 * time.Millisecond)

	// Loosen to 1h: the retune branch stops the old ticker and
	// runs any tick that fired while the revision was in flight
	// before rebuilding, so no due scan is swallowed.
	if _, err := w.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval(1h): %v", err)
	}
	// Let the run loop process the retune (and any drained tick)
	// before taking the baseline: the old ticker is still live
	// until the retune branch stops it.
	time.Sleep(100 * time.Millisecond)
	ticksNow := w.Ticks()
	if ticksNow < 2 {
		t.Fatalf("ticks = %d after loosening, want at least 2 (ticks fired before the revision must not be swallowed)", ticksNow)
	}
	// The next tick follows the loosened 1h cadence: nothing
	// within 200ms of the drained tick.
	time.Sleep(200 * time.Millisecond)
	if n := w.Ticks(); n != ticksNow {
		t.Fatalf("ticks = %d within 200ms of the loosened revision, want still %d on the 1h rhythm", n, ticksNow)
	}
}

func TestSetFXConsistencyWatchIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 10*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()
	setInconsistentTriangle(t, l)

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
				_, _ = w.SetInterval(d)
				_ = w.Interval()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			w.tick()
		}
	}()
	wg.Wait()
	if got := w.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	waitForFXConsistencyTicks(t, w, 1, 3*time.Second)
	if n := w.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the watch still ticking")
	}
}

func TestSetFXConsistencyWatchIntervalCtxCancelled(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// A cancelled context is fail-closed: the revision does not
	// land, the live interval is untouched, and nothing is
	// audited.
	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()
	prev, err := w.SetIntervalCtx(ctx, 30*time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SetIntervalCtx(cancelled) err = %v, want context.Canceled", err)
	}
	if prev != time.Hour {
		t.Fatalf("SetIntervalCtx(cancelled) returned %v, want the live 1h", prev)
	}
	if got := w.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v after cancelled revision, want still 1h", got)
	}
	// Cancellation wins over validation too: a cancelled ctx with
	// an invalid interval reports the cancellation, not the
	// interval error.
	if _, err := w.SetIntervalCtx(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetIntervalCtx(cancelled, 0) err = %v, want context.Canceled", err)
	}
	if events := retunedFXConsistencyWatchIntervalEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only cancelled revisions", len(events))
	}
}

func TestSetFXConsistencyWatchIntervalCtxActorAttribution(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := w.SetIntervalCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := w.SetInterval(2 * time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedFXConsistencyWatchIntervalEvents(t, w, al)
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

func TestSetFXConsistencyWatchIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	prev, err := w.SetInterval(30 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("returned previous interval = %v, want 1h", prev)
	}
	events := retunedFXConsistencyWatchIntervalEvents(t, w, al)
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

func TestSetFXConsistencyWatchIntervalNoRestartContinuousTicks(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 20*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()
	setInconsistentTriangle(t, l)

	// Let the watch tick on the original interval first, alerting
	// once about the 100 bps cycle.
	waitFor(t, 3*time.Second, "consistency alert", func() bool { return w.Total() >= 1 })
	ticksBefore := w.Ticks()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first alert, want the watch already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the alert counter is untouched.
	if _, err := w.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForFXConsistencyTicks(t, w, ticksBefore+2, 3*time.Second)
	if got := w.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after the revision, want still 1", got)
	}
	if got := w.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 10ms", got)
	}
}

func TestSetFXConsistencyWatchIntervalGetterFollowsLiveValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	if got := w.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v at start, want 1h", got)
	}
	if prev, err := w.SetInterval(30 * time.Minute); err != nil || prev != time.Hour {
		t.Fatalf("SetInterval(30m) = (%v, %v), want (1h, nil)", prev, err)
	}
	if got := w.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after tightening, want 30m", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := w.SetInterval(0); !errors.Is(err, ErrInvalidFXConsistencyWatchInterval) {
		t.Fatalf("SetInterval(0): err = %v, want ErrInvalidFXConsistencyWatchInterval", err)
	}
	if got := w.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after rejected revision, want still 30m", got)
	}
}

// TestSetFXConsistencyWatchIntervalDoesNotTouchThresholdOrRates
// verifies the revision moves only the scan cadence: the alert
// threshold, the rates themselves (Num/Den, ExpiresAt,
// EffectiveVersion) and the expiry gate are untouched — a cycle
// that deviates 100 bps still alerts once on the next tick, and
// the dedup marks are not reset.
func TestSetFXConsistencyWatchIntervalDoesNotTouchThresholdOrRates(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	before, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("no rate for USD->EUR after install")
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d before the revision, want 1", got)
	}
	if _, err := w.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if got := w.ThresholdBps(); got != 50 {
		t.Fatalf("ThresholdBps() = %d after the interval revision, want still 50", got)
	}
	after, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("rate for USD->EUR vanished after the revision")
	}
	if after.Num != before.Num || after.Den != before.Den ||
		!after.ExpiresAt.Equal(before.ExpiresAt) ||
		after.EffectiveVersion != before.EffectiveVersion {
		t.Fatalf("rate moved %+v -> %+v, want unchanged (only the scan cadence moves)", before, after)
	}
	// The next ticks still alert exactly once in total: the dedup
	// marks were not reset by the interval revision.
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after the revision, want 1", got)
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after a further tick, want still 1 (edge-triggered)", got)
	}
}
