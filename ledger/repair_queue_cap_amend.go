package ledger

import (
	"context"
	"errors"
)

// Payment repair queue capacity revision (LG-310).
//
// WithRepairQueueCap (repair.go) configures the repair queue's
// capacity only at construction — and non-positive values silently
// keep the default (1000), since it is mainly a test hook. A burst
// of STP failures can therefore fill the queue mid-incident, after
// which every new failed payout is hard-rejected instead of parked,
// and the only remedy was a restart with a bigger cap. LG-290 and
// LG-301 revised sibling structural knobs the same way; the repair
// queue capacity was the remaining construction-time knob in this
// module.
//
// This module is the orthogonal runtime path: an operator revises
// the capacity in place, instead of rebuilding the ledger. The
// semantics, deliberately fail-closed throughout, follow the LG-290
// / LG-301 / LG-309 structural revisions:
//
//   - Validation reuses the construction-time gate, without the
//     "keep the default" fallback — at runtime there is no default
//     to fall back to. The capacity must be a positive number of
//     records; a non-positive value is ErrInvalidRepairQueueCap and
//     the old capacity is left untouched (never silently
//     normalized, per LG-241).
//   - The setter takes the same lock order as EnqueueRepair (l.mu,
//     then repairQueues.Mutex) and swaps q.cap under both, so the
//     revision is atomic against concurrent enqueues.
//   - Expanding takes effect immediately: the next EnqueueRepair
//     admits against the new cap.
//   - Shrinking never evicts a parked record — the fail-closed
//     invariant: parked records leave the open state only through
//     resubmit or expiry (SweepExpiredRepairs). Note, however, that
//     terminal records are never removed from the queue map
//     (repair.go has no delete path: the only q.records operations
//     are the len() admission check and insertion), so expiry alone
//     does NOT free admission slots — a shrink below the record
//     count keeps new enqueues failing with ErrRepairQueueFull
//     until the cap is raised again. Changing what counts toward
//     the cap is a separate item; this module only moves the cap.
//   - Revising to the current capacity is a no-op: it returns nil
//     and writes no audit event, so a retried or duplicated
//     revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it
//     moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires.
//     It lands one repair_queue_cap_amended event (previous_cap /
//     cap). The Ctx variant attributes the operator (LG-73).
//   - Snapshots carry no repair queue (snapshot.go has no repair
//     state): a disaster-recovery restore returns the default cap
//     of 1000 (or the construction-time value), and the amendment
//     must be re-applied — the amendment events themselves stay in
//     the audit trail for traceability.
//
// Orthogonality: LG-177 owns the repair queue mechanism itself,
// LG-309 owns the audit-log async queue (a different queue), LG-290
// owns the split size cap, and this module owns only the runtime
// revision of the repair queue capacity. There is no environment
// variable or HTTP endpoint for the revision (per LG-241/242):
// repair-queue tuning is in-process structural config.

var (
	// ErrInvalidRepairQueueCap is returned by SetRepairQueueCap when
	// the requested capacity is not a positive number of records.
	ErrInvalidRepairQueueCap = errors.New("ledger: repair queue capacity must be a positive number of records")
)

// SetRepairQueueCap revises the repair queue's capacity in place. A
// larger capacity takes effect immediately: the next EnqueueRepair
// admits against the new cap. A smaller capacity never evicts a
// parked record — while the record count is at or above the new cap,
// new enqueues fail with ErrRepairQueueFull until the cap is raised
// again (expiry alone does not free slots: terminal records stay in
// the queue map). A non-positive capacity is rejected with
// ErrInvalidRepairQueueCap and the old capacity is left untouched.
// Revising to the current capacity is a no-op with no audit event.
// The revision is audited as repair_queue_cap_amended.
func (l *Ledger) SetRepairQueueCap(n int) error {
	return l.SetRepairQueueCapCtx(context.Background(), n)
}

// SetRepairQueueCapCtx is SetRepairQueueCap with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetRepairQueueCapCtx(ctx context.Context, n int) error {
	if n <= 0 {
		return ErrInvalidRepairQueueCap
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	q := repairQueueForLocked(l)
	if n == q.cap {
		return nil
	}
	previous := q.cap
	q.cap = n
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "repair_queue_cap_amended",
		Actor:         "SetRepairQueueCap",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_cap": previous,
			"cap":          n,
		},
	})
	return nil
}

// RepairQueueCap reports the repair queue's current capacity. The
// queue is created lazily on first use, so a ledger that never parked
// a record reports the default (1000) or the construction-time
// WithRepairQueueCap value.
func (l *Ledger) RepairQueueCap() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	return repairQueueForLocked(l).cap
}
