// Invoice due-date amendment (LG-271).
//
// IssueInvoice (LG-148) fixes an invoice's due date at issue; the
// sweep, the receivables aging, and the dunning escalation all read
// that date live. This module is the revision path: an operator moves
// a live invoice's due date instead of voiding it and issuing a
// replacement. Amending in place matters because a replacement is a
// new invoice — a fresh ID that breaks the payment FIFO lineage and
// the collections trail — while an amendment keeps the same invoice,
// the same applied payments, and one audit trail from issue to
// settlement.
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
//   - The new due date reuses the IssueInvoice validation: it must be
//     present and not before IssuedAt (equal to IssuedAt is allowed,
//     as at issue), otherwise ErrInvalidInvoice and the original due
//     date is untouched.
//   - The amendment touches DueDate and nothing else: amount, paid
//     amount, status, memo, currency, and account are unchanged. In
//     particular an overdue invoice keeps its overdue *label* until a
//     payment retires it — the sweep only moves issued/partial
//     invoices to overdue and never moves one back — but every live
//     read follows the new date from the next evaluation: the sweep
//     skips an issued invoice whose due date moved past now, the
//     receivables aging buckets an extended overdue invoice as
//     current again, and the dunning lots drop it (dpd <= 0). So
//     extending an overdue invoice's due date returns it to
//     "not overdue" in effect, without rewriting its status.
//   - Amending to the current due date is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like issuing and voiding, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the invoice was already reported when issued, and
//     a due-date move is not a new receivable. The amendment lands
//     one audit event (invoice_due_date_amended, with the invoice ID
//     and the previous and new due dates) so the "who moved the due
//     date, and from what to what" trail is tamper-evident.
//
// Orthogonality: IssueInvoice owns the initial due date,
// SweepOverdueInvoices owns the overdue transition, ApplyPayment owns
// retirement, VoidInvoice owns cancellation, and this module owns
// only the due-date revision. It is orthogonal to the invoice-memo
// amendment (LG-272, which moves the annotation) — this moves only
// when the money is due.
package ledger

import (
	"context"
	"time"
)

// AmendInvoiceDueDate revises an open invoice's due date in place.
// An unknown ID fails with ErrInvoiceNotFound; a paid invoice fails
// with ErrInvoiceHasPayments; a voided invoice fails with
// ErrInvoiceNotOpen; a zero due date or one before IssuedAt fails
// with ErrInvalidInvoice. Only DueDate changes; the amount, paid
// amount, status, memo, currency, and account are untouched, and the
// new due date governs from the next aging/dunning/sweep evaluation.
// Amending to the current due date is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as invoice_due_date_amended.
func (l *Ledger) AmendInvoiceDueDate(id string, newDueDate time.Time) (Invoice, error) {
	return l.AmendInvoiceDueDateCtx(context.Background(), id, newDueDate)
}

// AmendInvoiceDueDateCtx is AmendInvoiceDueDate with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendInvoiceDueDateCtx(ctx context.Context, id string, newDueDate time.Time) (Invoice, error) {
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
	if newDueDate.IsZero() || newDueDate.Before(inv.IssuedAt) {
		return Invoice{}, ErrInvalidInvoice
	}
	if newDueDate.Equal(inv.DueDate) {
		return inv, nil
	}
	previous := inv.DueDate
	inv.DueDate = newDueDate
	l.invoices[id] = inv
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_due_date_amended",
		Actor:         "AmendInvoiceDueDate",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"status":            string(inv.Status),
			"previous_due_date": previous.UTC().Format(time.RFC3339),
			"due_date":          newDueDate.UTC().Format(time.RFC3339),
		},
	})
	return inv, nil
}
