package ledger

import (
	"context"
	"errors"
	"time"
)

// FX-rate-watch interval runtime revision (LG-324).
//
// StartFXRateWatch fixes the watcher's scan cadence — how often the
// rate table is scanned for about-to-expire and expired rates — for
// the life of the worker: fxRateWatchConfig() (main.go) reads
// LEDGER_FX_RATE_WATCH_INTERVAL once at startup, and run() built its
// single ticker from that value with no revision entry point
// (SetFXRateWatchInterval was a whole-repo zero hit). The only way to
// tighten the scan from 5m to 30s (a jittery rate source needs the
// stale warning out sooner, so an expiring rate is renewed before
// transfers start failing with ErrFXRateExpired) or loosen it back
// to save polling on a stable source was to change the env var and
// restart the process. This module is the orthogonal runtime path:
// an operator revises the live watch's scan interval in place,
// instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-323's
// SetWarningThreshold (the same worker, the threshold knob) and the
// LG-316 ReportSweeper pattern (the same retune shape, a different
// worker):
//
//   - The revision is in place: the watch keeps its goroutine, its
//     warning threshold, its seen dedup marks, and its Ticks /
//     StaleTotal / ExpiredTotal counters — none of them resets.
//     Only the scan cadence moves. No restart, no Stop + Start.
//   - The revision is future-only: the run loop selects between
//     scans, so a scan already in progress runs to completion on
//     the old cadence (a triggered scan is never cut short), and
//     the new interval governs from the next tick. The loop
//     rebuilds its ticker inside run(): the old ticker is stopped
//     before the new one is built, so two tickers never run in
//     parallel, and a tick that fired while the revision was in
//     flight is run before the rebuild, so a revision cannot
//     swallow a due scan.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidFXRateWatchInterval
//     and the live interval stays exactly as it was. Zero is
//     rejected too — unlike LG-323's threshold, where 0 silences
//     stale warnings only: zero here would stop the whole worker,
//     and a runtime revision never silently disables it (along
//     LG-290 / LG-304 / LG-306 / LG-314 / LG-316).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     fx_rate_watch_interval_retuned audit event (previous /
//     current watch interval as millisecond integers — purely
//     numeric). The Ctx variant attributes the operator (LG-73).
//     Lock order: the setter releases w.mu, then takes l.mu.RLock
//     to emit — w.mu and l.mu are never held nested, so the lock
//     order is acyclic. run() reads the interval under w.mu.RLock
//     when it rebuilds the ticker, so the two paths never nest
//     either.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-323 family): watch tuning is
//     in-process structural config. main.go stores the started
//     watch handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the watch interval: the interval comes
//     only from the startup-time LEDGER_FX_RATE_WATCH_INTERVAL
//     parse, so a restart restores the env value while the revision
//     events themselves stay in the audit trail for traceability.
//     Persisting a revision across restarts would need the env var
//     to be re-read, a later round.
//   - The interval is NOT the warning threshold: it only decides
//     how often the table is scanned. The stale-warning window
//     (LG-323's SetWarningThreshold) still decides how far ahead
//     of expiry a rate counts as stale, and each rate's ExpiresAt
//     (SetFXRateRat's TTL) still decides when it expires. The seen
//     dedup marks are not reset: tightening the scan only makes
//     due warnings fire sooner, and loosening it only delays them
//     — neither re-reports nor revokes a warning already fired
//     for the same rate identity.
//   - A watch that was never started (env unset or invalid, so
//     fxRateWatchConfig() returned false and StartFXRateWatch was
//     never called) has no handle to revise: the revision applies
//     only to a running watch.
//
// Orthogonality: LG-68 owns the FX watch mechanism itself (this
// module is its missing runtime revision entry point for the scan
// cadence), LG-323 owns revising the warning threshold (the same
// worker, a different knob — this item is the fulfillment of the
// interval the LG-323 module explicitly excluded and deferred to a
// separate item), LG-76 owns the triangular-consistency watch (same watch
// family, different worker — its interval is frozen but explicitly
// a separate item), LG-289 owns the rate-provider runtime swap
// (that knob is the data source, this knob is the scan cadence),
// LG-214 owns the FX breaker (a per-pair deviation cap that blocks
// conversion; this module only warns, never blocks), SetFXRateRat
// owns a single rate's TTL / expiry instant (the expiry itself;
// this module owns how often the table is scanned for it),
// LG-316..LG-322 own the seven sweeper scan intervals (the same
// revision pattern, different workers), and LG-314 owns the
// audit-sink idle poll interval (the same (previous, error) return
// and audit shape, a different component).

var (
	// ErrInvalidFXRateWatchInterval is returned by
	// FXRateWatch.SetInterval when the requested interval is not
	// positive. The live interval is left untouched.
	ErrInvalidFXRateWatchInterval = errors.New("ledger: FX rate watch: invalid watch interval: watch interval must be positive")
)

// SetInterval revises the watch's scan interval in place and reports
// the pre-revision interval (for the audit trail). The revision is
// future-only: a scan already in progress runs to completion, and
// only the next tick uses the new interval; a tick that fired while
// the revision was in flight is run before the ticker is rebuilt,
// so it is never swallowed. A revision identical to the live
// interval is a no-op that returns the live value with a nil error
// and no side effect. A non-positive interval returns
// ErrInvalidFXRateWatchInterval with the live interval untouched
// (the returned value is the live interval in that case too).
func (w *FXRateWatch) SetInterval(d time.Duration) (time.Duration, error) {
	return w.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (w *FXRateWatch) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	w.mu.Lock()
	previous := w.interval
	if d <= 0 {
		w.mu.Unlock()
		return previous, ErrInvalidFXRateWatchInterval
	}
	if d == previous {
		w.mu.Unlock()
		return previous, nil
	}
	w.interval = d
	w.mu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under mu, so a
	// newer revision supersedes this one even if its wakeup was
	// coalesced into this one; the send is non-blocking so a
	// revision never stalls on a loop that is mid-scan.
	select {
	case w.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-323 retunes: w.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox.
	w.l.mu.RLock()
	w.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_rate_watch_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: w.l.version,
		VersionAfter:  w.l.version,
		Details: map[string]any{
			"previous_watch_interval_ms": previous.Milliseconds(),
			"current_watch_interval_ms":  d.Milliseconds(),
		},
	})
	w.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the watch's live scan interval.
func (w *FXRateWatch) Interval() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.interval
}
