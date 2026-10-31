// Payout instruction destination amendment (LG-274).
//
// CreatePayout (LG-140) fixes a payout instruction's destination at
// draft time — the external account reference the money will leave
// for, LG-122 style (e.g. "FPS:6222000012345678") — and the destination
// is validated against the registered external mappings when it parses
// as "<system>:<externalID>". An operator who mistyped the destination
// — a transposed digit in the external account, the wrong rail system
// prefix — had no way to correct it short of cancelling the draft and
// drafting a fresh one. A fresh instruction is a new instruction: a
// new ID, a new UETR, and a new audit trail, while the cancelled draft
// stays on the books as noise. This module is the revision path: an
// operator corrects a draft instruction's destination in place, before
// any approval has been given and before any money has moved.
//
// Why a draft-only destination change is safe where a live-plan
// redirect would not be: the recurring-transfer plans (see
// recurring.go) are approved, running instructions — changing their
// From/To would redirect a live money flow mid-flight, so those
// endpoints do not exist. A payout draft is different: it is an
// unapproved instruction, no approval has been given, no clearing leg
// has been journaled, and the approver still reviews the final
// destination at ApprovePayout time. The revision is a correction to
// the terms under review, not a redirect of approved funds.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved destination is the destination
//     the checker signed off on, and changing it behind the approval
//     would void the maker-checker separation; once submitted, the
//     clearing leg is already journaled for the original destination.
//     The state gate runs before the destination validation and the
//     no-op check, following the invoice-memo precedent (LG-272): a
//     departed instruction rejects even a same-value amendment.
//   - The new destination reuses the CreatePayout gate exactly:
//     validatePayoutDestinationLocked (payout.go) rejects an empty
//     destination with ErrEmptyPayoutDestination and, when the
//     destination parses as "<system>:<externalID>" and the ledger
//     holds a registered external mapping (LG-122) for the payee
//     under that system, rejects a mismatch with
//     ErrPayoutUnknownDestination. A failed amendment leaves the
//     original destination exactly as it was.
//   - The amendment touches Destination and nothing else: amount,
//     payee, currency, maker, status, UETR, and the idempotency key
//     are unchanged, and no journal row exists yet to revisit — the
//     draft is off-journal, so amending it has zero bookkeeping
//     impact. The approver reviews the amended destination at
//     ApprovePayout time, and SubmitPayout journals the clearing leg
//     against the payee account (not the destination string), so no
//     submit-path change is needed.
//   - Amending to the current destination is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a destination move on an unapproved draft is not a
//     new instruction. The amendment lands one audit event
//     (payout_destination_amended, with the payout ID and the previous
//     and new destinations) so the "who changed the destination, and
//     from what to what" trail is tamper-evident. Payout instructions
//     stay out of snapshots (see payout.go), so an amendment, like the
//     instruction itself, is in-memory operational state and never
//     round-trips.
//
// Orthogonality: CreatePayout owns the initial destination,
// ApprovePayout owns the approval (and reviews the final destination),
// SubmitPayout owns the clearing earmark, ExtendPayoutExpiry (LG-267)
// owns the approved instruction's deadline, AmendPayoutAmount (LG-273)
// owns the draft amount, and this module owns only the draft
// destination revision. It is orthogonal to the external-mapping
// mechanism itself (LG-122, which registers what a destination may
// point at) — this moves only which registered destination a draft
// points at.
package ledger

import (
	"context"
)

// AmendPayoutDestination revises a draft payout instruction's
// destination in place. An unknown ID fails with ErrPayoutNotFound; a
// non-draft instruction fails with ErrPayoutInvalidTransition; an
// empty destination fails with ErrEmptyPayoutDestination; a
// destination that mismatches the registered external mapping fails
// with ErrPayoutUnknownDestination. Only Destination changes; the
// amount, payee, currency, maker, status, UETR, and idempotency key
// are untouched. Amending to the current destination is a no-op.
// Amending is off-journal — it does not bump the ledger version — but
// it is audited as payout_destination_amended.
func (l *Ledger) AmendPayoutDestination(id string, newDestination string) (PayoutInstruction, error) {
	return l.AmendPayoutDestinationCtx(context.Background(), id, newDestination)
}

// AmendPayoutDestinationCtx is AmendPayoutDestination with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendPayoutDestinationCtx(ctx context.Context, id string, newDestination string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if err := l.validatePayoutDestinationLocked(p.PayeeAccount, newDestination); err != nil {
		return PayoutInstruction{}, err
	}
	if newDestination == p.Destination {
		return p, nil
	}
	previous := p.Destination
	p.Destination = newDestination
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_destination_amended",
		Actor:         "AmendPayoutDestination",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":            id,
			"previous_destination": previous,
			"destination":          newDestination,
		},
	})
	return p, nil
}
