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

// SWIFT FIN MT950 interbank statement import (LG-450): the inbound
// half of the MT950 lifecycle, the FIN version of the camt.053
// import (LG-408) for the interbank side. A correspondent bank
// hands us an MT950 interbank statement; ParseMT950 parses it and
// ImportMT950 returns a pre-posting reconciliation report: every
// :61: line is matched against the journal by its reference — or,
// failing that, against an outbound MT202 export record (LG-488) — and
// the :60F: opening balance and the :62F: closing balance are each
// compared with the balance the journal folds at the same instant.
//
// Like the camt.052 import, the MT941 import (LG-444) and the
// MT940 import (LG-445), this is deliberately a draft, not a
// posting: it writes no journal rows, enters no hash chain, and
// bumps no version. A :61: line that disagrees with its journal
// row, or a balance that disagrees with the journal fold, is
// reported, never fail-closed — the import is evidence, not a
// booking — and the ledger is never adjusted to close the gap.
// Duplicate imports of the same document are idempotent via the
// document's SHA-256 (see mt950Imports on Ledger). No audit event
// is emitted, along the MT941 import (LG-444, the named template
// for this item).
//
// ParseMT950 is the inverse of RenderMT950Statement by
// construction: it accepts the bare tag block ExportMT950 renders
// (:20: :25: :28C: :60F: (:61: [:86:])* :62F: :64: :65:
// [:86:]), in canonical order. There is no {1:}/{2:}/{3:}
// envelope to check — ExportMT950 renders none, so demanding one
// would fail the round-trip the parser exists for. Within the
// block the parsing is strict along ParseMT202/ParseMT300/
// ParseMT941: the field order, every field value, and the
// self-consistency of the statement (the closing balance must
// equal opening + signed lines, and both :64: and :65: must
// restate :62F: with the same signed amount and the same
// currency, exactly as RenderMT950Statement re-derives them) are
// validated, and anything outside the field set is refused — an
// interbank statement must not lose content quietly. CRLF line
// endings are accepted; a trailing newline after the last field
// is tolerated.
//
// An MT950 statement is single-currency: every balance tag names
// one currency, and a mixed-currency statement cannot be compared
// with the ledger. The :61: lines carry no currency, so line
// matching filters on the statement's currency, exactly like
// MT940.
//
// The :25: account must name a known ledger account: the internal
// ID directly, an external identifier registered under the mt950
// system (usually the account number the correspondent bank
// expects — see ExportMT950), or a registered IBAN. An unknown
// account is refused fail-closed with ErrMT950UnknownAccount (the
// MT101 :50H: gate, along ErrMT940UnknownAccount/
// ErrMT941UnknownAccount) — the ledger never reconciles a
// statement for an account it does not hold.

// ErrInvalidMT950 is returned by ParseMT950 and ImportMT950 when
// the input is not a readable MT950 statement: empty input, a
// field out of order or outside the field set, a malformed :20:/
// :25:/:28C: value, a balance tag with a bad mark, date, currency,
// or amount, balance tags in mixed currencies, a :64: or :65:
// that does not restate :62F:, a :61: line with a bad date, mark,
// amount, or reference, a statement whose closing balance does
// not reconcile with the opening balance and the lines, or a
// statement naming a different account than the import target.
// The HTTP layer maps it to 400.
var ErrInvalidMT950 = errors.New("ledger: invalid MT950 document")

// ErrMT950UnknownAccount is returned by ImportMT950 when the :25:
// account names no ledger account: no journaled rows, no
// registered IBAN, and no mt950-system external mapping. The HTTP
// layer maps it to 422.
var ErrMT950UnknownAccount = errors.New("ledger: mt950 inbound: account is unknown")

// MT950BalanceMismatch is one balance (:60F: or :62F:) that
// disagrees with the ledger: the balance the statement claims
// (ReportedCents) against the balance the journal folds at the
// same instant (LedgerCents). Reported-only: the import never
// adjusts the books to close the gap.
type MT950BalanceMismatch struct {
	Currency      string    `json:"currency"`
	ReportedCents int64     `json:"reported_cents"`
	LedgerCents   int64     `json:"ledger_cents"`
	At            time.Time `json:"at"`
}

// MT950LineMatch is one :61: statement line with its
// reconciliation outcome against the journal. Status reuses the
// camt.053 match vocabulary: matched (exactly one journaled row
// with this reference, equal amount and direction), mismatched
// (one row with this reference but a different amount or
// direction, or several rows sharing the truncated reference),
// unmatched (no journaled row carries this reference). A line
// whose reference names no journal row but an outbound MT202
// this ledger rendered is matched against the export record
// instead (LG-488): amount, currency and direction are
// compared, EntryID stays empty — the export posts no journal
// row — and ExportReference carries the :20:. EntryID names the
// journaled row when one was found; Reason explains a mismatch.
// Supplementary carries the line's :86: text verbatim when the
// bank attached one.
type MT950LineMatch struct {
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
	ExportReference  string             `json:"export_reference,omitempty"`
	Reason           string             `json:"reason,omitempty"`
}

// MT950ImportReport is the full pre-posting report for one imported
// MT950 interbank statement. Amounts are signed cents (credit
// positive, debit negative — the C/D mark folded in), so the report
// reads without re-deriving the marks. LineMatches carries every
// :61: line with its match outcome; BalanceMismatches names the
// :60F:/:62F: balances that disagreed with the ledger at import
// time (:64:/:65: are compared through :62F:, which the parser
// already proved they restate). OwnerInfo carries the trailing
// :86: text verbatim when the statement carries one. Duplicate is
// true when the report is a replay of an earlier import of the
// same document (see ImportMT950).
type MT950ImportReport struct {
	DocumentSHA256        string                 `json:"document_sha256"`
	Account               AccountID              `json:"account"`
	TransactionReference  string                 `json:"transaction_reference"`
	StatementNumber       string                 `json:"statement_number"`
	SequenceNumber        string                 `json:"sequence_number"`
	Currency              string                 `json:"currency"`
	OpeningBalanceCents   int64                  `json:"opening_balance_cents"`
	ClosingBalanceCents   int64                  `json:"closing_balance_cents"`
	AvailableBalanceCents int64                  `json:"available_balance_cents"`
	ForwardBalanceCents   int64                  `json:"forward_balance_cents"`
	OwnerInfo             string                 `json:"owner_info,omitempty"`
	Lines                 []MT950LineMatch       `json:"lines"`
	Matched               int                    `json:"matched"`
	Mismatched            int                    `json:"mismatched"`
	Unmatched             int                    `json:"unmatched"`
	BalanceMismatches     []MT950BalanceMismatch `json:"balance_mismatches,omitempty"`
	Duplicate             bool                   `json:"duplicate,omitempty"`
}

// mt950Signed folds a balance tag's C/D mark into signed cents:
// a credit balance reads positive, a debit balance negative.
func mt950Signed(b MT950Balance) int64 {
	if b.Credit {
		return b.AmountCents
	}
	return -b.AmountCents
}

// mt950ParseAmount parses an MT950 amount body (`<integer>,<ff>`)
// into cents. The integer part holds at most 13 digits (the 15d
// rule: 13 integer digits plus 2 fraction digits) with no leading
// zero unless it is exactly "0"; the fraction part holds exactly
// two digits. Zero is allowed — a balance tag may legitimately
// report a zero balance; the :61: line parser rejects zero
// separately.
func mt950ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrInvalidMT950, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || len(fracPart) != 2 || len(intPart) > 13 {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT950, body)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: amount %q has a leading zero", ErrInvalidMT950, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT950, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT950, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad amount %q", ErrInvalidMT950, body)
	}
	return whole*100 + frac, nil
}

// mt950ParseBalance parses one balance tag body
// (`C|D` + YYMMDD + 3-letter currency + amount) into its
// MT950Balance. The mark carries the sign; the amount is absolute.
func mt950ParseBalance(body, tag string) (MT950Balance, error) {
	if len(body) < 1+6+3+4 {
		return MT950Balance{}, fmt.Errorf("%w: %s too short: %q", ErrInvalidMT950, tag, body)
	}
	var b MT950Balance
	switch body[0] {
	case 'C':
		b.Credit = true
	case 'D':
		b.Credit = false
	default:
		return MT950Balance{}, fmt.Errorf("%w: %s: bad debit/credit mark %q", ErrInvalidMT950, tag, body[:1])
	}
	vd, err := time.Parse("060102", body[1:7])
	if err != nil {
		return MT950Balance{}, fmt.Errorf("%w: %s: bad value date %q: %v", ErrInvalidMT950, tag, body[1:7], err)
	}
	b.ValueDate = vd.UTC()
	currency, err := normalizeCurrency(body[7:10])
	if err != nil {
		return MT950Balance{}, fmt.Errorf("%w: %s: bad currency %q", ErrInvalidMT950, tag, body[7:10])
	}
	b.Currency = currency
	cents, err := mt950ParseAmount(body[10:])
	if err != nil {
		return MT950Balance{}, fmt.Errorf("%w: %s: %v", ErrInvalidMT950, tag, err)
	}
	b.AmountCents = cents
	return b, nil
}

// mt950ParseLine parses one :61: body (YYMMDD + MMDD + D/C mark +
// 15d comma amount + "NTRF//" + 1–16x reference) into its
// MT950Line. It mirrors mt940ParseLine in the MT950 error domain:
// the MT940 import is untouched (LG-450 excludes it), so the
// parser is duplicated here rather than re-wrapped — a bad :61:
// in an MT950 document is an ErrInvalidMT950, never an
// ErrInvalidMT940. The entry date carries no year on the wire;
// it takes the value date's year, and the month/day must be a
// real calendar date in that year. The amount must be positive —
// a zero-amount line is not a bank line, matching mt940LineOf.
func mt950ParseLine(body string) (MT950Line, error) {
	if len(body) < 6+4+1+4 {
		return MT950Line{}, fmt.Errorf("%w: :61: too short: %q", ErrInvalidMT950, body)
	}
	valueDate, err := time.Parse("060102", body[0:6])
	if err != nil {
		return MT950Line{}, fmt.Errorf("%w: :61: bad value date %q: %v", ErrInvalidMT950, body[0:6], err)
	}
	valueDate = valueDate.UTC()
	entryMonth, err := strconv.Atoi(body[6:8])
	if err != nil || entryMonth < 1 || entryMonth > 12 {
		return MT950Line{}, fmt.Errorf("%w: :61: bad entry month %q", ErrInvalidMT950, body[6:8])
	}
	entryDay, err := strconv.Atoi(body[8:10])
	if err != nil || entryDay < 1 || entryDay > 31 {
		return MT950Line{}, fmt.Errorf("%w: :61: bad entry day %q", ErrInvalidMT950, body[8:10])
	}
	entryDate := time.Date(valueDate.Year(), time.Month(entryMonth), entryDay, 0, 0, 0, 0, time.UTC)
	if int(entryDate.Month()) != entryMonth || entryDate.Day() != entryDay {
		return MT950Line{}, fmt.Errorf("%w: :61: bad entry date %q", ErrInvalidMT950, body[6:10])
	}
	var line MT950Line
	switch body[10] {
	case 'C':
		line.Credit = true
	case 'D':
		line.Credit = false
	default:
		return MT950Line{}, fmt.Errorf("%w: :61: bad debit/credit mark %q", ErrInvalidMT950, body[10:11])
	}
	rest := body[11:]
	sep := strings.Index(rest, "NTRF//")
	if sep < 0 {
		return MT950Line{}, fmt.Errorf("%w: :61: want NTRF// reference, got %q", ErrInvalidMT950, body)
	}
	cents, err := mt950ParseAmount(rest[:sep])
	if err != nil {
		return MT950Line{}, fmt.Errorf("%w: :61: %v", ErrInvalidMT950, err)
	}
	if cents <= 0 {
		return MT950Line{}, fmt.Errorf("%w: :61: amount must be positive, got %q", ErrInvalidMT950, rest[:sep])
	}
	line.AmountCents = cents
	ref := rest[sep+len("NTRF//"):]
	if ref == "" || len([]rune(ref)) > 16 {
		return MT950Line{}, fmt.Errorf("%w: :61: want 1-16x reference, got %q", ErrInvalidMT950, ref)
	}
	if strings.TrimSpace(ref) != ref || strings.ContainsAny(ref, " \t\r\n") {
		return MT950Line{}, fmt.Errorf("%w: :61: reference carries whitespace: %q", ErrInvalidMT950, ref)
	}
	line.ValueDate = valueDate
	line.EntryDate = entryDate
	line.Reference = ref
	return line, nil
}

// ParseMT950 parses a SWIFT FIN MT950 interbank statement message
// into its document model. It is the inverse of
// RenderMT950Statement: :20: (transaction reference, 16x), :25:
// (account identification, 35x), :28C: (statement/sequence
// number), :60F: (opening balance), one :61: (+ optional :86:)
// per statement line, :62F: (closing balance), :64: (available
// balance), :65: (forward available balance), and an optional
// trailing :86: (information to account owner). Parsing is
// strict: the field order is canonical, every field value is
// validated, and anything outside the field set is refused — an
// interbank statement must not lose content quietly. CRLF line
// endings are accepted; a trailing newline after the last field
// is tolerated.
//
// The self-consistency checks mirror RenderMT950Statement: the
// closing balance must equal opening + signed lines, both :64:
// and :65: must restate :62F: (same signed amount and same
// currency), and all balance tags must name one currency. A
// statement that does not foot is never parsed.
func ParseMT950(text string) (*MT950Statement, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidMT950)
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
			return "", fmt.Errorf("%w: want %s field, got end of document", ErrInvalidMT950, tag)
		}
		line := lines[i]
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrInvalidMT950, i+1, tag, line)
		}
		i++
		return line[len(tag):], nil
	}

	stmt := &MT950Statement{}

	// :20: — the transaction reference, the idempotency key.
	ref, err := expect(":20:")
	if err != nil {
		return nil, err
	}
	if err := mt202Reference(ref, ":20:"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMT950, err)
	}
	stmt.TransactionReference = ref

	// :25: — the account identification, 35x.
	acct, err := expect(":25:")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(acct) == "" {
		return nil, fmt.Errorf("%w: :25: account identification required", ErrInvalidMT950)
	}
	if len([]rune(acct)) > 35 {
		return nil, fmt.Errorf("%w: :25: exceeds 35x: %q", ErrInvalidMT950, acct)
	}
	stmt.AccountIdentification = acct

	// :28C: — statement/sequence number, both parts required.
	s28, err := expect(":28C:")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s28, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("%w: :28C: want <statement>/<sequence>, got %q", ErrInvalidMT950, s28)
	}
	stmt.StatementNumber = parts[0]
	stmt.SequenceNumber = parts[1]

	// :60F: — the opening balance. The export always renders it
	// (even a zero opening), so the import requires it too.
	oBody, err := expect(":60F:")
	if err != nil {
		return nil, err
	}
	opening, err := mt950ParseBalance(oBody, ":60F:")
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
		line, err := mt950ParseLine(body)
		if err != nil {
			return nil, err
		}
		if i < len(lines) && strings.HasPrefix(lines[i], ":86:") {
			supp, err := expect(":86:")
			if err != nil {
				return nil, err
			}
			if supp == "" {
				return nil, fmt.Errorf("%w: :86: field is empty", ErrInvalidMT950)
			}
			line.Supplementary = supp
		}
		stmt.Lines = append(stmt.Lines, line)
	}

	// :62F: — the closing balance.
	cBody, err := expect(":62F:")
	if err != nil {
		return nil, err
	}
	closing, err := mt950ParseBalance(cBody, ":62F:")
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
	avail, err := mt950ParseBalance(aBody, ":64:")
	if err != nil {
		return nil, err
	}
	if mt950Signed(avail) != mt950Signed(closing) || avail.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :64: does not restate :62F:", ErrInvalidMT950)
	}
	stmt.AvailableBalance = avail
	fBody, err := expect(":65:")
	if err != nil {
		return nil, err
	}
	fwd, err := mt950ParseBalance(fBody, ":65:")
	if err != nil {
		return nil, err
	}
	if mt950Signed(fwd) != mt950Signed(closing) || fwd.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :65: does not restate :62F:", ErrInvalidMT950)
	}
	stmt.ForwardBalance = fwd

	// :86: — information to account owner, verbatim; only the
	// trailing field may carry it, and nothing may follow it.
	if i < len(lines) {
		if !strings.HasPrefix(lines[i], ":86:") {
			return nil, fmt.Errorf("%w: line %d: unexpected field %q", ErrInvalidMT950, i+1, lines[i])
		}
		body, err := expect(":86:")
		if err != nil {
			return nil, err
		}
		if body == "" {
			return nil, fmt.Errorf("%w: :86: field is empty", ErrInvalidMT950)
		}
		stmt.OwnerInfo = body
	}
	if i != len(lines) {
		return nil, fmt.Errorf("%w: line %d: unexpected trailing content %q", ErrInvalidMT950, i+1, lines[i])
	}

	// Single-currency statement: the opening balance must name
	// the closing balance's currency.
	if opening.Currency != closing.Currency {
		return nil, fmt.Errorf("%w: :60F: currency %q does not match :62F: currency %q", ErrInvalidMT950, opening.Currency, closing.Currency)
	}

	// Reconciliation: closing == opening + signed lines. The
	// amounts are absolute in the model; the C/D marks carry the
	// signs. Saturating add, like RenderMT950Statement: an
	// adversarial line history can never wrap the re-derivation
	// into a false match.
	signed := func(credit bool, cents int64) int64 {
		if credit {
			return cents
		}
		return -cents
	}
	expected := signed(opening.Credit, opening.AmountCents)
	for _, l := range stmt.Lines {
		expected = satAddInt64(expected, signed(l.Credit, l.AmountCents))
	}
	if got := signed(closing.Credit, closing.AmountCents); expected != got {
		return nil, fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and %d lines",
			ErrInvalidMT950, got, signed(opening.Credit, opening.AmountCents), len(stmt.Lines))
	}
	return stmt, nil
}

// mt950ResolveAccountLocked resolves a :25: account identification
// to the ledger account it names: the internal ID directly, else
// an external identifier registered under the mt950 system
// (usually the account number the correspondent bank expects —
// see ExportMT950), else a registered IBAN. Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt950ResolveAccountLocked(id string) (AccountID, error) {
	id = strings.TrimSpace(id)
	if acct := AccountID(id); l.mt950KnownAccountLocked(acct) {
		return acct, nil
	}
	for k := range l.externalMappings {
		if k.system == ExternalSystemMT950 && k.externalID == id {
			return k.account, nil
		}
	}
	if acct, ok := l.accountByIBANLocked(normalizeIBAN(id)); ok {
		return acct, nil
	}
	return "", fmt.Errorf("%w: %q", ErrMT950UnknownAccount, id)
}

// mt950KnownAccountLocked reports whether acct is a known ledger
// account: it carries journaled rows, a registered IBAN, or an
// external mapping (the camt.057 resolve rule). Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) mt950KnownAccountLocked(acct AccountID) bool {
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

// mt950MatchLineLocked matches one :61: statement line against
// the account's effective journal rows by reference: the entry
// whose mt940LineRef (idempotency key, else entry ID) the
// statement truncates to 16x. Pending-review rows never touched
// balances and are excluded, like the export. The :61: line
// carries no currency, so candidates must be in the statement's
// currency. When no journal row carries the reference, the second
// form (LG-488) consults the outbound MT202 export registry
// (mt202Exports, read directly under the held lock — never via
// GetMT202Export, which would take the read lock under this write
// lock and deadlock): a :20: registered there, in the statement's
// currency, is compared on amount and direction, with the :20:
// carried as ExportReference and EntryID left empty — the export
// posts no journal row, so there is no entry to name. The journal
// is always consulted first: a reference that is both a journal
// key and an outbound :20: matches the journal row. An outbound
// MT202 is the credit leg of its own account (money out), so a D
// line agrees with it on direction and a C line does not. Callers
// must hold l.mu (either lock suffices).
func (l *Ledger) mt950MatchLineLocked(account AccountID, currency string, line MT950Line) MT950LineMatch {
	match := MT950LineMatch{
		Reference:     line.Reference,
		ValueDate:     line.ValueDate,
		AmountCents:   line.AmountCents,
		Credit:        line.Credit,
		Supplementary: line.Supplementary,
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
		// Second form (LG-488): the reference names an outbound
		// MT202 this ledger rendered, registered in mt202Exports
		// under its :20:. The registry is read directly under
		// the held lock — never via GetMT202Export, which would
		// take the read lock under this write lock and deadlock.
		// The export is a candidate only in the statement's
		// currency, along the journal path's currency filter: a
		// :20: registered in another currency leaves the line
		// unmatched exactly as if it were unregistered. The
		// export is the credit leg of its own account (money
		// out), so a D line agrees on direction and a C line
		// does not. Only amount and direction are compared —
		// never the value date (the :61: family precedent of
		// not comparing dates).
		if rec, ok := l.mt202Exports[line.Reference]; ok && rec.Request.Currency == currency {
			exportCredit := false
			match.ExportReference = line.Reference
			match.EntryAmountCents = rec.Request.AmountCents
			match.EntryCredit = exportCredit
			if rec.Request.AmountCents == line.AmountCents && exportCredit == line.Credit {
				match.Status = CAMT053Matched
			} else {
				match.Status = CAMT053Mismatched
				if rec.Request.AmountCents != line.AmountCents {
					match.Reason = fmt.Sprintf("amount differs: statement %d, export %d", line.AmountCents, rec.Request.AmountCents)
				} else {
					match.Reason = "direction differs: statement and export disagree on the C/D mark"
				}
			}
		} else {
			match.Status = CAMT053Unmatched
			match.Reason = "no journal entry carries this reference"
		}
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

// ImportMT950 imports a SWIFT FIN MT950 interbank statement as a
// pre-posting reconciliation report for account: it parses the
// document, resolves the :25: account (which must be the import
// target), matches every :61: line against the journal by its
// reference, and compares the :60F: opening balance with the
// ledger balance folded at the start of the opening day and the
// :62F: closing balance — :64:/:65: restate it by construction —
// with the ledger balance folded at the end of the closing day.
// Nothing is posted, no hash chain is entered, and the journal
// version does not move — the report is a draft for the operator
// to review.
//
// Line matches: a line whose reference names no journaled row is
// unmatched — unless it names an outbound MT202 this ledger
// rendered, which is matched against the export record instead
// (LG-488: amount, currency and direction compared, EntryID left
// empty, ExportReference carrying the :20:); one whose row (or
// export) disagrees on amount or direction is mismatched, with
// both sides in the report. Neither fails the import — the report
// is evidence, not a booking. Balances: a :60F: or :62F: that
// disagrees with the ledger lands in BalanceMismatches and never
// fails the import — the balance fold still reads the journal
// only, so a line matched to an outbound MT202 leaves a residual
// equal to its amount there.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns
// the original report with Duplicate set — a re-uploaded statement
// never double-counts.
//
// A statement naming a different account than the import target
// is an error: importing another account's statement into this one
// would corrupt the reconciliation. The statement's currency is
// the report's single currency (ParseMT950 already refused a
// mixed-currency statement), so the comparison folds the journal
// in that currency.
func (l *Ledger) ImportMT950(account AccountID, data []byte) (*MT950ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	stmt, err := ParseMT950(string(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	resolved, err := l.mt950ResolveAccountLocked(stmt.AccountIdentification)
	if err != nil {
		return nil, err
	}
	if resolved != account {
		return nil, fmt.Errorf("%w: statement names account %q, import targets %q", ErrInvalidMT950, stmt.AccountIdentification, account)
	}
	if !l.mt950KnownAccountLocked(resolved) {
		return nil, fmt.Errorf("%w: %q", ErrMT950UnknownAccount, stmt.AccountIdentification)
	}

	if prev, ok := l.mt950Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	currency := stmt.ClosingBalance.Currency
	reportedOpening := mt950Signed(stmt.OpeningBalance)
	reportedClosing := mt950Signed(stmt.ClosingBalance)
	report := &MT950ImportReport{
		DocumentSHA256:        key,
		Account:               account,
		TransactionReference:  stmt.TransactionReference,
		StatementNumber:       stmt.StatementNumber,
		SequenceNumber:        stmt.SequenceNumber,
		Currency:              currency,
		OpeningBalanceCents:   reportedOpening,
		ClosingBalanceCents:   reportedClosing,
		AvailableBalanceCents: mt950Signed(stmt.AvailableBalance),
		ForwardBalanceCents:   mt950Signed(stmt.ForwardBalance),
		OwnerInfo:             stmt.OwnerInfo,
		Lines:                 []MT950LineMatch{},
	}
	for _, line := range stmt.Lines {
		m := l.mt950MatchLineLocked(account, currency, line)
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
	// :60F: is the opening balance at the start of its value
	// date: compare it with the ledger balance folded before
	// that instant, so every row dated on the opening day counts
	// as a line, not as opening. A disagreement is reported,
	// never fail-closed.
	if got := l.balanceAtTimeLocked(account, currency, stmt.OpeningBalance.ValueDate); got != reportedOpening {
		report.BalanceMismatches = append(report.BalanceMismatches, MT950BalanceMismatch{
			Currency:      currency,
			ReportedCents: reportedOpening,
			LedgerCents:   got,
			At:            stmt.OpeningBalance.ValueDate,
		})
	}
	// :62F: is the closing balance at the end of its value date:
	// compare it with the ledger balance folded at the start of
	// the next day, so every row dated on the closing day counts.
	// A disagreement is reported, never fail-closed.
	foldAt := stmt.ClosingBalance.ValueDate.AddDate(0, 0, 1)
	if got := l.balanceAtTimeLocked(account, currency, foldAt); got != reportedClosing {
		report.BalanceMismatches = append(report.BalanceMismatches, MT950BalanceMismatch{
			Currency:      currency,
			ReportedCents: reportedClosing,
			LedgerCents:   got,
			At:            foldAt,
		})
	}

	stored := *report
	stored.Lines = append([]MT950LineMatch(nil), report.Lines...)
	stored.BalanceMismatches = append([]MT950BalanceMismatch(nil), report.BalanceMismatches...)
	if l.mt950Imports == nil {
		l.mt950Imports = make(map[string]*MT950ImportReport)
	}
	l.mt950Imports[key] = &stored
	return report, nil
}
