package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CAMT.053 bank statement export (LG-158, fintech: 银行对账单导出).
//
// ExportCAMT053 is the inverse of ImportCAMT053 (see camt053.go,
// LG-103): it renders the account's effective journal rows as an ISO
// 20022 `camt.053.001.02` `BankToCustomerStatement` document — one Stmt
// block, one Ntry per entry — so the operator can hand the statement
// to the bank, the auditor, or any camt.053 reader.
//
// Round-trip: RenderCAMT053Statement emits exactly what ParseCAMT053
// reads back — Stmt/Id, Acct/Id/Othr/Id, Amt+Ccy, CdtDbtInd,
// BookgDt/DtTm (UTC, full RFC 3339 so the instant survives),
// Refs/EndToEndId, and the remittance text — so
// ParseCAMT053(ExportCAMT053(...)) returns the same transactions. The
// export is read-only like GenerateStatement: no journal rows, no hash
// chain, no version bump.
//
// Direction is account-centric, mirroring the importer: the account
// as the debit leg reads CRDT (money in), as the credit leg DBIT
// (money out). Amounts are absolute — the sign lives in the
// indicator, exactly like the parsed CAMT053Transaction. The entry's
// idempotency key rides in Refs/EndToEndId (the entry ID when the row
// carries no key), and Refs/TxId always names the journal entry ID,
// so the bank's references trace back to ledger rows.

// ErrInvalidCAMT053Export is returned by ExportCAMT053 and
// RenderCAMT053Statement when the request cannot name a valid
// statement: empty account or statement ID, an invalid currency
// filter, an inverted time range, or a transaction missing its
// currency, direction, or booking date.
var ErrInvalidCAMT053Export = errors.New("ledger: invalid camt.053 export options")

// CAMT053ExportOptions tunes ExportCAMT053.
//
// Account and StmtID are required. MsgID defaults to StmtID.
// Since/Until bound the exported entries (half-open [Since, Until);
// zero means unbounded). Currency optionally filters to one ISO code.
// Entries are the account's effective journal rows in (CreatedAt, ID)
// order — pending-review rows never touched balances, so a bank
// statement must not show them (the OFX export's rule).
type CAMT053ExportOptions struct {
	Account  AccountID
	StmtID   string
	MsgID    string
	Since    time.Time
	Until    time.Time
	Currency string
}

// camt053FormatAmount renders integer cents as a decimal amount string
// ("150000" -> "1500.00") with no floats. The caller passes the
// absolute value; the direction indicator carries the sign.
func camt053FormatAmount(amountCents int64) string {
	if amountCents < 0 {
		amountCents = -amountCents
	}
	return fmt.Sprintf("%d.%02d", amountCents/100, amountCents%100)
}

// camt053IndicatorOf maps the account-centric direction to the
// CdtDbtInd code.
func camt053IndicatorOf(d CAMT053Direction) (string, error) {
	switch d {
	case CAMT053DirectionIn:
		return "CRDT", nil
	case CAMT053DirectionOut:
		return "DBIT", nil
	default:
		return "", fmt.Errorf("%w: bad direction %q", ErrInvalidCAMT053Export, d)
	}
}

// The camt053x* structs are the export-side XML model: a strict
// writer to the import structs' tolerant reader. They carry the
// camt.053.001.02 namespace on the root and omit every empty element,
// so the emitted document is what a bank expects to receive. The
// import path (camt053.go) is untouched.
type camt053xDocument struct {
	XMLName     xml.Name           `xml:"urn:iso:std:iso:20022:tech:xsd:camt.053.001.02 Document"`
	BkToCstmrStmt camt053xBkToCstmrStmt `xml:"BkToCstmrStmt"`
}

type camt053xBkToCstmrStmt struct {
	GrpHdr camt053xGrpHdr `xml:"GrpHdr"`
	Stmts  []camt053xStmt `xml:"Stmt"`
}

type camt053xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type camt053xStmt struct {
	ID      string        `xml:"Id"`
	Acct    camt053xAcct  `xml:"Acct"`
	Entries []camt053xNtry `xml:"Ntry"`
}

type camt053xAcct struct {
	ID camt053xAcctID `xml:"Id"`
}

type camt053xAcctID struct {
	Othr camt053xOthr `xml:"Othr"`
}

type camt053xOthr struct {
	ID string `xml:"Id"`
}

type camt053xNtry struct {
	Amt       camt053xAmt  `xml:"Amt"`
	CdtDbtInd string       `xml:"CdtDbtInd"`
	BookgDt   camt053xDt   `xml:"BookgDt"`
	NtryDtls  camt053xDtls `xml:"NtryDtls"`
}

type camt053xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt053xDt struct {
	DtTm string `xml:"DtTm"`
}

type camt053xDtls struct {
	TxDtls []camt053xTxDtls `xml:"TxDtls"`
}

type camt053xTxDtls struct {
	Refs   camt053xRefs    `xml:"Refs"`
	RmtInf *camt053xRmtInf `xml:"RmtInf,omitempty"`
}

type camt053xRefs struct {
	EndToEndID string `xml:"EndToEndId,omitempty"`
}

type camt053xRmtInf struct {
	Ustrd []string `xml:"Ustrd"`
}

// RenderCAMT053Statement renders txns as a camt.053.001.02
// BankToCustomerStatement document: GrpHdr (MsgId = msgID, CreDtTm =
// now), one Stmt (Id = stmtID, Acct/Id/Othr/Id = accountID), and one
// Ntry per transaction in order (Seq is implied by position, 1-based).
//
// Every transaction needs a currency, a direction, and a booking date;
// the amount is emitted absolute with the direction in CdtDbtInd. The
// document round-trips through ParseCAMT053: parsing the output
// returns transactions equal to the input (PostedAt normalized to
// UTC). Fail-loud on anything that would not survive the round trip.
func RenderCAMT053Statement(accountID, stmtID, msgID string, txns []CAMT053Transaction) ([]byte, error) {
	if strings.TrimSpace(stmtID) == "" {
		return nil, fmt.Errorf("%w: statement ID required", ErrInvalidCAMT053Export)
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("%w: account ID required", ErrInvalidCAMT053Export)
	}
	if strings.TrimSpace(msgID) == "" {
		msgID = stmtID
	}
	stmt := camt053xStmt{
		ID:   stmtID,
		Acct: camt053xAcct{ID: camt053xAcctID{Othr: camt053xOthr{ID: accountID}}},
	}
	for i, t := range txns {
		currency, err := normalizeCurrency(t.Currency)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: bad currency %q", ErrInvalidCAMT053Export, i+1, t.Currency)
		}
		ind, err := camt053IndicatorOf(t.Direction)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", ErrInvalidCAMT053Export, i+1, err)
		}
		if t.PostedAt.IsZero() {
			return nil, fmt.Errorf("%w: entry %d: missing booking date", ErrInvalidCAMT053Export, i+1)
		}
		amount := t.AmountCents
		if amount < 0 {
			amount = -amount
		}
		txDtls := camt053xTxDtls{Refs: camt053xRefs{EndToEndID: t.EndToEndID}}
		if t.Memo != "" {
			txDtls.RmtInf = &camt053xRmtInf{Ustrd: []string{t.Memo}}
		}
		stmt.Entries = append(stmt.Entries, camt053xNtry{
			Amt:       camt053xAmt{Ccy: currency, Value: camt053FormatAmount(amount)},
			CdtDbtInd: ind,
			BookgDt:   camt053xDt{DtTm: t.PostedAt.UTC().Format(time.RFC3339)},
			NtryDtls:  camt053xDtls{TxDtls: []camt053xTxDtls{txDtls}},
		})
	}
	doc := camt053xDocument{
		BkToCstmrStmt: camt053xBkToCstmrStmt{
			GrpHdr: camt053xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
			Stmts:  []camt053xStmt{stmt},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT053Export, err)
	}
	out := append([]byte(xml.Header), body...)
	return out, nil
}

// ExportCAMT053 renders a camt.053 statement for one account from the
// journal. The account named in Stmt/Acct is the external identifier
// mapped under the "camt053" system when one exists (see
// ExternalSystemCAMT053 — usually the IBAN the bank expects), else the
// internal account ID.
//
// Read-only: the scan holds the read lock from start to finish, so the
// statement describes one consistent point in time. It bumps no
// version, books nothing, and appends no audit-chain link — the same
// contract as GenerateStatement and ExportOFX.
//
// An account with no rows in the window exports a valid empty
// statement (banks do send those). Note the asymmetry: ParseCAMT053
// rejects documents with zero entries, so an empty export does not
// re-parse — the exporter is honest about the window, the parser is
// strict about importing only real statements.
func (l *Ledger) ExportCAMT053(opts CAMT053ExportOptions) ([]byte, error) {
	if opts.Account == "" {
		return nil, ErrEmptyAccountID
	}
	if strings.TrimSpace(opts.StmtID) == "" {
		return nil, fmt.Errorf("%w: statement ID required", ErrInvalidCAMT053Export)
	}
	currency := ""
	if opts.Currency != "" {
		var cerr error
		currency, cerr = normalizeCurrency(opts.Currency)
		if cerr != nil {
			return nil, cerr
		}
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, fmt.Errorf("%w: since is after until", ErrInvalidCAMT053Export)
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	var rows []JournalEntry
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportOFX and GenerateStatement: a
			// pending-review row never touched balances, so the bank
			// statement excludes it. Re-export after approval.
			continue
		}
		if currency != "" && e.Currency != currency {
			continue
		}
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !e.CreatedAt.Before(opts.Until) {
			continue
		}
		rows = append(rows, l.redactEntryMemoLocked(e))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})

	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemCAMT053); ok {
		acctID = ext
	}

	txns := make([]CAMT053Transaction, 0, len(rows))
	for _, e := range rows {
		dir := CAMT053DirectionOut
		if e.DebitAccount == opts.Account {
			// Debit leg first: a self-transfer (both legs the
			// account) reads as an inflow, the same bias Draft()
			// uses on the import side.
			dir = CAMT053DirectionIn
		}
		amount := e.AmountCents
		if amount < 0 {
			amount = -amount
		}
		e2e := e.IdempotencyKey
		if e2e == "" {
			e2e = e.ID
		}
		txns = append(txns, CAMT053Transaction{
			Account:     opts.Account,
			Currency:    e.Currency,
			StmtID:      opts.StmtID,
			Seq:         len(txns) + 1,
			Direction:   dir,
			AmountCents: amount,
			PostedAt:    e.CreatedAt.UTC(),
			Memo:        e.Memo,
			EndToEndID:  e2e,
		})
	}

	msgID := opts.MsgID
	if strings.TrimSpace(msgID) == "" {
		msgID = opts.StmtID
	}
	return RenderCAMT053Statement(acctID, opts.StmtID, msgID, txns)
}
