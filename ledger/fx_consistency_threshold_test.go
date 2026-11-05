package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// FX triangular-consistency threshold runtime revision tests (LG-325).
// They reuse mustAuditLog from ledger_audit_test.go, mustSetRate
// from ledger_fx_test.go, and waitFor from ledger_hold_sweep_test.go.

// retunedFXConsistencyThresholdEvents stops the watch, closes the
// audit log (which drains the async writer), and returns every
// fx_consistency_threshold_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedFXConsistencyThresholdEvents(t *testing.T, w *FXConsistencyWatch, al *AuditLog) []AuditEvent {
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
		if ev.Op == "fx_consistency_threshold_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startRetuneTestFXConsistencyWatch starts an FX consistency watch
// on a ledger with an audit log attached. The interval is long by
// default so only direct w.tick() calls drive scans unless the test
// waits on real ticks.
func startRetuneTestFXConsistencyWatch(t *testing.T, interval time.Duration, thresholdBps int64) (*Ledger, *FXConsistencyWatch, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))

	ctx, cancel := context.WithCancel(context.Background())
	w := StartFXConsistencyWatch(ctx, l, interval, thresholdBps, nil)
	if w == nil {
		t.Fatal("StartFXConsistencyWatch returned nil for positive interval and threshold")
	}
	return l, w, al, cancel
}

// setInconsistentTriangle installs the 100 bps-deviation triangle
// from ledger_fx_consistency_test.go: USD→EUR 1/1, EUR→JPY 1/1,
// JPY→USD 99/100, so the EUR→JPY→USD cycle's exact product is
// 99/100 and |product − 1| is exactly 100 bps.
func setInconsistentTriangle(t *testing.T, l *Ledger) {
	t.Helper()
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)
}

func TestSetFXConsistencyThresholdNegativeRejectedAndKeepsLive(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	for _, bps := range []int64{-1, -100, -1 << 62} {
		prev, err := w.SetThresholdBps(bps)
		if !errors.Is(err, ErrInvalidFXConsistencyThresholdBps) {
			t.Fatalf("SetThresholdBps(%d): err = %v, want ErrInvalidFXConsistencyThresholdBps", bps, err)
		}
		if prev != 50 {
			t.Fatalf("SetThresholdBps(%d) returned %d, want the live 50", bps, prev)
		}
		if got := w.ThresholdBps(); got != 50 {
			t.Fatalf("ThresholdBps() = %d after rejected %d, want still 50", got, bps)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := w.SetThresholdBpsCtx(context.Background(), -1); !errors.Is(err, ErrInvalidFXConsistencyThresholdBps) || prev != 50 {
		t.Fatalf("SetThresholdBpsCtx(-1) = (%d, %v), want (50, ErrInvalidFXConsistencyThresholdBps)", prev, err)
	}
	// No revision landed: no audit events at all.
	if events := retunedFXConsistencyThresholdEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetFXConsistencyThresholdZeroDisablesCheckButWatchKeepsTicking(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d before zeroing, want 1", got)
	}

	prev, err := w.SetThresholdBps(0)
	if err != nil {
		t.Fatalf("SetThresholdBps(0): %v", err)
	}
	if prev != 50 {
		t.Fatalf("returned previous threshold = %d, want 50", prev)
	}
	if got := w.ThresholdBps(); got != 0 {
		t.Fatalf("ThresholdBps() = %d, want 0", got)
	}

	// Zero disables the check — ScanFXRateInconsistency returns
	// nil — but the watch itself keeps ticking: Ticks grows and
	// no new alert fires.
	ticksBefore := w.Ticks()
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after tick at threshold 0, want still 1 (check disabled)", got)
	}
	if got := w.Ticks(); got != ticksBefore+1 {
		t.Fatalf("ticks = %d after tick at threshold 0, want %d (watch keeps ticking)", got, ticksBefore+1)
	}
	// The zero tick pruned the seen mark (nothing is inside the
	// gate when the check is off).
	if len(w.seen) != 0 {
		t.Fatalf("seen = %d entries after tick at threshold 0, want 0 (pruned)", len(w.seen))
	}
}

func TestSetFXConsistencyThresholdSameValueNoop(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	if prev, err := w.SetThresholdBps(50); err != nil || prev != 50 {
		t.Fatalf("SetThresholdBps(50) = (%d, %v), want (50, nil)", prev, err)
	}
	if prev, err := w.SetThresholdBpsCtx(context.Background(), 50); err != nil || prev != 50 {
		t.Fatalf("SetThresholdBpsCtx(50) = (%d, %v), want (50, nil)", prev, err)
	}
	// A same-value revision writes no audit event.
	if events := retunedFXConsistencyThresholdEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only same-value revisions", len(events))
	}
}

func TestSetFXConsistencyThresholdTightenReportsNewCycleOnce(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 150)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	// At 150 bps the 100 bps deviation is inside the gate: silent.
	w.tick()
	if got := w.Total(); got != 0 {
		t.Fatalf("total = %d at threshold 150, want 0", got)
	}

	// Tighten to 50: the cycle newly exceeds the gate and reports
	// once on the next tick — and only once.
	if prev, err := w.SetThresholdBps(50); err != nil || prev != 150 {
		t.Fatalf("SetThresholdBps(50) = (%d, %v), want (150, nil)", prev, err)
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after tighten + tick, want 1", got)
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after a second tick, want still 1 (edge-triggered)", got)
	}
}

func TestSetFXConsistencyThresholdTightenDoesNotReReportAlertedCycle(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d before tightening, want 1", got)
	}

	// Tighten further — the cycle is still over the gate, but it
	// already alerted for the same leg identity: no re-report.
	if _, err := w.SetThresholdBps(25); err != nil {
		t.Fatalf("SetThresholdBps(25): %v", err)
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after tightening an already-alerted cycle, want still 1", got)
	}
}

func TestSetFXConsistencyThresholdLoosenPrunesSeen(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d before loosening, want 1", got)
	}
	if len(w.seen) != 1 {
		t.Fatalf("seen = %d entries before loosening, want 1", len(w.seen))
	}

	// Loosen past the 100 bps deviation: the cycle falls back
	// inside the gate and the next tick prunes its seen mark.
	if _, err := w.SetThresholdBps(150); err != nil {
		t.Fatalf("SetThresholdBps(150): %v", err)
	}
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after loosening, want still 1 (no new alert)", got)
	}
	if len(w.seen) != 0 {
		t.Fatalf("seen = %d entries after loosening tick, want 0 (pruned)", len(w.seen))
	}
}

func TestSetFXConsistencyThresholdLoosenThenTightenReArms(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d at start, want 1", got)
	}

	// Loosen: the cycle drops out of the gate and its mark is pruned.
	if _, err := w.SetThresholdBps(150); err != nil {
		t.Fatalf("SetThresholdBps(150): %v", err)
	}
	w.tick()
	if len(w.seen) != 0 {
		t.Fatalf("seen = %d entries after loosening, want 0", len(w.seen))
	}

	// Re-tighten: the same cycle re-arms and reports again.
	if prev, err := w.SetThresholdBps(50); err != nil || prev != 150 {
		t.Fatalf("SetThresholdBps(50) = (%d, %v), want (150, nil)", prev, err)
	}
	w.tick()
	if got := w.Total(); got != 2 {
		t.Fatalf("total = %d after loosen + re-tighten, want 2 (re-armed)", got)
	}
}

func TestSetFXConsistencyThresholdZeroThenTightenReArms(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	setInconsistentTriangle(t, l)
	w.tick()
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d at start, want 1", got)
	}

	// Zero disables the check and prunes the mark on the next tick.
	if _, err := w.SetThresholdBps(0); err != nil {
		t.Fatalf("SetThresholdBps(0): %v", err)
	}
	w.tick()
	if len(w.seen) != 0 {
		t.Fatalf("seen = %d entries after zero tick, want 0", len(w.seen))
	}

	// Re-tighten from zero: the cycle reports again.
	if _, err := w.SetThresholdBps(50); err != nil {
		t.Fatalf("SetThresholdBps(50): %v", err)
	}
	w.tick()
	if got := w.Total(); got != 2 {
		t.Fatalf("total = %d after zero + re-tighten, want 2 (re-armed)", got)
	}
}

func TestSetFXConsistencyThresholdInFlightTickNotInterrupted(t *testing.T) {
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
		if _, err := w.SetThresholdBps(25); err != nil {
			t.Errorf("SetThresholdBps(25) mid-flight: %v", err)
		}
	}()
	<-done
	if got := w.ThresholdBps(); got != 25 {
		t.Fatalf("ThresholdBps() = %d after mid-flight revision, want 25", got)
	}
	ticksAfter := w.Ticks()
	waitFor(t, 3*time.Second, "consistency ticks after revision", func() bool { return w.Ticks() >= ticksAfter+2 })
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after mid-flight revision, want still 1 (already-alerted cycle not re-reported)", got)
	}
}

func TestSetFXConsistencyThresholdConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 10*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()
	setInconsistentTriangle(t, l)

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the watch
	// ticks underneath, plus direct tick() calls from another
	// goroutine: -race must stay silent, and the watch must keep
	// ticking on some non-negative threshold.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				bps := int64(10 + g*10 + i%5)
				_, _ = w.SetThresholdBps(bps)
				_ = w.ThresholdBps()
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
	if got := w.ThresholdBps(); got < 0 {
		t.Fatalf("ThresholdBps() = %d after concurrent revisions, want a non-negative live value", got)
	}
	waitFor(t, 3*time.Second, "consistency ticks", func() bool { return w.Ticks() >= 1 })
	if n := w.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the watch still ticking")
	}
}

func TestSetFXConsistencyThresholdCtxActorAttribution(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 50)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := w.SetThresholdBpsCtx(ctx, 25); err != nil {
		t.Fatalf("SetThresholdBpsCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := w.SetThresholdBps(100); err != nil {
		t.Fatalf("SetThresholdBps: %v", err)
	}
	events := retunedFXConsistencyThresholdEvents(t, w, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_threshold_bps"]; got != float64(50) {
		t.Fatalf("first previous_threshold_bps = %v, want 50", got)
	}
	if got := events[0].Details["current_threshold_bps"]; got != float64(25) {
		t.Fatalf("first current_threshold_bps = %v, want 25", got)
	}
	if events[1].Actor != "SetThresholdBps" {
		t.Fatalf("second event Actor = %q, want SetThresholdBps", events[1].Actor)
	}
	if got := events[1].Details["previous_threshold_bps"]; got != float64(25) {
		t.Fatalf("second previous_threshold_bps = %v, want 25", got)
	}
	if got := events[1].Details["current_threshold_bps"]; got != float64(100) {
		t.Fatalf("second current_threshold_bps = %v, want 100", got)
	}
}

func TestSetFXConsistencyThresholdAuditPreviousCurrentExact(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 200)
	defer cancel()

	// A chain of real revisions: each event carries the exact
	// previous and current int64 bps values, never durations.
	steps := []struct{ bps, prev int64 }{
		{50, 200},
		{0, 50},
		{150, 0},
	}
	for _, s := range steps {
		if prev, err := w.SetThresholdBps(s.bps); err != nil || prev != s.prev {
			t.Fatalf("SetThresholdBps(%d) = (%d, %v), want (%d, nil)", s.bps, prev, err, s.prev)
		}
	}
	events := retunedFXConsistencyThresholdEvents(t, w, al)
	if len(events) != len(steps) {
		t.Fatalf("retuned events = %d, want %d", len(events), len(steps))
	}
	for i, s := range steps {
		if got := events[i].Details["previous_threshold_bps"]; got != float64(s.prev) {
			t.Fatalf("event %d previous_threshold_bps = %v, want %d", i, got, s.prev)
		}
		if got := events[i].Details["current_threshold_bps"]; got != float64(s.bps) {
			t.Fatalf("event %d current_threshold_bps = %v, want %d", i, got, s.bps)
		}
		if events[i].VersionBefore != events[i].VersionAfter {
			t.Fatalf("event %d version moved %d -> %d, want unchanged (structural config)", i, events[i].VersionBefore, events[i].VersionAfter)
		}
	}
}

func TestSetFXConsistencyThresholdGetterFollowsLiveValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXConsistencyWatch(t, time.Hour, 200)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	if got := w.ThresholdBps(); got != 200 {
		t.Fatalf("ThresholdBps() = %d at start, want 200", got)
	}
	if prev, err := w.SetThresholdBps(50); err != nil || prev != 200 {
		t.Fatalf("SetThresholdBps(50) = (%d, %v), want (200, nil)", prev, err)
	}
	if got := w.ThresholdBps(); got != 50 {
		t.Fatalf("ThresholdBps() = %d after tightening, want 50", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := w.SetThresholdBps(-1); !errors.Is(err, ErrInvalidFXConsistencyThresholdBps) {
		t.Fatalf("SetThresholdBps(-1): err = %v, want ErrInvalidFXConsistencyThresholdBps", err)
	}
	if got := w.ThresholdBps(); got != 50 {
		t.Fatalf("ThresholdBps() = %d after rejected revision, want still 50", got)
	}
	// Zero is a live value too, and the getter follows it.
	if _, err := w.SetThresholdBps(0); err != nil {
		t.Fatalf("SetThresholdBps(0): %v", err)
	}
	if got := w.ThresholdBps(); got != 0 {
		t.Fatalf("ThresholdBps() = %d after zeroing, want 0", got)
	}
}

func TestSetFXConsistencyThresholdNoRestartContinuousTicks(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXConsistencyWatch(t, 20*time.Millisecond, 50)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()
	setInconsistentTriangle(t, l)

	// Let the watch tick on the original threshold first, alerting
	// once about the 100 bps cycle.
	waitFor(t, 3*time.Second, "consistency alert", func() bool { return w.Total() >= 1 })
	ticksBefore := w.Ticks()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first alert, want the watch already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the alert counter is untouched.
	if _, err := w.SetThresholdBps(25); err != nil {
		t.Fatalf("SetThresholdBps: %v", err)
	}
	waitFor(t, 3*time.Second, "consistency ticks after revision", func() bool { return w.Ticks() >= ticksBefore+2 })
	if got := w.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := w.Total(); got != 1 {
		t.Fatalf("total = %d after the revision, want still 1", got)
	}
	if got := w.ThresholdBps(); got != 25 {
		t.Fatalf("ThresholdBps() = %d after the revision, want 25", got)
	}
}
