package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Outbound webhook delivery-record ring capacity runtime revision
// (LG-336).
//
// NewWebhookDispatcher fixes the delivery-record ring capacity — how
// many of the newest delivery records the dispatcher keeps for GET
// /webhooks/deliveries — at DefaultWebhookMaxRecords (1024) for the
// life of the dispatcher: the constructor assembles it from the
// default with no env knob and no construction parameter, and the
// only live reader is appendRecord, which trims from the head when
// the ring overflows. The only way to widen the observation window
// during an event storm or an incident investigation (1024 records
// roll off a high-frequency endpoint within minutes, so the query
// surface only ever shows the most recent slice) or tighten it when
// memory is scarce was to change code and redeploy. This module is
// the orthogonal runtime path: an operator revises the live ring
// capacity in place, instead of rebuilding the dispatcher.
//
// Semantics, deliberately fail-closed throughout, along the LG-304 /
// LG-335 structural revisions:
//
//   - The revision is in place: the registry, the queues, the
//     workers, the delivery order, and the counters stay exactly as
//     they were. Only the ring capacity moves — a delivery already
//     queued keeps its place, and the records and counters keep
//     counting.
//   - Validation is fail-closed with no silent normalization: the
//     new capacity must be >= 1, else ErrInvalidWebhookMaxRecords
//     and the live capacity stays exactly as it was. Zero is
//     rejected too — it would make the ring permanently empty, which
//     is equivalent to switching the query surface off, and a
//     runtime revision never silently disables a surface (along
//     LG-314).
//   - A revision identical to the live capacity is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - Tightening trims the oldest records immediately, down to the
//     new capacity, under the same d.mu that guards the ring — the
//     next Deliveries query already sees the new capacity. Records
//     in flight are always among the newest (per-endpoint deliveries
//     are serial, so an in-flight record can never be evicted
//     mid-delivery), so the trim only ever touches the oldest end
//     and an in-flight record keeps its appendRecord handle for a
//     later updateRecord. Widening takes effect from the next
//     append: records that already rolled off are gone, the ring
//     just stops trimming until it reaches the new capacity.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     webhook_max_records_retuned audit event (previous_max_records
//     / current_max_records, purely numeric). The Ctx variant
//     attributes the operator (LG-73). Lock order is l.mu then
//     d.mu, the order LG-304 establishes.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     GET /webhooks/deliveries stays the only query surface.
//   - Snapshots do not carry the capacity: the dispatcher config
//     comes only from the startup-time LEDGER_WEBHOOKS parse, so a
//     restart restores the default 1024 while the revision events
//     themselves stay in the audit trail for traceability.
//   - Tenancy: WithWebhooks hands the same dispatcher to lazily
//     provisioned tenant ledgers, so a revision affects every tenant
//     sharing that dispatcher.
//
// Orthogonality: LG-102 owns the webhook mechanism itself (this
// module is its missing ring-capacity revision entry point),
// LG-304 owns the four delivery knobs (retries / backoff / timeout
// — this module owns the record-retention capacity), LG-335 owns the
// signing-key grace window (how long a retired key verifies — this
// module owns how deep the delivery history reads back), LG-306
// owns the audit-sink delivery policy (a different dispatcher),
// LG-309 owns the audit-log queue capacity (the audit-event queue —
// this module owns the webhook delivery-record ring, a different
// ring), and LG-301 owns the audit-log maxBytes (file rotation —
// this module owns a record-count bound).
//
// Explicitly out of scope: QueueSize (the channel capacity —
// construction-time only, along LG-304), URL / Events / Enabled /
// Keys (RotateKey owns keys), the four delivery knobs (LG-304),
// per-endpoint record capacities (the ring is shared), a new HTTP
// endpoint, stopping the dispatcher through a revision (Stop /
// Unregister own that), the Deliveries query limit semantics (1–100,
// default 50), and persisting the capacity across restarts.

var (
	// ErrInvalidWebhookMaxRecords is returned by
	// SetWebhookMaxRecords when the requested capacity is less than
	// 1. The live capacity is left untouched.
	ErrInvalidWebhookMaxRecords = errors.New("ledger: webhook: invalid max records: max records must be >= 1")
)

// SetMaxRecords revises the delivery-record ring capacity in place
// and returns the pre-revision capacity (for the audit trail). A
// capacity below 1 returns ErrInvalidWebhookMaxRecords with the live
// capacity untouched (the returned value is the live capacity in
// that case too). A revision identical to the live capacity is a
// no-op that returns the live value with a nil error and no side
// effect. Tightening trims the oldest records immediately to the new
// capacity; widening takes effect from the next append. Records in
// flight are always among the newest and are never trimmed.
func (d *WebhookDispatcher) SetMaxRecords(n int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n < 1 {
		return d.maxRecords, fmt.Errorf("%w: got %d", ErrInvalidWebhookMaxRecords, n)
	}
	previous := d.maxRecords
	if n == previous {
		return previous, nil
	}
	d.maxRecords = n
	if len(d.records) > n {
		// Keep the newest n records, in order — the same
		// copy+reslice shape appendRecord uses to drop the
		// oldest entry.
		copy(d.records, d.records[len(d.records)-n:])
		d.records = d.records[:n]
	}
	return previous, nil
}

// MaxRecords reports the live delivery-record ring capacity.
func (d *WebhookDispatcher) MaxRecords() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.maxRecords
}

// SetWebhookMaxRecords revises the delivery-record ring capacity in
// place — see SetMaxRecords. It returns the previous capacity. It is
// audited as webhook_max_records_retuned (previous_max_records /
// current_max_records, purely numeric). A ledger with no dispatcher
// attached returns ErrWebhooksNotConfigured; a capacity below 1
// returns ErrInvalidWebhookMaxRecords with the live capacity
// untouched. A revision identical to the live capacity is a no-op
// with no audit event. The revision bumps neither the version nor
// the audit chain, writes no journal entry, and emits no outbox
// event. Restarting restores the default 1024.
func (l *Ledger) SetWebhookMaxRecords(n int) (int, error) {
	return l.SetWebhookMaxRecordsCtx(context.Background(), n)
}

// SetWebhookMaxRecordsCtx is SetWebhookMaxRecords with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetWebhookMaxRecordsCtx(ctx context.Context, n int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.webhooks == nil {
		return 0, ErrWebhooksNotConfigured
	}
	previous, err := l.webhooks.SetMaxRecords(n)
	if err != nil {
		return previous, err
	}
	if n == previous {
		return previous, nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "webhook_max_records_retuned",
		Actor:         "SetWebhookMaxRecords",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_records": previous,
			"current_max_records":  n,
		},
	})
	return previous, nil
}

// WebhookMaxRecords reports the live delivery-record ring capacity.
// It returns false when the ledger has no dispatcher attached.
func (l *Ledger) WebhookMaxRecords() (int, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.webhooks == nil {
		return 0, false
	}
	return l.webhooks.MaxRecords(), true
}
