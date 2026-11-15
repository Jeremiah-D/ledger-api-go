package ledger

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.014.001.07 creditor payment activation request
// status report export (LG-429): the outbound half of the
// request-for-payment lifecycle's closing move — the debtor's side
// of the answer.
//
// ExportPain013 (LG-407) renders the creditor's request; this
// module renders the debtor's response to that request, after the
// debtor has decided locally through AcceptRfP / RejectRfP
// (rfp.go, LG-171). It is the outbound mirror of the LG-416
// inbound: LG-416 consumes a pain.014 the counterparty sent; this
// export renders the pain.014 this ledger sends. The order is
// local decision first, export second: accept or reject the RfP,
// then tell the creditor the verdict.
//
// Version note: the export renders pain.014.001.07
// (CdtrPmtActvtnReqStsRpt, namespace
// urn:iso:std:iso:20022:tech:xsd:pain.014.001.07), the same version
// the LG-416 inbound gates on — the FedNow/RTP one, verified
// against the moov-io rtp20022 gen/pain_014_001_07 bindings (see
// pain014_inbound.go). The rendering reuses the inbound wire
// struct (pain014Document and friends) for the Marshal, so the
// exported shape is by construction the shape ParsePain014
// accepts: the round-trip test nails it down.
//
// Field mapping, from the RfP and the export options:
//
//   - GrpHdr MsgId is deterministic: `pain014-<rfp ID>`, the
//     pain013MsgID convention. CreDtTm is the wall clock;
//     InitgPty is the ledger, as in the pain.013 export.
//   - OrgnlGrpInfAndSts answers the original request:
//     OrgnlMsgId is pain013MsgID(rfpID) — the pain.013 message
//     this responds to — OrgnlMsgNmId is the pain014OrgnlMsgName
//     constant ("pain.013.001.07", never hand-written),
//     OrgnlCreDtTm is the RfP's creation time, OrgnlNbOfTxs 1.
//   - OrgnlPmtInfAndSts OrgnlPmtInfId is the same pain.013
//     MsgId (the pain.013 PmtInfId is its MsgId).
//   - TxInfAndSts OrgnlEndToEndId is the RfP ID and TxSts is the
//     verdict taken from RfP.Status: paid -> ACCP, rejected ->
//     RJCT. OrgnlTxRef Amt/InstdAmt carries the RfP amount and
//     currency, the original request's amount.
//   - ACCP renders PmtCondSts/AccptdAmt at the full RfP amount:
//     the RfP has no partial-accept semantics, so a full-amount
//     accept is the only accept this export can state.
//   - RJCT renders StsRsnInf/Rsn/Cd with opts.ReasonCode carried
//     verbatim — never interpreted, never translated, never
//     invented from a code table the ledger does not hold. An
//     empty reason renders no StsRsnInf block at all. A reason
//     code on an ACCP export is ignored, not rendered: an accept
//     carries no refusal reason.
//
// The state gate is terminal-only: paid and rejected render their
// verdict; issued refuses with ErrRfPNotIssued — there is no
// verdict to report yet — and expired (swept, or lazily past its
// deadline) refuses with ErrRfPExpired — an expired request has
// no response left to give. A paid or rejected RfP renders its
// verdict even past its ExpiresAt: the decision was already made,
// and the verdict is what the export reports.
//
// The export is read-only, like ExportPain013: it takes the read
// lock, bumps no version, appends no chain link, changes no RfP
// row, and consumes no idempotency key. It still emits one
// structured audit event ("pain014_exported", details msg_id/
// rfp_id/tx_sts, plus reason_code when a reason is rendered) so
// the compliance trail records every generated response.
//
// Explicitly not this module: it does not change the RfP state
// machine, AcceptRfP/RejectRfP, ParsePain014, or
// ApplyPain014Response; it does no partial accept (AccptdAmt is
// always the full amount); it invents no RJCT reason-code table;
// it persists no copy of the exported document (only the audit
// summary).

// Pain014ExportOptions carries the optional caller inputs to
// ExportPain014.
type Pain014ExportOptions struct {
	// ReasonCode is the RJCT reason, rendered verbatim as
	// StsRsnInf/Rsn/Cd. It is ignored on an ACCP export and
	// omitted when empty.
	ReasonCode string
}

// pain014MsgID derives the deterministic message ID from the RfP —
// the pain013MsgID convention: the same RfP re-renders with the
// same ID, and a different RfP changes it.
func pain014MsgID(rfpID string) string {
	return "pain014-" + rfpID
}

// ExportPain014 renders an ISO 20022 pain.014.001.07 creditor
// payment activation request status report for one terminal
// request for payment: the debtor's verdict on the pain.013
// request, paid -> ACCP and rejected -> RJCT.
//
// Fail-closed: an unknown RfP, an issued RfP, and an expired RfP
// are all refused. The export moves no funds and changes no RfP
// state — exporting a verdict is not accepting or rejecting; the
// RfP status still comes from AcceptRfP/RejectRfP.
func (l *Ledger) ExportPain014(rfpID string, opts Pain014ExportOptions) ([]byte, error) {
	l.mu.RLock()
	r, ok := l.rfps[rfpID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrRfPNotFound, rfpID)
	}
	now := time.Now()
	// The verdict is the RfP's status, not a parameter: the
	// export reports a decision already made, it never makes
	// one. A terminal RfP renders its verdict even past its
	// ExpiresAt; only an issued RfP is expiry-gated, lazily,
	// like ExportPain013.
	var txSts string
	switch r.Status {
	case RfPStatusPaid:
		txSts = pain014TxStatusACCP
	case RfPStatusRejected:
		txSts = pain014TxStatusRJCT
	case RfPStatusIssued:
		if rfpExpiredLocked(r, now) {
			l.mu.RUnlock()
			return nil, fmt.Errorf("%w: rfp %q", ErrRfPExpired, rfpID)
		}
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q status %q", ErrRfPNotIssued, rfpID, r.Status)
	case RfPStatusExpired:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q", ErrRfPExpired, rfpID)
	default:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q status %q", ErrRfPNotIssued, rfpID, r.Status)
	}

	msgID := pain014MsgID(r.ID)
	tx := pain014TxSts{
		OrgnlEndToEndID: r.ID,
		TxSts:           txSts,
		OrgnlTxRef: pain014OrgnlTxRef{
			Amt: &pain014AmtChoice{
				InstdAmt: &pain014Amount{Ccy: r.Currency, Value: pain001Amount(r.AmountCents)},
			},
		},
	}
	if txSts == pain014TxStatusACCP {
		// Full-amount accept only: the RfP has no
		// partial-accept semantics.
		tx.PmtCondSts = &pain014PmtCondSts{
			AccptdAmt: &pain014Amount{Ccy: r.Currency, Value: pain001Amount(r.AmountCents)},
		}
	} else if reason := strings.TrimSpace(opts.ReasonCode); reason != "" {
		// The debtor's reason, carried verbatim: never
		// interpreted, never translated, never invented from a
		// code table. An empty reason renders no block; a
		// reason on an ACCP export is ignored above.
		tx.StsRsnInf = &pain014StsRsnInf{
			Rsn: pain014Rsn{Cd: reason},
		}
	}
	doc := pain014Document{
		Report: pain014Report{
			GrpHdr: pain014GrpHdr{
				MsgID:    msgID,
				CreDtTm:  now.UTC().Format(time.RFC3339),
				InitgPty: pain014InitgPty{Nm: "LEDGER"},
			},
			OrgnlGrpInfAndSts: pain014OrgnlGrp{
				OrgnlMsgID:   pain013MsgID(r.ID),
				OrgnlMsgNmID: pain014OrgnlMsgName,
				OrgnlCreDtTm: r.CreatedAt.UTC().Format(time.RFC3339),
				OrgnlNbOfTxs: "1",
			},
			OrgnlPmtInfAndSts: pain014OrgnlPmt{
				OrgnlPmtInfID: pain013MsgID(r.ID),
				TxInfAndSts:   tx,
			},
		},
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		l.mu.RUnlock()
		return nil, fmt.Errorf("ledger: pain.014 export: marshal: %w", err)
	}

	// Read-only, like the pain.013 export: the version brackets
	// the export but does not move.
	details := map[string]any{
		"msg_id": msgID,
		"rfp_id": rfpID,
		"tx_sts": txSts,
	}
	if tx.StsRsnInf != nil {
		details["reason_code"] = tx.StsRsnInf.Rsn.Cd
	}
	l.emitAudit(AuditEvent{
		Op:            "pain014_exported",
		Actor:         "ExportPain014",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details:       details,
	})
	l.mu.RUnlock()
	return append([]byte(xml.Header), out...), nil
}
