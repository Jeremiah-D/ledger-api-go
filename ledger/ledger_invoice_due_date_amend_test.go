package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInvoiceDueDateValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Unknown ID.
	if _, err := l.AmendInvoiceDueDate("nope", due.Add(24*time.Hour)); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("unknown invoice: err = %v, want ErrInvoiceNotFound", err)
	}

	// The new due date reuses the IssueInvoice validation: zero or
	// before IssuedAt is rejected, and the original due date is
	// untouched.
	for _, bad := range []time.Time{time.Time{}, issuedAt.Add(-time.Hour)} {
		if _, err := l.AmendInvoiceDueDate("inv-1", bad); !errors.Is(err, ErrInvalidInvoice) {
			t.Errorf("due date %v: err = %v, want ErrInvalidInvoice", bad, err)
		}
		got, _ := l.GetInvoice("inv-1")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("invoice after due date %v rejection = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// Equal to IssuedAt is allowed, as at issue (only "before" is
	// refused).
	amended, err := l.AmendInvoiceDueDate("inv-1", issuedAt)
	if err != nil {
		t.Fatalf("due date == IssuedAt: err = %v, want nil", err)
	}
	if !amended.DueDate.Equal(issuedAt) {
		t.Errorf("amended due date = %v, want %v", amended.DueDate, issuedAt)
	}
}

func TestAmendInvoiceDueDateStateGate(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	later := due.Add(30 * 24 * time.Hour)

	// Paid: the money moved, so the record must stay as-is — the
	// same rejection VoidInvoice returns.
	issueTestInvoice(t, l, "inv-paid", "acme", 1000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 1000, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-paid"); got.Status != InvoicePaid {
		t.Fatalf("inv-paid status = %v, want paid", got.Status)
	}
	if _, err := l.AmendInvoiceDueDate("inv-paid", later); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The state gate runs before the no-op check: even a same-value
	// amendment is rejected on a terminal invoice.
	if _, err := l.AmendInvoiceDueDate("inv-paid", due); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice, same due date: err = %v, want ErrInvoiceHasPayments", err)
	}

	// Voided: closed, so its due date cannot move.
	issueTestInvoice(t, l, "inv-void", "acme", 1000, issuedAt, due)
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if _, err := l.AmendInvoiceDueDate("inv-void", later); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice: err = %v, want ErrInvoiceNotOpen", err)
	}
	if _, err := l.AmendInvoiceDueDate("inv-void", due); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice, same due date: err = %v, want ErrInvoiceNotOpen", err)
	}

	// Partially paid is still open and amendable.
	issueTestInvoice(t, l, "inv-part", "acme", 2000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	amended, err := l.AmendInvoiceDueDate("inv-part", later)
	if err != nil {
		t.Fatalf("partial invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoicePartial || !amended.DueDate.Equal(later) {
		t.Errorf("amended partial = %+v, want partial/%v", amended, later)
	}
}

func TestAmendInvoiceDueDateNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Amending to the current due date is a no-op, not a change.
	if got, err := l.AmendInvoiceDueDate("inv-1", due); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-date amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event (the issue event is the only
	// invoice_due_date_amended-free trail here).
	if events := auditEventsByOp(t, dir)["invoice_due_date_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendInvoiceDueDateSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	versionBefore := l.Version()

	later := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	amended, err := l.AmendInvoiceDueDate("inv-1", later)
	if err != nil {
		t.Fatalf("AmendInvoiceDueDate: %v", err)
	}
	if !amended.DueDate.Equal(later) {
		t.Errorf("amended due date = %v, want %v", amended.DueDate, later)
	}
	// Only the due date moves: amount, paid amount, status, memo,
	// currency, and account are untouched.
	amended.DueDate = orig.DueDate
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended invoice = %+v, want %+v except due date", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["invoice_due_date_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "inv-1" || ev.Actor != "AmendInvoiceDueDate" {
		t.Errorf("amend event = trace %q actor %q, want inv-1/AmendInvoiceDueDate", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_due_date"] != "2026-10-31T00:00:00Z" || ev.Details["due_date"] != "2026-12-15T00:00:00Z" {
		t.Errorf("amend event details = %v, want previous 2026-10-31 / new 2026-12-15", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendInvoiceDueDateLiveEffect(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	asof := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	// An issued invoice whose due date moves past now is not swept
	// overdue at the next evaluation.
	issueTestInvoice(t, l, "inv-issued", "acme", 5000, issuedAt, due)
	if _, err := l.AmendInvoiceDueDate("inv-issued", later); err != nil {
		t.Fatalf("AmendInvoiceDueDate: %v", err)
	}
	if overdue := l.SweepOverdueInvoices(asof); len(overdue) != 0 {
		t.Errorf("sweep after extension = %v, want none overdue", overdue)
	}
	if got, _ := l.GetInvoice("inv-issued"); got.Status != InvoiceIssued {
		t.Errorf("inv-issued status = %v, want issued", got.Status)
	}

	// An overdue invoice keeps its overdue *label* after the
	// extension — the sweep never moves one back — but the live
	// reads follow the new date: the aging buckets it current again.
	issueTestInvoice(t, l, "inv-overdue", "acme", 7000, issuedAt, due)
	if overdue := l.SweepOverdueInvoices(asof); len(overdue) != 1 || overdue[0] != "inv-overdue" {
		t.Fatalf("sweep before extension = %v, want [inv-overdue]", overdue)
	}
	if _, err := l.AmendInvoiceDueDate("inv-overdue", later); err != nil {
		t.Fatalf("AmendInvoiceDueDate: %v", err)
	}
	if got, _ := l.GetInvoice("inv-overdue"); got.Status != InvoiceOverdue {
		t.Errorf("inv-overdue status = %v, want overdue (label untouched)", got.Status)
	}
	report := l.Reconcile(asof)
	if len(report.ReceivablesAging) != 1 {
		t.Fatalf("receivables aging rows = %d, want 1", len(report.ReceivablesAging))
	}
	row := report.ReceivablesAging[0]
	if row.CurrentCents != 12000 || row.Days1_30Cents != 0 {
		t.Errorf("aging after extension = current %d / 1-30 %d, want 12000 / 0 (both invoices current again)", row.CurrentCents, row.Days1_30Cents)
	}
}
