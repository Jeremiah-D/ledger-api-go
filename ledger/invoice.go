package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// 应收发票生命周期 (LG-148, 支付 fintech).
//
// An invoice is an off-journal receivable: the operator's claim that a
// payer account owes a fixed amount in a currency by a due date.
// Invoices are operational state, not books — they never appear in
// entries, the audit chain, or the version counter — but they drive the
// collections workflow:
//
//   - IssueInvoice opens an invoice (status issued).
//   - ApplyPayment retires open invoices FIFO: the oldest invoice is
//     paid first, a partially covered invoice becomes partial, a fully
//     covered one becomes paid. Any amount beyond the open balance is
//     returned as a remainder (an overpayment), never silently kept.
//   - SweepOverdueInvoices moves issued/partial invoices past their due
//     date to overdue. The sweep is the operator's "到期复核" for
//     receivables.
//   - VoidInvoice cancels an invoice that never received a payment.
//     Invoices with any payment applied cannot be voided: money moved,
//     so the record must stay for the audit trail.
//
// Dunning 衔接: open invoices past due feed the collections escalation
// directly (see dunningAgeLocked in dunning.go) — an overdue invoice is a
// FIFO lot aged from its due date, so a (account, currency) with a
// dunning config escalates on an overdue invoice even when the journal
// shows no outstanding receivable. The invoice sweep does not duplicate
// the escalation machinery; EscalateDunning picks the invoices up on its
// next scan.
//
// Structural state, like holds: invoices round-trip through
// disaster-recovery snapshots.

// InvoiceStatus is the lifecycle state of an invoice.
type InvoiceStatus string

// Invoice states.
const (
	InvoiceIssued  InvoiceStatus = "issued"
	InvoicePartial InvoiceStatus = "partial"
	InvoicePaid    InvoiceStatus = "paid"
	InvoiceOverdue InvoiceStatus = "overdue"
	InvoiceVoided  InvoiceStatus = "voided"
)

// Invoice is one issued receivable: the payer account, the currency,
// the amount, and the due date. PaidCents tracks the FIFO-applied
// payments; Status is derived from PaidCents and the due-date sweep.
type Invoice struct {
	ID          string        `json:"id"`
	Account     AccountID     `json:"account"`
	Currency    string        `json:"currency"`
	AmountCents int64         `json:"amount_cents"`
	PaidCents   int64         `json:"paid_cents"`
	IssuedAt    time.Time     `json:"issued_at"`
	DueDate     time.Time     `json:"due_date"`
	Status      InvoiceStatus `json:"status"`
	Memo        string        `json:"memo,omitempty"`
}

// PaymentApplication is one invoice's share of an ApplyPayment call:
// how many cents landed on it and the status it moved to.
type PaymentApplication struct {
	InvoiceID    string        `json:"invoice_id"`
	AppliedCents int64         `json:"applied_cents"`
	StatusAfter  InvoiceStatus `json:"status_after"`
}

// Invoice validation and lifecycle errors.
var (
	// ErrInvoiceNotFound is returned by GetInvoice/VoidInvoice for an
	// unknown invoice ID. 404-class.
	ErrInvoiceNotFound = errors.New("ledger: invoice not found")
	// ErrDuplicateInvoiceID is returned by IssueInvoice when the ID is
	// already issued. 409-class: the ID namespace is per ledger.
	ErrDuplicateInvoiceID = errors.New("ledger: invoice ID already exists")
	// ErrInvalidInvoice is returned by IssueInvoice for a malformed
	// invoice: an empty ID or account, a bad currency code, a missing
	// due date, a due date before the issue date, or an overlong memo.
	// 400-class field validation.
	ErrInvalidInvoice = errors.New("ledger: invalid invoice")
	// ErrInvoiceHasPayments is returned by VoidInvoice when the invoice
	// already has payments applied (PaidCents > 0) or is fully paid.
	// 422-class: the request is well-formed, the money just moved — a
	// paid invoice cannot be erased, only the audit trail keeps it.
	// AmendInvoiceDueDate (LG-271) reuses it for a paid invoice: the
	// record must stay as-is.
	ErrInvoiceHasPayments = errors.New("ledger: invoice has payments and cannot be voided")
	// ErrInvoiceNotOpen is returned by AmendInvoiceDueDate (LG-271)
	// for a voided invoice: the receivable is closed, so its due date
	// cannot move. 422-class: the request is well-formed, the invoice
	// is just closed. (VoidInvoice has no rejection for a voided
	// invoice — voiding one is an idempotent no-op — so the amendment
	// carries its own closed-invoice error.)
	ErrInvoiceNotOpen = errors.New("ledger: invoice is not open")
)

// validInvoiceStatus reports whether s is a status the register can
// hold (used by the snapshot import validator).
func validInvoiceStatus(s InvoiceStatus) bool {
	switch s {
	case InvoiceIssued, InvoicePartial, InvoicePaid, InvoiceOverdue, InvoiceVoided:
		return true
	}
	return false
}

// invoiceOpen reports whether the invoice still has an outstanding
// balance to collect: issued, partially paid, or overdue.
func invoiceOpen(s InvoiceStatus) bool {
	return s == InvoiceIssued || s == InvoicePartial || s == InvoiceOverdue
}

// IssueInvoice opens a new receivable. The ID must be unique per
// ledger; the account must be non-empty; the amount must be positive;
// the currency is normalized (empty means the default); the due date
// must be present and not before the issue date (a zero IssuedAt is
// stamped with the current time). The invoice opens as issued with
// zero paid. Issuing does not bump the journal version: no money moved.
func (l *Ledger) IssueInvoice(inv Invoice) (Invoice, error) {
	return l.IssueInvoiceCtx(context.Background(), inv)
}

// IssueInvoiceCtx is IssueInvoice with per-call audit actor attribution
// (LG-73).
func (l *Ledger) IssueInvoiceCtx(ctx context.Context, inv Invoice) (Invoice, error) {
	if inv.ID == "" || inv.Account == "" {
		return Invoice{}, ErrInvalidInvoice
	}
	if inv.AmountCents <= 0 {
		return Invoice{}, ErrNonPositiveAmount
	}
	cur, err := normalizeCurrency(inv.Currency)
	if err != nil {
		return Invoice{}, err
	}
	inv.Currency = cur
	if inv.IssuedAt.IsZero() {
		inv.IssuedAt = time.Now().UTC()
	}
	if inv.DueDate.IsZero() || inv.DueDate.Before(inv.IssuedAt) {
		return Invoice{}, ErrInvalidInvoice
	}
	if err := checkMemoLength(inv.Memo); err != nil {
		return Invoice{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.invoices[inv.ID]; exists {
		return Invoice{}, ErrDuplicateInvoiceID
	}
	inv.Status = InvoiceIssued
	inv.PaidCents = 0
	l.invoices[inv.ID] = inv
	l.invoicesByAccount[inv.Account] = append(l.invoicesByAccount[inv.Account], inv.ID)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_issued",
		Actor:         "IssueInvoice",
		TraceID:       inv.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"currency":     inv.Currency,
			"amount_cents": inv.AmountCents,
			"due_date":     inv.DueDate.UTC().Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxInvoiceIssued,
		Account:     inv.Account,
		AmountCents: inv.AmountCents,
		Currency:    inv.Currency,
		Details:     map[string]any{"invoice_id": inv.ID},
	})
	return inv, nil
}

// GetInvoice returns the invoice with the given ID. The second return
// value is false for unknown IDs.
func (l *Ledger) GetInvoice(id string) (Invoice, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	inv, ok := l.invoices[id]
	return inv, ok
}

// ListInvoices returns every invoice for the account, optionally
// narrowed to one currency (empty currency means all currencies), in
// (IssuedAt, ID) order. An empty account lists every invoice on the
// ledger.
func (l *Ledger) ListInvoices(account AccountID, currency string) []Invoice {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Invoice, 0)
	for _, inv := range l.invoices {
		if account != "" && inv.Account != account {
			continue
		}
		if currency != "" && inv.Currency != currency {
			continue
		}
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssuedAt.Equal(out[j].IssuedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].IssuedAt.Before(out[j].IssuedAt)
	})
	return out
}

// ApplyPayment retires the account's open invoices FIFO with amountCents:
// the oldest invoice (by IssuedAt, then ID) is paid first, a partially
// covered invoice becomes partial, a fully covered one becomes paid.
// Invoices already overdue are collected like any open invoice — paying
// an overdue invoice clears it. The returned applications are in FIFO
// order; any amount beyond the total open balance is returned as the
// remainder (an overpayment), never silently kept. The call is a no-op
// returning the full amount as remainder when no invoice is open.
//
// Applying a payment does not bump the journal version: the invoice
// register is operational state, and the money movement itself books
// through the ordinary Post/Transfer paths. The payment application is
// recorded as an "invoice_payment_applied" structured audit event.
func (l *Ledger) ApplyPayment(account AccountID, currency string, amountCents int64, now time.Time) ([]PaymentApplication, int64, error) {
	return l.ApplyPaymentCtx(context.Background(), account, currency, amountCents, now)
}

// ApplyPaymentCtx is ApplyPayment with per-call audit actor attribution
// (LG-73).
func (l *Ledger) ApplyPaymentCtx(ctx context.Context, account AccountID, currency string, amountCents int64, now time.Time) ([]PaymentApplication, int64, error) {
	if account == "" {
		return nil, 0, ErrInvalidInvoice
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return nil, 0, err
	}
	if amountCents <= 0 {
		return nil, 0, ErrNonPositiveAmount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Collect the open invoices FIFO: issued, partial, and overdue all
	// still owe money.
	open := make([]Invoice, 0)
	for _, inv := range l.invoices {
		if inv.Account != account || inv.Currency != cur {
			continue
		}
		if !invoiceOpen(inv.Status) {
			continue
		}
		if inv.AmountCents-inv.PaidCents <= 0 {
			continue
		}
		open = append(open, inv)
	}
	sort.Slice(open, func(i, j int) bool {
		if open[i].IssuedAt.Equal(open[j].IssuedAt) {
			return open[i].ID < open[j].ID
		}
		return open[i].IssuedAt.Before(open[j].IssuedAt)
	})
	applied := make([]PaymentApplication, 0, len(open))
	remaining := amountCents
	appliedIDs := make([]string, 0, len(open))
	for _, inv := range open {
		if remaining <= 0 {
			break
		}
		need := inv.AmountCents - inv.PaidCents
		take := need
		if take > remaining {
			take = remaining
		}
		inv.PaidCents += take
		remaining -= take
		if inv.PaidCents >= inv.AmountCents {
			inv.Status = InvoicePaid
		} else {
			inv.Status = InvoicePartial
		}
		l.invoices[inv.ID] = inv
		applied = append(applied, PaymentApplication{
			InvoiceID:    inv.ID,
			AppliedCents: take,
			StatusAfter:  inv.Status,
		})
		appliedIDs = append(appliedIDs, inv.ID)
		if inv.Status == InvoicePaid {
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxInvoicePaid,
				Account:     account,
				AmountCents: inv.AmountCents,
				Currency:    cur,
				Details:     map[string]any{"invoice_id": inv.ID},
			})
		}
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_payment_applied",
		Actor:         "ApplyPayment",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"currency":        cur,
			"amount_cents":    amountCents,
			"applied_cents":   amountCents - remaining,
			"remainder_cents": remaining,
			"invoices":        appliedIDs,
		},
	})
	return applied, remaining, nil
}

// VoidInvoice cancels an invoice that never received a payment.
// Voiding is idempotent: voiding an already-voided invoice is a no-op.
// An invoice with any payment applied (or fully paid) cannot be voided
// — the money moved, so the record must stay for the audit trail; use
// a reversing Post/Transfer to unwind it instead.
func (l *Ledger) VoidInvoice(id string) error {
	return l.VoidInvoiceCtx(context.Background(), id)
}

// VoidInvoiceCtx is VoidInvoice with per-call audit actor attribution
// (LG-73).
func (l *Ledger) VoidInvoiceCtx(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	inv, ok := l.invoices[id]
	if !ok {
		return ErrInvoiceNotFound
	}
	if inv.Status == InvoiceVoided {
		return nil
	}
	if inv.PaidCents > 0 || inv.Status == InvoicePaid {
		return ErrInvoiceHasPayments
	}
	inv.Status = InvoiceVoided
	l.invoices[id] = inv
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "invoice_voided",
		Actor:         "VoidInvoice",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{inv.Account},
		Details: map[string]any{
			"currency":     inv.Currency,
			"amount_cents": inv.AmountCents,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxInvoiceVoided,
		Account: inv.Account,
		Details: map[string]any{"invoice_id": id},
	})
	return nil
}

// SweepOverdueInvoices moves every issued/partial invoice with
// now > DueDate to overdue and returns their IDs, sorted. It is the
// operator's "到期复核" for receivables: the same scan, the same audit
// events as the lazy paths. Each newly overdue invoice emits an
// "invoice_overdue" structured audit event and an outbox event, so the
// collections trail shows exactly when the invoice went past due. The
// dunning 衔接 is automatic: EscalateDunning's next scan reads the
// invoices' due dates through dunningAgeLocked, so a configured
// (account, currency) escalates on the overdue invoice without any
// further wiring here.
func (l *Ledger) SweepOverdueInvoices(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	overdue := make([]string, 0)
	for id, inv := range l.invoices {
		if inv.Status != InvoiceIssued && inv.Status != InvoicePartial {
			continue
		}
		if !now.After(inv.DueDate) {
			continue
		}
		inv.Status = InvoiceOverdue
		l.invoices[id] = inv
		overdue = append(overdue, id)
		l.emitAudit(AuditEvent{
			Op:            "invoice_overdue",
			Actor:         "SweepOverdueInvoices",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{inv.Account},
			Details: map[string]any{
				"currency":      inv.Currency,
				"amount_cents":  inv.AmountCents,
				"paid_cents":    inv.PaidCents,
				"due_date":      inv.DueDate.UTC().Format(time.RFC3339),
				"days_past_due": int64(now.Sub(inv.DueDate) / (24 * time.Hour)),
			},
		})
		l.emitOutboxLocked(OutboxEvent{
			Type:        OutboxInvoiceOverdue,
			Account:     inv.Account,
			AmountCents: inv.AmountCents - inv.PaidCents,
			Currency:    inv.Currency,
			Details:     map[string]any{"invoice_id": id},
		})
	}
	sort.Strings(overdue)
	return overdue
}

// ReceivablesAging is the Reconcile disclosure of open receivables
// (see ReconciliationReport.ReceivablesAging): per currency, the
// outstanding cents of open invoices (issued/partial/overdue) bucketed
// by days past due as of the report time. Buckets follow the
// collections convention: current (not yet due), 1-30, 31-60, 61-90,
// and 90+ days past due.
type ReceivablesAging struct {
	Currency        string `json:"currency"`
	CurrentCents    int64  `json:"current_cents"`
	Days1_30Cents   int64  `json:"days_1_30_cents"`
	Days31_60Cents  int64  `json:"days_31_60_cents"`
	Days61_90Cents  int64  `json:"days_61_90_cents"`
	Days90PlusCents int64  `json:"days_90_plus_cents"`
	OpenInvoices    int    `json:"open_invoices"`
}

// receivablesAgingLocked builds the Reconcile receivables-aging
// disclosure. Callers must hold l.mu.
func (l *Ledger) receivablesAgingLocked(now time.Time) []ReceivablesAging {
	byCur := make(map[string]*ReceivablesAging)
	for _, inv := range l.invoices {
		if !invoiceOpen(inv.Status) {
			continue
		}
		outstanding := inv.AmountCents - inv.PaidCents
		if outstanding <= 0 {
			continue
		}
		row := byCur[inv.Currency]
		if row == nil {
			row = &ReceivablesAging{Currency: inv.Currency}
			byCur[inv.Currency] = row
		}
		row.OpenInvoices++
		dpd := int64(now.Sub(inv.DueDate) / (24 * time.Hour))
		switch {
		case dpd <= 0:
			row.CurrentCents += outstanding
		case dpd <= 30:
			row.Days1_30Cents += outstanding
		case dpd <= 60:
			row.Days31_60Cents += outstanding
		case dpd <= 90:
			row.Days61_90Cents += outstanding
		default:
			row.Days90PlusCents += outstanding
		}
	}
	out := make([]ReceivablesAging, 0, len(byCur))
	for _, row := range byCur {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// invoicesLocked returns every invoice, sorted by ID, for the snapshot
// config record. Callers must hold l.mu.
func (l *Ledger) invoicesLocked() []Invoice {
	out := make([]Invoice, 0, len(l.invoices))
	for _, inv := range l.invoices {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rebuildInvoiceIndexLocked rebuilds the per-account invoice index from
// the invoice map. Callers must hold the write lock; used by the
// snapshot import path.
func (l *Ledger) rebuildInvoiceIndexLocked() {
	l.invoicesByAccount = make(map[AccountID][]string)
	for id, inv := range l.invoices {
		l.invoicesByAccount[inv.Account] = append(l.invoicesByAccount[inv.Account], id)
	}
}
