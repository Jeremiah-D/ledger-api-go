package ledger

import (
	"context"
	"errors"
)

// FX triangular-consistency threshold runtime revision (LG-325).
//
// StartFXConsistencyWatch fixes the watcher's alert threshold — how
// far a directed 3-cycle's exact product may deviate from 1 before it
// pages — for the life of the worker: fxConsistencyWatchConfig()
// (main.go) reads LEDGER_FX_CONSISTENCY_THRESHOLD_BPS once at
// startup, and tick() read w.thresholdBps with no revision entry
// point (SetFXConsistencyThreshold / SetThresholdBps were whole-repo
// zero hits). The only way to tighten the gate from 200 bps to 50
// (a jittery rate feed needs the leak caught sooner) or loosen it
// back to 500 (a noisy feed pages too often) was to change the env
// var and restart the process. This module is the orthogonal runtime
// path: an operator revises the live watch's threshold in place,
// instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-323's
// SetWarningThreshold (the same watch family, the threshold knob —
// this item is its exact isomorph for the consistency watch) and
// LG-289's live-read pattern:
//
//   - The revision is in place: the watch keeps its goroutine, its
//     scan interval, its seen dedup marks, and its Ticks / Total
//     counters — none of them resets. Only the alert gate moves. No
//     restart, no Stop + Start, no ticker rebuild (the interval is a
//     separate knob, LG-324's SetInterval on the sibling watch —
//     this watch's interval stays frozen by this item).
//   - The revision is future-only: tick() re-reads the live value
//     under w.mu on every round, so the new threshold governs from
//     the next tick. A scan already in progress runs to completion
//     on the threshold it read (its ScanFXRateInconsistency +
//     ReportFXRateInconsistency pair is never cut short).
//   - Validation is fail-closed with no silent normalization: the
//     new threshold must not be negative, else
//     ErrInvalidFXConsistencyThresholdBps and the live threshold
//     stays exactly as it was. Zero is ALLOWED — unlike the
//     sweeper interval retunes, which reject d <= 0: zero
//     explicitly disables the consistency check (ScanFXRateInconsistency
//     returns nil for a non-positive threshold) while the watch
//     itself keeps ticking, the same reading the construction-time
//     threshold already had. A runtime revision never stops the
//     worker (stopping still goes through Stop / process
//     shutdown). This is the LG-323 zero semantics, not the
//     sweeper full-reject: zero here silences one alert category,
//     not the worker — the FX watch's warningThreshold zero keeps
//     expiry warnings alive, and here the zero keeps the tick
//     alive with the check off.
//   - A revision identical to the live threshold is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     fx_consistency_threshold_retuned audit event (previous /
//     current threshold as int64 basis points — purely numeric,
//     never a duration or milliseconds). The Ctx variant
//     attributes the operator (LG-73). Lock order: the setter
//     releases w.mu, then takes l.mu.RLock to emit — w.mu and
//     l.mu are never held nested, so the lock order is acyclic.
//     tick() reads the threshold under w.mu.RLock before the
//     scan, so the two paths never nest either.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-325 family): threshold tuning is
//     in-process structural config. main.go stores the started
//     watch handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the threshold: it comes only from the
//     startup-time LEDGER_FX_CONSISTENCY_THRESHOLD_BPS parse, so
//     a restart restores the env value while the revision events
//     themselves stay in the audit trail for traceability.
//   - The threshold is NOT a rate's expiry and NOT the watch
//     interval: it only decides how far a cycle's product may
//     deviate from 1 before it pages. Each rate's ExpiresAt
//     (SetFXRateRat's TTL) still decides when it expires, the
//     ErrFXRateExpired transfer gate is untouched, and the
//     FX watch's warningThreshold (LG-323) is a different knob on
//     a different worker. The seen dedup marks are not reset:
//     tightening lets a cycle that newly exceeds the gate report
//     once on the next tick, and neither re-reports nor revokes
//     an alert already fired for the same leg identity; loosening
//     prunes the marks of cycles that fall back inside the gate
//     on the next tick (the existing prune pass), so re-tightening
//     re-arms them.
//   - A watch that was never started (env unset or invalid, so
//     fxConsistencyWatchConfig() returned false and
//     StartFXConsistencyWatch was never called) has no handle to
//     revise: the revision applies only to a running watch.
//
// Orthogonality: LG-76 owns the consistency watch mechanism itself
// (this module is its missing runtime revision entry point for the
// alert threshold), LG-323 owns revising the FX-rate-watch warning
// threshold (the same revision pattern, a different worker and a
// different knob — a duration window ahead of expiry, this one an
// int64 basis-point gate), LG-324 owns revising the FX-rate-watch
// scan interval (this watch's interval is frozen and explicitly a
// separate item), LG-289 owns the rate-provider runtime swap (that
// knob is the data source, this knob is the alert gate), LG-34
// owns the rate table and the expiry gate (this module only moves
// the warning gate, never the gate), LG-214 owns the FX breaker (a
// per-pair deviation cap that blocks conversion; this module only
// warns, never blocks), SetFXRateRat owns a single rate's TTL /
// expiry instant (the expiry itself; this module owns how far a
// cycle may deviate before it pages), LG-316..LG-322 own the seven
// sweeper scan intervals (the same revision pattern, different
// workers), and LG-314 owns the audit-sink idle poll interval (the
// same (previous, error) return and audit shape, a different
// component).

var (
	// ErrInvalidFXConsistencyThresholdBps is returned by
	// FXConsistencyWatch.SetThresholdBps when the requested
	// threshold is negative. The live threshold is left untouched.
	ErrInvalidFXConsistencyThresholdBps = errors.New("ledger: FX consistency watch: invalid threshold: threshold bps must not be negative")
)

// SetThresholdBps revises the watch's triangular-consistency alert
// threshold in place and reports the pre-revision threshold (for
// the audit trail). The revision is future-only: a scan already in
// progress runs to completion on the threshold it read, and only
// the next tick uses the new value. A revision identical to the
// live threshold is a no-op that returns the live value with a nil
// error and no side effect. A negative threshold returns
// ErrInvalidFXConsistencyThresholdBps with the live threshold
// untouched (the returned value is the live threshold in that
// case too). Zero is allowed: it disables the consistency check —
// ScanFXRateInconsistency returns nil — while the watch itself
// keeps ticking.
func (w *FXConsistencyWatch) SetThresholdBps(bps int64) (int64, error) {
	return w.SetThresholdBpsCtx(context.Background(), bps)
}

// SetThresholdBpsCtx is SetThresholdBps with per-call audit actor
// attribution (LG-73).
func (w *FXConsistencyWatch) SetThresholdBpsCtx(ctx context.Context, bps int64) (int64, error) {
	w.mu.Lock()
	previous := w.thresholdBps
	if bps < 0 {
		w.mu.Unlock()
		return previous, ErrInvalidFXConsistencyThresholdBps
	}
	if bps == previous {
		w.mu.Unlock()
		return previous, nil
	}
	w.thresholdBps = bps
	w.mu.Unlock()

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-324 retunes: w.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox.
	w.l.mu.RLock()
	w.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_consistency_threshold_retuned",
		Actor:         "SetThresholdBps",
		VersionBefore: w.l.version,
		VersionAfter:  w.l.version,
		Details: map[string]any{
			"previous_threshold_bps": previous,
			"current_threshold_bps":  bps,
		},
	})
	w.l.mu.RUnlock()
	return previous, nil
}

// ThresholdBps reports the watch's live triangular-consistency
// alert threshold in basis points. Zero means the check is
// disabled (the watch itself keeps ticking).
func (w *FXConsistencyWatch) ThresholdBps() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.thresholdBps
}
