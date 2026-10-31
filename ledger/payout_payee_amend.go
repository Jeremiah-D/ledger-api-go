// Payout instruction payee-account amendment (LG-279).
//
// CreatePayout (LG-140) fixes a payout instruction's payee account at
// draft time — the ledger account the money leaves — and ApprovePayout
// (LG-42) runs maker-checker against that account's review threshold,
// while SubmitPayout journals the clearing leg against it
// (payout.go). An operator who drafted against the wrong payee — the
// payout was meant to leave the operating account, not the savings
// account, or the payee's books were reorganized after drafting — had
// no way to correct it short of cancelling the draft and drafting a
// fresh one. A fresh instruction is a new instruction: a new ID, a new
// UETR, and a new audit trail, while the cancelled draft stays on the
// books as noise. This module is the revision path: an operator
// corrects a draft instruction's payee account in place, before any
// approval has been given and before any money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved payee is the payee the checker
//     signed off on, and changing it behind the approval would void
//     the maker-checker separation; once submitted, the clearing leg
//     is already journaled against the original payee. The state
//     gate runs before the payee validation and the no-op check,
//     following the invoice-memo precedent (LG-272): a departed
//     instruction rejects even a same-value amendment.
//   - The new payee reuses the CreatePayout gate exactly: an empty
//     payee fails with ErrEmptyPayoutAccount and the original payee
//     is untouched. The live destination is then re-validated against
//     the new payee — validatePayoutDestinationLocked (payout.go)
//     with the new payee and the unchanged destination — because the
//     destination may be registered under the old payee's external
//     mapping (LG-122) but not the new one's: a live destination
//     unregistered under the new payee fails with
//     ErrPayoutUnknownDestination, an empty live destination with
//     ErrEmptyPayoutDestination, and the original payee is untouched
//     in either case. A payee that could not have been drafted
//     against the live destination cannot be amended to either.
//   - The amendment touches PayeeAccount and nothing else: amount,
//     currency, destination, maker, status, UETR, and the idempotency
//     key are unchanged, and no journal row exists yet to revisit —
//     the draft is off-journal, so amending it has zero bookkeeping
//     impact. The four-eyes threshold reads the live payee at
//     approval time (ApprovePayout, payout.go), so a revised payee
//     re-enters the maker-checker decision from the very next
//     approval with no approval-path change: moving a draft onto a
//     thresholded account newly requires a different checker, and
//     moving one off it newly lets the maker self-approve. The
//     clearing legs read the live payee at submit time (SubmitPayout,
//     payout.go), so the revised payee governs the next submit with
//     no submit-path change: the earmark debits the new payee.
//   - Amending to the current payee is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a payee move on an unapproved draft is not a new
//     instruction. The amendment lands one audit event
//     (payout_payee_account_amended, with the payout ID and the
//     previous and new payee accounts) so the "who moved the payout,
//     and from which account to which" trail is tamper-evident. The
//     event names both accounts: it concerns the old payee's trail
//     as much as the new one's. Payout instructions stay out of
//     snapshots (see payout.go), so an amendment, like the
//     instruction itself, is in-memory operational state and never
//     round-trips.
//
// Orthogonality: CreatePayout owns the initial payee, ApprovePayout
// owns the approval (and reads the live payee for maker-checker),
// SubmitPayout owns the clearing earmark (and journals it against
// the live payee), AmendPayoutAmount (LG-273) owns the draft amount,
// AmendPayoutDestination (LG-274) owns the draft destination,
// AmendPayoutCurrency (LG-277) owns the draft currency,
// AmendPayoutRail (LG-278) owns the draft settlement rail, and this
// module owns only the draft payee-account revision. It is orthogonal
// to the external-mapping mechanism itself (LG-122, which registers
// what a destination may point at) — this moves only which account a
// draft leaves from, and re-checks the live destination against the
// new account's registrations.
package ledger

import (
	"context"
)

// AmendPayoutPayeeAccount revises a draft payout instruction's payee
// account in place. An unknown ID fails with ErrPayoutNotFound; a
// non-draft instruction fails with ErrPayoutInvalidTransition; an
// empty payee fails with ErrEmptyPayoutAccount; a live destination
// that is empty fails with ErrEmptyPayoutDestination, and one that
// mismatches the new payee's registered external mapping fails with
// ErrPayoutUnknownDestination. Only PayeeAccount changes; the amount,
// currency, destination, maker, status, UETR, and idempotency key are
// untouched. Amending to the current payee is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as payout_payee_account_amended.
func (l *Ledger) AmendPayoutPayeeAccount(id string, newPayeeAccount AccountID) (PayoutInstruction, error) {
	return l.AmendPayoutPayeeAccountCtx(context.Background(), id, newPayeeAccount)
}

// AmendPayoutPayeeAccountCtx is AmendPayoutPayeeAccount with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendPayoutPayeeAccountCtx(ctx context.Context, id string, newPayeeAccount AccountID) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if newPayeeAccount == "" {
		return PayoutInstruction{}, ErrEmptyPayoutAccount
	}
	// The live destination must still be valid under the new payee:
	// a destination registered under the old payee does not travel
	// with the instruction.
	if err := l.validatePayoutDestinationLocked(newPayeeAccount, p.Destination); err != nil {
		return PayoutInstruction{}, err
	}
	if newPayeeAccount == p.PayeeAccount {
		return p, nil
	}
	previous := p.PayeeAccount
	p.PayeeAccount = newPayeeAccount
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_payee_account_amended",
		Actor:         "AmendPayoutPayeeAccount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{previous, newPayeeAccount},
		Details: map[string]any{
			"payout_id":              id,
			"previous_payee_account": string(previous),
			"payee_account":          string(newPayeeAccount),
		},
	})
	return p, nil
}
