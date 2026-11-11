package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT MT941 balance report message export (LG-387, fintech:
// 银企直连日间余额报告格式).
//
// ExportMT941 renders one account's effective journal rows as a SWIFT
// MT941 balance report — the tag block the bank's intraday-balance
// downloader would produce: :20: (transaction reference), :25:
// (account identification), :28C: (statement/sequence number), :13D:
// (report date/time), :60F: (opening balance), the :90D:/:90C:
// number-and-sum trailers, :62F: (book balance), :64: (available
// balance), :65: (forward available balance), and an optional :86:
// (information to account owner).
//
// MT941 is the balance-report leg of the SWIFT FIN intraday trio: it
// sits between MT940 (LG-159 library, LG-379 HTTP — the end-of-day
// customer statement, with :61: detail lines) and MT942 (LG-386 —
// the intraday transaction report, with :61: lines and no balance
// tags). MT941 carries balances and summaries but no per-transaction
// detail: the window's rows fold into the :90D:/:90C: trailers only,
// so a bank can pull an account's balance snapshot several times a
// day without pulling the full detail.
//
// The export is read-only like ExportMT940 and ExportMT942: the scan
// holds the read lock from start to finish, bumps no version, books
// nothing, and appends no audit-chain link. Pending-review rows never
// touched balances and are excluded, exactly like the other
// statement exports.
//
// Direction is account-centric, mirroring MT940 and camt.053: the
// account as the debit leg reads C (credit — money in), as the credit
// leg D (debit — money out). Amounts are absolute; the sign lives in
// the C/D mark. A self-transfer (both legs the account) reads as a
// credit, the same bias the import-side drafts use.
//
// The window is half-open [Since, Until) like MT940: rows before
// Since fold into the :60F: opening balance (the MT940 fold rule),
// rows in the window fold into the :90D:/:90C: trailers. When the
// account has no pre-window rows there is no opening balance to
// report and :60F: is omitted — the reconciliation then reads
// :62F: == :90C: − :90D: with a zero opening.
//
// Reconciliation is structural, not asserted after the fact: the
// closing balance is computed as opening + credits − debits, and
// RenderMT941Statement re-derives it and fails loud if the two
// disagree (a bookkeeping-arithmetic bug must never ship a report
// that does not foot). The :64: and :65: tags repeat :62F: exactly:
// the ledger has no intraday credit-line or forward-balance concept,
// so the available and forward available balances are the honest
// closing balance (the MT950 treatment, mt950.go).
//
// Lines are joined with "\n" and there is no trailing newline, the
// same convention as the MT940/MT942 builders: convert to CRLF for
// the bank if the uploader requires it. This module is Go-only (like
// MT940 and MT942): the operator builds the file and hands it to the
// bank's report processor.

// ErrInvalidMT941Export is returned by ExportMT941 and
// RenderMT941Statement when the request cannot name a valid report:
// an empty account is ErrEmptyAccountID, not this error — this one
// covers an invalid currency code, an inverted time range, a balance
// tag missing its value date or carrying a negative amount, a :90D:/
// :90C: trailer with a negative count or amount, or a report whose
// closing balance does not reconcile with the opening balance and
// the trailers.
var ErrInvalidMT941Export = errors.New("ledger: invalid MT941 export options")

// MT941ExportOptions tunes ExportMT941.
//
// Account is required. Currency is required: an MT941 report is
// single-currency, and every balance tag names its currency.
// StatementID and SequenceID default to "1". Since/Until bound the
// exported window (half-open [Since, Until); zero means unbounded).
// Rows are the account's effective journal rows; pre-window rows
// fold into :60F:, window rows into the :90D:/:90C: trailers.
type MT941ExportOptions struct {
	Account     AccountID
	StatementID string
	SequenceID  string
	Since       time.Time
	Until       time.Time
	Currency    string
}

// MT941Statement is the export document model: header tags, the
// report date/time, the optional opening balance, the :90D:/:90C:
// trailers, and the reconciled closing balances. OpeningBalance is
// nil when the account has no pre-window rows — :60F: is then
// omitted and the reconciliation reads the opening as zero.
type MT941Statement struct {
	TransactionReference  string
	AccountIdentification string
	StatementNumber       string
	SequenceNumber        string
	ReportDateTime        time.Time
	OpeningBalance        *MT940Balance
	DebitSummary          MT942Summary
	CreditSummary         MT942Summary
	ClosingBalance        MT940Balance
	AvailableBalance      MT940Balance
	ForwardBalance        MT940Balance
	Supplementary         string // :86: free text; empty omits the tag
}

// mt941SummaryLine renders one :90D:/:90C: number-and-sum trailer:
// the entry count followed by the total amount (comma decimal
// separator, absolute — the tag carries the side). It mirrors
// mt942SummaryLine in the MT941 error domain.
func mt941SummaryLine(tag string, s MT942Summary) (string, error) {
	if s.Count < 0 {
		return "", fmt.Errorf("%w: summary %s: negative count", ErrInvalidMT941Export, tag)
	}
	if s.AmountCents < 0 {
		return "", fmt.Errorf("%w: summary %s: negative amount", ErrInvalidMT941Export, tag)
	}
	return fmt.Sprintf(":%s:%d%s", tag, s.Count, mt940Amount(s.AmountCents)), nil
}

// RenderMT941Statement renders s as an MT941 balance report
// message: :20:, :25:, :28C:, :13D:, :60F: (when the statement
// carries an opening balance), :90D:, :90C:, :62F:, :64:, :65:,
// and a trailing :86: when supplementary text is set. There are
// deliberately no :61: lines — MT941 reports balances and
// summaries, not transactions. Lines join with "\n"; there is no
// trailing newline.
//
// The reconciliation invariant is re-derived here and fails loud:
// the closing balance must equal opening + credits − debits, and
// both :64: and :65: must repeat the closing balance. A report
// that does not foot is never rendered.
func RenderMT941Statement(s MT941Statement) (string, error) {
	if strings.TrimSpace(s.AccountIdentification) == "" {
		return "", fmt.Errorf("%w: account identification required", ErrInvalidMT941Export)
	}
	if strings.TrimSpace(s.StatementNumber) == "" {
		return "", fmt.Errorf("%w: statement number required", ErrInvalidMT941Export)
	}
	if strings.TrimSpace(s.SequenceNumber) == "" {
		return "", fmt.Errorf("%w: sequence number required", ErrInvalidMT941Export)
	}
	if s.ReportDateTime.IsZero() {
		return "", fmt.Errorf("%w: report date/time required", ErrInvalidMT941Export)
	}

	// Reconciliation: closing == opening + credits − debits. The
	// amounts are absolute in the model; the C/D marks carry the
	// signs. A nil opening balance reads as zero.
	signed := func(credit bool, cents int64) int64 {
		if credit {
			return cents
		}
		return -cents
	}
	expected := int64(0)
	openingSigned := int64(0)
	if s.OpeningBalance != nil {
		openingSigned = signed(s.OpeningBalance.Credit, s.OpeningBalance.AmountCents)
		expected = openingSigned
	}
	expected += s.CreditSummary.AmountCents
	expected -= s.DebitSummary.AmountCents
	got := signed(s.ClosingBalance.Credit, s.ClosingBalance.AmountCents)
	if expected != got {
		return "", fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and trailers :90D: %d / :90C: %d",
			ErrInvalidMT941Export, got, openingSigned, s.DebitSummary.AmountCents, s.CreditSummary.AmountCents)
	}
	// Available and forward balances must track the closing
	// balance: the ledger has no intraday credit-line or forward
	// concept, so :64: and :65: repeat :62F: exactly.
	if signed(s.AvailableBalance.Credit, s.AvailableBalance.AmountCents) != got {
		return "", fmt.Errorf("%w: available balance does not match closing balance", ErrInvalidMT941Export)
	}
	if signed(s.ForwardBalance.Credit, s.ForwardBalance.AmountCents) != got {
		return "", fmt.Errorf("%w: forward available balance does not match closing balance", ErrInvalidMT941Export)
	}

	debitSummary, err := mt941SummaryLine("90D", s.DebitSummary)
	if err != nil {
		return "", err
	}
	creditSummary, err := mt941SummaryLine("90C", s.CreditSummary)
	if err != nil {
		return "", err
	}
	closing, err := mt940BalanceLine("62F", s.ClosingBalance)
	if err != nil {
		return "", err
	}
	available, err := mt940BalanceLine("64", s.AvailableBalance)
	if err != nil {
		return "", err
	}
	forward, err := mt940BalanceLine("65", s.ForwardBalance)
	if err != nil {
		return "", err
	}

	lines := []string{
		":20:" + mt940Truncate(strings.TrimSpace(s.TransactionReference), 16),
		":25:" + mt940Truncate(strings.TrimSpace(s.AccountIdentification), 35),
		":28C:" + strings.TrimSpace(s.StatementNumber) + "/" + strings.TrimSpace(s.SequenceNumber),
		":13D:" + mt942ReportDateTime(s.ReportDateTime),
	}
	if s.OpeningBalance != nil {
		opening, err := mt940BalanceLine("60F", *s.OpeningBalance)
		if err != nil {
			return "", err
		}
		lines = append(lines, opening)
	}
	lines = append(lines, debitSummary, creditSummary, closing, available, forward)
	if strings.TrimSpace(s.Supplementary) != "" {
		lines = append(lines, ":86:"+mt940Truncate(strings.TrimSpace(s.Supplementary), 65))
	}
	return strings.Join(lines, "\n"), nil
}

// ExportMT941 renders an MT941 balance report for one account from
// the journal. The account named in :25: is the external identifier
// mapped under the "mt941" system when one exists (see
// ExternalSystemMT941 — usually the IBAN the bank expects), else the
// internal account ID. A mapping declared under the "mt940" or
// "mt942" system does not apply here: the three messages go to
// different recipients and may name the account differently.
//
// The window is half-open [Since, Until). Rows before Since fold
// into the :60F: opening balance (the MT940 fold rule); rows in the
// window fold into the :90D:/:90C: trailers — no :61: lines are
// ever rendered. Pending-review rows never touched balances and are
// excluded, like every other statement export.
//
// The :13D: report date/time is Until when given, else the wall
// clock at export time — so a repeat export over a pinned Until is
// byte-identical, while an open-ended export stamps the moment it
// was produced.
//
// Read-only: the scan holds the read lock from start to finish. It
// bumps no version, books nothing, and appends no audit-chain link.
func (l *Ledger) ExportMT941(opts MT941ExportOptions) (string, error) {
	if opts.Account == "" {
		return "", ErrEmptyAccountID
	}
	currency, err := normalizeCurrency(opts.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidMT941Export, err)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return "", fmt.Errorf("%w: since is after until", ErrInvalidMT941Export)
	}

	stmtID := strings.TrimSpace(opts.StatementID)
	if stmtID == "" {
		stmtID = "1"
	}
	seqID := strings.TrimSpace(opts.SequenceID)
	if seqID == "" {
		seqID = "1"
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	var debitCount, creditCount int
	var debitTotal, creditTotal int64
	var openingCents int64
	hasOpening := false
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportMT940 and ExportMT942: a
			// pending-review row never touched balances, so the
			// report excludes it. Re-export after approval.
			continue
		}
		if e.Currency != currency {
			continue
		}
		credit := e.DebitAccount == opts.Account
		signed := e.AmountCents
		if !credit {
			signed = -signed
		}
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			// Folds into the opening balance (the MT940 fold
			// rule). Saturating add: an adversarial amount
			// history can never wrap the fold into a false
			// balance.
			openingCents = satAddInt64(openingCents, signed)
			hasOpening = true
			continue
		}
		if !opts.Until.IsZero() && !e.CreatedAt.Before(opts.Until) {
			continue
		}
		if credit {
			creditCount++
			creditTotal = satAddInt64(creditTotal, e.AmountCents)
		} else {
			debitCount++
			debitTotal = satAddInt64(debitTotal, e.AmountCents)
		}
	}

	// The report date/time is Until when given, else the wall clock
	// at export time (along the MT940/MT942 date default rule: a
	// pinned bound pins the document, an open bound reads the
	// clock).
	reportDateTime := opts.Until.UTC()
	if reportDateTime.IsZero() {
		reportDateTime = time.Now().UTC()
	}

	balanceOf := func(cents int64, date time.Time) MT940Balance {
		credit := cents >= 0
		if !credit {
			cents = -cents
		}
		return MT940Balance{Credit: credit, ValueDate: date, Currency: currency, AmountCents: cents}
	}
	closingCents := openingCents
	closingCents = satAddInt64(closingCents, creditTotal)
	closingCents = satAddInt64(closingCents, -debitTotal)
	closingDate := opts.Until.UTC()
	if closingDate.IsZero() {
		closingDate = time.Now().UTC()
	}
	closing := balanceOf(closingCents, closingDate)

	var opening *MT940Balance
	if hasOpening {
		ob := balanceOf(openingCents, opts.Since.UTC())
		opening = &ob
	}

	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemMT941); ok {
		acctID = ext
	}

	return RenderMT941Statement(MT941Statement{
		TransactionReference:  stmtID,
		AccountIdentification: acctID,
		StatementNumber:       stmtID,
		SequenceNumber:        seqID,
		ReportDateTime:        reportDateTime,
		OpeningBalance:        opening,
		DebitSummary:          MT942Summary{Count: debitCount, AmountCents: debitTotal},
		CreditSummary:         MT942Summary{Count: creditCount, AmountCents: creditTotal},
		ClosingBalance:        closing,
		AvailableBalance:      closing,
		ForwardBalance:        closing,
	})
}
