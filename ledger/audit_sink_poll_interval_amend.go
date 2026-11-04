package ledger

import (
	"context"
	"errors"
	"time"
)

// Audit-sink idle-poll-interval amendment (LG-314).
//
// StartAuditSink fixes the sink's idle poll rhythm — PollInterval,
// the wait between outbox polls when no new events are waiting —
// for the life of the forwarder: withDefaults and Validate run only
// at construction, and the run goroutine already reads the live
// value under cfgMu on every idle sleep (pollInterval, LG-306), so
// the setter below only swaps s.cfg.PollInterval under cfgMu.Lock.
// The run loop, deliver, post, and sinkSleep need zero changes. The
// only way to retune a live sink was Stop + StartAuditSink, which is
// a teardown, not a revision: Stop waits for the run goroutine to
// exit, a restart re-reads the cursor file (no progress is lost, but
// the stream stalls for the gap), a delivery in flight is cut short
// by the done channel, and a failed restart stops forwarding
// entirely. This module is the orthogonal runtime path: an operator
// revises the idle poll interval in place, instead of rebuilding
// the sink.
//
// Semantics, deliberately fail-closed throughout, along LG-306's
// delivery-policy revision (the same shape, one knob instead of
// four):
//
//   - The revision is in place: the cursor, the queue order, the
//     counters, the signing secret, and the four LG-306 delivery
//     knobs stay exactly as they were. Only the idle poll interval
//     moves.
//   - The revision is future-only: the run goroutine's idle sleeps
//     already read the live value under cfgMu via pollInterval, so
//     the next poll cycle uses the new value. A sleep already in
//     progress is not woken or shortened: sinkSleep built its
//     time.Timer when the sleep started, and the revision only swaps
//     the value the next sleep is built from (the same ordering note
//     as LG-306's "in-flight delivery keeps old policy", applied to
//     the idle sleep instead of a delivery). Tightening the interval
//     therefore never hurries an event past the remainder of the
//     sleep the sink is already in; it shortens only the sleeps
//     that start after the revision.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidSinkPollInterval and
//     the live value stays exactly as it was. Zero is rejected too
//     — the construction-time withDefaults zero-fill ("zero selects
//     the default") is deliberately NOT reused: a runtime revision
//     never silently normalizes (along LG-290 / LG-304 / LG-306).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event — so the sink never forwards
//     its own revision event and there is no self-loop. It lands one
//     sink_poll_interval_retuned audit event (previous/current
//     poll interval as millisecond integers — purely numeric, no URL
//     or key material). The Ctx variant attributes the operator
//     (LG-73). Lock order follows deadLetter and LG-306: the setter
//     releases cfgMu, then takes l.mu.RLock to emit — cfgMu and
//     l.mu are never held nested, so the lock order is acyclic.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     poll-interval tuning is in-process structural config.
//   - Snapshots do not carry the poll interval: the sink config
//     comes only from the startup-time LEDGER_AUDIT_SINK_URL/SECRET
//     parse (main.go never sets PollInterval, so the sink starts on
//     the 1s default), so a restart restores the default 1s while
//     the revision events themselves stay in the audit trail for
//     traceability. Persisting a revision across restarts would need
//     a LEDGER_AUDIT_SINK_POLL_INTERVAL env var, a later round.
//   - This module deliberately adds no Ledger-level setter: the
//     Ledger never holds the sink (AuditSink appears in ledger/*.go
//     only inside audit_sink.go; main.go's srv.auditSink is the sole
//     holder), so callers holding the *AuditSink revise it directly
//     — the same deliberate departure as LG-306 and LG-313.
//   - Explicitly out of scope: URL (endpoint identity — changing
//     the address is Stop + StartAuditSink), Secret (signing-key
//     rotation is LG-313), CursorFile (cursor identity, fixed at
//     start), the four LG-306 delivery knobs, and sinkPollLimit
//     (the hardcoded 100-event poll-page cap, audit_sink.go).
//
// Orthogonality: LG-67 owns the sink mechanism itself (polling,
// cursor, signing, dead-lettering — configurable only at start;
// this module is its runtime entry point for the idle rhythm),
// LG-306 owns the four delivery knobs (a different concern: retry
// budget, backoff, per-attempt timeout — this module is the idle
// poll interval, not a delivery knob), LG-313 owns signing-secret
// rotation (key material; this module is a duration), and LG-301
// owns the audit log's rotation cap and retention window.

var (
	// ErrInvalidSinkPollInterval is returned by
	// AuditSink.SetSinkPollInterval when the requested interval is
	// not positive. The live interval is left untouched.
	ErrInvalidSinkPollInterval = errors.New("ledger: audit sink: invalid sink poll interval: poll interval must be positive")
)

// SetSinkPollInterval revises the sink's idle poll interval in place
// and reports the pre-revision interval (for the audit trail). The
// revision is future-only: a sleep already in progress keeps running
// out its old duration (sinkSleep built its timer when the sleep
// started); only the next poll cycle uses the new value. A revision
// identical to the live interval is a no-op that returns the live
// value with a nil error and no side effect. A non-positive interval
// returns ErrInvalidSinkPollInterval with the live interval
// untouched (the returned value is the live interval in that case
// too).
func (s *AuditSink) SetSinkPollInterval(d time.Duration) (time.Duration, error) {
	return s.SetSinkPollIntervalCtx(context.Background(), d)
}

// SetSinkPollIntervalCtx is SetSinkPollInterval with per-call audit
// actor attribution (LG-73).
func (s *AuditSink) SetSinkPollIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	s.cfgMu.Lock()
	previous := s.cfg.PollInterval
	if d <= 0 {
		s.cfgMu.Unlock()
		return previous, ErrInvalidSinkPollInterval
	}
	if d == previous {
		s.cfgMu.Unlock()
		return previous, nil
	}
	s.cfg.PollInterval = d
	s.cfgMu.Unlock()

	// Audit after the swap, under l.mu.RLock like deadLetter and
	// LG-306: cfgMu and l.mu are never held nested, so the lock
	// order is acyclic. emitAudit is a no-op when no audit log is
	// attached, and the event goes only to the audit log — never to
	// the outbox, so the sink cannot forward its own revision event.
	s.l.mu.RLock()
	s.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sink_poll_interval_retuned",
		Actor:         "SetSinkPollInterval",
		VersionBefore: s.l.version,
		VersionAfter:  s.l.version,
		Details: map[string]any{
			"previous_poll_interval_ms": previous.Milliseconds(),
			"current_poll_interval_ms":  d.Milliseconds(),
		},
	})
	s.l.mu.RUnlock()
	return previous, nil
}

// PollInterval reports the sink's live idle poll interval.
func (s *AuditSink) PollInterval() time.Duration {
	return s.pollInterval()
}
