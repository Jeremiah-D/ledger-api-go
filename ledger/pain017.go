package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.017.001.03 mandate copy request export (LG-433):
// the outbound half of asking the debtor's bank for the copy of the
// mandate it holds.
//
// CreateMandate (LG-125) registers the standing authorization;
// ExportPain009 (LG-199) initiates it with the debtor's bank;
// ExportPain010 (LG-207) amends it; ExportPain011 (LG-402) asks
// the bank to cancel it; ExportPain018 (LG-432) asks the bank to
// suspend it; this module asks the bank for the copy of the
// mandate it holds — the request a creditor sends when reconciling
// a mandate, evidencing a disputed collection, or migrating an
// authorization to another creditor. The messages are orthogonal
// by construction:
//
//   - pain.009 initiates the mandate (MndtId + CdtrSchmeId + Dbtr):
//     no money moves, no collection is ordered.
//   - pain.010 amends the mandate (OrgnlMndt + AmdmntInf): the
//     authorization itself changes, still no money moves.
//   - pain.008 (LG-166) executes collections under the mandate as
//     currently amended: the money moves.
//   - pain.011 cancels the mandate (OrgnlMndt + CxlRsn): the
//     authorization is asked to die, still no money moves.
//   - pain.018 suspends the mandate (OrgnlMndt + SspnsnRsn): the
//     authorization is asked to pause, still no money moves.
//   - pain.017 requests a copy of the mandate (UndrlygCpyReqDtls
//     with OrgnlMndt + MndtSts): the authorization is only asked
//     to be shown, still no money moves.
//
// Version and structure note (checked 2026-11-15, not assumed):
// pain.017.001.03 (Payments Mandates MDR, May 2013,
// MandateCopyRequestV03; docs.rs `iso-20022-pain` crate,
// `pain_017_001_03` module: `Document` root field
// `mndt_cpy_req`, `MandateCopyRequestV03` = GrpHdr +
// UndrlygCpyReqDtls [1..n] of MandateCopy3; `MandateCopy3` =
// OrgnlMsgInf [0..1] + OrgnlMndt [1..1] (OriginalMandate9Choice)
// + MndtSts [0..1] (MandateStatus1Choice) + SplmtryData [0..n]).
// The document shape follows the LG-433 spec's simplified
// rendering: a single UndrlygCpyReqDtls carrying OrgnlMndt (MndtId
// + CdtrSchmeId, the pain.011 precedent — the full V03 shape puts
// the full Mandate17 block, or the bare OrgnlMndtId arm, behind
// that choice) plus MndtSts. OrgnlMsgInf is omitted: the ledger
// keeps no record of the original initiation message to reference,
// and the copy request identifies the mandate itself. A later
// round can adopt the full shape if a bank-facing validator
// requires it.
//
// MndtSts (checked 2026-11-15, not assumed): MandateStatus1Choice
// offers Cd (ExternalMandateStatus1Code) or Prtry (Max35Text).
// ExternalMandateStatus1Code has exactly four codes, per the
// official ISO 20022 external code set publication (verified
// against the code-set JSON: ACTV "The mandate is active.",
// CANC "The mandate is cancelled.", EXPI "The mandate has passed
// the end date.", SUSP "The mandate is put on hold."). The
// ledger's four effective statuses map onto them one-to-one —
// active -> ACTV, suspended -> SUSP, cancelled -> CANC, expired
// -> EXPI — so no code is invented and no ledger status is left
// without a code. MndtSts is always rendered: the mandate always
// has an effective status at export time.
//
// The export renders a copy *request*, not the copy itself: the
// bank returns the copy it holds (its parsing is a later round's
// item), and the ledger's mandate row is untouched. That is why
// the export is read-only (no version bump, no journal row, no
// mandate mutation) and still emits one "pain017_exported" audit
// event, the pain.009 convention: requesting a mandate copy is a
// compliance-sensitive action, and the trail records every
// request generated.
//
// The state gate differs from pain.009/010/011/018 on purpose:
// there is no state gate at all. A copy request is a record
// request, not a lifecycle move — active, suspended, cancelled,
// and expired mandates can all be asked for, and a cancelled
// mandate's copy is precisely the evidence a dispute needs. The
// only gate is existence: an unknown mandate is refused with
// ErrMandateNotFound. Unlike pain.009, no debtor/creditor IBAN
// is required: the request identifies only the mandate itself
// (MndtId + CdtrSchmeId) and the message definition carries no
// account, so a mandate without registered IBANs still exports.
//
// MsgId is deterministic: `pain017-<mandate ID>-<scheme ID>`. The
// same mandate requested under the same scheme re-renders
// byte-identical apart from CreDtTm.

// pain.017 export errors.
var (
	// ErrMissingPain017CreditorScheme is returned by ExportPain017
	// when no creditor scheme ID is supplied, the pain.009
	// convention: SEPA mandates CdtrSchmeId on every mandate
	// message, and the ledger stores no creditor identifier to
	// default from.
	ErrMissingPain017CreditorScheme = errors.New("ledger: pain.017 export refused: creditor scheme ID is required")
)

// pain017MsgID derives the deterministic message ID from the mandate
// and the creditor scheme — the pain009MsgID convention: the same
// copy request re-renders with the same ID, and a different mandate
// or scheme changes it.
func pain017MsgID(mandateID, schemeID string) string {
	return "pain017-" + mandateID + "-" + schemeID
}

// pain017MandateStatusCode maps a ledger effective mandate status
// onto its ExternalMandateStatus1Code. The code list has exactly
// four codes (see the header note), one per effective status, so
// the mapping is total over the ledger's status domain; an
// unexpected status falls back to the empty string and the caller
// omits MndtSts rather than emitting an invented code.
func pain017MandateStatusCode(effectiveStatus string) string {
	switch effectiveStatus {
	case MandateStatusActive:
		return "ACTV"
	case MandateStatusSuspended:
		return "SUSP"
	case MandateStatusCancelled:
		return "CANC"
	case MandateStatusExpired:
		return "EXPI"
	default:
		return ""
	}
}

// ExportPain017 renders an ISO 20022 pain.017.001.03 mandate copy
// request for one registered mandate: the UndrlygCpyReqDtls carrying
// the OrgnlMndt (mandate ID + creditor scheme) plus the MndtSts
// (Cd, the ExternalMandateStatus1Code for the mandate's effective
// status at export time).
//
// Fail-closed: a missing creditor scheme ID and an unknown mandate
// are refused. There is deliberately no state gate: active,
// suspended, cancelled, and expired mandates are all exportable.
// No IBAN is required on either side.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, consumes no idempotency key, and mutates
// no mandate row. Like the pain.009 export, it still emits one
// structured audit event ("pain017_exported", details msg_id/
// mandate_id/creditor_scheme_id) so the compliance trail records
// every generated copy request.
func (l *Ledger) ExportPain017(mandateID, creditorSchemeID string) ([]byte, error) {
	if strings.TrimSpace(creditorSchemeID) == "" {
		return nil, ErrMissingPain017CreditorScheme
	}

	l.mu.RLock()
	m, ok := l.mandates[mandateID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrMandateNotFound, mandateID)
	}
	now := time.Now()
	eff := mandateEffectiveStatusLocked(m, now)
	statusCode := pain017MandateStatusCode(eff)

	msgID := pain017MsgID(m.ID, creditorSchemeID)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.017.001.03">` + "\n")
	b.WriteString("<MndtCpyReq>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("</GrpHdr>\n")
	b.WriteString("<UndrlygCpyReqDtls>\n")
	b.WriteString("<OrgnlMndt>\n")
	b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
	b.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n",
		ofxEscape(creditorSchemeID)))
	b.WriteString("</OrgnlMndt>\n")
	if statusCode != "" {
		b.WriteString(fmt.Sprintf("<MndtSts><Cd>%s</Cd></MndtSts>\n", statusCode))
	}
	b.WriteString("</UndrlygCpyReqDtls>\n")
	b.WriteString("</MndtCpyReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like the pain.009 export: the version brackets the
	// export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain017_exported",
		Actor:         "ExportPain017",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"msg_id":             msgID,
			"mandate_id":         mandateID,
			"creditor_scheme_id": creditorSchemeID,
		},
	})
	l.mu.RUnlock()
	return []byte(b.String()), nil
}
