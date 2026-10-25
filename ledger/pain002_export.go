package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ISO 20022 pain.002.001.10 customer payment status report export
// (LG-205).
//
// This is the outbound mirror of the inbound pain.002 parser (LG-162):
// where ParsePain002 reads the bank's status report about a pain.001
// instruction the ledger sent, ExportPain002 renders the ledger's own
// status report about that instruction — for the enterprise customer
// that initiated the payments, so it can learn, per payment, whether
// the bank has settled, accepted, rejected, or not yet decided it.
// The per-transaction states are the pain.001 lifecycle states (see
// pain001_status.go), mapped onto the pain.002 TxSts vocabulary the
// bank itself uses:
//
//	exported -> PDNG  (Pending: the bank has not reported a verdict)
//	accepted -> ACCP  (AcceptedCustomerProfile: accepted, settlement pending)
//	settled  -> ACSC  (AcceptedSettlementCompleted: the money moved)
//	rejected -> RJCT  (Rejected, with the bank's reason carried verbatim)
//
// The mapping is the exact inverse of the inbound one (LG-162 maps
// ACSC/ACCP to "accepted" and RJCT to "rejected", and PDNG to no
// receipt at all): a report the ledger generates now can be parsed
// back by the same vocabulary without inventing a state the bank did
// not report. Note the one deliberate asymmetry: the inbound path
// collapses ACSC and ACCP into one "accepted" state, so a settled
// transfer reports ACSC here only after the operator's settle
// confirmation (see SettlePain001Instruction) — the ledger never
// claims settlement on the bank's behalf from an acceptance alone.
//
// Group status (GrpSts) is derived, not stored: unanimous per-tx
// statuses report that status; any mix reports PART (partially
// decided). A mixed report never claims the group verdict the
// transactions do not support.
//
// MsgId is deterministic in (original MsgId, per-transaction states):
// the same instruction in the same states re-renders the same report
// MsgId, and any state change changes it — the same content-bound
// principle as the pain.001 MsgId (see pain001MsgID). CreDtTm is the
// generation time, like the pain.001 export: the MsgId is the stable
// identity, the timestamp is the generation record.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like the
// pain.001 export, it still emits one structured audit event
// ("pain002_exported", details msg_id/orgnl_msg_id/tx_count/grp_sts)
// so the compliance trail records every generated report. An unknown
// MsgId is refused with ErrUnknownPainInstruction (the LG-99 error):
// a status report about an instruction the ledger never exported
// would be a fabrication, not a report.

// pain002ExportState maps a pain.001 lifecycle state onto its pain.002
// TxSts. Unknown states are refused, not coerced — a lifecycle state
// the mapper does not know must never render as a bank verdict.
func pain002ExportState(state string) (string, error) {
	switch state {
	case Pain001StateExported:
		return Pain002TxPDNG, nil
	case Pain001StateAccepted:
		return Pain002TxACCP, nil
	case Pain001StateSettled:
		return Pain002TxACSC, nil
	case Pain001StateRejected:
		return Pain002TxRJCT, nil
	}
	return "", fmt.Errorf("ledger: unknown pain.001 state %q for pain.002 export", state)
}

// pain002ExportGroupState derives the report's GrpSts from the
// per-transaction statuses: unanimous reports that status, any mix
// reports PART.
func pain002ExportGroupState(statuses []string) string {
	if len(statuses) == 0 {
		return Pain002TxPDNG
	}
	first := statuses[0]
	for _, s := range statuses[1:] {
		if s != first {
			return "PART"
		}
	}
	return first
}

// pain002ExportMsgID derives the deterministic report MsgId from the
// original instruction MsgId and the per-transaction states in TxID
// order: the same states re-render the same ID, a state change
// changes it.
func pain002ExportMsgID(origMsgID string, txs []Pain001TxState) string {
	var b strings.Builder
	b.WriteString(origMsgID)
	for _, tx := range txs {
		b.WriteString("|")
		b.WriteString(tx.TxID)
		b.WriteString("=")
		b.WriteString(tx.State)
		b.WriteString("=")
		b.WriteString(tx.Reason)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "pain002-" + origMsgID + "-" + hex.EncodeToString(sum[:])[:16]
}

// ExportPain002 renders an ISO 20022 pain.002.001.10 customer payment
// status report for the pain.001 instruction named by msgID, reporting
// the bank-processing status of every payment the customer initiated
// under that instruction. See the package comment above for the state
// mapping, the group-status derivation, and the deterministic MsgId.
func (l *Ledger) ExportPain002(msgID string) ([]byte, error) {
	l.mu.RLock()
	rec, ok := l.painInstructions[msgID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrUnknownPainInstruction, msgID)
	}
	// Copy the transaction states out under the lock, in TxID order
	// (the snapshot form), so the render below reads a stable set.
	txs := make([]Pain001TxState, len(rec.Txs))
	copy(txs, rec.Txs)
	sort.Slice(txs, func(i, j int) bool { return txs[i].TxID < txs[j].TxID })

	statuses := make([]string, len(txs))
	for i, tx := range txs {
		sts, err := pain002ExportState(tx.State)
		if err != nil {
			l.mu.RUnlock()
			return nil, err
		}
		statuses[i] = sts
	}
	grpSts := pain002ExportGroupState(statuses)
	reportMsgID := pain002ExportMsgID(msgID, txs)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.10">` + "\n")
	b.WriteString("<CstmrPmtStsRpt>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(reportMsgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n")
	b.WriteString("</GrpHdr>\n")
	b.WriteString("<OrgnlGrpInfAndSts>\n")
	b.WriteString(fmt.Sprintf("<OrgnlMsgId>%s</OrgnlMsgId>\n", ofxEscape(msgID)))
	b.WriteString("<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId>\n")
	b.WriteString(fmt.Sprintf("<GrpSts>%s</GrpSts>\n", grpSts))
	b.WriteString("</OrgnlGrpInfAndSts>\n")
	b.WriteString("<OrgnlPmtInfAndSts>\n")
	for i, tx := range txs {
		b.WriteString("<TxInfAndSts>\n")
		b.WriteString(fmt.Sprintf("<OrgnlEndToEndId>%s</OrgnlEndToEndId>\n", ofxEscape(tx.TxID)))
		b.WriteString(fmt.Sprintf("<TxSts>%s</TxSts>\n", statuses[i]))
		if statuses[i] == Pain002TxRJCT && tx.Reason != "" {
			// The bank's reason rides verbatim in AddtlInf: it is
			// the bank's vocabulary, not the ledger's, so the
			// export never reinterprets it as a reason code.
			b.WriteString("<StsRsnInf>\n")
			b.WriteString(fmt.Sprintf("<AddtlInf>%s</AddtlInf>\n", ofxEscape(tx.Reason)))
			b.WriteString("</StsRsnInf>\n")
		}
		b.WriteString("</TxInfAndSts>\n")
	}
	b.WriteString("</OrgnlPmtInfAndSts>\n")
	b.WriteString("</CstmrPmtStsRpt>\n")
	b.WriteString("</Document>\n")
	doc := []byte(b.String())

	// Read-only, like the pain.001 export: the version brackets the
	// export but does not move — the audit event proves the report
	// was generated against a known ledger state.
	l.emitAudit(AuditEvent{
		Op:            "pain002_exported",
		Actor:         "ExportPain002",
		TraceID:       reportMsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":       reportMsgID,
			"orgnl_msg_id": msgID,
			"tx_count":     len(txs),
			"grp_sts":      grpSts,
		},
	})
	l.mu.RUnlock()
	return doc, nil
}
