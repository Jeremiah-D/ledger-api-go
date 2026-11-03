package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Outbound webhook delivery-policy amendment (LG-304).
//
// Register fixes an endpoint's four delivery knobs — MaxAttempts,
// InitialBackoff, MaxBackoff, HTTPTimeout — for the life of the
// registration: withDefaults and Validate run only at construction,
// and the delivery worker reads the live config on every delivery.
// The only way to retune a live endpoint was Unregister + Register,
// which is a teardown, not a revision: Register refuses a duplicate
// name, Unregister deletes the registry entry, stops the worker and
// drains queued events as dropped, and a re-registration in between
// leaves a window in which new events match nothing. This module is
// the orthogonal runtime path: an operator revises any subset of the
// four knobs in place, instead of rebuilding the endpoint.
//
// Semantics, deliberately fail-closed throughout, along the LG-292 /
// LG-296 / LG-299 / LG-301 structural revisions:
//
//   - The revision is in place: the registry entry, the queue, the
//     worker, the delivery order, and the delivery records all stay
//     exactly as they were. Only the four knobs move — a delivery
//     already queued keeps its place, and the records and counters
//     keep counting.
//   - The race is the real implementation point. deliver reads the
//     endpoint config without any lock today (the worker goroutine),
//     while SetEnabled already writes under d.mu — so a setter that
//     wrote the four knobs under d.mu would race the worker's
//     lock-free reads. deliver therefore snapshots the four knobs
//     plus the http.Client under d.mu.RLock at entry and throws the
//     snapshot away after the delivery; post takes the snapshotted
//     client as a parameter. The snapshot is taken at entry and the
//     lock is released before the first POST — deliver still calls
//     appendRecord/updateRecord, which take d.mu themselves, so
//     holding the read lock across the delivery would deadlock.
//     A delivery in flight keeps the policy it started with; the
//     revision governs only the next delivery (future-only, along
//     LG-288 / LG-301). An HTTPTimeout change rebuilds e.client —
//     the client pointer is only ever read through the snapshot.
//   - Validation is merge-then-install: the requested partial policy
//     is merged over the live one (nil fields keep the live value)
//     and the merged policy reuses the construction-time gates —
//     MaxAttempts >= 1, every duration > 0, InitialBackoff <=
//     MaxBackoff — without the construction-time "zero selects the
//     default" fallback or the withDefaults clamp. A runtime revision
//     never silently normalizes (along LG-290): an illegal merged
//     value is ErrInvalidWebhookDeliveryPolicy and the live policy
//     stays exactly as it was.
//   - An all-nil policy, and a revision identical to the live policy
//     on every field, are no-ops: they return nil and write no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - An unknown endpoint returns ErrWebhookEndpointNotFound (a
//     sentinel, along RotateKey's unknown-endpoint shape). A ledger
//     with no dispatcher attached (WithWebhooks was never used)
//     returns ErrWebhooksNotConfigured (along LG-301's
//     ErrAuditLogNotConfigured).
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     webhook_delivery_policy_retuned audit event (the endpoint plus
//     previous_*/current_* per changed field, purely numeric —
//     durations in nanoseconds — no key or URL material). The Ctx
//     variant attributes the operator (LG-73). Lock order is l.mu
//     then d.mu, the order emitOutboxLocked already establishes when
//     it calls Notify under the ledger write lock.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     delivery-policy tuning is in-process structural config.
//   - Snapshots do not carry the policy: the dispatcher config comes
//     only from the startup-time LEDGER_WEBHOOKS parse, so a restart
//     restores the env values while the amendment events themselves
//     stay in the audit trail for traceability. Dry-run twins never
//     emit outbox events, so they never deliver webhooks either.
//   - Tenancy: WithWebhooks hands the same dispatcher to lazily
//     provisioned tenant ledgers, so a revision affects every tenant
//     sharing that dispatcher.
//   - Explicitly out of scope: URL (endpoint identity — changing the
//     address is Unregister + Register), Events (the subscription
//     filter, not a delivery knob), Keys (RotateKey owns them),
//     Enabled (SetEnabled owns it), and QueueSize (the channel
//     capacity is fixed at construction — along LG-301's QueueSize
//     exclusion for the audit log).
//
// Orthogonality: LG-102 owns the webhook mechanism itself (registry,
// retries, queries), LG-67 owns the audit sink's external forwarding
// (a different sink component with its own retries),
// Register/Unregister/SetEnabled/RotateKey own endpoint lifecycle
// and keys (none of them touches the four knobs), and this module
// owns only the runtime revision of an already-registered endpoint's
// delivery policy.

var (
	// ErrInvalidWebhookDeliveryPolicy is returned by
	// SetWebhookDeliveryPolicy when the merged delivery policy fails
	// the construction-time gates: MaxAttempts < 1, a non-positive
	// duration, or InitialBackoff > MaxBackoff. The live policy is
	// left untouched.
	ErrInvalidWebhookDeliveryPolicy = errors.New("ledger: webhook: invalid delivery policy: MaxAttempts must be >= 1, durations must be positive, and InitialBackoff must not exceed MaxBackoff")

	// ErrWebhookEndpointNotFound is returned by
	// SetWebhookDeliveryPolicy for an endpoint name the dispatcher
	// has never registered (or has unregistered).
	ErrWebhookEndpointNotFound = errors.New("ledger: webhook: unknown endpoint")

	// ErrWebhooksNotConfigured is returned by
	// SetWebhookDeliveryPolicy when the ledger has no webhook
	// dispatcher attached (WithWebhooks was never used).
	ErrWebhooksNotConfigured = errors.New("ledger: webhooks are not configured on this ledger")
)

// WebhookDeliveryPolicy is a partial revision of one endpoint's four
// delivery knobs. A nil field keeps the live value; a non-nil field
// replaces it. All four nil is a no-op.
type WebhookDeliveryPolicy struct {
	// MaxAttempts bounds the POST attempts per delivery, including
	// the first try. Must be >= 1 when set.
	MaxAttempts *int
	// InitialBackoff is the wait before the first retry; it doubles
	// per retry up to MaxBackoff. Must be positive and at most
	// MaxBackoff when set.
	InitialBackoff *time.Duration
	// MaxBackoff caps the doubling backoff. Must be positive when
	// set.
	MaxBackoff *time.Duration
	// HTTPTimeout bounds a single POST attempt. Must be positive
	// when set; changing it rebuilds the endpoint's http.Client.
	HTTPTimeout *time.Duration
}

// webhookDeliveryTuning is the four knobs as concrete values — the
// merged, validated form. It is comparable, so a no-op revision is a
// plain equality check against the live tuning.
type webhookDeliveryTuning struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	httpTimeout    time.Duration
}

// mergeWebhookDeliveryPolicy overlays a partial revision on the live
// tuning: nil fields keep the live value.
func mergeWebhookDeliveryPolicy(live webhookDeliveryTuning, p WebhookDeliveryPolicy) webhookDeliveryTuning {
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

// validateWebhookDeliveryTuning applies the construction-time gates to
// a merged policy, without the construction-time "zero selects the
// default" fallback or the withDefaults clamp: a runtime revision
// never silently normalizes.
func validateWebhookDeliveryTuning(t webhookDeliveryTuning) error {
	if t.maxAttempts < 1 {
		return fmt.Errorf("%w: MaxAttempts must be >= 1, got %d", ErrInvalidWebhookDeliveryPolicy, t.maxAttempts)
	}
	if t.initialBackoff <= 0 || t.maxBackoff <= 0 || t.httpTimeout <= 0 {
		return fmt.Errorf("%w: durations must be positive (initial_backoff=%s, max_backoff=%s, http_timeout=%s)",
			ErrInvalidWebhookDeliveryPolicy, t.initialBackoff, t.maxBackoff, t.httpTimeout)
	}
	if t.initialBackoff > t.maxBackoff {
		return fmt.Errorf("%w: InitialBackoff (%s) must not exceed MaxBackoff (%s)",
			ErrInvalidWebhookDeliveryPolicy, t.initialBackoff, t.maxBackoff)
	}
	return nil
}

// tuningFromConfig reads the four knobs out of a registered endpoint
// config as a concrete tuning.
func tuningFromConfig(cfg WebhookEndpointConfig) webhookDeliveryTuning {
	return webhookDeliveryTuning{
		maxAttempts:    cfg.MaxAttempts,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		httpTimeout:    cfg.HTTPTimeout,
	}
}

// policyFromTuning renders a concrete tuning as a WebhookDeliveryPolicy
// with every field set, so callers can read the live values back.
func policyFromTuning(t webhookDeliveryTuning) WebhookDeliveryPolicy {
	return WebhookDeliveryPolicy{
		MaxAttempts:    ptrInt(t.maxAttempts),
		InitialBackoff: ptrDuration(t.initialBackoff),
		MaxBackoff:     ptrDuration(t.maxBackoff),
		HTTPTimeout:    ptrDuration(t.httpTimeout),
	}
}

func ptrInt(v int) *int { return &v }

func ptrDuration(v time.Duration) *time.Duration { return &v }

// webhookDeliverySnapshot is the delivery policy one in-flight
// delivery runs under: the four knobs plus the http.Client, snapshotted
// under d.mu.RLock at deliver entry and thrown away after the delivery.
// The client pointer is only ever read through the snapshot, so an
// HTTPTimeout revision that rebuilds e.client can never tear a
// delivery that already started.
type webhookDeliverySnapshot struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	client         *http.Client
}

// deliverySnapshot snapshots the endpoint's live delivery policy.
// The lock is released before the caller POSTs anything — deliver
// still calls appendRecord/updateRecord, which take d.mu themselves.
func (e *webhookEndpoint) deliverySnapshot() webhookDeliverySnapshot {
	e.d.mu.RLock()
	defer e.d.mu.RUnlock()
	return webhookDeliverySnapshot{
		maxAttempts:    e.cfg.MaxAttempts,
		initialBackoff: e.cfg.InitialBackoff,
		maxBackoff:     e.cfg.MaxBackoff,
		client:         e.client,
	}
}

// SetDeliveryPolicy revises one registered endpoint's delivery knobs
// in place and reports the pre-revision policy (for the audit trail).
// The revision is future-only: a delivery already in flight keeps the
// policy it snapshotted at entry; only the next delivery reads the new
// values. An all-nil policy, and a revision identical to the live
// policy, are no-ops that return the live policy with a nil error and
// no side effect. An unknown endpoint returns
// ErrWebhookEndpointNotFound; an illegal merged policy returns
// ErrInvalidWebhookDeliveryPolicy with the live policy untouched.
func (d *WebhookDispatcher) SetDeliveryPolicy(name string, p WebhookDeliveryPolicy) (WebhookDeliveryPolicy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return WebhookDeliveryPolicy{}, ErrWebhookEndpointNotFound
	}
	live := tuningFromConfig(ep.cfg)
	merged := mergeWebhookDeliveryPolicy(live, p)
	if err := validateWebhookDeliveryTuning(merged); err != nil {
		return WebhookDeliveryPolicy{}, err
	}
	if merged == live {
		return policyFromTuning(live), nil
	}
	ep.cfg.MaxAttempts = merged.maxAttempts
	ep.cfg.InitialBackoff = merged.initialBackoff
	ep.cfg.MaxBackoff = merged.maxBackoff
	if merged.httpTimeout != live.httpTimeout {
		// Rebuild the client so the next delivery times out under
		// the new bound; in-flight deliveries hold the old client
		// through their snapshot.
		ep.client = &http.Client{Timeout: merged.httpTimeout}
	}
	ep.cfg.HTTPTimeout = merged.httpTimeout
	return policyFromTuning(live), nil
}

// DeliveryPolicy reports one registered endpoint's live delivery
// policy, with every field set. It returns false when the name is
// unknown.
func (d *WebhookDispatcher) DeliveryPolicy(name string) (WebhookDeliveryPolicy, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return WebhookDeliveryPolicy{}, false
	}
	return policyFromTuning(tuningFromConfig(ep.cfg)), true
}

// SetWebhookDeliveryPolicy revises one registered endpoint's delivery
// knobs in place — see SetDeliveryPolicy. It is audited as
// webhook_delivery_policy_retuned (the endpoint plus previous_*/
// current_* per changed field, durations in nanoseconds). A ledger
// with no dispatcher attached returns ErrWebhooksNotConfigured; an
// unknown endpoint returns ErrWebhookEndpointNotFound; an illegal
// merged policy returns ErrInvalidWebhookDeliveryPolicy with the live
// policy untouched. An all-nil policy, and a revision identical to the
// live policy, are no-ops with no audit event. The revision bumps
// neither the version nor the audit chain, writes no journal entry,
// and emits no outbox event.
func (l *Ledger) SetWebhookDeliveryPolicy(name string, p WebhookDeliveryPolicy) error {
	return l.SetWebhookDeliveryPolicyCtx(context.Background(), name, p)
}

// SetWebhookDeliveryPolicyCtx is SetWebhookDeliveryPolicy with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetWebhookDeliveryPolicyCtx(ctx context.Context, name string, p WebhookDeliveryPolicy) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.webhooks == nil {
		return ErrWebhooksNotConfigured
	}
	previous, err := l.webhooks.SetDeliveryPolicy(name, p)
	if err != nil {
		return err
	}
	// The dispatcher returned the pre-revision policy: merging the
	// request over it reproduces the installed policy, and equality
	// with the previous policy is exactly the no-op case (the
	// dispatcher already skipped the swap and the audit for it).
	prevTuning := webhookDeliveryTuning{
		maxAttempts:    *previous.MaxAttempts,
		initialBackoff: *previous.InitialBackoff,
		maxBackoff:     *previous.MaxBackoff,
		httpTimeout:    *previous.HTTPTimeout,
	}
	merged := mergeWebhookDeliveryPolicy(prevTuning, p)
	if merged == prevTuning {
		return nil
	}
	details := map[string]any{"endpoint": name}
	if merged.maxAttempts != prevTuning.maxAttempts {
		details["previous_max_attempts"] = prevTuning.maxAttempts
		details["current_max_attempts"] = merged.maxAttempts
	}
	if merged.initialBackoff != prevTuning.initialBackoff {
		details["previous_initial_backoff_ns"] = prevTuning.initialBackoff.Nanoseconds()
		details["current_initial_backoff_ns"] = merged.initialBackoff.Nanoseconds()
	}
	if merged.maxBackoff != prevTuning.maxBackoff {
		details["previous_max_backoff_ns"] = prevTuning.maxBackoff.Nanoseconds()
		details["current_max_backoff_ns"] = merged.maxBackoff.Nanoseconds()
	}
	if merged.httpTimeout != prevTuning.httpTimeout {
		details["previous_http_timeout_ns"] = prevTuning.httpTimeout.Nanoseconds()
		details["current_http_timeout_ns"] = merged.httpTimeout.Nanoseconds()
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "webhook_delivery_policy_retuned",
		Actor:         "SetWebhookDeliveryPolicy",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return nil
}

// WebhookDeliveryPolicy reports one registered endpoint's live
// delivery policy, with every field set. It returns false when the
// ledger has no dispatcher attached or the name is unknown.
func (l *Ledger) WebhookDeliveryPolicy(name string) (WebhookDeliveryPolicy, bool) {
	if l.webhooks == nil {
		return WebhookDeliveryPolicy{}, false
	}
	return l.webhooks.DeliveryPolicy(name)
}
