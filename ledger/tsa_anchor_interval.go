package ledger

import (
	"context"
	"errors"
	"time"
)

// TSA timestamp-anchoring interval runtime revision (LG-328).
//
// StartTimestampAnchoring fixes the worker's anchoring cadence — how
// often the audit-chain head is countersigned by the external RFC 3161
// timestamp authority — for the life of the worker:
// tsaAnchorInterval() (main.go) reads LEDGER_TSA_INTERVAL once at
// startup, and run() built its single ticker from that value with no
// revision entry point (SetTimestampAnchorInterval was a whole-repo
// zero hit). The only way to tighten the cadence from 1h to 5m (after
// a TSA outage, re-anchor the chain head sooner to shrink the gap in
// external timestamp evidence) or loosen it back to save per-call TSA
// charges and audit noise under a rate limit was to change the env var
// and restart the process. This module is the orthogonal runtime path:
// an operator revises the live worker's anchoring interval in place,
// instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-324's
// FXRateWatch.SetInterval (the same retune shape, a different worker)
// and the LG-316 ReportSweeper pattern:
//
//   - The revision is in place: the worker keeps its goroutine, its
//     authority, its onAnchor hook, and its Ticks / AnchoredTotal /
//     FailuresTotal counters — none of them resets. Only the anchoring
//     cadence moves. No restart, no Stop + Start.
//   - The revision is future-only: the run loop selects between
//     anchors, so an anchor already in progress runs to completion
//     on the old cadence (a triggered AnchorTimestamp is never cut
//     short, and it still records the endpoint it was started
//     against), and the new interval governs from the next tick. The
//     loop rebuilds its ticker inside run(): the old ticker is
//     stopped before the new one is built, so two tickers never run
//     in parallel, and a tick that fired while the revision was in
//     flight is run before the rebuild, so a revision cannot
//     swallow a due anchor.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidTSAAnchorInterval
//     and the live interval stays exactly as it was. Zero is
//     rejected too — unlike the startup-time rule, where a
//     non-positive interval disables the worker: zero here would
//     stop the whole worker, and a runtime revision never silently
//     disables it (along LG-314 / LG-316 / LG-324). Stopping the
//     worker still goes through Stop / process shutdown.
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     tsa_anchor_interval_retuned audit event (previous / current
//     anchor interval as millisecond integers — purely numeric).
//     The Ctx variant attributes the operator (LG-73). Lock order:
//     the setter releases a.mu, then takes l.mu.RLock to emit —
//     a.mu and l.mu are never held nested, so the lock order is
//     acyclic. run() reads the interval under a.mu.RLock when it
//     rebuilds the ticker, so the two paths never nest either. The
//     setter never calls AnchorTimestamp while holding a.mu — that
//     path takes tsa.mu itself (and holds it while calling the
//     authority), so the three locks (a.mu, l.mu, tsa.mu) are never
//     held nested on the revision path.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-327 family): worker tuning is
//     in-process structural config. main.go stores the started
//     anchorer handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the anchor interval: the interval comes
//     only from the startup-time LEDGER_TSA_INTERVAL parse, so a
//     restart restores the env value while the revision events
//     themselves stay in the audit trail for traceability.
//     Persisting a revision across restarts would need the env var
//     to be re-read, a later round.
//   - The interval is NOT the endpoint and NOT the trust root: it
//     only decides how often the head is anchored. The TSA endpoint
//     (LG-303's SetTSAURL, resolved per tick) still decides where
//     timestamps come from — a retune never freezes the endpoint,
//     and the next tick after a retune still dials the live URL —
//     and the trust root (LG-302's SetTSATrustCert) still decides
//     which signers are trusted. Anchoring idempotency is untouched:
//     a revision changes when an anchor is attempted, never whether
//     one is needed — an unchanged head is still not re-anchored
//     against the same TSA, and old anchors keep their recorded URL
//     and Verified status.
//   - A worker that was never started (env unset or invalid, so
//     tsaAnchorInterval() returned false, or LEDGER_TSA_URL
//     unconfigured, so StartTimestampAnchoring was never called)
//     has no handle to revise: the revision applies only to a
//     running anchorer.
//
// Orthogonality: LG-84 owns the anchoring mechanism itself (this
// module is its missing runtime revision entry point for the cadence
// knob), LG-303 owns the TSA endpoint runtime re-sourcing ("who
// stamps" — this item is "how often"; LG-303's per-tick URL
// resolution is exactly what lets the tick after a retune still
// follow a new endpoint), LG-302 owns the TSA X.509 trust root
// ("who is trusted" — this item is the cadence), LG-100 owns the
// chain_anchored audit-chain evidence (the evidence consumer; this
// item is the evidence production cadence), LG-316..LG-322 own the
// seven sweeper scan intervals (the same revision pattern, different
// workers), LG-323..LG-326 own the FX watch knobs (same family,
// different workers), LG-327 owns the balance-health-watch interval
// (same family, different worker), and LG-314 owns the audit-sink
// idle poll interval (the same (previous, error) return and audit
// shape, a different component).

var (
	// ErrInvalidTSAAnchorInterval is returned by
	// TimestampAnchorer.SetInterval when the requested interval is
	// not positive. The live interval is left untouched.
	ErrInvalidTSAAnchorInterval = errors.New("ledger: TSA timestamp anchoring: invalid anchor interval: anchor interval must be positive")
)

// SetInterval revises the worker's anchoring interval in place and
// reports the pre-revision interval (for the audit trail). The
// revision is future-only: an anchor already in progress runs to
// completion, and only the next tick uses the new interval; a tick
// that fired while the revision was in flight is run before the
// ticker is rebuilt, so it is never swallowed. A revision identical
// to the live interval is a no-op that returns the live value with a
// nil error and no side effect. A non-positive interval returns
// ErrInvalidTSAAnchorInterval with the live interval untouched (the
// returned value is the live interval in that case too).
func (a *TimestampAnchorer) SetInterval(d time.Duration) (time.Duration, error) {
	return a.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (a *TimestampAnchorer) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	a.mu.Lock()
	previous := a.interval
	if d <= 0 {
		a.mu.Unlock()
		return previous, ErrInvalidTSAAnchorInterval
	}
	if d == previous {
		a.mu.Unlock()
		return previous, nil
	}
	a.interval = d
	a.mu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under mu, so a
	// newer revision supersedes this one even if its wakeup was
	// coalesced into this one; the send is non-blocking so a
	// revision never stalls on a loop that is mid-anchor.
	select {
	case a.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-327 retunes: a.mu and l.mu are never held nested,
	// so the lock order is acyclic. emitAuditCtx is a no-op when
	// no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry intervals
	// only, never token or secret material.
	a.l.mu.RLock()
	a.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "tsa_anchor_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: a.l.version,
		VersionAfter:  a.l.version,
		Details: map[string]any{
			"previous_anchor_interval_ms": previous.Milliseconds(),
			"current_anchor_interval_ms":  d.Milliseconds(),
		},
	})
	a.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the worker's live anchoring interval.
func (a *TimestampAnchorer) Interval() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.interval
}
