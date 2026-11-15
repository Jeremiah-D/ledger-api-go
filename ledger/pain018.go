package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.018.001.03 mandate suspension request export
// (LG-432): the outbound half of the mandate lifecycle's pause move.
//
// CreateMandate (LG-125) registers the standing authorization;
// ExportPain009 (LG-199) initiates it with the debtor's bank;
// ExportPain010 (LG-207) amends it; ExportPain011 (LG-402) asks
// the bank to cancel it; this module asks the bank to suspend it —
// the request that pauses future collections under the mandate
// without ending the authorization. The messages are orthogonal
// by construction:
//
//   - pain.009 initiates the mandate (MndtId + CdtrSchmeId + Dbtr):
//     no money moves, no collection is ordered.
//   - pain.010 amends the mandate (OrgnlMndt + AmdmntInf): the
//     authorization itself changes, still no money moves.
//   - pain.008 (LG-166) executes collections under the mandate as
//     currently amended: the money moves.
//   - pain.018 suspends the mandate (OrgnlMndt + SspnsnRsn): the
//     authorization is asked to pause, still no money moves.
//   - pain.011 cancels the mandate (OrgnlMndt + CxlRsn): the
//     authorization is asked to die, still no money moves.
//
// Version note (checked 2026-11-15, not assumed): the registry
// knows pain.018.001.03 (Payments Mandates MDR, May 2013,
// MandateSuspensionRequestV03; docs.rs `iso-20022-pain` crate,
// `pain_018_001_03` module: `Document` root field
// `mndt_sspnsn_req`, `MandateSuspensionRequestV03` =
// GrpHdr + UndrlygSspnsnDtls [1..n]). The document shape follows
// the LG-432 spec's simplified `MndtSspnsnReq{SspnsnReqId,
// OrgnlMndt, SspnsnRsn}` rendering rather than the full V03 shape,
// which wraps the same blocks in UndrlygSspnsnDtls, carries the
// full Cdtr/Dbtr blocks, and adds an optional OrgnlMsgInf — a
// later round can adopt the full shape if a bank-facing validator
// requires it. This is the same simplification precedent as
// pain.011 (LG-402), whose full V03 shape likewise wraps in
// UndrlygCxlDtls.
//
// SspnsnRsn multiplicity (checked 2026-11-15, not assumed): in
// the full V03 definition `MandateSuspension3.sspnsn_rsn` is a
// plain (non-Option) `MandateSuspensionReason2` field — [1..1],
// mandatory. Per the LG-432 fallback rule, a missing reason is
// therefore refused (handler 400, `ErrMissingPain018Reason`),
// never a defaulted code and never a silently omitted block: the
// export never emits a document without SspnsnRsn. The reason
// rides Rsn/Prtry (MandateSuspensionReason1Choice), the pain.011
// precedent for a free-form reason: `MandateSuspensionReason2`
// is {Orgtr [0..1], Rsn: MandateSuspensionReason1Choice [1..1],
// AddtlInf [0..n]} and the choice offers Cd
// (ExternalMandateSuspensionReason1Code) or Prtry — no
// suspension-reason code table is invented here, so a code
// vocabulary is never asserted.
//
// The export renders a suspension *request*, not the suspension
// itself: the ledger's mandate row is untouched. The bank
// confirms (or rejects) via pain.012; the ledger's internal
// suspension is the separate operator decision SuspendMandate
// (mandate.go), never an automatic consequence of exporting.
// That is why the export is read-only (no version bump, no
// journal row, no mandate mutation) and still emits one
// "pain018_exported" audit event, the pain.009 convention: a
// mandate suspension is a compliance-sensitive action, and the
// trail records every request generated.
//
// Suspension content, the backlog's contract: OrgnlMndt carries
// the original mandate (MndtId + CdtrSchmeId); SspnsnRsn carries
// the reason (Rsn/Prtry, always present — see above). SspnsnReqId
// carries the same deterministic identifier as GrpHdr MsgId: the
// full V03 shape requires a per-request SspnsnReqId [1..1], and
// reusing the deterministic MsgId keeps exactly one identifier
// for the request instead of inventing a second id scheme.
//
// The state gate differs from pain.011 on purpose: only active
// mandates can be suspended — a suspension request targets a
// live authorization, and asking the bank to pause an already
// suspended mandate is empty talk. A suspended or cancelled
// mandate refuses with ErrMandateNotActive and an expired one
// with ErrMandateExpired; both are 422-class. Unlike pain.009,
// no debtor/creditor IBAN is required: the request identifies
// only the mandate itself (MndtId + CdtrSchmeId) and the message
// definition carries no account, so a mandate without registered
// IBANs still suspends.
//
// MsgId is deterministic: `pain018-<mandate ID>-<scheme ID>`. The
// same mandate suspended under the same scheme re-renders
// byte-identical apart from CreDtTm.

// pain.018 export errors.
var (
	// ErrMissingPain018CreditorScheme is returned by ExportPain018
	// when no creditor scheme ID is supplied, the pain.009
	// convention: SEPA mandates CdtrSchmeId on every mandate
	// message, and the ledger stores no creditor identifier to
	// default from.
	ErrMissingPain018CreditorScheme = errors.New("ledger: pain.018 export refused: creditor scheme ID is required")
	// ErrMissingPain018Reason is returned by ExportPain018 when no
	// suspension reason is supplied. SspnsnRsn is [1..1] in the
	// full V03 definition (see the header note), so a reasonless
	// request is refused rather than rendered without one — no
	// default reason code is ever invented.
	ErrMissingPain018Reason = errors.New("ledger: pain.018 export refused: suspension reason is required")
)

// pain018MsgID derives the deterministic message ID from the mandate
// and the creditor scheme — the pain009MsgID convention: the same
// suspension re-renders with the same ID, and a different mandate
// or scheme changes it.
func pain018MsgID(mandateID, schemeID string) string {
	return "pain018-" + mandateID + "-" + schemeID
}

// ExportPain018 renders an ISO 20022 pain.018.001.03 mandate
// suspension request for one registered mandate: the OrgnlMndt
// (mandate ID + creditor scheme) plus the SspnsnRsn (Rsn/Prtry),
// with SspnsnReqId carrying the deterministic MsgId.
//
// Fail-closed: a missing creditor scheme ID, a missing reason, an
// unknown mandate, a suspended or cancelled mandate, and an
// expired mandate are all refused. Only an active mandate can be
// suspended. No IBAN is required on either side.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, consumes no idempotency key, and mutates
// no mandate row. Like the pain.009 export, it still emits one
// structured audit event ("pain018_exported", details msg_id/
// mandate_id/creditor_scheme_id/reason_code) so the compliance
// trail records every generated suspension request.
func (l *Ledger) ExportPain018(mandateID, creditorSchemeID, reasonCode string) ([]byte, error) {
	if strings.TrimSpace(creditorSchemeID) == "" {
		return nil, ErrMissingPain018CreditorScheme
	}
	reason := strings.TrimSpace(reasonCode)
	if reason == "" {
		return nil, ErrMissingPain018Reason
	}

	l.mu.RLock()
	m, ok := l.mandates[mandateID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrMandateNotFound, mandateID)
	}
	now := time.Now()
	eff := mandateEffectiveStatusLocked(m, now)
	switch eff {
	case MandateStatusExpired:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: mandate %q", ErrMandateExpired, mandateID)
	case MandateStatusActive:
		// Only a live authorization can be asked to pause.
	default:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: mandate %q status %q", ErrMandateNotActive, mandateID, eff)
	}

	msgID := pain018MsgID(m.ID, creditorSchemeID)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.018.001.03">` + "\n")
	b.WriteString("<MndtSspnsnReq>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("</GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<SspnsnReqId>%s</SspnsnReqId>\n", ofxEscape(msgID)))
	b.WriteString("<OrgnlMndt>\n")
	b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
	b.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n",
		ofxEscape(creditorSchemeID)))
	b.WriteString("</OrgnlMndt>\n")
	b.WriteString(fmt.Sprintf("<SspnsnRsn><Rsn><Prtry>%s</Prtry></Rsn></SspnsnRsn>\n",
		ofxEscape(reason)))
	b.WriteString("</MndtSspnsnReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like the pain.009 export: the version brackets the
	// export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain018_exported",
		Actor:         "ExportPain018",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"msg_id":             msgID,
			"mandate_id":         mandateID,
			"creditor_scheme_id": creditorSchemeID,
			"reason_code":        reason,
		},
	})
	l.mu.RUnlock()
	return []byte(b.String()), nil
}
