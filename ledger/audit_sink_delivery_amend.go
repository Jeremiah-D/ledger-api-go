package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Audit-sink delivery-policy amendment (LG-306).
//
// StartAuditSink fixes the sink's four delivery knobs — MaxAttempts,
// InitialBackoff, MaxBackoff, HTTPTimeout — for the life of the
// forwarder: withDefaults and Validate run only at construction, and
// the run goroutine reads the live config on every delivery. The only
// way to retune a live sink was Stop + StartAuditSink, which is a
// teardown, not a revision: Stop waits for the run goroutine to exit,
// a restart re-reads the cursor file (no progress is lost, but the
// stream stalls for the gap), a delivery in flight is cut short by
// the done channel, and a failed restart (outbox directory
// unavailable, corrupt cursor) stops forwarding entirely. This module
// is the orthogonal runtime path: an operator revises any subset of
// the four knobs in place, instead of rebuilding the sink.
//
// Semantics, deliberately fail-closed throughout, along LG-304's
// outbound-webhook delivery-policy revision (the same four knobs,
// the same shape):
//
//   - The revision is in place: the cursor, the queue order, the
//     counters, and the signing secret stay exactly as they were.
//     Only the four knobs move.
//   - The race is the real implementation point. The run goroutine
//     reads s.cfg without any lock today (deliver is only ever called
//     from run, so a single goroutine makes it race-free today) —
//     while a setter on another goroutine writing s.cfg directly
//     would race those reads. AuditSink therefore gains cfgMu,
//     a sync.RWMutex that guards cfg + client and nothing else: it
//     never reuses l.mu (the ledger write lock must never enter the
//     delivery path) and never reuses LG-301's AuditLog cfgMu (that
//     one guards the audit log's own config, a different component).
//     deliver snapshots the four knobs plus the client and the secret
//     under cfgMu.RLock at entry and throws the snapshot away after
//     the delivery; post takes the snapshotted client as a parameter.
//     The snapshot is taken at entry and the lock is released before
//     the first POST, so a long delivery never holds the lock. A
//     delivery in flight keeps the policy it started with; the
//     revision governs only the next delivery (future-only, along
//     LG-288 / LG-301 / LG-304). An HTTPTimeout change rebuilds
//     s.client — the client pointer is only ever read through the
//     snapshot. The run goroutine's PollInterval reads go through the
//     same lock (a small helper), so no field of cfg is ever read
//     without cfgMu.
//   - Validation is merge-then-install: the requested partial policy
//     is merged over the live one (nil fields keep the live value)
//     and the merged policy reuses the construction-time gates —
//     MaxAttempts >= 1, every duration > 0, InitialBackoff <=
//     MaxBackoff — without the construction-time "zero selects the
//     default" fallback or the withDefaults clamp. A runtime revision
//     never silently normalizes (along LG-290 / LG-304): an illegal
//     merged value is ErrInvalidSinkDeliveryPolicy and the live
//     policy stays exactly as it was.
//   - An all-nil policy, and a revision identical to the live policy
//     on every field, are no-ops: they return the live policy with a
//     nil error and write no audit event, so a retried or duplicated
//     revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event — so the sink never forwards
//     its own revision event and there is no self-loop. It lands one
//     audit_sink_delivery_policy_retuned audit event (per changed
//     field, previous_*/current_*; attempts as integers, durations
//     as millisecond integers — purely numeric, no URL or key
//     material). The Ctx variant attributes the operator (LG-73).
//     Lock order follows deadLetter: the setter releases cfgMu, then
//     takes l.mu.RLock to emit — cfgMu and l.mu are never held
//     nested, so the lock order is acyclic.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     delivery-policy tuning is in-process structural config.
//   - Snapshots do not carry the policy: the sink config comes only
//     from the startup-time LEDGER_AUDIT_SINK_URL/SECRET parse, so a
//     restart restores the defaults while the amendment events
//     themselves stay in the audit trail for traceability. Persisting
//     a revision across restarts would need LEDGER_AUDIT_SINK_* env
//     parsing for the four knobs, a later round.
//   - This module deliberately adds no Ledger-level setter: the
//     Ledger never holds the sink (AuditSink appears in ledger/*.go
//     only inside audit_sink.go; main.go's srv.auditSink is the sole
//     holder), so callers holding the *AuditSink revise it directly
//     — the one deliberate departure from LG-300–305's Ledger
//     delegate shape, for a reason verified in the code.
//   - Explicitly out of scope: URL (endpoint identity — LG-352
//     owns its live revision, SetSinkURL), Secret (signing-key rotation
//     is a separate concern), CursorFile (cursor identity, fixed at
//     start), and PollInterval (the run loop's idle poll rhythm, not
//     a delivery knob).
//
// Orthogonality: LG-67 owns the sink mechanism itself (polling,
// cursor, signing, dead-lettering — configurable only at start; this
// module is its runtime entry point), LG-304 owns the outbound
// webhook endpoints' delivery policy (a different component: a
// dispatcher with a multi-endpoint registry; this module is a single
// sink with no registry and no Ledger delegate), LG-301 owns the
// audit log's rotation cap and retention window, LG-305 owns audit
// line signing-key rotation (key material; this module is the four
// delivery knobs), and LG-102 owns the webhook mechanism itself.

var (
	// ErrInvalidSinkDeliveryPolicy is returned by
	// AuditSink.SetDeliveryPolicy when the merged delivery policy
	// fails the construction-time gates: MaxAttempts < 1, a
	// non-positive duration, or InitialBackoff > MaxBackoff. The
	// live policy is left untouched.
	ErrInvalidSinkDeliveryPolicy = errors.New("ledger: audit sink: invalid delivery policy: MaxAttempts must be >= 1, durations must be positive, and InitialBackoff must not exceed MaxBackoff")
)

// SinkDeliveryPolicy is a partial revision of the audit sink's four
// delivery knobs. A nil field keeps the live value; a non-nil field
// replaces it. All four nil is a no-op.
type SinkDeliveryPolicy struct {
	// MaxAttempts bounds the POST attempts per event, including the
	// first try. Must be >= 1 when set.
	MaxAttempts *int
	// InitialBackoff is the wait before the first retry; it doubles
	// per retry up to MaxBackoff. Must be positive and at most
	// MaxBackoff when set.
	InitialBackoff *time.Duration
	// MaxBackoff caps the doubling backoff. Must be positive when
	// set.
	MaxBackoff *time.Duration
	// HTTPTimeout bounds a single POST attempt. Must be positive
	// when set; changing it rebuilds the sink's http.Client.
	HTTPTimeout *time.Duration
}

// sinkDeliveryTuning is the four knobs as concrete values — the
// merged, validated form. It is comparable, so a no-op revision is a
// plain equality check against the live tuning.
type sinkDeliveryTuning struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	httpTimeout    time.Duration
}

// mergeSinkDeliveryPolicy overlays a partial revision on the live
// tuning: nil fields keep the live value.
func mergeSinkDeliveryPolicy(live sinkDeliveryTuning, p SinkDeliveryPolicy) sinkDeliveryTuning {
	merged := live
	if p.MaxAttempts != nil {
		merged.maxAttempts = *p.MaxAttempts
	}
	if p.InitialBackoff != nil {
		merged.initialBackoff = *p.InitialBackoff
	}
	if p.MaxBackoff != nil {
		merged.maxBackoff = *p.MaxBackoff
	}
	if p.HTTPTimeout != nil {
		merged.httpTimeout = *p.HTTPTimeout
	}
	return merged
}

// validateSinkDeliveryTuning applies the construction-time gates
// (SinkConfig.Validate) to a merged policy, without the
// construction-time "zero selects the default" fallback or the
// withDefaults clamp: a runtime revision never silently normalizes.
func validateSinkDeliveryTuning(t sinkDeliveryTuning) error {
	if t.maxAttempts < 1 {
		return fmt.Errorf("%w: MaxAttempts must be >= 1, got %d", ErrInvalidSinkDeliveryPolicy, t.maxAttempts)
	}
	if t.initialBackoff <= 0 || t.maxBackoff <= 0 || t.httpTimeout <= 0 {
		return fmt.Errorf("%w: durations must be positive (initial_backoff=%s, max_backoff=%s, http_timeout=%s)",
			ErrInvalidSinkDeliveryPolicy, t.initialBackoff, t.maxBackoff, t.httpTimeout)
	}
	if t.initialBackoff > t.maxBackoff {
		return fmt.Errorf("%w: InitialBackoff (%s) must not exceed MaxBackoff (%s)",
			ErrInvalidSinkDeliveryPolicy, t.initialBackoff, t.maxBackoff)
	}
	return nil
}

// tuningFromSinkConfig reads the four knobs out of a sink config as a
// concrete tuning.
func tuningFromSinkConfig(cfg SinkConfig) sinkDeliveryTuning {
	return sinkDeliveryTuning{
		maxAttempts:    cfg.MaxAttempts,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		httpTimeout:    cfg.HTTPTimeout,
	}
}

// policyFromSinkTuning renders a concrete tuning as a
// SinkDeliveryPolicy with every field set, so callers can read the
// live values back.
func policyFromSinkTuning(t sinkDeliveryTuning) SinkDeliveryPolicy {
	return SinkDeliveryPolicy{
		MaxAttempts:    ptrInt(t.maxAttempts),
		InitialBackoff: ptrDuration(t.initialBackoff),
		MaxBackoff:     ptrDuration(t.maxBackoff),
		HTTPTimeout:    ptrDuration(t.httpTimeout),
	}
}

// sinkDeliverySnapshot is the delivery policy one in-flight delivery
// runs under: the four knobs plus the http.Client and the signing
// secret, snapshotted under cfgMu.RLock at deliver entry and thrown
// away after the delivery. The client pointer is only ever read
// through the snapshot, so an HTTPTimeout revision that rebuilds
// s.client can never tear a delivery that already started. The
// target URL joins the snapshot (LG-352): post reads snap.url, never
// s.cfg.URL, so a SetSinkURL revision mid-delivery cannot tear a
// delivery that already started either.
type sinkDeliverySnapshot struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	secret         []byte
	client         *http.Client
	url            string
}

// deliverySnapshot snapshots the sink's live delivery policy. The
// lock is released before the caller POSTs anything — deliver holds
// no lock across the delivery.
func (s *AuditSink) deliverySnapshot() sinkDeliverySnapshot {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return sinkDeliverySnapshot{
		maxAttempts:    s.cfg.MaxAttempts,
		initialBackoff: s.cfg.InitialBackoff,
		maxBackoff:     s.cfg.MaxBackoff,
		secret:         s.cfg.Secret,
		client:         s.client,
		url:            s.cfg.URL,
	}
}

// pollInterval reads the sink's idle poll interval under cfgMu, so
// the run goroutine never reads cfg without the lock.
func (s *AuditSink) pollInterval() time.Duration {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.PollInterval
}

// SetDeliveryPolicy revises the sink's delivery knobs in place and
// reports the pre-revision policy (for the audit trail). The revision
// is future-only: a delivery already in flight keeps the policy it
// snapshotted at entry; only the next delivery reads the new values.
// An all-nil policy, and a revision identical to the live policy,
// are no-ops that return the live policy with a nil error and no
// side effect. An illegal merged policy returns
// ErrInvalidSinkDeliveryPolicy with the live policy untouched.
func (s *AuditSink) SetDeliveryPolicy(p SinkDeliveryPolicy) (SinkDeliveryPolicy, error) {
	return s.SetDeliveryPolicyCtx(context.Background(), p)
}

// SetDeliveryPolicyCtx is SetDeliveryPolicy with per-call audit actor
// attribution (LG-73).
func (s *AuditSink) SetDeliveryPolicyCtx(ctx context.Context, p SinkDeliveryPolicy) (SinkDeliveryPolicy, error) {
	s.cfgMu.Lock()
	live := tuningFromSinkConfig(s.cfg)
	merged := mergeSinkDeliveryPolicy(live, p)
	if err := validateSinkDeliveryTuning(merged); err != nil {
		s.cfgMu.Unlock()
		return SinkDeliveryPolicy{}, err
	}
	if merged == live {
		s.cfgMu.Unlock()
		return policyFromSinkTuning(live), nil
	}
	previous := live
	s.cfg.MaxAttempts = merged.maxAttempts
	s.cfg.InitialBackoff = merged.initialBackoff
	s.cfg.MaxBackoff = merged.maxBackoff
	if merged.httpTimeout != live.httpTimeout {
		// Rebuild the client so the next delivery times out under
		// the new bound; in-flight deliveries hold the old client
		// through their snapshot.
		s.client = &http.Client{Timeout: merged.httpTimeout}
	}
	s.cfg.HTTPTimeout = merged.httpTimeout
	s.cfgMu.Unlock()

	// Audit after the swap, under l.mu.RLock like deadLetter: cfgMu
	// and l.mu are never held nested, so the lock order is acyclic.
	// emitAudit is a no-op when no audit log is attached, and the
	// event goes only to the audit log — never to the outbox, so the
	// sink cannot forward its own revision event.
	details := map[string]any{}
	if merged.maxAttempts != previous.maxAttempts {
		details["previous_max_attempts"] = previous.maxAttempts
		details["current_max_attempts"] = merged.maxAttempts
	}
	if merged.initialBackoff != previous.initialBackoff {
		details["previous_initial_backoff_ms"] = previous.initialBackoff.Milliseconds()
		details["current_initial_backoff_ms"] = merged.initialBackoff.Milliseconds()
	}
	if merged.maxBackoff != previous.maxBackoff {
		details["previous_max_backoff_ms"] = previous.maxBackoff.Milliseconds()
		details["current_max_backoff_ms"] = merged.maxBackoff.Milliseconds()
	}
	if merged.httpTimeout != previous.httpTimeout {
		details["previous_http_timeout_ms"] = previous.httpTimeout.Milliseconds()
		details["current_http_timeout_ms"] = merged.httpTimeout.Milliseconds()
	}
	s.l.mu.RLock()
	s.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_sink_delivery_policy_retuned",
		Actor:         "SetDeliveryPolicy",
		VersionBefore: s.l.version,
		VersionAfter:  s.l.version,
		Details:       details,
	})
	s.l.mu.RUnlock()
	return policyFromSinkTuning(previous), nil
}

// DeliveryPolicy reports the sink's live delivery policy, with every
// field set to its resolved value.
func (s *AuditSink) DeliveryPolicy() SinkDeliveryPolicy {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return policyFromSinkTuning(tuningFromSinkConfig(s.cfg))
}
