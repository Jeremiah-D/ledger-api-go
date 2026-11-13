package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.011.001.03 mandate cancellation request export
// (LG-402): the outbound half of the mandate lifecycle's closing
// move.
//
// CreateMandate (LG-125) registers the standing authorization;
// ExportPain009 (LG-199) initiates it with the debtor's bank;
// ExportPain010 (LG-207) amends it; this module asks the bank to
// cancel it — the request that stops future collections under the
// mandate. The four messages are orthogonal by construction:
//
//   - pain.009 initiates the mandate (MndtId + CdtrSchmeId + Dbtr):
//     no money moves, no collection is ordered.
//   - pain.010 amends the mandate (OrgnlMndt + AmdmntInf): the
//     authorization itself changes, still no money moves.
//   - pain.008 (LG-166) executes collections under the mandate as
//     currently amended: the money moves.
//   - pain.011 cancels the mandate (OrgnlMndt + CxlRsn): the
//     authorization is asked to die, still no money moves.
//
// Version note (checked 2026-11-12, not assumed): pain.011.001.02
// is not a registered ISO 20022 version. The registry knows only
// .001.01 (Payments Mandate MDR, August 2009, MandateCancellation-
// RequestV01) and .001.03 (Payments Mandates MDR, May 2013,
// MandateCancellationRequestV03; Nordea MIGs exist for .01 and .03
// only, and an exact-phrase search for "pain.011.001.02" /
// "MandateCancellationRequestV02" returns no such document). Per
// the LG-402 fallback rule the export therefore renders .001.03.
// The document shape follows the LG-402 spec's simplified
// Mndt{OrgnlMndt, CxlRsn} rendering rather than the full V03
// MandateCancellation3 shape, which wraps the same two blocks in
// UndrlygCxlDtls, mandates CxlRsn [1..1], and carries the full
// Cdtr/Dbtr blocks — a later round can adopt the full shape if a
// bank-facing validator requires it.
//
// The export renders a cancellation *request*, not the cancellation
// itself: the ledger's mandate row is untouched. The bank confirms
// (or rejects) via pain.012; the ledger's internal cancellation is
// the separate operator decision CancelMandate (mandate.go), never
// an automatic consequence of exporting. That is why the export is
// read-only (no version bump, no journal row, no mandate mutation)
// and still emits one "pain011_exported" audit event, the pain.009
// convention: an authorization revocation is a compliance-sensitive
// action, and the trail records every request generated.
//
// Cancellation content, the backlog's contract: OrgnlMndt carries
// the original mandate (MndtId + CdtrSchmeId); CxlRsn carries the
// reason, but only when one is given — an empty reason omits the
// block entirely, never a defaulted code. The reason rides
// Rsn/Prtry (MandateReason1Choice), the pain.007 precedent for a
// free-form reason: no cancellation-reason code table is invented
// here, so a code vocabulary is never asserted.
//
// The state gate differs from pain.009/010 on purpose: active and
// suspended mandates can both be cancelled — a suspended mandate
// still exists at the bank, so it still needs a revocation notice;
// the active-only gate of initiation/amendment would block exactly
// the mandates most in need of cancelling. A cancelled mandate
// refuses with ErrMandateNotActive and an expired one with
// ErrMandateExpired; both are 422-class. Unlike pain.009, no
// debtor/creditor IBAN is required: the request identifies only the
// mandate itself (MndtId + CdtrSchmeId) and the message definition
// carries no account, so a mandate without registered IBANs still
// cancels.
//
// MsgId is deterministic: `pain011-<mandate ID>-<scheme ID>`. The
// same mandate cancelled under the same scheme re-renders
// byte-identical apart from CreDtTm.

// pain.011 export errors.
var (
	// ErrMissingPain011CreditorScheme is returned by ExportPain011
	// when no creditor scheme ID is supplied, the pain.009
	// convention: SEPA mandates CdtrSchmeId on every mandate
	// message, and the ledger stores no creditor identifier to
	// default from.
	ErrMissingPain011CreditorScheme = errors.New("ledger: pain.011 export refused: creditor scheme ID is required")
)

// pain011MsgID derives the deterministic message ID from the mandate
// and the creditor scheme — the pain009MsgID convention: the same
// cancellation re-renders with the same ID, and a different mandate
// or scheme changes it.
func pain011MsgID(mandateID, schemeID string) string {
	return "pain011-" + mandateID + "-" + schemeID
}

// ExportPain011 renders an ISO 20022 pain.011.001.03 mandate
// cancellation request for one registered mandate: the OrgnlMndt
// (mandate ID + creditor scheme) plus, when a reason is given, the
// CxlRsn (Rsn/Prtry).
//
// Fail-closed: a missing creditor scheme ID, an unknown mandate, a
// cancelled mandate, and an expired mandate are all refused. A
// suspended mandate is *not* refused: it still exists at the bank.
// No IBAN is required on either side.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, consumes no idempotency key, and mutates
// no mandate row. Like the pain.009 export, it still emits one
// structured audit event ("pain011_exported", details msg_id/
// mandate_id/creditor_scheme_id/reason_code) so the compliance
// trail records every generated cancellation request.
func (l *Ledger) ExportPain011(mandateID, creditorSchemeID, reasonCode string) ([]byte, error) {
	if strings.TrimSpace(creditorSchemeID) == "" {
		return nil, ErrMissingPain011CreditorScheme
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
	case MandateStatusActive, MandateStatusSuspended:
		// Both can be cancelled: a suspended mandate still exists
		// at the bank, so it still needs a revocation notice.
	default:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: mandate %q status %q", ErrMandateNotActive, mandateID, eff)
	}

	msgID := pain011MsgID(m.ID, creditorSchemeID)
	reason := strings.TrimSpace(reasonCode)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.011.001.03">` + "\n")
	b.WriteString("<MndtCxlReq>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("</GrpHdr>\n")
	b.WriteString("<Mndt>\n")
	b.WriteString("<OrgnlMndt>\n")
	b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
	b.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n",
		ofxEscape(creditorSchemeID)))
	b.WriteString("</OrgnlMndt>\n")
	if reason != "" {
		b.WriteString(fmt.Sprintf("<CxlRsn><Rsn><Prtry>%s</Prtry></Rsn></CxlRsn>\n",
			ofxEscape(reason)))
	}
	b.WriteString("</Mndt>\n")
	b.WriteString("</MndtCxlReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like the pain.009 export: the version brackets the
	// export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain011_exported",
		Actor:         "ExportPain011",
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
