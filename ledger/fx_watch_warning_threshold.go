package ledger

import (
	"context"
	"errors"
	"time"
)

// FX-rate-warning-threshold runtime revision (LG-323).
//
// StartFXRateWatch fixes the watcher's stale-warning window — how
// far ahead of a rate's expiry the fx_rate_stale warning fires —
// for the life of the worker: fxRateWatchConfig() (main.go) reads
// LEDGER_FX_RATE_WARNING_THRESHOLD once at startup, and tick()
// read w.warningThreshold with no revision entry point
// (SetFXRateWarningThreshold was a whole-repo zero hit). The only
// way to widen the window from 72h to 7d (warn earlier in a volatile
// market, so a rate is renewed before it expires and transfers start
// failing with ErrFXRateExpired) or tighten it to 1h (less stale
// audit/outbox noise) was to change the env var and restart the
// process. This module is the orthogonal runtime path: an operator
// revises the live watch's warning threshold in place, instead of
// restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-314's
// SetSinkPollInterval (the same (previous, error) shape) and the
// LG-316..LG-322 sweeper retunes (the same revision pattern, a
// different knob family — this one is a threshold knob, not an
// interval knob):
//
//   - The revision is in place: the watch keeps its goroutine, its
//     scan interval, its seen dedup marks, and its Ticks /
//     StaleTotal / ExpiredTotal counters — none of them resets. Only
//     the stale-warning window moves. No restart, no Stop + Start.
//   - The revision is future-only: tick() re-reads the live value
//     under w.mu on every round, so the new threshold governs from
//     the next tick. A scan already in progress runs to completion
//     on the threshold it read (its ScanFXRateAlerts +
//     ReportFXRateAlerts pair is never cut short). No ticker
//     rebuild: the scan cadence is a separate knob (FXRateWatch.
//     interval) and is untouched.
//   - Validation is fail-closed with no silent normalization: the
//     new threshold must not be negative, else
//     ErrInvalidFXRateWarningThreshold and the live threshold stays
//     exactly as it was. Zero is ALLOWED — unlike the sweeper
//     retunes, which reject d <= 0: zero explicitly silences stale
//     warnings only (ScanFXRateAlerts checks stale only when
//     warningThreshold > 0) while expiry warnings continue, the
//     same reading the construction-time threshold already had. A
//     runtime revision never stops the worker (stopping still goes
//     through Stop / process shutdown).
//   - A revision identical to the live threshold is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     fx_rate_warning_threshold_retuned audit event (previous /
//     current warning threshold as millisecond integers — purely
//     numeric). The Ctx variant attributes the operator (LG-73).
//     Lock order: the setter releases w.mu, then takes l.mu.RLock
//     to emit — w.mu and l.mu are never held nested, so the lock
//     order is acyclic. tick() reads the threshold under w.mu.RLock
//     before the scan, so the two paths never nest either.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-322 family): threshold tuning is
//     in-process structural config. main.go stores the started
//     watch handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the warning threshold: the threshold
//     comes only from the startup-time
//     LEDGER_FX_RATE_WARNING_THRESHOLD parse, so a restart restores
//     the env value while the revision events themselves stay in
//     the audit trail for traceability.
//   - The threshold is NOT a rate's expiry: it only decides how
//     far ahead of expiry a rate counts as stale. Each rate's
//     ExpiresAt (SetFXRateRat's TTL) still decides when it expires,
//     and the ErrFXRateExpired transfer gate is untouched —
//     tightening the threshold never lets an expired rate through,
//     and widening it never rejects a live one. The seen dedup
//     marks are not reset: widening lets a rate that newly enters
//     the window report stale once on the next tick, and narrowing
//     neither re-reports nor revokes a stale warning already fired
//     for the same rate identity.
//   - A watch that was never started (env unset or invalid, so
//     fxRateWatchConfig() returned false and StartFXRateWatch was
//     never called) has no handle to revise: the revision applies
//     only to a running watch.
//
// Orthogonality: LG-68 owns the FX watch mechanism itself (this
// module is its missing runtime revision entry point), LG-289 owns
// the rate-provider runtime swap (that knob is the data source,
// this knob is the warning window — its tick reads the live
// provider per round, this module's tick reads the live threshold
// per round, the same shape), LG-34 owns the rate table and the
// expiry gate (this module only moves the warning window, never
// the gate), LG-76 owns the triangular-consistency watch (same
// watch family, different worker — its thresholdBps is frozen but
// explicitly a separate item), LG-214 owns the FX breaker (a
// per-pair deviation cap that blocks conversion; this module only
// warns, never blocks), SetFXRateRat owns a single rate's TTL /
// expiry instant (the expiry itself; this module owns the warning
// window ahead of it), SetFXRateBreaker owns the per-pair breaker
// threshold (a rejection gate; this module owns a warning window),
// LG-316..LG-322 own the seven sweeper scan intervals (the same
// revision pattern, different knob family — this item is a
// threshold knob, not an interval knob), and LG-314 owns the
// audit-sink idle poll interval (the same (previous, error) return
// and audit shape, a different component).

var (
	// ErrInvalidFXRateWarningThreshold is returned by
	// FXRateWatch.SetWarningThreshold when the requested threshold
	// is negative. The live threshold is left untouched.
	ErrInvalidFXRateWarningThreshold = errors.New("ledger: FX rate watch: invalid warning threshold: warning threshold must not be negative")
)

// SetWarningThreshold revises the watch's stale-warning threshold
// in place and reports the pre-revision threshold (for the audit
// trail). The revision is future-only: a scan already in progress
// runs to completion on the threshold it read, and only the next
// tick uses the new value. A revision identical to the live
// threshold is a no-op that returns the live value with a nil
// error and no side effect. A negative threshold returns
// ErrInvalidFXRateWarningThreshold with the live threshold
// untouched (the returned value is the live threshold in that
// case too). Zero is allowed: it silences stale warnings only —
// expiry warnings continue.
func (w *FXRateWatch) SetWarningThreshold(d time.Duration) (time.Duration, error) {
	return w.SetWarningThresholdCtx(context.Background(), d)
}

// SetWarningThresholdCtx is SetWarningThreshold with per-call audit
// actor attribution (LG-73).
func (w *FXRateWatch) SetWarningThresholdCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	w.mu.Lock()
	previous := w.warningThreshold
	if d < 0 {
		w.mu.Unlock()
		return previous, ErrInvalidFXRateWarningThreshold
	}
	if d == previous {
		w.mu.Unlock()
		return previous, nil
	}
	w.warningThreshold = d
	w.mu.Unlock()

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-322 retunes: w.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox.
	w.l.mu.RLock()
	w.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_rate_warning_threshold_retuned",
		Actor:         "SetWarningThreshold",
		VersionBefore: w.l.version,
		VersionAfter:  w.l.version,
		Details: map[string]any{
			"previous_warning_threshold_ms": previous.Milliseconds(),
			"current_warning_threshold_ms":  d.Milliseconds(),
		},
	})
	w.l.mu.RUnlock()
	return previous, nil
}

// WarningThreshold reports the watch's live stale-warning
// threshold. Zero means stale warnings are silenced (expiry
// warnings stay active).
func (w *FXRateWatch) WarningThreshold() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.warningThreshold
}
