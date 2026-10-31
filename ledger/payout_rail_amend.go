// Payout instruction settlement-rail amendment (LG-278).
//
// CreatePayout (LG-140) fixes a payout instruction's settlement rail
// at draft time — one of fednow, rtp, sepa, swift, gpi, ach, or empty
// for "no rail stated" (see sla.go, LG-185/LG-187) — and SubmitPayout
// opens the settlement-SLA watch on the live rail, defaulting an
// empty rail to gpi (payout.go). An operator who drafted against the
// wrong rail — a SEPA instruction that should have ridden FedNow, a
// rail chosen before the payee's routing changed — had no way to
// correct it short of cancelling the draft and drafting a fresh one.
// A fresh instruction is a new instruction: a new ID, a new UETR, and
// a new audit trail, while the cancelled draft stays on the books as
// noise. This module is the revision path: an operator corrects a
// draft instruction's rail in place, before any approval has been
// given and before any money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved rail is the rail the checker
//     signed off on, and changing it behind the approval would void
//     the maker-checker separation; once submitted, the SLA watch is
//     already open on the original rail. The state gate runs before
//     the rail validation and the no-op check, following the
//     invoice-memo precedent (LG-272): a departed instruction rejects
//     even a same-value amendment.
//   - The new rail reuses the CreatePayout gate exactly: normalizeRail
//     (sla.go) canonicalizes it — trimmed, lowercased — and a name
//     outside the SLA vocabulary fails with ErrInvalidRail and the
//     original rail is untouched. Empty stays empty ("no rail
//     stated"), so an amendment may clear the rail back to
//     unspecified, and the next submit then defaults to gpi as usual.
//     A rail that could not have been drafted cannot be amended to
//     either.
//   - The amendment touches Rail and nothing else: payee, amount,
//     currency, destination, maker, status, UETR, and purpose code are
//     unchanged, and no journal row exists yet to revisit — the draft
//     is off-journal, so amending it has zero bookkeeping impact.
//     SubmitPayout reads the live rail when it opens the SLA watch
//     (payout.go), so the revised rail governs the next submit with no
//     submit-path change: the watch opens on the new rail, and an
//     empty rail still defaults to gpi.
//   - Amending to the current rail is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing. The comparison runs on the normalized rail, so
//     amending "swift" to "SWIFT" is a no-op, not a change.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a rail move on an unapproved draft is not a new
//     instruction. The amendment lands one audit event
//     (payout_rail_amended, with the payout ID and the previous and
//     new rails) so the "who re-routed the payout, and from what to
//     what" trail is tamper-evident. Payout instructions stay out of
//     snapshots (see payout.go), so an amendment, like the instruction
//     itself, is in-memory operational state and never round-trips.
//
// Orthogonality: CreatePayout owns the initial rail, ApprovePayout
// owns the approval, SubmitPayout owns the clearing earmark (and
// opens the SLA watch on the live rail), AmendPayoutAmount (LG-273)
// owns the draft amount, AmendPayoutDestination (LG-274) owns the
// draft destination, AmendPayoutCurrency (LG-277) owns the draft
// currency, and this module owns only the draft settlement-rail
// revision. It is orthogonal to the SLA mechanism itself (LG-185/
// LG-187, which defines the rail vocabulary and the watch) — this
// changes which rail a draft rides, not how any rail is watched.
package ledger

import (
	"context"
)

// AmendPayoutRail revises a draft payout instruction's settlement rail
// in place. An unknown ID fails with ErrPayoutNotFound; a non-draft
// instruction fails with ErrPayoutInvalidTransition; a rail name
// outside the SLA vocabulary fails with ErrInvalidRail. Only Rail
// changes; the payee, amount, currency, destination, maker, status,
// UETR, and purpose code are untouched, and the new rail governs the
// next SubmitPayout's SLA watch. Amending to the current rail is a
// no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as payout_rail_amended.
func (l *Ledger) AmendPayoutRail(id string, newRail string) (PayoutInstruction, error) {
	return l.AmendPayoutRailCtx(context.Background(), id, newRail)
}

// AmendPayoutRailCtx is AmendPayoutRail with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendPayoutRailCtx(ctx context.Context, id string, newRail string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	rail, err := normalizeRail(newRail)
	if err != nil {
		return PayoutInstruction{}, err
	}
	if rail == p.Rail {
		return p, nil
	}
	previous := p.Rail
	p.Rail = rail
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_rail_amended",
		Actor:         "AmendPayoutRail",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":     id,
			"previous_rail": previous,
			"rail":          rail,
		},
	})
	return p, nil
}
