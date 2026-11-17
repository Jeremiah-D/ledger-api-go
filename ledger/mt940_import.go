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

// SWIFT FIN MT940 customer statement import (LG-445): the inbound half
// of the MT940 lifecycle, the FIN version of the camt.053 import
// (LG-103). A bank hands us an MT940 customer statement; ParseMT940
// parses it and ImportMT940 returns a pre-posting reconciliation
// report: every :61: line is matched against the journal by its
// reference, and the :62F: closing balance is compared with the
// ledger balance folded at the end of the closing day.
//
// Like the camt.052 import (LG-408) and the MT941 import (LG-444),
// this is deliberately a draft, not a posting: it writes no journal
// rows, enters no hash chain, and bumps no version. A :61: line that
// disagrees with its journal row, or a :62F: that disagrees with the
// ledger balance, is reported, never fail-closed — the import is
// evidence, not a booking — and the ledger is never adjusted to
// close the gap. Duplicate imports of the same document are
// idempotent via the document's SHA-256 (see mt940Imports on
// Ledger). A successful first import emits one
// `mt940_import_received` audit event (details document_sha256 /
// account / statement_number, actor `ImportMT940`) — receipt only,
// no posting; a replay emits no second event, like the MT300
// confirmation marker.
//
// ParseMT940 is the inverse of RenderMT940Statement by
// construction: it accepts the bare tag block ExportMT940 renders
// (:20: :25: :28C: :60F: (:61: [:86:])* :62F: :64:, no :65: —
// MT940Statement carries no forward balance, unlike MT941), in
// canonical order. There is no {1:}/{2:}/{3:} envelope to check —
// ExportMT940 renders none, so demanding one would fail the
// round-trip the parser exists for. Within the block the parsing
// is strict along ParseMT202/ParseMT300/ParseMT941: the field
// order, every field value, and the self-consistency of the
// statement (closing == opening + signed lines, :64: restating
// :62F:, one currency) are validated, and anything outside the
// field set is refused. CRLF line endings are accepted; a trailing
// newline after the last field is tolerated.
//
// The :25: account must name a known ledger account: the internal
// ID directly, an external identifier registered under the mt940
// system (usually the IBAN the bank names — see ExportMT940), or
// a registered IBAN. An unknown account is refused fail-closed
// with ErrMT940UnknownAccount (the MT101 :50H: gate, along
// ErrMT941UnknownAccount) — the ledger never reconciles a
// statement for an account it does not hold.

// ErrInvalidMT940 is returned by ParseMT940 and ImportMT940 when
// the input is not a readable MT940 statement: empty input, a field
// out of order or outside the field set, a malformed :20:/:25:/
// :28C: value, a :61: line with a bad date, mark, amount, or
// reference, a balance tag with a bad mark, date, currency, or
// amount, balance tags in mixed currencies, a :64: that does not
// restate :62F:, an unbalanced statement, or a statement naming a
// different account than the import target. The HTTP layer maps it
// to 400.
var ErrInvalidMT940 = errors.New("ledger: invalid MT940 document")

// ErrMT940UnknownAccount is returned by ImportMT940 when the :25:
// account names no ledger account: no journaled rows, no
// registered IBAN, and no mt940-system external mapping. The HTTP
// layer maps it to 422.
var ErrMT940UnknownAccount = errors.New("ledger: mt940 inbound: account is unknown")

// MT940BalanceMismatch is one :62F: closing balance that disagrees
// with the ledger: the balance the statement claims
// (ReportedCents) against the balance the journal folds at the end
// of the closing day (LedgerCents). Reported-only: the import
// never adjusts the books to close the gap.
type MT940BalanceMismatch struct {
	Currency      string    `json:"currency"`
	ReportedCents int64     `json:"reported_cents"`
	LedgerCents   int64     `json:"ledger_cents"`
	At            time.Time `json:"at"`
}

// MT940LineMatch is one :61: statement line with its reconciliation
// outcome against the journal. Status reuses the camt.053 match
// vocabulary: matched (exactly one journaled row with this
// reference, equal amount and direction), mismatched (one row with
// this reference but a different amount or direction, or several
// rows sharing the truncated reference), unmatched (no journaled
// row carries this reference). EntryID names the journaled row
// when one was found; Reason explains a mismatch.
type MT940LineMatch struct {
	Reference        string             `json:"reference"`
	ValueDate        time.Time          `json:"value_date"`
	AmountCents      int64              `json:"amount_cents"`
	Credit           bool               `json:"credit"`
	Status           CAMT053MatchStatus `json:"status"`
	EntryID          string             `json:"entry_id,omitempty"`
	EntryAmountCents int64              `json:"entry_amount_cents,omitempty"`
	EntryCredit      bool               `json:"entry_credit,omitempty"`
	CandidateCount   int                `json:"candidate_count,omitempty"`
	Reason           string             `json:"reason,omitempty"`
}

// MT940ImportReport is the full pre-posting report for one imported
// MT940 customer statement. Amounts are signed cents (credit
// positive, debit negative — the C/D mark folded in), so the report
// reads without re-deriving the marks. LineMatches carries every
// :61: line with its match outcome; BalanceMismatches names the
// :62F: balance that disagreed with the ledger at import time.
// Duplicate is true when the report is a replay of an earlier
// import of the same document (see ImportMT940).
type MT940ImportReport struct {
	DocumentSHA256        string                 `json:"document_sha256"`
	Account               AccountID              `json:"account"`
	TransactionReference  string                 `json:"transaction_reference"`
	StatementNumber       string                 `json:"statement_number"`
	SequenceNumber        string                 `json:"sequence_number"`
	Currency              string                 `json:"currency"`
	OpeningBalanceCents   int64                  `json:"opening_balance_cents"`
	ClosingBalanceCents   int64                  `json:"closing_balance_cents"`
	AvailableBalanceCents int64                  `json:"available_balance_cents"`
	Lines                 []MT940LineMatch       `json:"lines"`
	Matched               int                    `json:"matched"`
	Mismatched            int                    `json:"mismatched"`
	Unmatched             int                    `json:"unmatched"`
	BalanceMismatches     []MT940BalanceMismatch `json:"balance_mismatches,omitempty"`
	Duplicate             bool                   `json:"duplicate,omitempty"`
}

// mt940Signed folds a balance tag's C/D mark into signed cents: a
// credit balance reads positive, a debit balance negative.
func mt940Signed(b MT940Balance) int64 {
	if b.Credit {
		return b.AmountCents
	}
	return -b.AmountCents
}

// mt940ParseAmount parses an MT940 amount body (`<integer>,<ff>`)
// into cents. The integer part holds at most 13 digits (the 15d
// rule: 13 integer digits plus 2 fraction digits) with no leading
// zero unless it is exactly "0"; the fraction part holds exactly
// two digits. Zero is rejected by the :61: line parser (a bank
// line without an amount is not a bank line); balance tags use
// this helper through mt940ParseBalance, where zero is allowed.
func mt940ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrInvalidMT940, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || len(fracPart) != 2 || len(intPart) > 13 {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT940, body)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: amount %q has a leading zero", ErrInvalidMT940, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT940, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT940, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT940, body)
	}
	return whole*100 + frac, nil
}

// mt940ParseBalance parses one balance tag body
// (`C|D` + YYMMDD + 3-letter currency + amount) into its
// MT940Balance. The mark carries the sign; the amount is absolute.
func mt940ParseBalance(body, tag string) (MT940Balance, error) {
	if len(body) < 1+6+3+4 {
		return MT940Balance{}, fmt.Errorf("%w: %s too short: %q", ErrInvalidMT940, tag, body)
	}
	var b MT940Balance
	switch body[0] {
	case 'C':
		b.Credit = true
	case 'D':
		b.Credit = false
	default:
		return MT940Balance{}, fmt.Errorf("%w: %s: bad debit/credit mark %q", ErrInvalidMT940, tag, body[:1])
	}
	vd, err := time.Parse("060102", body[1:7])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: bad value date %q: %v", ErrInvalidMT940, tag, body[1:7], err)
	}
	b.ValueDate = vd.UTC()
	currency, err := normalizeCurrency(body[7:10])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: bad currency %q", ErrInvalidMT940, tag, body[7:10])
	}
	b.Currency = currency
	cents, err := mt940ParseAmount(body[10:])
	if err != nil {
		return MT940Balance{}, fmt.Errorf("%w: %s: %v", ErrInvalidMT940, tag, err)
	}
	b.AmountCents = cents
	return b, nil
}

// mt940ParseLine parses one :61: body (YYMMDD + MMDD + D/C mark +
// 15d comma amount + "NTRF//" + 1–16x reference) into its
// MT940Line. The entry date carries no year on the wire; it takes
// the value date's year, and the month/day must be a real calendar
// date in that year. The amount must be positive — a zero-amount
// line is not a bank line, matching mt940LineOf.
func mt940ParseLine(body string) (MT940Line, error) {
	if len(body) < 6+4+1+4 {
		return MT940Line{}, fmt.Errorf("%w: :61: too short: %q", ErrInvalidMT940, body)
	}
	valueDate, err := time.Parse("060102", body[0:6])
	if err != nil {
		return MT940Line{}, fmt.Errorf("%w: :61: bad value date %q: %v", ErrInvalidMT940, body[0:6], err)
	}
	valueDate = valueDate.UTC()
	entryMonth, err := strconv.Atoi(body[6:8])
	if err != nil || entryMonth < 1 || entryMonth > 12 {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry month %q", ErrInvalidMT940, body[6:8])
	}
	entryDay, err := strconv.Atoi(body[8:10])
	if err != nil || entryDay < 1 || entryDay > 31 {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry day %q", ErrInvalidMT940, body[8:10])
	}
	entryDate := time.Date(valueDate.Year(), time.Month(entryMonth), entryDay, 0, 0, 0, 0, time.UTC)
	if int(entryDate.Month()) != entryMonth || entryDate.Day() != entryDay {
		return MT940Line{}, fmt.Errorf("%w: :61: bad entry date %q", ErrInvalidMT940, body[6:10])
	}
	var line MT940Line
	switch body[10] {
	case 'C':
		line.Credit = true
	case 'D':
		line.Credit = false
	default:
		return MT940Line{}, fmt.Errorf("%w: :61: bad debit/credit mark %q", ErrInvalidMT940, body[10:11])
	}
	rest := body[11:]
	sep := strings.Index(rest, "NTRF//")
	if sep < 0 {
		return MT940Line{}, fmt.Errorf("%w: :61: want NTRF// reference, got %q", ErrInvalidMT940, body)
	}
	cents, err := mt940ParseAmount(rest[:sep])
	if err != nil {
		return MT940Line{}, fmt.Errorf("%w: :61: %v", ErrInvalidMT940, err)
	}
	if cents <= 0 {
		return MT940Line{}, fmt.Errorf("%w: :61: amount must be positive, got %q", ErrInvalidMT940, rest[:sep])
	}
	line.AmountCents = cents
	ref := rest[sep+len("NTRF//"):]
	if ref == "" || len([]rune(ref)) > 16 {
		return MT940Line{}, fmt.Errorf("%w: :61: want 1-16x reference, got %q", ErrInvalidMT940, ref)
	}
	if strings.TrimSpace(ref) != ref || strings.ContainsAny(ref, " \t\r\n") {
		return MT940Line{}, fmt.Errorf("%w: :61: reference carries whitespace: %q", ErrInvalidMT940, ref)
	}
	line.ValueDate = valueDate
	line.EntryDate = entryDate
	line.Reference = ref
	return line, nil
}

// ParseMT940 parses a SWIFT FIN MT940 customer statement message
// into its document model. It is the inverse of
// RenderMT940Statement: :20: (transaction reference, 16x), :25:
// (account identification, 35x), :28C: (statement/sequence
// number), :60F: (opening balance), one :61: (+ optional :86:)
// per statement line, :62F: (closing balance), :64: (available
// balance, restating :62F:). There is no :65: — MT940Statement
// carries no forward balance, and a :65: field is refused like
// any other out-of-block field. Parsing is strict: the field
// order is canonical, every field value is validated, and
// anything outside the field set is refused — a statement must
// not lose content quietly. CRLF line endings are accepted; a
// trailing newline after the last field is tolerated.
//
// The self-consistency checks mirror RenderMT940Statement: the
// closing balance must equal opening + signed lines, and :64:
// must restate :62F: (same signed amount and same currency). All
// balance tags must name one currency — an MT940 statement is
// single-currency. A statement that does not foot is never parsed.
func ParseMT940(text string) (*MT940Statement, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidMT940)
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
			return "", fmt.Errorf("%w: want %s field, got end of document", ErrInvalidMT940, tag)
		}
		line := lines[i]
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrInvalidMT940, i+1, tag, line)
		}
		i++
		return line[len(tag):], nil
	}

	stmt := &MT940Statement{}

	// :20: — the transaction reference, the idempotency key.
	ref, err := expect(":20:")
	if err != nil {
		return nil, err
	}
	if err := mt202Reference(ref, ":20:"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMT940, err)
	}
	stmt.TransactionReference = ref

	// :25: — the account identification, 35x.
	acct, err := expect(":25:")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(acct) == "" {
		return nil, fmt.Errorf("%w: :25: account identification required", ErrInvalidMT940)
	}
	if len([]rune(acct)) > 35 {
		return nil, fmt.Errorf("%w: :25: exceeds 35x: %q", ErrInvalidMT940, acct)
	}
	stmt.AccountIdentification = acct

	// :28C: — statement/sequence number, both parts required.
	s28, err := expect(":28C:")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s28, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("%w: :28C: want <statement>/<sequence>, got %q", ErrInvalidMT940, s28)
	}
	stmt.StatementNumber = parts[0]
	stmt.SequenceNumber = parts[1]

	// :60F: — the opening balance, always present on a rendered
	// MT940 statement (ExportMT940 folds the pre-window rows
	// into it).
	openingBody, err := expect(":60F:")
	if err != nil {
		return nil, err
	}
	opening, err := mt940ParseBalance(openingBody, ":60F:")
	if err != nil {
		return nil, err
	}
	stmt.OpeningBalance = opening

	// (:61: [:86:])* — the statement lines, each with optional
	// supplementary details.
	for i < len(lines) && strings.HasPrefix(lines[i], ":61:") {
		body, err := expect(":61:")
		if err != nil {
			return nil, err
		}
		line, err := mt940ParseLine(body)
		if err != nil {
			return nil, err
		}
		if i < len(lines) && strings.HasPrefix(lines[i], ":86:") {
			supp, err := expect(":86:")
			if err != nil {
				return nil, err
			}
			if supp == "" {
				return nil, fmt.Errorf("%w: :86: field is empty", ErrInvalidMT940)
			}
			line.Supplementary = supp
		}
		stmt.Lines = append(stmt.Lines, line)
	}

	// :62F: — the closing balance.
	closingBody, err := expect(":62F:")
	if err != nil {
		return nil, err
	}
	closing, err := mt940ParseBalance(closingBody, ":62F:")
	if err != nil {
		return nil, err
	}
	stmt.ClosingBalance = closing

	// :64: — the available balance: it must restate :62F:
	// exactly, same signed amount and same currency (the ledger
	// has no intraday credit-line concept).
	availBody, err := expect(":64:")
	if err != nil {
		return nil, err
	}
	avail, err := mt940ParseBalance(availBody, ":64:")
	if err != nil {
		return nil, err
	}
	if mt940Signed(avail) != mt940Signed(closing) || avail.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :64: does not restate :62F:", ErrInvalidMT940)
	}
	stmt.AvailableBalance = avail

	// Nothing may follow :64:.
	if i != len(lines) {
		return nil, fmt.Errorf("%w: line %d: unexpected trailing content %q", ErrInvalidMT940, i+1, lines[i])
	}

	// Single-currency statement: the opening balance must name
	// the closing balance's currency.
	if stmt.OpeningBalance.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :60F: currency %q does not match :62F: currency %q", ErrInvalidMT940, stmt.OpeningBalance.Currency, closing.Currency)
	}

	// Reconciliation: closing == opening + signed lines.
	signed := func(credit bool, cents int64) int64 {
		if credit {
			return cents
		}
		return -cents
	}
	expected := signed(stmt.OpeningBalance.Credit, stmt.OpeningBalance.AmountCents)
	for _, l := range stmt.Lines {
		expected += signed(l.Credit, l.AmountCents)
	}
	got := signed(stmt.ClosingBalance.Credit, stmt.ClosingBalance.AmountCents)
	if expected != got {
		return nil, fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and %d lines",
			ErrInvalidMT940, got, signed(stmt.OpeningBalance.Credit, stmt.OpeningBalance.AmountCents), len(stmt.Lines))
	}
	return stmt, nil
}

// mt940ResolveAccountLocked resolves a :25: account identification
// to the ledger account it names: the internal ID directly, else
// an external identifier registered under the mt940 system
// (usually the IBAN the bank names — see ExportMT940), else a
// registered IBAN. Callers must hold l.mu (either lock suffices).
func (l *Ledger) mt940ResolveAccountLocked(id string) (AccountID, error) {
	id = strings.TrimSpace(id)
	if acct := AccountID(id); l.mt940KnownAccountLocked(acct) {
		return acct, nil
	}
	for k := range l.externalMappings {
		if k.system == ExternalSystemMT940 && k.externalID == id {
			return k.account, nil
		}
	}
	if acct, ok := l.accountByIBANLocked(normalizeIBAN(id)); ok {
		return acct, nil
	}
	return "", fmt.Errorf("%w: %q", ErrMT940UnknownAccount, id)
}

// mt940KnownAccountLocked reports whether acct is a known ledger
// account: it carries journaled rows, a registered IBAN, or an
// external mapping (the camt.057 resolve rule). Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt940KnownAccountLocked(acct AccountID) bool {
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

// mt940MatchLineLocked matches one :61: statement line against the
// account's effective journal rows by reference: the entry whose
// mt940LineRef (idempotency key, else entry ID) the statement
// truncates to 16x. Pending-review rows never touched balances and
// are excluded, like the export. The :61: line carries no currency,
// so candidates must be in the statement's currency. Callers must
// hold l.mu (either lock suffices).
func (l *Ledger) mt940MatchLineLocked(account AccountID, currency string, line MT940Line) MT940LineMatch {
	match := MT940LineMatch{
		Reference:   line.Reference,
		ValueDate:   line.ValueDate,
		AmountCents: line.AmountCents,
		Credit:      line.Credit,
	}
	var cands []JournalEntry
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != currency {
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
			match.Reason = fmt.Sprintf("amount differs: statement %d, journal %d", line.AmountCents, e.AmountCents)
		} else {
			match.Reason = "direction differs: statement and journal disagree on the C/D mark"
		}
	default:
		match.Status = CAMT053Mismatched
		match.CandidateCount = len(cands)
		match.Reason = fmt.Sprintf("reference matches %d journal entries", len(cands))
	}
	return match
}

// ImportMT940 imports a SWIFT FIN MT940 customer statement as a
// pre-posting reconciliation report for account: it parses the
// document, resolves the :25: account (which must be the import
// target), matches every :61: line against the journal by its
// reference, and compares the :62F: closing balance — :64:
// restates it by construction — with the ledger balance folded at
// the end of the closing day. Nothing is posted, no hash chain is
// entered, and the journal version does not move — the report is a
// draft for the operator to review.
//
// Line matches: a line whose reference names no journaled row is
// unmatched; one whose row disagrees on amount or direction is
// mismatched, with both sides in the report. Neither fails the
// import — the report is evidence, not a booking. Balances: a
// :62F: that disagrees with the ledger lands in
// BalanceMismatches and never fails the import.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns
// the original report with Duplicate set — a re-uploaded statement
// never double-counts, and no second audit event is emitted.
//
// A statement naming a different account than the import target
// is an error: importing another account's statement into this one
// would corrupt the reconciliation. The statement's currency is
// the report's single currency (ParseMT940 already refused a
// mixed-currency statement), so the comparison folds the journal
// in that currency.
func (l *Ledger) ImportMT940(account AccountID, data []byte) (*MT940ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	stmt, err := ParseMT940(string(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	resolved, err := l.mt940ResolveAccountLocked(stmt.AccountIdentification)
	if err != nil {
		return nil, err
	}
	if resolved != account {
		return nil, fmt.Errorf("%w: statement names account %q, import targets %q", ErrInvalidMT940, stmt.AccountIdentification, account)
	}
	if !l.mt940KnownAccountLocked(resolved) {
		return nil, fmt.Errorf("%w: %q", ErrMT940UnknownAccount, stmt.AccountIdentification)
	}

	if prev, ok := l.mt940Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	reported := mt940Signed(stmt.ClosingBalance)
	report := &MT940ImportReport{
		DocumentSHA256:        key,
		Account:               account,
		TransactionReference:  stmt.TransactionReference,
		StatementNumber:       stmt.StatementNumber,
		SequenceNumber:        stmt.SequenceNumber,
		Currency:              stmt.ClosingBalance.Currency,
		OpeningBalanceCents:   mt940Signed(stmt.OpeningBalance),
		ClosingBalanceCents:   reported,
		AvailableBalanceCents: mt940Signed(stmt.AvailableBalance),
		Lines:                 []MT940LineMatch{},
	}
	for _, line := range stmt.Lines {
		m := l.mt940MatchLineLocked(account, stmt.ClosingBalance.Currency, line)
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
	// :62F: is the closing balance at the end of its value date:
	// compare it with the ledger balance folded at the start of
	// the next day, so every row dated on the closing day counts.
	// A disagreement is reported, never fail-closed.
	foldAt := stmt.ClosingBalance.ValueDate.AddDate(0, 0, 1)
	if got := l.balanceAtTimeLocked(account, stmt.ClosingBalance.Currency, foldAt); got != reported {
		report.BalanceMismatches = append(report.BalanceMismatches, MT940BalanceMismatch{
			Currency:      stmt.ClosingBalance.Currency,
			ReportedCents: reported,
			LedgerCents:   got,
			At:            foldAt,
		})
	}

	stored := *report
	stored.Lines = append([]MT940LineMatch(nil), report.Lines...)
	stored.BalanceMismatches = append([]MT940BalanceMismatch(nil), report.BalanceMismatches...)
	if l.mt940Imports == nil {
		l.mt940Imports = make(map[string]*MT940ImportReport)
	}
	l.mt940Imports[key] = &stored
	// Receipt only: one audit event records the import, with the
	// version unchanged — nothing was posted.
	l.emitAudit(AuditEvent{
		Op:            "mt940_import_received",
		Actor:         "ImportMT940",
		TraceID:       stmt.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"document_sha256":  key,
			"account":          string(account),
			"statement_number": stmt.StatementNumber,
		},
	})
	return report, nil
}
