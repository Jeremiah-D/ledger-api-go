package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInvoiceAmountValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Unknown ID.
	if _, err := l.AmendInvoiceAmount("nope", 6000); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("unknown invoice: err = %v, want ErrInvoiceNotFound", err)
	}

	// The new amount reuses the IssueInvoice gate: zero and negative
	// amounts are rejected, and the original amount is untouched.
	for _, bad := range []int64{0, -1, -5000} {
		if _, err := l.AmendInvoiceAmount("inv-1", bad); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("amount %d: err = %v, want ErrNonPositiveAmount", bad, err)
		}
		got, _ := l.GetInvoice("inv-1")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("invoice after bad amount %d = %+v, want %+v (untouched)", bad, got, orig)
		}
	}
}

func TestAmendInvoiceAmountStateGate(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	asof := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	// Paid: the money moved, so the record must stay as-is — the
	// same rejection VoidInvoice returns.
	issueTestInvoice(t, l, "inv-paid", "acme", 1000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 1000, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-paid"); got.Status != InvoicePaid {
		t.Fatalf("inv-paid status = %v, want paid", got.Status)
	}
	if _, err := l.AmendInvoiceAmount("inv-paid", 2000); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The state gate runs before the no-op check: even a same-value
	// amendment is rejected on a terminal invoice.
	if _, err := l.AmendInvoiceAmount("inv-paid", 1000); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice, same amount: err = %v, want ErrInvoiceHasPayments", err)
	}

	// Partially paid: still open, but the amount is locked the
	// moment money lands — the settled cents were priced against
	// the old amount.
	issueTestInvoice(t, l, "inv-part", "acme", 2000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-part"); got.Status != InvoicePartial {
		t.Fatalf("inv-part status = %v, want partial", got.Status)
	}
	if _, err := l.AmendInvoiceAmount("inv-part", 3000); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("partial invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	if _, err := l.AmendInvoiceAmount("inv-part", 2000); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("partial invoice, same amount: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The failed amendment leaves the invoice untouched.
	if got, _ := l.GetInvoice("inv-part"); got.AmountCents != 2000 || got.PaidCents != 500 {
		t.Errorf("invoice after rejected amendment = %+v, want amount 2000 / paid 500", got)
	}

	// Voided: closed, so its amount cannot move.
	issueTestInvoice(t, l, "inv-void", "acme", 1000, issuedAt, due)
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if _, err := l.AmendInvoiceAmount("inv-void", 2000); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice: err = %v, want ErrInvoiceNotOpen", err)
	}
	if _, err := l.AmendInvoiceAmount("inv-void", 1000); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice, same amount: err = %v, want ErrInvoiceNotOpen", err)
	}

	// Overdue is still open and unpaid, so it is amendable.
	issueTestInvoice(t, l, "inv-overdue", "acme", 3000, issuedAt, due)
	swept := l.SweepOverdueInvoices(asof)
	found := false
	for _, id := range swept {
		if id == "inv-overdue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sweep = %v, want it to contain inv-overdue", swept)
	}
	amended, err := l.AmendInvoiceAmount("inv-overdue", 4000)
	if err != nil {
		t.Fatalf("overdue invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoiceOverdue || amended.AmountCents != 4000 {
		t.Errorf("amended overdue = %+v, want overdue/4000", amended)
	}
}

func TestAmendInvoiceAmountNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Amending to the current amount is a no-op, not a change.
	if got, err := l.AmendInvoiceAmount("inv-1", 5000); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-amount amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event (the issue event is the only
	// invoice_amount_amended-free trail here).
	if events := auditEventsByOp(t, dir)["invoice_amount_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendInvoiceAmountSuccess(t *testing.T) {
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

	amended, err := l.AmendInvoiceAmount("inv-1", 7500)
	if err != nil {
		t.Fatalf("AmendInvoiceAmount: %v", err)
	}
	if amended.AmountCents != 7500 {
		t.Errorf("amended amount = %d, want 7500", amended.AmountCents)
	}
	// Only the amount moves: paid amount, currency, status, due
	// date, memo, and account are untouched.
	amended.AmountCents = orig.AmountCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended invoice = %+v, want %+v except amount", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["invoice_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "inv-1" || ev.Actor != "AmendInvoiceAmount" {
		t.Errorf("amend event = trace %q actor %q, want inv-1/AmendInvoiceAmount", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_amount_cents"] != float64(5000) || ev.Details["amount_cents"] != float64(7500) {
		t.Errorf("amend event details = %v, want previous 5000 / new 7500", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendInvoiceAmountLiveReads(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	if _, err := l.AmendInvoiceAmount("inv-1", 3000); err != nil {
		t.Fatalf("AmendInvoiceAmount: %v", err)
	}

	// ApplyPayment retires against the new amount from the next
	// evaluation: a 3000-cent payment now pays the invoice in full,
	// where the old amount would have left 2000 outstanding.
	applied, rem, err := l.ApplyPayment("acme", "USD", 3000, issuedAt)
	if err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if rem != 0 || len(applied) != 1 || applied[0].InvoiceID != "inv-1" || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("ApplyPayment: applied = %+v remainder = %d, want [inv-1 paid] remainder 0", applied, rem)
	}
	if got, _ := l.GetInvoice("inv-1"); got.AmountCents != 3000 || got.PaidCents != 3000 {
		t.Errorf("invoice after payment = %+v, want amount 3000 / paid 3000", got)
	}

	// Raising the amount likewise raises what a payment must cover:
	// a partial payment against the raised amount leaves the new
	// outstanding balance.
	issueTestInvoice(t, l, "inv-2", "acme", 2000, issuedAt, due)
	if _, err := l.AmendInvoiceAmount("inv-2", 8000); err != nil {
		t.Fatalf("AmendInvoiceAmount: %v", err)
	}
	applied, rem, err = l.ApplyPayment("acme", "USD", 2000, issuedAt)
	if err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if rem != 0 || len(applied) != 1 || applied[0].AppliedCents != 2000 || applied[0].StatusAfter != InvoicePartial {
		t.Fatalf("ApplyPayment: applied = %+v remainder = %d, want [inv-2 partial 2000] remainder 0", applied, rem)
	}
	if got, _ := l.GetInvoice("inv-2"); got.AmountCents != 8000 || got.PaidCents != 2000 || got.Status != InvoicePartial {
		t.Errorf("invoice after partial payment = %+v, want amount 8000 / paid 2000 / partial", got)
	}
}
