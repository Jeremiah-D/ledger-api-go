package ledger

import (
	"errors"
	"sort"
	"time"
)

// Payout instruction expiry (LG-213, fintech: instruction lifecycle).
//
// An approved payout is an operator promise to move money, but it is
// not an open-ended one: a payout approved for a payment run that
// never happened must not sit approved forever and then settle
// months later against a stale approval. The instruction carries an
// opt-in ExpiresAt (see PayoutInstruction): zero means "never
// expires", preserving every existing instruction's behavior, and a
// set value is the last instant the instruction may still be
// submitted — at or after ExpiresAt the instruction is dead.
//
// Only approved instructions expire. Drafts are not swept: a draft
// has no approval to lapse, and approving a past-expiry draft simply
// makes it expirable on the next sweep (or on its next submit
// attempt, see below). Submitted instructions are in flight and
// never expire here: once the clearing leg is journaled, the
// instruction's fate is settle or fail, never a silent expiry —
// expiring a submitted instruction would leave the clearing suspense
// with no instruction to settle it. Cancelled, settled, failed,
// returned, and already-expired instructions are terminal and are
// never touched again.
//
// Expiry is a status transition, not a booking: it posts no journal
// row, bumps no ledger version, and moves no money — exactly like
// cancelling an approved instruction, except the terminal status is
// "expired" rather than "cancelled", so an auditor can tell a
// lapsed approval from a deliberate human cancellation. Every
// expired instruction emits one payout_expired audit event (with
// the expiry instant and the previous status) and one
// payout_expired outbox event, so downstream consumers see the
// lapse individually, like the hold-expiry outbox.
//
// Two paths expire an instruction, and both use the same locked
// helper, so they can never disagree:
//
//   - ExpirePayoutsAt sweeps every approved, past-expiry
//     instruction at once — the operator-facing bulk path
//     (POST /payouts/expire), mirroring ExpireHolds.
//   - SubmitPayout lazily expires the single instruction it was
//     asked to submit when that instruction is already past its
//     ExpiresAt: without this guard, a submit racing ahead of the
//     next sweep would move money on a lapsed approval. The lazy
//     expiry emits the same audit and outbox events, then the
//     submit fails with ErrPayoutExpired instead of booking.
//
// This is orthogonal to the settlement cutoff (LG-101, see
// settlement_cutoff.go): the cutoff defers a booked transfer's
// settlement window, while expiry kills an unsubmitted instruction
// outright. It is orthogonal to the payout idempotency TTL (see
// prunePayoutKeysLocked): the TTL governs how long a replayed open
// key returns the original instruction, while expiry governs how
// long an approved instruction may still be submitted.

// Payout instruction expiry errors.
var (
	// ErrPayoutExpired is returned by SubmitPayout when the
	// instruction's ExpiresAt has passed: the submit lazily expires
	// the instruction (status "expired", payout_expired audit and
	// outbox events) and books nothing.
	ErrPayoutExpired = errors.New("ledger: payout instruction has expired")
)

// ExpirePayouts sweeps every approved payout instruction whose
// ExpiresAt has passed as of now into the expired terminal status
// and returns the expired instructions, sorted by ID. It is
// idempotent: a second sweep finds nothing left to expire.
func (l *Ledger) ExpirePayouts() []PayoutInstruction {
	return l.ExpirePayoutsAt(time.Now())
}

// ExpirePayoutsAt is ExpirePayouts with a caller-supplied timestamp,
// so tests can pin expiry deterministically.
func (l *Ledger) ExpirePayoutsAt(now time.Time) []PayoutInstruction {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expirePayoutsLocked(now, "ExpirePayouts")
}

// expirePayoutLocked expires one approved, past-expiry instruction
// in place and emits its audit and outbox events. Callers must hold
// the write lock. It returns false when the instruction must not
// expire (not approved, no expiry set, or not yet past expiry), so
// the submit guard and the sweep share one definition of "expired".
func (l *Ledger) expirePayoutLocked(p *PayoutInstruction, now time.Time, actor string) bool {
	if p.Status != PayoutApproved {
		return false
	}
	if p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt) {
		return false
	}
	p.Status = PayoutExpired
	p.ExpiredAt = now
	l.payouts[p.ID] = *p
	l.emitAudit(AuditEvent{
		Op:            "payout_expired",
		Actor:         actor,
		TraceID:       p.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"amount_cents": p.AmountCents,
			"currency":     p.Currency,
			"from_status":  PayoutApproved,
			"expires_at":   p.ExpiresAt.UTC().Format(time.RFC3339),
			"expired_at":   now.UTC().Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutExpired,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": p.ID},
	})
	return true
}

// expirePayoutsLocked sweeps every approved, past-expiry instruction
// and returns the expired copies, sorted by ID for determinism.
// Callers must hold the write lock.
func (l *Ledger) expirePayoutsLocked(now time.Time, actor string) []PayoutInstruction {
	ids := make([]string, 0, len(l.payouts))
	for id := range l.payouts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var expired []PayoutInstruction
	for _, id := range ids {
		p := l.payouts[id]
		if l.expirePayoutLocked(&p, now, actor) {
			expired = append(expired, p)
		}
	}
	return expired
}
