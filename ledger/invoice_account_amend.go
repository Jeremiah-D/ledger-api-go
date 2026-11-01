// Invoice payer-account amendment (LG-283).
//
// IssueInvoice (LG-148) fixes an invoice's payer account at issue;
// the account is who owes the money, and ApplyPayment retires
// invoices only against payments from that account — a payment from
// another account never touches it. This module is the revision
// path: an operator corrects a live invoice's payer account instead
// of voiding it and issuing a replacement. Amending in place matters
// because a replacement is a new invoice — a fresh ID that breaks
// the payment FIFO lineage and the collections trail — while an
// amendment keeps the same invoice, the same due date, and one audit
// trail from issue to settlement. (The amount and currency are
// untouched: the amendment re-points the receivable at a different
// payer, it does not re-price or re-denominate it.)
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
//     to the old payer, and moving the invoice would hang another
//     payer's money on the new account's name. A voided invoice
//     fails with ErrInvoiceNotOpen — VoidInvoice has no rejection
//     for it (voiding a voided invoice is an idempotent no-op), so
//     the amendment carries its own closed-invoice error. The state
//     gate runs before the no-op check, following the fixed-asset
//     precedent (LG-270): a terminal invoice rejects even a
//     same-value amendment.
//   - The new account reuses the IssueInvoice gate: it must be
//     non-empty, otherwise ErrInvalidInvoice and the original
//     account is untouched. Issue time does not check that the
//     account exists, and neither does the amendment.
//   - The amendment touches Account and nothing else: amount, paid
//     amount (always zero here, by the payments gate), currency, due
//     date, memo, and status are unchanged. The per-account invoice
//     index moves with it — the invoice ID leaves the old account's
//     list and joins the new account's, so the index stays identical
//     to a rebuild from the invoice map. Every live read follows
//     the new account from the next evaluation: ApplyPayment from
//     the new account retires the invoice, a payment from the old
//     account no longer sees it, ListInvoices filters by it, and
//     the dunning scan picks it up under the new account.
//   - Amending to the current account is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like issuing and voiding, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the invoice was already reported when issued, and
//     an account move is not a new receivable. The amendment lands
//     one audit event (invoice_account_amended, with the invoice ID
//     and the previous and new accounts, and both accounts in the
//     event's Accounts — the event concerns the old payer's trail as
//     much as the new one's, following the payout payee-account
//     amendment precedent) so the "who moved the receivable, and
//     from whom to whom" trail is tamper-evident.
//
// Orthogonality: IssueInvoice owns the initial account,
// ApplyPayment owns retirement against it, VoidInvoice owns
// cancellation, and this module owns only the account revision. It is
// orthogonal to the invoice amount amendment (LG-282, which moves
// how much money is owed), the invoice currency amendment (LG-276,
// which moves what currency the money is owed in), the invoice
// due-date amendment (LG-271, which moves when the money is due),
// and the invoice-memo amendment (LG-272, which moves the
// annotation) — this moves only who owes the money.
package ledger

import (
	"context"
)

// AmendInvoiceAccount revises an open, unpaid invoice's payer account
// in place. An unknown ID fails with ErrInvoiceNotFound; a paid or
// partially paid invoice fails with ErrInvoiceHasPayments; a voided
// invoice fails with ErrInvoiceNotOpen; an empty account fails with
// ErrInvalidInvoice. Only Account changes; the amount, paid amount,
// currency, due date, memo, and status are untouched, the
// per-account invoice index moves with the invoice, and the new
// account governs the next ApplyPayment, ListInvoices filter, and
// dunning evaluation. Amending to the current account is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as invoice_account_amended.
func (l *Ledger) AmendInvoiceAccount(id string, newAccount AccountID) (Invoice, error) {
	return l.AmendInvoiceAccountCtx(context.Background(), id, newAccount)
}

// AmendInvoiceAccountCtx is AmendInvoiceAccount with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendInvoiceAccountCtx(ctx context.Context, id string, newAccount AccountID) (Invoice, error) {
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
	// The payer is locked the moment money lands on the invoice:
	// a partially paid invoice is still open, but its payer can no
	// longer move — the settled cents belong to the old payer.
	if inv.PaidCents > 0 {
		return Invoice{}, ErrInvoiceHasPayments
	}
	if newAccount == "" {
		return Invoice{}, ErrInvalidInvoice
	}
	if newAccount == inv.Account {
		return inv, nil
	}
	previous := inv.Account
	inv.Account = newAccount
	l.invoices[id] = inv
	l.moveInvoiceIndexLocked(id, previous, newAccount)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_account_amended",
		Actor:         "AmendInvoiceAccount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{previous, newAccount},
		Details: map[string]any{
			"status":           string(inv.Status),
			"previous_account": string(previous),
			"account":          string(newAccount),
		},
	})
	return inv, nil
}

// moveInvoiceIndexLocked moves one invoice ID from the old account's
// per-account index list to the new account's. Callers must hold the
// write lock. An emptied old-account list is dropped, so the index
// stays identical to a rebuild from the invoice map
// (rebuildInvoiceIndexLocked).
func (l *Ledger) moveInvoiceIndexLocked(id string, previous, newAccount AccountID) {
	kept := l.invoicesByAccount[previous][:0]
	for _, listed := range l.invoicesByAccount[previous] {
		if listed != id {
			kept = append(kept, listed)
		}
	}
	if len(kept) == 0 {
		delete(l.invoicesByAccount, previous)
	} else {
		l.invoicesByAccount[previous] = kept
	}
	l.invoicesByAccount[newAccount] = append(l.invoicesByAccount[newAccount], id)
}
