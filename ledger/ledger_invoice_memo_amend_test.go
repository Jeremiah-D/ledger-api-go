package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// issueTestInvoiceWithMemo issues an invoice carrying a memo, like
// issueTestInvoice but with the annotation set at issue.
func issueTestInvoiceWithMemo(t *testing.T, l *Ledger, id string, account AccountID, amount int64, issuedAt, dueDate time.Time, memo string) Invoice {
	t.Helper()
	inv, err := l.IssueInvoice(Invoice{
		ID:          id,
		Account:     account,
		Currency:    "USD",
		AmountCents: amount,
		IssuedAt:    issuedAt,
		DueDate:     dueDate,
		Memo:        memo,
	})
	if err != nil {
		t.Fatalf("IssueInvoice(%q): %v", id, err)
	}
	return inv
}

func TestAmendInvoiceMemoValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoiceWithMemo(t, l, "inv-1", "acme", 5000, issuedAt, due, "order 1001")

	// Unknown ID.
	if _, err := l.AmendInvoiceMemo("nope", "new memo"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("unknown invoice: err = %v, want ErrInvoiceNotFound", err)
	}

	// The new memo reuses the IssueInvoice gate: more than 255 UTF-8
	// characters is rejected, and the original memo is untouched.
	// 255 multi-byte runes fit; 256 do not — the cap counts runes,
	// not bytes.
	for _, bad := range []string{strings.Repeat("x", 256), strings.Repeat("é", 256)} {
		if _, err := l.AmendInvoiceMemo("inv-1", bad); !errors.Is(err, ErrMemoTooLong) {
			t.Errorf("memo of %d runes: err = %v, want ErrMemoTooLong", len([]rune(bad)), err)
		}
		got, _ := l.GetInvoice("inv-1")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("invoice after overlong memo = %+v, want %+v (untouched)", got, orig)
		}
	}
	if _, err := l.AmendInvoiceMemo("inv-1", strings.Repeat("é", 255)); err != nil {
		t.Errorf("memo of 255 multi-byte runes: err = %v, want nil", err)
	}

	// An empty memo is valid: it clears the annotation.
	cleared, err := l.AmendInvoiceMemo("inv-1", "")
	if err != nil {
		t.Fatalf("empty memo: err = %v, want nil", err)
	}
	if cleared.Memo != "" {
		t.Errorf("cleared memo = %q, want empty", cleared.Memo)
	}
}

func TestAmendInvoiceMemoStateGate(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	asof := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	// Paid: the money moved, so the record must stay as-is — the
	// same rejection VoidInvoice returns.
	issueTestInvoiceWithMemo(t, l, "inv-paid", "acme", 1000, issuedAt, due, "paid memo")
	if _, _, err := l.ApplyPayment("acme", "USD", 1000, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-paid"); got.Status != InvoicePaid {
		t.Fatalf("inv-paid status = %v, want paid", got.Status)
	}
	if _, err := l.AmendInvoiceMemo("inv-paid", "new memo"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The state gate runs before the no-op check: even a same-value
	// amendment is rejected on a terminal invoice.
	if _, err := l.AmendInvoiceMemo("inv-paid", "paid memo"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice, same memo: err = %v, want ErrInvoiceHasPayments", err)
	}

	// Voided: closed, so its memo cannot move.
	issueTestInvoiceWithMemo(t, l, "inv-void", "acme", 1000, issuedAt, due, "void memo")
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if _, err := l.AmendInvoiceMemo("inv-void", "new memo"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice: err = %v, want ErrInvoiceNotOpen", err)
	}
	if _, err := l.AmendInvoiceMemo("inv-void", "void memo"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice, same memo: err = %v, want ErrInvoiceNotOpen", err)
	}

	// Partially paid is still open and amendable.
	issueTestInvoiceWithMemo(t, l, "inv-part", "acme", 2000, issuedAt, due, "part memo")
	if _, _, err := l.ApplyPayment("acme", "USD", 500, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	amended, err := l.AmendInvoiceMemo("inv-part", "part memo, updated")
	if err != nil {
		t.Fatalf("partial invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoicePartial || amended.Memo != "part memo, updated" {
		t.Errorf("amended partial = %+v, want partial/updated memo", amended)
	}

	// Overdue is still open and amendable.
	issueTestInvoiceWithMemo(t, l, "inv-overdue", "acme", 3000, issuedAt, due, "overdue memo")
	// The partial invoice from above is past due too, so the sweep
	// returns both; only inv-overdue's presence matters here.
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
	amended, err = l.AmendInvoiceMemo("inv-overdue", "overdue memo, updated")
	if err != nil {
		t.Fatalf("overdue invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoiceOverdue || amended.Memo != "overdue memo, updated" {
		t.Errorf("amended overdue = %+v, want overdue/updated memo", amended)
	}
}

func TestAmendInvoiceMemoNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoiceWithMemo(t, l, "inv-1", "acme", 5000, issuedAt, due, "order 1001")

	// Amending to the current memo is a no-op, not a change.
	if got, err := l.AmendInvoiceMemo("inv-1", "order 1001"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-memo amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event (the issue event is the only
	// invoice_memo_amended-free trail here).
	if events := auditEventsByOp(t, dir)["invoice_memo_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendInvoiceMemoSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoiceWithMemo(t, l, "inv-1", "acme", 5000, issuedAt, due, "order 1001")
	versionBefore := l.Version()

	amended, err := l.AmendInvoiceMemo("inv-1", "order 1001, terms renegotiated")
	if err != nil {
		t.Fatalf("AmendInvoiceMemo: %v", err)
	}
	if amended.Memo != "order 1001, terms renegotiated" {
		t.Errorf("amended memo = %q, want renegotiated memo", amended.Memo)
	}
	// Only the memo moves: amount, paid amount, status, due date,
	// currency, and account are untouched.
	amended.Memo = orig.Memo
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended invoice = %+v, want %+v except memo", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["invoice_memo_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "inv-1" || ev.Actor != "AmendInvoiceMemo" {
		t.Errorf("amend event = trace %q actor %q, want inv-1/AmendInvoiceMemo", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_memo"] != "order 1001" || ev.Details["memo"] != "order 1001, terms renegotiated" {
		t.Errorf("amend event details = %v, want previous order 1001 / new renegotiated memo", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}
