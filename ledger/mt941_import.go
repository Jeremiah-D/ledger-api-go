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

// SWIFT FIN MT941 balance report import (LG-444): the inbound half
// of the MT941 lifecycle, the FIN version of the camt.052 import
// (LG-408). A bank hands us an MT941 balance report; ParseMT941
// parses it and ImportMT941 returns a pre-posting reconciliation
// report: the :62F:/:64: book balance at the :13D: instant is
// compared with the balance the journal folds at the same instant,
// and the :90D:/:90C: summaries ride along in the report.
//
// Like the camt.052 import, this is deliberately a draft, not a
// posting: it writes no journal rows, enters no hash chain, and
// bumps no version. A :62F: that disagrees with the ledger balance
// is reported, never fail-closed — the import is evidence, not a
// booking — and the ledger is never adjusted to close the gap.
// Duplicate imports of the same document are idempotent via the
// document's SHA-256 (see mt941Imports on Ledger).
//
// ParseMT941 is the inverse of RenderMT941Statement by
// construction: it accepts the bare tag block ExportMT941 renders
// (:20: :25: :28C: :13D: [:60F:] :90D: :90C: :62F: :64: :65:
// [:86:]), in canonical order. Unlike ParseMT202/ParseMT300 there
// is no {1:}/{2:}/{3:} envelope to check — ExportMT941 renders no
// envelope, so demanding one would fail the round-trip the parser
// exists for. Within the block the parsing is strict along
// ParseMT202/ParseMT300: the field order, every field value, and
// the self-consistency of the report (:62F: must equal :60F: +
// :90C: − :90D:, :64:/:65: must restate :62F:, all balance tags
// one currency) are validated, and anything outside the field set
// is refused. CRLF line endings are accepted; a trailing newline
// after the last field is tolerated.
//
// The :90D:/:90C: trailers carry no separator between the entry
// count and the amount, so a body like "21500,00" admits several
// (count, amount) splits. The parser enumerates the splits (the
// count is at most 5 digits, the amount at most 15d, neither with
// a leading zero unless it is exactly "0") and keeps the one
// split-pair that foots the reconciliation equation; a document
// with no footing split is unbalanced, and one with two is
// ambiguous — both fail closed.
//
// The :25: account must name a known ledger account: the internal
// ID directly, an external identifier registered under the mt941
// system (usually the IBAN the bank names — see ExportMT941), or
// a registered IBAN. An unknown account is refused fail-closed
// with ErrMT941UnknownAccount (the MT101 :50H: gate) — the ledger
// never reconciles a report for an account it does not hold.

// ErrInvalidMT941 is returned by ParseMT941 and ImportMT941 when
// the input is not a readable MT941 report: empty input, a field
// out of order or outside the field set, a malformed :20:/:25:/
// :28C:/:13D: value, a balance tag with a bad mark, date,
// currency, or amount, balance tags in mixed currencies, a :64:
// or :65: that does not restate :62F:, an unbalanced report, an
// ambiguous :90D:/:90C: split, or a report naming a different
// account than the import target. The HTTP layer maps it to 400.
var ErrInvalidMT941 = errors.New("ledger: invalid MT941 document")

// ErrMT941UnknownAccount is returned by ImportMT941 when the :25:
// account names no ledger account: no journaled rows, no
// registered IBAN, and no mt941-system external mapping. The HTTP
// layer maps it to 422.
var ErrMT941UnknownAccount = errors.New("ledger: mt941 inbound: account is unknown")

// MT941BalanceMismatch is one :62F: book balance that disagrees
// with the ledger: the balance the report claims (ReportedCents)
// against the balance the journal folds at the same instant
// (LedgerCents). Reported-only: the import never adjusts the books
// to close the gap.
type MT941BalanceMismatch struct {
	Currency      string    `json:"currency"`
	ReportedCents int64     `json:"reported_cents"`
	LedgerCents   int64     `json:"ledger_cents"`
	At            time.Time `json:"at"`
}

// MT941ImportReport is the full pre-posting report for one imported
// MT941 balance report. Amounts are signed cents (credit positive,
// debit negative — the C/D mark folded in), so the report reads
// without re-deriving the marks. BalanceMismatches names the :62F:
// balance that disagreed with the ledger at import time (LG-444);
// :64: is compared through :62F:, which the parser already proved
// it restates. Duplicate is true when the report is a replay of an
// earlier import of the same document (see ImportMT941).
type MT941ImportReport struct {
	DocumentSHA256        string                 `json:"document_sha256"`
	Account               AccountID              `json:"account"`
	TransactionReference  string                 `json:"transaction_reference"`
	StatementNumber       string                 `json:"statement_number"`
	SequenceNumber        string                 `json:"sequence_number"`
	ReportDateTime        time.Time              `json:"report_date_time"`
	Currency              string                 `json:"currency"`
	HasOpeningBalance     bool                   `json:"has_opening_balance"`
	OpeningBalanceCents   int64                  `json:"opening_balance_cents"`
	DebitCount            int                    `json:"debit_count"`
	DebitTotalCents       int64                  `json:"debit_total_cents"`
	CreditCount           int                    `json:"credit_count"`
	CreditTotalCents      int64                  `json:"credit_total_cents"`
	ClosingBalanceCents   int64                  `json:"closing_balance_cents"`
	AvailableBalanceCents int64                  `json:"available_balance_cents"`
	ForwardBalanceCents   int64                  `json:"forward_balance_cents"`
	Supplementary         string                 `json:"supplementary,omitempty"`
	BalanceMismatches     []MT941BalanceMismatch `json:"balance_mismatches,omitempty"`
	Duplicate             bool                   `json:"duplicate,omitempty"`
}

// mt941Signed folds a balance tag's C/D mark into signed cents:
// a credit balance reads positive, a debit balance negative.
func mt941Signed(b MT940Balance) int64 {
	if b.Credit {
		return b.AmountCents
	}
	return -b.AmountCents
}

// mt941ParseAmount parses an MT941 amount body (`<integer>,<ff>`)
// into cents. The integer part holds at most 13 digits (the 15d
// rule: 13 integer digits plus 2 fraction digits) with no leading
// zero unless it is exactly "0"; the fraction part holds exactly
// two digits. Unlike mt202ParseAmount, zero is allowed — a
// balance report may legitimately report a zero balance.
func mt941ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrInvalidMT941, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || len(fracPart) != 2 || len(intPart) > 13 {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT941, body)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: amount %q has a leading zero", ErrInvalidMT941, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT941, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT941, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT941, body)
	}
	return whole*100 + frac, nil
}

// mt941ParseBalance parses one balance tag body
// (`C|D` + YYMMDD + 3-letter currency + amount) into its
// MT940Balance. The mark carries the sign; the amount is absolute.
func mt941ParseBalance(body, tag string) (MT940Balance, error) {
	if len(body) < 1+6+3+4 {
		return MT940Balance{}, fmt.Errorf("%w: %s too short: %q", ErrInvalidMT941, tag, body)
	}
	var b MT940Balance
	switch body[0] {
	case 'C':
		b.Credit = true
	case 'D':
		b.Credit = false
	default:
		return MT940Balance{}, fmt.Errorf("%w: %s: bad debit/credit mark %q", ErrInvalidMT941, tag, body[:1])
	}
	vd, err := time.Parse("060102", body[1:7])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: bad value date %q: %v", ErrInvalidMT941, tag, body[1:7], err)
	}
	b.ValueDate = vd.UTC()
	currency, err := normalizeCurrency(body[7:10])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: bad currency %q", ErrInvalidMT941, tag, body[7:10])
	}
	b.Currency = currency
	cents, err := mt941ParseAmount(body[10:])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: %v", ErrInvalidMT941, tag, err)
	}
	b.AmountCents = cents
	return b, nil
}

// mt941ParseReportDateTime parses a :13D: body (YYMMDDHHMM, UTC)
// into the report instant. time.Parse rejects an impossible
// month, day, hour, or minute — a report dated at an invalid
// instant cannot be compared with the ledger.
func mt941ParseReportDateTime(body string) (time.Time, error) {
	if len(body) != 10 {
		return time.Time{}, fmt.Errorf("%w: :13D: want YYMMDDHHMM, got %q", ErrInvalidMT941, body)
	}
	at, err := time.Parse("0601021504", body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: :13D: %q: %v", ErrInvalidMT941, body, err)
	}
	return at.UTC(), nil
}

// mt941SummaryCandidates enumerates the (count, amount) splits a
// :90D:/:90C: body admits: the count is 1–5 digits, the amount's
// integer part 1–13 digits, neither with a leading zero unless it
// is exactly "0". See the package comment for why the split is
// resolved by the reconciliation equation, not by position.
func mt941SummaryCandidates(body, tag string) ([]MT942Summary, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 || len(parts[1]) != 2 {
		return nil, fmt.Errorf("%w: %s: want <count><amount>,<2 fraction digits>, got %q", ErrInvalidMT941, tag, body)
	}
	digits, fracPart := parts[0], parts[1]
	if digits == "" {
		return nil, fmt.Errorf("%w: %s: empty body %q", ErrInvalidMT941, tag, body)
	}
	for _, c := range digits + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %s: bad summary %q", ErrInvalidMT941, tag, body)
		}
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: bad summary %q", ErrInvalidMT941, tag, body)
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
		return nil, fmt.Errorf("%w: %s: no valid (count, amount) split in %q", ErrInvalidMT941, tag, body)
	}
	return out, nil
}

// ParseMT941 parses a SWIFT FIN MT941 balance report message into
// its document model. It is the inverse of RenderMT941Statement:
// :20: (transaction reference, 16x), :25: (account
// identification, 35x), :28C: (statement/sequence number),
// :13D: (report date/time, YYMMDDHHMM), an optional :60F:
// (opening balance), the :90D:/:90C: number-and-sum trailers,
// :62F: (book balance), :64: (available balance), :65: (forward
// available balance), and an optional trailing :86: (information
// to account owner). Parsing is strict: the field order is
// canonical, every field value is validated, and anything outside
// the field set is refused — a balance report must not lose
// content quietly. CRLF line endings are accepted; a trailing
// newline after the last field is tolerated.
//
// The self-consistency checks mirror RenderMT941Statement: the
// closing balance must equal opening + credits − debits, and both
// :64: and :65: must restate :62F: (same signed amount and same
// currency). All balance tags must name one currency — an MT941
// report is single-currency, and a mixed-currency report cannot
// be compared with the ledger. A report that does not foot is
// never parsed.
func ParseMT941(text string) (*MT941Statement, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidMT941)
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
			return "", fmt.Errorf("%w: want %s field, got end of document", ErrInvalidMT941, tag)
		}
		line := lines[i]
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrInvalidMT941, i+1, tag, line)
		}
		i++
		return line[len(tag):], nil
	}

	stmt := &MT941Statement{}

	// :20: — the transaction reference, the idempotency key.
	ref, err := expect(":20:")
	if err != nil {
		return nil, err
	}
	if err := mt202Reference(ref, ":20:"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMT941, err)
	}
	stmt.TransactionReference = ref

	// :25: — the account identification, 35x.
	acct, err := expect(":25:")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(acct) == "" {
		return nil, fmt.Errorf("%w: :25: account identification required", ErrInvalidMT941)
	}
	if len([]rune(acct)) > 35 {
		return nil, fmt.Errorf("%w: :25: exceeds 35x: %q", ErrInvalidMT941, acct)
	}
	stmt.AccountIdentification = acct

	// :28C: — statement/sequence number, both parts required.
	s28, err := expect(":28C:")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s28, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("%w: :28C: want <statement>/<sequence>, got %q", ErrInvalidMT941, s28)
	}
	stmt.StatementNumber = parts[0]
	stmt.SequenceNumber = parts[1]

	// :13D: — the report date/time; it must be a valid instant.
	dt13, err := expect(":13D:")
	if err != nil {
		return nil, err
	}
	at, err := mt941ParseReportDateTime(dt13)
	if err != nil {
		return nil, err
	}
	stmt.ReportDateTime = at

	// :60F: — the opening balance, optional (omitted when the
	// account has no pre-window rows — the reconciliation then
	// reads the opening as zero).
	if i < len(lines) && strings.HasPrefix(lines[i], ":60F:") {
		body, err := expect(":60F:")
		if err != nil {
			return nil, err
		}
		opening, err := mt941ParseBalance(body, ":60F:")
		if err != nil {
			return nil, err
		}
		stmt.OpeningBalance = &opening
	}

	// :90D:/:90C: — the number-and-sum trailers. The split is
	// resolved by the reconciliation equation below, after :62F:
	// is known.
	dBody, err := expect(":90D:")
	if err != nil {
		return nil, err
	}
	debitCands, err := mt941SummaryCandidates(dBody, ":90D:")
	if err != nil {
		return nil, err
	}
	cBody, err := expect(":90C:")
	if err != nil {
		return nil, err
	}
	creditCands, err := mt941SummaryCandidates(cBody, ":90C:")
	if err != nil {
		return nil, err
	}

	// :62F: — the book balance.
	cBody62, err := expect(":62F:")
	if err != nil {
		return nil, err
	}
	closing, err := mt941ParseBalance(cBody62, ":62F:")
	if err != nil {
		return nil, err
	}
	stmt.ClosingBalance = closing

	// :64: and :65: — available and forward available balances:
	// they must restate :62F: exactly, same signed amount and
	// same currency (the ledger has no intraday credit-line or
	// forward concept).
	aBody, err := expect(":64:")
	if err != nil {
		return nil, err
	}
	avail, err := mt941ParseBalance(aBody, ":64:")
	if err != nil {
		return nil, err
	}
	if mt941Signed(avail) != mt941Signed(closing) || avail.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :64: does not restate :62F:", ErrInvalidMT941)
	}
	stmt.AvailableBalance = avail
	fBody, err := expect(":65:")
	if err != nil {
		return nil, err
	}
	fwd, err := mt941ParseBalance(fBody, ":65:")
	if err != nil {
		return nil, err
	}
	if mt941Signed(fwd) != mt941Signed(closing) || fwd.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :65: does not restate :62F:", ErrInvalidMT941)
	}
	stmt.ForwardBalance = fwd

	// :86: — free text, verbatim; only the trailing field may
	// carry it, and nothing may follow it.
	if i < len(lines) {
		if !strings.HasPrefix(lines[i], ":86:") {
			return nil, fmt.Errorf("%w: line %d: unexpected field %q", ErrInvalidMT941, i+1, lines[i])
		}
		body, err := expect(":86:")
		if err != nil {
			return nil, err
		}
		if body == "" {
			return nil, fmt.Errorf("%w: :86: field is empty", ErrInvalidMT941)
		}
		stmt.Supplementary = body
	}
	if i != len(lines) {
		return nil, fmt.Errorf("%w: line %d: unexpected trailing content %q", ErrInvalidMT941, i+1, lines[i])
	}

	// Single-currency report: the opening balance, when present,
	// must name the closing balance's currency.
	if stmt.OpeningBalance != nil && stmt.OpeningBalance.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :60F: currency %q does not match :62F: currency %q", ErrInvalidMT941, stmt.OpeningBalance.Currency, closing.Currency)
	}

	// Reconciliation: closing == opening + credits − debits.
	// The split-pair that foots is the reading; a report with no
	// footing split is unbalanced, and one with two is ambiguous.
	openingSigned := int64(0)
	if stmt.OpeningBalance != nil {
		openingSigned = mt941Signed(*stmt.OpeningBalance)
	}
	closingSigned := mt941Signed(closing)
	matches := 0
	var chosenD, chosenC MT942Summary
	for _, d := range debitCands {
		for _, c := range creditCands {
			if openingSigned+c.AmountCents-d.AmountCents == closingSigned {
				chosenD, chosenC = d, c
				matches++
			}
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and trailers :90D: %q / :90C: %q",
			ErrInvalidMT941, closingSigned, openingSigned, dBody, cBody)
	}
	if matches > 1 {
		return nil, fmt.Errorf("%w: :90D:/:90C: split is ambiguous in %q / %q", ErrInvalidMT941, dBody, cBody)
	}
	stmt.DebitSummary = chosenD
	stmt.CreditSummary = chosenC
	return stmt, nil
}

// mt941ResolveAccountLocked resolves a :25: account identification
// to the ledger account it names: the internal ID directly, else
// an external identifier registered under the mt941 system
// (usually the IBAN the bank names — see ExportMT941), else a
// registered IBAN. Callers must hold l.mu (either lock suffices).
func (l *Ledger) mt941ResolveAccountLocked(id string) (AccountID, error) {
	id = strings.TrimSpace(id)
	if acct := AccountID(id); l.mt941KnownAccountLocked(acct) {
		return acct, nil
	}
	for k := range l.externalMappings {
		if k.system == ExternalSystemMT941 && k.externalID == id {
			return k.account, nil
		}
	}
	if acct, ok := l.accountByIBANLocked(normalizeIBAN(id)); ok {
		return acct, nil
	}
	return "", fmt.Errorf("%w: %q", ErrMT941UnknownAccount, id)
}

// mt941KnownAccountLocked reports whether acct is a known ledger
// account: it carries journaled rows, a registered IBAN, or an
// external mapping (the camt.057 resolve rule). Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt941KnownAccountLocked(acct AccountID) bool {
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

// ImportMT941 imports a SWIFT FIN MT941 balance report as a
// pre-posting reconciliation report for account: it parses the
// document, resolves the :25: account (which must be the import
// target), and compares the :62F: book balance — :64: restates it
// by construction — at the :13D: instant with the balance the
// journal folds at the same instant. Nothing is posted, no hash
// chain is entered, and the journal version does not move — the
// report is a draft for the operator to review.
//
// Balances: a :62F: that disagrees with the ledger lands in
// BalanceMismatches and never fails the import — the report is
// evidence, not a booking. The :90D:/:90C: summaries and the :86:
// text ride along in the report verbatim.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns
// the original report with Duplicate set — a re-uploaded report
// never double-counts.
//
// A report naming a different account than the import target is
// an error: importing another account's report into this one would
// corrupt the reconciliation. The account's currency is the
// report's single currency (ParseMT941 already refused a
// mixed-currency report), so the comparison folds the journal in
// that currency.
func (l *Ledger) ImportMT941(account AccountID, data []byte) (*MT941ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	stmt, err := ParseMT941(string(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	resolved, err := l.mt941ResolveAccountLocked(stmt.AccountIdentification)
	if err != nil {
		return nil, err
	}
	if resolved != account {
		return nil, fmt.Errorf("%w: report names account %q, import targets %q", ErrInvalidMT941, stmt.AccountIdentification, account)
	}
	if !l.mt941KnownAccountLocked(resolved) {
		return nil, fmt.Errorf("%w: %q", ErrMT941UnknownAccount, stmt.AccountIdentification)
	}

	if prev, ok := l.mt941Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	reported := mt941Signed(stmt.ClosingBalance)
	report := &MT941ImportReport{
		DocumentSHA256:        key,
		Account:               account,
		TransactionReference:  stmt.TransactionReference,
		StatementNumber:       stmt.StatementNumber,
		SequenceNumber:        stmt.SequenceNumber,
		ReportDateTime:        stmt.ReportDateTime,
		Currency:              stmt.ClosingBalance.Currency,
		DebitCount:            stmt.DebitSummary.Count,
		DebitTotalCents:       stmt.DebitSummary.AmountCents,
		CreditCount:           stmt.CreditSummary.Count,
		CreditTotalCents:      stmt.CreditSummary.AmountCents,
		ClosingBalanceCents:   reported,
		AvailableBalanceCents: mt941Signed(stmt.AvailableBalance),
		ForwardBalanceCents:   mt941Signed(stmt.ForwardBalance),
		Supplementary:         stmt.Supplementary,
	}
	if stmt.OpeningBalance != nil {
		report.HasOpeningBalance = true
		report.OpeningBalanceCents = mt941Signed(*stmt.OpeningBalance)
	}
	// :62F: is the book balance at the :13D: instant: compare it
	// with the ledger balance folded at the same instant. A
	// disagreement is reported, never fail-closed.
	if got := l.balanceAtTimeLocked(account, stmt.ClosingBalance.Currency, stmt.ReportDateTime); got != reported {
		report.BalanceMismatches = append(report.BalanceMismatches, MT941BalanceMismatch{
			Currency:      stmt.ClosingBalance.Currency,
			ReportedCents: reported,
			LedgerCents:   got,
			At:            stmt.ReportDateTime,
		})
	}

	stored := *report
	stored.BalanceMismatches = append([]MT941BalanceMismatch(nil), report.BalanceMismatches...)
	if l.mt941Imports == nil {
		l.mt941Imports = make(map[string]*MT941ImportReport)
	}
	l.mt941Imports[key] = &stored
	return report, nil
}
