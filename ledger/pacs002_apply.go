package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pacs.002.001.03 FI-to-FI payment status report apply
// (LG-409): the inbound half of the pacs.002 lifecycle.
//
// ExportPacs002 (see pacs002.go, LG-167) renders a gpi track as the
// bank-facing status report; this is the reverse direction — the
// correspondent bank answers the ledger's pacs.008.001.02 customer
// credit transfer with a pacs.002.001.03 FIToFIPmtStsRpt, and the
// report's verdict advances the gpi track it names:
//
//	ACSP  AcceptedSettlementInProcess — the bank took the instruction.
//	ACSC  AcceptedSettlementCompleted — settlement completed.
//	ACCP  AcceptedCustomerProfile     — credited to the beneficiary.
//	RJCT  Rejected                    — refused, with the reason code.
//
// The join is the UETR: the report's TxInfAndSts/OrgnlUETR names the
// tracked payment, and OrgnlGrpInfAndSts/OrgnlMsgId must be that
// track's payout's pacs.008 export MsgId (`<payout>/pacs008`, see
// pacs008ExportMsgID) — the OrgnlMsgNmId/OrgnlMsgId pairing is
// enforced so a mis-addressed report fails loud instead of applying a
// verdict to the wrong payment (the pain.002 precedent, see
// pain002.go, LG-406). A track is always linked to its payout at
// creation (see registerGPITrackLocked), so the expected MsgId is
// always well-defined; there is no unlinked-track case to invent.
//
// Application delegates to AdvanceGPI (see gpi.go, LG-165): the
// forward-only machine, same-state replay as an idempotent no-op,
// terminal/backward moves refused, RJCT without a reason refused, and
// the bank's reason code verbatim into the hash-chained `gpi_status`
// audit event. The batch is validated before anything is applied —
// an unknown UETR, a foreign MsgId, or an illegal TxSts leaves the
// track untouched.
//
// Deliberately out of scope: driving the payout instruction. Like the
// pain.002 report, a status report is a statement of fact, not an
// operator attestation — SettlePayout/FailPayout stay operator-driven
// on bank-statement evidence, and no journal entry is booked.

// pacs.002 status-report errors.
var (
	// ErrInvalidPacs002 is returned when the report is not a parseable
	// pacs.002.001.03 document: malformed XML, a missing
	// FIToFIPmtStsRpt, or a missing/empty OrgnlMsgId or OrgnlUETR.
	// Fail-loud: a half-readable bank file must not partially apply.
	ErrInvalidPacs002 = errors.New("ledger: invalid pacs.002 status report")
	// ErrPacs002UnexpectedMsgName is returned when OrgnlMsgNmId is not
	// "pacs.008.001.02": the report answers a different message type,
	// and applying it to a gpi track would misattribute the bank's
	// verdict.
	ErrPacs002UnexpectedMsgName = errors.New("ledger: pacs.002 report is not for a pacs.008 message")
	// ErrPacs002ForeignMsgID is returned when OrgnlMsgId is not the
	// `<payout>/pacs008` MsgId of the track the report's UETR names —
	// either the wrong shape at parse time or another payout's MsgId
	// at apply time. A status report for another payment is a
	// category error, not a verdict.
	ErrPacs002ForeignMsgID = errors.New("ledger: pacs.002 report answers a message of another payment")
	// ErrInvalidPacs002TxSts is returned when the transaction carries
	// a TxSts outside the pacs.002 vocabulary (ACSP/ACSC/ACCP/RJCT):
	// unknown statuses are refused, not coerced — the allowed table
	// is pacs002TxSts (see pacs002.go).
	ErrInvalidPacs002TxSts = errors.New("ledger: invalid pacs.002 transaction status: want ACSP, ACSC, ACCP or RJCT")
)

// Pacs002Report is the parsed bank status report: the report's own
// MsgId, the original pacs.008 MsgId it answers (OrgnlMsgID), the
// original message name, the UETR the verdict names, the bank's TxSts,
// and the rejection reason code (only set for RJCT).
type Pacs002Report struct {
	MsgID        string `json:"msg_id"`
	OrgnlMsgID   string `json:"orgnl_msg_id"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id"`
	UETR         string `json:"uetr"`
	TxSts        string `json:"tx_sts"`
	ReasonCode   string `json:"reason_code,omitempty"`
	ReasonInfo   string `json:"reason_info,omitempty"`
}

// pacs002ValidTxSts reports whether s is in the pacs.002 TxSts
// vocabulary: the gpi mirror (ACSP/ACSC/ACCP/RJCT). PDNG is not a
// pacs.002 verdict — the bank either took the instruction or it did
// not — so a pending report is refused, not skipped.
func pacs002ValidTxSts(s string) bool {
	switch s {
	case GPIStatusACSP, GPIStatusACSC, GPIStatusACCP, GPIStatusRJCT:
		return true
	}
	return false
}

// pacs002ValidOrgnlMsgID reports whether id has the shape of a
// pacs.008 export MsgId: a non-empty payout part plus the "/pacs008"
// suffix. The exact payout is checked at apply time against the
// track the UETR names; this is only the family gate.
func pacs002ValidOrgnlMsgID(id string) bool {
	const suffix = "/pacs008"
	if !strings.HasSuffix(id, suffix) {
		return false
	}
	return strings.TrimSpace(strings.TrimSuffix(id, suffix)) != ""
}

// ParsePacs002 parses a bank-returned ISO 20022 pacs.002.001.03
// FI-to-FI payment status report. It reuses the export wire struct
// (see pacs002.go) and validates the document shape — well-formed XML
// carrying FIToFIPmtStsRpt under the pacs.002.001.03 namespace,
// OrgnlMsgNmId == "pacs.008.001.02", a non-empty OrgnlMsgId in the
// `<payout>/pacs008` shape, a non-empty OrgnlUETR, and a TxSts in the
// ACSP/ACSC/ACCP/RJCT vocabulary — and refuses anything else
// fail-loud: a half-readable bank file must not partially apply.
// Namespaces are carried on the Document element, the export
// convention, and unknown elements are skipped, so bank extensions do
// not break parsing.
func ParsePacs002(data []byte) (*Pacs002Report, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidPacs002)
	}
	var doc pacs002Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPacs002, err)
	}
	grp := doc.Report.OrgnlGrpInf
	if strings.TrimSpace(grp.OrgnlMsgID) == "" && strings.TrimSpace(grp.OrgnlMsgNmID) == "" {
		// Nothing answered: either the document carries no
		// FIToFIPmtStsRpt at all or it is under a different
		// namespace than pacs.002.001.03.
		return nil, fmt.Errorf("%w: missing FIToFIPmtStsRpt", ErrInvalidPacs002)
	}
	msgNmID := strings.TrimSpace(grp.OrgnlMsgNmID)
	if msgNmID != pacs002OrgnlMsgName {
		return nil, fmt.Errorf("%w: got %q", ErrPacs002UnexpectedMsgName, grp.OrgnlMsgNmID)
	}
	orgnlMsgID := strings.TrimSpace(grp.OrgnlMsgID)
	if orgnlMsgID == "" {
		return nil, fmt.Errorf("%w: missing OrgnlMsgId", ErrInvalidPacs002)
	}
	// The name and the MsgId shape are paired: a report answering a
	// pacs.008 message must carry a `<payout>/pacs008` MsgId. A
	// mis-addressed report fails loud here so it can never apply a
	// verdict to the wrong payment.
	if !pacs002ValidOrgnlMsgID(orgnlMsgID) {
		return nil, fmt.Errorf("%w: %q", ErrPacs002ForeignMsgID, orgnlMsgID)
	}
	uetr := strings.TrimSpace(doc.Report.Tx.OrgnlUETR)
	if uetr == "" {
		return nil, fmt.Errorf("%w: missing OrgnlUETR", ErrInvalidPacs002)
	}
	txSts := strings.TrimSpace(doc.Report.Tx.TxSts)
	if !pacs002ValidTxSts(txSts) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPacs002TxSts, doc.Report.Tx.TxSts)
	}
	report := &Pacs002Report{
		MsgID:        strings.TrimSpace(doc.Report.GrpHdr.MsgID),
		OrgnlMsgID:   orgnlMsgID,
		OrgnlMsgNmID: msgNmID,
		UETR:         uetr,
		TxSts:        txSts,
	}
	if txSts == GPIStatusRJCT && doc.Report.Tx.StsRsnInf != nil && doc.Report.Tx.StsRsnInf.Rsn != nil {
		// The bank's reason, kept verbatim: the proprietary code
		// the export renders, never interpreted — it is the bank's
		// vocabulary, like the pain.002 reason passthrough.
		report.ReasonCode = strings.TrimSpace(doc.Report.Tx.StsRsnInf.Rsn.Prtry)
	}
	return report, nil
}

// ApplyPacs002Report applies a parsed pacs.002 report to the gpi
// track its UETR names, advancing the track along the gpi state
// machine via AdvanceGPI: ACSP/ACSC/ACCP each move the track forward
// one level (a verdict may skip a level, never move backward), RJCT
// rejects with the bank's reason code verbatim in the `gpi_status`
// audit event.
//
// The report is validated before anything is applied: an unknown
// UETR is refused (ErrGPIUnknownUETR), an OrgnlMsgId that is not
// this track's payout's `<payout>/pacs008` is refused
// (ErrPacs002ForeignMsgID), and the TxSts vocabulary is re-checked
// for hand-built reports. The verdict never moves funds and never
// touches the payout state machine — the track only mirrors what the
// bank reported.
func (l *Ledger) ApplyPacs002Report(report *Pacs002Report, actor string) (GPITrack, error) {
	if report == nil {
		return GPITrack{}, fmt.Errorf("%w: nil report", ErrInvalidPacs002)
	}
	if !pacs002ValidTxSts(report.TxSts) {
		return GPITrack{}, fmt.Errorf("%w: %q", ErrInvalidPacs002TxSts, report.TxSts)
	}
	track, ok := l.GetGPITrack(report.UETR)
	if !ok {
		return GPITrack{}, fmt.Errorf("%w: %q", ErrGPIUnknownUETR, report.UETR)
	}
	// The UETR is the join key; the MsgId is the cross-check. A
	// track always carries its payout (see registerGPITrackLocked),
	// so the expected MsgId is always defined — an empty payout
	// fails closed rather than matching anything.
	if track.PayoutID == "" || report.OrgnlMsgID != pacs008ExportMsgID(track.PayoutID) {
		return GPITrack{}, fmt.Errorf("%w: %q is not the pacs.008 MsgId of payout %q", ErrPacs002ForeignMsgID, report.OrgnlMsgID, track.PayoutID)
	}
	return l.AdvanceGPI(report.UETR, report.TxSts, report.ReasonCode, actor)
}
