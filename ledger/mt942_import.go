package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT942 interim transaction report import (LG-449): the
// inbound half of the MT942 lifecycle, the FIN version of the
// camt.052 import (LG-408) for the intraday side. A bank hands us
// an MT942 interim transaction report; ParseMT942 parses it and
// ImportMT942 returns a pre-posting reconciliation report: every
// :61: line is matched against the journal by its reference, and
// the :90D:/:90C: trailers are compared with the journal's own
// debit/credit totals folded over the report window.
//
// Like the camt.052 import, the MT941 import (LG-444) and the
// MT940 import (LG-445), this is deliberately a draft, not a
// posting: it writes no journal rows, enters no hash chain, and
// bumps no version. A :61: line that disagrees with its journal
// row, or a :90D:/:90C: trailer that disagrees with the journal
// fold, is reported, never fail-closed — the import is evidence,
// not a booking — and the ledger is never adjusted to close the
// gap. Duplicate imports of the same document are idempotent via
// the document's SHA-256 (see mt942Imports on Ledger). No audit
// event is emitted, along the MT941 import (LG-444, the named
// template for this item).
//
// ParseMT942 is the inverse of RenderMT942Statement by
// construction: it accepts the bare tag block ExportMT942 renders
// (:20: :25: :28C: [:34F:] :13D: (:61: [:86:])* :90D: :90C:), in
// canonical order. There is no {1:}/{2:}/{3:} envelope to check —
// ExportMT942 renders none, so demanding one would fail the
// round-trip the parser exists for. Within the block the parsing
// is strict along ParseMT202/ParseMT300/ParseMT941: the field
// order, every field value, and the self-consistency of the report
// (the :90D:/:90C: trailers must foot the :61: lines, exactly as
// RenderMT942Statement re-derives them) are validated, and
// anything outside the field set is refused — an interim report
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the last field is tolerated.
//
// An MT942 report carries no balance tags, so it carries no
// currency either — except through the optional :34F: floor limit
// indicator, whose currency is the report currency. When :34F:
// is present, line matching and the trailer fold filter on that
// currency, exactly like MT940 filters on the statement currency;
// when it is absent, the document names no currency and the lines
// match currency-agnostically (by reference, amount, and
// direction) and no trailer fold is computed — there is nothing
// to fold in. The single-currency rule therefore lives at the
// import, not the wire: a report names at most one currency, and
// a journal row in another currency never matches its lines.
//
// The :90D:/:90C: trailers carry no separator between the entry
// count and the amount, so a body like "21500,00" admits several
// (count, amount) splits. The parser enumerates the splits (the
// count is at most 5 digits, the amount at most 15d, neither with
// a leading zero unless it is exactly "0") and keeps the one
// split-pair that foots the :61: lines; a document with no footing
// split is unbalanced, and one with two is ambiguous — both fail
// closed. The footing itself is re-derived from the lines, so
// unlike MT941 no balance equation is needed to resolve the split.
//
// The :25: account must name a known ledger account: the internal
// ID directly, an external identifier registered under the mt942
// system (usually the IBAN the bank names — see ExportMT942), or
// a registered IBAN. An unknown account is refused fail-closed
// with ErrMT942UnknownAccount (the MT101 :50H: gate, along
// ErrMT940UnknownAccount/ErrMT941UnknownAccount) — the ledger
// never reconciles a report for an account it does not hold.

// ErrInvalidMT942 is returned by ParseMT942 and ImportMT942 when
// the input is not a readable MT942 report: empty input, a field
// out of order or outside the field set, a malformed :20:/:25:/
// :28C:/:34F:/:13D: value, a :61: line with a bad date, mark,
// amount, or reference, a :90D:/:90C: trailer that does not foot
// the :61: lines, an ambiguous :90D:/:90C: split, or a report
// naming a different account than the import target. The HTTP
// layer maps it to 400.
var ErrInvalidMT942 = errors.New("ledger: invalid MT942 document")

// ErrMT942UnknownAccount is returned by ImportMT942 when the :25:
// account names no ledger account: no journaled rows, no
// registered IBAN, and no mt942-system external mapping. The HTTP
// layer maps it to 422.
var ErrMT942UnknownAccount = errors.New("ledger: mt942 inbound: account is unknown")

// MT942LineMatch is one :61: interim-report line with its
// reconciliation outcome against the journal. Status reuses the
// camt.053 match vocabulary: matched (exactly one journaled row
// with this reference, equal amount and direction), mismatched
// (one row with this reference but a different amount or
// direction, or several rows sharing the truncated reference),
// unmatched (no journaled row carries this reference). EntryID
// names the journaled row when one was found; Reason explains a
// mismatch. Supplementary carries the line's :86: text verbatim
// when the bank attached one.
type MT942LineMatch struct {
	Reference        string             `json:"reference"`
	ValueDate        time.Time          `json:"value_date"`
	AmountCents      int64              `json:"amount_cents"`
	Credit           bool               `json:"credit"`
	Supplementary    string             `json:"supplementary,omitempty"`
	Status           CAMT053MatchStatus `json:"status"`
	EntryID          string             `json:"entry_id,omitempty"`
	EntryAmountCents int64              `json:"entry_amount_cents,omitempty"`
	EntryCredit      bool               `json:"entry_credit,omitempty"`
	CandidateCount   int                `json:"candidate_count,omitempty"`
	Reason           string             `json:"reason,omitempty"`
}

// MT942SummaryMismatch is one :90D:/:90C: trailer that disagrees
// with the journal: the trailer total the report claims
// (ReportedCents) against the total the journal folds over the
// report window (LedgerCents), on the D or C side. Reported-only:
// the import never adjusts the books to close the gap.
type MT942SummaryMismatch struct {
	Currency      string    `json:"currency"`
	Side          string    `json:"side"`
	ReportedCents int64     `json:"reported_cents"`
	LedgerCents   int64     `json:"ledger_cents"`
	At            time.Time `json:"at"`
}

// MT942ImportReport is the full pre-posting report for one imported
// MT942 interim transaction report. Amounts are absolute cents —
// the D/C mark carries the side, so the report reads without
// re-deriving the marks. LineMatches carries every :61: line with
// its match outcome; SummaryMismatches names the :90D:/:90C:
// trailers that disagreed with the journal fold over the report
// window (empty when the report carries no :34F: currency to fold
// in, or when the report carries no lines). FloorLimit carries
// the :34F: indicator verbatim when the report carries one.
// Duplicate is true when the report is a replay of an earlier
// import of the same document (see ImportMT942).
type MT942ImportReport struct {
	DocumentSHA256       string                 `json:"document_sha256"`
	Account              AccountID              `json:"account"`
	TransactionReference string                 `json:"transaction_reference"`
	StatementNumber      string                 `json:"statement_number"`
	SequenceNumber       string                 `json:"sequence_number"`
	ReportDateTime       time.Time              `json:"report_date_time"`
	Currency             string                 `json:"currency"`
	FloorLimit           *MT942FloorLimit       `json:"floor_limit,omitempty"`
	DebitCount           int                    `json:"debit_count"`
	DebitTotalCents      int64                  `json:"debit_total_cents"`
	CreditCount          int                    `json:"credit_count"`
	CreditTotalCents     int64                  `json:"credit_total_cents"`
	Lines                []MT942LineMatch       `json:"lines"`
	Matched              int                    `json:"matched"`
	Mismatched           int                    `json:"mismatched"`
	Unmatched            int                    `json:"unmatched"`
	SummaryMismatches    []MT942SummaryMismatch `json:"summary_mismatches,omitempty"`
	Duplicate            bool                   `json:"duplicate,omitempty"`
}

// mt942ParseAmount parses an MT942 amount body (`<integer>,<ff>`)
// into cents. The integer part holds at most 13 digits (the 15d
// rule: 13 integer digits plus 2 fraction digits) with no leading
// zero unless it is exactly "0"; the fraction part holds exactly
// two digits. Unlike the :61: line amount, zero is allowed — a
// :34F: floor limit of zero is a limit, not a missing amount.
func mt942ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrInvalidMT942, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || len(fracPart) != 2 || len(intPart) > 13 {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT942, body)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: amount %q has a leading zero", ErrInvalidMT942, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT942, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT942, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT942, body)
	}
	return whole*100 + frac, nil
}

// mt942ParseFloorLimit parses one :34F: body (3-letter currency +
// D/C mark + amount) into its MT942FloorLimit. The mark carries
// the side the floor applies to; the amount is absolute.
func mt942ParseFloorLimit(body string) (*MT942FloorLimit, error) {
	if len(body) < 3+1+4 {
		return nil, fmt.Errorf("%w: :34F: too short: %q", ErrInvalidMT942, body)
	}
	currency, err := normalizeCurrency(body[0:3])
	if err != nil {
		return nil, fmt.Errorf("%w: :34F: bad currency %q", ErrInvalidMT942, body[0:3])
	}
	var fl MT942FloorLimit
	fl.Currency = currency
	switch body[3] {
	case 'C':
		fl.Credit = true
	case 'D':
		fl.Credit = false
	default:
		return nil, fmt.Errorf("%w: :34F: bad debit/credit mark %q", ErrInvalidMT942, body[3:4])
	}
	cents, err := mt942ParseAmount(body[4:])
	if err != nil {
		return nil, fmt.Errorf("%w: :34F: %v", ErrInvalidMT942, err)
	}
	fl.AmountCents = cents
	return &fl, nil
}

// mt942ParseLine parses one :61: body (YYMMDD + MMDD + D/C mark +
// 15d comma amount + "NTRF//" + 1–16x reference) into its
// MT940Line. It mirrors mt940ParseLine in the MT942 error domain:
// the MT940 import is untouched (LG-449 excludes it), so the
// parser is duplicated here rather than re-wrapped — a bad :61:
// in an MT942 document is an ErrInvalidMT942, never an
// ErrInvalidMT940. The entry date carries no year on the wire;
// it takes the value date's year, and the month/day must be a
// real calendar date in that year. The amount must be positive —
// a zero-amount line is not a bank line, matching mt940LineOf.
func mt942ParseLine(body string) (MT940Line, error) {
	if len(body) < 6+4+1+4 {
		return MT940Line{}, fmt.Errorf("%w: :61: too short: %q", ErrInvalidMT942, body)
	}
	valueDate, err := time.Parse("060102", body[0:6])
	if err != nil {
		return MT940Line{}, fmt.Errorf("%w: :61: bad value date %q: %v", ErrInvalidMT942, body[0:6], err)
	}
	valueDate = valueDate.UTC()
	entryMonth, err := strconv.Atoi(body[6:8])
	if err != nil || entryMonth < 1 || entryMonth > 12 {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry month %q", ErrInvalidMT942, body[6:8])
	}
	entryDay, err := strconv.Atoi(body[8:10])
	if err != nil || entryDay < 1 || entryDay > 31 {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry day %q", ErrInvalidMT942, body[8:10])
	}
	entryDate := time.Date(valueDate.Year(), time.Month(entryMonth), entryDay, 0, 0, 0, 0, time.UTC)
	if int(entryDate.Month()) != entryMonth || entryDate.Day() != entryDay {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry date %q", ErrInvalidMT942, body[6:10])
	}
	var line MT940Line
	switch body[10] {
	case 'C':
		line.Credit = true
	case 'D':
		line.Credit = false
	default:
		return MT940Line{}, fmt.Errorf("%w: :61: bad debit/credit mark %q", ErrInvalidMT942, body[10:11])
	}
	rest := body[11:]
	sep := strings.Index(rest, "NTRF//")
	if sep < 0 {
		return MT940Line{}, fmt.Errorf("%w: :61: want NTRF// reference, got %q", ErrInvalidMT942, body)
	}
	cents, err := mt942ParseAmount(rest[:sep])
	if err != nil {
		return MT940Line{}, fmt.Errorf("%w: :61: %v", ErrInvalidMT942, err)
	}
	if cents <= 0 {
		return MT940Line{}, fmt.Errorf("%w: :61: amount must be positive, got %q", ErrInvalidMT942, rest[:sep])
	}
	line.AmountCents = cents
	ref := rest[sep+len("NTRF//"):]
	if ref == "" || len([]rune(ref)) > 16 {
		return MT940Line{}, fmt.Errorf("%w: :61: want 1-16x reference, got %q", ErrInvalidMT942, ref)
	}
	if strings.TrimSpace(ref) != ref || strings.ContainsAny(ref, " \t\r\n") {
		return MT940Line{}, fmt.Errorf("%w: :61: reference carries whitespace: %q", ErrInvalidMT942, ref)
	}
	line.ValueDate = valueDate
	line.EntryDate = entryDate
	line.Reference = ref
	return line, nil
}

// mt942SummaryCandidates enumerates the (count, amount) splits a
// :90D:/:90C: body admits: the count is 1–5 digits, the amount's
// integer part 1–13 digits, neither with a leading zero unless it
// is exactly "0". It mirrors mt941SummaryCandidates in the MT942
// error domain; the split is resolved by footing the :61: lines,
// not by position.
func mt942SummaryCandidates(body, tag string) ([]MT942Summary, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 || len(parts[1]) != 2 {
		return nil, fmt.Errorf("%w: %s: want <count><amount>,<2 fraction digits>, got %q", ErrInvalidMT942, tag, body)
	}
	digits, fracPart := parts[0], parts[1]
	if digits == "" {
		return nil, fmt.Errorf("%w: %s: empty body %q", ErrInvalidMT942, tag, body)
	}
	for _, c := range digits + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %s: bad summary %q", ErrInvalidMT942, tag, body)
		}
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: bad summary %q", ErrInvalidMT942, tag, body)
	}
	var out []MT942Summary
	for k := 1; k <= len(digits) && k <= 5; k++ {
		countStr, wholeStr := digits[:k], digits[k:]
		if wholeStr == "" || len(wholeStr) > 13 {
			continue
		}
		if len(countStr) > 1 && countStr[0] == '0' {
			continue
		}
		if len(wholeStr) > 1 && wholeStr[0] == '0' {
			continue
		}
		count, err := strconv.Atoi(countStr)
		if err != nil {
			continue
		}
		whole, err := strconv.ParseInt(wholeStr, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, MT942Summary{Count: count, AmountCents: whole*100 + frac})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s: no valid (count, amount) split in %q", ErrInvalidMT942, tag, body)
	}
	return out, nil
}

// ParseMT942 parses a SWIFT FIN MT942 interim transaction report
// message into its document model. It is the inverse of
// RenderMT942Statement: :20: (transaction reference, 16x), :25:
// (account identification, 35x), :28C: (statement/sequence
// number), an optional :34F: (floor limit indicator), :13D:
// (report date/time, YYMMDDHHMM), one :61: (+ optional :86:)
// per transaction line, :90D: (number-and-sum of the debit lines),
// :90C: (number-and-sum of the credit lines). There are no balance
// tags — a :60F:/:62F:/:64: field is refused like any other
// out-of-block field, and nothing may follow :90C:. Parsing is
// strict: the field order is canonical, every field value is
// validated, and anything outside the field set is refused. CRLF
// line endings are accepted; a trailing newline after the last
// field is tolerated.
//
// The self-consistency check mirrors RenderMT942Statement: the
// :90D:/:90C: trailers must equal the count and total of the :61:
// lines on their side. A report whose trailer does not foot its
// lines is never parsed.
func ParseMT942(text string) (*MT942Statement, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidMT942)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")

	// Walk the block in canonical order: each expect takes the
	// next line and refuses anything but its tag, so an unknown,
	// duplicated, or out-of-order field is a framing error, not a
	// continuation.
	i := 0
	expect := func(tag string) (string, error) {
		if i >= len(lines) {
			return "", fmt.Errorf("%w: want %s field, got end of document", ErrInvalidMT942, tag)
		}
		line := lines[i]
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrInvalidMT942, i+1, tag, line)
		}
		i++
		return line[len(tag):], nil
	}

	stmt := &MT942Statement{}

	// :20: — the transaction reference, the idempotency key.
	ref, err := expect(":20:")
	if err != nil {
		return nil, err
	}
	if err := mt202Reference(ref, ":20:"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMT942, err)
	}
	stmt.TransactionReference = ref

	// :25: — the account identification, 35x.
	acct, err := expect(":25:")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(acct) == "" {
		return nil, fmt.Errorf("%w: :25: account identification required", ErrInvalidMT942)
	}
	if len([]rune(acct)) > 35 {
		return nil, fmt.Errorf("%w: :25: exceeds 35x: %q", ErrInvalidMT942, acct)
	}
	stmt.AccountIdentification = acct

	// :28C: — statement/sequence number, both parts required.
	s28, err := expect(":28C:")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s28, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("%w: :28C: want <statement>/<sequence>, got %q", ErrInvalidMT942, s28)
	}
	stmt.StatementNumber = parts[0]
	stmt.SequenceNumber = parts[1]

	// :34F: — the floor limit indicator, optional, before :13D:
	// (the export renders it there).
	if i < len(lines) && strings.HasPrefix(lines[i], ":34F:") {
		body, err := expect(":34F:")
		if err != nil {
			return nil, err
		}
		fl, err := mt942ParseFloorLimit(body)
		if err != nil {
			return nil, err
		}
		stmt.FloorLimit = fl
	}

	// :13D: — the report date/time; it must be a valid instant.
	dt13, err := expect(":13D:")
	if err != nil {
		return nil, err
	}
	if len(dt13) != 10 {
		return nil, fmt.Errorf("%w: :13D: want YYMMDDHHMM, got %q", ErrInvalidMT942, dt13)
	}
	at, err := time.Parse("0601021504", dt13)
	if err != nil {
		return nil, fmt.Errorf("%w: :13D: %q: %v", ErrInvalidMT942, dt13, err)
	}
	stmt.ReportDateTime = at.UTC()

	// (:61: [:86:])* — the transaction lines, each with optional
	// supplementary details.
	for i < len(lines) && strings.HasPrefix(lines[i], ":61:") {
		body, err := expect(":61:")
		if err != nil {
			return nil, err
		}
		line, err := mt942ParseLine(body)
		if err != nil {
			return nil, err
		}
		if i < len(lines) && strings.HasPrefix(lines[i], ":86:") {
			supp, err := expect(":86:")
			if err != nil {
				return nil, err
			}
			if supp == "" {
				return nil, fmt.Errorf("%w: :86: field is empty", ErrInvalidMT942)
			}
			line.Supplementary = supp
		}
		stmt.Lines = append(stmt.Lines, line)
	}

	// :90D:/:90C: — the number-and-sum trailers. The split is
	// resolved by footing the :61: lines below.
	dBody, err := expect(":90D:")
	if err != nil {
		return nil, err
	}
	debitCands, err := mt942SummaryCandidates(dBody, ":90D:")
	if err != nil {
		return nil, err
	}
	cBody, err := expect(":90C:")
	if err != nil {
		return nil, err
	}
	creditCands, err := mt942SummaryCandidates(cBody, ":90C:")
	if err != nil {
		return nil, err
	}

	// Nothing may follow :90C: — an MT942 report carries no
	// trailing :86: (only the per-line ones).
	if i != len(lines) {
		return nil, fmt.Errorf("%w: line %d: unexpected trailing content %q", ErrInvalidMT942, i+1, lines[i])
	}

	// Reconciliation: the trailers must foot the lines. The
	// amounts are absolute in the model; the tag identity carries
	// the side. Saturating add, like RenderMT942Statement: an
	// adversarial line history can never wrap the re-derivation
	// into a false match.
	var debitCount, creditCount int
	var debitTotal, creditTotal int64
	for _, l := range stmt.Lines {
		if l.Credit {
			creditCount++
			creditTotal = satAddInt64(creditTotal, l.AmountCents)
		} else {
			debitCount++
			debitTotal = satAddInt64(debitTotal, l.AmountCents)
		}
	}
	matches := 0
	var chosenD, chosenC MT942Summary
	for _, d := range debitCands {
		if d.Count == debitCount && d.AmountCents == debitTotal {
			for _, c := range creditCands {
				if c.Count == creditCount && c.AmountCents == creditTotal {
					chosenD, chosenC = d, c
					matches++
				}
			}
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf("%w: :90D:/:90C: trailers (%q / %q) do not foot %d debit lines totalling %d cents and %d credit lines totalling %d cents",
			ErrInvalidMT942, dBody, cBody, debitCount, debitTotal, creditCount, creditTotal)
	}
	if matches > 1 {
		return nil, fmt.Errorf("%w: :90D:/:90C: split is ambiguous in %q / %q", ErrInvalidMT942, dBody, cBody)
	}
	stmt.DebitSummary = chosenD
	stmt.CreditSummary = chosenC
	return stmt, nil
}

// mt942ResolveAccountLocked resolves a :25: account identification
// to the ledger account it names: the internal ID directly, else
// an external identifier registered under the mt942 system
// (usually the IBAN the bank names — see ExportMT942), else a
// registered IBAN. Callers must hold l.mu (either lock suffices).
func (l *Ledger) mt942ResolveAccountLocked(id string) (AccountID, error) {
	id = strings.TrimSpace(id)
	if acct := AccountID(id); l.mt942KnownAccountLocked(acct) {
		return acct, nil
	}
	for k := range l.externalMappings {
		if k.system == ExternalSystemMT942 && k.externalID == id {
			return k.account, nil
		}
	}
	if acct, ok := l.accountByIBANLocked(normalizeIBAN(id)); ok {
		return acct, nil
	}
	return "", fmt.Errorf("%w: %q", ErrMT942UnknownAccount, id)
}

// mt942KnownAccountLocked reports whether acct is a known ledger
// account: it carries journaled rows, a registered IBAN, or an
// external mapping (the camt.057 resolve rule). Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt942KnownAccountLocked(acct AccountID) bool {
	if acct == "" {
		return false
	}
	if len(l.byAccount[acct]) > 0 {
		return true
	}
	if _, ok := l.accountIBANs[acct]; ok {
		return true
	}
	for k := range l.externalMappings {
		if k.account == acct {
			return true
		}
	}
	return false
}

// mt942MatchLineLocked matches one :61: interim-report line
// against the account's effective journal rows by reference: the
// entry whose mt940LineRef (idempotency key, else entry ID) the
// report truncates to 16x. Pending-review rows never touched
// balances and are excluded, like the export. The :61: line
// carries no currency, so when the report names one (through
// :34F:) candidates must be in that currency; when it names none
// (currency == "") the match is currency-agnostic — the document
// gives no currency to filter on. Callers must hold l.mu (either
// lock suffices).
func (l *Ledger) mt942MatchLineLocked(account AccountID, currency string, line MT940Line) MT942LineMatch {
	match := MT942LineMatch{
		Reference:     line.Reference,
		ValueDate:     line.ValueDate,
		AmountCents:   line.AmountCents,
		Credit:        line.Credit,
		Supplementary: line.Supplementary,
	}
	var cands []JournalEntry
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview {
			continue
		}
		if currency != "" && e.Currency != currency {
			continue
		}
		if mt940Truncate(mt940LineRef(e), 16) != line.Reference {
			continue
		}
		cands = append(cands, e)
	}
	switch len(cands) {
	case 0:
		match.Status = CAMT053Unmatched
		match.Reason = "no journal entry carries this reference"
	case 1:
		e := cands[0]
		// Direction is account-centric, mirroring the export:
		// the account as the debit leg reads C (money in), as
		// the credit leg D (money out).
		entryCredit := e.DebitAccount == account
		match.EntryID = e.ID
		match.EntryAmountCents = e.AmountCents
		match.EntryCredit = entryCredit
		if e.AmountCents == line.AmountCents && entryCredit == line.Credit {
			match.Status = CAMT053Matched
			break
		}
		match.Status = CAMT053Mismatched
		if e.AmountCents != line.AmountCents {
			match.Reason = fmt.Sprintf("amount differs: report %d, journal %d", line.AmountCents, e.AmountCents)
		} else {
			match.Reason = "direction differs: report and journal disagree on the C/D mark"
		}
	default:
		match.Status = CAMT053Mismatched
		match.CandidateCount = len(cands)
		match.Reason = fmt.Sprintf("reference matches %d journal entries", len(cands))
	}
	return match
}

// mt942FoldWindowLocked folds the account's debit and credit totals
// over the intraday window [from, until) in currency: entries the
// account books as its debit leg read C (money in, credit side),
// as its credit leg D (money out, debit side); a self-transfer
// reads as a credit, the same bias the export uses. Pending-review
// rows never touched balances and are excluded. Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt942FoldWindowLocked(account AccountID, currency string, from, until time.Time) (debitTotal, creditTotal int64) {
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != currency {
			continue
		}
		if e.CreatedAt.Before(from) || !e.CreatedAt.Before(until) {
			continue
		}
		if e.DebitAccount == account {
			creditTotal = satAddInt64(creditTotal, e.AmountCents)
		} else {
			debitTotal = satAddInt64(debitTotal, e.AmountCents)
		}
	}
	return debitTotal, creditTotal
}

// ImportMT942 imports a SWIFT FIN MT942 interim transaction report
// as a pre-posting reconciliation report for account: it parses
// the document, resolves the :25: account (which must be the
// import target), matches every :61: line against the journal by
// its reference, and — when the report names a currency through
// :34F: — compares the :90D:/:90C: trailers with the journal's
// debit/credit totals folded over the report window (from the
// earliest :61: value date to the :13D: report instant). Nothing
// is posted, no hash chain is entered, and the journal version
// does not move — the report is a draft for the operator to
// review.
//
// Line matches: a line whose reference names no journaled row is
// unmatched; one whose row disagrees on amount or direction is
// mismatched, with both sides in the report. Neither fails the
// import — the report is evidence, not a booking. Trailer
// differences: a :90D:/:90C: total that disagrees with the
// journal fold lands in SummaryMismatches and never fails the
// import, along the MT941 balance mismatch. The window fold is
// approximate by construction — an MT942 covers the transactions
// since the last statement, and the earliest line value date is
// the only window bound the document carries — so a mismatch
// there is a prompt to look, not a verdict.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns
// the original report with Duplicate set — a re-uploaded report
// never double-counts.
//
// A report naming a different account than the import target is
// an error: importing another account's report into this one
// would corrupt the reconciliation. The report's currency is the
// :34F: currency when present (ParseMT942 already refused a
// mixed-currency report); without :34F: the report names no
// currency, the lines match currency-agnostically, and no trailer
// fold is computed.
func (l *Ledger) ImportMT942(account AccountID, data []byte) (*MT942ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	stmt, err := ParseMT942(string(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	resolved, err := l.mt942ResolveAccountLocked(stmt.AccountIdentification)
	if err != nil {
		return nil, err
	}
	if resolved != account {
		return nil, fmt.Errorf("%w: report names account %q, import targets %q", ErrInvalidMT942, stmt.AccountIdentification, account)
	}
	if !l.mt942KnownAccountLocked(resolved) {
		return nil, fmt.Errorf("%w: %q", ErrMT942UnknownAccount, stmt.AccountIdentification)
	}

	if prev, ok := l.mt942Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	currency := ""
	if stmt.FloorLimit != nil {
		currency = stmt.FloorLimit.Currency
	}
	report := &MT942ImportReport{
		DocumentSHA256:       key,
		Account:              account,
		TransactionReference: stmt.TransactionReference,
		StatementNumber:      stmt.StatementNumber,
		SequenceNumber:       stmt.SequenceNumber,
		ReportDateTime:       stmt.ReportDateTime,
		Currency:             currency,
		DebitCount:           stmt.DebitSummary.Count,
		DebitTotalCents:      stmt.DebitSummary.AmountCents,
		CreditCount:          stmt.CreditSummary.Count,
		CreditTotalCents:     stmt.CreditSummary.AmountCents,
		Lines:                []MT942LineMatch{},
	}
	if stmt.FloorLimit != nil {
		fl := *stmt.FloorLimit
		report.FloorLimit = &fl
	}
	for _, line := range stmt.Lines {
		m := l.mt942MatchLineLocked(account, currency, line)
		switch m.Status {
		case CAMT053Matched:
			report.Matched++
		case CAMT053Mismatched:
			report.Mismatched++
		default:
			report.Unmatched++
		}
		report.Lines = append(report.Lines, m)
	}
	// The trailers foot the lines by construction (ParseMT942
	// proved it); the journal fold asks whether the journal
	// agrees with the report over the intraday window. Only
	// computable when the report names a currency and carries
	// lines to bound the window with — otherwise there is
	// nothing to fold in. A disagreement is reported, never
	// fail-closed.
	if currency != "" && len(stmt.Lines) > 0 {
		earliest := stmt.Lines[0].ValueDate
		for _, line := range stmt.Lines[1:] {
			if line.ValueDate.Before(earliest) {
				earliest = line.ValueDate
			}
		}
		debitFold, creditFold := l.mt942FoldWindowLocked(account, currency, earliest, stmt.ReportDateTime)
		if debitFold != stmt.DebitSummary.AmountCents {
			report.SummaryMismatches = append(report.SummaryMismatches, MT942SummaryMismatch{
				Currency:      currency,
				Side:          "D",
				ReportedCents: stmt.DebitSummary.AmountCents,
				LedgerCents:   debitFold,
				At:            stmt.ReportDateTime,
			})
		}
		if creditFold != stmt.CreditSummary.AmountCents {
			report.SummaryMismatches = append(report.SummaryMismatches, MT942SummaryMismatch{
				Currency:      currency,
				Side:          "C",
				ReportedCents: stmt.CreditSummary.AmountCents,
				LedgerCents:   creditFold,
				At:            stmt.ReportDateTime,
			})
		}
	}

	stored := *report
	stored.Lines = append([]MT942LineMatch(nil), report.Lines...)
	stored.SummaryMismatches = append([]MT942SummaryMismatch(nil), report.SummaryMismatches...)
	if report.FloorLimit != nil {
		fl := *report.FloorLimit
		stored.FloorLimit = &fl
	}
	if l.mt942Imports == nil {
		l.mt942Imports = make(map[string]*MT942ImportReport)
	}
	l.mt942Imports[key] = &stored
	return report, nil
}
