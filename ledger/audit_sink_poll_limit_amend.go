package ledger

import (
	"context"
	"errors"
)

// Audit-sink poll-page-limit amendment (LG-350).
//
// StartAuditSink fixes the sink's poll page — sinkPollLimit, the
// hardcoded 100-event cap on one outbox poll — for the life of the
// forwarder: the run goroutine passed the constant to
// ReadOutboxEvents on every poll, and the only way to change how
// many events one poll pulls was to edit the constant, recompile,
// and restart. A slow downstream SIEM wants a smaller page to
// shrink bursts; a fast one wants a larger page to cut poll cycles.
// This module is the orthogonal runtime path: an operator revises
// the page limit in place, instead of rebuilding the sink.
//
// Semantics, deliberately fail-closed throughout, along LG-314's
// poll-interval revision (the same shape, one knob instead of a
// duration):
//
//   - The limit is a cfgMu-guarded field on the sink, not a
//     SinkConfig field: the page limit never rode in SinkConfig
//     (the constant lived in audit_sink.go, not in the config), and
//     keeping it out of SinkConfig keeps this revision independent
//     of LG-351's startup-time env parsing for the five
//     SinkConfig knobs — there is no LEDGER_AUDIT_SINK_POLL_LIMIT
//     env var, by design. The run goroutine reads the live value
//     under cfgMu.RLock on every poll (pollLimit), so no restart
//     of the goroutine is needed.
//   - The revision is in place: the cursor, the queue order, the
//     counters, the signing secret, the four LG-306 delivery knobs,
//     and the LG-314 poll interval stay exactly as they were. Only
//     the page limit moves.
//   - The revision is future-only: the run goroutine reads the
//     limit once per poll, when it builds the OutboxQuery, so the
//     next poll uses the new value. A page already fetched is
//     unaffected: its events deliver in full under the old limit
//     before the next poll is built (the same ordering note as
//     LG-306's "in-flight delivery keeps old policy", applied to
//     the fetched page instead of a delivery).
//   - Validation is fail-closed with no silent normalization: the
//     new limit must be >= 1, else ErrInvalidSinkPollLimit and the
//     live value stays exactly as it was. Zero is rejected too —
//     the construction-time withDefaults zero-fill ("zero selects
//     the default") is deliberately NOT reused: a runtime revision
//     never silently normalizes (along LG-290 / LG-304 / LG-306 /
//     LG-314).
//   - A revision identical to the live limit is a no-op: it returns
//     the live value with a nil error and writes no audit event, so
//     a retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it
//     bumps neither the version nor the audit chain, writes no
//     journal entry, and emits no outbox event — so the sink never
//     forwards its own revision event and there is no self-loop. It
//     lands one sink_poll_limit_retuned audit event
//     (previous/current poll limit as plain integers — purely
//     numeric, no URL or key material). The Ctx variant attributes
//     the operator (LG-73). Lock order follows deadLetter and
//     LG-306/LG-314: the setter releases cfgMu, then takes l.mu.RLock
//     to emit — cfgMu and l.mu are never held nested, so the lock
//     order is acyclic.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     page-limit tuning is in-process structural config.
//   - Snapshots do not carry the page limit: the sink starts on the
//     100-event default, so a restart restores it while the
//     revision events themselves stay in the audit trail for
//     traceability. Persisting a revision across restarts would need
//     a LEDGER_AUDIT_SINK_POLL_LIMIT env var, a later round.
//   - This module deliberately adds no Ledger-level setter: the
//     Ledger never holds the sink (AuditSink appears in ledger/*.go
//     only inside audit_sink.go; main.go's srv.auditSink is the sole
//     holder), so callers holding the *AuditSink revise it directly
//     — the same deliberate departure as LG-306, LG-313, and LG-314.
//   - Explicitly out of scope: the delivery knobs (LG-306), the
//     poll interval (LG-314), the signing secret (LG-313), the
//     cursor, and any env var for the limit (LG-351).
//
// Orthogonality: LG-67 owns the sink mechanism itself (polling,
// cursor, signing, dead-lettering — configurable only at start;
// this module is its runtime entry point for the poll page size),
// LG-306 owns the four delivery knobs (a different concern: retry
// budget, backoff, per-attempt timeout — this module is how many
// events one poll pulls, not how each one is delivered), LG-314
// owns the idle poll interval (how often the sink polls — this
// module is how much each poll pulls), and LG-313 owns signing-
// secret rotation (key material; this module is a page size).

var (
	// ErrInvalidSinkPollLimit is returned by
	// AuditSink.SetSinkPollLimit when the requested limit is below 1.
	// The live limit is left untouched.
	ErrInvalidSinkPollLimit = errors.New("ledger: audit sink: invalid sink poll limit: poll limit must be >= 1")
)

// pollLimit reads the sink's live poll page limit under cfgMu, so the
// run goroutine never reads it without the lock.
func (s *AuditSink) pollLimit() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.pollPageLimit
}

// SetSinkPollLimit revises the sink's poll page limit in place and
// reports the pre-revision limit (for the audit trail). The revision
// is future-only: a page already fetched delivers in full under the
// old limit; only the next poll uses the new value. A revision
// identical to the live limit is a no-op that returns the live value
// with a nil error and no side effect. A limit below 1 returns
// ErrInvalidSinkPollLimit with the live limit untouched (the returned
// value is the live limit in that case too).
func (s *AuditSink) SetSinkPollLimit(n int) (int, error) {
	return s.SetSinkPollLimitCtx(context.Background(), n)
}

// SetSinkPollLimitCtx is SetSinkPollLimit with per-call audit actor
// attribution (LG-73).
func (s *AuditSink) SetSinkPollLimitCtx(ctx context.Context, n int) (int, error) {
	s.cfgMu.Lock()
	previous := s.pollPageLimit
	if n < 1 {
		s.cfgMu.Unlock()
		return previous, ErrInvalidSinkPollLimit
	}
	if n == previous {
		s.cfgMu.Unlock()
		return previous, nil
	}
	s.pollPageLimit = n
	s.cfgMu.Unlock()

	// Audit after the swap, under l.mu.RLock like deadLetter and
	// LG-306/LG-314: cfgMu and l.mu are never held nested, so the
	// lock order is acyclic. emitAudit is a no-op when no audit log
	// is attached, and the event goes only to the audit log — never
	// to the outbox, so the sink cannot forward its own revision
	// event.
	s.l.mu.RLock()
	s.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sink_poll_limit_retuned",
		Actor:         "SetSinkPollLimit",
		VersionBefore: s.l.version,
		VersionAfter:  s.l.version,
		Details: map[string]any{
			"previous_poll_limit": previous,
			"current_poll_limit":  n,
		},
	})
	s.l.mu.RUnlock()
	return previous, nil
}

// PollLimit reports the sink's live poll page limit.
func (s *AuditSink) PollLimit() int {
	return s.pollLimit()
}
