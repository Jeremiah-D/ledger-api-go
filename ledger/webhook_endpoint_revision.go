package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
)

// Outbound webhook endpoint revision (LG-353).
//
// Register fixes an endpoint's delivery target URL and its event
// subscription (Events) for the life of the registration:
// withDefaults and Validate run only at construction, and the
// delivery worker reads the live URL on every delivery while Notify
// matches the live filter on every fan-out. The only way to move a
// live endpoint to a new receiver, or to add or drop a subscribed
// event type, was Unregister + Register, which is a teardown, not a
// revision: Unregister stops the worker and drops queued events, a
// re-registration in between leaves a window in which new events
// match nothing, and the queue and the delivery records restart.
// This module is the orthogonal runtime path: an operator revises
// the target and the subscription in place, instead of rebuilding
// the endpoint.
//
// Semantics, deliberately fail-closed throughout, along LG-304's
// outbound-webhook delivery-policy revision (the same component, the
// same shape) and LG-352's audit-sink URL revision (the same
// snapshot discipline, a single target instead of a registry):
//
//   - The revision is in place: the registry entry, the queue, the
//     worker, the delivery order, and the delivery records all stay
//     exactly as they were. Only the URL and the event filter move —
//     a delivery already queued keeps its place, and the records and
//     counters keep counting.
//   - The race is the real implementation point. post read
//     e.cfg.URL without any lock, while a setter writing e.cfg.URL
//     under d.mu would race those reads — so the URL joins the
//     delivery snapshot (LG-304/LG-352): deliver snapshots it under
//     d.mu.RLock at entry, post takes the snapshotted URL as a
//     parameter, and the snapshot is thrown away after the delivery.
//     A delivery in flight keeps the target it started with —
//     including all its retries — and the revision governs only the
//     next delivery (future-only, along LG-288 / LG-301 / LG-304).
//     The event filter needs no snapshot: matches runs only inside
//     Notify, which holds d.mu.RLock, and the setter writes under
//     d.mu.Lock — the same lock discipline SetEnabled already uses
//     for the enabled gate — so a filter revision can never tear a
//     fan-out. An event already queued was matched at fan-out under
//     the old filter and still delivers; the new filter governs only
//     events fanned out after the revision.
//   - Validation is merge-then-install: the requested partial
//     revision is merged over the live endpoint (nil fields keep the
//     live value) and the merged endpoint reuses the construction-time
//     gates — the URL must be an absolute http(s) URL with a host,
//     along WebhookEndpointConfig.Validate. A runtime revision never
//     silently normalizes (along LG-290 / LG-304): an illegal URL is
//     ErrInvalidWebhookEndpointRevision and the live endpoint stays
//     exactly as it was. The event filter carries no element
//     validation, along Register: Validate checks the name, the URL,
//     the keys, and the knobs, and never inspects Events elements —
//     so neither does this revision. An empty list means all types
//     (the same as a nil list); an element that matches no real
//     event type simply never matches.
//   - An all-nil patch, and a revision identical to the live
//     endpoint on both fields, are no-ops: they return the live
//     config with a nil error and write no audit event, so a retried
//     or duplicated revision cannot spam the audit trail.
//   - An unknown endpoint returns ErrWebhookEndpointNotFound (a
//     sentinel, along RotateKey's unknown-endpoint shape). A ledger
//     with no dispatcher attached (WithWebhooks was never used)
//     returns ErrWebhooksNotConfigured (along LG-301's
//     ErrAuditLogNotConfigured).
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     webhook_endpoint_revised audit event (the endpoint plus
//     previous_url/current_url and previous_events/current_events per
//     changed field — URLs and event-type lists only, never key
//     material). The Ctx variant attributes the operator (LG-73).
//     Lock order is l.mu then d.mu, the order emitOutboxLocked
//     already establishes when it calls Notify under the ledger
//     write lock.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     endpoint retargeting is in-process structural config.
//   - Snapshots do not carry the revision: the dispatcher config
//     comes only from the startup-time LEDGER_WEBHOOKS parse, so a
//     restart restores the env values while the amendment events
//     themselves stay in the audit trail for traceability.
//   - Tenancy: WithWebhooks hands the same dispatcher to lazily
//     provisioned tenant ledgers, so a revision affects every tenant
//     sharing that dispatcher.
//   - Explicitly out of scope: the four delivery knobs (LG-304 owns
//     them), Keys (RotateKey owns them), Enabled (SetEnabled owns
//     it), and QueueSize (LG-343 owns it).
//
// Orthogonality: LG-102 owns the webhook mechanism itself (registry,
// retries, queries), LG-67 owns the audit sink's external forwarding
// (a different sink component with its own retries — the sink's own
// URL revision is LG-352), LG-304 owns the four delivery knobs (a
// different axis of the same endpoint), Register/Unregister own
// endpoint lifecycle, RotateKey owns key material, SetEnabled owns
// the delivery gate, and LG-343 owns the queue capacity. This module
// owns only the runtime revision of an already-registered endpoint's
// target and subscription.

var (
	// ErrInvalidWebhookEndpointRevision is returned by
	// ReviseWebhookEndpoint when the merged endpoint fails the
	// construction-time gates: the URL must be an absolute http(s)
	// URL with a host. The live endpoint is left untouched.
	ErrInvalidWebhookEndpointRevision = errors.New("ledger: webhook: invalid endpoint revision: URL must be an absolute http(s) URL")
)

// WebhookEndpointRevision is a partial revision of one endpoint's
// delivery target and event subscription. A nil field keeps the live
// value; a non-nil field replaces it. All nil is a no-op. A nil
// Events list and an empty one mean the same thing (all event types),
// so revising Events to an empty list opens the endpoint to every
// type.
type WebhookEndpointRevision struct {
	// URL is the webhook receiver. Must be an absolute http(s) URL
	// when set.
	URL *string
	// Events filters which outbox event types are forwarded (exact
	// match on the event type). Empty means all types when set.
	Events *[]string
}

// validWebhookEndpointURL applies the construction-time URL gate
// (WebhookEndpointConfig.Validate) to a candidate target.
func validWebhookEndpointURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: URL is required", ErrInvalidWebhookEndpointRevision)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: invalid URL %q", ErrInvalidWebhookEndpointRevision, raw)
	}
	return nil
}

// endpointConfigCopy copies an endpoint config with its own Events
// slice, so the caller can neither mutate the live filter through
// the copy nor observe a later revision through it. Keys stays a
// shared pointer: the ring guards itself with its own mutex.
func endpointConfigCopy(cfg WebhookEndpointConfig) WebhookEndpointConfig {
	cfg.Events = slices.Clone(cfg.Events)
	return cfg
}

// ReviseWebhookEndpoint revises one registered endpoint's delivery
// target URL and event subscription in place and reports the
// pre-revision config (for the audit trail). The revision is
// future-only: a delivery already in flight keeps the target it
// snapshotted at entry, and an event already queued was matched at
// fan-out under the old filter; only the next delivery and the next
// fan-out read the new values. An all-nil patch, and a revision
// identical to the live endpoint, are no-ops that return the live
// config with a nil error and no side effect. An unknown endpoint
// returns ErrWebhookEndpointNotFound; an illegal URL returns
// ErrInvalidWebhookEndpointRevision with the live endpoint untouched.
func (d *WebhookDispatcher) ReviseWebhookEndpoint(name string, patch WebhookEndpointRevision) (WebhookEndpointConfig, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return WebhookEndpointConfig{}, ErrWebhookEndpointNotFound
	}
	previous := endpointConfigCopy(ep.cfg)
	merged := previous
	if patch.URL != nil {
		if err := validWebhookEndpointURL(*patch.URL); err != nil {
			return WebhookEndpointConfig{}, err
		}
		merged.URL = *patch.URL
	}
	if patch.Events != nil {
		merged.Events = slices.Clone(*patch.Events)
	}
	if merged.URL == previous.URL && slices.Equal(merged.Events, previous.Events) {
		return previous, nil
	}
	ep.cfg.URL = merged.URL
	ep.cfg.Events = slices.Clone(merged.Events)
	return previous, nil
}

// EndpointConfig reports one registered endpoint's live config, with
// its own Events slice. It returns false when the name is unknown.
func (d *WebhookDispatcher) EndpointConfig(name string) (WebhookEndpointConfig, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return WebhookEndpointConfig{}, false
	}
	return endpointConfigCopy(ep.cfg), true
}

// ReviseWebhookEndpoint revises one registered endpoint's delivery
// target URL and event subscription in place — see
// WebhookDispatcher.ReviseWebhookEndpoint. It is audited as
// webhook_endpoint_revised (the endpoint plus previous_url/
// current_url and previous_events/current_events per changed field —
// URLs and event-type lists only, never key material). A ledger with
// no dispatcher attached returns ErrWebhooksNotConfigured; an unknown
// endpoint returns ErrWebhookEndpointNotFound; an illegal URL returns
// ErrInvalidWebhookEndpointRevision with the live endpoint untouched.
// An all-nil patch, and a revision identical to the live endpoint,
// are no-ops with no audit event. The revision bumps neither the
// version nor the audit chain, writes no journal entry, and emits no
// outbox event.
func (l *Ledger) ReviseWebhookEndpoint(name string, patch WebhookEndpointRevision) error {
	return l.ReviseWebhookEndpointCtx(context.Background(), name, patch)
}

// ReviseWebhookEndpointCtx is ReviseWebhookEndpoint with per-call
// audit actor attribution (LG-73).
func (l *Ledger) ReviseWebhookEndpointCtx(ctx context.Context, name string, patch WebhookEndpointRevision) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.webhooks == nil {
		return ErrWebhooksNotConfigured
	}
	previous, err := l.webhooks.ReviseWebhookEndpoint(name, patch)
	if err != nil {
		return err
	}
	// The dispatcher returned the pre-revision config: merging the
	// patch over it reproduces the installed endpoint, and equality
	// with the previous config is exactly the no-op case (the
	// dispatcher already skipped the swap and the audit for it).
	mergedURL := previous.URL
	if patch.URL != nil {
		mergedURL = *patch.URL
	}
	mergedEvents := previous.Events
	if patch.Events != nil {
		mergedEvents = *patch.Events
	}
	if mergedURL == previous.URL && slices.Equal(mergedEvents, previous.Events) {
		return nil
	}
	details := map[string]any{"endpoint": name}
	if mergedURL != previous.URL {
		details["previous_url"] = previous.URL
		details["current_url"] = mergedURL
	}
	if !slices.Equal(mergedEvents, previous.Events) {
		details["previous_events"] = slices.Clone(previous.Events)
		details["current_events"] = slices.Clone(mergedEvents)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "webhook_endpoint_revised",
		Actor:         "ReviseWebhookEndpoint",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return nil
}

// WebhookEndpoint reports one registered endpoint's live config, with
// its own Events slice. It returns false when the ledger has no
// dispatcher attached or the name is unknown.
func (l *Ledger) WebhookEndpoint(name string) (WebhookEndpointConfig, bool) {
	if l.webhooks == nil {
		return WebhookEndpointConfig{}, false
	}
	return l.webhooks.EndpointConfig(name)
}
