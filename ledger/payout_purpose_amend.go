// Payout instruction purpose-code amendment (LG-280).
//
// CreatePayout (LG-140) fixes a payout instruction's ISO 20022
// business purpose at draft time — one of the admitted
// ExternalPurpose1Code entries (see purpose.go, LG-180) — and the
// pacs.008 outbound export stamps it on the wire as <Purp><Cd>
// (pacs008_export.go). An operator who drafted against the wrong
// purpose — a supplier payment filed as a salary, or a draft whose
// purpose was only decided after drafting — had no way to correct
// it short of cancelling the draft and drafting a fresh one. A fresh
// instruction is a new instruction: a new ID, a new UETR, and a new
// audit trail, while the cancelled draft stays on the books as noise.
// This module is the revision path: an operator corrects a draft
// instruction's purpose code in place, before any approval has been
// given and before any money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved purpose is the purpose the
//     checker signed off on, and changing it behind the approval
//     would void the maker-checker separation; once submitted, the
//     clearing leg is already journaled. The state gate runs before
//     the purpose validation and the no-op check, following the
//     invoice-memo precedent (LG-272): a departed instruction
//     rejects even a same-value amendment.
//   - The new purpose code reuses the CreatePayout gate exactly:
//     checkPurposeCode (purpose.go) admits an empty code — "no
//     purpose stated", which clears a previously stated purpose —
//     and rejects anything outside the admitted ISO 20022 subset
//     with ErrInvalidPurposeCode; the original code is untouched.
//     The match is exact and case-sensitive: a lowercase near-miss
//     is not a code.
//   - The amendment touches PurposeCode and nothing else: amount,
//     currency, destination, payee account, maker, status, UETR,
//     and the idempotency key are unchanged, and no journal row
//     exists yet to revisit — the draft is off-journal, so amending
//     it has zero bookkeeping impact. The clearing leg carries no
//     purpose code (SubmitPayout, payout.go), so there is no
//     clearing entry to restate; the revised code governs the next
//     pacs.008 export with no export-path change, because the
//     export reads the live PurposeCode at export time
//     (ExportPacs008, pacs008_export.go).
//   - Amending to the current purpose code is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a purpose-code move on an unapproved draft is
//     not a new instruction. The amendment lands one audit event
//     (payout_purpose_code_amended, with the payout ID and the
//     previous and new purpose codes) so the "who changed the
//     purpose, and from what to what" trail is tamper-evident.
//     Payout instructions stay out of snapshots (see payout.go), so
//     an amendment, like the instruction itself, is in-memory
//     operational state and never round-trips.
//
// Orthogonality: CreatePayout owns the initial purpose code,
// ApprovePayout owns the approval, SubmitPayout owns the clearing
// earmark (which carries no purpose code), ExportPacs008 owns the
// wire projection (and reads the live code), AmendPayoutAmount
// (LG-273) owns the draft amount, AmendPayoutDestination (LG-274)
// owns the draft destination, AmendPayoutCurrency (LG-277) owns
// the draft currency, AmendPayoutRail (LG-278) owns the draft
// settlement rail, AmendPayoutPayeeAccount (LG-279) owns the draft
// payee account, and this module owns only the draft purpose-code
// revision. It is orthogonal to the purpose-code mechanism itself
// (LG-180, which admits the ISO 20022 subset) — this revises only
// which admitted code a draft carries.
package ledger

import (
	"context"
)

// AmendPayoutPurposeCode revises a draft payout instruction's ISO
// 20022 purpose code in place. An unknown ID fails with
// ErrPayoutNotFound; a non-draft instruction fails with
// ErrPayoutInvalidTransition; a code outside the admitted ISO 20022
// subset fails with ErrInvalidPurposeCode. An empty code clears the
// purpose ("no purpose stated"). Only PurposeCode changes; the
// amount, currency, destination, payee account, maker, status, UETR,
// and idempotency key are untouched. Amending to the current purpose
// code is a no-op. Amending is off-journal — it does not bump the
// ledger version — but it is audited as
// payout_purpose_code_amended.
func (l *Ledger) AmendPayoutPurposeCode(id string, newPurposeCode string) (PayoutInstruction, error) {
	return l.AmendPayoutPurposeCodeCtx(context.Background(), id, newPurposeCode)
}

// AmendPayoutPurposeCodeCtx is AmendPayoutPurposeCode with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendPayoutPurposeCodeCtx(ctx context.Context, id string, newPurposeCode string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if err := checkPurposeCode(newPurposeCode); err != nil {
		return PayoutInstruction{}, err
	}
	if newPurposeCode == p.PurposeCode {
		return p, nil
	}
	previous := p.PurposeCode
	p.PurposeCode = newPurposeCode
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_purpose_code_amended",
		Actor:         "AmendPayoutPurposeCode",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":             id,
			"previous_purpose_code": previous,
			"purpose_code":          newPurposeCode,
		},
	})
	return p, nil
}
