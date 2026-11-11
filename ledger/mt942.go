package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SWIFT MT942 interim transaction report message export (LG-386,
// fintech: 银企直连日间交易报告格式).
//
// ExportMT942 renders one account's effective journal rows as a SWIFT
// MT942 interim transaction report — the tag block the bank's
// intraday-report downloader would produce: :20: (transaction
// reference), :25: (account identification), :28C: (statement/sequence
// number), :34F: (floor limit indicator, optional), :13D: (report
// date/time), one :61: + optional :86: per transaction line, and the
// :90D:/:90C: number-and-sum trailers.
//
// MT942 is MT940's intraday counterpart: the same accounting fact in
// a different reporting tense — MT940 is the end-of-day customer
// statement, MT942 is the intraday transaction report a bank pulls
// several times a day for the transactions since the last statement,
// without waiting for the day to close. The block therefore mirrors
// ExportMT940 (LG-379) with two deliberate differences: there are no
// balance tags at all (:60F:/:62F:/:64: are absent — an interim
// report carries transactions, not balances), and the :90D:/:90C:
// trailers summarize the :61: lines (count and total per side) so the
// receiver can foot the report without a balance to check against.
//
// The export is read-only like ExportMT940 and ExportCAMT053: the
// scan holds the read lock from start to finish, bumps no version,
// books nothing, and appends no audit-chain link. Pending-review rows
// never touched balances and are excluded, exactly like the other
// statement exports.
//
// Direction is account-centric, mirroring MT940 and camt.053: the
// account as the debit leg reads C (credit — money in), as the credit
// leg D (debit — money out). Amounts are absolute; the sign lives in
// the C/D mark. A self-transfer (both legs the account) reads as a
// credit, the same bias the import-side drafts use.
//
// The window is half-open [Since, Until) like MT940 — but the
// pre-window rows are dropped, not folded: MT940 folds them into the
// opening balance because a statement must foot from the window start,
// while an interim report has no balance to foot, so a row before the
// window is simply outside the report.
//
// Reconciliation is structural, not asserted after the fact: the
// :90D:/:90C: trailers are computed from the :61: lines, and
// RenderMT942Statement re-derives them and fails loud if the two
// disagree (a bookkeeping-arithmetic bug must never ship a report
// whose trailer does not foot the lines). The per-line fail-loud
// cases are a missing value date, a missing amount, and a missing
// reference — a bank line without those is not a bank line.
//
// Lines are joined with "\n" and there is no trailing newline, the
// same convention as the MT940 builder: convert to CRLF for the bank
// if the uploader requires it. This module is Go-only (like MT940 and
// camt.053 export): the operator builds the file and hands it to the
// bank's statement processor.

// ErrInvalidMT942Export is returned by ExportMT942 and
// RenderMT942Statement when the request cannot name a valid report:
// an empty account is ErrEmptyAccountID, not this error — this one
// covers an invalid currency code, an inverted time range, a
// statement line missing its value date, amount, or reference, a
// :90D:/:90C: trailer that does not foot the :61: lines, or an
// invalid :34F: floor limit.
var ErrInvalidMT942Export = errors.New("ledger: invalid MT942 export options")

// MT942ExportOptions tunes ExportMT942.
//
// Account is required. Currency is required: an MT942 report is
// single-currency, and every amount tag names its currency.
// StatementID and SequenceID default to "1". Since/Until bound the
// exported lines (half-open [Since, Until); zero means unbounded).
// Lines are the account's effective journal rows in (CreatedAt, ID)
// order. FloorLimit, when set, renders the :34F: floor limit
// indicator; nil omits the tag.
type MT942ExportOptions struct {
	Account     AccountID
	StatementID string
	SequenceID  string
	Since       time.Time
	Until       time.Time
	Currency    string
	FloorLimit  *MT942FloorLimit
}

// MT942FloorLimit is one :34F: floor limit indicator: the currency,
// the D/C mark (which side the floor applies to), and the limit
// amount, in cents. An empty Currency renders as the report
// currency. The tag renders only the indicator value — this export
// never raises a breach alert on it.
type MT942FloorLimit struct {
	Credit      bool
	Currency    string
	AmountCents int64
}

// MT942Summary is one :90D:/:90C: number-and-sum trailer: how many
// entries on the side, and their total, in cents (absolute — the
// tag's D/C identity carries the side).
type MT942Summary struct {
	Count       int
	AmountCents int64
}

// MT942Statement is the export document model: header tags, the
// report date/time, the lines, and the :90D:/:90C: trailers that
// foot them.
type MT942Statement struct {
	TransactionReference  string
	AccountIdentification string
	StatementNumber       string
	SequenceNumber        string
	FloorLimit            *MT942FloorLimit
	ReportDateTime        time.Time
	Lines                 []MT940Line
	DebitSummary          MT942Summary
	CreditSummary         MT942Summary
}

// mt942ReportDateTime renders the :13D: date/time indication
// (YYMMDDHHMM, UTC).
func mt942ReportDateTime(t time.Time) string {
	return t.UTC().Format("0601021504")
}

// mt942FloorLimitLine renders one :34F: floor limit indicator:
// currency + D/C mark + amount, no separators — the same compact
// form as the :60F:/:62F: balance tags.
func mt942FloorLimitLine(fl *MT942FloorLimit) (string, error) {
	currency, err := normalizeCurrency(fl.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: floor limit: %v", ErrInvalidMT942Export, err)
	}
	if fl.AmountCents < 0 {
		return "", fmt.Errorf("%w: floor limit: negative amount", ErrInvalidMT942Export)
	}
	return fmt.Sprintf(":34F:%s%s%s", currency, mt940DC(fl.Credit), mt940Amount(fl.AmountCents)), nil
}

// mt942SummaryLine renders one :90D:/:90C: number-and-sum trailer:
// the entry count followed by the total amount (comma decimal
// separator, absolute — the tag carries the side).
func mt942SummaryLine(tag string, s MT942Summary) (string, error) {
	if s.Count < 0 {
		return "", fmt.Errorf("%w: summary %s: negative count", ErrInvalidMT942Export, tag)
	}
	if s.AmountCents < 0 {
		return "", fmt.Errorf("%w: summary %s: negative amount", ErrInvalidMT942Export, tag)
	}
	return fmt.Sprintf(":%s:%d%s", tag, s.Count, mt940Amount(s.AmountCents)), nil
}

// RenderMT942Statement renders s as an MT942 interim transaction
// report message: :20:, :25:, :28C:, :34F: (when a floor limit is
// set), :13D:, one :61: (+ optional :86:) per line, :90D:, :90C:.
// Lines join with "\n"; there is no trailing newline.
//
// The reconciliation invariant is re-derived here and fails loud:
// the :90D:/:90C: trailers must equal the count and total of the
// :61: lines on their side. A report whose trailer does not foot
// its lines is never rendered.
func RenderMT942Statement(s MT942Statement) (string, error) {
	if strings.TrimSpace(s.AccountIdentification) == "" {
		return "", fmt.Errorf("%w: account identification required", ErrInvalidMT942Export)
	}
	if strings.TrimSpace(s.StatementNumber) == "" {
		return "", fmt.Errorf("%w: statement number required", ErrInvalidMT942Export)
	}
	if strings.TrimSpace(s.SequenceNumber) == "" {
		return "", fmt.Errorf("%w: sequence number required", ErrInvalidMT942Export)
	}
	if s.ReportDateTime.IsZero() {
		return "", fmt.Errorf("%w: report date/time required", ErrInvalidMT942Export)
	}

	// Reconciliation: the trailers must foot the lines. The amounts
	// are absolute in the model; the tag identity carries the side.
	// Saturating add: an adversarial line history can never wrap
	// the re-derivation into a false match.
	var debitCount, creditCount int
	var debitTotal, creditTotal int64
	for i, l := range s.Lines {
		if l.Credit {
			creditCount++
			creditTotal = satAddInt64(creditTotal, l.AmountCents)
		} else {
			debitCount++
			debitTotal = satAddInt64(debitTotal, l.AmountCents)
		}
		if debitTotal < 0 || creditTotal < 0 {
			return "", fmt.Errorf("%w: line %d overflows reconciliation", ErrInvalidMT942Export, i+1)
		}
	}
	if debitCount != s.DebitSummary.Count || debitTotal != s.DebitSummary.AmountCents {
		return "", fmt.Errorf("%w: :90D: trailer (%d entries, %d cents) does not foot %d lines totalling %d cents",
			ErrInvalidMT942Export, s.DebitSummary.Count, s.DebitSummary.AmountCents, debitCount, debitTotal)
	}
	if creditCount != s.CreditSummary.Count || creditTotal != s.CreditSummary.AmountCents {
		return "", fmt.Errorf("%w: :90C: trailer (%d entries, %d cents) does not foot %d lines totalling %d cents",
			ErrInvalidMT942Export, s.CreditSummary.Count, s.CreditSummary.AmountCents, creditCount, creditTotal)
	}

	debitSummary, err := mt942SummaryLine("90D", s.DebitSummary)
	if err != nil {
		return "", err
	}
	creditSummary, err := mt942SummaryLine("90C", s.CreditSummary)
	if err != nil {
		return "", err
	}

	lines := []string{
		":20:" + mt940Truncate(strings.TrimSpace(s.TransactionReference), 16),
		":25:" + mt940Truncate(strings.TrimSpace(s.AccountIdentification), 35),
		":28C:" + strings.TrimSpace(s.StatementNumber) + "/" + strings.TrimSpace(s.SequenceNumber),
	}
	if s.FloorLimit != nil {
		floorLimit, err := mt942FloorLimitLine(s.FloorLimit)
		if err != nil {
			return "", err
		}
		lines = append(lines, floorLimit)
	}
	lines = append(lines, ":13D:"+mt942ReportDateTime(s.ReportDateTime))
	for _, l := range s.Lines {
		rendered, err := mt940LineOf(l)
		if err != nil {
			return "", err
		}
		lines = append(lines, rendered...)
	}
	lines = append(lines, debitSummary, creditSummary)
	return strings.Join(lines, "\n"), nil
}

// ExportMT942 renders an MT942 interim transaction report for one
// account from the journal. The account named in :25: is the
// external identifier mapped under the "mt942" system when one exists
// (see ExternalSystemMT942 — usually the IBAN the bank expects),
// else the internal account ID. A mapping declared under the "mt940"
// system does not apply here: the two messages go to different
// recipients and may name the account differently.
//
// The window is half-open [Since, Until). Rows before Since are
// outside the report — dropped, never folded: unlike MT940, there
// is no opening balance for them to fold into. Pending-review rows
// never touched balances and are excluded, like every other
// statement export.
//
// The :13D: report date/time is Until when given, else the wall
// clock at export time — so a repeat export over a pinned Until is
// byte-identical, while an open-ended export stamps the moment it
// was produced.
//
// Read-only: the scan holds the read lock from start to finish. It
// bumps no version, books nothing, and appends no audit-chain link.
func (l *Ledger) ExportMT942(opts MT942ExportOptions) (string, error) {
	if opts.Account == "" {
		return "", ErrEmptyAccountID
	}
	currency, err := normalizeCurrency(opts.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidMT942Export, err)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return "", fmt.Errorf("%w: since is after until", ErrInvalidMT942Export)
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

	var lines []MT940Line
	var debitCount, creditCount int
	var debitTotal, creditTotal int64
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportMT940 and ExportCAMT053: a
			// pending-review row never touched balances, so the
			// report excludes it. Re-export after approval.
			continue
		}
		if e.Currency != currency {
			continue
		}
		credit := e.DebitAccount == opts.Account
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			// Outside the report: dropped, never folded. MT942
			// has no opening balance for a pre-window row to
			// fold into — the MT940 fold does not apply here.
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
		lines = append(lines, MT940Line{
			ValueDate:     e.CreatedAt.UTC(),
			EntryDate:     e.CreatedAt.UTC(),
			Credit:        credit,
			AmountCents:   e.AmountCents,
			Reference:     mt940LineRef(e),
			Supplementary: e.Memo,
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].ValueDate.Equal(lines[j].ValueDate) {
			return lines[i].Reference < lines[j].Reference
		}
		return lines[i].ValueDate.Before(lines[j].ValueDate)
	})

	// The report date/time is Until when given, else the wall clock
	// at export time (along the MT940 opening/closing date default
	// rule: a pinned bound pins the document, an open bound reads
	// the clock).
	reportDateTime := opts.Until.UTC()
	if reportDateTime.IsZero() {
		reportDateTime = time.Now().UTC()
	}

	var floorLimit *MT942FloorLimit
	if opts.FloorLimit != nil {
		fl := *opts.FloorLimit
		if strings.TrimSpace(fl.Currency) == "" {
			fl.Currency = currency
		}
		floorLimit = &fl
	}

	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemMT942); ok {
		acctID = ext
	}

	return RenderMT942Statement(MT942Statement{
		TransactionReference:  stmtID,
		AccountIdentification: acctID,
		StatementNumber:       stmtID,
		SequenceNumber:        seqID,
		FloorLimit:            floorLimit,
		ReportDateTime:        reportDateTime,
		Lines:                 lines,
		DebitSummary:          MT942Summary{Count: debitCount, AmountCents: debitTotal},
		CreditSummary:         MT942Summary{Count: creditCount, AmountCents: creditTotal},
	})
}
