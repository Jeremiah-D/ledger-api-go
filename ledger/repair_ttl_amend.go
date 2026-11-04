package ledger

import (
	"context"
	"errors"
	"time"
)

// Payment repair queue TTL revision (LG-311).
//
// defaultRepairTTL (repair.go) fixes the per-record survival window
// at 7 days, with no construction-time option at all (WithRepair*
// only has WithRepairQueueCap) — changing it meant editing source
// and recompiling. When an STP incident bursts mid-holiday-freeze
// the operator may want to widen the window so parked instructions
// survive until someone can work them, or tighten it during a
// post-incident review so stale instructions expire early. This
// module is the orthogonal runtime path: an operator revises the
// TTL in place, instead of rebuilding the ledger.
//
// This module follows the LG-310 same-module pattern exactly: the
// live value lives on the repairQueue struct (repair.go), next to
// cap, initialized to defaultRepairTTL on first use, and
// EnqueueRepair stamps q.ttl onto each new record. The semantics,
// deliberately fail-closed throughout, follow the LG-241 / LG-242 /
// LG-309 / LG-310 structural revisions:
//
//   - Validation is strict, unlike LG-242: a non-positive TTL is
//     ErrInvalidRepairTTL and the live value stays untouched. A
//     non-positive TTL would make every newly enqueued record
//     immediately expired — that is an operator error, not a
//     "disable expiry" switch, so it is rejected, never silently
//     normalized (per LG-241).
//   - The setter takes the same lock order as EnqueueRepair (l.mu,
//     then repairQueues.Mutex) and swaps q.ttl under both, so the
//     revision is atomic against concurrent enqueues.
//   - Future-only: the revision stamps only records enqueued after
//     it. Already-open records keep the TTL they were enqueued
//     with — both expiry paths (SweepExpiredRepairs and the lazy
//     expiry in RepairAndResubmit) read the record's own TTL, so
//     shortening never expires a parked record early and
//     lengthening never revives an already-expired record
//     (expired is terminal: ErrRepairTerminal).
//   - Revising to the current TTL is a no-op: it returns nil and
//     writes no audit event, so a retried or duplicated revision
//     cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it
//     moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires.
//     It lands one repair_ttl_amended event (previous_ttl / ttl,
//     as duration strings, following the LG-242
//     idempotency_ttl_amended detail shape). The Ctx variant
//     attributes the operator (LG-73).
//   - Snapshots carry no repair queue (snapshot.go has no repair
//     state): a disaster-recovery restore returns the default TTL
//     of 7 days, and the amendment must be re-applied — the
//     amendment events themselves stay in the audit trail for
//     traceability.
//
// Orthogonality: LG-177 owns the repair queue mechanism itself,
// LG-310 owns the same module's capacity knob (a different
// dimension), LG-242 owns the idempotency-key TTL (whose
// non-positive values disable expiry — the opposite validation
// choice, deliberately), and SweepExpiredRepairs owns the expiry
// sweep path. There is no environment variable or HTTP endpoint
// for the revision (per LG-241/242): repair-queue tuning is
// in-process structural config.

var (
	// ErrInvalidRepairTTL is returned by SetRepairTTL when the
	// requested TTL is not positive: a non-positive TTL would make
	// newly enqueued records immediately expired.
	ErrInvalidRepairTTL = errors.New("ledger: repair record TTL must be positive")
)

// SetRepairTTL revises the repair queue's per-record TTL in place.
// Records enqueued after the revision are stamped with the new TTL;
// already-open records keep the TTL they were enqueued with, so
// shortening never expires a parked record early and lengthening
// never revives an already-expired record. A non-positive TTL is
// rejected with ErrInvalidRepairTTL and the live value is left
// untouched. Revising to the current TTL is a no-op with no audit
// event. The revision is audited as repair_ttl_amended.
func (l *Ledger) SetRepairTTL(ttl time.Duration) error {
	return l.SetRepairTTLCtx(context.Background(), ttl)
}

// SetRepairTTLCtx is SetRepairTTL with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetRepairTTLCtx(ctx context.Context, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidRepairTTL
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	q := repairQueueForLocked(l)
	if ttl == q.ttl {
		return nil
	}
	previous := q.ttl
	q.ttl = ttl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "repair_ttl_amended",
		Actor:         "SetRepairTTL",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ttl": previous.String(),
			"ttl":          ttl.String(),
		},
	})
	return nil
}

// RepairTTL reports the repair queue's current per-record TTL: the
// TTL stamped onto records enqueued from now on. The queue is
// created lazily on first use, so a ledger that never parked a
// record reports the default (7 days).
func (l *Ledger) RepairTTL() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	return repairQueueForLocked(l).ttl
}
