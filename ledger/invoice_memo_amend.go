// Invoice memo amendment (LG-272).
//
// IssueInvoice (LG-148) fixes an invoice's memo at issue; the memo is
// the operator's annotation on the receivable — an order reference, a
// collections note, a reason the terms changed — and it travels with
// the invoice through the collections workflow. This module is the
// revision path: an operator corrects a live invoice's memo instead of
// voiding it and issuing a replacement. Amending in place matters
// because a replacement is a new invoice — a fresh ID that breaks the
// payment FIFO lineage and the collections trail — while an amendment
// keeps the same invoice, the same applied payments, and one audit
// trail from issue to settlement.
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
//   - The new memo reuses the creation gate (checkMemoLength, the
//     same 255 UTF-8 character cap IssueInvoice enforces): an overlong
//     memo fails with ErrMemoTooLong and the original memo is
//     untouched. An empty memo is valid — it clears the annotation.
//   - The amendment touches Memo and nothing else: amount, paid
//     amount, status, due date, currency, and account are unchanged.
//     The memo is annotation only: no aging bucket, no dunning lot,
//     and no sweep reads it, so a memo move has no live effect beyond
//     what the next reader of the invoice sees.
//   - Amending to the current memo is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Like issuing and voiding, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the invoice was already reported when issued, and
//     a memo move is not a new receivable. The amendment lands one
//     audit event (invoice_memo_amended, with the invoice ID and the
//     previous and new memos) so the "who changed the annotation, and
//     from what to what" trail is tamper-evident.
//
// Orthogonality: IssueInvoice owns the initial memo,
// SweepOverdueInvoices owns the overdue transition, ApplyPayment owns
// retirement, VoidInvoice owns cancellation, and this module owns only
// the memo revision. It is orthogonal to the invoice due-date
// amendment (LG-271, which moves when the money is due) — this moves
// only the annotation.
package ledger

import (
	"context"
)

// AmendInvoiceMemo revises an open invoice's memo in place.
// An unknown ID fails with ErrInvoiceNotFound; a paid invoice fails
// with ErrInvoiceHasPayments; a voided invoice fails with
// ErrInvoiceNotOpen; an overlong memo fails with ErrMemoTooLong.
// Only Memo changes; the amount, paid amount, status, due date,
// currency, and account are untouched. Amending to the current memo
// is a no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as invoice_memo_amended.
func (l *Ledger) AmendInvoiceMemo(id string, newMemo string) (Invoice, error) {
	return l.AmendInvoiceMemoCtx(context.Background(), id, newMemo)
}

// AmendInvoiceMemoCtx is AmendInvoiceMemo with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendInvoiceMemoCtx(ctx context.Context, id string, newMemo string) (Invoice, error) {
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
	if err := checkMemoLength(newMemo); err != nil {
		return Invoice{}, err
	}
	if newMemo == inv.Memo {
		return inv, nil
	}
	previous := inv.Memo
	inv.Memo = newMemo
	l.invoices[id] = inv
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_memo_amended",
		Actor:         "AmendInvoiceMemo",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"status":        string(inv.Status),
			"previous_memo": previous,
			"memo":          newMemo,
		},
	})
	return inv, nil
}
