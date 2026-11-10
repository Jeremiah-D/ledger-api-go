package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CAMT.054 debit/credit notification export (LG-384, fintech: 银行逐笔借记/贷记通知导出).
//
// ExportCAMT054 is the inverse of ImportCAMT054 (see camt054.go,
// LG-164): it renders the account's effective journal rows as an ISO
// 20022 `camt.054.001.08` `BankToCustomerDebitCreditNotification`
// document — one Ntfctn block per entry, one Ntry each — so the
// operator can hand the bank, the auditor, or any camt.054 reader the
// per-entry notifications the import side consumes.
//
// Round-trip: RenderCAMT054Notification emits exactly what
// ParseCAMT054 reads back — Ntfctn/Id, Acct/Id/Othr/Id, Amt+Ccy,
// CdtDbtInd, BookgDt/DtTm (UTC, full RFC 3339 so the instant
// survives), Refs/EndToEndId, and the remittance text — so
// ParseCAMT054(ExportCAMT054(...)) returns the same notifications
// (NtfctnID carrying the "camt054/<entryID>/1" stable identity, Seq
// always 1 because each Ntfctn holds exactly one Ntry). The export is
// read-only like ExportCAMT053: no journal rows, no hash chain, no
// version bump.
//
// Direction is account-centric, mirroring the importer: the account
// as the debit leg reads CRDT (money in), as the credit leg DBIT
// (money out). Amounts are absolute — the sign lives in the
// indicator, exactly like the parsed CAMT054Notification. The entry's
// idempotency key rides in Refs/EndToEndId (the entry ID when the row
// carries no key), so the bank's references trace back to ledger rows.
//
// The notification batch identifier is the GrpHdr/MsgId: the caller
// may pass one, else it defaults to "camt054-<account>" — stable
// across repeat exports, so two exports of the same window differ
// only in the wall-clock CreDtTm.

// ErrInvalidCAMT054Export is returned by ExportCAMT054 and
// RenderCAMT054Notification when the request cannot name a valid
// notification: empty account, an invalid currency filter, an
// inverted time range, or a notification missing its currency,
// direction, or booking date.
var ErrInvalidCAMT054Export = errors.New("ledger: invalid camt.054 export options")

// CAMT054ExportOptions tunes ExportCAMT054.
//
// Account is required. MsgID defaults to the notification batch
// identifier ("camt054-<account>"). Since/Until bound the exported
// entries (half-open [Since, Until); zero means unbounded). Currency
// optionally filters to one ISO code. Entries are the account's
// effective journal rows in (CreatedAt, ID) order — pending-review
// rows never touched balances, so a bank notification must not show
// them (the camt.053 export's rule).
type CAMT054ExportOptions struct {
	Account  AccountID
	MsgID    string
	Since    time.Time
	Until    time.Time
	Currency string
}

// camt054IndicatorOf maps the account-centric direction to the
// CdtDbtInd code.
func camt054IndicatorOf(d CAMT054Direction) (string, error) {
	switch d {
	case CAMT054DirectionIn:
		return "CRDT", nil
	case CAMT054DirectionOut:
		return "DBIT", nil
	default:
		return "", fmt.Errorf("%w: bad direction %q", ErrInvalidCAMT054Export, d)
	}
}

// The camt054x* structs are the export-side XML model: a strict
// writer to the import structs' tolerant reader. They carry the
// camt.054.001.08 namespace on the root and omit every empty element,
// so the emitted document is what a bank expects to receive. The
// import path (camt054.go) is untouched.
type camt054xDocument struct {
	XMLName xml.Name             `xml:"urn:iso:std:iso:20022:tech:xsd:camt.054.001.08 Document"`
	Ntfctn  camt054xDbtCdtNtfctn `xml:"BkToCstmrDbtCdtNtfctn"`
}

type camt054xDbtCdtNtfctn struct {
	GrpHdr  camt054xGrpHdr   `xml:"GrpHdr"`
	Ntfctns []camt054xNtfctn `xml:"Ntfctn"`
}

type camt054xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type camt054xNtfctn struct {
	ID      string         `xml:"Id"`
	Acct    camt054xAcct   `xml:"Acct"`
	Entries []camt054xNtry `xml:"Ntry"`
}

type camt054xAcct struct {
	ID camt054xAcctID `xml:"Id"`
}

type camt054xAcctID struct {
	Othr camt054xOthr `xml:"Othr"`
}

type camt054xOthr struct {
	ID string `xml:"Id"`
}

type camt054xNtry struct {
	Amt       camt054xAmt  `xml:"Amt"`
	CdtDbtInd string       `xml:"CdtDbtInd"`
	BookgDt   camt054xDt   `xml:"BookgDt"`
	NtryDtls  camt054xDtls `xml:"NtryDtls"`
}

type camt054xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt054xDt struct {
	DtTm string `xml:"DtTm"`
}

type camt054xDtls struct {
	TxDtls []camt054xTxDtls `xml:"TxDtls"`
}

type camt054xTxDtls struct {
	Refs   camt054xRefs    `xml:"Refs"`
	RmtInf *camt054xRmtInf `xml:"RmtInf,omitempty"`
}

type camt054xRefs struct {
	EndToEndID string `xml:"EndToEndId,omitempty"`
}

type camt054xRmtInf struct {
	Ustrd []string `xml:"Ustrd"`
}

// RenderCAMT054Notification renders notifications as a
// camt.054.001.08 BankToCustomerDebitCreditNotification document:
// GrpHdr (MsgId = msgID, CreDtTm = now), and one Ntfctn per
// notification (Id = camt054NotificationID — "camt054/<entryID>/1",
// the entry's stable identity — Acct/Id/Othr/Id = accountID, one
// Ntry each).
//
// Every notification needs a currency, a direction, and a booking
// date; the amount is emitted absolute with the direction in
// CdtDbtInd. The document round-trips through ParseCAMT054: parsing
// the output returns notifications equal to the input (PostedAt
// normalized to UTC). Fail-loud on anything that would not survive
// the round trip.
func RenderCAMT054Notification(accountID, msgID string, notifications []CAMT054Notification) ([]byte, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("%w: account ID required", ErrInvalidCAMT054Export)
	}
	if strings.TrimSpace(msgID) == "" {
		msgID = "camt054-" + accountID
	}
	doc := camt054xDocument{
		Ntfctn: camt054xDbtCdtNtfctn{
			GrpHdr: camt054xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
		},
	}
	for i, n := range notifications {
		currency, err := normalizeCurrency(n.Currency)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: bad currency %q", ErrInvalidCAMT054Export, i+1, n.Currency)
		}
		ind, err := camt054IndicatorOf(n.Direction)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", ErrInvalidCAMT054Export, i+1, err)
		}
		if n.PostedAt.IsZero() {
			return nil, fmt.Errorf("%w: entry %d: missing booking date", ErrInvalidCAMT054Export, i+1)
		}
		amount := n.AmountCents
		if amount < 0 {
			amount = -amount
		}
		txDtls := camt054xTxDtls{Refs: camt054xRefs{EndToEndID: n.EndToEndID}}
		if n.Memo != "" {
			txDtls.RmtInf = &camt054xRmtInf{Ustrd: []string{n.Memo}}
		}
		doc.Ntfctn.Ntfctns = append(doc.Ntfctn.Ntfctns, camt054xNtfctn{
			ID:   camt054NotificationID(n),
			Acct: camt054xAcct{ID: camt054xAcctID{Othr: camt054xOthr{ID: accountID}}},
			Entries: []camt054xNtry{{
				Amt:       camt054xAmt{Ccy: currency, Value: camt053FormatAmount(amount)},
				CdtDbtInd: ind,
				BookgDt:   camt054xDt{DtTm: n.PostedAt.UTC().Format(time.RFC3339)},
				NtryDtls:  camt054xDtls{TxDtls: []camt054xTxDtls{txDtls}},
			}},
		})
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT054Export, err)
	}
	out := append([]byte(xml.Header), body...)
	return out, nil
}

// ExportCAMT054 renders a camt.054 debit/credit notification for one
// account from the journal. The account named in Ntfctn/Acct is the
// external identifier mapped under the "camt054" system when one
// exists (see ExternalSystemCAMT054 — usually the IBAN the bank
// expects), else the internal account ID.
//
// Read-only: the scan holds the read lock from start to finish, so
// the notification describes one consistent point in time. It bumps
// no version, books nothing, and appends no audit-chain link — the
// same contract as GenerateStatement and ExportCAMT053.
//
// An account with no rows in the window exports a valid empty
// notification (banks do send those). Note the asymmetry:
// ParseCAMT054 rejects documents with zero entries, so an empty
// export does not re-parse — the exporter is honest about the window,
// the parser is strict about importing only real notifications.
func (l *Ledger) ExportCAMT054(opts CAMT054ExportOptions) ([]byte, error) {
	if opts.Account == "" {
		return nil, ErrEmptyAccountID
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
		return nil, fmt.Errorf("%w: since is after until", ErrInvalidCAMT054Export)
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	var rows []JournalEntry
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportCAMT053 and GenerateStatement: a
			// pending-review row never touched balances, so the bank
			// notification excludes it. Re-export after approval.
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
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemCAMT054); ok {
		acctID = ext
	}

	notifications := make([]CAMT054Notification, 0, len(rows))
	for _, e := range rows {
		dir := CAMT054DirectionOut
		if e.DebitAccount == opts.Account {
			// Debit leg first: a self-transfer (both legs the
			// account) reads as an inflow, the same bias Draft()
			// uses on the import side.
			dir = CAMT054DirectionIn
		}
		amount := e.AmountCents
		if amount < 0 {
			amount = -amount
		}
		e2e := e.IdempotencyKey
		if e2e == "" {
			e2e = e.ID
		}
		notifications = append(notifications, CAMT054Notification{
			NtfctnID:    e.ID,
			Account:     opts.Account,
			Currency:    e.Currency,
			Seq:         1,
			Direction:   dir,
			AmountCents: amount,
			PostedAt:    e.CreatedAt.UTC(),
			Memo:        e.Memo,
			EndToEndID:  e2e,
		})
	}

	msgID := opts.MsgID
	if strings.TrimSpace(msgID) == "" {
		msgID = "camt054-" + acctID
	}
	return RenderCAMT054Notification(acctID, msgID, notifications)
}
