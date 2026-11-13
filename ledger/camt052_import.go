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

// CAMT.052 intraday account report import (LG-408): the inbound half
// of the camt.052 round-trip (see camt052.go, LG-170). A bank hands us
// a camt.052.001.02 `BankToCustomerAccountReport` document;
// ParseCAMT052 parses it and ImportCAMT052 returns a pre-posting
// reconciliation report: every Ntry entry is drafted and matched
// against the journaled rows for the same account, and the OPBD/CLBD
// balance snapshots ride along in the report.
//
// Like the camt.053 import (LG-103), this is deliberately a draft,
// not a posting: it writes no journal rows, enters no hash chain, and
// bumps no version. A CLBD that disagrees with the ledger balance is
// reported, never fail-closed — the import is evidence, not a booking.
// Duplicate imports of the same document are idempotent via the
// document's SHA-256 (see camt052Imports on Ledger).

// ErrInvalidCAMT052 is returned by ParseCAMT052 and ImportCAMT052 when
// the input is not a readable camt.052 report: empty input, no
// BkToCstmrAcctRpt/Rpt blocks, a balance with an unknown type or an
// unparseable amount/currency/date, an entry missing its amount,
// currency, or booking date, a report without an Id, a report naming a
// different account than the import target, or a report with no Ntry
// entries at all. The HTTP layer maps it to 400.
var ErrInvalidCAMT052 = errors.New("ledger: invalid camt.052 document")

// CAMT052Statement is one parsed camt.052 intraday account report:
// the balance snapshots (OPBD/CLBD per currency) plus the Ntry entries
// as camt.053 transactions — the same entry shape the camt.053 import
// produces, so the two reports reconcile through the same matcher.
type CAMT052Statement struct {
	Balances     []CAMT052Balance
	Transactions []CAMT053Transaction
}

// CAMT052BalanceMismatch is one CLBD balance that disagrees with the
// ledger: the balance the report claims (ReportedCents) against the
// balance the journal folds at the same instant (LedgerCents).
// Reported-only: the import never adjusts the books to close the gap.
type CAMT052BalanceMismatch struct {
	Currency      string    `json:"currency"`
	ReportedCents int64     `json:"reported_cents"`
	LedgerCents   int64     `json:"ledger_cents"`
	At            time.Time `json:"at"`
}

// camt052Document is the parse-side XML model for a camt.052.001.02
// BankToCustomerAccountReport document. The root is namespace-tolerant
// (a bare local name, like camt053Document); the nested Rpt/Bal/Ntry
// shapes reuse the export-side wire structs from camt052.go, so the
// parser is the inverse of RenderCAMT052Report by construction.
type camt052Document struct {
	XMLName          xml.Name                 `xml:"Document"`
	BkToCstmrAcctRpt camt052xBkToCstmrAcctRpt `xml:"BkToCstmrAcctRpt"`
}

// camt052BalanceAmount converts a Bal amount to signed cents: the Amt
// value is absolute and the sign lives in CdtDbtInd (CRDT = credit
// balance, DBIT = debit balance); an absent indicator falls back to
// the value's own sign.
func camt052BalanceAmount(b camt052xBal) (int64, error) {
	signed, err := parseCAMT053Amount(b.Amt.Value)
	if err != nil {
		return 0, err
	}
	amount := signed
	if amount < 0 {
		amount = -amount
	}
	switch strings.ToUpper(strings.TrimSpace(b.CdtDbtInd)) {
	case "CRDT":
		return amount, nil
	case "DBIT":
		return -amount, nil
	default:
		return signed, nil
	}
}

// camt052BalanceDate picks a Bal date: Dt/DtTm, in that order. A
// missing date is an error — a balance without an instant cannot be
// compared with the ledger.
func camt052BalanceDate(b camt052xBal) (time.Time, error) {
	for _, s := range []string{b.Dt.Dt, b.Dt.DtTm} {
		if strings.TrimSpace(s) != "" {
			return parseCAMT053Date(s)
		}
	}
	return time.Time{}, fmt.Errorf("%w: Bal without date", ErrInvalidCAMT052)
}

// camt052xAccountIDOf resolves the report's account identifier from
// the export-side Acct shape: the IBAN when present, else the
// Other/Id — the camt053AccountIDOf rule, on the camt053xAcct type
// the camt.052 wire structs carry.
func camt052xAccountIDOf(a camt053xAcct) string {
	if id := strings.TrimSpace(a.ID.IBAN); id != "" {
		return id
	}
	return strings.TrimSpace(a.ID.Othr.ID)
}

// camt052xEntryDate picks the entry's booking date: BookgDt/Dt, then
// BookgDt/DtTm, then ValDt/Dt, then ValDt/DtTm — the camt053EntryDate
// rule, on the camt053xNtry type. A missing date is an error.
func camt052xEntryDate(n camt053xNtry) (time.Time, error) {
	for _, s := range []string{n.BookgDt.Dt, n.BookgDt.DtTm} {
		if strings.TrimSpace(s) != "" {
			return parseCAMT053Date(s)
		}
	}
	if n.ValDt != nil {
		for _, s := range []string{n.ValDt.Dt, n.ValDt.DtTm} {
			if strings.TrimSpace(s) != "" {
				return parseCAMT053Date(s)
			}
		}
	}
	return time.Time{}, fmt.Errorf("%w: Ntry without booking date", ErrInvalidCAMT052)
}

// camt052xEntryMemo joins the entry's remittance information: the
// first TxDtls' Ustrd lines, joined with " | " — the camt053EntryMemo
// rule, on the camt053xNtry type (whose RmtInf is a pointer).
func camt052xEntryMemo(n camt053xNtry) (memo, endToEndID string) {
	if len(n.NtryDtls.TxDtls) == 0 {
		return "", ""
	}
	d := n.NtryDtls.TxDtls[0]
	var parts []string
	if d.RmtInf != nil {
		for _, u := range d.RmtInf.Ustrd {
			if t := strings.TrimSpace(u); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " | "), strings.TrimSpace(d.Refs.EndToEndID)
}

// ParseCAMT052 parses an ISO 20022 camt.052 intraday account report
// document into its balances and statement entries. It is the inverse
// of RenderCAMT052Report: GrpHdr is skipped, per Rpt it collects Id
// and the account identifier (IBAN, else Other/Id), per Bal the
// OPBD/CLBD type, amount, currency, and date, and per Ntry the amount
// (+ Ccy), CdtDbtInd, booking date, and first TxDtls' EndToEndId +
// remittance text — the same entry contract as ParseCAMT053.
//
// A balance or entry that cannot be fully understood makes the whole
// document invalid (a report the reader cannot fully understand must
// not half-import). A document with no Ntry entries at all is likewise
// invalid — banks do send empty intraday reports, but an import with
// nothing to reconcile is a caller error, like ParseCAMT053's
// "no statement entries found".
func ParseCAMT052(data []byte) (*CAMT052Statement, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT052)
	}
	var doc camt052Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT052, err)
	}
	if len(doc.BkToCstmrAcctRpt.Rpts) == 0 {
		return nil, fmt.Errorf("%w: no Rpt blocks found", ErrInvalidCAMT052)
	}
	stmt := &CAMT052Statement{}
	for _, rpt := range doc.BkToCstmrAcctRpt.Rpts {
		rptID := strings.TrimSpace(rpt.ID)
		if rptID == "" {
			return nil, fmt.Errorf("%w: Rpt without Id", ErrInvalidCAMT052)
		}
		account := AccountID(camt052xAccountIDOf(rpt.Acct))
		if account == "" {
			return nil, fmt.Errorf("%w: Rpt %q names no account", ErrInvalidCAMT052, rptID)
		}
		for i, b := range rpt.Bals {
			seq := i + 1
			if b.Tp.CdOrPrtry.Cd != CAMT052BalanceOPBD && b.Tp.CdOrPrtry.Cd != CAMT052BalanceCLBD {
				return nil, fmt.Errorf("%w: Rpt %q Bal %d: bad type %q", ErrInvalidCAMT052, rptID, seq, b.Tp.CdOrPrtry.Cd)
			}
			amount, err := camt052BalanceAmount(b)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Bal %d: %v", ErrInvalidCAMT052, rptID, seq, err)
			}
			ccy := strings.TrimSpace(b.Amt.Ccy)
			if ccy == "" {
				return nil, fmt.Errorf("%w: Rpt %q Bal %d: missing Ccy", ErrInvalidCAMT052, rptID, seq)
			}
			currency, err := normalizeCurrency(ccy)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Bal %d: bad Ccy %q", ErrInvalidCAMT052, rptID, seq, b.Amt.Ccy)
			}
			at, err := camt052BalanceDate(b)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Bal %d: %v", ErrInvalidCAMT052, rptID, seq, err)
			}
			stmt.Balances = append(stmt.Balances, CAMT052Balance{
				Type:        b.Tp.CdOrPrtry.Cd,
				Currency:    currency,
				AmountCents: amount,
				At:          at,
			})
		}
		for i, n := range rpt.Ntries {
			seq := i + 1
			signed, err := parseCAMT053Amount(n.Amt.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Ntry %d: %v", ErrInvalidCAMT052, rptID, seq, err)
			}
			ccy := strings.TrimSpace(n.Amt.Ccy)
			if ccy == "" {
				return nil, fmt.Errorf("%w: Rpt %q Ntry %d: missing Ccy", ErrInvalidCAMT052, rptID, seq)
			}
			currency, err := normalizeCurrency(ccy)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Ntry %d: bad Ccy %q", ErrInvalidCAMT052, rptID, seq, n.Amt.Ccy)
			}
			posted, err := camt052xEntryDate(n)
			if err != nil {
				return nil, fmt.Errorf("%w: Rpt %q Ntry %d: %v", ErrInvalidCAMT052, rptID, seq, err)
			}
			dir := camt053DirectionOf(n.CdtDbtInd, signed)
			amount := signed
			if amount < 0 {
				amount = -amount
			}
			memo, e2e := camt052xEntryMemo(n)
			stmt.Transactions = append(stmt.Transactions, CAMT053Transaction{
				Account:     account,
				Currency:    currency,
				StmtID:      rptID,
				Seq:         seq,
				Direction:   dir,
				AmountCents: amount,
				PostedAt:    posted,
				Memo:        memo,
				EndToEndID:  e2e,
			})
		}
	}
	if len(stmt.Transactions) == 0 {
		return nil, fmt.Errorf("%w: no statement entries found", ErrInvalidCAMT052)
	}
	return stmt, nil
}

// balanceAtTimeLocked folds the effective journal rows for (account,
// currency) with CreatedAt before at into the account-centric balance
// at that instant — the same fold ExportCAMT052 uses for its OPBD/CLBD
// snapshots (pending-review rows never touched balances, so they are
// excluded; the debit leg adds, the credit leg subtracts). Callers
// must hold l.mu (either lock suffices).
func (l *Ledger) balanceAtTimeLocked(account AccountID, currency string, at time.Time) int64 {
	var bal int64
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != currency {
			continue
		}
		if !e.CreatedAt.Before(at) {
			continue
		}
		if e.DebitAccount == account {
			bal += e.AmountCents
		}
		if e.CreditAccount == account {
			bal -= e.AmountCents
		}
	}
	return bal
}

// ImportCAMT052 imports a camt.052 intraday account report as a
// pre-posting reconciliation report for account: it parses the
// document, drafts every Ntry entry, matches each entry against the
// account's journaled rows, and carries the OPBD/CLBD balances along.
// Nothing is posted, no hash chain is entered, and the journal version
// does not move — the report is a draft for the operator to review.
//
// Matching reuses matchStatementCandidatesLocked, the same matcher
// the camt.053 and OFX imports share: exactly one candidate is
// matched; several is mismatched; none is unmatched.
//
// Balances: every CLBD is compared with the ledger balance folded at
// the same instant; a disagreement lands in BalanceMismatches and
// never fails the import — the report is evidence, not a booking.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns the
// original report with Duplicate set — a re-uploaded report never
// double-counts.
//
// A report naming a different account than the import target is an
// error: importing another account's report into this one would
// corrupt the reconciliation.
func (l *Ledger) ImportCAMT052(account AccountID, data []byte, opts CAMT053ImportOptions) (*CAMT053ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	stmt, err := ParseCAMT052(data)
	if err != nil {
		return nil, err
	}
	for _, t := range stmt.Transactions {
		if t.Account != account {
			return nil, fmt.Errorf("%w: report names account %q, import targets %q", ErrInvalidCAMT052, t.Account, account)
		}
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	// The live default (LG-394): an explicit opts.MatchWindow
	// always wins; an unset one reads the revised default under
	// the lock, so a revision governs the next line matched.
	window := l.effectiveStatementMatchWindowLocked(opts.MatchWindow)

	if prev, ok := l.camt052Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	report := &CAMT053ImportReport{
		DocumentSHA256: key,
		Account:        account,
		Items:          []CAMT053ImportItem{},
		Balances:       append([]CAMT052Balance(nil), stmt.Balances...),
	}
	for _, t := range stmt.Transactions {
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
	// CLBD is the closing booked balance: compare it with the ledger
	// balance folded at the same instant. A disagreement is reported,
	// never fail-closed.
	for _, b := range stmt.Balances {
		if b.Type != CAMT052BalanceCLBD {
			continue
		}
		if got := l.balanceAtTimeLocked(account, b.Currency, b.At); got != b.AmountCents {
			report.BalanceMismatches = append(report.BalanceMismatches, CAMT052BalanceMismatch{
				Currency:      b.Currency,
				ReportedCents: b.AmountCents,
				LedgerCents:   got,
				At:            b.At,
			})
		}
	}

	stored := *report
	stored.Items = append([]CAMT053ImportItem(nil), report.Items...)
	stored.Balances = append([]CAMT052Balance(nil), report.Balances...)
	stored.BalanceMismatches = append([]CAMT052BalanceMismatch(nil), report.BalanceMismatches...)
	if l.camt052Imports == nil {
		l.camt052Imports = make(map[string]*CAMT053ImportReport)
	}
	l.camt052Imports[key] = &stored
	return report, nil
}

// camt052ImportsLocked renders the camt.052 import registry as sorted
// snapshot records (see snapshotConfigLine). Callers must hold l.mu
// (either lock suffices).
func (l *Ledger) camt052ImportsLocked() []CAMT052ImportRecord {
	if len(l.camt052Imports) == 0 {
		return nil
	}
	out := make([]CAMT052ImportRecord, 0, len(l.camt052Imports))
	for sha, rep := range l.camt052Imports {
		out = append(out, CAMT052ImportRecord{DocumentSHA256: sha, Report: *rep})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocumentSHA256 < out[j].DocumentSHA256 })
	return out
}

// CAMT052ImportRecord is one entry of the camt.052 import registry in
// a snapshot: the document's SHA-256 and the report ImportCAMT052
// produced for it. Restoring it preserves duplicate-import idempotency
// across disaster recovery.
type CAMT052ImportRecord struct {
	DocumentSHA256 string              `json:"document_sha256"`
	Report         CAMT053ImportReport `json:"report"`
}
