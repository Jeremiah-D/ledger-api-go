package ledger

import (
	"context"
	"errors"
	"time"
)

// Period-auto-close-sweeper interval runtime revision (LG-320).
//
// StartPeriodAutoCloseSweeper fixes the sweeper's scan cadence — how
// often the auto-close plan table is scanned for due plans — for the
// life of the worker: periodAutoCloseInterval() (main.go) reads
// LEDGER_PERIOD_AUTOCLOSE_INTERVAL once at startup, and run() built
// its single ticker from that value with no revision entry point
// (SetInterval was a whole-repo zero hit). The only way to move the
// scan from 1m to 10s (so due plans execute and their
// period_autoclose_executed audit event and reconcile-report entry
// become visible sooner) or back off from 10s to 1m (less scan and
// audit noise) was to change the env var and restart the process.
// This module is the orthogonal runtime path: an operator revises the
// live sweeper's interval in place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-316's
// SetReportSweeperInterval, LG-317's SetScheduleSweeperInterval,
// LG-318's SetHoldSweeperInterval, and LG-319's
// SetFreezeSweeperInterval (the same shape, a different worker):
//
//   - The revision is in place: the sweeper keeps its goroutine, its
//     registered plans, their CloseAt deadlines, and its Ticks /
//     DecidedTotal counters — none of them resets. Only the scan
//     cadence moves. No restart, no Stop + Start.
//   - The revision is future-only: the run loop selects between
//     sweeps, so a sweep already in progress runs to completion on
//     the old cadence (a triggered SweepPeriodAutoCloses is never cut
//     short), and the new interval governs from the next tick. The
//     loop rebuilds its ticker inside run(): the old ticker is
//     stopped before the new one is built, so two tickers never run
//     in parallel, and a tick that fired while the revision was in
//     flight is run before the rebuild, so a revision cannot swallow
//     a due sweep. Unlike LG-314's sink — whose run loop already
//     re-reads the live poll interval under cfgMu on every idle
//     sleep — the sweeper read its interval only once, when the
//     ticker was built, so this revision needs the mu-guarded live
//     value plus the retune wakeup channel on PeriodAutoCloseSweeper;
//     the setter releases sw.mu, then takes l.mu.RLock to emit —
//     sw.mu and l.mu are never held nested, so the lock order is
//     acyclic.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else
//     ErrInvalidPeriodAutoCloseSweepInterval and the live interval
//     stays exactly as it was. Zero is rejected too — the
//     construction-time "non-positive disables the sweeper" reading
//     is deliberately NOT reused: a runtime revision never silently
//     disables the worker (along LG-290 / LG-304 / LG-306 / LG-314 /
//     LG-316 / LG-317 / LG-318 / LG-319).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     period_autoclose_sweep_interval_retuned audit event
//     (previous/current sweep interval as millisecond integers —
//     purely numeric). The Ctx variant attributes the operator
//     (LG-73). The setter releases sw.mu, then takes l.mu.RLock to
//     emit — sw.mu and l.mu are never held nested, so the lock order
//     is acyclic.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-315/LG-316/LG-317/LG-318/LG-319 family):
//     sweeper tuning is in-process structural config. main.go stores
//     the started sweeper handle on srv so existing operator paths
//     can call it.
//   - Snapshots do not carry the sweeper interval: the interval
//     comes only from the startup-time
//     LEDGER_PERIOD_AUTOCLOSE_INTERVAL parse, so a restart restores
//     the env value while the revision events themselves stay in the
//     audit trail for traceability. Persisting a revision across
//     restarts would need the env var to be re-read, a later round.
//   - The sweeper interval is NOT a plan's CloseAt: it only decides
//     how often the plan table is scanned. Each plan's CloseAt still
//     decides when that plan becomes due — the sweep only decides
//     when the execution and its audit event become visible.
//     Tightening the sweeper makes due plans fire sooner; it never
//     changes any plan's CloseAt, status, or closing flags, and a
//     failed plan stays terminal and is never retried by the
//     sweeper.
//   - A sweeper that was never started (env unset or invalid, so
//     periodAutoCloseInterval() returned false and
//     StartPeriodAutoCloseSweeper was never called) has no handle to
//     revise: the revision applies only to a running sweeper.
//     Stopping a sweeper still goes through Stop / process shutdown,
//     never through a revision.
//
// Orthogonality: LG-151 owns the period auto-close sweeper mechanism
// itself (this module is its missing runtime revision entry point),
// LG-316 owns the report sweeper's interval, LG-317 the
// transfer-schedule sweeper's interval, LG-318 the hold-expiry
// sweeper's interval, LG-319 the freeze sweeper's interval (the same
// revision pattern, different workers), and LG-314 the audit-sink idle
// poll interval (the same revision pattern, a different worker).

var (
	// ErrInvalidPeriodAutoCloseSweepInterval is returned by
	// PeriodAutoCloseSweeper.SetInterval when the requested interval
	// is not positive. The live interval is left untouched.
	ErrInvalidPeriodAutoCloseSweepInterval = errors.New("ledger: period auto-close sweeper: invalid sweep interval: sweep interval must be positive")
)

// SetInterval revises the sweeper's scan interval in place and
// reports the pre-revision interval (for the audit trail). The
// revision is future-only: a sweep already in progress runs to
// completion, and only the next tick uses the new interval. A
// revision identical to the live interval is a no-op that returns
// the live value with a nil error and no side effect. A
// non-positive interval returns
// ErrInvalidPeriodAutoCloseSweepInterval with the live interval
// untouched (the returned value is the live interval in that case
// too).
func (sw *PeriodAutoCloseSweeper) SetInterval(d time.Duration) (time.Duration, error) {
	return sw.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (sw *PeriodAutoCloseSweeper) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	sw.mu.Lock()
	previous := sw.interval
	if d <= 0 {
		sw.mu.Unlock()
		return previous, ErrInvalidPeriodAutoCloseSweepInterval
	}
	if d == previous {
		sw.mu.Unlock()
		return previous, nil
	}
	sw.interval = d
	sw.mu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under mu, so a
	// newer revision supersedes this one even if its wakeup was
	// coalesced into this one; the send is non-blocking so a
	// revision never stalls on a loop that is mid-sweep.
	select {
	case sw.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314/LG-316/
	// LG-317/LG-318/LG-319: sw.mu and l.mu are never held nested, so
	// the lock order is acyclic. emitAuditCtx is a no-op when no audit
	// log is attached, and the event goes only to the audit log —
	// never to the outbox.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "period_autoclose_sweep_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_sweep_interval_ms": previous.Milliseconds(),
			"current_sweep_interval_ms":  d.Milliseconds(),
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the sweeper's live scan interval.
func (sw *PeriodAutoCloseSweeper) Interval() time.Duration {
	sw.mu.RLock()
	defer sw.mu.RUnlock()
	return sw.interval
}
