package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Outbound webhook endpoint queue-capacity runtime revision (LG-343).
//
// Register fixes an endpoint's delivery-queue capacity —
// WebhookEndpointConfig.QueueSize, DefaultWebhookQueueSize (1024) —
// for the life of the registration: withDefaults and Validate run
// only at construction, and the channel capacity is frozen at make
// time. A burst that fills the queue drops live fan-out events
// (counted, but the real-time callback is gone — the outbox stream
// stays the durable record, so the event can be replayed later, but
// the live push is lost), and the only way to retune it was
// Unregister + Register: a teardown that stops the worker, drops the
// queued events per stop semantics, and leaves a window in which new
// events match nothing. This module is the orthogonal runtime path:
// an operator revises the live queue capacity in place, instead of
// rebuilding the endpoint.
//
// Semantics, deliberately fail-closed throughout, along the LG-309
// generation swap (audit-log queue) and the LG-336 return shape
// (delivery-record ring):
//
//   - The queue itself is a channel, so unlike maxRecords it cannot
//     be swapped by writing a field: the worker reads its generation
//     in run, and Notify sends into it. The swap is therefore owned
//     by the worker (applyQueueResize): a revision travels to the
//     worker as a queueResizeRequest — the same request type LG-309
//     uses — and the worker applies it at a select point where it
//     holds no received event. The old generation is drained into
//     pending under queueMu — Notify sends under queueMu.RLock, so no
//     event can enter the old channel mid-drain and no drained event
//     can be overtaken by a new one — then the new channel is
//     installed and the setter is released. The worker flushes
//     pending, oldest first, before taking new events from the
//     channel, so a shrink loses nothing and per-endpoint delivery
//     order still matches outbox seq order.
//   - While pending is non-empty the total depth (len(pending) +
//     len(queue)) can exceed the new capacity, and new events are
//     dropped by plain channel semantics whenever the (smaller) new
//     channel is full, until the worker works the backlog down. The
//     total queue depth is unbounded only by the old generation's
//     buffered events — they were already accepted, so nothing new
//     is admitted beyond the new capacity. Notify never blocks: a
//     revision can never stall a poster.
//   - Lock order is the red line, and it is enforced structurally:
//     the swap happens only in the worker goroutine and calls
//     nothing that takes d.mu (applyQueueResize takes queueMu only);
//     Notify's send reads the current generation and sends under
//     the same queueMu.RLock (tryEnqueue — the AuditLog.queueChan
//     shape), never by holding d.mu and reading the bare queue
//     field; the worker tracks generations locally in run and never
//     re-reads the field. The only lock chain is l.mu -> d.mu ->
//     queueMu (Notify), and d.mu -> queueMu (the revision lookup),
//     with the worker taking queueMu alone — no inversion, and the
//     -race concurrency test pins it.
//   - Validation reuses the construction-time gate, without the
//     "keep the default" fallback — at runtime there is no default
//     to fall back to. The capacity must be a positive number of
//     events: n < 1 is ErrInvalidWebhookQueueSize and the live
//     capacity is left untouched. The check order is the capacity
//     gate first, then the dispatcher (ErrWebhooksNotConfigured),
//     then the endpoint (ErrWebhookEndpointNotFound).
//   - A revision identical to the live capacity is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - A stopped endpoint takes the inline path (the LG-309 closed
//     path): the worker has exited, so the setter installs the new
//     channel itself under queueMu instead of waiting on a
//     req.done that will never close — a revision after Stop never
//     deadlocks. The install skips the drain on purpose: the worker
//     already drained and counted its generation in shutdownDrain
//     (or it was empty when it returned on the closed queue), and
//     draining here would only move events to a pending no one
//     flushes. A revision racing the worker's exit re-checks
//     req.done before installing, so an applied swap is never
//     installed twice and no notified event can strand in an
//     abandoned generation.
//   - The revision is future-only: a delivery already in flight
//     keeps the deliverySnapshot it started with — the queue size
//     is not in the snapshot — and only the next delivery reads the
//     new capacity. It bumps neither the version nor the audit
//     chain, writes no journal entry, and emits no outbox event.
//     It lands one webhook_queue_size_retuned audit event (the
//     endpoint plus previous_queue_size / current_queue_size,
//     purely numeric). The Ctx variant attributes the operator
//     (LG-73).
//   - Snapshots do not carry the capacity: the dispatcher config
//     comes only from the startup-time LEDGER_WEBHOOKS parse, so a
//     restart restores the construction-time value while the
//     revision events themselves stay in the audit trail for
//     traceability.
//
// Orthogonality: LG-304 owns the four delivery knobs (retries /
// backoff / timeout — this module owns the admission buffer in
// front of them), LG-335 owns the signing-key grace window, LG-336
// owns the delivery-record ring (how deep the history reads back —
// this module owns how deep the live fan-out buffers), LG-309 owns
// the audit-log queue (the same generation-swap pattern, a
// different component), and LG-102 owns the webhook mechanism
// itself. There is no HTTP endpoint for the revision (per
// LG-241/242): queue tuning is in-process structural config.
//
// Explicitly out of scope: the four knobs, the record ring, the
// signing keys, the event filter, per-event priority queueing, the
// run() serial delivery order, the deliver() retry/backoff
// semantics, Unregister + Register as a revision path, a new HTTP
// endpoint, carrying the capacity in snapshots, and stopping an
// endpoint through a revision (Stop / Unregister own that).

var (
	// ErrInvalidWebhookQueueSize is returned by
	// SetWebhookQueueSize when the requested capacity is less than
	// 1. The live capacity is left untouched.
	ErrInvalidWebhookQueueSize = errors.New("ledger: webhook: invalid queue size: queue size must be >= 1")
)

// queueChan reads the current queue generation under queueMu.RLock.
// The worker calls it once at startup; afterwards it tracks
// generations through applyQueueResize, never by re-reading the
// field mid-generation.
func (e *webhookEndpoint) queueChan() chan OutboxEvent {
	e.queueMu.RLock()
	defer e.queueMu.RUnlock()
	return e.queue
}

// tryEnqueue is Notify's single generational read point: it reads
// the current queue generation and sends under the same
// queueMu.RLock, so a capacity revision draining the old generation
// (which holds queueMu) cannot interleave with the send. It reports
// whether the event was accepted; a full queue reports false and
// the caller counts the drop. It never blocks.
func (e *webhookEndpoint) tryEnqueue(ev OutboxEvent) bool {
	e.queueMu.RLock()
	defer e.queueMu.RUnlock()
	select {
	case e.queue <- ev:
		return true
	default:
		return false
	}
}

// QueueSize reports the endpoint's live queue capacity, read under
// queueMu. It is the runtime counterpart of the construction-time
// WebhookEndpointConfig.QueueSize value.
func (e *webhookEndpoint) QueueSize() int {
	e.queueMu.RLock()
	defer e.queueMu.RUnlock()
	return e.queueSize
}

// takePending pops the oldest event drained out of a previous queue
// generation during a shrink, if any. The worker flushes pending
// before taking new events from the channel, so shrink survivors
// keep their place in line.
func (e *webhookEndpoint) takePending() (OutboxEvent, bool) {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	if len(e.pending) == 0 {
		return OutboxEvent{}, false
	}
	ev := e.pending[0]
	e.pending[0] = OutboxEvent{} // avoid retaining the delivered entry
	e.pending = e.pending[1:]
	if len(e.pending) == 0 {
		e.pending = nil // release the backing array between revisions
	}
	return ev, true
}

// dropPending discards every shrink survivor still buffered —
// shutdown only — and reports how many there were, so the worker
// can count them as dropped along with the queued generation.
func (e *webhookEndpoint) dropPending() int {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	n := len(e.pending)
	e.pending = nil
	return n
}

// drainQueueLocked moves every event still buffered in ch into
// pending, preserving order. Callers must hold queueMu.Lock: Notify
// sends under queueMu.RLock, so no event can enter the channel
// mid-drain, and the worker is the only other consumer — and it is
// the caller here (applyQueueResize). The closed-channel check
// matters: Unregister closes the queue, and a receive on a closed,
// drained channel returns a zero value immediately — appending it
// would spin forever.
func (e *webhookEndpoint) drainQueueLocked(ch chan OutboxEvent) {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			e.pending = append(e.pending, ev)
		default:
			return
		}
	}
}

// applyQueueResize installs a new queue channel of capacity req.n
// and reports it to the worker. It runs in the worker goroutine, at
// a select point where the worker holds no received event and takes
// no d.mu: the old generation is drained into pending under queueMu
// (Notify is excluded for the duration, so nothing can enter the
// old channel mid-drain and no drained event can be overtaken by a
// new one), then the new channel is installed and the setter is
// released. The drained events are flushed by the worker's
// takePending path, in order, before any event taken from the new
// channel — a shrink therefore loses nothing, and while the backlog
// drains, new events are dropped by plain channel semantics
// whenever the (smaller) new channel is full.
func (e *webhookEndpoint) applyQueueResize(req queueResizeRequest, q chan OutboxEvent) chan OutboxEvent {
	e.queueMu.Lock()
	e.drainQueueLocked(q)
	next := make(chan OutboxEvent, req.n)
	e.queue = next
	e.queueSize = req.n
	e.queueMu.Unlock()
	close(req.done)
	return next
}

// installQueueSizeLocked installs a new queue channel of capacity n
// under queueMu. Callers must hold queueMu.Lock. This is the
// stopped-endpoint path only — the worker has exited, so there is no
// generation to drain and no writer to hand off to (see
// setQueueSize).
func (e *webhookEndpoint) installQueueSizeLocked(n int) {
	e.queue = make(chan OutboxEvent, n)
	e.queueSize = n
}

// setQueueSize installs a new queue capacity and reports the
// previous value and whether it changed. Callers must validate
// n >= 1 first; the swap itself is unconditional once past the
// same-value check, so a rejected value leaves the live capacity
// untouched. A stopped endpoint takes the inline path (the worker
// has exited and req.done would never close); a revision racing
// the worker's exit re-checks req.done before installing, so an
// applied swap is never installed twice. resizeReq is buffered 1
// and resizeMu serializes setters, so the send itself never blocks
// — except when a stale request from a revision that raced a stop
// still sits in the buffer, in which case the send waits on done
// (Unregister closes done right after closing the queue) instead of
// deadlocking.
func (e *webhookEndpoint) setQueueSize(n int) (previous int, changed bool) {
	e.resizeMu.Lock()
	defer e.resizeMu.Unlock()
	e.queueMu.RLock()
	previous = e.queueSize
	e.queueMu.RUnlock()
	if previous == n {
		return previous, false
	}
	select {
	case <-e.done:
		e.queueMu.Lock()
		e.installQueueSizeLocked(n)
		e.queueMu.Unlock()
		return previous, true
	default:
	}
	req := queueResizeRequest{n: n, done: make(chan struct{})}
	select {
	case e.resizeReq <- req:
	case <-e.done:
		e.queueMu.Lock()
		e.installQueueSizeLocked(n)
		e.queueMu.Unlock()
		return previous, true
	}
	select {
	case <-req.done:
		// The worker applied the swap.
	case <-e.done:
		// The worker exited mid-revision: install inline, but
		// only if it did not apply in the meantime — installing
		// over an applied swap would strand the generation the
		// worker is reading.
		select {
		case <-req.done:
		default:
			e.queueMu.Lock()
			e.installQueueSizeLocked(n)
			e.queueMu.Unlock()
		}
	}
	return previous, true
}

// closeQueue closes the current queue generation under queueMu, so
// no Notify send can race the close (a send on a closed channel
// panics). Callers must hold no endpoint lock; Unregister is the
// only caller, and it has already removed the endpoint from the
// registry, so the generation cannot be closed twice.
func (e *webhookEndpoint) closeQueue() {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	close(e.queue)
}

// SetQueueSize revises one registered endpoint's delivery-queue
// capacity in place and returns the pre-revision capacity (for the
// audit trail). A capacity below 1 returns
// ErrInvalidWebhookQueueSize with the live capacity untouched (the
// returned value is the live capacity in that case too). A revision
// identical to the live capacity is a no-op that returns the live
// value with a nil error and no side effect. An unknown endpoint
// returns ErrWebhookEndpointNotFound. Widening takes effect from
// the next Notify; tightening never drops a buffered event —
// survivors drain in order — while new events are dropped by plain
// channel semantics whenever the smaller channel is full, until the
// backlog drains.
func (d *WebhookDispatcher) SetQueueSize(name string, n int) (int, error) {
	d.mu.RLock()
	ep, ok := d.endpoints[name]
	d.mu.RUnlock()
	if !ok {
		return 0, ErrWebhookEndpointNotFound
	}
	if n < 1 {
		return ep.QueueSize(), fmt.Errorf("%w: got %d", ErrInvalidWebhookQueueSize, n)
	}
	previous, _ := ep.setQueueSize(n)
	return previous, nil
}

// QueueSize reports one registered endpoint's live queue capacity.
// It returns false when the name is unknown.
func (d *WebhookDispatcher) QueueSize(name string) (int, bool) {
	d.mu.RLock()
	ep, ok := d.endpoints[name]
	d.mu.RUnlock()
	if !ok {
		return 0, false
	}
	return ep.QueueSize(), true
}

// SetWebhookQueueSize revises one registered endpoint's
// delivery-queue capacity in place — see SetQueueSize. It returns
// the previous capacity. It is audited as
// webhook_queue_size_retuned (endpoint plus previous_queue_size /
// current_queue_size, purely numeric). A ledger with no dispatcher
// attached returns ErrWebhooksNotConfigured; an unknown endpoint
// returns ErrWebhookEndpointNotFound; a capacity below 1 returns
// ErrInvalidWebhookQueueSize with the live capacity untouched (the
// returned value is the live capacity when the endpoint is known).
// A revision identical to the live capacity is a no-op with no
// audit event. The revision bumps neither the version nor the audit
// chain, writes no journal entry, and emits no outbox event.
// Restarting restores the construction-time capacity.
func (l *Ledger) SetWebhookQueueSize(name string, n int) (int, error) {
	return l.SetWebhookQueueSizeCtx(context.Background(), name, n)
}

// SetWebhookQueueSizeCtx is SetWebhookQueueSize with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetWebhookQueueSizeCtx(ctx context.Context, name string, n int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Check order: the capacity gate first — an illegal capacity is
	// rejected even when no dispatcher is attached — then the
	// dispatcher, then the endpoint.
	if n < 1 {
		if l.webhooks != nil {
			if live, ok := l.webhooks.QueueSize(name); ok {
				return live, fmt.Errorf("%w: got %d", ErrInvalidWebhookQueueSize, n)
			}
		}
		return 0, fmt.Errorf("%w: got %d", ErrInvalidWebhookQueueSize, n)
	}
	if l.webhooks == nil {
		return 0, ErrWebhooksNotConfigured
	}
	previous, err := l.webhooks.SetQueueSize(name, n)
	if err != nil {
		return previous, err
	}
	if n == previous {
		return previous, nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "webhook_queue_size_retuned",
		Actor:         "SetWebhookQueueSize",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"endpoint":            name,
			"previous_queue_size": previous,
			"current_queue_size":  n,
		},
	})
	return previous, nil
}

// WebhookQueueSize reports one registered endpoint's live queue
// capacity. It returns false when the ledger has no dispatcher
// attached or the name is unknown.
func (l *Ledger) WebhookQueueSize(name string) (int, bool) {
	if l.webhooks == nil {
		return 0, false
	}
	return l.webhooks.QueueSize(name)
}
