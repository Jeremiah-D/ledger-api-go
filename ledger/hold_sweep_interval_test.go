package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Hold-sweeper interval runtime revision tests (LG-318). They reuse
// the ledger_hold_sweep_test.go harness (fundAccount, waitFor) and
// mustAuditLog from ledger_audit_test.go.

// retunedHoldSweepEvents stops the sweeper, closes the audit log
// (which drains the async writer), and returns every
// hold_sweep_interval_retuned event recorded in it. The ledger must
// not be used after this call.
func retunedHoldSweepEvents(t *testing.T, sw *HoldSweeper, al *AuditLog) []AuditEvent {
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
		if ev.Op == "hold_sweep_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startRetuneTestHoldSweeper starts a hold sweeper on a ledger with
// one lapsed hold and an audit log attached.
func startRetuneTestHoldSweeper(t *testing.T, interval time.Duration) (*Ledger, *HoldSweeper, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "old", Account: "card", AmountCents: 3000, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold old: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sw := StartHoldSweeper(ctx, l, interval, nil)
	if sw == nil {
		cancel()
		t.Fatalf("sweeper not started")
	}
	return l, sw, al, cancel
}

// waitForHoldTicks waits until the hold sweeper has completed at
// least n ticks.
func waitForHoldTicks(t *testing.T, sw *HoldSweeper, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "ticks", func() bool { return sw.Ticks() >= n })
}

func TestSetHoldSweepIntervalInvalidRejectedLiveUnchanged(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 500*time.Millisecond)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Second, -time.Millisecond} {
		if prev, err := sw.SetInterval(d); !errors.Is(err, ErrInvalidHoldSweepInterval) {
			t.Errorf("SetInterval(%v): err = %v, want ErrInvalidHoldSweepInterval", d, err)
		} else if prev != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): returned %v, want the live 500ms", d, prev)
		}
		if got := sw.Interval(); got != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): live interval moved to %v, want unchanged 500ms", d, got)
		}
	}
	if events := retunedHoldSweepEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("rejected revisions wrote %d audit events, want 0", len(events))
	}
}

func TestSetHoldSweepIntervalReturnsPreviousValue(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartHoldSweeper(ctx, l, 500*time.Millisecond, nil)
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

func TestSetHoldSweepIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 500*time.Millisecond)
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
	if events := retunedHoldSweepEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-op", len(events))
	}
}

func TestSetHoldSweepIntervalNextTickUsesNewInterval(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForHoldTicks(t, sw, 1, 3*time.Second)
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
	waitForHoldTicks(t, sw, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetHoldSweepIntervalInFlightSweepNotInterrupted(t *testing.T) {
	l, sw, al, cancel := startRetuneTestHoldSweeper(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let a sweep run on the original cadence: the lapsed hold is
	// expired by the in-flight tick.
	waitForHoldTicks(t, sw, 1, 3*time.Second)
	if h, _ := l.GetHold("old"); h.Status != HoldStatusExpired {
		t.Fatalf("hold status = %q after the first tick, want expired", h.Status)
	}
	if got := sw.ExpiredTotal(); got != 1 {
		t.Fatalf("ExpiredTotal = %d, want 1 after the first sweep", got)
	}

	// Revise mid-stream while the sweeper keeps ticking: the
	// revision must not interrupt, drop, or double-count sweeps.
	if _, err := sw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval mid-sweep: %v", err)
	}
	// A second lapsed hold placed after the revision must still be
	// expired by a later tick — the sweeper keeps working on the
	// new cadence.
	if _, _, err := l.Hold(Hold{ID: "old2", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold old2: %v", err)
	}
	waitFor(t, 3*time.Second, "second lapsed hold to expire after the revision", func() bool {
		h, ok := l.GetHold("old2")
		return ok && h.Status == HoldStatusExpired && sw.ExpiredTotal() == 2
	})
	if got := sw.ExpiredTotal(); got != 2 {
		t.Fatalf("ExpiredTotal = %d, want 2: each lapsed hold expires exactly once", got)
	}
}

func TestSetHoldSweepIntervalConcurrentRevisionAndSweepRaceFree(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 10*time.Millisecond)
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
	waitForHoldTicks(t, sw, 1, 3*time.Second)
	if n := sw.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the sweeper still ticking")
	}
}

func TestSetHoldSweepIntervalCtxActorAttribution(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 500*time.Millisecond)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetIntervalCtx(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedHoldSweepEvents(t, sw, al)
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

func TestSetHoldSweepIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 500*time.Millisecond)
	defer cancel()

	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	events := retunedHoldSweepEvents(t, sw, al)
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

func TestSetHoldSweepIntervalNoRestartContinuousTicks(t *testing.T) {
	_, sw, al, cancel := startRetuneTestHoldSweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the sweeper tick on the original cadence first.
	waitForHoldTicks(t, sw, 2, 3*time.Second)
	ticksBefore := sw.Ticks()
	expiredBefore := sw.ExpiredTotal()
	if expiredBefore != 1 {
		t.Fatalf("ExpiredTotal = %d after %d ticks, want 1: the lapsed hold expires once", expiredBefore, ticksBefore)
	}

	// Revise the cadence: the same goroutine keeps running — the
	// counters never reset and the ticks keep accumulating on the
	// new interval instead of restarting from zero.
	if _, err := sw.SetInterval(30 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForHoldTicks(t, sw, ticksBefore+3, 3*time.Second)
	if got := sw.Ticks(); got < ticksBefore+3 {
		t.Fatalf("Ticks() = %d, want >= %d: ticks must continue, not restart", got, ticksBefore+3)
	}
	if got := sw.ExpiredTotal(); got != expiredBefore {
		t.Fatalf("ExpiredTotal() = %d, want %d: no new lapsed holds, and the counter must not reset", got, expiredBefore)
	}
}

func TestSetHoldSweepIntervalCountersKeepAccumulating(t *testing.T) {
	l, sw, al, cancel := startRetuneTestHoldSweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	waitForHoldTicks(t, sw, 2, 3*time.Second)
	ticksBefore, expiredBefore := sw.Ticks(), sw.ExpiredTotal()
	if _, err := sw.SetInterval(25 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	// A revision is not a reset: the counters observed before it
	// are still there, and they grow from there.
	if got := sw.Ticks(); got < ticksBefore {
		t.Fatalf("Ticks() = %d after revision, want >= %d (pre-revision count)", got, ticksBefore)
	}
	if got := sw.ExpiredTotal(); got < expiredBefore {
		t.Fatalf("ExpiredTotal() = %d after revision, want >= %d (pre-revision count)", got, expiredBefore)
	}
	// A lapsed hold placed after the revision is still expired by
	// a later tick on the new cadence.
	if _, _, err := l.Hold(Hold{ID: "old2", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold old2: %v", err)
	}
	waitFor(t, 3*time.Second, "ExpiredTotal to grow past the revision", func() bool {
		return sw.ExpiredTotal() > expiredBefore
	})
	if got := sw.ExpiredTotal(); got != expiredBefore+1 {
		t.Fatalf("ExpiredTotal() = %d, want %d after the post-revision sweep", got, expiredBefore+1)
	}
}
