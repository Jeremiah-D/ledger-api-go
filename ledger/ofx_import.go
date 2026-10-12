package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OFX statement import (LG-66): the inverse of ExportOFX (see ofx.go).
// A bank (or this ledger's own export) hands us an OFX 2.x statement;
// ImportOFX parses it and returns a pre-posting report: every statement
// line becomes a DraftEntry the operator can review, matched against
// the journaled rows for the same account.
//
// The import is deliberately a draft, not a posting: it writes no
// journal rows, enters no hash chain, and bumps no version. Booking
// external money movement is an operator decision — the report only
// says "here is what the statement claims, and here is what the
// ledger already has". Duplicate imports of the same document are
// idempotent via the document's SHA-256 (see ofxImports on Ledger).

// ErrInvalidOFX is returned by ParseOFX and ImportOFX when the input is
// not a readable OFX statement: empty input, no statement segments, a
// transaction missing a required field (FITID, TRNAMT, DTPOSTED), an
// unparseable amount or date, or a statement naming a different account
// than the import target. The HTTP layer maps it to 400.
var ErrInvalidOFX = errors.New("ledger: invalid OFX document")

// DefaultOFXMatchWindow is the default ± window around a statement
// line's DTPOSTED inside which a journaled row counts as a match
// candidate (see OFXImportOptions).
const DefaultOFXMatchWindow = 24 * time.Hour

// OFXDirection is the money-movement direction of a statement line,
// account-centric like ExportOFX: "in" means the account was the debit
// leg (money added to it), "out" means it was the credit leg (money
// left it).
type OFXDirection string

const (
	// OFXDirectionIn: TRNTYPE CREDIT (positive amount) — the account
	// is the debit leg, funds flowed in.
	OFXDirectionIn OFXDirection = "in"
	// OFXDirectionOut: TRNTYPE DEBIT (negative amount) — the account
	// is the credit leg, funds flowed out.
	OFXDirectionOut OFXDirection = "out"
)

// OFXTransaction is one parsed STMTTRN statement line. AmountCents is
// the absolute value in cents — the sign lives in Direction, mirroring
// how ExportOFX renders TRNTYPE/TRNAMT from the account's leg.
type OFXTransaction struct {
	Account     AccountID    `json:"account"`
	Currency    string       `json:"currency"`
	FITID       string       `json:"fitid"`
	Direction   OFXDirection `json:"direction"`
	AmountCents int64        `json:"amount_cents"`
	PostedAt    time.Time    `json:"posted_at"`
	Memo        string       `json:"memo,omitempty"`
	Ref         string       `json:"ref,omitempty"`
}

// DraftEntry is the pre-posting form of a statement line: the OFX side
// of the money movement, without a booked counterparty. The account
// named by the statement occupies its leg (debit for inflows, credit
// for outflows); the other leg is empty — the operator supplies the
// counterparty when they decide to book it. The ID is "ofx/<FITID>",
// stable across re-imports of the same statement.
type DraftEntry struct {
	ID            string    `json:"id"`
	DebitAccount  AccountID `json:"debit_account,omitempty"`
	CreditAccount AccountID `json:"credit_account,omitempty"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	Memo          string    `json:"memo,omitempty"`
	ExternalRef   string    `json:"external_ref,omitempty"`
}

// Draft renders the transaction as its pre-posting draft entry.
func (t OFXTransaction) Draft() DraftEntry {
	d := DraftEntry{
		ID:          "ofx/" + t.FITID,
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		Memo:        t.Memo,
		ExternalRef: t.Ref,
	}
	if t.Direction == OFXDirectionIn {
		d.DebitAccount = t.Account
	} else {
		d.CreditAccount = t.Account
	}
	return d
}

// OFXMatchStatus is the reconciliation outcome of one statement line
// against the journal.
type OFXMatchStatus string

const (
	// OFXMatched: exactly one journaled row matches the line — the
	// statement line is already booked.
	OFXMatched OFXMatchStatus = "matched"
	// OFXMismatched: several journaled rows match the line — the
	// operator must disambiguate.
	OFXMismatched OFXMatchStatus = "mismatched"
	// OFXUnmatched: no journaled row matches — a candidate for
	// booking.
	OFXUnmatched OFXMatchStatus = "unmatched"
)

// OFXImportItem is one statement line with its draft and its match
// outcome.
type OFXImportItem struct {
	Transaction OFXTransaction `json:"transaction"`
	Draft       DraftEntry     `json:"draft"`
	Status      OFXMatchStatus `json:"status"`
	// MatchedEntryID names the journaled row when Status is matched.
	MatchedEntryID string `json:"matched_entry_id,omitempty"`
	// CandidateCount names how many journaled rows competed when
	// Status is mismatched.
	CandidateCount int `json:"candidate_count,omitempty"`
}

// OFXImportReport is the full pre-posting report for one imported
// statement. Duplicate is true when the report is a replay of an
// earlier import of the same document (see ImportOFX).
type OFXImportReport struct {
	DocumentSHA256 string          `json:"document_sha256"`
	Account        AccountID      `json:"account"`
	Items          []OFXImportItem `json:"items"`
	Matched        int             `json:"matched"`
	Mismatched     int             `json:"mismatched"`
	Unmatched      int             `json:"unmatched"`
	Duplicate      bool            `json:"duplicate"`
}

// OFXImportOptions tunes ImportOFX. MatchWindow is the ± window around
// each statement line's DTPOSTED inside which a journaled row of equal
// |amount| counts as a candidate; zero means DefaultOFXMatchWindow.
type OFXImportOptions struct {
	MatchWindow time.Duration
}

// ofxToken is one <TAG>content pair from the OFX body, in document
// order. Closing tags surface as "/TAG" with empty content so the
// assembler can track nesting; the tokenizer itself skips nothing else.
type ofxToken struct {
	tag     string
	content string
}

// stripOFXHeader drops the SGML header block and processing
// instructions that precede the OFX body: the OFXHEADER:... header
// lines, the <?xml ...?> and <?OFX ...?> PIs, and blank lines. The body
// starts at the first line beginning with '<'; a document with no
// header block (body first) passes through unchanged.
func stripOFXHeader(data []byte) string {
	var sb strings.Builder
	inBody := false
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if !inBody {
			if t == "" || strings.HasPrefix(t, "<?") {
				continue
			}
			if strings.HasPrefix(t, "<") {
				inBody = true
			} else {
				// A header line (OFXHEADER:100, DATA:OFXSGML, ...):
				// skipped until the body starts.
				continue
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// tokenizeOFXBody walks the OFX body and yields (TAG, content) pairs in
// document order. It handles both OFX shapes in the wild:
//
//   - SGML style (OFX 1.x): <TAG>content with no closing tag; content
//     runs to the next '<'.
//   - XML style (OFX 2.x): <TAG>content</TAG>; the closing tag is
//     skipped and content still runs to the next '<'.
//
// Tag names are upper-cased (OFX tags are case-insensitive; the
// exporter writes upper case) and XML entities in content are
// unescaped. A truncated final tag ends the scan rather than inventing
// content.
func tokenizeOFXBody(body string) []ofxToken {
	var toks []ofxToken
	i := 0
	for i < len(body) {
		lt := strings.IndexByte(body[i:], '<')
		if lt < 0 {
			break
		}
		i += lt + 1
		// The tag name runs to '>' or whitespace (attributes never
		// carry data in OFX; being lenient here costs nothing).
		j := i
		for j < len(body) && body[j] != '>' && body[j] != ' ' && body[j] != '\t' && body[j] != '\n' && body[j] != '\r' {
			j++
		}
		tag := strings.ToUpper(body[i:j])
		gt := strings.IndexByte(body[j:], '>')
		if gt < 0 {
			break
		}
		j += gt + 1
		if strings.HasPrefix(tag, "/") || strings.HasPrefix(tag, "?") || strings.HasPrefix(tag, "!") {
			// Closing tag (content was captured at the opening tag),
			// processing instruction, or comment/doctype: the
			// closer still marks nesting for the assembler.
			if strings.HasPrefix(tag, "/") {
				toks = append(toks, ofxToken{tag: tag})
			}
			i = j
			continue
		}
		// Content runs to the next '<' (both SGML and XML styles) or
		// to the end of input.
		k := strings.IndexByte(body[j:], '<')
		var content string
		if k < 0 {
			content = body[j:]
			i = len(body)
		} else {
			content = body[j : j+k]
			i = j + k
		}
		toks = append(toks, ofxToken{tag: tag, content: ofxUnescape(strings.TrimSpace(content))})
	}
	return toks
}

// ofxEntityReplacer unescapes the five XML entities the exporter
// writes (see ofxEscape); numeric character references are handled by
// ofxUnescape below.
var ofxEntityReplacer = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", "\"",
	"&apos;", "'",
)

// ofxUnescape reverses ofxEscape: the five named entities plus decimal
// (&#65;) and hexadecimal (&#x41;) character references. Unknown
// entities are left as-is — a tolerant reader does not fail a whole
// statement over one memo's typo.
func ofxUnescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	s = ofxEntityReplacer.Replace(s)
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' || !strings.HasPrefix(s[i:], "&#") {
			sb.WriteByte(s[i])
			i++
			continue
		}
		semi := strings.IndexByte(s[i:], ';')
		if semi < 0 {
			sb.WriteString(s[i:])
			break
		}
		num := s[i+2 : i+semi]
		var v int64
		var err error
		if strings.HasPrefix(num, "x") || strings.HasPrefix(num, "X") {
			v, err = strconv.ParseInt(num[1:], 16, 32)
		} else {
			v, err = strconv.ParseInt(num, 10, 32)
		}
		if err != nil || v < 0 || v > 0x10FFFF {
			sb.WriteString(s[i : i+semi+1])
		} else {
			sb.WriteRune(rune(v))
		}
		i += semi + 1
	}
	return sb.String()
}

// ofxRawTxn accumulates one STMTTRN's raw fields; parsing to typed
// values happens once the transaction closes, so a single bad field
// fails the whole document instead of half-importing it.
type ofxRawTxn struct {
	trnType  string
	dtPosted string
	trnAmt   string
	fitID    string
	memo     string
	ref      string
}

// parseOFXAmount converts a decimal amount string ("-1.05", "150.00",
// "100", ".5") to integer cents with no floats. The sign is preserved.
// More than two fraction digits round half away from zero; anything
// that is not a decimal number is an error.
func parseOFXAmount(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("%w: bad TRNAMT %q", ErrInvalidOFX, s)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return bad()
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return bad()
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return bad()
		}
	}
	dollars, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || dollars > (1<<62)/100 {
		return bad()
	}
	// Round to cents half away from zero via the third fraction digit.
	padded := fracPart + "000"
	cents, err := strconv.ParseInt(padded[:2], 10, 64)
	if err != nil {
		return bad()
	}
	if padded[2] >= '5' {
		cents++
	}
	total := dollars*100 + cents
	if neg {
		total = -total
	}
	return total, nil
}

// parseOFXDate parses an OFX datetime: the 14-digit YYYYMMDDHHMMSS
// stamp or the 8-digit YYYYMMDD date, in UTC. A fractional-seconds or
// "[offset:tz]" suffix after the stamp is tolerated (the leading digit
// run decides); anything else is an error.
func parseOFXDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	digits := s[:n]
	switch {
	case len(digits) >= 14:
		t, err := time.Parse("20060102150405", digits[:14])
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: bad DTPOSTED %q", ErrInvalidOFX, s)
		}
		return t, nil
	case len(digits) >= 8:
		t, err := time.Parse("20060102", digits[:8])
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: bad DTPOSTED %q", ErrInvalidOFX, s)
		}
		return t, nil
	default:
		return time.Time{}, fmt.Errorf("%w: bad DTPOSTED %q", ErrInvalidOFX, s)
	}
}

// ofxDirectionOf maps TRNTYPE/TRNAMT to the account-centric direction,
// mirroring ExportOFX: CREDIT (positive) means the account was the
// debit leg — money in; DEBIT (negative) means it was the credit leg —
// money out. Statement types outside the CREDIT/DEBIT pair (INT, FEE,
// XFER, ...) fall back to the amount's sign, which is what those
// statements mean by it; a missing TRNTYPE does the same. Zero amounts
// read as inflows.
func ofxDirectionOf(trnType string, signedCents int64) OFXDirection {
	switch strings.ToUpper(strings.TrimSpace(trnType)) {
	case "CREDIT":
		return OFXDirectionIn
	case "DEBIT":
		return OFXDirectionOut
	default:
		if signedCents < 0 {
			return OFXDirectionOut
		}
		return OFXDirectionIn
	}
}

// ParseOFX parses an OFX 2.x statement document (SGML or XML body
// style, with or without the SGML header block) into its statement
// lines. It is the inverse of ExportOFX: a document the exporter wrote
// parses back to the same FITIDs, directions, and amounts.
//
// Per statement segment it collects CURDEF and ACCTID; per STMTTRN it
// collects TRNTYPE, DTPOSTED, TRNAMT, FITID, MEMO, and REFNUM. A
// transaction missing FITID, TRNAMT, or DTPOSTED — or carrying a value
// that does not parse — makes the whole document invalid (a statement
// the reader cannot fully understand must not half-import). A document
// with no statement lines at all is likewise invalid: there is nothing
// to import.
func ParseOFX(data []byte) ([]OFXTransaction, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidOFX)
	}
	toks := tokenizeOFXBody(stripOFXHeader(data))

	type segment struct {
		account  AccountID
		currency string
		raw      []ofxRawTxn
	}
	var segments []segment
	var cur *segment
	var txn *ofxRawTxn

	for _, tk := range toks {
		switch tk.tag {
		case "STMTTRNRS":
			// A new opening tag also closes the previous segment:
			// SGML-style documents never close anything.
			segments = append(segments, segment{})
			cur = &segments[len(segments)-1]
			txn = nil
		case "/STMTTRNRS":
			cur, txn = nil, nil
		case "STMTTRN":
			if cur == nil {
				return nil, fmt.Errorf("%w: STMTTRN outside a statement", ErrInvalidOFX)
			}
			// Same rule: a new STMTTRN closes the previous one in
			// SGML style; in XML style the explicit closer already
			// cleared txn.
			cur.raw = append(cur.raw, ofxRawTxn{})
			txn = &cur.raw[len(cur.raw)-1]
		case "/STMTTRN":
			txn = nil
		default:
			if txn != nil {
				switch tk.tag {
				case "TRNTYPE":
					txn.trnType = tk.content
				case "DTPOSTED":
					txn.dtPosted = tk.content
				case "TRNAMT":
					txn.trnAmt = tk.content
				case "FITID":
					txn.fitID = tk.content
				case "MEMO":
					txn.memo = tk.content
				case "REFNUM":
					txn.ref = tk.content
				}
			} else if cur != nil {
				switch tk.tag {
				case "CURDEF":
					cur.currency = tk.content
				case "ACCTID":
					cur.account = AccountID(tk.content)
				}
			}
		}
	}

	var out []OFXTransaction
	for _, seg := range segments {
		if seg.currency == "" {
			return nil, fmt.Errorf("%w: statement segment without CURDEF", ErrInvalidOFX)
		}
		currency, err := normalizeCurrency(seg.currency)
		if err != nil {
			return nil, fmt.Errorf("%w: bad CURDEF %q", ErrInvalidOFX, seg.currency)
		}
		for _, r := range seg.raw {
			if r.fitID == "" {
				return nil, fmt.Errorf("%w: STMTTRN without FITID", ErrInvalidOFX)
			}
			signed, err := parseOFXAmount(r.trnAmt)
			if err != nil {
				return nil, err
			}
			posted, err := parseOFXDate(r.dtPosted)
			if err != nil {
				return nil, err
			}
			dir := ofxDirectionOf(r.trnType, signed)
			amount := signed
			if amount < 0 {
				amount = -amount
			}
			out = append(out, OFXTransaction{
				Account:     seg.account,
				Currency:    currency,
				FITID:       r.fitID,
				Direction:   dir,
				AmountCents: amount,
				PostedAt:    posted,
				Memo:        r.memo,
				Ref:         r.ref,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no statement transactions found", ErrInvalidOFX)
	}
	return out, nil
}

// ImportOFX imports an OFX statement as a pre-posting report for
// account: it parses the document, drafts every line, and matches each
// line against the account's journaled rows. Nothing is posted, no hash
// chain is entered, and the journal version does not move — the report
// is a draft for the operator to review.
//
// Matching: a journaled row is a candidate for a statement line when
// it is effective (pending-review rows never touched balances, so they
// cannot match a bank statement — the same rule ExportOFX applies),
// carries the same currency, has equal |amount|, and its CreatedAt
// falls within ±MatchWindow of the line's DTPOSTED. Exactly one
// candidate is matched; several is mismatched; none is unmatched.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns the
// original report with Duplicate set — a re-uploaded statement never
// double-counts.
//
// A statement naming a different account than the import target is an
// error: importing another account's statement into this one would
// corrupt the reconciliation.
func (l *Ledger) ImportOFX(account AccountID, data []byte, opts OFXImportOptions) (*OFXImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	txns, err := ParseOFX(data)
	if err != nil {
		return nil, err
	}
	for _, t := range txns {
		if t.Account != account {
			return nil, fmt.Errorf("%w: statement names account %q, import targets %q", ErrInvalidOFX, t.Account, account)
		}
	}
	window := opts.MatchWindow
	if window <= 0 {
		window = DefaultOFXMatchWindow
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	if prev, ok := l.ofxImports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	report := &OFXImportReport{
		DocumentSHA256: key,
		Account:        account,
		Items:          []OFXImportItem{},
	}
	for _, t := range txns {
		cands := l.ofxMatchCandidatesLocked(account, t, window)
		item := OFXImportItem{Transaction: t, Draft: t.Draft()}
		switch len(cands) {
		case 0:
			item.Status = OFXUnmatched
			report.Unmatched++
		case 1:
			item.Status = OFXMatched
			item.MatchedEntryID = cands[0]
			report.Matched++
		default:
			item.Status = OFXMismatched
			item.CandidateCount = len(cands)
			report.Mismatched++
		}
		report.Items = append(report.Items, item)
	}

	stored := *report
	stored.Items = append([]OFXImportItem(nil), report.Items...)
	if l.ofxImports == nil {
		l.ofxImports = make(map[string]*OFXImportReport)
	}
	l.ofxImports[key] = &stored
	return report, nil
}

// ofxMatchCandidatesLocked returns the IDs of effective journaled rows
// for (account, currency) whose |amount| equals the statement line's
// and whose CreatedAt falls within ±window of the line's DTPOSTED.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) ofxMatchCandidatesLocked(account AccountID, t OFXTransaction, window time.Duration) []string {
	var out []string
	lo := t.PostedAt.Add(-window)
	hi := t.PostedAt.Add(window)
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != t.Currency {
			continue
		}
		amt := e.AmountCents
		if amt < 0 {
			amt = -amt
		}
		if amt != t.AmountCents {
			continue
		}
		if e.CreatedAt.Before(lo) || e.CreatedAt.After(hi) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// ofxImportsLocked renders the OFX import registry as sorted snapshot
// records (see snapshotConfigLine). Callers must hold l.mu (either
// lock suffices).
func (l *Ledger) ofxImportsLocked() []OFXImportRecord {
	if len(l.ofxImports) == 0 {
		return nil
	}
	out := make([]OFXImportRecord, 0, len(l.ofxImports))
	for sha, rep := range l.ofxImports {
		out = append(out, OFXImportRecord{DocumentSHA256: sha, Report: *rep})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocumentSHA256 < out[j].DocumentSHA256 })
	return out
}

// OFXImportRecord is one entry of the OFX import registry in a
// snapshot: the document's SHA-256 and the report ImportOFX produced
// for it. Restoring it preserves duplicate-import idempotency across
// disaster recovery.
type OFXImportRecord struct {
	DocumentSHA256 string          `json:"document_sha256"`
	Report         OFXImportReport `json:"report"`
}
