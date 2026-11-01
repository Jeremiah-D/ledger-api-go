// Payout instruction expiry amendment (LG-281).
//
// CreatePayout (LG-140) accepts an opt-in ExpiresAt at draft time —
// zero means "never expires" — and deliberately does not check that
// a set expiry lies in the future, so a draft may be created carrying
// an already-past deadline (approving it simply makes it expirable on
// the next sweep). An operator who drafted against the wrong
// deadline — a payment run moved a week out, or a deadline that was
// never meant to be set — had no way to correct it short of
// cancelling the draft and drafting a fresh one. A fresh instruction
// is a new instruction: a new ID, a new UETR, and a new audit trail,
// while the cancelled draft stays on the books as noise. This module
// is the revision path: an operator corrects a draft instruction's
// expiry in place, before any approval has been given and before any
// money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved deadline is the deadline the
//     checker signed off on, and changing it behind the approval
//     would void the maker-checker separation; once submitted, the
//     clearing leg is already journaled. The state gate runs before
//     the expiry validation and the no-op check, following the
//     invoice-memo precedent (LG-272): a departed instruction
//     rejects even a same-value amendment.
//   - The new expiry is stricter than creation: zero clears the
//     deadline ("never expires" again), but a non-zero value must
//     lie in the future, else ErrInvalidPayoutExpiresAt and the
//     original deadline is untouched. Creation does not check
//     futureness, so the revision gate is deliberately stricter:
//     writing an already-past deadline onto a draft would make the
//     instruction expire the moment it is approved, so a past
//     deadline is refused rather than recorded.
//   - The amendment touches ExpiresAt and nothing else: amount,
//     currency, destination, payee account, maker, status, UETR,
//     and the idempotency key are unchanged, and no journal row
//     exists yet to revisit — the draft is off-journal, so amending
//     it has zero bookkeeping impact. The revised deadline takes
//     effect from approval: ExpirePayouts sweeps and SubmitPayout's
//     lazy expiry both read the live ExpiresAt (payout_expiry.go),
//     so a revised deadline governs the next approval with no
//     approval-path change. Drafts are never swept, so revising a
//     draft's deadline cannot expire anything.
//   - Amending to the current expiry is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing. Zero and zero are the same no-op.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a deadline move on an unapproved draft is not a
//     new instruction. The amendment lands one audit event
//     (payout_expires_at_amended, with the payout ID and the
//     previous and new deadlines — a zero deadline recorded as
//     null) so the "who changed the deadline, and from what to
//     what" trail is tamper-evident. Payout instructions stay out
//     of snapshots (see payout.go), so an amendment, like the
//     instruction itself, is in-memory operational state and never
//     round-trips.
//
// Orthogonality: CreatePayout owns the initial expiry, ApprovePayout
// owns the approval, SubmitPayout owns the clearing earmark,
// ExpirePayouts owns the sweep, ExtendPayoutExpiry (LG-267) owns the
// approved instruction's deadline lengthening, and this module owns
// only the draft expiry revision. It is orthogonal to the expiry
// mechanism itself (LG-213, which expires approved instructions) —
// this revises only which deadline a draft carries — and to the
// sibling draft amendments (LG-273 amount, LG-274 destination,
// LG-277 currency, LG-278 rail, LG-279 payee account, LG-280
// purpose code), each of which owns one other field.
package ledger

import (
	"context"
	"errors"
	"time"
)

// Payout instruction expiry amendment errors.
var (
	// ErrInvalidPayoutExpiresAt is returned by AmendPayoutExpiresAt
	// when the new deadline is non-zero but not in the future: a
	// draft revised onto an already-past deadline would expire the
	// moment it is approved, so the revision is refused and the
	// original deadline is untouched. Zero clears the deadline and
	// is always accepted.
	ErrInvalidPayoutExpiresAt = errors.New("ledger: payout expires_at must be in the future")
)

// AmendPayoutExpiresAt revises a draft payout instruction's expiry
// in place. An unknown ID fails with ErrPayoutNotFound; a non-draft
// instruction fails with ErrPayoutInvalidTransition; a non-zero
// deadline that is not in the future fails with
// ErrInvalidPayoutExpiresAt. Zero clears the deadline ("never
// expires"). Only ExpiresAt changes; the amount, currency,
// destination, payee account, maker, status, UETR, and idempotency
// key are untouched. Amending to the current expiry is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as payout_expires_at_amended.
func (l *Ledger) AmendPayoutExpiresAt(id string, newExpiresAt time.Time) (PayoutInstruction, error) {
	return l.AmendPayoutExpiresAtCtx(context.Background(), id, newExpiresAt)
}

// AmendPayoutExpiresAtCtx is AmendPayoutExpiresAt with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendPayoutExpiresAtCtx(ctx context.Context, id string, newExpiresAt time.Time) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if !newExpiresAt.IsZero() && !newExpiresAt.After(time.Now()) {
		return PayoutInstruction{}, ErrInvalidPayoutExpiresAt
	}
	if newExpiresAt.Equal(p.ExpiresAt) {
		return p, nil
	}
	previous := p.ExpiresAt
	p.ExpiresAt = newExpiresAt
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_expires_at_amended",
		Actor:         "AmendPayoutExpiresAt",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":           id,
			"previous_expires_at": formatNullableExpiresAt(previous),
			"expires_at":          formatNullableExpiresAt(newExpiresAt),
		},
	})
	return p, nil
}

// formatNullableExpiresAt renders an expiry for audit details: a
// zero deadline is null ("never expires"), a set one is the
// RFC3339 UTC instant, matching payout_expired and
// payout_expiry_extended.
func formatNullableExpiresAt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
