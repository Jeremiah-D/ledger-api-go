package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CAMT.060 account reporting request export (LG-400, fintech: ISO 20022
// 对账索要面).
//
// ExportCAMT060 is the request side of the camt.052/053/054 family
// (LG-170/381/384): where those exports *render* the reports the
// ledger already holds, this one *asks* the account servicing
// institution to send one — a camt.060.001.03 `AccountReportingRequest`
// naming the account (by its registered IBAN) and, when given, the
// reporting period. The bank answers with a camt.052, camt.053, or
// camt.054, according to the standing agreement; the request itself
// names no report type (no `ReqdMsgNmId`), so it never invents a
// preference the operator did not state.
//
// Structure (verified against the ISO 20022 camt.060 message
// definition — `AccountReportingRequestV03`: `GrpHdr` with `MsgId` /
// `CreDtTm`, one `RptgReq` with `Id`, `Acct/Id/IBAN`, and optional
// `RptgPrd/FrToDt`):
//
//	Document (xmlns camt.060.001.03)
//	  AcctRptgReq
//	    GrpHdr: MsgId (deterministic, see camt060MsgID), CreDtTm
//	    RptgReq: Id (= MsgId), Acct/Id/IBAN, RptgPrd/FrToDt
//	      (FrDtTm / ToDtTm, only when the window is given)
//
// Semantics, made explicit:
//
//   - Read-only: the lookup holds the read lock, bumps no version,
//     books nothing, and appends no audit-chain link — the MT300
//     precedent. A request is not an attestation, so unlike
//     ExportPain001 it emits no audit event either.
//   - Fail-closed on identity: the account is named by its registered
//     IBAN, never by the internal account ID and never by an empty or
//     forged identifier — an account with no registered IBAN is
//     refused with ErrMissingAccountIBAN (the pain.001 precedent).
//   - Unknown account: an account the ledger has never seen — no
//     journal entries and no IBAN registration — is a 404-class
//     error (ErrUnknownCAMT060Account). This differs deliberately
//     from the camt.053 export, where an unknown account yields a
//     valid empty statement: a *request* to a bank about an account
//     that does not exist here is a client error, not a report.
//   - MsgId is deterministic in (account, window): the same request
//     re-renders the same MsgId. CreDtTm is the honest wall-clock
//     creation time, so two exports a second apart differ in that
//     one line only — the document is byte-identical modulo CreDtTm
//     (the camt.053 HTTP-test precedent, which strips CreDtTm and
//     pins MsgId). Falsifying CreDtTm to force byte-identity would
//     mislabel when the request was created.
//   - Reporting period: `FrToDt` is written only when a window is
//     given. Per ISO `DatePeriodDetails`, `FrDtTm` is mandatory and
//     `ToDtTm` optional, so a lone `until` (no start) cannot be
//     expressed — it is refused with ErrInvalidCAMT060Export rather
//     than rendered with an invented start date.

// CAMT.060 export errors.
var (
	// ErrInvalidCAMT060Export is returned by ExportCAMT060 and
	// RenderCAMT060Request when the request cannot be rendered: an
	// inverted time range, a lone `until` with no `since` (the
	// reporting period cannot express an open start), or an empty
	// message/request ID.
	ErrInvalidCAMT060Export = errors.New("ledger: invalid camt.060 export options")
	// ErrUnknownCAMT060Account is returned by ExportCAMT060 when the
	// account has no journal entries and no registered IBAN — the
	// ledger has never seen it. It is a 404-class error: a report
	// request about an account that does not exist here is a client
	// error, not an empty report. (One sentinel beyond
	// ErrInvalidCAMT060Export: the specified 404 mapping is
	// otherwise unreachable — the handler cannot distinguish
	// "unknown account" from "invalid request" from a single
	// error value. See the LG-400 round log.)
	ErrUnknownCAMT060Account = errors.New("ledger: camt.060 export: unknown account")
)

// CAMT060ExportOptions tunes ExportCAMT060.
//
// Account is required. FromDate/ToDate bound the requested reporting
// period (half-open [FromDate, ToDate), the camt.053 export
// convention); both zero means "no period" and omits RptgPrd. A lone
// ToDate with no FromDate is refused: DatePeriodDetails requires a
// start date, and the export will not invent one.
type CAMT060ExportOptions struct {
	Account  AccountID
	FromDate time.Time
	ToDate   time.Time
}

// camt060MsgID derives the deterministic message/request ID from the
// account and the window: the same request re-renders the same ID,
// and any change (a different account, a moved window) changes it.
// Zero bounds render as "0", the pain001MsgID convention.
func camt060MsgID(account AccountID, from, until time.Time) string {
	return fmt.Sprintf("camt060-%s-%s-%s", account, camt060UnixOrZero(from), camt060UnixOrZero(until))
}

func camt060UnixOrZero(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d", t.UTC().Unix())
}

// The camt060x* structs are the export-side XML model, the
// camt053_export.go convention: a strict writer carrying the
// camt.060.001.03 namespace on the root and omitting every empty
// element, so the emitted document is what a bank expects to receive.
type camt060xDocument struct {
	XMLName     xml.Name            `xml:"urn:iso:std:iso:20022:tech:xsd:camt.060.001.03 Document"`
	AcctRptgReq camt060xAcctRptgReq `xml:"AcctRptgReq"`
}

type camt060xAcctRptgReq struct {
	GrpHdr  camt060xGrpHdr  `xml:"GrpHdr"`
	RptgReq camt060xRptgReq `xml:"RptgReq"`
}

type camt060xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type camt060xRptgReq struct {
	ID      string           `xml:"Id"`
	Acct    camt060xAcct     `xml:"Acct"`
	RptgPrd *camt060xRptgPrd `xml:"RptgPrd,omitempty"`
}

type camt060xAcct struct {
	ID camt060xAcctID `xml:"Id"`
}

type camt060xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type camt060xRptgPrd struct {
	FrToDt camt060xFrToDt `xml:"FrToDt"`
}

type camt060xFrToDt struct {
	FrDtTm string `xml:"FrDtTm,omitempty"`
	ToDtTm string `xml:"ToDtTm,omitempty"`
}

// RenderCAMT060Request renders the account reporting request for the
// account named by iban: GrpHdr (MsgId = msgID, CreDtTm = now) and
// one RptgReq (Id = msgID, Acct/Id/IBAN = iban, RptgPrd/FrToDt only
// when the window is given). The caller owns the identity checks;
// this is the pure, testable renderer underneath ExportCAMT060.
func RenderCAMT060Request(account, iban, msgID string, from, until time.Time) ([]byte, error) {
	if strings.TrimSpace(account) == "" {
		return nil, fmt.Errorf("%w: account required", ErrInvalidCAMT060Export)
	}
	if strings.TrimSpace(iban) == "" {
		return nil, fmt.Errorf("%w: IBAN required", ErrInvalidCAMT060Export)
	}
	if strings.TrimSpace(msgID) == "" {
		return nil, fmt.Errorf("%w: message ID required", ErrInvalidCAMT060Export)
	}
	if !from.IsZero() && !until.IsZero() && from.After(until) {
		return nil, fmt.Errorf("%w: from is after until", ErrInvalidCAMT060Export)
	}
	if from.IsZero() && !until.IsZero() {
		return nil, fmt.Errorf("%w: until without since cannot be expressed in FrToDt", ErrInvalidCAMT060Export)
	}
	var prd *camt060xRptgPrd
	if !from.IsZero() || !until.IsZero() {
		fr := camt060xFrToDt{}
		if !from.IsZero() {
			fr.FrDtTm = from.UTC().Format(time.RFC3339)
		}
		if !until.IsZero() {
			fr.ToDtTm = until.UTC().Format(time.RFC3339)
		}
		prd = &camt060xRptgPrd{FrToDt: fr}
	}
	doc := camt060xDocument{
		AcctRptgReq: camt060xAcctRptgReq{
			GrpHdr: camt060xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
			RptgReq: camt060xRptgReq{
				ID:      msgID,
				Acct:    camt060xAcct{ID: camt060xAcctID{IBAN: iban}},
				RptgPrd: prd,
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT060Export, err)
	}
	return append([]byte(xml.Header), body...), nil
}

// ExportCAMT060 renders a camt.060 account reporting request for one
// account: the ask that brings back a camt.052/053/054 report.
//
// An account is known when the ledger has journal entries for it or
// a registered IBAN; anything else is ErrUnknownCAMT060Account. A
// known account with no registered IBAN is refused with
// ErrMissingAccountIBAN — the request must name the real IBAN, never
// the internal ID.
//
// Read-only: the read lock is held for the whole lookup, so the
// request describes one consistent point in time. It bumps no
// version, books nothing, and appends no audit-chain link.
func (l *Ledger) ExportCAMT060(opts CAMT060ExportOptions) ([]byte, error) {
	if opts.Account == "" {
		return nil, ErrEmptyAccountID
	}
	if !opts.FromDate.IsZero() && !opts.ToDate.IsZero() && opts.FromDate.After(opts.ToDate) {
		return nil, fmt.Errorf("%w: from is after until", ErrInvalidCAMT060Export)
	}
	if opts.FromDate.IsZero() && !opts.ToDate.IsZero() {
		return nil, fmt.Errorf("%w: until without since cannot be expressed in FrToDt", ErrInvalidCAMT060Export)
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	_, hasEntries := l.byAccount[opts.Account]
	reg, hasIBAN := l.accountIBANs[opts.Account]
	if !hasEntries && !hasIBAN {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCAMT060Account, opts.Account)
	}
	if !hasIBAN {
		return nil, fmt.Errorf("%w: account %q", ErrMissingAccountIBAN, opts.Account)
	}
	return RenderCAMT060Request(string(opts.Account), reg.IBAN, camt060MsgID(opts.Account, opts.FromDate, opts.ToDate), opts.FromDate, opts.ToDate)
}
