package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func issueTestInvoice(t *testing.T, l *Ledger, id string, account AccountID, amount int64, issuedAt, dueDate time.Time) Invoice {
	t.Helper()
	inv, err := l.IssueInvoice(Invoice{
		ID:          id,
		Account:     account,
		Currency:    "USD",
		AmountCents: amount,
		IssuedAt:    issuedAt,
		DueDate:     dueDate,
	})
	if err != nil {
		t.Fatalf("IssueInvoice(%q): %v", id, err)
	}
	return inv
}

func TestIssueInvoiceRoundTrip(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	inv := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	if inv.Status != InvoiceIssued || inv.PaidCents != 0 || inv.Currency != "USD" {
		t.Fatalf("issued invoice = %+v, want issued/0/USD", inv)
	}
	got, ok := l.GetInvoice("inv-1")
	if !ok || got.ID != "inv-1" || got.AmountCents != 5000 {
		t.Fatalf("GetInvoice(inv-1) = %+v, %v", got, ok)
	}
	if _, ok := l.GetInvoice("nope"); ok {
		t.Fatal("GetInvoice(nope) = found, want not found")
	}
	// Currency defaults to USD when empty.
	inv2, err := l.IssueInvoice(Invoice{ID: "inv-2", Account: "acme", AmountCents: 100, IssuedAt: issuedAt, DueDate: due})
	if err != nil || inv2.Currency != "USD" {
		t.Fatalf("IssueInvoice empty currency = %+v, %v; want USD", inv2, err)
	}
}

func TestIssueInvoiceValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	base := Invoice{ID: "x", Account: "a", Currency: "USD", AmountCents: 100, IssuedAt: issuedAt, DueDate: due}

	with := func(mut func(*Invoice)) Invoice {
		inv := base
		mut(&inv)
		return inv
	}
	cases := []struct {
		name string
		inv  Invoice
		want error
	}{
		{"empty ID", with(func(i *Invoice) { i.ID = "" }), ErrInvalidInvoice},
		{"empty account", with(func(i *Invoice) { i.Account = "" }), ErrInvalidInvoice},
		{"zero amount", with(func(i *Invoice) { i.AmountCents = 0 }), ErrNonPositiveAmount},
		{"negative amount", with(func(i *Invoice) { i.AmountCents = -5 }), ErrNonPositiveAmount},
		{"bad currency", with(func(i *Invoice) { i.Currency = "US" }), ErrInvalidCurrency},
		{"missing due date", with(func(i *Invoice) { i.DueDate = time.Time{} }), ErrInvalidInvoice},
		{"due before issued", with(func(i *Invoice) { i.DueDate = issuedAt.Add(-time.Hour) }), ErrInvalidInvoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.IssueInvoice(tc.inv); !errors.Is(err, tc.want) {
				t.Fatalf("IssueInvoice: got %v, want %v", err, tc.want)
			}
		})
	}
	// Duplicate IDs are rejected.
	if _, err := l.IssueInvoice(base); err != nil {
		t.Fatalf("first IssueInvoice: %v", err)
	}
	if _, err := l.IssueInvoice(base); !errors.Is(err, ErrDuplicateInvoiceID) {
		t.Fatalf("duplicate IssueInvoice: got %v, want ErrDuplicateInvoiceID", err)
	}
}

func TestApplyPaymentFIFO(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-old", "acme", 1000, base, due)
	issueTestInvoice(t, l, "inv-mid", "acme", 2000, base.Add(24*time.Hour), due)
	issueTestInvoice(t, l, "inv-new", "acme", 3000, base.Add(48*time.Hour), due)

	// 2500 covers inv-old fully and inv-mid partially.
	applied, remainder, err := l.ApplyPayment("acme", "USD", 2500, base.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if remainder != 0 {
		t.Fatalf("remainder = %d, want 0", remainder)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %d entries, want 2", len(applied))
	}
	if applied[0].InvoiceID != "inv-old" || applied[0].AppliedCents != 1000 || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("applied[0] = %+v, want inv-old/1000/paid", applied[0])
	}
	if applied[1].InvoiceID != "inv-mid" || applied[1].AppliedCents != 1500 || applied[1].StatusAfter != InvoicePartial {
		t.Fatalf("applied[1] = %+v, want inv-mid/1500/partial", applied[1])
	}
	if inv, _ := l.GetInvoice("inv-new"); inv.Status != InvoiceIssued || inv.PaidCents != 0 {
		t.Fatalf("inv-new = %+v, want issued/0 paid", inv)
	}

	// A second payment retires inv-mid and starts inv-new.
	applied, remainder, err = l.ApplyPayment("acme", "USD", 1000, base.Add(72*time.Hour))
	if err != nil || remainder != 0 {
		t.Fatalf("ApplyPayment 2: err=%v remainder=%d", err, remainder)
	}
	if applied[0].InvoiceID != "inv-mid" || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("applied[0] = %+v, want inv-mid/paid", applied[0])
	}

	// Overpaying returns the remainder: 1500 left after clearing inv-new's
	// remaining 2500.
	applied, remainder, err = l.ApplyPayment("acme", "USD", 4000, base.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("ApplyPayment 3: %v", err)
	}
	if remainder != 1500 {
		t.Fatalf("remainder = %d, want 1500", remainder)
	}
	if applied[0].InvoiceID != "inv-new" || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("applied[0] = %+v, want inv-new/paid", applied[0])
	}

	// Nothing open: the full amount comes back as remainder.
	applied, remainder, err = l.ApplyPayment("acme", "USD", 100, base.Add(72*time.Hour))
	if err != nil || remainder != 100 || len(applied) != 0 {
		t.Fatalf("ApplyPayment empty: err=%v remainder=%d applied=%d", err, remainder, len(applied))
	}
}

func TestApplyPaymentValidation(t *testing.T) {
	l := New()
	if _, _, err := l.ApplyPayment("", "USD", 100, time.Now()); !errors.Is(err, ErrInvalidInvoice) {
		t.Fatalf("ApplyPayment empty account: got %v, want ErrInvalidInvoice", err)
	}
	if _, _, err := l.ApplyPayment("a", "USD", 0, time.Now()); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("ApplyPayment zero amount: got %v, want ErrNonPositiveAmount", err)
	}
	if _, _, err := l.ApplyPayment("a", "US", 100, time.Now()); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("ApplyPayment bad currency: got %v, want ErrInvalidCurrency", err)
	}
}

func TestVoidInvoice(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-void", "acme", 1000, base, due)
	issueTestInvoice(t, l, "inv-paid", "acme", 1000, base, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 1000, base); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}

	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice unpaid: %v", err)
	}
	if inv, _ := l.GetInvoice("inv-void"); inv.Status != InvoiceVoided {
		t.Fatalf("inv-void status = %q, want voided", inv.Status)
	}
	// Voiding again is a no-op.
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice idempotent: %v", err)
	}
	// A paid invoice cannot be voided.
	if err := l.VoidInvoice("inv-paid"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Fatalf("VoidInvoice paid: got %v, want ErrInvoiceHasPayments", err)
	}
	// Unknown invoice: 404-class.
	if err := l.VoidInvoice("nope"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("VoidInvoice unknown: got %v, want ErrInvoiceNotFound", err)
	}
	// A partially paid invoice cannot be voided either.
	issueTestInvoice(t, l, "inv-partial", "acme", 2000, base, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, base); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if err := l.VoidInvoice("inv-partial"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Fatalf("VoidInvoice partial: got %v, want ErrInvoiceHasPayments", err)
	}
}

func TestSweepOverdueInvoices(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-past", "acme", 1000,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	issueTestInvoice(t, l, "inv-future", "acme", 2000,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	issueTestInvoice(t, l, "inv-edge", "acme", 3000,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), now)

	overdue := l.SweepOverdueInvoices(now)
	if len(overdue) != 1 || overdue[0] != "inv-past" {
		t.Fatalf("SweepOverdueInvoices = %v, want [inv-past]", overdue)
	}
	if inv, _ := l.GetInvoice("inv-past"); inv.Status != InvoiceOverdue {
		t.Fatalf("inv-past status = %q, want overdue", inv.Status)
	}
	if inv, _ := l.GetInvoice("inv-future"); inv.Status != InvoiceIssued {
		t.Fatalf("inv-future status = %q, want issued", inv.Status)
	}
	// now == dueDate is not past due (strict >).
	if inv, _ := l.GetInvoice("inv-edge"); inv.Status != InvoiceIssued {
		t.Fatalf("inv-edge status = %q, want issued (now == dueDate)", inv.Status)
	}
	// The sweep is idempotent: a second run finds nothing new.
	if again := l.SweepOverdueInvoices(now); len(again) != 0 {
		t.Fatalf("second sweep = %v, want []", again)
	}
}

func TestInvoiceDunningLinkage(t *testing.T) {
	l := New()
	if err := l.SetDunningConfig(DunningConfig{
		Account: "acme", Currency: "USD",
		Level1Days: 1, Level2Days: 7, Level3Days: 30,
	}); err != nil {
		t.Fatalf("SetDunningConfig: %v", err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	// No journal activity at all — the invoice alone must drive the
	// escalation.
	issueTestInvoice(t, l, "inv-dun", "acme", 5000,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))

	l.SweepOverdueInvoices(now)
	escalations := l.EscalateDunning(now)
	if len(escalations) != 1 {
		t.Fatalf("EscalateDunning = %d escalations, want 1", len(escalations))
	}
	esc := escalations[0]
	if esc.Level != DunningLevelL2 || esc.DaysPastDue != 10 || esc.OverdueCents != 5000 {
		t.Fatalf("escalation = %+v, want L2/10d/5000c", esc)
	}
	// A not-yet-due invoice never escalates.
	l2 := New()
	if err := l2.SetDunningConfig(DunningConfig{
		Account: "acme", Currency: "USD",
		Level1Days: 1, Level2Days: 7, Level3Days: 30,
	}); err != nil {
		t.Fatalf("SetDunningConfig: %v", err)
	}
	issueTestInvoice(t, l2, "inv-future", "acme", 5000,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if escalations := l2.EscalateDunning(now); len(escalations) != 0 {
		t.Fatalf("EscalateDunning with future-due invoice = %v, want none", escalations)
	}
}

func TestInvoiceReceivablesAgingDisclosure(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	issued := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// current (due in future), 1-30, 61-90 buckets.
	issueTestInvoice(t, l, "inv-cur", "acme", 1000, issued, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	issueTestInvoice(t, l, "inv-20d", "acme", 2000, issued, now.Add(-20*24*time.Hour))
	issueTestInvoice(t, l, "inv-70d", "acme", 3000, issued, now.Add(-70*24*time.Hour))
	// A paid invoice is excluded from the aging. It is issued earliest so
	// the FIFO payment lands on it first.
	issueTestInvoice(t, l, "inv-paid", "acme", 9000, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), now.Add(-10*24*time.Hour))
	if _, _, err := l.ApplyPayment("acme", "USD", 9000, now); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	// A voided invoice is excluded too.
	issueTestInvoice(t, l, "inv-void", "acme", 8000, issued, now.Add(-10*24*time.Hour))
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}

	rep := l.Reconcile(now)
	if len(rep.ReceivablesAging) != 1 {
		t.Fatalf("ReceivablesAging = %d rows, want 1", len(rep.ReceivablesAging))
	}
	row := rep.ReceivablesAging[0]
	if row.Currency != "USD" || row.OpenInvoices != 3 {
		t.Fatalf("aging row = %+v, want USD/3 open", row)
	}
	if row.CurrentCents != 1000 || row.Days1_30Cents != 2000 || row.Days61_90Cents != 3000 {
		t.Fatalf("aging buckets = %+v, want 1000/2000/3000", row)
	}
	if row.Days31_60Cents != 0 || row.Days90PlusCents != 0 {
		t.Fatalf("aging buckets = %+v, want empty 31-60/90+", row)
	}
}

func TestInvoiceSnapshotRoundTrip(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-a", "acme", 1000, base, due)
	issueTestInvoice(t, l, "inv-b", "acme", 2000, base.Add(24*time.Hour), due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, base); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	a, ok := restored.GetInvoice("inv-a")
	if !ok || a.Status != InvoicePartial || a.PaidCents != 500 {
		t.Fatalf("restored inv-a = %+v, %v; want partial/500 paid", a, ok)
	}
	if _, ok := restored.GetInvoice("inv-b"); !ok {
		t.Fatal("restored inv-b = not found")
	}
	// The restored register gates like the original: duplicate IDs fail.
	if _, err := restored.IssueInvoice(Invoice{ID: "inv-a", Account: "acme", AmountCents: 1, IssuedAt: base, DueDate: due}); !errors.Is(err, ErrDuplicateInvoiceID) {
		t.Fatalf("restored duplicate issue: got %v, want ErrDuplicateInvoiceID", err)
	}
}

func TestListInvoices(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-1", "acme", 1000, base, due)
	issueTestInvoice(t, l, "inv-2", "acme", 2000, base, due)
	inv3, err := l.IssueInvoice(Invoice{ID: "inv-3", Account: "acme", Currency: "EUR", AmountCents: 3000, IssuedAt: base, DueDate: due})
	if err != nil {
		t.Fatalf("IssueInvoice EUR: %v", err)
	}
	_ = inv3
	if got := l.ListInvoices("acme", ""); len(got) != 3 {
		t.Fatalf("ListInvoices(acme) = %d, want 3", len(got))
	}
	if got := l.ListInvoices("acme", "EUR"); len(got) != 1 || got[0].ID != "inv-3" {
		t.Fatalf("ListInvoices(acme, EUR) = %v, want [inv-3]", got)
	}
	if got := l.ListInvoices("", ""); len(got) != 3 {
		t.Fatalf("ListInvoices(all) = %d, want 3", len(got))
	}
}
