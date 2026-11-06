package ledger

import (
	"context"
	"errors"
	"time"
)

// Balance-history retention window runtime revision (LG-331).
//
// StartBalanceHistoryRecorder fixes the recorder's retention window —
// how long balance-history-YYYY-MM-DD.jsonl files survive the per-tick
// pruning — for the life of the worker:
// balanceHistoryConfig() (main.go) reads
// LEDGER_BALANCE_HISTORY_MAX_AGE_MS once at startup (missing, invalid,
// or non-positive means the 90-day default), and tick() pruned on that
// value with no revision entry point (SetBalanceHistoryMaxAge was a
// whole-repo zero hit). The only way to lengthen the window from 90
// days to 180 days (a reconciliation anomaly or suspicious fund
// movement just landed and the operator wants the time series kept
// longer for post-hoc forensics, instead of the daily pruning deleting
// it) or shorten it back to 30 days (the disk is hot and old history
// files have to go) was to change the env var and restart the process.
// This module is the orthogonal runtime path: an operator revises the
// live recorder's retention window in place, instead of restarting
// the server.
//
// Semantics, deliberately fail-closed throughout, along LG-323's
// FXRateWatch.SetWarningThreshold (a per-tick threshold knob, read
// live with no ticker rebuild) and LG-330's
// BalanceHistoryRecorder.SetInterval (the same worker, the same lock):
//
//   - The revision is in place: the recorder keeps its goroutine, its
//     directory, its sampling interval, and its Ticks / PointsTotal
//     counters — none of them resets. Only the retention window
//     moves. No restart, no Stop + Start, no ticker rebuild.
//   - The revision is future-only: a sample already in progress runs
//     to completion (its SnapshotBalances + WriteBalanceHistory +
//     PruneBalanceHistory finish on the old window), and the new
//     window governs from the next tick's pruning. The window is a
//     threshold the tick reads live, not a cadence, so the revision
//     needs no retune wakeup — along LG-323, the new value simply
//     takes effect at the next tick.
//   - The revision itself deletes no files. This answers the policy
//     question LG-330 (c) left open ("does shortening delete old
//     files immediately"): shortening only takes effect through the
//     next tick's regular pruning (files past the new window are
//     deleted by that tick, exactly as that window's normal pruning
//     behaves), and there is no out-of-band sweep. Lengthening
//     keeps old files from the next tick on.
//   - Validation is fail-closed with no silent normalization: the
//     new window must be > 0, else ErrInvalidBalanceHistoryMaxAge
//     and the live window stays exactly as it was. Zero is rejected
//     too — this item has no "0 = keep forever" mode, and the
//     startup-time "non-positive normalizes to the 90-day default"
//     rule is never reused for a runtime revision (along LG-314):
//     a zero window let through would make the next tick's pruning
//     delete the entire history. A runtime revision never silently
//     normalizes.
//   - A revision identical to the live window is a no-op: it returns
//     the live value with a nil error and writes no audit event, so
//     a retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     balance_history_max_age_retuned audit event (previous /
//     current retention window as millisecond integers — purely
//     numeric, no balance or account material). The Ctx variant
//     attributes the operator (LG-73). Lock order: the setter
//     releases r.mu, then takes l.mu.RLock to emit — r.mu and l.mu
//     are never held nested, so the lock order is acyclic. tick()
//     reads the window under r.mu.RLock, so the two paths never
//     nest either. In particular the setter never calls tick() or
//     PruneBalanceHistory while holding r.mu: tick() takes l.mu.RLock
//     only for the SnapshotBalances capture instant and does all
//     disk I/O unlocked, so a setter that called it under r.mu
//     would couple the two locks.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-330 family): retention tuning is
//     in-process structural config. main.go stores the started
//     recorder handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the retention window: the window comes
//     only from the startup-time
//     LEDGER_BALANCE_HISTORY_MAX_AGE_MS parse, so a restart restores
//     the env value while the revision events themselves stay in the
//     audit trail for traceability. Persisting a revision across
//     restarts would need the env var to be re-read, a later round.
//   - The window is NOT the sampling interval and NOT the query
//     interface: it only decides how long files survive pruning.
//     Interval still decides how often a sample is taken, the
//     per-day balance-history-YYYY-MM-DD.jsonl naming and the
//     append-only write still decide how rows land, the mtime
//     pruning rule itself is unchanged, and GET /balance-history
//     still reads the directory the same way — none of them
//     participates in the revision. The Ticks / PointsTotal
//     counters and ledger_balance_history_snapshots_total keep
//     accumulating across a revision.
//   - A recorder that was never started (dir unset or interval
//     invalid, so balanceHistoryConfig() returned false and
//     StartBalanceHistoryRecorder was never called) has no handle
//     to revise: the revision applies only to a running recorder.
//
// Orthogonality: LG-59 owns the balance-history time-series
// mechanism itself (this module is its missing runtime revision entry
// point for the retention window), LG-330 owns the same worker's
// sampling interval (when a sample is taken; this module owns how
// long it is kept — the LG-330 draft explicitly excluded the window
// and deferred it to a separate item, and this module is that
// fulfillment), LG-329 owns the snapshot-backup interval and LG-332
// owns the snapshot-backup retention depths (the same revision
// pattern, a different worker — disaster-recovery files versus the
// observability time series), LG-323/325 own the FX thresholds (the
// same threshold-revision mode, different workers), LG-316..LG-322
// own the seven sweeper poll intervals (the same revision pattern,
// different workers), and LG-314 owns the audit-sink idle poll
// interval (the same (previous, error) return and audit shape, a
// different component).

var (
	// ErrInvalidBalanceHistoryMaxAge is returned by
	// BalanceHistoryRecorder.SetMaxAge when the requested retention
	// window is not positive. The live window is left untouched.
	ErrInvalidBalanceHistoryMaxAge = errors.New("ledger: balance history: invalid retention window: retention window must be positive")
)

// SetMaxAge revises the recorder's retention window in place and
// reports the pre-revision window (for the audit trail). The revision
// is future-only: a tick already in progress runs to completion on
// the old window, and only the next tick's pruning uses the new one.
// The revision itself deletes no files — shortening takes effect
// through the next tick's regular pruning. A revision identical to
// the live window is a no-op that returns the live value with a nil
// error and no side effect. A non-positive window returns
// ErrInvalidBalanceHistoryMaxAge with the live window untouched (the
// returned value is the live window in that case too).
func (r *BalanceHistoryRecorder) SetMaxAge(d time.Duration) (time.Duration, error) {
	return r.SetMaxAgeCtx(context.Background(), d)
}

// SetMaxAgeCtx is SetMaxAge with per-call audit actor attribution
// (LG-73).
func (r *BalanceHistoryRecorder) SetMaxAgeCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	r.mu.Lock()
	previous := r.maxAge
	if d <= 0 {
		r.mu.Unlock()
		return previous, ErrInvalidBalanceHistoryMaxAge
	}
	if d == previous {
		r.mu.Unlock()
		return previous, nil
	}
	r.maxAge = d
	r.mu.Unlock()

	// No retune wakeup: the window is a per-tick threshold the tick
	// reads live, not a cadence — along LG-323, the new value takes
	// effect at the next tick's pruning without rebuilding
	// anything. The revision itself deletes no files.

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-330 retunes: r.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two numeric millisecond windows: no balance, account, or
	// other ledger material ever rides in a retune event.
	r.l.mu.RLock()
	r.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "balance_history_max_age_retuned",
		Actor:         "SetMaxAge",
		VersionBefore: r.l.version,
		VersionAfter:  r.l.version,
		Details: map[string]any{
			"previous_max_age_ms": previous.Milliseconds(),
			"current_max_age_ms":  d.Milliseconds(),
		},
	})
	r.l.mu.RUnlock()
	return previous, nil
}

// MaxAge reports the recorder's live retention window.
func (r *BalanceHistoryRecorder) MaxAge() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxAge
}
