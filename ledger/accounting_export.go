package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Accounting software export (LG-184, fintech/后端工程: 财务集成).
//
// ExportAccounting renders journal entries for external accounting
// software in two wire formats:
//   - AccountingXero: Xero bank-statement CSV — Date, Amount, Payee,
//     Description, Reference, CheckNumber — Xero's documented bank
//     statement import set, per account.
//   - AccountingQuickBooks: QuickBooks general-journal IIF —
//     !TRNS/!SPL/!ENDTRNS tab-separated blocks, whole-journal
//     double-entry import.
//
// This is the bookkeeping twin of the payment-rail exports: those
// (nacha.go, pain001.go, pacs.008/009, MT103) move money; this one
// carries the journal to the books. It is orthogonal to
// audit_export.go (the audit-chain evidence export) and export.go
// (the signed reconciliation export): those speak to auditors, this
// one speaks to the accounting system.
//
// Scope honesty:
//   - The Xero bank-statement CSV has exactly six columns — no
//     currency column, no tax-code column. Tax mapping is out of
//     scope (documented, not faked): tax codes live on invoices and
//     bills, not on bank statements.
//   - QuickBooks general-journal IIF likewise carries no currency:
//     both formats are single-currency per export. Mixed currencies
//     without an explicit Currency filter are refused fail-closed,
//     never silently converted.
//   - The per-currency summary (the 多币种汇总行) rides the
//     AccountingExport result and the HTTP
//     X-Accounting-Summary header — never inside the import files,
//     where extra rows would corrupt the import.
//   - The Xero Date column renders ISO YYYY-MM-DD; the operator's
//     Xero organization date-format setting must accept it — the
//     export documents the format rather than guessing the org's
//     locale.
//   - The Xero export is account-centric, the mt940.go convention:
//     the account as the debit leg reads as money out (negative
//     amount), as the credit leg as money in (positive); a
//     self-transfer reads as a credit.
//   - Both formats render the economic date (see EffectiveValueDate,
//     LG-181): the value date when the entry carries one, otherwise
//     the booking date — the date the accountant cares about.
//   - Lines are joined with "\n", the nacha.go convention; convert to
//     CRLF for the importer if it requires it.
//
// Read-only: the journal is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event — the same
// contract as ExportMT940 and ExportOFX. The export is deterministic:
// repeat exports of the same journal render byte-identical output.

// Accounting export formats, the admitted vocabulary.
const (
	// AccountingXero renders the Xero bank-statement CSV.
	AccountingXero = "xero"
	// AccountingQuickBooks renders the QuickBooks general-journal IIF.
	AccountingQuickBooks = "quickbooks"
)

// Accounting export errors.
var (
	// ErrAccountingFormat is returned for a format outside the
	// admitted vocabulary ("xero", "quickbooks").
	ErrAccountingFormat = errors.New("ledger: accounting export: unknown format")
	// ErrAccountingAccount is returned by the Xero export when no
	// account is named: a bank-statement import is per bank account.
	ErrAccountingAccount = errors.New("ledger: accounting export: xero export requires an account")
	// ErrAccountingRange is returned when Since is after Until.
	ErrAccountingRange = errors.New("ledger: accounting export: since is after until")
	// ErrAccountingMultiCurrency is returned when the exported
	// entries span multiple currencies and no Currency filter is
	// set: neither format carries a currency column, so a mixed
	// export would silently misstate amounts. Fail-closed — the
	// operator picks a currency.
	ErrAccountingMultiCurrency = errors.New("ledger: accounting export: entries span multiple currencies, set Currency")
	// ErrAccountingField is returned when an IIF field carries a
	// tab, newline, or other control character that would corrupt
	// the tab-separated framing. Fail-loud, never silently mangled.
	ErrAccountingField = errors.New("ledger: accounting export: field breaks IIF framing")
)

// AccountingExportOptions tunes ExportAccounting.
//
// Format is required: "xero" or "quickbooks". Account is required
// for the Xero bank-statement CSV; for QuickBooks it is an optional
// entry filter (empty exports the whole journal — both legs still
// render, so the journal balances). Since/Until bound the exported
// entries on the economic date, half-open [Since, Until); zero means
// unbounded. Currency is an optional single-currency filter; empty
// refuses mixed-currency journals fail-closed. AccountNames is the
// 科目映射表: ledger account IDs mapped to the display names the
// accounting system knows (Xero payees, QuickBooks chart-of-accounts
// names); unmapped accounts render as their raw IDs.
type AccountingExportOptions struct {
	Format       string
	Account      AccountID
	Since        time.Time
	Until        time.Time
	Currency     string
	AccountNames map[AccountID]string
}

// AccountingCurrencySummary is one per-currency summary row (the
// 多币种汇总行): the cross-check totals for the export. For Xero,
// DebitTotal is money out of the account and CreditTotal money in;
// for QuickBooks, they are the journal's debit and credit totals
// (equal by construction — a mismatch would mean a bookkeeping bug,
// so the exporter asserts it).
type AccountingCurrencySummary struct {
	Currency         string
	DebitTotalCents  int64
	CreditTotalCents int64
	Count            int
}

// AccountingExport is one rendered accounting export.
type AccountingExport struct {
	Format   string
	Filename string
	Content  []byte
	// Summary carries the per-currency totals sorted by currency —
	// the machine-readable cross-check, kept out of the import file
	// itself so the file stays import-clean.
	Summary []AccountingCurrencySummary
}

// accountingDisplayName resolves the display name for account: the
// configured mapping wins, otherwise the raw account ID.
func accountingDisplayName(names map[AccountID]string, account AccountID) string {
	if n, ok := names[account]; ok && n != "" {
		return n
	}
	return string(account)
}

// accountingSigned formats cents as a signed decimal with a dot
// separator and two fraction digits: "2500.00", "-123.45".
func accountingSigned(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// accountingCSVField quotes one CSV field per RFC 4180: fields
// containing a comma, quote, CR, or LF are wrapped in quotes with
// internal quotes doubled.
func accountingCSVField(s string) string {
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// accountingCheckIIFField refuses tab, newline, and other control
// characters in an IIF field: they would corrupt the tab-separated
// framing, and the exporter never mangles data silently.
func accountingCheckIIFField(field, value string) error {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return fmt.Errorf("%w: %s: %q", ErrAccountingField, field, value)
		}
	}
	return nil
}

// accountingCollect gathers the entries ExportAccounting renders:
// canonical (CreatedAt, ID) order, economic-date range filter,
// optional account filter, and the single-currency rule.
func accountingCollect(l *Ledger, opts AccountingExportOptions) ([]JournalEntry, error) {
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, ErrAccountingRange
	}
	currency := ""
	if opts.Currency != "" {
		var err error
		currency, err = normalizeCurrency(opts.Currency)
		if err != nil {
			return nil, err
		}
	}
	entries := l.Entries()
	sort.Slice(entries, func(i, j int) bool {
		ti, tj := entries[i].EffectiveValueDate(), entries[j].EffectiveValueDate()
		if ti.Equal(tj) {
			return entries[i].ID < entries[j].ID
		}
		return ti.Before(tj)
	})
	var out []JournalEntry
	currencies := map[string]bool{}
	for _, e := range entries {
		ed := e.EffectiveValueDate()
		if !opts.Since.IsZero() && ed.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !ed.Before(opts.Until) {
			continue
		}
		if opts.Account != "" && e.DebitAccount != opts.Account && e.CreditAccount != opts.Account {
			continue
		}
		if currency != "" && e.Currency != currency {
			continue
		}
		currencies[e.Currency] = true
		out = append(out, e)
	}
	if currency == "" && len(currencies) > 1 {
		return nil, fmt.Errorf("%w", ErrAccountingMultiCurrency)
	}
	return out, nil
}

// accountingSummary folds the exported entries into per-currency
// summary rows sorted by currency. debitOf/creditOf are
// format-specific: Xero is account-centric, QuickBooks is
// journal-centric.
func accountingSummary(entries []JournalEntry, debitOf, creditOf func(e JournalEntry) (AccountID, int64)) []AccountingCurrencySummary {
	byCcy := map[string]*AccountingCurrencySummary{}
	for _, e := range entries {
		s := byCcy[e.Currency]
		if s == nil {
			s = &AccountingCurrencySummary{Currency: e.Currency}
			byCcy[e.Currency] = s
		}
		_, d := debitOf(e)
		_, c := creditOf(e)
		s.DebitTotalCents += d
		s.CreditTotalCents += c
		s.Count++
	}
	out := make([]AccountingCurrencySummary, 0, len(byCcy))
	for _, s := range byCcy {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// exportXeroCSV renders the Xero bank-statement CSV: Date, Amount,
// Payee, Description, Reference, CheckNumber. Account-centric, the
// mt940.go convention.
func exportXeroCSV(entries []JournalEntry, opts AccountingExportOptions) AccountingExport {
	var b strings.Builder
	b.WriteString("Date,Amount,Payee,Description,Reference,CheckNumber\n")
	name := func(a AccountID) string { return accountingDisplayName(opts.AccountNames, a) }
	for _, e := range entries {
		date := e.EffectiveValueDate().UTC().Format("2006-01-02")
		var amount int64
		var counterparty AccountID
		// The account as the credit leg reads money in (positive),
		// as the debit leg money out (negative); a self-transfer
		// reads as a credit, the mt940.go bias.
		if e.CreditAccount == opts.Account {
			amount = e.AmountCents
			counterparty = e.DebitAccount
		} else {
			amount = -e.AmountCents
			counterparty = e.CreditAccount
		}
		fields := []string{
			date,
			accountingSigned(amount),
			name(counterparty),
			e.Memo,
			e.ID,
			"",
		}
		for i, f := range fields {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(accountingCSVField(f))
		}
		b.WriteByte('\n')
	}
	exp := AccountingExport{
		Format:   AccountingXero,
		Filename: "xero-statement.csv",
		Content:  []byte(b.String()),
		Summary: accountingSummary(entries,
			func(e JournalEntry) (AccountID, int64) {
				if e.DebitAccount == opts.Account && e.CreditAccount != opts.Account {
					return e.DebitAccount, e.AmountCents
				}
				return "", 0
			},
			func(e JournalEntry) (AccountID, int64) {
				if e.CreditAccount == opts.Account {
					return e.CreditAccount, e.AmountCents
				}
				return "", 0
			}),
	}
	return exp
}

// exportQuickBooksIIF renders the QuickBooks general-journal IIF: one
// TRNS (debit leg) + one SPL (credit leg) + ENDTRNS per journal
// entry, tab-separated, DATE as MM/DD/YYYY.
func exportQuickBooksIIF(entries []JournalEntry, opts AccountingExportOptions) (AccountingExport, error) {
	var b strings.Builder
	b.WriteString("!TRNS\tTRNSTYPE\tDATE\tACCNT\tNAME\tAMOUNT\tDOCNUM\tMEMO\n")
	b.WriteString("!SPL\tTRNSTYPE\tDATE\tACCNT\tNAME\tAMOUNT\tDOCNUM\tMEMO\n")
	b.WriteString("!ENDTRNS\n")
	name := func(a AccountID) string { return accountingDisplayName(opts.AccountNames, a) }
	check := func(field, value string) error { return accountingCheckIIFField(field, value) }
	for _, e := range entries {
		date := e.EffectiveValueDate().UTC().Format("01/02/2006")
		debitName, creditName := name(e.DebitAccount), name(e.CreditAccount)
		for _, v := range [][2]string{
			{"ACCNT", debitName}, {"NAME", creditName},
			{"ACCNT", creditName}, {"NAME", debitName},
			{"DOCNUM", e.ID}, {"MEMO", e.Memo},
		} {
			if err := check(v[0], v[1]); err != nil {
				return AccountingExport{}, err
			}
		}
		amount := accountingSigned(e.AmountCents)
		fmt.Fprintf(&b, "TRNS\tGENERAL JOURNAL\t%s\t%s\t%s\t%s\t%s\t%s\n",
			date, debitName, creditName, amount, e.ID, e.Memo)
		fmt.Fprintf(&b, "SPL\tGENERAL JOURNAL\t%s\t%s\t%s\t%s\t%s\t%s\n",
			date, creditName, debitName, "-"+amount, e.ID, e.Memo)
		fmt.Fprintf(&b, "ENDTRNS\n")
	}
	exp := AccountingExport{
		Format:   AccountingQuickBooks,
		Filename: "quickbooks-journal.iif",
		Content:  []byte(b.String()),
		Summary: accountingSummary(entries,
			func(e JournalEntry) (AccountID, int64) { return e.DebitAccount, e.AmountCents },
			func(e JournalEntry) (AccountID, int64) { return e.CreditAccount, e.AmountCents }),
	}
	// Structural self-check: a general journal must balance per
	// currency — a mismatch would mean a bookkeeping bug, and a
	// corrupt journal must never ship to the accounting system.
	for _, s := range exp.Summary {
		if s.DebitTotalCents != s.CreditTotalCents {
			return AccountingExport{}, fmt.Errorf("ledger: accounting export: journal does not balance in %s", s.Currency)
		}
	}
	return exp, nil
}

// ExportAccounting renders the journal for external accounting
// software — Xero bank-statement CSV or QuickBooks general-journal
// IIF. See the package doc for the format contracts and scope
// honesty notes.
//
// The export is read-only and deterministic: repeat exports of the
// same journal render byte-identical output.
func (l *Ledger) ExportAccounting(opts AccountingExportOptions) (AccountingExport, error) {
	switch opts.Format {
	case AccountingXero:
	case AccountingQuickBooks:
	default:
		return AccountingExport{}, fmt.Errorf("%w: %q", ErrAccountingFormat, opts.Format)
	}
	if opts.Format == AccountingXero && opts.Account == "" {
		return AccountingExport{}, ErrAccountingAccount
	}
	entries, err := accountingCollect(l, opts)
	if err != nil {
		return AccountingExport{}, err
	}
	if opts.Format == AccountingXero {
		return exportXeroCSV(entries, opts), nil
	}
	return exportQuickBooksIIF(entries, opts)
}

// AccountingSummaryHeader renders the export summary for the HTTP
// X-Accounting-Summary header: "USD debit=1234.56 credit=1234.56
// count=10; EUR ...".
func AccountingSummaryHeader(summary []AccountingCurrencySummary) string {
	parts := make([]string, 0, len(summary))
	for _, s := range summary {
		parts = append(parts, fmt.Sprintf("%s debit=%s credit=%s count=%d",
			s.Currency, accountingSigned(s.DebitTotalCents),
			accountingSigned(s.CreditTotalCents), s.Count))
	}
	return strings.Join(parts, "; ")
}
