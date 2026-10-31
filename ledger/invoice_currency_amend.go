// Invoice currency amendment (LG-276).
//
// IssueInvoice (LG-148) fixes an invoice's currency at issue; the
// amount is denominated in that currency, and ApplyPayment retires
// invoices only in the currency it is paid in — a USD payment never
// touches a EUR invoice. This module is the revision path: an operator
// corrects a live invoice's currency instead of voiding it and issuing
// a replacement. Amending in place matters because a replacement is a
// new invoice — a fresh ID that breaks the payment FIFO lineage and
// the collections trail — while an amendment keeps the same invoice,
// the same applied payments, and one audit trail from issue to
// settlement. (The amount in cents is untouched: the amendment
// re-denominates the receivable as issued, it does not re-price it —
// any FX conversion belongs to the payment, not the invoice.)
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrInvoiceNotFound. Only an open
//     invoice (issued, partially paid, or overdue — see
//     invoiceOpen) can be amended. A paid invoice fails with
//     ErrInvoiceHasPayments, the same rejection VoidInvoice returns:
//     the money moved, so the record must stay as-is for the audit
//     trail. A voided invoice fails with ErrInvoiceNotOpen —
//     VoidInvoice has no rejection for it (voiding a voided invoice
//     is an idempotent no-op), so the amendment carries its own
//     closed-invoice error. The state gate runs before the no-op
//     check, following the fixed-asset precedent (LG-270): a terminal
//     invoice rejects even a same-value amendment.
//   - The new currency must be exactly three ASCII uppercase letters
//     (ISO 4217 alpha-3, e.g. "EUR"), otherwise ErrInvalidCurrency
//     and the original currency is untouched. Unlike normalizeCurrency,
//     an empty code does NOT fall back to the default currency here:
//     an amendment names the currency explicitly, so "" is malformed,
//     not "USD".
//   - The amendment touches Currency and nothing else: amount, paid
//     amount, status, due date, memo, and account are unchanged. In
//     particular a partially paid invoice keeps its paid cents and its
//     partial status — the payment already applied stays applied, in
//     the old currency's cents, and the remaining balance is now owed
//     in the new currency. Every live read follows the new currency
//     from the next evaluation: ListInvoices filters by it,
//     ApplyPayment in the new currency retires the invoice, a payment
//     in the old currency no longer sees it, and the receivables aging
//     buckets its outstanding cents under the new currency.
//   - Amending to the current currency is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like issuing and voiding, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the invoice was already reported when issued, and
//     a currency move is not a new receivable. The amendment lands
//     one audit event (invoice_currency_amended, with the invoice ID
//     and the previous and new currencies) so the "who re-denominated
//     the receivable, and from what to what" trail is tamper-evident.
//
// Orthogonality: IssueInvoice owns the initial currency,
// SweepOverdueInvoices owns the overdue transition, ApplyPayment owns
// retirement, VoidInvoice owns cancellation, and this module owns only
// the currency revision. It is orthogonal to the invoice due-date
// amendment (LG-271, which moves when the money is due) and the
// invoice-memo amendment (LG-272, which moves the annotation) — this
// moves only what currency the money is owed in.
package ledger

import (
	"context"
)

// AmendInvoiceCurrency revises an open invoice's currency in place.
// An unknown ID fails with ErrInvoiceNotFound; a paid invoice fails
// with ErrInvoiceHasPayments; a voided invoice fails with
// ErrInvoiceNotOpen; a currency that is not three ASCII uppercase
// letters fails with ErrInvalidCurrency. Only Currency changes; the
// amount, paid amount, status, due date, memo, and account are
// untouched, and the new currency governs the next ListInvoices
// filter, ApplyPayment, and receivables-aging evaluation. Amending to
// the current currency is a no-op. Amending is off-journal — it does
// not bump the ledger version — but it is audited as
// invoice_currency_amended.
func (l *Ledger) AmendInvoiceCurrency(id string, newCurrency string) (Invoice, error) {
	return l.AmendInvoiceCurrencyCtx(context.Background(), id, newCurrency)
}

// AmendInvoiceCurrencyCtx is AmendInvoiceCurrency with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendInvoiceCurrencyCtx(ctx context.Context, id string, newCurrency string) (Invoice, error) {
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
	if len(newCurrency) != 3 {
		return Invoice{}, ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if c := newCurrency[i]; c < 'A' || c > 'Z' {
			return Invoice{}, ErrInvalidCurrency
		}
	}
	if newCurrency == inv.Currency {
		return inv, nil
	}
	previous := inv.Currency
	inv.Currency = newCurrency
	l.invoices[id] = inv
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_currency_amended",
		Actor:         "AmendInvoiceCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"status":            string(inv.Status),
			"previous_currency": previous,
			"currency":          newCurrency,
		},
	})
	return inv, nil
}
