package ledger

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Background hold-expiry sweeper.
//
// Expiry is lazy by predicate for reads (an expired hold already counts
// as inactive for available-balance purposes), but the explicit
// ExpireHolds sweep exists so operators — and the reconcile report — can
// observe which holds lapsed. Polling POST /holds/expire on a timer is
// the manual version of that; HoldSweeper is the in-process version: a
// single goroutine that calls ExpireHolds on a ticker until its context
// is cancelled or Stop is called.
//
// The sweeper never touches active, captured, or released holds, and the
// sweep does not bump the ledger version (see ExpireHolds).
type HoldSweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(expired int)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks        atomic.Uint64
	expiredTotal atomic.Uint64
}

// StartHoldSweeper launches a background goroutine that calls
// l.ExpireHolds() every interval until ctx is cancelled or Stop is
// called. After each tick it invokes onSweep (when non-nil) with the
// number of holds that tick expired, so the caller can feed its own
// metrics — the HTTP server wires this to ledger_hold_sweeps_total, the
// same counter POST /holds/expire increments.
//
// A non-positive interval disables the sweeper: it returns nil and
// starts nothing, so callers can pass a configured interval straight
// through without branching. The first tick fires after one full
// interval, not immediately, so a freshly started process settles before
// it sweeps.
func StartHoldSweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(expired int)) *HoldSweeper {
	if interval <= 0 {
		return nil
	}
	sw := &HoldSweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *HoldSweeper) run(ctx context.Context) {
	defer sw.wg.Done()
	ticker := time.NewTicker(sw.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-ticker.C:
			expired := sw.l.ExpireHolds()
			sw.ticks.Add(1)
			sw.expiredTotal.Add(uint64(expired))
			if sw.onSweep != nil {
				sw.onSweep(expired)
			}
		}
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation — whichever
// fires first wins, the other is a no-op.
func (sw *HoldSweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many sweeps have completed. ExpiredTotal reports the
// total number of holds expired across all ticks.
func (sw *HoldSweeper) Ticks() uint64        { return sw.ticks.Load() }
func (sw *HoldSweeper) ExpiredTotal() uint64 { return sw.expiredTotal.Load() }
