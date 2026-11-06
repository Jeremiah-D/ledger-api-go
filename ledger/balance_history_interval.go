package ledger

import (
	"context"
	"errors"
	"time"
)

// Balance-history sampling interval runtime revision (LG-330).
//
// StartBalanceHistoryRecorder fixes the recorder's sampling cadence —
// how often every (account, currency) balance is snapshotted into the
// per-day JSONL time series — for the life of the worker:
// balanceHistoryConfig() (main.go) reads
// LEDGER_BALANCE_HISTORY_INTERVAL once at startup, and run() built
// its single ticker from that value with no revision entry point
// (SetBalanceHistoryInterval was a whole-repo zero hit). The only way
// to tighten the sampling from 5m to 10s (a reconciliation anomaly or
// suspicious fund movement just landed and the operator wants a denser
// time series for post-hoc forensics) or loosen it back to save the
// full-snapshot and disk-write load while the account count is large
// was to change the env var and restart the process. This module is
// the orthogonal runtime path: an operator revises the live
// recorder's sampling interval in place, instead of restarting the
// server.
//
// Semantics, deliberately fail-closed throughout, along LG-324's
// FXRateWatch.SetInterval (the same retune shape, a different worker),
// LG-329's SnapshotBackup.SetInterval (the same worker family, the
// previous round's item), and the LG-316 ReportSweeper pattern:
//
//   - The revision is in place: the recorder keeps its goroutine, its
//     directory, its retention window, and its Ticks / PointsTotal
//     counters — none of them resets. Only the sampling cadence
//     moves. No restart, no Stop + Start.
//   - The revision is future-only: the run loop selects between
//     ticks, so a sample already in progress runs to completion on
//     the old cadence (a triggered sample is never cut short, its
//     rows still land, the counters stay continuous), and the new
//     interval governs from the next tick. The loop rebuilds its
//     ticker inside run(): the old ticker is stopped before the new
//     one is built, so two tickers never run in parallel, and a tick
//     that fired while the revision was in flight is run before the
//     rebuild, so a revision cannot swallow a due sample.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidBalanceHistoryInterval
//     and the live interval stays exactly as it was. Zero is
//     rejected too — unlike the startup-time "non-positive disables
//     the worker" rule: zero here would stop the whole worker, and
//     a runtime revision never silently disables it (along LG-314 /
//     LG-316 / LG-324 / LG-329).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     balance_history_interval_retuned audit event (previous /
//     current sampling interval as millisecond integers — purely
//     numeric, no balance or account material). The Ctx variant
//     attributes the operator (LG-73). Lock order: the setter
//     releases r.mu, then takes l.mu.RLock to emit — r.mu and l.mu
//     are never held nested, so the lock order is acyclic. run()
//     reads the interval under r.mu.RLock when it rebuilds the
//     ticker, so the two paths never nest either. In particular the
//     setter never calls tick() while holding r.mu: tick() takes
//     l.mu.RLock only for the SnapshotBalances capture instant and
//     does all disk I/O unlocked, so a setter that called tick()
//     under r.mu would couple the two locks.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-329 family): sampling tuning is
//     in-process structural config. main.go stores the started
//     recorder handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the sampling interval: the interval
//     comes only from the startup-time
//     LEDGER_BALANCE_HISTORY_INTERVAL parse, so a restart restores
//     the env value while the revision events themselves stay in the
//     audit trail for traceability. Persisting a revision across
//     restarts would need the env var to be re-read, a later round.
//   - The interval is NOT the retention window and NOT the query
//     interface: it only decides how often a sample is taken.
//     MaxAgeMs still decides how long files survive pruning, the
//     per-day balance-history-YYYY-MM-DD.jsonl naming and the
//     append-only write still decide how rows land, and GET
//     /balance-history still reads the directory the same way —
//     none of them participates in the revision. The Ticks /
//     PointsTotal counters and
//     ledger_balance_history_snapshots_total keep accumulating
//     across a revision.
//   - A recorder that was never started (dir unset or interval
//     invalid, so balanceHistoryConfig() returned false and
//     StartBalanceHistoryRecorder was never called) has no handle
//     to revise: the revision applies only to a running recorder.
//
// Orthogonality: LG-59 owns the balance-history time-series
// mechanism itself (this module is its missing runtime revision entry
// point for the sampling cadence), LG-327 owns the health-watch scan
// interval and LG-328 owns the TSA anchor interval (the same family,
// different workers — the LG-327 and LG-328 drafts both explicitly
// deferred this item to a separate item, and this module is that
// fulfillment), LG-329 owns the snapshot-backup interval (the same
// family, a different worker — disaster-recovery cadence versus
// observability sampling cadence), LG-61 owns the per-account
// balance-threshold watch (a different object: alert bands, not the
// full-series sampling), LG-316..LG-322 own the seven sweeper poll
// intervals (the same revision pattern, different workers),
// LG-323/LG-324/LG-325/LG-326 own the FX watch knobs (the same
// family, different workers), and LG-314 owns the audit-sink idle
// poll interval (the same (previous, error) return and audit shape,
// a different component).

var (
	// ErrInvalidBalanceHistoryInterval is returned by
	// BalanceHistoryRecorder.SetInterval when the requested interval
	// is not positive. The live interval is left untouched.
	ErrInvalidBalanceHistoryInterval = errors.New("ledger: balance history: invalid sampling interval: sampling interval must be positive")
)

// SetInterval revises the recorder's sampling interval in place and
// reports the pre-revision interval (for the audit trail). The
// revision is future-only: a sample already in progress runs to
// completion, and only the next tick uses the new interval; a tick
// that fired while the revision was in flight is run before the
// ticker is rebuilt, so it is never swallowed. A revision identical
// to the live interval is a no-op that returns the live value with a
// nil error and no side effect. A non-positive interval returns
// ErrInvalidBalanceHistoryInterval with the live interval untouched
// (the returned value is the live interval in that case too).
func (r *BalanceHistoryRecorder) SetInterval(d time.Duration) (time.Duration, error) {
	return r.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (r *BalanceHistoryRecorder) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	r.mu.Lock()
	previous := r.interval
	if d <= 0 {
		r.mu.Unlock()
		return previous, ErrInvalidBalanceHistoryInterval
	}
	if d == previous {
		r.mu.Unlock()
		return previous, nil
	}
	r.interval = d
	r.mu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under mu, so a
	// newer revision supersedes this one even if its wakeup was
	// coalesced into this one; the send is non-blocking so a
	// revision never stalls on a loop that is mid-sample.
	select {
	case r.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-329 retunes: r.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two numeric millisecond intervals: no balance, account, or
	// other ledger material ever rides in a retune event.
	r.l.mu.RLock()
	r.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "balance_history_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: r.l.version,
		VersionAfter:  r.l.version,
		Details: map[string]any{
			"previous_history_interval_ms": previous.Milliseconds(),
			"current_history_interval_ms":  d.Milliseconds(),
		},
	})
	r.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the recorder's live sampling interval.
func (r *BalanceHistoryRecorder) Interval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.interval
}
