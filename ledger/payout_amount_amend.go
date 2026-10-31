// Payout instruction amount amendment (LG-273).
//
// CreatePayout (LG-140) fixes a payout instruction's amount at draft
// time, and ApprovePayout (LG-42) runs maker-checker against that
// amount: at or above the payee's review threshold the approver must
// differ from the maker. An operator who mistyped the amount — or
// whose instruction was drafted against a stale quote — had no way to
// correct it short of cancelling the draft and drafting a fresh one.
// A fresh instruction is a new instruction: a new ID, a new UETR, and
// a new audit trail, while the cancelled draft stays on the books as
// noise. This module is the revision path: an operator corrects a
// draft instruction's amount in place, before any approval has been
// given and before any money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved amount is the amount the
//     checker signed off on, and changing it behind the approval
//     would void the maker-checker separation; once submitted, the
//     clearing leg is already journaled against the original amount.
//     The state gate runs before the amount validation and the no-op
//     check, following the invoice-memo precedent (LG-272): a departed
//     instruction rejects even a same-value amendment.
//   - The new amount reuses the CreatePayout gate exactly: it must
//     be positive, else ErrPayoutNonPositiveAmount, and the original
//     amount is untouched. Zero is not a revision here — zero is not
//     a payable amount, and this module never deletes an instruction,
//     so an operator who wants the instruction gone still cancels it.
//   - The amendment touches AmountCents and nothing else: payee,
//     currency, destination, maker, status, UETR, and the idempotency
//     key are unchanged, and no journal row exists yet to revisit —
//     the draft is off-journal, so amending it has zero bookkeeping
//     impact. The four-eyes threshold reads the live amount at
//     approval time (ApprovePayout, payout.go), so a revised amount
//     re-enters the maker-checker decision from the very next approval
//     with no approval-path change: raising a draft across the
//     threshold newly requires a different checker, and lowering one
//     below it newly lets the maker self-approve.
//   - Amending to the current amount is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and an amount move on an unapproved draft is not a new
//     instruction. The amendment lands one audit event
//     (payout_amount_amended, with the payout ID and the previous and
//     new amounts) so the "who changed the amount, and from what to
//     what" trail is tamper-evident. Payout instructions stay out of
//     snapshots (see payout.go), so an amendment, like the instruction
//     itself, is in-memory operational state and never round-trips.
//
// Orthogonality: CreatePayout owns the initial amount, ApprovePayout
// owns the approval (and reads the live amount for maker-checker),
// SubmitPayout owns the clearing earmark, ExtendPayoutExpiry (LG-267)
// owns the approved instruction's deadline, and this module owns only
// the draft amount revision. It is orthogonal to the payout
// destination amendment (LG-274, which moves where the money goes) —
// this moves only how much goes.
package ledger

import (
	"context"
)

// AmendPayoutAmount revises a draft payout instruction's amount in
// place. An unknown ID fails with ErrPayoutNotFound; a non-draft
// instruction fails with ErrPayoutInvalidTransition; a non-positive
// amount fails with ErrPayoutNonPositiveAmount. Only AmountCents
// changes; the payee, currency, destination, maker, status, UETR, and
// idempotency key are untouched. Amending to the current amount is a
// no-op. Amending is off-journal — it does not bump the ledger version
// — but it is audited as payout_amount_amended.
func (l *Ledger) AmendPayoutAmount(id string, newAmountCents int64) (PayoutInstruction, error) {
	return l.AmendPayoutAmountCtx(context.Background(), id, newAmountCents)
}

// AmendPayoutAmountCtx is AmendPayoutAmount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendPayoutAmountCtx(ctx context.Context, id string, newAmountCents int64) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if newAmountCents <= 0 {
		return PayoutInstruction{}, ErrPayoutNonPositiveAmount
	}
	if newAmountCents == p.AmountCents {
		return p, nil
	}
	previous := p.AmountCents
	p.AmountCents = newAmountCents
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_amount_amended",
		Actor:         "AmendPayoutAmount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":             id,
			"previous_amount_cents": previous,
			"amount_cents":          newAmountCents,
		},
	})
	return p, nil
}
