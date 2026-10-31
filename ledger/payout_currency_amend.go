// Payout instruction currency amendment (LG-277).
//
// CreatePayout (LG-140) fixes a payout instruction's currency at draft
// time — the ISO 4217 code the amount is denominated in, defaulting to
// USD when the draft names none — and SubmitPayout journals the
// clearing leg in that currency. An operator who drafted against the
// wrong currency — a USD instruction that should have been EUR, a
// stale quote's denomination — had no way to correct it short of
// cancelling the draft and drafting a fresh one. A fresh instruction
// is a new instruction: a new ID, a new UETR, and a new audit trail,
// while the cancelled draft stays on the books as noise. This module
// is the revision path: an operator corrects a draft instruction's
// currency in place, before any approval has been given and before any
// money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a draft instruction may be amended. An unknown ID fails
//     with ErrPayoutNotFound. Any non-draft status — approved,
//     submitted, settled, failed, cancelled, returned, or expired —
//     fails with ErrPayoutInvalidTransition: once the instruction
//     has been approved, the approved currency is the currency the
//     checker signed off on, and changing it behind the approval
//     would void the maker-checker separation; once submitted, the
//     clearing leg is already journaled in the original currency.
//     The state gate runs before the currency validation and the
//     no-op check, following the invoice-memo precedent (LG-272): a
//     departed instruction rejects even a same-value amendment.
//   - The new currency must be exactly three ASCII uppercase letters
//     (ISO 4217 alpha-3, e.g. "EUR"), otherwise ErrInvalidCurrency
//     and the original currency is untouched. Unlike normalizeCurrency,
//     an empty code does NOT fall back to the default currency here:
//     an amendment names the currency explicitly, so "" is malformed,
//     not "USD". The gate is the same one AmendInvoiceCurrency
//     (LG-276) applies, and the same one CreatePayout applies at
//     draft time (via normalizeCurrency) — a currency that could not
//     have been drafted cannot be amended to either.
//   - The amendment touches Currency and nothing else: payee, amount,
//     destination, maker, status, UETR, and purpose code are
//     unchanged, and no journal row exists yet to revisit — the draft
//     is off-journal, so amending it has zero bookkeeping impact.
//     The amount in cents is untouched: the amendment re-denominates
//     the instruction as drafted, it does not re-price it — any FX
//     conversion belongs to the payment, not the instruction.
//     SubmitPayout reads the live currency when it journals the
//     clearing leg (payout.go), so the revised currency governs the
//     next submit with no submit-path change: the clearing entry is
//     booked in the new currency, and the audit and outbox events
//     carry it.
//   - Amending to the current currency is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like drafting and cancelling, the amendment is structural: it
//     bumps neither the ledger version nor the audit chain, and no
//     outbox event fires — the draft was already reported when
//     created, and a currency move on an unapproved draft is not a
//     new instruction. The amendment lands one audit event
//     (payout_currency_amended, with the payout ID and the previous
//     and new currencies) so the "who re-denominated the payout, and
//     from what to what" trail is tamper-evident. Payout instructions
//     stay out of snapshots (see payout.go), so an amendment, like the
//     instruction itself, is in-memory operational state and never
//     round-trips.
//
// Orthogonality: CreatePayout owns the initial currency, ApprovePayout
// owns the approval, SubmitPayout owns the clearing earmark (and reads
// the live currency), AmendPayoutAmount (LG-273) owns the draft amount,
// AmendPayoutDestination (LG-274) owns the draft destination, and this
// module owns only the draft currency revision. It is orthogonal to the
// invoice currency amendment (LG-276, which re-denominates a live
// receivable) — this re-denominates an unapproved payout instruction.
package ledger

import (
	"context"
)

// AmendPayoutCurrency revises a draft payout instruction's currency
// in place. An unknown ID fails with ErrPayoutNotFound; a non-draft
// instruction fails with ErrPayoutInvalidTransition; a currency that
// is not three ASCII uppercase letters fails with ErrInvalidCurrency.
// Only Currency changes; the payee, amount, destination, maker,
// status, UETR, and purpose code are untouched, and the new currency
// governs the next SubmitPayout. Amending to the current currency is
// a no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as payout_currency_amended.
func (l *Ledger) AmendPayoutCurrency(id string, newCurrency string) (PayoutInstruction, error) {
	return l.AmendPayoutCurrencyCtx(context.Background(), id, newCurrency)
}

// AmendPayoutCurrencyCtx is AmendPayoutCurrency with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendPayoutCurrencyCtx(ctx context.Context, id string, newCurrency string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if len(newCurrency) != 3 {
		return PayoutInstruction{}, ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if c := newCurrency[i]; c < 'A' || c > 'Z' {
			return PayoutInstruction{}, ErrInvalidCurrency
		}
	}
	if newCurrency == p.Currency {
		return p, nil
	}
	previous := p.Currency
	p.Currency = newCurrency
	l.payouts[id] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "payout_currency_amended",
		Actor:         "AmendPayoutCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":         id,
			"previous_currency": previous,
			"currency":          newCurrency,
		},
	})
	return p, nil
}
