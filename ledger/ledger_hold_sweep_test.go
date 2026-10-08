package ledger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it is true or the deadline passes. Sweeper
// ticks are wall-clock driven, so tests synchronize on state rather than
// sleeping a fixed amount.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartHoldSweeperDisabled(t *testing.T) {
	l := New()
	if sw := StartHoldSweeper(context.Background(), l, 0, nil); sw != nil {
		t.Error("interval 0: want nil sweeper (disabled)")
	}
	if sw := StartHoldSweeper(context.Background(), l, -time.Second, nil); sw != nil {
		t.Error("negative interval: want nil sweeper (disabled)")
	}
}

func TestHoldSweeperExpiresLapsedHolds(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	before := l.version
	// A hold that already lapsed, plus one still active: the sweeper must
	// expire only the lapsed one.
	if _, _, err := l.Hold(Hold{ID: "old", Account: "card", AmountCents: 3000, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold old: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "fresh", Account: "card", AmountCents: 2000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold fresh: %v", err)
	}

	var callbacks atomic.Uint64
	var lastExpired atomic.Int64
	sw := StartHoldSweeper(context.Background(), l, 10*time.Millisecond, func(expired int) {
		callbacks.Add(1)
		lastExpired.Store(int64(expired))
	})
	if sw == nil {
		t.Fatal("positive interval: want a running sweeper")
	}
	defer sw.Stop()

	waitFor(t, 5*time.Second, "sweeper to expire the lapsed hold", func() bool {
		h, ok := l.GetHold("old")
		return ok && h.Status == HoldStatusExpired
	})

	if h, _ := l.GetHold("fresh"); h.Status != HoldStatusActive {
		t.Errorf("fresh hold status = %q, want active (sweeper must not touch it)", h.Status)
	}
	if got := sw.ExpiredTotal(); got != 1 {
		t.Errorf("ExpiredTotal = %d, want 1", got)
	}
	if callbacks.Load() == 0 {
		t.Error("onSweep was never called")
	}
	if lastExpired.Load() < 0 {
		t.Errorf("last onSweep expired = %d, want >= 0", lastExpired.Load())
	}
	// The sweep is off-journal: no version bump.
	if l.version != before {
		t.Error("sweeper bumped the ledger version: expiry sweeps are off-journal")
	}
	// Available funds reflect the expiry: only the fresh hold reserves.
	if got := l.Available("card"); got != 8000 {
		t.Errorf("Available = %d, want 8000 (lapsed hold released its reservation)", got)
	}
}

func TestHoldSweeperNilCallbackIsSafe(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "old", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	sw := StartHoldSweeper(context.Background(), l, 10*time.Millisecond, nil)
	defer sw.Stop()
	waitFor(t, 5*time.Second, "tick with nil callback", func() bool { return sw.Ticks() >= 1 })
	if h, _ := l.GetHold("old"); h.Status != HoldStatusExpired {
		t.Errorf("hold status = %q, want expired", h.Status)
	}
}

func TestHoldSweeperContextCancelStops(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	sw := StartHoldSweeper(ctx, l, 10*time.Millisecond, nil)
	waitFor(t, 5*time.Second, "first tick", func() bool { return sw.Ticks() >= 1 })
	cancel()
	// Give a cancelled sweeper a full interval to prove it does not tick
	// again, then stop it (Stop must not hang after cancellation).
	ticksAtCancel := sw.Ticks()
	time.Sleep(50 * time.Millisecond)
	if got := sw.Ticks(); got != ticksAtCancel {
		t.Errorf("ticks grew after cancel: %d -> %d", ticksAtCancel, got)
	}
	sw.Stop()
}

func TestHoldSweeperStopIsIdempotent(t *testing.T) {
	l := New()
	sw := StartHoldSweeper(context.Background(), l, time.Hour, nil)
	sw.Stop()
	sw.Stop() // must not panic or hang
}

func TestHoldSweeperTicksWithoutExpiries(t *testing.T) {
	l := New()
	sw := StartHoldSweeper(context.Background(), l, 10*time.Millisecond, nil)
	defer sw.Stop()
	waitFor(t, 5*time.Second, "at least two ticks", func() bool { return sw.Ticks() >= 2 })
	if got := sw.ExpiredTotal(); got != 0 {
		t.Errorf("ExpiredTotal = %d, want 0 (nothing lapsed)", got)
	}
}
