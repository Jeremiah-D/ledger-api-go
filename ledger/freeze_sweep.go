package ledger

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Background freeze-expiry sweeper.
//
// A timed freeze (see FreezeUntil) already releases lazily on the write
// path: every write operation checks for expired deadlines before its
// frozen gate. But a freeze on an account that sees no writes would sit
// frozen past its deadline until someone posts through it. The explicit
// ExpireFrozenAccounts sweep exists so operators — and the reconcile
// report — can observe releases without write traffic. FreezeSweeper is
// the in-process version of polling that sweep: a single goroutine that
// calls ExpireFrozenAccounts on a ticker until its context is cancelled
// or Stop is called.
//
// The sweeper never touches manually frozen accounts, and the sweep does
// not bump the ledger version (see releaseExpiredFreezesLocked): like
// Freeze and Unfreeze, an automatic release moves no money.
type FreezeSweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(released int)

	// mu guards interval. The run loop re-reads the live value when
	// a revision wakes it (LG-319), so a newer revision always
	// supersedes an older one that has not been applied yet.
	mu sync.RWMutex
	// retune wakes the run loop so it rebuilds its ticker on the
	// live interval. Buffered size 1 and sent non-blocking: the
	// channel is only a wakeup — the loop re-reads interval under
	// mu — so a coalesced wakeup still applies the newest value.
	retune chan struct{}

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks         atomic.Uint64
	releasedTotal atomic.Uint64
}

// StartFreezeSweeper launches a background goroutine that calls
// l.ExpireFrozenAccounts() every interval until ctx is cancelled or Stop
// is called. After each tick it invokes onSweep (when non-nil) with the
// number of freezes that tick released, so the caller can feed its own
// observability; the ledger's own FrozenAutoReleases counter is bumped
// inside the sweep regardless.
//
// A non-positive interval disables the sweeper: it returns nil and
// starts nothing, so callers can pass a configured interval straight
// through without branching. The first tick fires after one full
// interval, not immediately, so a freshly started process settles before
// it sweeps.
func StartFreezeSweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(released int)) *FreezeSweeper {
	if interval <= 0 {
		return nil
	}
	sw := &FreezeSweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		retune:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *FreezeSweeper) run(ctx context.Context) {
	defer sw.wg.Done()
	ticker := time.NewTicker(sw.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-sw.retune:
			// A tick may have fired while the revision was in
			// flight: run it now so the revision cannot swallow a
			// due sweep, then rebuild the ticker on the new live
			// interval. The old ticker is stopped before the new
			// one is built, so two tickers never run in parallel;
			// a sweep already in progress is unaffected — the
			// loop only selects between sweeps, so the revision
			// waits until it finishes.
			ticker.Stop()
			select {
			case <-ticker.C:
				sw.sweepOnce()
			default:
			}
			ticker = time.NewTicker(sw.Interval())
		case <-ticker.C:
			sw.sweepOnce()
		}
	}
}

// sweepOnce runs one sweep: every expired timed freeze is released,
// the counters advance, and the onSweep callback fires. Extracted from
// run so the retune path can run a tick that fired while a revision
// was in flight.
func (sw *FreezeSweeper) sweepOnce() {
	released := sw.l.ExpireFrozenAccounts()
	sw.ticks.Add(1)
	sw.releasedTotal.Add(uint64(released))
	if sw.onSweep != nil {
		sw.onSweep(released)
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation — whichever
// fires first wins, the other is a no-op.
func (sw *FreezeSweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many sweeps have completed. ReleasedTotal reports the
// total number of timed freezes released across all ticks.
func (sw *FreezeSweeper) Ticks() uint64         { return sw.ticks.Load() }
func (sw *FreezeSweeper) ReleasedTotal() uint64 { return sw.releasedTotal.Load() }
