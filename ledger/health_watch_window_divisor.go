package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Balance-health-watch spot-check window runtime revision (LG-356).
//
// The accounting-equation spot check re-checks a rotating slice of the
// (account, currency) keys on every scan: ceil(n/divisor) keys per
// scan, so every key is re-checked at least once every divisor scans.
// The divisor was a construction-time constant (4): on a large book
// the per-scan cost of a quarter of the keys grows linearly with the
// book, and on a small book four scans pass before a given key is
// re-checked, stretching violation-discovery latency. The only way to
// move it was to change the code and redeploy. This module is the
// orthogonal runtime path: an operator revises the live divisor in
// place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along the LG-327
// HealthWatch.SetInterval retune shape (a different knob on the same
// worker) and the LG-314/LG-316..LG-326 family:
//
//   - The revision is in place: the watch keeps its goroutine, its
//     rotation cursor, its Ticks / ViolationsTotal counters, and the
//     ledger's own violation counter — none of them resets. Only the
//     per-scan coverage moves. No restart, no Stop + Start.
//   - The revision is future-only: a scan already in progress runs to
//     completion on the old window (a triggered ScanBalanceHealth /
//     RunHealthCheck is never cut short), and the new divisor governs
//     from the next scan. The rotation cursor is not reset: the next
//     scan continues from where the previous one left off, on the new
//     window size.
//   - Validation is fail-closed with no silent normalization: the new
//     divisor must be >= 1, else ErrInvalidHealthWatchWindowDivisor
//     and the live divisor stays exactly as it was. Zero is rejected
//     — it would divide by zero in the window computation — and
//     negatives are rejected. There is no upper bound: the compute
//     trade-off is the operator's (along the LG-346 outbox buffer,
//     whose cap is likewise unbounded).
//   - A revision identical to the live divisor is a no-op: it returns
//     the live value with a nil error and writes no audit event, so a
//     retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     health_watch_window_divisor_retuned audit event (previous /
//     current divisor as plain integers). The Ctx variant attributes
//     the operator (LG-73). The read path (scanBalanceHealthLocked)
//     holds l.mu's read lock; the setter holds the write lock, along
//     the representmentWindow morphology.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-326 family): watch tuning is
//     in-process structural config.
//   - Snapshots do not carry the divisor: a restore starts again at
//     the default 4, while the revision events themselves stay in the
//     audit trail for traceability.
//   - The divisor is NOT the scan cadence: it only decides how much
//     each scan covers. How often the book is scanned stays with
//     HealthWatch.SetInterval (LG-327) — cadence and coverage are
//     orthogonal knobs.
//
// Orthogonality: LG-49 owns the health watchdog mechanism itself
// (this module is its missing runtime revision entry point for the
// per-scan coverage), LG-327 owns the scan cadence (how often, this
// module how much per scan), LG-61 owns the per-account
// balance-threshold watch (a different object: per-(account,
// currency) alert bands, not the background whole-book invariant
// re-check).

var (
	// ErrInvalidHealthWatchWindowDivisor is returned by
	// SetHealthWatchWindowDivisor when the requested divisor is less
	// than 1. The live divisor is left untouched.
	ErrInvalidHealthWatchWindowDivisor = errors.New("ledger: balance health watch: invalid window divisor: window divisor must be at least 1")
)

// SetHealthWatchWindowDivisor revises the live spot-check window
// divisor in place and reports the pre-revision divisor (for the
// audit trail). A divisor of 1 makes a single scan cover every key;
// a larger divisor shrinks each scan's slice and stretches full
// coverage over more scans. The revision is future-only and never
// resets the rotation cursor. A divisor less than 1 returns
// ErrInvalidHealthWatchWindowDivisor with the live divisor untouched
// (the returned value is the live divisor in that case too). A
// revision identical to the live divisor is a no-op that returns the
// live value with a nil error and no side effect.
func (l *Ledger) SetHealthWatchWindowDivisor(n int) (int, error) {
	return l.SetHealthWatchWindowDivisorCtx(context.Background(), n)
}

// SetHealthWatchWindowDivisorCtx is SetHealthWatchWindowDivisor with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetHealthWatchWindowDivisorCtx(ctx context.Context, n int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.healthWatchWindowDivisor
	if n < 1 {
		return previous, fmt.Errorf("%w: %d", ErrInvalidHealthWatchWindowDivisor, n)
	}
	if n == previous {
		return previous, nil
	}
	l.healthWatchWindowDivisor = n
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "health_watch_window_divisor_retuned",
		Actor:         "SetHealthWatchWindowDivisor",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_window_divisor": previous,
			"current_window_divisor":  n,
		},
	})
	return previous, nil
}

// HealthWatchWindowDivisor reports the live spot-check window
// divisor: each scan covers ceil(n/divisor) of the (account,
// currency) keys.
func (l *Ledger) HealthWatchWindowDivisor() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.healthWatchWindowDivisor
}
