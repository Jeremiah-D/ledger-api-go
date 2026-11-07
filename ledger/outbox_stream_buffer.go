package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Outbox live-tail slow-consumer queue capacity runtime revision (LG-346).
//
// The slow-consumer drop threshold is the construction-time constant
// outboxStreamBuffer (512, ledger/outbox.go): a burst of batched
// postings or a bulk import can fill a downstream indexer's 512-deep
// live queue faster than it drains, and the outbox drops the
// subscription — counted as a slow drop, and the consumer must replay
// from disk. An operator who wants to widen the buffer to 4096 to ride
// out a burst, or tighten it to 64 to bound per-subscription memory,
// had no runtime path: the only way was to change the code and
// redeploy. This module is the orthogonal runtime path: an operator
// revises the live per-subscriber queue capacity in place, instead of
// restarting the server.
//
// Semantics, deliberately fail-closed throughout, along the LG-309
// positive-only capacity revision (audit-log queue) and the LG-343
// (previous, error) return shape (webhook endpoint queue):
//
//   - The capacity is a live field, not a channel generation swap:
//     unlike the LG-309/LG-343 queues, the outbox subscription
//     channel is built per Subscribe call, so the revision only has
//     to move the number the next Subscribe reads. A subscription
//     already open keeps the capacity its channel was built with
//     (a channel's capacity is fixed at make time) — the revision
//     governs only subscriptions opened after it. Nothing is
//     drained, reordered, or re-created; in-flight deliveries are
//     untouched.
//   - The revision is future-only and bumps neither the version nor
//     the audit chain, writes no journal entry, and emits no outbox
//     event (the revision tunes the outbox's own governance
//     parameter; the audit event goes only to the audit chain, along
//     LG-344). It lands one outbox_stream_buffer_size_retuned audit
//     event (previous_buffer_size / current_buffer_size, purely
//     numeric — no event content). The Ctx variant attributes the
//     operator (LG-73); with no operator in ctx the actor defaults
//     to DefaultAuditActor ("system").
//   - Validation is fail-closed with no silent normalization: the
//     capacity must be positive, else
//     ErrInvalidOutboxStreamBufferSize and the live capacity stays
//     exactly as it was. Zero is rejected, not a "close the queue"
//     state: a zero-capacity channel would drop every live
//     subscriber on the very next Emit (deliverLocked's default
//     branch), so it is not a meaningful capacity. There is no upper
//     bound: the memory accounting is per-subscription buffer
//     capacity times event size, and the operator owns that
//     trade-off (documented in the README, along LG-309).
//   - A ledger with no outbox attached (WithOutbox was never used)
//     rejects the revision with ErrOutboxNotConfigured — the
//     mutation form of OutboxSubscribe's ok=false. The getter
//     reports (0, false) in that case, along OutboxSlowDrops.
//   - A revision identical to the live capacity is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - Lock order: the setter never takes o.mu (Emit/deliverLocked
//     hold o.mu, so a setter that took it would be serialized behind
//     the commit path — along LG-344, a live value must never reuse
//     the worker's lock). Subscribe holds o.mu while taking
//     streamBufferSizeMu.RLock to read the live capacity, so the
//     order is o.mu -> streamBufferSizeMu; the setter takes only
//     streamBufferSizeMu, releases it, and only then takes l.mu.RLock
//     to emit the audit event (along LG-344) — the three locks are
//     never held nested, so the order is acyclic. A revision can
//     never stall a poster: Emit never waits on the setter.
//   - Snapshots do not carry the capacity: the outbox is built by
//     NewOutbox, which always seeds the construction-time default
//     512, so a restart restores 512 while the revision events
//     themselves stay in the audit trail for traceability.
//   - The webhook dispatcher shares the same Subscribe path, so a
//     revision also moves the buffering depth of the dispatcher's
//     live tail — its delivery rhythm and retry policy are
//     untouched. The heartbeat interval (LG-345), the disconnect
//     policy (full queue drops the subscriber — unchanged, only the
//     threshold moves), the SlowDrops counting, resume, filters,
//     auth, and the 404 fail-closed behaviour are all untouched.
//
// Orthogonality: LG-133 owns the outbox live-tail mechanism itself
// (this module is its missing capacity-revision entry point — the
// LG-345 draft explicitly excluded "the 512-event slow-consumer
// queue" and deferred it here); LG-345 owns the same stream's
// heartbeat interval (the keep-alive rhythm — this module owns the
// slow-consumer queue depth); LG-343 owns the webhook endpoint
// delivery-queue capacity (a per-endpoint POST buffer — this module
// owns the outbox live-stream subscription queue, no endpoint
// involved; the same (previous, error) return shape, a different
// object); LG-336 owns the webhook delivery-record ring (a
// look-back store — this module owns live-stream backpressure);
// LG-309 owns the audit-log queue capacity (the same positive-only
// capacity-revision shape, a different queue); LG-314 owns the
// audit-sink poll interval (a background cadence — this module owns
// a foreground stream queue); LG-102 owns the webhook mechanism
// itself. There is no HTTP endpoint for the revision (per LG-241/242
// and the LG-329/344 family): outbox tuning is in-process structural
// config, and the operator path is the ledger handle. There is no
// new env knob and no per-tenant capacity.

var (
	// ErrOutboxNotConfigured is returned by
	// SetOutboxStreamBufferSize when no outbox is attached to the
	// ledger (WithOutbox was never used). There is no live capacity
	// to revise, so the returned value is 0.
	ErrOutboxNotConfigured = errors.New("ledger: outbox is not configured")

	// ErrInvalidOutboxStreamBufferSize is returned by
	// SetOutboxStreamBufferSize when the requested capacity is not
	// positive. The live capacity is left untouched (the returned
	// value is the live capacity in that case).
	ErrInvalidOutboxStreamBufferSize = errors.New("ledger: outbox stream buffer size must be positive")
)

// StreamBufferSize reports the outbox's live per-subscriber
// live-event queue depth, read under streamBufferSizeMu. It is the
// runtime counterpart of the construction-time outboxStreamBuffer
// default (512): the capacity the next Subscribe builds its channel
// with.
func (o *Outbox) StreamBufferSize() int {
	o.streamBufferSizeMu.RLock()
	defer o.streamBufferSizeMu.RUnlock()
	return o.streamBufferSize
}

// setStreamBufferSize installs a new per-subscriber queue capacity
// and reports the previous value and whether it changed. Callers
// must validate n >= 1 first; the swap itself is unconditional once
// past the same-value check, so a rejected value leaves the live
// capacity untouched. It takes only streamBufferSizeMu — never o.mu
// — so a revision is never serialized behind Emit/deliverLocked,
// and it is never called with l.mu held, so the audit section that
// follows it cannot invert with the commit path.
func (o *Outbox) setStreamBufferSize(n int) (previous int, changed bool) {
	o.streamBufferSizeMu.Lock()
	defer o.streamBufferSizeMu.Unlock()
	previous = o.streamBufferSize
	if previous == n {
		return previous, false
	}
	o.streamBufferSize = n
	return previous, true
}

// SetOutboxStreamBufferSize revises the outbox's per-subscriber
// live-event queue capacity in place and returns the pre-revision
// capacity (for the audit trail). A larger capacity takes effect
// from the next Subscribe: a burst that would have dropped a slow
// consumer at 512 is absorbed up to the new depth. A smaller
// capacity takes effect the same way — subscriptions already open
// keep the capacity their channel was built with, and only new
// subscriptions are built at the smaller depth, so a shrink never
// drops a buffered event. A non-positive capacity is rejected with
// ErrInvalidOutboxStreamBufferSize and the live capacity is left
// untouched (the returned value is the live capacity in that case
// too); a ledger with no outbox attached returns
// ErrOutboxNotConfigured (and 0). Revising to the current capacity
// is a no-op with no audit event. The revision is audited as
// outbox_stream_buffer_size_retuned. A restart restores the 512
// default.
func (l *Ledger) SetOutboxStreamBufferSize(n int) (int, error) {
	return l.SetOutboxStreamBufferSizeCtx(context.Background(), n)
}

// SetOutboxStreamBufferSizeCtx is SetOutboxStreamBufferSize with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetOutboxStreamBufferSizeCtx(ctx context.Context, n int) (int, error) {
	// Check order, along LG-344: the outbox gate first — without an
	// outbox there is no live value to return — then the capacity
	// gate, then the same-value comparison, then the audit.
	if l.outbox == nil {
		return 0, ErrOutboxNotConfigured
	}
	if n <= 0 {
		return l.outbox.StreamBufferSize(), fmt.Errorf("%w: got %d", ErrInvalidOutboxStreamBufferSize, n)
	}
	previous, changed := l.outbox.setStreamBufferSize(n)
	if !changed {
		return previous, nil
	}
	// Audit after the swap, under l.mu.RLock like LG-344:
	// streamBufferSizeMu and l.mu are never held nested, so the
	// lock order is acyclic. emitAuditCtx is a no-op when no audit
	// log is attached, and the event goes only to the audit log —
	// never to the outbox. The details carry only the two integer
	// capacities: no event content ever rides in a retune event.
	l.mu.RLock()
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "outbox_stream_buffer_size_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_buffer_size": previous,
			"current_buffer_size":  n,
		},
	})
	l.mu.RUnlock()
	return previous, nil
}

// OutboxStreamBufferSize reports the outbox's live per-subscriber
// live-event queue depth. ok is false when no outbox is attached
// (WithOutbox was never used): in that case the capacity is zero
// and meaningless. Follows the OutboxSlowDrops enabled shape.
func (l *Ledger) OutboxStreamBufferSize() (int, bool) {
	if l.outbox == nil {
		return 0, false
	}
	return l.outbox.StreamBufferSize(), true
}
