package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// FX-rate-warning-threshold runtime revision tests (LG-323).
// They reuse mustAuditLog from ledger_audit_test.go,
// mustSetRateTTL / setRateExpiry from ledger_fx_watch_test.go, and
// waitFor from ledger_hold_sweep_test.go.

// retunedFXWatchEvents stops the watch, closes the audit log (which
// drains the async writer), and returns every
// fx_rate_warning_threshold_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedFXWatchEvents(t *testing.T, w *FXRateWatch, al *AuditLog) []AuditEvent {
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
		if ev.Op == "fx_rate_warning_threshold_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startRetuneTestFXWatch starts an FX rate watch on a ledger with an
// audit log attached. The interval is long by default so only direct
// w.tick() calls drive scans unless the test waits on real ticks.
func startRetuneTestFXWatch(t *testing.T, interval, threshold time.Duration) (*Ledger, *FXRateWatch, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))

	ctx, cancel := context.WithCancel(context.Background())
	w := StartFXRateWatch(ctx, l, interval, threshold, nil)
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil for positive interval")
	}
	return l, w, al, cancel
}

// waitForFXTicks waits until the watch has completed at least n
// ticks, or fails the test on timeout.
func waitForFXTicks(t *testing.T, w *FXRateWatch, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "fx watch ticks", func() bool { return w.Ticks() >= n })
}

func TestSetFXWarningThresholdNegativeRejectedAndKeepsLive(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()

	for _, d := range []time.Duration{-time.Hour, -time.Nanosecond} {
		prev, err := w.SetWarningThreshold(d)
		if !errors.Is(err, ErrInvalidFXRateWarningThreshold) {
			t.Fatalf("SetWarningThreshold(%v): err = %v, want ErrInvalidFXRateWarningThreshold", d, err)
		}
		if prev != 24*time.Hour {
			t.Fatalf("SetWarningThreshold(%v) returned %v, want the live 24h", d, prev)
		}
		if got := w.WarningThreshold(); got != 24*time.Hour {
			t.Fatalf("WarningThreshold() = %v after rejected %v, want still 24h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := w.SetWarningThresholdCtx(context.Background(), -time.Second); !errors.Is(err, ErrInvalidFXRateWarningThreshold) || prev != 24*time.Hour {
		t.Fatalf("SetWarningThresholdCtx(-1s) = (%v, %v), want (24h, ErrInvalidFXRateWarningThreshold)", prev, err)
	}
	// No revision landed: no audit events at all.
	if events := retunedFXWatchEvents(t, w, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetFXWarningThresholdZeroSilencesStaleButNotExpiry(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	now := time.Now()
	// Expires in 2h: stale inside the 24h window, silent at 0.
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	// Already expired: expiry warnings never depend on the threshold.
	mustSetRateTTL(t, l, "EUR", "USD", time.Hour)
	setRateExpiry(t, l, "EUR", "USD", now.Add(-time.Minute))

	prev, err := w.SetWarningThreshold(0)
	if err != nil {
		t.Fatalf("SetWarningThreshold(0): %v", err)
	}
	if prev != 24*time.Hour {
		t.Fatalf("returned previous threshold = %v, want 24h", prev)
	}
	if got := w.WarningThreshold(); got != 0 {
		t.Fatalf("WarningThreshold() = %v, want 0", got)
	}

	w.tick()
	if got := w.StaleTotal(); got != 0 {
		t.Fatalf("stale=%d after tick at threshold 0, want 0 (stale silenced)", got)
	}
	if got := w.ExpiredTotal(); got != 1 {
		t.Fatalf("expired=%d after tick at threshold 0, want 1 (expiry still reported)", got)
	}
	// A second tick stays silent on stale: zero is a steady state,
	// not a one-tick suppression.
	w.tick()
	if got := w.StaleTotal(); got != 0 {
		t.Fatalf("stale=%d after second tick at threshold 0, want 0", got)
	}
	if got := w.ExpiredTotal(); got != 1 {
		t.Fatalf("expired=%d after second tick, want still 1 (edge-triggered)", got)
	}
}

func TestSetFXWarningThresholdSameValueNoOpNoAudit(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()

	prev, err := w.SetWarningThreshold(24 * time.Hour)
	if err != nil {
		t.Fatalf("same-value SetWarningThreshold: %v", err)
	}
	if prev != 24*time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 24h", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := w.SetWarningThreshold(time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold(1h): %v", err)
	}
	if prev, err := w.SetWarningThreshold(time.Hour); err != nil || prev != time.Hour {
		t.Fatalf("same-value SetWarningThreshold(1h) = (%v, %v), want (1h, nil)", prev, err)
	}
	if events := retunedFXWatchEvents(t, w, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetFXWarningThresholdWideningReportsNewlyInWindow(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// Expires in 5h: outside the 1h window, so no warning yet.
	mustSetRateTTL(t, l, "USD", "JPY", 5*time.Hour)
	w.tick()
	if got := w.StaleTotal(); got != 0 {
		t.Fatalf("stale=%d inside the 1h window, want 0", got)
	}

	// Widen to 24h: the rate newly enters the window and reports
	// stale once on the next tick — seen marks are not reset, but
	// this identity was never warned, so it fires.
	if _, err := w.SetWarningThreshold(24 * time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold(24h): %v", err)
	}
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after widening, want 1", got)
	}
	if got := l.FXRateStaleAlertCount(); got != 1 {
		t.Fatalf("ledger stale counter = %d, want 1", got)
	}
	// Still edge-triggered: a further tick does not re-fire.
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after a further tick, want still 1", got)
	}
}

func TestSetFXWarningThresholdNarrowingKeepsReportedStale(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after the first tick, want 1", got)
	}

	// Narrow to 1h: the rate falls out of the window, but the
	// warning already fired is neither re-reported nor revoked —
	// the stale mark for this identity stays, and the ledger
	// counter keeps the one alert.
	if _, err := w.SetWarningThreshold(time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold(1h): %v", err)
	}
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after narrowing, want still 1 (no re-report, no revoke)", got)
	}
	if got := l.FXRateStaleAlertCount(); got != 1 {
		t.Fatalf("ledger stale counter = %d after narrowing, want still 1", got)
	}
	w.tick()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after a further tick, want still 1", got)
	}
}

func TestSetFXWarningThresholdInFlightTickNotInterrupted(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, 10*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	// Let a real tick fire the stale warning on the original
	// threshold first.
	waitFor(t, 3*time.Second, "stale warning", func() bool { return w.StaleTotal() >= 1 })
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d, want 1 after the first real tick", got)
	}

	// Revise mid-stream while ticks keep firing — both direct
	// w.tick() calls and the run loop's own ticker — and the
	// revision must not interrupt, drop, or double-count scans.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			w.tick()
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := w.SetWarningThreshold(time.Duration(1+i%5) * time.Hour); err != nil {
			t.Errorf("SetWarningThreshold mid-tick: %v", err)
		}
	}
	wg.Wait()
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after concurrent ticks and revisions, want still 1 (each identity warns once)", got)
	}
	if got := w.ExpiredTotal(); got != 0 {
		t.Fatalf("expired=%d, want 0", got)
	}
	if got := w.Ticks(); got < 20 {
		t.Fatalf("ticks=%d after the concurrent ticks, want at least 20", got)
	}
	// The watch is still alive on a live threshold: a further
	// real tick lands without a restart.
	waitForFXTicks(t, w, w.Ticks()+1, 3*time.Second)
}

func TestSetFXWarningThresholdConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, 10*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the watch
	// ticks underneath: -race must stay silent, and the watch must
	// keep ticking on some non-negative threshold.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				// Includes 0 (silence stale) and positive windows.
				d := time.Duration(g*10+i%5) * time.Hour
				_, _ = w.SetWarningThreshold(d)
				_ = w.WarningThreshold()
			}
		}(g)
	}
	wg.Wait()
	if got := w.WarningThreshold(); got < 0 {
		t.Fatalf("WarningThreshold() = %v after concurrent revisions, want a non-negative live value", got)
	}
	waitForFXTicks(t, w, 1, 3*time.Second)
	if n := w.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the watch still ticking")
	}
}

func TestSetFXWarningThresholdCtxActorAttribution(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := w.SetWarningThresholdCtx(ctx, time.Hour); err != nil {
		t.Fatalf("SetWarningThresholdCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := w.SetWarningThreshold(2 * time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold: %v", err)
	}
	events := retunedFXWatchEvents(t, w, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_warning_threshold_ms"]; got != float64(72*time.Hour/time.Millisecond) {
		t.Fatalf("first previous_warning_threshold_ms = %v, want %v", got, float64(72*time.Hour/time.Millisecond))
	}
	if got := events[0].Details["current_warning_threshold_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Fatalf("first current_warning_threshold_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if events[1].Actor != "SetWarningThreshold" {
		t.Fatalf("second event Actor = %q, want SetWarningThreshold", events[1].Actor)
	}
	if got := events[1].Details["previous_warning_threshold_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Fatalf("second previous_warning_threshold_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := events[1].Details["current_warning_threshold_ms"]; got != float64(2*time.Hour/time.Millisecond) {
		t.Fatalf("second current_warning_threshold_ms = %v, want %v", got, float64(2*time.Hour/time.Millisecond))
	}
}

func TestSetFXWarningThresholdAuditPreviousCurrentExact(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
	defer cancel()

	prev, err := w.SetWarningThreshold(24 * time.Hour)
	if err != nil {
		t.Fatalf("SetWarningThreshold: %v", err)
	}
	if prev != 72*time.Hour {
		t.Fatalf("returned previous threshold = %v, want 72h", prev)
	}
	events := retunedFXWatchEvents(t, w, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetWarningThreshold" {
		t.Errorf("Actor = %q, want SetWarningThreshold", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_warning_threshold_ms"]; got != float64(72*time.Hour/time.Millisecond) {
		t.Errorf("previous_warning_threshold_ms = %v, want %v", got, float64(72*time.Hour/time.Millisecond))
	}
	if got := ev.Details["current_warning_threshold_ms"]; got != float64(24*time.Hour/time.Millisecond) {
		t.Errorf("current_warning_threshold_ms = %v, want %v", got, float64(24*time.Hour/time.Millisecond))
	}
	// Only the two numeric ms fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current warning threshold ms", ev.Details)
	}
}

func TestSetFXWarningThresholdNoRestartContinuousTicks(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, 20*time.Millisecond, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	// Let the watch tick on the original threshold first, warning
	// once about the rate expiring in 2h.
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	waitFor(t, 3*time.Second, "stale warning", func() bool { return w.StaleTotal() >= 1 })
	ticksBefore := w.Ticks()
	if ticksBefore == 0 {
		t.Fatal("ticks = 0 after the first warning, want the watch already ticking")
	}

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the alert counters are untouched.
	if _, err := w.SetWarningThreshold(time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold: %v", err)
	}
	waitForFXTicks(t, w, ticksBefore+2, 3*time.Second)
	if got := w.Ticks(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if got := w.StaleTotal(); got != 1 {
		t.Fatalf("stale=%d after the revision, want still 1", got)
	}
	if got := w.WarningThreshold(); got != time.Hour {
		t.Fatalf("WarningThreshold() = %v after the revision, want 1h", got)
	}
}

func TestSetFXWarningThresholdGetterFollowsLiveValue(t *testing.T) {
	_, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 72*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	if got := w.WarningThreshold(); got != 72*time.Hour {
		t.Fatalf("WarningThreshold() = %v at start, want 72h", got)
	}
	if prev, err := w.SetWarningThreshold(7 * 24 * time.Hour); err != nil || prev != 72*time.Hour {
		t.Fatalf("SetWarningThreshold(7d) = (%v, %v), want (72h, nil)", prev, err)
	}
	if got := w.WarningThreshold(); got != 7*24*time.Hour {
		t.Fatalf("WarningThreshold() = %v after widening, want 7d", got)
	}
	// Zero is a live value too, not a sentinel: the getter reports
	// it, and a rejected negative revision leaves it in place.
	if _, err := w.SetWarningThreshold(0); err != nil {
		t.Fatalf("SetWarningThreshold(0): %v", err)
	}
	if got := w.WarningThreshold(); got != 0 {
		t.Fatalf("WarningThreshold() = %v after silencing, want 0", got)
	}
	if _, err := w.SetWarningThreshold(-time.Minute); !errors.Is(err, ErrInvalidFXRateWarningThreshold) {
		t.Fatalf("SetWarningThreshold(-1m): err = %v, want ErrInvalidFXRateWarningThreshold", err)
	}
	if got := w.WarningThreshold(); got != 0 {
		t.Fatalf("WarningThreshold() = %v after rejected revision, want still 0", got)
	}
}

// TestSetFXWarningThresholdDoesNotTouchRates verifies the revision
// moves only the warning window: the rates themselves (Num/Den,
// ExpiresAt, EffectiveVersion) and the expiry gate are untouched —
// a rate that expired stays expired and still blocks conversion.
func TestSetFXWarningThresholdDoesNotTouchRates(t *testing.T) {
	l, w, al, cancel := startRetuneTestFXWatch(t, time.Hour, 24*time.Hour)
	defer cancel()
	defer w.Stop()
	defer func() { _ = al.Close() }()

	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	before, ok := l.FXRate("USD", "JPY")
	if !ok {
		t.Fatal("no rate for USD->JPY after install")
	}
	if _, err := w.SetWarningThreshold(7 * 24 * time.Hour); err != nil {
		t.Fatalf("SetWarningThreshold(7d): %v", err)
	}
	after, ok := l.FXRate("USD", "JPY")
	if !ok {
		t.Fatal("rate for USD->JPY vanished after the revision")
	}
	if after.Num != before.Num || after.Den != before.Den ||
		!after.ExpiresAt.Equal(before.ExpiresAt) ||
		after.EffectiveVersion != before.EffectiveVersion {
		t.Fatalf("rate moved %+v -> %+v, want unchanged (only the warning window moves)", before, after)
	}
}
