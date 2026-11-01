// Invoice amount amendment (LG-282).
//
// IssueInvoice (LG-148) fixes an invoice's amount at issue; the
// amount is the receivable's face value, and ApplyPayment retires
// invoices FIFO against it — a payment never exceeds the amount it
// lands on. This module is the revision path: an operator corrects a
// live invoice's amount instead of voiding it and issuing a
// replacement. Amending in place matters because a replacement is a
// new invoice — a fresh ID that breaks the payment FIFO lineage and
// the collections trail — while an amendment keeps the same invoice
// and one audit trail from issue to settlement. (The currency is
// untouched: the amendment re-prices the receivable as issued in its
// own currency, it does not re-denominate it — any FX conversion
// belongs to the payment, not the invoice.)
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrInvoiceNotFound. Only an invoice
//     that has received no payment can be amended: it must be open
//     (issued or overdue — see invoiceOpen) and its PaidCents must
//     still be zero. A paid invoice fails with ErrInvoiceHasPayments,
//     the same rejection VoidInvoice returns: the money moved, so the
//     record must stay as-is for the audit trail. A partially paid
//     invoice fails the same way — the cents already received belong
//     to the old amount, and rewriting the face value would re-price
//     the settled part of the receivable. A voided invoice fails with
//     ErrInvoiceNotOpen — VoidInvoice has no rejection for it (voiding
//     a voided invoice is an idempotent no-op), so the amendment
//     carries its own closed-invoice error. The state gate runs
//     before the no-op check, following the fixed-asset precedent
//     (LG-270): a terminal invoice rejects even a same-value
//     amendment.
//   - The new amount reuses the IssueInvoice gate: it must be
//     positive, otherwise ErrNonPositiveAmount and the original amount
//     is untouched.
//   - The amendment touches AmountCents and nothing else: paid
//     amount (always zero here, by the payments gate), currency, due
//     date, memo, account, and status are unchanged. Every live read
//     follows the new amount from the next evaluation: ApplyPayment
//     retires the invoice against the new amount, and the receivables
//     aging buckets its outstanding cents under the new amount.
//   - Amending to the current amount is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like issuing and voiding, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the invoice was already reported when issued, and
//     an amount move is not a new receivable. The amendment lands
//     one audit event (invoice_amount_amended, with the invoice ID
//     and the previous and new amounts) so the "who re-priced the
//     receivable, and from what to what" trail is tamper-evident.
//
// Orthogonality: IssueInvoice owns the initial amount,
// ApplyPayment owns retirement against it, VoidInvoice owns
// cancellation, and this module owns only the amount revision. It is
// orthogonal to the invoice currency amendment (LG-276, which moves
// what currency the money is owed in), the invoice due-date
// amendment (LG-271, which moves when the money is due), and the
// invoice-memo amendment (LG-272, which moves the annotation) — this
// moves only how much money is owed.
package ledger

import (
	"context"
)

// AmendInvoiceAmount revises an open, unpaid invoice's amount in
// place. An unknown ID fails with ErrInvoiceNotFound; a paid or
// partially paid invoice fails with ErrInvoiceHasPayments; a voided
// invoice fails with ErrInvoiceNotOpen; a non-positive amount fails
// with ErrNonPositiveAmount. Only AmountCents changes; the paid
// amount, currency, due date, memo, account, and status are
// untouched, and the new amount governs the next ApplyPayment and
// receivables-aging evaluation. Amending to the current amount is a
// no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as invoice_amount_amended.
func (l *Ledger) AmendInvoiceAmount(id string, newAmountCents int64) (Invoice, error) {
	return l.AmendInvoiceAmountCtx(context.Background(), id, newAmountCents)
}

// AmendInvoiceAmountCtx is AmendInvoiceAmount with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendInvoiceAmountCtx(ctx context.Context, id string, newAmountCents int64) (Invoice, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	inv, ok := l.invoices[id]
	if !ok {
		return Invoice{}, ErrInvoiceNotFound
	}
	if !invoiceOpen(inv.Status) {
		if inv.Status == InvoicePaid {
			return Invoice{}, ErrInvoiceHasPayments
		}
		return Invoice{}, ErrInvoiceNotOpen
	}
	// The amount is locked the moment money lands on the invoice:
	// a partially paid invoice is still open, but its face value
	// can no longer move — the settled cents were priced against
	// the old amount.
	if inv.PaidCents > 0 {
		return Invoice{}, ErrInvoiceHasPayments
	}
	if newAmountCents <= 0 {
		return Invoice{}, ErrNonPositiveAmount
	}
	if newAmountCents == inv.AmountCents {
		return inv, nil
	}
	previous := inv.AmountCents
	inv.AmountCents = newAmountCents
	l.invoices[id] = inv
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_amount_amended",
		Actor:         "AmendInvoiceAmount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"status":                string(inv.Status),
			"previous_amount_cents": previous,
			"amount_cents":          newAmountCents,
		},
	})
	return inv, nil
}
