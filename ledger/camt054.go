package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CAMT.054 bank debit/credit notification import (LG-164, fintech).
//
// This is the per-notification counterpart of the camt.053 day-end
// statement (see camt053.go, LG-158): a bank hands us a
// camt.054.001.xx `BankToCustomerDebitCreditNotification` document and
// ImportCAMT054 parses every Ntry, then auto-matches each notification
// against the ledger's outstanding items by the (EndToEndId, amount,
// date) triple and advances the reconciliation status of matched items
// — outstanding -> reconciled.
//
// The outstanding domain is transfers and payouts, not journal rows
// (that is the camt.053 import's job — the two are orthogonal by
// design): a notification's EndToEndId is looked up against the
// original payout/transfer idempotency key (the same association as
// the pacs.008 notification, see associatePacs008Locked), then the
// amount must equal the original's and the booking date must fall
// within ± the match window of the original's creation. Exactly one
// triple-match is matched; several is mismatched; none is unmatched.
//
// A matched notification records the reconciliation advance in the
// camt054Matches registry and emits a hash-chained camt054_reconciled
// audit event — the durable record that the item is no longer
// outstanding. Deliberately out of scope: moving money or advancing
// payout/transfer lifecycle states. A notification is bank evidence
// the operator can feed to SettlePayout; the import itself writes no
// journal rows, enters no hash chain, and bumps no version.
//
// Like the camt.053 import, importing the same document twice is
// idempotent via the document's SHA-256 (see camt054Imports).

// ErrInvalidCAMT054 is returned by ParseCAMT054 and ImportCAMT054 when
// the input is not a readable camt.054 notification: empty input, no
// BkToCstmrDbtCdtNtfctn/Ntfctn blocks, a notification missing its Id or
// account, an entry missing its amount, currency, or booking date, or
// an unparseable amount or date. The HTTP layer maps it to 400.
var ErrInvalidCAMT054 = errors.New("ledger: invalid camt.054 document")

// DefaultCAMT054MatchWindow is the default ± window around a
// notification's booking date inside which the original
// transfer/payout's creation counts as a date match (see
// CAMT054ImportOptions).
const DefaultCAMT054MatchWindow = 24 * time.Hour

// CAMT054Direction is the money-movement direction of a notification
// entry, account-centric like the camt.053 import: "in" means the
// account was the debit leg (money added to it), "out" means it was
// the credit leg (money left it).
type CAMT054Direction string

const (
	// CAMT054DirectionIn: CdtDbtInd CRDT — money in.
	CAMT054DirectionIn CAMT054Direction = "in"
	// CAMT054DirectionOut: CdtDbtInd DBIT — money out.
	CAMT054DirectionOut CAMT054Direction = "out"
)

// CAMT054Notification is one parsed Ntry debit/credit notification.
// AmountCents is the absolute value in cents — the sign lives in
// Direction, mirroring the camt.053 import. Seq is the 1-based entry
// sequence within its notification; NtfctnID/Seq is the stable
// notification identity.
type CAMT054Notification struct {
	NtfctnID    string           `json:"ntfctn_id"`
	Account     AccountID        `json:"account"`
	Currency    string           `json:"currency"`
	Seq         int              `json:"seq"`
	Direction   CAMT054Direction `json:"direction"`
	AmountCents int64            `json:"amount_cents"`
	PostedAt    time.Time        `json:"posted_at"`
	Memo        string           `json:"memo,omitempty"`
	EndToEndID  string           `json:"end_to_end_id,omitempty"`
}

// CAMT054MatchStatus is the triple-match outcome of one notification
// against the outstanding transfers/payouts.
type CAMT054MatchStatus string

const (
	// CAMT054Matched: exactly one outstanding item matches the
	// (EndToEndId, amount, date) triple — its reconciliation status
	// advances to reconciled.
	CAMT054Matched CAMT054MatchStatus = "matched"
	// CAMT054Mismatched: several outstanding items match the triple.
	CAMT054Mismatched CAMT054MatchStatus = "mismatched"
	// CAMT054Unmatched: no outstanding item matches the triple.
	CAMT054Unmatched CAMT054MatchStatus = "unmatched"
)

// CAMT054MatchItem is one notification with its triple-match outcome.
// On a match, MatchedKind/MatchedID name the outstanding item whose
// reconciliation status advanced; on a mismatch, MismatchReason says
// which leg of the triple failed (amount_mismatch, currency_mismatch,
// or date_out_of_window).
type CAMT054MatchItem struct {
	Notification   CAMT054Notification `json:"notification"`
	Status         CAMT054MatchStatus   `json:"status"`
	MatchedKind    string              `json:"matched_kind,omitempty"`
	MatchedID      string              `json:"matched_id,omitempty"`
	MismatchReason string              `json:"mismatch_reason,omitempty"`
}

// CAMT054ImportReport is the full import report for one notification
// document. Duplicate is true when the report is a replay of an
// earlier import of the same document (see ImportCAMT054).
type CAMT054ImportReport struct {
	DocumentSHA256 string            `json:"document_sha256"`
	Account        AccountID         `json:"account"`
	Items          []CAMT054MatchItem `json:"items"`
	Matched        int               `json:"matched"`
	Mismatched     int               `json:"mismatched"`
	Unmatched      int               `json:"unmatched"`
	Duplicate      bool              `json:"duplicate"`
}

// CAMT054ImportOptions tunes ImportCAMT054. MatchWindow is the ± window
// around each notification's booking date inside which the original
// transfer/payout's creation counts as a date match; zero means
// DefaultCAMT054MatchWindow.
type CAMT054ImportOptions struct {
	MatchWindow time.Duration
}

// camt054MatchRecord is one row of the camt.054 match registry (see
// camt054Matches on Ledger): the notification's last triple-match
// outcome, keyed by the notification's stable ID. A matched
// notification replays its recorded match on re-import; an unmatched
// or mismatched one is re-scanned because the journal only grows — a
// transfer/payout booked later can resolve it.
type camt054MatchRecord struct {
	notification CAMT054Notification
	status       CAMT054MatchStatus
	matchedKind  string
	matchedID    string
}

// The camt054* structs model the ISO 20022 camt.054.001.xx
// BankToCustomerDebitCreditNotification shape. The struct tags name
// bare local element names, so encoding/xml matches them in any
// namespace — banks ship the default
// `urn:iso:std:iso:20022:tech:xsd:camt.054…` namespace, and a tolerant
// reader does not care which version suffix it carries. Unknown
// elements are skipped, which is what makes the parser tolerant of the
// optional blocks real notifications carry.
type camt054Document struct {
	XMLName xml.Name         `xml:"Document"`
	Ntfctn  camt054DbtCdtNtf `xml:"BkToCstmrDbtCdtNtfctn"`
}

type camt054DbtCdtNtf struct {
	GrpHdr camt054GrpHdr `xml:"GrpHdr"`
	Ntfctns []camt054Ntfctn `xml:"Ntfctn"`
}

type camt054GrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type camt054Ntfctn struct {
	ID      string        `xml:"Id"`
	Acct    camt054Acct   `xml:"Acct"`
	Entries []camt054Ntry `xml:"Ntry"`
}

type camt054Acct struct {
	ID camt054AcctID `xml:"Id"`
}

type camt054AcctID struct {
	IBAN string       `xml:"IBAN"`
	Othr camt054Othr   `xml:"Othr"`
}

type camt054Othr struct {
	ID string `xml:"Id"`
}

type camt054Ntry struct {
	Amt       camt054Amt  `xml:"Amt"`
	CdtDbtInd string      `xml:"CdtDbtInd"`
	BookgDt   camt054Dt   `xml:"BookgDt"`
	ValDt     camt054Dt   `xml:"ValDt"`
	NtryDtls  camt054Dtls `xml:"NtryDtls"`
}

type camt054Amt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt054Dt struct {
	Dt   string `xml:"Dt"`
	DtTm string `xml:"DtTm"`
}

type camt054Dtls struct {
	TxDtls []camt054TxDtls `xml:"TxDtls"`
}

type camt054TxDtls struct {
	Refs   camt054Refs   `xml:"Refs"`
	RmtInf camt054RmtInf `xml:"RmtInf"`
}

type camt054Refs struct {
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
}

type camt054RmtInf struct {
	Ustrd []string `xml:"Ustrd"`
}

// ParseCAMT054 parses an ISO 20022 camt.054 debit/credit notification
// document into its notifications. It is namespace-tolerant and skips
// the optional blocks real banks emit.
//
// Per Ntfctn it collects Id and the account identifier (IBAN, else
// Other/Id); per Ntry it collects Amt (+ Ccy), CdtDbtInd, the booking
// date, and the first TxDtls' EndToEndId + remittance text. An entry
// missing its amount, currency, or booking date — or carrying a value
// that does not parse — makes the whole document invalid (a
// notification the reader cannot fully understand must not
// half-import). A document with no notification entries at all is
// likewise invalid. The amount/date parsers are shared with the
// camt.053 import: one rule for both statement formats.
func ParseCAMT054(data []byte) ([]CAMT054Notification, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT054)
	}
	var doc camt054Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT054, err)
	}
	if len(doc.Ntfctn.Ntfctns) == 0 {
		return nil, fmt.Errorf("%w: no Ntfctn blocks found", ErrInvalidCAMT054)
	}
	var out []CAMT054Notification
	for _, ntfctn := range doc.Ntfctn.Ntfctns {
		ntfctnID := strings.TrimSpace(ntfctn.ID)
		if ntfctnID == "" {
			return nil, fmt.Errorf("%w: Ntfctn without Id", ErrInvalidCAMT054)
		}
		account := AccountID(camt054AccountIDOf(ntfctn.Acct))
		if account == "" {
			return nil, fmt.Errorf("%w: Ntfctn %q names no account", ErrInvalidCAMT054, ntfctnID)
		}
		for i, n := range ntfctn.Entries {
			seq := i + 1
			signed, err := parseCAMT053Amount(n.Amt.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: Ntfctn %q Ntry %d: bad Amt %q", ErrInvalidCAMT054, ntfctnID, seq, n.Amt.Value)
			}
			ccy := strings.TrimSpace(n.Amt.Ccy)
			if ccy == "" {
				return nil, fmt.Errorf("%w: Ntfctn %q Ntry %d: missing Ccy", ErrInvalidCAMT054, ntfctnID, seq)
			}
			currency, err := normalizeCurrency(ccy)
			if err != nil {
				return nil, fmt.Errorf("%w: Ntfctn %q Ntry %d: bad Ccy %q", ErrInvalidCAMT054, ntfctnID, seq, n.Amt.Ccy)
			}
			posted, err := camt054EntryDate(n)
			if err != nil {
				return nil, fmt.Errorf("%w: Ntfctn %q Ntry %d: %v", ErrInvalidCAMT054, ntfctnID, seq, err)
			}
			dir := CAMT054DirectionIn
			if camt053DirectionOf(n.CdtDbtInd, signed) == CAMT053DirectionOut {
				dir = CAMT054DirectionOut
			}
			amount := signed
			if amount < 0 {
				amount = -amount
			}
			memo, e2e := camt054EntryMemo(n)
			out = append(out, CAMT054Notification{
				NtfctnID:    ntfctnID,
				Account:     account,
				Currency:    currency,
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
		return nil, fmt.Errorf("%w: no notification entries found", ErrInvalidCAMT054)
	}
	return out, nil
}

// camt054AccountIDOf resolves the notification's account identifier:
// the IBAN when present, else the Other/Id.
func camt054AccountIDOf(a camt054Acct) string {
	if id := strings.TrimSpace(a.ID.IBAN); id != "" {
		return id
	}
	return strings.TrimSpace(a.ID.Othr.ID)
}

// camt054EntryDate picks the entry's booking date: BookgDt/Dt, then
// BookgDt/DtTm, then ValDt/Dt, then ValDt/DtTm. A missing date is an
// error — an undated notification cannot be triple-matched.
func camt054EntryDate(n camt054Ntry) (time.Time, error) {
	for _, s := range []string{n.BookgDt.Dt, n.BookgDt.DtTm, n.ValDt.Dt, n.ValDt.DtTm} {
		if strings.TrimSpace(s) != "" {
			t, err := parseCAMT053Date(s)
			if err != nil {
				return time.Time{}, err
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("Ntry without booking date")
}

// camt054EntryMemo joins the entry's remittance information: the first
// TxDtls' Ustrd lines, joined with " | ", plus its EndToEndId.
func camt054EntryMemo(n camt054Ntry) (memo, endToEndID string) {
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

// camt054NotificationID builds the notification's stable identity:
// "camt054/<ntfctnID>/<seq>" — the key of the match registry, so
// re-imports of the same notification address the same row.
func camt054NotificationID(n CAMT054Notification) string {
	return "camt054/" + n.NtfctnID + "/" + fmt.Sprint(n.Seq)
}

// camt054TripleCandidate is one outstanding transfer/payout the
// (EndToEndId, amount, date) triple is evaluated against.
type camt054TripleCandidate struct {
	kind        string // "transfer" or "payout"
	id          string
	amountCents int64
	currency    string
	createdAt   time.Time
}

// camt054TripleCandidatesLocked collects the outstanding items the
// notification's EndToEndId names: the transfer and/or payout the
// association resolves (see associatePacs008Locked). Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) camt054TripleCandidatesLocked(e2e string) []camt054TripleCandidate {
	if e2e == "" {
		return nil
	}
	var out []camt054TripleCandidate
	assoc := l.associatePacs008Locked(e2e)
	switch assoc.Kind {
	case Pacs008AssociatedTransfer:
		out = append(out, camt054TripleCandidate{
			kind:        "transfer",
			id:          assoc.ID,
			amountCents: assoc.AmountCents,
			currency:    assoc.Currency,
		})
	case Pacs008AssociatedPayout:
		out = append(out, camt054TripleCandidate{
			kind:        "payout",
			id:          assoc.ID,
			amountCents: assoc.AmountCents,
			currency:    assoc.Currency,
		})
	}
	// Stamp creation times: the principal entry's CreatedAt for a
	// transfer, the instruction's CreatedAt for a payout.
	for i := range out {
		switch out[i].kind {
		case "transfer":
			if e, ok := l.entries[out[i].id]; ok {
				out[i].createdAt = e.CreatedAt
			}
		case "payout":
			if p, ok := l.payouts[out[i].id]; ok {
				out[i].createdAt = p.CreatedAt
			}
		}
	}
	return out
}

// ImportCAMT054 imports a camt.054 debit/credit notification document
// for account: it parses the document and triple-matches every
// notification against the outstanding transfers/payouts. Nothing is
// posted, no hash chain is entered, and the journal version does not
// move — the report tells the operator what the bank notified and
// which outstanding items it reconciles.
//
// Triple-match: a candidate is the transfer/payout the notification's
// EndToEndId names (see associatePacs008Locked). The amount must equal
// the candidate's (amount_mismatch), the currency must equal the
// candidate's (currency_mismatch), and the notification's booking date
// must fall within ±MatchWindow of the candidate's creation
// (date_out_of_window). Exactly one passing candidate is matched —
// its reconciliation status advances to reconciled, recorded in the
// camt054Matches registry and in a hash-chained camt054_reconciled
// audit event; several is mismatched; none is unmatched.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns the
// original report with Duplicate set — a re-delivered notification
// never double-advances a reconciliation.
//
// A notification naming a different account than the import target is
// an error: matching another account's notifications into this one
// would corrupt the reconciliation.
func (l *Ledger) ImportCAMT054(account AccountID, data []byte, opts CAMT054ImportOptions) (*CAMT054ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	notifications, err := ParseCAMT054(data)
	if err != nil {
		return nil, err
	}
	for _, n := range notifications {
		if n.Account != account {
			return nil, fmt.Errorf("%w: notification names account %q, import targets %q", ErrInvalidCAMT054, n.Account, account)
		}
	}
	window := opts.MatchWindow
	if window <= 0 {
		window = DefaultCAMT054MatchWindow
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	if prev, ok := l.camt054Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	report := &CAMT054ImportReport{
		DocumentSHA256: key,
		Account:        account,
		Items:          []CAMT054MatchItem{},
	}
	for _, n := range notifications {
		item := l.matchCAMT054Locked(n, window)
		switch item.Status {
		case CAMT054Matched:
			report.Matched++
		case CAMT054Mismatched:
			report.Mismatched++
		default:
			report.Unmatched++
		}
		report.Items = append(report.Items, item)
	}

	stored := *report
	stored.Items = append([]CAMT054MatchItem(nil), report.Items...)
	if l.camt054Imports == nil {
		l.camt054Imports = make(map[string]*CAMT054ImportReport)
	}
	l.camt054Imports[key] = &stored
	return report, nil
}

// matchCAMT054Locked triple-matches one notification. Callers must
// hold l.mu (the write lock).
func (l *Ledger) matchCAMT054Locked(n CAMT054Notification, window time.Duration) CAMT054MatchItem {
	item := CAMT054MatchItem{Notification: n}
	id := camt054NotificationID(n)
	// A notification recorded as matched replays its recorded match —
	// re-importing the same document never double-advances.
	if rec, ok := l.camt054Matches[id]; ok && rec.status == CAMT054Matched {
		item.Status = CAMT054Matched
		item.MatchedKind = rec.matchedKind
		item.MatchedID = rec.matchedID
		return item
	}
	cands := l.camt054TripleCandidatesLocked(n.EndToEndID)
	var passing []camt054TripleCandidate
	var firstFailure string
	for _, c := range cands {
		switch {
		case n.AmountCents != c.amountCents:
			firstFailure = "amount_mismatch"
		case n.Currency != c.currency:
			firstFailure = "currency_mismatch"
		default:
			lo := n.PostedAt.Add(-window)
			hi := n.PostedAt.Add(window)
			if c.createdAt.Before(lo) || c.createdAt.After(hi) {
				firstFailure = "date_out_of_window"
			} else {
				passing = append(passing, c)
			}
		}
	}
	switch len(passing) {
	case 0:
		item.Status = CAMT054Unmatched
		if firstFailure != "" && len(cands) > 0 {
			// The EndToEndId named an outstanding item, but the
			// triple did not hold: report the mismatch explicitly
			// rather than a bare unmatched.
			item.Status = CAMT054Mismatched
			item.MismatchReason = firstFailure
		}
	case 1:
		c := passing[0]
		item.Status = CAMT054Matched
		item.MatchedKind = c.kind
		item.MatchedID = c.id
		// The reconciliation status advance: outstanding ->
		// reconciled, recorded in the match registry and in a
		// hash-chained audit event — the durable record.
		if l.camt054Matches == nil {
			l.camt054Matches = make(map[string]camt054MatchRecord)
		}
		l.camt054Matches[id] = camt054MatchRecord{
			notification: n,
			status:       CAMT054Matched,
			matchedKind:  c.kind,
			matchedID:    c.id,
		}
		l.emitAudit(AuditEvent{
			Op:      "camt054_reconciled",
			Actor:   "ImportCAMT054",
			TraceID: id,
			Accounts: []AccountID{n.Account},
			Details: map[string]any{
				"notification_id": id,
				"end_to_end_id":   n.EndToEndID,
				"matched_kind":    c.kind,
				"matched_id":      c.id,
				"amount_cents":    n.AmountCents,
				"currency":        n.Currency,
				"direction":       string(n.Direction),
			},
		})
	default:
		item.Status = CAMT054Mismatched
		item.MismatchReason = "ambiguous"
	}
	if item.Status != CAMT054Matched {
		if l.camt054Matches == nil {
			l.camt054Matches = make(map[string]camt054MatchRecord)
		}
		l.camt054Matches[id] = camt054MatchRecord{notification: n, status: item.Status}
	}
	return item
}

// camt054MatchesLocked renders the camt.054 match registry as sorted
// snapshot records. Callers must hold l.mu (either lock suffices).
func (l *Ledger) camt054MatchesLocked() []CAMT054MatchRecord {
	if len(l.camt054Matches) == 0 {
		return nil
	}
	out := make([]CAMT054MatchRecord, 0, len(l.camt054Matches))
	ids := make([]string, 0, len(l.camt054Matches))
	for id := range l.camt054Matches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := l.camt054Matches[id]
		out = append(out, CAMT054MatchRecord{
			NotificationID: id,
			Status:         string(rec.status),
			MatchedKind:    rec.matchedKind,
			MatchedID:      rec.matchedID,
		})
	}
	return out
}

// CAMT054MatchRecord is one row of the camt.054 match registry in a
// snapshot: the notification's stable ID and its last triple-match
// outcome. Restoring it preserves reconciliation advances across
// disaster recovery.
type CAMT054MatchRecord struct {
	NotificationID string `json:"notification_id"`
	Status         string `json:"status"`
	MatchedKind    string `json:"matched_kind,omitempty"`
	MatchedID      string `json:"matched_id,omitempty"`
}
