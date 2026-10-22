package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CAMT.052 intraday account report (LG-170, fintech: 日间流动性报告).
//
// ExportCAMT052 renders the account's journal as an ISO 20022
// `camt.052.001.02` `BankToCustomerAccountReport` document — the
// intraday sibling of the camt.053 end-of-day statement (see
// ExportCAMT053, LG-158). Where camt.053 covers one settled period,
// camt.052 answers "where do we stand right now": the report carries
// the intraday slice [Since, Until) as Ntry rows plus two booked
// balance snapshots per currency — OPBD (opening booked balance at
// Since) and CLBD (closing booked balance at Until) — so treasury can
// read the liquidity position without pulling a full statement.
//
// Orthogonal to camt.053 import/export (LG-103/LG-158): camt.053 is
// the end-of-day statement round-trip; this module is the intraday
// report and never re-parses its own output. Like ExportCAMT053 it is
// strictly read-only: no journal rows, no hash-chain links, no version
// bump, no audit event — it is a snapshot, not a booking.
//
// Direction, amounts, and entry selection follow the camt.053 export
// conventions: account-centric direction (debit leg = CRDT inflow),
// absolute amounts with the sign in CdtDbtInd, pending-review rows
// excluded (they never touched balances), and the Ntry rendering
// reuses the camt.053 Ntry machinery so the two documents agree on
// every entry they share.

// ErrInvalidCAMT052Report is returned by ExportCAMT052 and
// RenderCAMT052Report when the request cannot name a valid report:
// empty account or report ID, a missing or inverted [Since, Until)
// window, an invalid currency filter, or a balance/transaction missing
// its currency, type, or date.
var ErrInvalidCAMT052Report = errors.New("ledger: invalid camt.052 report options")

// CAMT052ReportOptions tunes ExportCAMT052.
//
// Account, RptID, Since, and Until are required. MsgID defaults to
// RptID. Since/Until bound the intraday slice as a half-open window
// [Since, Until): OPBD is the balance just before Since, CLBD the
// balance just before Until. Currency optionally filters to one ISO
// code; without it the report carries every currency the account
// touched, with OPBD/CLBD pairs per currency in ascending currency
// order for determinism.
type CAMT052ReportOptions struct {
	Account  AccountID
	RptID    string
	MsgID    string
	Since    time.Time
	Until    time.Time
	Currency string
}

// Balance type codes emitted in Bal/Tp/Cd: opening and closing booked
// balances — the camt.052 balance taxonomy's intraday pair.
const (
	CAMT052BalanceOPBD = "OPBD"
	CAMT052BalanceCLBD = "CLBD"
)

// CAMT052Balance is one balance snapshot in a camt.052 report: the
// opening (OPBD) or closing (CLBD) booked balance of the account in
// one currency at one instant. AmountCents is signed; the renderer
// takes the absolute value and carries the sign in CdtDbtInd.
type CAMT052Balance struct {
	Type        string
	Currency    string
	AmountCents int64
	At          time.Time
}

// The camt052x* structs are the export-side XML model: a strict
// writer for the camt.052.001.02 namespace. Ntry rendering is shared
// with the camt.053 exporter (see camt053_export.go) so the two
// documents render identical rows for identical entries.
type camt052xDocument struct {
	XMLName        xml.Name                `xml:"urn:iso:std:iso:20022:tech:xsd:camt.052.001.02 Document"`
	BkToCstmrAcctRpt camt052xBkToCstmrAcctRpt `xml:"BkToCstmrAcctRpt"`
}

type camt052xBkToCstmrAcctRpt struct {
	GrpHdr camt052xGrpHdr `xml:"GrpHdr"`
	Rpts   []camt052xRpt  `xml:"Rpt"`
}

type camt052xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type camt052xRpt struct {
	ID     string         `xml:"Id"`
	CreDtTm string        `xml:"CreDtTm"`
	Acct   camt053xAcct   `xml:"Acct"`
	Bals   []camt052xBal  `xml:"Bal"`
	Ntries []camt053xNtry `xml:"Ntry"`
}

type camt052xBal struct {
	Tp        camt052xBalTp `xml:"Tp"`
	Amt       camt053xAmt   `xml:"Amt"`
	CdtDbtInd string        `xml:"CdtDbtInd"`
	Dt        camt053xDt    `xml:"Dt"`
}

type camt052xBalTp struct {
	CdOrPrtry camt052xCdOrPrtry `xml:"CdOrPrtry"`
}

type camt052xCdOrPrtry struct {
	Cd string `xml:"Cd"`
}

// camt052IndicatorOf maps a signed balance to the CdtDbtInd code: a
// non-negative balance is a credit balance (CRDT), a negative one a
// debit balance (DBIT).
func camt052IndicatorOf(amountCents int64) string {
	if amountCents < 0 {
		return "DBIT"
	}
	return "CRDT"
}

// RenderCAMT052Report renders balances and transactions as a
// camt.052.001.02 BankToCustomerAccountReport document: GrpHdr (MsgId
// = msgID, CreDtTm = now), one Rpt (Id = rptID, Acct/Id/Othr/Id =
// accountID), one Bal per balance in order, and one Ntry per
// transaction in order.
//
// Every balance needs a type (OPBD/CLBD), a currency, and a date; the
// amount is emitted absolute with the direction in CdtDbtInd.
// Transactions follow the camt.053 Ntry contract exactly (see
// RenderCAMT053Statement): fail-loud on anything that would not
// survive it.
func RenderCAMT052Report(accountID, rptID, msgID string, balances []CAMT052Balance, txns []CAMT053Transaction) ([]byte, error) {
	if strings.TrimSpace(rptID) == "" {
		return nil, fmt.Errorf("%w: report ID required", ErrInvalidCAMT052Report)
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("%w: account ID required", ErrInvalidCAMT052Report)
	}
	if strings.TrimSpace(msgID) == "" {
		msgID = rptID
	}
	rpt := camt052xRpt{
		ID:      rptID,
		CreDtTm: time.Now().UTC().Format(time.RFC3339),
		Acct:    camt053xAcct{ID: camt053xAcctID{Othr: camt053xOthr{ID: accountID}}},
	}
	for i, b := range balances {
		if b.Type != CAMT052BalanceOPBD && b.Type != CAMT052BalanceCLBD {
			return nil, fmt.Errorf("%w: balance %d: bad type %q", ErrInvalidCAMT052Report, i+1, b.Type)
		}
		currency, err := normalizeCurrency(b.Currency)
		if err != nil {
			return nil, fmt.Errorf("%w: balance %d: bad currency %q", ErrInvalidCAMT052Report, i+1, b.Currency)
		}
		if b.At.IsZero() {
			return nil, fmt.Errorf("%w: balance %d: missing date", ErrInvalidCAMT052Report, i+1)
		}
		amount := b.AmountCents
		if amount < 0 {
			amount = -amount
		}
		rpt.Bals = append(rpt.Bals, camt052xBal{
			Tp:        camt052xBalTp{CdOrPrtry: camt052xCdOrPrtry{Cd: b.Type}},
			Amt:       camt053xAmt{Ccy: currency, Value: camt053FormatAmount(amount)},
			CdtDbtInd: camt052IndicatorOf(b.AmountCents),
			Dt:        camt053xDt{DtTm: b.At.UTC().Format(time.RFC3339)},
		})
	}
	for i, t := range txns {
		currency, err := normalizeCurrency(t.Currency)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: bad currency %q", ErrInvalidCAMT052Report, i+1, t.Currency)
		}
		ind, err := camt053IndicatorOf(t.Direction)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", ErrInvalidCAMT052Report, i+1, err)
		}
		if t.PostedAt.IsZero() {
			return nil, fmt.Errorf("%w: entry %d: missing booking date", ErrInvalidCAMT052Report, i+1)
		}
		amount := t.AmountCents
		if amount < 0 {
			amount = -amount
		}
		txDtls := camt053xTxDtls{Refs: camt053xRefs{EndToEndID: t.EndToEndID}}
		if t.Memo != "" {
			txDtls.RmtInf = &camt053xRmtInf{Ustrd: []string{t.Memo}}
		}
		rpt.Ntries = append(rpt.Ntries, camt053xNtry{
			Amt:       camt053xAmt{Ccy: currency, Value: camt053FormatAmount(amount)},
			CdtDbtInd: ind,
			BookgDt:   camt053xDt{DtTm: t.PostedAt.UTC().Format(time.RFC3339)},
			NtryDtls:  camt053xDtls{TxDtls: []camt053xTxDtls{txDtls}},
		})
	}
	doc := camt052xDocument{
		BkToCstmrAcctRpt: camt052xBkToCstmrAcctRpt{
			GrpHdr: camt052xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
			Rpts:   []camt052xRpt{rpt},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT052Report, err)
	}
	out := append([]byte(xml.Header), body...)
	return out, nil
}

// ExportCAMT052 renders a camt.052 intraday report for one account
// from the journal: the [Since, Until) entries as Ntry rows plus
// per-currency OPBD/CLBD balance snapshots. The account named in
// Rpt/Acct is the external identifier mapped under the "camt053"
// system when one exists (the bank's IBAN view), else the internal
// account ID — the same rule as ExportCAMT053.
//
// Read-only: the scan holds the read lock from start to finish, so the
// report describes one consistent point in time. It bumps no version,
// books nothing, and appends no audit-chain link — the same contract
// as ExportCAMT053 and GenerateStatement.
//
// Balance semantics: OPBD is the booked balance just before Since,
// CLBD just before Until, folded from the effective journal rows under
// the same rules as the Ntry list (pending-review rows excluded,
// currency filter applied). By construction CLBD == OPBD + net of the
// reported entries, per currency — the identity the tests pin.
//
// An account with no rows in the window exports a valid report with
// no Bal and no Ntry rows (banks do send empty intraday reports).
func (l *Ledger) ExportCAMT052(opts CAMT052ReportOptions) ([]byte, error) {
	if opts.Account == "" {
		return nil, ErrEmptyAccountID
	}
	if strings.TrimSpace(opts.RptID) == "" {
		return nil, fmt.Errorf("%w: report ID required", ErrInvalidCAMT052Report)
	}
	if opts.Since.IsZero() || opts.Until.IsZero() {
		return nil, fmt.Errorf("%w: since and until are required", ErrInvalidCAMT052Report)
	}
	if !opts.Since.Before(opts.Until) {
		return nil, fmt.Errorf("%w: since must be before until", ErrInvalidCAMT052Report)
	}
	currency := ""
	if opts.Currency != "" {
		var cerr error
		currency, cerr = normalizeCurrency(opts.Currency)
		if cerr != nil {
			return nil, cerr
		}
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	// opbd/clbd fold the effective journal rows on either side of the
	// window: a pending-review row never touched balances, so like
	// ExportCAMT053 it is excluded from both the snapshots and the
	// entry list. Account-centric sign: the debit leg adds, the credit
	// leg subtracts (see BalanceAt).
	opbd := map[string]int64{}
	clbd := map[string]int64{}
	seen := map[string]bool{}
	var rows []JournalEntry
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			continue
		}
		if currency != "" && e.Currency != currency {
			continue
		}
		seen[e.Currency] = true
		// Account-centric sign, exactly like BalanceAt: the debit
		// leg adds, the credit leg subtracts — a self-transfer nets
		// to zero.
		var delta int64
		if e.DebitAccount == opts.Account {
			delta += e.AmountCents
		}
		if e.CreditAccount == opts.Account {
			delta -= e.AmountCents
		}
		if e.CreatedAt.Before(opts.Since) {
			opbd[e.Currency] += delta
		}
		if e.CreatedAt.Before(opts.Until) {
			clbd[e.Currency] += delta
		}
		if !e.CreatedAt.Before(opts.Since) && e.CreatedAt.Before(opts.Until) {
			rows = append(rows, l.redactEntryMemoLocked(e))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})

	currencies := make([]string, 0, len(seen))
	for c := range seen {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	var balances []CAMT052Balance
	for _, c := range currencies {
		balances = append(balances,
			CAMT052Balance{Type: CAMT052BalanceOPBD, Currency: c, AmountCents: opbd[c], At: opts.Since},
			CAMT052Balance{Type: CAMT052BalanceCLBD, Currency: c, AmountCents: clbd[c], At: opts.Until},
		)
	}

	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemCAMT053); ok {
		acctID = ext
	}

	txns := make([]CAMT053Transaction, 0, len(rows))
	for _, e := range rows {
		dir := CAMT053DirectionOut
		if e.DebitAccount == opts.Account {
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
			StmtID:      opts.RptID,
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
		msgID = opts.RptID
	}
	return RenderCAMT052Report(acctID, opts.RptID, msgID, balances, txns)
}
