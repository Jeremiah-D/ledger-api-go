package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.009.001.02 mandate initiation request export
// (LG-199): the outbound half of the mandate lifecycle's opening
// move.
//
// CreateMandate (see mandate.go, LG-125) registers the standing
// authorization in the ledger; ExportPain009 renders that registered
// mandate as the pain.009.001.02 `MandateInitiationRequest` the
// creditor (the merchant) sends to the debtor's bank to initiate the
// mandate — the message that turns a registered agreement into a
// bank-recognized direct-debit mandate. The two are orthogonal by
// construction:
//
//   - pain.009 initiates the mandate (MndtId + CdtrSchmeId + Dbtr):
//     no money moves, no collection is ordered.
//   - pain.008 (LG-166) executes collections under an already
//     initiated mandate (DrctDbtTxInf, SeqTp, ReqdColltnDt): the
//     money moves.
//   - mandate_preauth (LG-192) occupies a spending allowance
//     against a mandate: an internal ledger hold, never a message
//     to any bank.
//
// The initiation request carries the mandate's identity, the
// creditor scheme ID, both parties and their accounts, and the
// validity window (FrDt/ToDt). The creditor scheme ID must be
// supplied (SEPA requires CdtrSchmeId and the ledger stores no
// creditor identifier on the mandate — there is no honest default),
// and both the debtor (payer) and creditor (payee) need registered
// IBANs.
//
// Fail-closed, like the pain.008 export: the mandate must exist and
// be effectively active (unknown/suspended/cancelled/expired
// mandates refuse the export — an initiation request for a dead
// mandate would ask the bank to recognize an authorization that no
// longer authorizes anything).
//
// MsgId is deterministic: `pain009-<mandate ID>-<scheme ID>`. The
// same mandate initiated under the same scheme re-renders
// byte-identical apart from CreDtTm.
//
// The export is read-only: it takes the read lock, bumps no
// version, appends no chain link, and consumes no idempotency key.
// Like Reconcile, it still emits one structured audit event
// ("pain009_exported") so the compliance trail records every
// generated initiation request.

// pain.009 export errors.
var (
	// ErrMissingPain009CreditorScheme is returned by ExportPain009
	// when no creditor scheme ID is supplied. SEPA mandates
	// CdtrSchmeId on every pain.009, and the ledger stores no
	// creditor identifier on the mandate — inventing one would forge
	// the initiation request, so the export is refused instead.
	ErrMissingPain009CreditorScheme = errors.New("ledger: pain.009 export refused: creditor scheme ID is required")
)

// pain009MsgID derives the deterministic message ID from the mandate
// and the creditor scheme — the pain001MsgID convention: the same
// initiation re-renders with the same ID, and a different mandate or
// scheme changes it.
func pain009MsgID(mandateID, schemeID string) string {
	return "pain009-" + mandateID + "-" + schemeID
}

// ExportPain009 renders an ISO 20022 pain.009.001.02 mandate
// initiation request for one registered mandate (see CreateMandate).
//
// Semantics, made explicit so the debtor's bank can trust them:
//   - MndtId is the mandate ID; CdtrSchmeId is the caller-supplied
//     creditor scheme ID — the ledger stores none, so it is never
//     defaulted.
//   - The debtor is the mandate's payer (the customer), the creditor
//     the mandate's payee (the merchant); their account identifiers
//     are the registered IBANs, the debtor's BIC rides DbtrAgt when
//     registered.
//   - FrDt is the mandate's ValidFrom date (omitted when ValidFrom
//     is zero — effective since the beginning of time, the mandate.go
//     convention); ToDt is the mandate's ValidUntil date, always
//     present because a mandate without an end is rejected at
//     creation.
//   - Amounts never ride a pain.009: the initiation request carries
//     the authorization, not a collection — the per-debit cap stays
//     in the ledger, not on the wire.
//   - MsgId is deterministic in (mandate, scheme).
//   - Fail-closed: an unknown, suspended, cancelled, or expired
//     mandate refuses the export, as do a missing creditor scheme
//     ID and missing debtor/creditor IBANs.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile and the pain.008 export, it still emits one structured
// audit event ("pain009_exported", details msg_id/mandate_id/
// creditor_scheme_id) so the compliance trail records every
// generated initiation request.
func (l *Ledger) ExportPain009(mandateID, creditorSchemeID string) ([]byte, error) {
	if strings.TrimSpace(creditorSchemeID) == "" {
		return nil, ErrMissingPain009CreditorScheme
	}

	l.mu.RLock()
	m, ok := l.mandates[mandateID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrMandateNotFound, mandateID)
	}
	now := time.Now()
	if eff := mandateEffectiveStatusLocked(m, now); eff != MandateStatusActive {
		l.mu.RUnlock()
		if eff == MandateStatusExpired {
			return nil, fmt.Errorf("%w: mandate %q", ErrMandateExpired, mandateID)
		}
		return nil, fmt.Errorf("%w: mandate %q status %q", ErrMandateNotActive, mandateID, eff)
	}
	debtorIBAN, ok := l.accountIBANs[m.Payer]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, m.Payer)
	}
	creditorIBAN, ok := l.accountIBANs[m.Payee]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, m.Payee)
	}

	msgID := pain009MsgID(m.ID, creditorSchemeID)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.001.02">` + "\n")
	b.WriteString("<MndtInitnReq>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("</GrpHdr>\n")
	b.WriteString("<Mndt>\n")
	b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
	b.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n",
		ofxEscape(creditorSchemeID)))
	b.WriteString(fmt.Sprintf("<Cdtr><Nm>%s</Nm></Cdtr>\n", ofxEscape(string(m.Payee))))
	b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", ofxEscape(creditorIBAN.IBAN)))
	b.WriteString(fmt.Sprintf("<Dbtr><Nm>%s</Nm></Dbtr>\n", ofxEscape(string(m.Payer))))
	b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", ofxEscape(debtorIBAN.IBAN)))
	if debtorIBAN.BIC != "" {
		b.WriteString(fmt.Sprintf("<DbtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></DbtrAgt>\n", ofxEscape(debtorIBAN.BIC)))
	}
	if !m.ValidFrom.IsZero() {
		b.WriteString(fmt.Sprintf("<FrDt>%s</FrDt>\n", m.ValidFrom.UTC().Format("2006-01-02")))
	}
	b.WriteString(fmt.Sprintf("<ToDt>%s</ToDt>\n", m.ValidUntil.UTC().Format("2006-01-02")))
	b.WriteString("</Mndt>\n")
	b.WriteString("</MndtInitnReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like Reconcile and the pain.008 export: the version
	// brackets the export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain009_exported",
		Actor:         "ExportPain009",
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
