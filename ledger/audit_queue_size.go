package ledger

import (
	"context"
	"errors"
)

// Audit-log async queue capacity revision (LG-309).
//
// WithAuditLogQueueSize (audit.go) configures the async event queue
// only at construction — the channel between ledger operations and the
// background writer was fixed for the life of the process, so a burst
// that filled it could only be absorbed by restarting with a bigger
// queue. LG-301 revised the rotation cap and the retention window the
// same way; the queue capacity was the remaining construction-time
// knob in the same file and the same module.
//
// This module is the orthogonal runtime path: an operator revises the
// capacity in place, instead of rebuilding the ledger and replaying
// its journal. The semantics, deliberately fail-closed throughout,
// follow the LG-301 structural revisions:
//
//   - The queue itself is a channel, so unlike maxBytes it cannot be
//     swapped by writing a field: the background writer reads its
//     generation in run, and Log sends into it. The swap is therefore
//     owned by the writer (audit.go applyQueueResize): a revision
//     travels to the writer as a queueResizeRequest, and the writer
//     applies it at a select point where it holds no received event.
//     The old generation is drained into pending under cfgMu — Log
//     sends under cfgMu.RLock, so no event can enter the old channel
//     mid-drain and no drained event can be overtaken by a new one —
//     then the new channel is installed. The writer flushes pending,
//     oldest first, before taking new events from the channel, so a
//     shrink loses nothing and the hash chain stays in enqueue order.
//   - While pending is non-empty the total depth (len(pending) +
//     len(queue)) can exceed the new capacity, and new events are
//     dropped by plain channel semantics whenever the (smaller) new
//     channel is full, until the writer works the backlog down. Log
//     never blocks: a revision can never stall a poster.
//   - Validation reuses the construction-time gate, without the "keep
//     the default" fallback — at runtime there is no default to fall
//     back to. The capacity must be a positive number of events
//     (WithAuditLogQueueSize ignores non-positive values at
//     construction; here they are ErrInvalidAuditLogQueueSize and the
//     old capacity is left untouched).
//   - A ledger with no audit log attached (WithAuditLog was never
//     used) rejects the revision with ErrAuditLogNotConfigured — the
//     mutation form of AuditStats' enabled=false.
//   - Revising to the current capacity is a no-op: it returns nil and
//     writes no audit event, so a retried or duplicated revision
//     cannot spam the audit trail.
//   - The amendment is log infrastructure tuning, not ledger state:
//     it moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires. It
//     lands one audit_log_queue_size_amended event
//     (previous_queue_size / queue_size). The Ctx variant attributes
//     the operator (LG-73).
//   - Snapshots carry no capacity (AuditLog lives outside the ledger
//     snapshot): a restart restores the construction-time / env value,
//     while the amendment events themselves stay in the audit trail
//     for traceability.
//
// Orthogonality: WithAuditLogQueueSize owns construction-time install,
// LG-301 owns the rotation cap and retention window, LG-65 owns
// per-line HMAC signing, LG-32 owns hash-chain sealing, and this
// module owns only the runtime revision of the queue capacity. There
// is no HTTP endpoint for the revision (per LG-241/242): audit-log
// tuning is in-process structural config.

var (
	// ErrInvalidAuditLogQueueSize is returned by SetAuditLogQueueSize
	// when the requested capacity is not a positive number of events.
	ErrInvalidAuditLogQueueSize = errors.New("ledger: audit log queue size must be a positive number of events")
)

// SetAuditLogQueueSize revises the audit log's async event-queue
// capacity in place. A larger capacity takes effect immediately: the
// next burst enqueues into the new channel. A smaller capacity never
// drops buffered events — they are drained and flushed in order —
// while the new (smaller) channel is full new events are dropped by
// plain channel semantics until the writer works the backlog down. A
// non-positive capacity is rejected with
// ErrInvalidAuditLogQueueSize and the old capacity is left untouched;
// a ledger with no audit log attached returns
// ErrAuditLogNotConfigured. Revising to the current capacity is a
// no-op with no audit event. The revision is audited as
// audit_log_queue_size_amended.
func (l *Ledger) SetAuditLogQueueSize(n int) error {
	return l.SetAuditLogQueueSizeCtx(context.Background(), n)
}

// SetAuditLogQueueSizeCtx is SetAuditLogQueueSize with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetAuditLogQueueSizeCtx(ctx context.Context, n int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 {
		return ErrInvalidAuditLogQueueSize
	}
	if l.audit == nil {
		return ErrAuditLogNotConfigured
	}
	previous, changed := l.audit.setQueueSize(n)
	if !changed {
		return nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_log_queue_size_amended",
		Actor:         "SetAuditLogQueueSize",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_queue_size": previous,
			"queue_size":          n,
		},
	})
	return nil
}

// AuditLogQueueSize reports the audit log's current async event-queue
// capacity. Enabled is false when no audit log is attached (WithAuditLog
// was never used): in that case the capacity is zero and meaningless.
// Follows the AuditStats enabled shape.
func (l *Ledger) AuditLogQueueSize() (queueSize int, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.QueueSize(), true
}
