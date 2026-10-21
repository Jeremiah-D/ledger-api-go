package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pain.002.001.03 customer payment status report parsing
// (LG-162).
//
// This is the inbound half of the pain.001 lifecycle: ExportPain001
// (see pain001.go) renders the payment instruction the ledger sends to
// the bank, and the bank answers with a pain.002.001.03 status report
// naming that instruction's MsgId (OrgnlMsgId) and, per transaction,
// the EndToEndId the ledger exported (the transfer ID) plus a status:
//
//	ACSC  AcceptedSettlementCompleted — the money moved.
//	ACCP  AcceptedCustomerProfile     — accepted, settlement pending.
//	RJCT  Rejected                    — refused, with a reason code.
//	PDNG  Pending                     — the bank has not decided yet.
//
// Status mapping into the pain.001 state machine (see
// pain001_status.go): ACSC/ACCP become "accepted", RJCT becomes
// "rejected" (the bank's StsRsnInf reason code and text ride the
// receipt's Reason into the hash-chained pain001_status audit event,
// verbatim — the bank's vocabulary, not the ledger's). PDNG generates
// no receipt: the transfer stays "exported" until the bank reports a
// verdict, so a pending report never invents a state the bank did not
// report. A report whose every transaction is PDNG is an explicit
// no-op: the instruction is returned unchanged, no audit event.
//
// Application reuses RecordPain001Receipts wholesale — the batch is
// validated before anything is applied, receipt replay is idempotent,
// and every applied transition emits its pain001_status audit event
// plus the domain outbox event — so ApplyPain002Report inherits the
// atomicity and the audit trail instead of reimplementing them.
//
// Deliberately out of scope: driving PayoutInstruction (see payout.go).
// A pain.002 names transfer IDs (EndToEndId), not payout IDs, so there
// is no natural join; and SettlePayout/FailPayout demand a bank
// statement line as evidence and book clearing-leg reversals — an
// operator attestation, not something a read-only status report may
// perform as a side effect. The pain.001 instruction lifecycle is the
// transfer-level truth the bank actually reports on.

// pain.002 status-report errors.
var (
	// ErrInvalidPain002 is returned when the report is not a parseable
	// pain.002.001.03 document: malformed XML, a missing
	// CstmrPmtStsRpt, or a missing/empty OrgnlMsgId or
	// OrgnlEndToEndId. Fail-loud: a half-readable bank file must not
	// partially apply.
	ErrInvalidPain002 = errors.New("ledger: invalid pain.002 status report")
	// ErrPain002UnexpectedMsgName is returned when OrgnlMsgNmId is not
	// "pain.001.001.03": the report answers a different message type
	// (e.g. a pain.008 direct debit), and applying it to a pain.001
	// instruction would misattribute the bank's verdict.
	ErrPain002UnexpectedMsgName = errors.New("ledger: pain.002 report is not for a pain.001 message")
	// ErrInvalidPain002TxSts is returned when a transaction carries a
	// TxSts outside the pain.002 vocabulary
	// (ACSC/ACCP/RJCT/PDNG): unknown statuses are refused, not
	// coerced — like the receipt vocabulary in pain001_status.go.
	ErrInvalidPain002TxSts = errors.New("ledger: invalid pain.002 transaction status: want ACSC, ACCP, RJCT or PDNG")
)

// pain002ExpectedMsgName is the only OrgnlMsgNmId this parser accepts:
// the report must answer the pain.001.001.03 export this ledger
// produces (see ExportPain001).
const pain002ExpectedMsgName = "pain.001.001.03"

// pain.002 transaction statuses (the TxSts vocabulary).
const (
	Pain002TxACSC = "ACSC" // AcceptedSettlementCompleted
	Pain002TxACCP = "ACCP" // AcceptedCustomerProfile
	Pain002TxRJCT = "RJCT" // Rejected
	Pain002TxPDNG = "PDNG" // Pending
)

// pain002Document is the wire form: Document/CstmrPmtStsRpt. The xml
// tags name local elements only, so the bank's namespace declaration
// (urn:iso:std:iso:20022:tech:xsd:pain.002.001.03 or a variant) is
// accepted without matching it, and unknown elements are skipped —
// banks extend these reports, and an extension must not refuse the
// statuses the ledger understands.
type pain002Document struct {
	XMLName xml.Name   `xml:"Document"`
	Report  pain002Rpt `xml:"CstmrPmtStsRpt"`
}

type pain002Rpt struct {
	GrpHdr       pain002GrpHdr      `xml:"GrpHdr"`
	OrgnlGrpInf  pain002OrgnlGrpInf `xml:"OrgnlGrpInfAndSts"`
	OrgnlPmtInfs []pain002PmtInf    `xml:"OrgnlPmtInfAndSts"`
}

type pain002GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain002OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
	GrpSts       string `xml:"GrpSts"`
}

type pain002PmtInf struct {
	Txs []pain002Tx `xml:"TxInfAndSts"`
}

type pain002Tx struct {
	OrgnlEndToEndID string            `xml:"OrgnlEndToEndId"`
	TxSts           string            `xml:"TxSts"`
	StsRsnInf       *pain002StsRsnInf `xml:"StsRsnInf"`
}

type pain002StsRsnInf struct {
	Rsn      *pain002Rsn `xml:"Rsn"`
	AddtlInf string      `xml:"AddtlInf"`
}

type pain002Rsn struct {
	Cd    string `xml:"Cd"`
	Prtry string `xml:"Prtry"`
}

// Pain002Tx is one transaction verdict inside a pain.002 report: the
// original EndToEndId (the ledger's transfer ID), the bank's TxSts,
// and the rejection reason split into code and free text (only set for
// RJCT).
type Pain002Tx struct {
	EndToEndID string `json:"end_to_end_id"`
	TxSts      string `json:"tx_sts"`
	ReasonCode string `json:"reason_code,omitempty"`
	ReasonInfo string `json:"reason_info,omitempty"`
}

// Pain002Report is the parsed bank status report: the report's own
// MsgId, the original pain.001 MsgId it answers (OrgnlMsgID), the
// original message name, the optional group status, and the per-
// transaction verdicts in document order.
type Pain002Report struct {
	MsgID        string      `json:"msg_id"`
	OrgnlMsgID   string      `json:"orgnl_msg_id"`
	OrgnlMsgNmID string      `json:"orgnl_msg_nm_id"`
	GrpSts       string      `json:"grp_sts,omitempty"`
	Txs          []Pain002Tx `json:"txs"`
}

// pain002ValidTxSts reports whether s is in the pain.002 TxSts
// vocabulary.
func pain002ValidTxSts(s string) bool {
	switch s {
	case Pain002TxACSC, Pain002TxACCP, Pain002TxRJCT, Pain002TxPDNG:
		return true
	}
	return false
}

// ParsePain002 parses a bank-returned ISO 20022 pain.002.001.03 customer
// payment status report. It validates the document shape — well-formed
// XML, the CstmrPmtStsRpt root, OrgnlMsgNmId == "pain.001.001.03", a
// non-empty OrgnlMsgId, and every transaction carrying a non-empty
// OrgnlEndToEndId plus a TxSts in the ACSC/ACCP/RJCT/PDNG vocabulary —
// and refuses anything else fail-loud: a half-readable bank file must
// not partially apply. Namespaces are ignored and unknown elements
// skipped, so bank extensions do not break parsing.
//
// GrpSts is carried through for disclosure only: when it disagrees with
// the per-transaction verdicts, the per-transaction verdicts win — the
// group status is the bank's summary, the TxInfAndSts entries are its
// statement of fact.
func ParsePain002(data []byte) (*Pain002Report, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidPain002)
	}
	var doc pain002Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPain002, err)
	}
	grp := doc.Report.OrgnlGrpInf
	if strings.TrimSpace(grp.OrgnlMsgNmID) != pain002ExpectedMsgName {
		return nil, fmt.Errorf("%w: got %q", ErrPain002UnexpectedMsgName, grp.OrgnlMsgNmID)
	}
	orgnlMsgID := strings.TrimSpace(grp.OrgnlMsgID)
	if orgnlMsgID == "" {
		return nil, fmt.Errorf("%w: missing OrgnlMsgId", ErrInvalidPain002)
	}
	report := &Pain002Report{
		MsgID:        strings.TrimSpace(doc.Report.GrpHdr.MsgID),
		OrgnlMsgID:   orgnlMsgID,
		OrgnlMsgNmID: strings.TrimSpace(grp.OrgnlMsgNmID),
		GrpSts:       strings.TrimSpace(grp.GrpSts),
	}
	for pi, pmtInf := range doc.Report.OrgnlPmtInfs {
		for ti, tx := range pmtInf.Txs {
			e2e := strings.TrimSpace(tx.OrgnlEndToEndID)
			if e2e == "" {
				return nil, fmt.Errorf("%w: OrgnlPmtInfAndSts[%d].TxInfAndSts[%d] missing OrgnlEndToEndId",
					ErrInvalidPain002, pi, ti)
			}
			txSts := strings.TrimSpace(tx.TxSts)
			if !pain002ValidTxSts(txSts) {
				return nil, fmt.Errorf("%w: %q for %q", ErrInvalidPain002TxSts, tx.TxSts, e2e)
			}
			ptx := Pain002Tx{EndToEndID: e2e, TxSts: txSts}
			if txSts == Pain002TxRJCT && tx.StsRsnInf != nil {
				// The bank's reason, kept verbatim: code plus free
				// text, never interpreted — it is the bank's
				// vocabulary, like the receipt Reason in
				// pain001_status.go.
				if tx.StsRsnInf.Rsn != nil {
					ptx.ReasonCode = strings.TrimSpace(tx.StsRsnInf.Rsn.Cd)
					if ptx.ReasonCode == "" {
						ptx.ReasonCode = strings.TrimSpace(tx.StsRsnInf.Rsn.Prtry)
					}
				}
				ptx.ReasonInfo = strings.TrimSpace(tx.StsRsnInf.AddtlInf)
			}
			report.Txs = append(report.Txs, ptx)
		}
	}
	return report, nil
}

// pain002ReceiptReason renders a rejected transaction's reason the way
// the pain001_status audit event records it: "CODE: free text", or
// whichever half the bank supplied.
func pain002ReceiptReason(tx Pain002Tx) string {
	switch {
	case tx.ReasonCode != "" && tx.ReasonInfo != "":
		return tx.ReasonCode + ": " + tx.ReasonInfo
	case tx.ReasonCode != "":
		return tx.ReasonCode
	default:
		return tx.ReasonInfo
	}
}

// ApplyPain002Report applies a parsed pain.002 report to the pain.001
// instruction it answers, advancing each decided transaction along the
// pain.001 state machine: ACSC/ACCP become "accepted", RJCT becomes
// "rejected" with the bank's reason code in the audit event, PDNG is
// skipped (the transfer stays "exported" — the bank has not decided).
//
// The application reuses RecordPain001Receipts, so the batch is
// validated before anything is applied, receipt replay is idempotent,
// and every transition emits its hash-chained pain001_status audit
// event plus the domain outbox event. A report whose every transaction
// is PDNG is an explicit no-op: the instruction is returned unchanged
// and nothing is recorded — a pending report is information, not a
// verdict.
func (l *Ledger) ApplyPain002Report(report *Pain002Report, actor string) (PainInstruction, error) {
	if report == nil {
		return PainInstruction{}, fmt.Errorf("%w: nil report", ErrInvalidPain002)
	}
	if _, ok := l.PainInstruction(report.OrgnlMsgID); !ok {
		return PainInstruction{}, fmt.Errorf("%w: %q", ErrUnknownPainInstruction, report.OrgnlMsgID)
	}
	receipts := make([]Pain001Receipt, 0, len(report.Txs))
	for _, tx := range report.Txs {
		var status string
		switch tx.TxSts {
		case Pain002TxACSC, Pain002TxACCP:
			// AcceptedSettlementCompleted and
			// AcceptedCustomerProfile both mean the bank took the
			// payment: the ledger's vocabulary has one accepted
			// state, and settlement completion is confirmed by
			// the operator (see SettlePain001Instruction), not
			// inferred from the bank's wording.
			status = Pain001StateAccepted
		case Pain002TxRJCT:
			status = Pain001StateRejected
		default:
			// PDNG: the bank has not decided. No receipt, no
			// transition — the transfer stays exported until a
			// verdict arrives.
			continue
		}
		receipt := Pain001Receipt{TxID: tx.EndToEndID, Status: status}
		if status == Pain001StateRejected {
			receipt.Reason = pain002ReceiptReason(tx)
		}
		receipts = append(receipts, receipt)
	}
	if len(receipts) == 0 {
		// Every transaction is PDNG (or the report names none): a
		// pending report records nothing.
		rec, _ := l.PainInstruction(report.OrgnlMsgID)
		return rec, nil
	}
	return l.RecordPain001Receipts(report.OrgnlMsgID, receipts, actor)
}
