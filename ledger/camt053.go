package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CAMT.053 bank statement import (LG-103): the ISO 20022 counterpart of
// the OFX import (see ofx_import.go, LG-66). A bank hands us a
// camt.053.001.xx `BankToCustomerStatement` document; ImportCAMT053
// parses it and returns a pre-posting report: every statement entry
// becomes a DraftEntry the operator can review, matched against the
// journaled rows for the same account.
//
// Like the OFX import, this is deliberately a draft, not a posting: it
// writes no journal rows, enters no hash chain, and bumps no version.
// Duplicate imports of the same document are idempotent via the
// document's SHA-256 (see camt053Imports on Ledger).

// ErrInvalidCAMT053 is returned by ParseCAMT053 and ImportCAMT053 when
// the input is not a readable camt.053 statement: empty input, no
// BkToCstmrStmt/Stmt blocks, an entry missing its amount, currency, or
// booking date, an unparseable amount or date, a statement without an
// Id (stable entry IDs need it), or a statement naming a different
// account than the import target. The HTTP layer maps it to 400.
var ErrInvalidCAMT053 = errors.New("ledger: invalid camt.053 document")

// DefaultCAMT053MatchWindow is the default ± window around a statement
// entry's booking date inside which a journaled row counts as a match
// candidate (see CAMT053ImportOptions).
const DefaultCAMT053MatchWindow = 24 * time.Hour

// CAMT053Direction is the money-movement direction of a statement
// entry, account-centric like the OFX import: "in" means the account
// was the debit leg (money added to it), "out" means it was the credit
// leg (money left it).
type CAMT053Direction string

const (
	// CAMT053DirectionIn: CdtDbtInd CRDT — the account is the debit
	// leg, funds flowed in.
	CAMT053DirectionIn CAMT053Direction = "in"
	// CAMT053DirectionOut: CdtDbtInd DBIT — the account is the credit
	// leg, funds flowed out.
	CAMT053DirectionOut CAMT053Direction = "out"
)

// CAMT053Transaction is one parsed Ntry statement entry. AmountCents
// is the absolute value in cents — the sign lives in Direction,
// mirroring the OFX import. Seq is the 1-based entry sequence within
// its statement; StmtID/Seq is the stable entry identity.
type CAMT053Transaction struct {
	Account     AccountID        `json:"account"`
	Currency    string           `json:"currency"`
	StmtID      string           `json:"stmt_id"`
	Seq         int              `json:"seq"`
	Direction   CAMT053Direction `json:"direction"`
	AmountCents int64            `json:"amount_cents"`
	PostedAt    time.Time        `json:"posted_at"`
	Memo        string           `json:"memo,omitempty"`
	EndToEndID  string           `json:"end_to_end_id,omitempty"`
}

// Draft renders the entry as its pre-posting draft entry. The account
// named by the statement occupies its leg (debit for inflows, credit
// for outflows); the other leg is empty — the operator supplies the
// counterparty when they decide to book it. The ID is
// "camt053/<stmtID>/<seq>", stable across re-imports of the same
// statement.
func (t CAMT053Transaction) Draft() DraftEntry {
	d := DraftEntry{
		ID:          fmt.Sprintf("camt053/%s/%d", t.StmtID, t.Seq),
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		Memo:        t.Memo,
		ExternalRef: t.EndToEndID,
	}
	if t.Direction == CAMT053DirectionIn {
		d.DebitAccount = t.Account
	} else {
		d.CreditAccount = t.Account
	}
	return d
}

// CAMT053MatchStatus is the reconciliation outcome of one statement
// entry against the journal.
type CAMT053MatchStatus string

const (
	// CAMT053Matched: exactly one journaled row matches the entry.
	CAMT053Matched CAMT053MatchStatus = "matched"
	// CAMT053Mismatched: several journaled rows match the entry.
	CAMT053Mismatched CAMT053MatchStatus = "mismatched"
	// CAMT053Unmatched: no journaled row matches the entry.
	CAMT053Unmatched CAMT053MatchStatus = "unmatched"
)

// CAMT053ImportItem is one statement entry with its draft and its match
// outcome.
type CAMT053ImportItem struct {
	Transaction CAMT053Transaction `json:"transaction"`
	Draft       DraftEntry        `json:"draft"`
	Status      CAMT053MatchStatus `json:"status"`
	// MatchedEntryID names the journaled row when Status is matched.
	MatchedEntryID string `json:"matched_entry_id,omitempty"`
	// CandidateCount names how many journaled rows competed when
	// Status is mismatched.
	CandidateCount int `json:"candidate_count,omitempty"`
}

// CAMT053ImportReport is the full pre-posting report for one imported
// statement. Duplicate is true when the report is a replay of an
// earlier import of the same document (see ImportCAMT053).
type CAMT053ImportReport struct {
	DocumentSHA256 string             `json:"document_sha256"`
	Account        AccountID         `json:"account"`
	Items          []CAMT053ImportItem `json:"items"`
	Matched        int                `json:"matched"`
	Mismatched     int                `json:"mismatched"`
	Unmatched      int                `json:"unmatched"`
	Duplicate      bool               `json:"duplicate"`
}

// CAMT053ImportOptions tunes ImportCAMT053. MatchWindow is the ± window
// around each statement entry's booking date inside which a journaled
// row of equal |amount| counts as a candidate; zero means
// DefaultCAMT053MatchWindow.
type CAMT053ImportOptions struct {
	MatchWindow time.Duration
}

// The camt053* structs model the ISO 20022 camt.053.001.xx
// BankToCustomerStatement shape. The struct tags name bare local
// element names, so encoding/xml matches them in any namespace —
// banks ship the default `urn:iso:std:iso:20022:tech:xsd:camt.053…`
// namespace, and a tolerant reader does not care which version suffix
// it carries. Unknown elements are skipped, which is what makes the
// parser tolerant of the many optional blocks (Bal, Chrgs, …) real
// statements carry.
type camt053Document struct {
	XMLName     xml.Name           `xml:"Document"`
	BkToCstmrStmt camt053BkToCstmrStmt `xml:"BkToCstmrStmt"`
}

type camt053BkToCstmrStmt struct {
	GrpHdr camt053GrpHdr `xml:"GrpHdr"`
	Stmts  []camt053Stmt `xml:"Stmt"`
}

type camt053GrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type camt053Stmt struct {
	ID      string         `xml:"Id"`
	Acct    camt053Acct    `xml:"Acct"`
	Entries []camt053Ntry  `xml:"Ntry"`
}

type camt053Acct struct {
	ID camt053AcctID `xml:"Id"`
}

type camt053AcctID struct {
	IBAN string      `xml:"IBAN"`
	Othr camt053Othr  `xml:"Othr"`
}

type camt053Othr struct {
	ID string `xml:"Id"`
}

type camt053Ntry struct {
	Amt       camt053Amt  `xml:"Amt"`
	CdtDbtInd string      `xml:"CdtDbtInd"`
	BookgDt   camt053Dt   `xml:"BookgDt"`
	ValDt     camt053Dt   `xml:"ValDt"`
	NtryDtls  camt053Dtls `xml:"NtryDtls"`
}

type camt053Amt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt053Dt struct {
	Dt   string `xml:"Dt"`
	DtTm string `xml:"DtTm"`
}

type camt053Dtls struct {
	TxDtls []camt053TxDtls `xml:"TxDtls"`
}

type camt053TxDtls struct {
	Refs   camt053Refs   `xml:"Refs"`
	RmtInf camt053RmtInf `xml:"RmtInf"`
}

type camt053Refs struct {
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
}

type camt053RmtInf struct {
	Ustrd []string `xml:"Ustrd"`
}

// camt053AccountIDOf resolves the statement's account identifier: the
// IBAN when present, else the Other/Id. Empty when the statement names
// no account at all.
func camt053AccountIDOf(a camt053Acct) string {
	if id := strings.TrimSpace(a.ID.IBAN); id != "" {
		return id
	}
	return strings.TrimSpace(a.ID.Othr.ID)
}

// parseCAMT053Amount converts a decimal amount string ("100.00",
// "-1.05", "100") to integer cents with no floats — the same
// half-away-from-zero rounding as parseOFXAmount. The sign is
// preserved; anything that is not a decimal number is an error.
func parseCAMT053Amount(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("%w: bad Amt %q", ErrInvalidCAMT053, s)
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

// parseCAMT053Date parses a camt.053 date: either the YYYY-MM-DD date
// or the full ISO 8601 datetime. A trailing "Z" or numeric zone offset
// is tolerated; anything else is an error. The result is normalized to
// UTC so snapshot round-trips compare equal under reflect.DeepEqual
// (location-pointer insensitive) and match-window arithmetic is
// zone-free.
func parseCAMT053Date(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("%w: missing booking date", ErrInvalidCAMT053)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// Some banks emit the datetime without a zone designator.
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: bad date %q", ErrInvalidCAMT053, s)
}

// camt053DirectionOf maps CdtDbtInd to the account-centric direction:
// CRDT (credit) means the account was the debit leg — money in; DBIT
// (debit) means it was the credit leg — money out. An absent or
// unrecognized indicator falls back to the amount's sign, like the OFX
// import does for statement types outside the CREDIT/DEBIT pair; zero
// amounts read as inflows.
func camt053DirectionOf(ind string, signedCents int64) CAMT053Direction {
	switch strings.ToUpper(strings.TrimSpace(ind)) {
	case "CRDT":
		return CAMT053DirectionIn
	case "DBIT":
		return CAMT053DirectionOut
	default:
		if signedCents < 0 {
			return CAMT053DirectionOut
		}
		return CAMT053DirectionIn
	}
}

// camt053EntryDate picks the entry's booking date: BookgDt/Dt, then
// BookgDt/DtTm, then ValDt/Dt, then ValDt/DtTm. A missing date is an
// error — an undated entry cannot be match-windowed.
func camt053EntryDate(n camt053Ntry) (time.Time, error) {
	for _, s := range []string{n.BookgDt.Dt, n.BookgDt.DtTm, n.ValDt.Dt, n.ValDt.DtTm} {
		if strings.TrimSpace(s) != "" {
			return parseCAMT053Date(s)
		}
	}
	return time.Time{}, fmt.Errorf("%w: Ntry without booking date", ErrInvalidCAMT053)
}

// camt053EntryMemo joins the entry's remittance information: the first
// TxDtls' Ustrd lines, joined with " | ".
func camt053EntryMemo(n camt053Ntry) (memo, endToEndID string) {
	if len(n.NtryDtls.TxDtls) == 0 {
		return "", ""
	}
	d := n.NtryDtls.TxDtls[0]
	var parts []string
	for _, u := range d.RmtInf.Ustrd {
		if t := strings.TrimSpace(u); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " | "), strings.TrimSpace(d.Refs.EndToEndID)
}

// ParseCAMT053 parses an ISO 20022 camt.053 bank statement document
// into its statement entries. It is namespace-tolerant (any
// camt.053.001.xx version) and skips the optional blocks real banks
// emit (balances, charges, …).
//
// Per Stmt it collects Id and the account identifier (IBAN, else
// Other/Id); per Ntry it collects Amt (+ Ccy), CdtDbtInd, the booking
// date, and the first TxDtls' EndToEndId + remittance text. An entry
// missing its amount, currency, or booking date — or carrying a value
// that does not parse — makes the whole document invalid (a statement
// the reader cannot fully understand must not half-import). A document
// with no statement entries at all is likewise invalid.
func ParseCAMT053(data []byte) ([]CAMT053Transaction, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT053)
	}
	var doc camt053Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT053, err)
	}
	if len(doc.BkToCstmrStmt.Stmts) == 0 {
		return nil, fmt.Errorf("%w: no Stmt blocks found", ErrInvalidCAMT053)
	}
	var out []CAMT053Transaction
	for _, stmt := range doc.BkToCstmrStmt.Stmts {
		stmtID := strings.TrimSpace(stmt.ID)
		if stmtID == "" {
			return nil, fmt.Errorf("%w: Stmt without Id", ErrInvalidCAMT053)
		}
		account := AccountID(camt053AccountIDOf(stmt.Acct))
		if account == "" {
			return nil, fmt.Errorf("%w: Stmt %q names no account", ErrInvalidCAMT053, stmtID)
		}
		for i, n := range stmt.Entries {
			seq := i + 1
			signed, err := parseCAMT053Amount(n.Amt.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: Stmt %q Ntry %d: %v", ErrInvalidCAMT053, stmtID, seq, err)
			}
			ccy := strings.TrimSpace(n.Amt.Ccy)
			if ccy == "" {
				return nil, fmt.Errorf("%w: Stmt %q Ntry %d: missing Ccy", ErrInvalidCAMT053, stmtID, seq)
			}
			currency, err := normalizeCurrency(ccy)
			if err != nil {
				return nil, fmt.Errorf("%w: Stmt %q Ntry %d: bad Ccy %q", ErrInvalidCAMT053, stmtID, seq, n.Amt.Ccy)
			}
			posted, err := camt053EntryDate(n)
			if err != nil {
				return nil, fmt.Errorf("%w: Stmt %q Ntry %d: %v", ErrInvalidCAMT053, stmtID, seq, err)
			}
			dir := camt053DirectionOf(n.CdtDbtInd, signed)
			amount := signed
			if amount < 0 {
				amount = -amount
			}
			memo, e2e := camt053EntryMemo(n)
			out = append(out, CAMT053Transaction{
				Account:     account,
				Currency:    currency,
				StmtID:      stmtID,
				Seq:         seq,
				Direction:   dir,
				AmountCents: amount,
				PostedAt:    posted,
				Memo:        memo,
				EndToEndID:  e2e,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no statement entries found", ErrInvalidCAMT053)
	}
	return out, nil
}

// ImportCAMT053 imports a camt.053 statement as a pre-posting report
// for account: it parses the document, drafts every entry, and matches
// each entry against the account's journaled rows. Nothing is posted,
// no hash chain is entered, and the journal version does not move —
// the report is a draft for the operator to review.
//
// Matching: a journaled row is a candidate for a statement entry when
// it is effective (pending-review rows never touched balances, so they
// cannot match a bank statement), carries the same currency, has equal
// |amount|, and its CreatedAt falls within ±MatchWindow of the entry's
// booking date. Exactly one candidate is matched; several is
// mismatched; none is unmatched.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns the
// original report with Duplicate set — a re-uploaded statement never
// double-counts.
//
// A statement naming a different account than the import target is an
// error: importing another account's statement into this one would
// corrupt the reconciliation.
func (l *Ledger) ImportCAMT053(account AccountID, data []byte, opts CAMT053ImportOptions) (*CAMT053ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	txns, err := ParseCAMT053(data)
	if err != nil {
		return nil, err
	}
	for _, t := range txns {
		if t.Account != account {
			return nil, fmt.Errorf("%w: statement names account %q, import targets %q", ErrInvalidCAMT053, t.Account, account)
		}
	}
	window := opts.MatchWindow
	if window <= 0 {
		window = DefaultCAMT053MatchWindow
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	if prev, ok := l.camt053Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	report := &CAMT053ImportReport{
		DocumentSHA256: key,
		Account:        account,
		Items:          []CAMT053ImportItem{},
	}
	for _, t := range txns {
		cands := l.matchStatementCandidatesLocked(account, t.Currency, t.AmountCents, t.PostedAt, window)
		item := CAMT053ImportItem{Transaction: t, Draft: t.Draft()}
		switch len(cands) {
		case 0:
			item.Status = CAMT053Unmatched
			report.Unmatched++
		case 1:
			item.Status = CAMT053Matched
			item.MatchedEntryID = cands[0]
			report.Matched++
		default:
			item.Status = CAMT053Mismatched
			item.CandidateCount = len(cands)
			report.Mismatched++
		}
		report.Items = append(report.Items, item)
	}

	stored := *report
	stored.Items = append([]CAMT053ImportItem(nil), report.Items...)
	if l.camt053Imports == nil {
		l.camt053Imports = make(map[string]*CAMT053ImportReport)
	}
	l.camt053Imports[key] = &stored
	return report, nil
}

// matchStatementCandidatesLocked returns the IDs of effective journaled
// rows for (account, currency) whose |amount| equals the statement
// entry's and whose CreatedAt falls within ±window of the entry's
// booking date. Callers must hold l.mu (either lock suffices). The OFX
// import shares this matcher — one rule for both statement formats.
func (l *Ledger) matchStatementCandidatesLocked(account AccountID, currency string, amountCents int64, postedAt time.Time, window time.Duration) []string {
	var out []string
	lo := postedAt.Add(-window)
	hi := postedAt.Add(window)
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != currency {
			continue
		}
		amt := e.AmountCents
		if amt < 0 {
			amt = -amt
		}
		if amt != amountCents {
			continue
		}
		if e.CreatedAt.Before(lo) || e.CreatedAt.After(hi) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// camt053ImportsLocked renders the camt.053 import registry as sorted
// snapshot records (see snapshotConfigLine). Callers must hold l.mu
// (either lock suffices).
func (l *Ledger) camt053ImportsLocked() []CAMT053ImportRecord {
	if len(l.camt053Imports) == 0 {
		return nil
	}
	out := make([]CAMT053ImportRecord, 0, len(l.camt053Imports))
	for sha, rep := range l.camt053Imports {
		out = append(out, CAMT053ImportRecord{DocumentSHA256: sha, Report: *rep})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocumentSHA256 < out[j].DocumentSHA256 })
	return out
}

// CAMT053ImportRecord is one entry of the camt.053 import registry in a
// snapshot: the document's SHA-256 and the report ImportCAMT053
// produced for it. Restoring it preserves duplicate-import idempotency
// across disaster recovery.
type CAMT053ImportRecord struct {
	DocumentSHA256 string             `json:"document_sha256"`
	Report         CAMT053ImportReport `json:"report"`
}
