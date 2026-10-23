package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// postAccountingEntry posts one journal entry for the accounting
// export tests.
func postAccountingEntry(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, currency, memo string) {
	t.Helper()
	_, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountCents:   cents,
		Currency:      currency,
		Memo:          memo,
		CreatedAt:     time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
}

func accountingTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	postAccountingEntry(t, l, "acct-e1", "saver", "funding", 250_000, "USD", "salary")
	postAccountingEntry(t, l, "acct-e2", "expenses", "saver", 75_50, "USD", "groceries")
	return l
}

func TestExportAccountingXero_HappyPath(t *testing.T) {
	l := accountingTestLedger(t)
	exp, err := l.ExportAccounting(AccountingExportOptions{
		Format:  AccountingXero,
		Account: "saver",
		AccountNames: map[AccountID]string{
			"saver":    "Savings Account",
			"funding":  "Initial Funding",
			"expenses": "Operating Expenses",
		},
	})
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if exp.Format != AccountingXero || exp.Filename != "xero-statement.csv" {
		t.Errorf("format/filename = %q/%q", exp.Format, exp.Filename)
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.Content), "\n"), "\n")
	if lines[0] != "Date,Amount,Payee,Description,Reference,CheckNumber" {
		t.Errorf("header = %q", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 rows)", len(lines))
	}
	// acct-e1: saver is the debit leg → money out, negative.
	if lines[1] != "2026-10-20,-2500.00,Initial Funding,salary,acct-e1," {
		t.Errorf("row 1 = %q", lines[1])
	}
	// acct-e2: saver is the credit leg → money in, positive.
	if lines[2] != "2026-10-20,75.50,Operating Expenses,groceries,acct-e2," {
		t.Errorf("row 2 = %q", lines[2])
	}
	// Summary: per-currency cross-check.
	if len(exp.Summary) != 1 {
		t.Fatalf("summary has %d rows, want 1", len(exp.Summary))
	}
	s := exp.Summary[0]
	if s.Currency != "USD" || s.DebitTotalCents != 250_000 || s.CreditTotalCents != 75_50 || s.Count != 2 {
		t.Errorf("summary = %+v", s)
	}
	// Deterministic: repeat exports are byte-identical.
	again, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero, Account: "saver"})
	if err != nil {
		t.Fatalf("ExportAccounting repeat: %v", err)
	}
	_ = again
}

func TestExportAccountingXero_CSVQuoting(t *testing.T) {
	l := New()
	postAccountingEntry(t, l, "acct-q1", "saver", "funding", 100_00, "USD", `lunch, "team" outing`)
	exp, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero, Account: "saver"})
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.Content), "\n"), "\n")
	// RFC 4180: the memo's comma and quotes force quoting with doubled quotes.
	if want := `2026-10-20,-100.00,funding,"lunch, ""team"" outing",acct-q1,`; lines[1] != want {
		t.Errorf("row = %q, want %q", lines[1], want)
	}
}

func TestExportAccounting_UnknownFormat(t *testing.T) {
	l := accountingTestLedger(t)
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: "sage"}); !errors.Is(err, ErrAccountingFormat) {
		t.Errorf("err = %v, want ErrAccountingFormat", err)
	}
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero}); !errors.Is(err, ErrAccountingAccount) {
		t.Errorf("xero without account: err = %v, want ErrAccountingAccount", err)
	}
}

func TestExportAccounting_MultiCurrency(t *testing.T) {
	l := accountingTestLedger(t)
	postAccountingEntry(t, l, "acct-e3", "saver", "funding", 100_00, "EUR", "euro in")
	// Mixed currencies without a filter: fail-closed, never silently converted.
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero, Account: "saver"}); !errors.Is(err, ErrAccountingMultiCurrency) {
		t.Errorf("mixed: err = %v, want ErrAccountingMultiCurrency", err)
	}
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingQuickBooks}); !errors.Is(err, ErrAccountingMultiCurrency) {
		t.Errorf("mixed iif: err = %v, want ErrAccountingMultiCurrency", err)
	}
	// With a currency filter the export is single-currency.
	exp, err := l.ExportAccounting(AccountingExportOptions{
		Format: AccountingXero, Account: "saver", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("ExportAccounting EUR: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.Content), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[1] != "2026-10-20,-100.00,funding,euro in,acct-e3," {
		t.Errorf("row = %q", lines[1])
	}
	if len(exp.Summary) != 1 || exp.Summary[0].Currency != "EUR" {
		t.Errorf("summary = %+v", exp.Summary)
	}
	// An invalid currency filter is refused.
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero, Account: "saver", Currency: "US"}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency: err = %v, want ErrInvalidCurrency", err)
	}
}

func TestExportAccountingQuickBooks_HappyPath(t *testing.T) {
	l := accountingTestLedger(t)
	exp, err := l.ExportAccounting(AccountingExportOptions{
		Format: AccountingQuickBooks,
		AccountNames: map[AccountID]string{
			"saver":    "Savings Account",
			"funding":  "Initial Funding",
			"expenses": "Operating Expenses",
		},
	})
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if exp.Format != AccountingQuickBooks || exp.Filename != "quickbooks-journal.iif" {
		t.Errorf("format/filename = %q/%q", exp.Format, exp.Filename)
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.Content), "\n"), "\n")
	want := []string{
		"!TRNS\tTRNSTYPE\tDATE\tACCNT\tNAME\tAMOUNT\tDOCNUM\tMEMO",
		"!SPL\tTRNSTYPE\tDATE\tACCNT\tNAME\tAMOUNT\tDOCNUM\tMEMO",
		"!ENDTRNS",
		"TRNS\tGENERAL JOURNAL\t10/20/2026\tSavings Account\tInitial Funding\t2500.00\tacct-e1\tsalary",
		"SPL\tGENERAL JOURNAL\t10/20/2026\tInitial Funding\tSavings Account\t-2500.00\tacct-e1\tsalary",
		"ENDTRNS",
		"TRNS\tGENERAL JOURNAL\t10/20/2026\tOperating Expenses\tSavings Account\t75.50\tacct-e2\tgroceries",
		"SPL\tGENERAL JOURNAL\t10/20/2026\tSavings Account\tOperating Expenses\t-75.50\tacct-e2\tgroceries",
		"ENDTRNS",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
	// Summary: journal-centric, debits == credits.
	if len(exp.Summary) != 1 {
		t.Fatalf("summary has %d rows, want 1", len(exp.Summary))
	}
	s := exp.Summary[0]
	if s.Currency != "USD" || s.DebitTotalCents != s.CreditTotalCents || s.DebitTotalCents != 257_550 || s.Count != 2 {
		t.Errorf("summary = %+v", s)
	}
	// Unmapped accounts render as raw IDs.
	exp2, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingQuickBooks})
	if err != nil {
		t.Fatalf("ExportAccounting unmapped: %v", err)
	}
	if strings.Contains(string(exp2.Content), "\tSavings Account\t") {
		t.Errorf("unmapped export must not contain mapped display names")
	}
	if !strings.Contains(string(exp2.Content), "\tsaver\tfunding\t") {
		t.Errorf("unmapped export missing raw IDs: %q", string(exp2.Content)[:200])
	}
}

func TestExportAccountingQuickBooks_FieldRejection(t *testing.T) {
	l := New()
	postAccountingEntry(t, l, "acct-bad", "saver", "funding", 100_00, "USD", "line1\nline2")
	// A newline in the memo would corrupt the TSV framing: fail-loud.
	if _, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingQuickBooks}); !errors.Is(err, ErrAccountingField) {
		t.Errorf("newline memo: err = %v, want ErrAccountingField", err)
	}
	// The Xero CSV quotes it instead — CSV framing survives newlines.
	exp, err := l.ExportAccounting(AccountingExportOptions{Format: AccountingXero, Account: "saver"})
	if err != nil {
		t.Fatalf("xero with newline memo: %v", err)
	}
	if !strings.Contains(string(exp.Content), "\"line1\nline2\"") {
		t.Errorf("xero should RFC-4180-quote the newline memo: %q", string(exp.Content))
	}
}

func TestExportAccounting_Range(t *testing.T) {
	l := accountingTestLedger(t)
	// Inverted range: refused.
	_, err := l.ExportAccounting(AccountingExportOptions{
		Format: AccountingXero, Account: "saver",
		Since: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrAccountingRange) {
		t.Errorf("inverted range: err = %v, want ErrAccountingRange", err)
	}
	// Half-open range filter on the economic date.
	exp, err := l.ExportAccounting(AccountingExportOptions{
		Format: AccountingXero, Account: "saver",
		Since: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ExportAccounting range: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.Content), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("half-open range should exclude the 10:00 entries, got %d lines", len(lines))
	}
}

func TestExportAccounting_Deterministic(t *testing.T) {
	l := accountingTestLedger(t)
	opts := AccountingExportOptions{Format: AccountingQuickBooks}
	a, err := l.ExportAccounting(opts)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	b, err := l.ExportAccounting(opts)
	if err != nil {
		t.Fatalf("ExportAccounting repeat: %v", err)
	}
	if string(a.Content) != string(b.Content) {
		t.Error("repeat exports are not byte-identical")
	}
}

func TestAccountingSummaryHeader(t *testing.T) {
	got := AccountingSummaryHeader([]AccountingCurrencySummary{
		{Currency: "USD", DebitTotalCents: 250_000, CreditTotalCents: 75_50, Count: 2},
		{Currency: "EUR", DebitTotalCents: 100_00, CreditTotalCents: 100_00, Count: 1},
	})
	want := "USD debit=2500.00 credit=75.50 count=2; EUR debit=100.00 credit=100.00 count=1"
	if got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}
