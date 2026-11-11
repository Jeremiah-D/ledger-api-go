package ledger

import (
	"encoding/xml"
	"fmt"
	"time"
)

// camt.055.001.01 customer payment cancellation request (LG-389).
//
// camt.056 (see camt056.go, LG-168) is the FI-to-FI cancellation
// request: the bank asks the bank. camt.055 is the customer-side
// counterpart: the originator (the customer) asks its bank to cancel
// a payment instruction it already issued. One open cancellation
// record (see PayoutCancellation, camt056.go) renders as either
// message — same record, two shapes — and the counterparty answers
// both with a camt.029 status report (see ApplyCAMT029Report).
//
// The document is the Nordea MIG / iotafinance camt.055.001.01 shape:
// Document/CstmrPmtCxlReq with Assgnmt (the assignment: who asks
// whom, when) and Undrlyg/TxInf (the instruction under cancellation).
// One case addresses exactly one original instruction: the UETR and
// the payout ID locate it, the reason code says why.
//
// The "write only what the ledger holds" convention (see
// ExportCancellationRequest) applies throughout: Case and CtrlData
// are optional in the schema and omitted — the ledger holds no case
// reference and no control data — and OrgnlEndToEndId is omitted
// because the ledger holds no independent end-to-end ID; inventing
// one would anchor the cancellation to an identifier nobody issued.
// The original message name is pacs.008.001.02 (the
// pacs002OrgnlMsgName convention): the instruction being recalled is
// a customer credit transfer, whatever rail it travelled on.
//
// Read-only, like the camt.056 export: the record is read as a
// snapshot copy under the read lock — no version bump, no journal
// rows, no audit event. A UETR with no open (requested) cancellation
// is refused fail-closed with ErrNoOpenCancellation.

// camt.055 wire structs: Document/CstmrPmtCxlReq. The namespace rides
// the Document element, the camt053_export.go convention.
type camt055Document struct {
	XMLName xml.Name   `xml:"urn:iso:std:iso:20022:tech:xsd:camt.055.001.01 Document"`
	Request camt055Req `xml:"CstmrPmtCxlReq"`
}

type camt055Req struct {
	Assgnmt camt055Assgnmt `xml:"Assgnmt"`
	Undrlyg camt055Undrlyg `xml:"Undrlyg"`
}

type camt055Assgnmt struct {
	ID      string       `xml:"Id"`
	Assgnr  camt055Party `xml:"Assgnr"`
	Assgne  camt055Party `xml:"Assgne"`
	CreDtTm string       `xml:"CreDtTm"`
}

type camt055Party struct {
	Nm string `xml:"Nm"`
}

type camt055Undrlyg struct {
	TxInf camt055TxInf `xml:"TxInf"`
}

type camt055TxInf struct {
	CxlID        string             `xml:"CxlId"`
	OrgnlGrpInf  camt055OrgnlGrpInf `xml:"OrgnlGrpInf"`
	OrgnlInstrID string             `xml:"OrgnlInstrId"`
	OrgnlUETR    string             `xml:"OrgnlUETR"`
	CxlRsnInf    camt055CxlRsnInf   `xml:"CxlRsnInf"`
}

type camt055OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type camt055CxlRsnInf struct {
	Rsn *camt055Rsn `xml:"Rsn"`
}

type camt055Rsn struct {
	// Prtry carries the CxRsn reason code verbatim: cancellation
	// reasons are the operator's proprietary vocabulary, not ISO
	// external codes — the pacs002.go StsRsnInf convention.
	Prtry string `xml:"Prtry"`
}

// ExportCAMT055 renders the open cancellation for uetr as an ISO 20022
// camt.055.001.01 CustomerPaymentCancellationRequest document — the
// message the customer sends to its bank.
//
// Assgnmt/Id is the deterministic cancellation MsgId
// ("<uetr>/cxl/<seq>", see cancellationMsgID); Assgnr names the
// instructing side's best-known identifier — the payee account's
// registered BIC when one exists (see SetAccountIBAN), else the
// internal account ID — and Assgne the payout's external destination
// reference; CreDtTm is the request time. The underlying transaction
// names the original instruction by payout ID (OrgnlGrpInf/
// OrgnlMsgId, modelled as pacs.008.001.02, and OrgnlInstrId, the
// instruction identifier the ledger holds) and by UETR, with the
// operator's reason code as CxlRsnInf/Rsn/Prtry.
//
// The export is deterministic: MsgID is deterministic per attempt
// and CreDtTm is the stored request time, so a repeat export renders
// byte-identical output.
func (l *Ledger) ExportCAMT055(uetr string) ([]byte, error) {
	l.mu.RLock()
	c, ok := l.payoutCancellations[uetr]
	if !ok || c.Status != CancellationRequested {
		l.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrNoOpenCancellation, uetr)
		}
		return nil, fmt.Errorf("%w: %q is %s", ErrNoOpenCancellation, uetr, c.Status)
	}
	rec := copyCancellation(c)
	// Tracks are never deleted and cancellations only open from a
	// live track, so the payout and IBAN lookups below always hit.
	p := l.payouts[rec.PayoutID]
	iban, hasIBAN := l.accountIBANs[p.PayeeAccount]
	l.mu.RUnlock()

	assignor := string(p.PayeeAccount)
	if hasIBAN && iban.BIC != "" {
		assignor = iban.BIC
	}
	doc := camt055Document{
		Request: camt055Req{
			Assgnmt: camt055Assgnmt{
				ID:      rec.MsgID,
				Assgnr:  camt055Party{Nm: assignor},
				Assgne:  camt055Party{Nm: p.Destination},
				CreDtTm: rec.RequestedAt.UTC().Format(time.RFC3339),
			},
			Undrlyg: camt055Undrlyg{
				TxInf: camt055TxInf{
					CxlID: rec.MsgID,
					OrgnlGrpInf: camt055OrgnlGrpInf{
						OrgnlMsgID:   rec.PayoutID,
						OrgnlMsgNmID: pacs002OrgnlMsgName,
					},
					OrgnlInstrID: rec.PayoutID,
					OrgnlUETR:    rec.UETR,
					CxlRsnInf:    camt055CxlRsnInf{Rsn: &camt055Rsn{Prtry: rec.ReasonCode}},
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ledger: camt.055 marshal: %v", err)
	}
	return append([]byte(xml.Header), body...), nil
}
