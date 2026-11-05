package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// FX-rate-watch interval runtime revision tests (LG-324).
// They reuse mustAuditLog from ledger_audit_test.go,
// mustSetRateTTL / setRateExpiry from ledger_fx_watch_test.go,
// startRetuneTestFXWatch / waitForFXTicks from
// ledger_fx_watch_threshold_test.go, and waitFor from
// ledger_hold_sweep_test.go.

// retunedFXWatchIntervalEvents stops the watch, closes the audit log
// (which drains the async writer), and returns every
// fx_rate_watch_interval_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedFXWatchIntervalEvents(t *testing.T, w *FXRateWatch, al *AuditLog) []AuditEvent {
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
		if ev.Op == "fx_rate_watch_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetFXWatchIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := w.SetInterval(d)
		if !errors.Is(err, ErrInvalidFXRateWatchInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidFXRateWatchInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := w.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := w.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidFXRateWatchInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidFXRateWatchInterval)", prev, err)
	}
	// Zero is rejected here, unlike LG-323's threshold: zero would
	// stop the whole worker, and a runtime revision never silently
	// disables it.
	if events := retunedFXWatchIntervalEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetFXWatchIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
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
	if events := retunedFXWatchIntervalEvents(t, w, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetFXWatchIntervalReturnsPreviousValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, 500*time.Millisecond, 24*time.Hour)
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

func TestSetFXWatchIntervalNextTickUsesNewInterval(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, 500*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForFXTicks(t, w, 1, 3*time.Second)
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
	waitForFXTicks(t, w, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

// blockingFXProvider holds the watch inside FetchRates until
// released, so a scan is observably in flight while the ticker
// keeps firing underneath.
type blockingFXProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingFXProvider) FetchRates(ctx context.Context) ([]MarketRate, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return nil, nil
}

func TestSetFXWatchIntervalInFlightTickNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	provider := &blockingFXProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	release := provider.release
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	l := New(WithAuditLog(al), WithFXRateProvider(provider))
	defer func() { _ = al.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := StartFXRateWatch(ctx, l, 10*time.Millisecond, 24*time.Hour, nil)
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil for positive interval")
	}
	defer w.Stop()

	// Wait until the first scan is inside the blocking provider.
	select {
	case <-provider.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first scan never entered the blocking provider")
	}
	// Let the ticker fire while the scan is in flight: the tick
	// channel (buffered 1) now holds a due scan the loop has not
	// consumed yet.
	time.Sleep(50 * time.Millisecond)

	// Revise mid-scan: the in-flight scan must not be cut short or
	// dropped, and the tick that fired in flight must not be
	// swallowed — the new interval governs only from the next tick.
	if _, err := w.SetInterval(500 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval mid-scan: %v", err)
	}
	close(release)

	// The in-flight scan completes (tick 1), and the tick that fired
	// during the revision runs too (tick 2) — either straight off
	// the ticker channel or via the retune branch's drain. Either
	// way both land well inside the new 500ms cadence.
	waitForFXTicks(t, w, 2, 3*time.Second)
	if got := w.Ticks(); got != 2 {
		t.Fatalf("ticks = %d after the revision, want exactly 2 (in-flight scan + drained tick)", got)
	}
	// The next tick follows the loosened 500ms cadence: nothing
	// within 200ms of the drained tick, then a tick arrives.
	time.Sleep(200 * time.Millisecond)
	if n := w.Ticks(); n != 2 {
		t.Fatalf("ticks = %d within 200ms of the drained tick, want still 2 on the loosened 500ms rhythm", n)
	}
	waitForFXTicks(t, w, 3, 3*time.Second)
}

func TestSetFXWatchIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, 10*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
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
	waitForFXTicks(t, w, 1, 3*time.Second)
	if n := w.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the watch still ticking")
	}
}

func TestSetFXWatchIntervalCtxActorAttribution(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := w.SetIntervalCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := w.SetInterval(2 * time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedFXWatchIntervalEvents(t, w, al)
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

func TestSetFXWatchIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
	defer cancel()

	prev, err := w.SetInterval(30 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("returned previous interval = %v, want 1h", prev)
	}
	events := retunedFXWatchIntervalEvents(t, w, al)
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

func TestSetFXWatchIntervalNoRestartContinuousTicks(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, 20*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// Let the watch tick on the original interval first, warning
	// once about the rate expiring in 2h.
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	waitFor(t, 3*time.Second, "stale warning", func() bool { return w.StaleTotal() >= 1 })
	ticksBefore := w.Ticks()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first warning, want the watch already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the alert counters are untouched.
	if _, err := w.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForFXTicks(t, w, ticksBefore+2, 3*time.Second)
	if got := w.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after the revision, want still 1", got)
	}
	if got := w.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 10ms", got)
	}
}

func TestSetFXWatchIntervalGetterFollowsLiveValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
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
	if _, err := w.SetInterval(0); !errors.Is(err, ErrInvalidFXRateWatchInterval) {
		t.Fatalf("SetInterval(0): err = %v, want ErrInvalidFXRateWatchInterval", err)
	}
	if got := w.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after rejected revision, want still 30m", got)
	}
}

// TestSetFXWatchIntervalDoesNotTouchThresholdOrRates verifies the
// revision moves only the scan cadence: the warning threshold, the
// rates themselves (Num/Den, ExpiresAt, EffectiveVersion) and the
// expiry gate are untouched — a rate that expires in 2h still warns
// stale once on the next tick, and the dedup marks are not reset.
func TestSetFXWatchIntervalDoesNotTouchThresholdOrRates(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	before, ok := l.FXRate("USD", "JPY")
	if !ok {
		t.Fatal("no rate for USD->JPY after install")
	}
	if _, err := w.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if got := w.WarningThreshold(); got != 24*time.Hour {
		t.Fatalf("WarningThreshold() = %v after the interval revision, want still 24h", got)
	}
	after, ok := l.FXRate("USD", "JPY")
	if !ok {
		t.Fatal("rate for USD->JPY vanished after the revision")
	}
	if after.Num != before.Num || after.Den != before.Den ||
		!after.ExpiresAt.Equal(before.ExpiresAt) ||
		after.EffectiveVersion != before.EffectiveVersion {
		t.Fatalf("rate moved %+v -> %+v, want unchanged (only the scan cadence moves)", before, after)
	}
	// The next tick still warns stale exactly once: the dedup marks
	// were not reset by the interval revision.
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after the revision, want 1", got)
	}
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after a further tick, want still 1 (edge-triggered)", got)
	}
}
