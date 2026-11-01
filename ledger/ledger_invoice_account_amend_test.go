package ledger

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestAmendInvoiceAccountValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Unknown ID.
	if _, err := l.AmendInvoiceAccount("nope", "globex"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("unknown invoice: err = %v, want ErrInvoiceNotFound", err)
	}

	// The new account reuses the IssueInvoice gate: an empty
	// account is rejected, and the original account is untouched.
	// (Issue time does not check that the account exists, and
	// neither does the amendment — any non-empty account is fine.)
	if _, err := l.AmendInvoiceAccount("inv-1", ""); !errors.Is(err, ErrInvalidInvoice) {
		t.Errorf("empty account: err = %v, want ErrInvalidInvoice", err)
	}
	got, _ := l.GetInvoice("inv-1")
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("invoice after bad account = %+v, want %+v (untouched)", got, orig)
	}
}

func TestAmendInvoiceAccountStateGate(t *testing.T) {
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
	if _, err := l.AmendInvoiceAccount("inv-paid", "globex"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The state gate runs before the no-op check: even a same-value
	// amendment is rejected on a terminal invoice.
	if _, err := l.AmendInvoiceAccount("inv-paid", "acme"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice, same account: err = %v, want ErrInvoiceHasPayments", err)
	}

	// Partially paid: still open, but the payer is locked the moment
	// money lands — the settled cents belong to the old payer, and
	// moving the invoice would hang them on the new account's name.
	issueTestInvoice(t, l, "inv-part", "acme", 2000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-part"); got.Status != InvoicePartial {
		t.Fatalf("inv-part status = %v, want partial", got.Status)
	}
	if _, err := l.AmendInvoiceAccount("inv-part", "globex"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("partial invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	if _, err := l.AmendInvoiceAccount("inv-part", "acme"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("partial invoice, same account: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The failed amendment leaves the invoice untouched.
	if got, _ := l.GetInvoice("inv-part"); got.Account != "acme" || got.PaidCents != 500 {
		t.Errorf("invoice after rejected amendment = %+v, want account acme / paid 500", got)
	}

	// Voided: closed, so its payer cannot move.
	issueTestInvoice(t, l, "inv-void", "acme", 1000, issuedAt, due)
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if _, err := l.AmendInvoiceAccount("inv-void", "globex"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice: err = %v, want ErrInvoiceNotOpen", err)
	}
	if _, err := l.AmendInvoiceAccount("inv-void", "acme"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice, same account: err = %v, want ErrInvoiceNotOpen", err)
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
	amended, err := l.AmendInvoiceAccount("inv-overdue", "globex")
	if err != nil {
		t.Fatalf("overdue invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoiceOverdue || amended.Account != "globex" {
		t.Errorf("amended overdue = %+v, want overdue/globex", amended)
	}
}

func TestAmendInvoiceAccountNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Amending to the current account is a no-op, not a change.
	if got, err := l.AmendInvoiceAccount("inv-1", "acme"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-account amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event (the issue event is the only
	// invoice_account_amended-free trail here).
	if events := auditEventsByOp(t, dir)["invoice_account_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendInvoiceAccountSuccess(t *testing.T) {
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

	amended, err := l.AmendInvoiceAccount("inv-1", "globex")
	if err != nil {
		t.Fatalf("AmendInvoiceAccount: %v", err)
	}
	if amended.Account != "globex" {
		t.Errorf("amended account = %q, want globex", amended.Account)
	}
	// Only the account moves: paid amount (zero), amount, currency,
	// status, due date, and memo are untouched.
	amended.Account = orig.Account
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended invoice = %+v, want %+v except account", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["invoice_account_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "inv-1" || ev.Actor != "AmendInvoiceAccount" {
		t.Errorf("amend event = trace %q actor %q, want inv-1/AmendInvoiceAccount", ev.TraceID, ev.Actor)
	}
	// The event names both accounts: it concerns the old payer's
	// trail as much as the new one's.
	if len(ev.Accounts) != 2 || ev.Accounts[0] != "acme" || ev.Accounts[1] != "globex" {
		t.Errorf("amend event accounts = %v, want [acme globex]", ev.Accounts)
	}
	if ev.Details["previous_account"] != "acme" || ev.Details["account"] != "globex" {
		t.Errorf("amend event details = %v, want previous acme / new globex", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendInvoiceAccountIndexMigration(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	issueTestInvoice(t, l, "inv-2", "acme", 3000, issuedAt, due)
	issueTestInvoice(t, l, "inv-3", "globex", 1000, issuedAt, due)

	if _, err := l.AmendInvoiceAccount("inv-1", "globex"); err != nil {
		t.Fatalf("AmendInvoiceAccount: %v", err)
	}

	// The per-account index moves with the invoice: inv-1 leaves
	// acme's list and joins globex's, and the index stays identical
	// to a rebuild from the invoice map.
	if got := l.invoicesByAccount["acme"]; len(got) != 1 || got[0] != "inv-2" {
		t.Errorf("acme index = %v, want [inv-2]", got)
	}
	if got := l.invoicesByAccount["globex"]; len(got) != 2 || got[0] != "inv-3" || got[1] != "inv-1" {
		t.Errorf("globex index = %v, want [inv-3 inv-1]", got)
	}
	rebuilt := make(map[AccountID][]string)
	for id, inv := range l.invoices {
		rebuilt[inv.Account] = append(rebuilt[inv.Account], id)
	}
	// List order inside each account is append order in the live
	// index but map-iteration order in the rebuild: sort both
	// before comparing.
	for _, m := range []map[AccountID][]string{l.invoicesByAccount, rebuilt} {
		for _, ids := range m {
			sort.Strings(ids)
		}
	}
	if !reflect.DeepEqual(l.invoicesByAccount, rebuilt) {
		t.Errorf("index = %v, want rebuild %v", l.invoicesByAccount, rebuilt)
	}

	// Moving the last invoice off an account drops the emptied list,
	// exactly as a rebuild would.
	if _, err := l.AmendInvoiceAccount("inv-2", "globex"); err != nil {
		t.Fatalf("AmendInvoiceAccount: %v", err)
	}
	if _, ok := l.invoicesByAccount["acme"]; ok {
		t.Errorf("acme index still present = %v, want it dropped", l.invoicesByAccount["acme"])
	}
}

func TestAmendInvoiceAccountLiveReads(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	if _, err := l.AmendInvoiceAccount("inv-1", "globex"); err != nil {
		t.Fatalf("AmendInvoiceAccount: %v", err)
	}

	// ApplyPayment from the old account no longer sees the invoice:
	// the payment comes back in full as remainder.
	applied, rem, err := l.ApplyPayment("acme", "USD", 5000, issuedAt)
	if err != nil {
		t.Fatalf("ApplyPayment old account: %v", err)
	}
	if rem != 5000 || len(applied) != 0 {
		t.Fatalf("ApplyPayment old account: applied = %+v remainder = %d, want [] remainder 5000", applied, rem)
	}

	// ApplyPayment from the new account retires it from the next
	// evaluation.
	applied, rem, err = l.ApplyPayment("globex", "USD", 5000, issuedAt)
	if err != nil {
		t.Fatalf("ApplyPayment new account: %v", err)
	}
	if rem != 0 || len(applied) != 1 || applied[0].InvoiceID != "inv-1" || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("ApplyPayment new account: applied = %+v remainder = %d, want [inv-1 paid] remainder 0", applied, rem)
	}
	if got, _ := l.GetInvoice("inv-1"); got.Account != "globex" || got.PaidCents != 5000 {
		t.Errorf("invoice after payment = %+v, want account globex / paid 5000", got)
	}

	// ListInvoices follows the live account too.
	issueTestInvoice(t, l, "inv-2", "acme", 2000, issuedAt, due)
	if _, err := l.AmendInvoiceAccount("inv-2", "globex"); err != nil {
		t.Fatalf("AmendInvoiceAccount: %v", err)
	}
	if listed := l.ListInvoices("acme", ""); len(listed) != 0 {
		t.Errorf("ListInvoices(acme) = %v, want empty", listed)
	}
	if listed := l.ListInvoices("globex", ""); len(listed) != 2 {
		t.Errorf("ListInvoices(globex) = %d invoices, want 2", len(listed))
	}
}
