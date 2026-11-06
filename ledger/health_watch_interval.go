package ledger

import (
	"context"
	"errors"
	"time"
)

// Balance-health-watch interval runtime revision (LG-327).
//
// StartHealthWatch fixes the watchdog's scan cadence — how often the
// whole book is re-checked against the three invariants (negative
// balances on overdraft-protected accounts, the accounting equation,
// chain/version continuity) — for the life of the worker:
// healthWatchInterval() (main.go) reads LEDGER_HEALTH_WATCH_INTERVAL
// once at startup, and run() built its single ticker from that value
// with no revision entry point (SetHealthWatchInterval was a
// whole-repo zero hit). The only way to tighten the scan from 5m to
// 10s (a suspicious snapshot import or suspected ledger corruption
// needs a negative balance, a broken accounting equation, or a
// chain/version drift found sooner) or loosen it back to shed
// full-table scan load and audit noise on a quiet day was to change
// the env var and restart the process. This module is the orthogonal
// runtime path: an operator revises the live watch's scan interval in
// place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-324's
// FXRateWatch.SetInterval (the same retune shape, a different worker)
// and the LG-316 ReportSweeper pattern:
//
//   - The revision is in place: the watch keeps its goroutine, its
//     rotation cursor, its Ticks / ViolationsTotal counters, and the
//     ledger's own violation counter — none of them resets. Only the
//     scan cadence moves. No restart, no Stop + Start.
//   - The revision is future-only: the run loop selects between
//     scans, so a scan already in progress runs to completion on
//     the old cadence (a triggered RunHealthCheck is never cut
//     short), and the new interval governs from the next tick. The
//     loop rebuilds its ticker inside run(): the old ticker is
//     stopped before the new one is built, so two tickers never run
//     in parallel, and a tick that fired while the revision was in
//     flight is run before the rebuild, so a revision cannot
//     swallow a due scan.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidHealthWatchInterval
//     and the live interval stays exactly as it was. Zero is
//     rejected too — unlike the startup-time rule, where a
//     non-positive interval disables the watchdog: zero here would
//     stop the whole worker, and a runtime revision never silently
//     disables it (along LG-314 / LG-316 / LG-324).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     health_watch_interval_retuned audit event (previous /
//     current watch interval as millisecond integers — purely
//     numeric). The Ctx variant attributes the operator (LG-73).
//     Lock order: the setter releases hw.mu, then takes l.mu.RLock
//     to emit — hw.mu and l.mu are never held nested, so the lock
//     order is acyclic. run() reads the interval under hw.mu.RLock
//     when it rebuilds the ticker, so the two paths never nest
//     either. The setter never calls RunHealthCheck while holding
//     hw.mu — that path takes l.mu itself.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-326 family): watch tuning is
//     in-process structural config. main.go stores the started
//     watch handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the watch interval: the interval comes
//     only from the startup-time LEDGER_HEALTH_WATCH_INTERVAL
//     parse, so a restart restores the env value while the revision
//     events themselves stay in the audit trail for traceability.
//     Persisting a revision across restarts would need the env var
//     to be re-read, a later round.
//   - The interval is NOT the per-scan coverage: it only decides
//     how often the book is scanned. Each scan still spot-checks
//     exactly a quarter of the (account, currency) keys
//     (healthWatchWindowDivisor = 4, a hard-coded design constant),
//     so every key is still re-checked at least once every four
//     scans — tightening the cadence only shortens the wall-clock
//     time to full coverage. The rotation cursor is an atomic
//     counter and is not reset by a revision.
//   - A watch that was never started (env unset or invalid, so
//     healthWatchInterval() returned false and StartHealthWatch was
//     never called) has no handle to revise: the revision applies
//     only to a running watch.
//
// Orthogonality: LG-49 owns the health watchdog mechanism itself
// (this module is its missing runtime revision entry point for the
// scan cadence), LG-318/LG-319/LG-320 own the pre-auth / freeze /
// close sweeper intervals (the same revision pattern, different
// workers — each explicitly excluded this item and deferred it to a
// separate item), LG-316/LG-317/LG-321/LG-322 own the remaining
// sweeper intervals, LG-323/LG-324/LG-325/LG-326 own the FX watch
// knobs (same family, different workers), LG-314 owns the audit-sink
// idle poll interval (the same (previous, error) return and audit
// shape, a different component), and LG-61 owns the per-account
// balance-threshold watch (a different object: per-(account,
// currency) alert bands, not the background whole-book invariant
// re-check).

var (
	// ErrInvalidHealthWatchInterval is returned by
	// HealthWatch.SetInterval when the requested interval is not
	// positive. The live interval is left untouched.
	ErrInvalidHealthWatchInterval = errors.New("ledger: balance health watch: invalid watch interval: watch interval must be positive")
)

// SetInterval revises the watch's scan interval in place and reports
// the pre-revision interval (for the audit trail). The revision is
// future-only: a scan already in progress runs to completion, and
// only the next tick uses the new interval; a tick that fired while
// the revision was in flight is run before the ticker is rebuilt,
// so it is never swallowed. A revision identical to the live
// interval is a no-op that returns the live value with a nil error
// and no side effect. A non-positive interval returns
// ErrInvalidHealthWatchInterval with the live interval untouched
// (the returned value is the live interval in that case too).
func (hw *HealthWatch) SetInterval(d time.Duration) (time.Duration, error) {
	return hw.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (hw *HealthWatch) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	hw.mu.Lock()
	previous := hw.interval
	if d <= 0 {
		hw.mu.Unlock()
		return previous, ErrInvalidHealthWatchInterval
	}
	if d == previous {
		hw.mu.Unlock()
		return previous, nil
	}
	hw.interval = d
	hw.mu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under mu, so a
	// newer revision supersedes this one even if its wakeup was
	// coalesced into this one; the send is non-blocking so a
	// revision never stalls on a loop that is mid-scan.
	select {
	case hw.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-326 retunes: hw.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox.
	hw.l.mu.RLock()
	hw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "health_watch_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: hw.l.version,
		VersionAfter:  hw.l.version,
		Details: map[string]any{
			"previous_watch_interval_ms": previous.Milliseconds(),
			"current_watch_interval_ms":  d.Milliseconds(),
		},
	})
	hw.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the watch's live scan interval.
func (hw *HealthWatch) Interval() time.Duration {
	hw.mu.RLock()
	defer hw.mu.RUnlock()
	return hw.interval
}
