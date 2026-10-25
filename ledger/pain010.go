package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.010.001.02 mandate amendment request export
// (LG-207): the outbound half of the mandate lifecycle's amendment
// move.
//
// CreateMandate (LG-125) registers the standing authorization;
// ExportPain009 (LG-199) initiates it with the debtor's bank; this
// module amends it — the creditor (or debtor) account changed, the
// per-debit cap moved, or the collection frequency changed, and the
// bank must be told before the next collection runs under the new
// terms. The three messages are orthogonal by construction:
//
//   - pain.009 initiates the mandate (MndtId + CdtrSchmeId + Dbtr):
//     no money moves, no collection is ordered.
//   - pain.010 amends the mandate (OrgnlMndt + AmdmntInf): the
//     authorization itself changes, still no money moves.
//   - pain.008 (LG-166) executes collections under the mandate as
//     currently amended: the money moves.
//
// The export renders an amendment *request*, not the amendment
// itself: the ledger's mandate row is untouched. The bank applies
// the amendment on the effective date; the ledger's next pain.008
// under the new terms is a separate operator decision, never an
// automatic consequence of exporting. That is why the export is
// read-only (no version bump, no journal row, no mandate mutation)
// and still emits one "pain010_exported" audit event, the pain.009
// convention: the compliance trail records every amendment request
// generated.
//
// Amendment content, the backlog's contract: OrgnlMndt carries the
// original mandate (MndtId + CdtrSchmeId), AmdmntInf carries the
// amended items and the effective date. Only amended items render:
// an unchanged field is absent from the document, never re-stated —
// re-stating an unchanged value would let a stale export silently
// re-impose terms the operator did not intend to touch. At least one
// amended item is required; an amendment that changes nothing is
// refused, not rendered as an empty request.
//
// Fail-closed, like the pain.009 export: the mandate must exist and
// be effectively active (unknown/suspended/cancelled/expired
// mandates refuse the export — an amendment to a dead mandate would
// ask the bank to change an authorization that no longer authorizes
// anything). That gate is also the backlog's "修改未生效" refusal: an
// amendment whose original mandate is not effectively in force has
// nothing to amend. The effective date itself must be present (a
// dateless amendment has no takeover point); a past effective date
// is *not* refused — backdated amendments are a bank decision, and
// the ledger refuses to invent that policy. The creditor scheme ID
// is required, the pain.009 convention: the ledger stores no
// creditor identifier, so there is no honest default.
//
// MsgId is deterministic: `pain010-<mandate ID>-<effective date>`.
// The same mandate amended to the same effective date re-renders
// byte-identical apart from CreDtTm; a different effective date
// changes it, so two amendment rounds for one mandate never share
// an ID.

// pain.010 export errors.
var (
	// ErrMissingPain010CreditorScheme is returned by ExportPain010
	// when no creditor scheme ID is supplied, the pain.009
	// convention: SEPA mandates CdtrSchmeId on every mandate
	// message, and the ledger stores no creditor identifier to
	// default from.
	ErrMissingPain010CreditorScheme = errors.New("ledger: pain.010 export refused: creditor scheme ID is required")
	// ErrPain010NoAmendment is returned when the amendment changes
	// nothing: every field is "unchanged". An empty amendment
	// request would ask the bank to do nothing, loudly.
	ErrPain010NoAmendment = errors.New("ledger: pain.010 export refused: amendment changes no field")
	// ErrPain010EffectiveDateRequired is returned when the
	// amendment carries no effective date: an amendment without a
	// takeover point cannot be applied by the bank.
	ErrPain010EffectiveDateRequired = errors.New("ledger: pain.010 export refused: amendment effective date is required")
)

// MandateAmendment is one requested change to a registered mandate.
// Zero values mean "unchanged", except EffectiveDate, which is
// always required:
//
//   - CreditorIBAN: the creditor's new account IBAN (the merchant's
//     account changed).
//   - DebtorIBAN: the debtor's new account IBAN (the customer's
//     account changed).
//   - MaxAmountCents: the new per-debit cap, positive when set.
//   - Frequency: the new collection frequency, one of the mandate
//     frequency vocabulary (per-day, per-week, per-month).
//   - EffectiveDate: the date the amendment takes effect
//     (YYYY-MM-DD on the wire).
type MandateAmendment struct {
	CreditorIBAN   string    `json:"creditor_iban,omitempty"`
	DebtorIBAN     string    `json:"debtor_iban,omitempty"`
	MaxAmountCents int64     `json:"max_amount_cents,omitempty"`
	Frequency      string    `json:"frequency,omitempty"`
	EffectiveDate  time.Time `json:"effective_date"`
}

// pain010MsgID derives the deterministic message ID from the mandate
// and the amendment's effective date, the pain009MsgID convention:
// the same amendment round re-renders with the same ID, and a
// different effective date changes it.
func pain010MsgID(mandateID string, effectiveDate time.Time) string {
	return "pain010-" + mandateID + "-" + effectiveDate.UTC().Format("2006-01-02")
}

// pain010AmendedFields lists the amendment's changed fields in
// document order, for the audit event: the operator can see at a
// glance what the request asked the bank to change.
func pain010AmendedFields(amd MandateAmendment) []string {
	var fields []string
	if amd.CreditorIBAN != "" {
		fields = append(fields, "creditor_iban")
	}
	if amd.DebtorIBAN != "" {
		fields = append(fields, "debtor_iban")
	}
	if amd.MaxAmountCents != 0 {
		fields = append(fields, "max_amount_cents")
	}
	if amd.Frequency != "" {
		fields = append(fields, "frequency")
	}
	return fields
}

// validateMandateAmendment fail-closes on an amendment the export
// must refuse, before any mandate is read: no changed field, no
// effective date, a negative cap, an invalid IBAN, or a frequency
// outside the mandate vocabulary. It runs before the mandate gate so
// a malformed amendment never reaches the bank-facing render.
func validateMandateAmendment(amd MandateAmendment) error {
	if len(pain010AmendedFields(amd)) == 0 {
		return ErrPain010NoAmendment
	}
	if amd.EffectiveDate.IsZero() {
		return ErrPain010EffectiveDateRequired
	}
	if amd.MaxAmountCents < 0 {
		return fmt.Errorf("%w: max_amount_cents %d is negative", ErrMandateNonPositiveMaxAmount, amd.MaxAmountCents)
	}
	if amd.CreditorIBAN != "" && !validIBAN(normalizeIBAN(amd.CreditorIBAN)) {
		return fmt.Errorf("%w: creditor IBAN %q", ErrInvalidIBAN, amd.CreditorIBAN)
	}
	if amd.DebtorIBAN != "" && !validIBAN(normalizeIBAN(amd.DebtorIBAN)) {
		return fmt.Errorf("%w: debtor IBAN %q", ErrInvalidIBAN, amd.DebtorIBAN)
	}
	if amd.Frequency != "" && !validMandateFrequency(amd.Frequency) {
		return fmt.Errorf("%w: %q", ErrInvalidMandateFrequency, amd.Frequency)
	}
	return nil
}

// ExportPain010 renders an ISO 20022 pain.010.001.02 mandate
// amendment request for one registered mandate: the OrgnlMndt
// (mandate ID + creditor scheme) plus the AmdmntInf (the amended
// items that actually changed + the effective date).
//
// Fail-closed: a missing creditor scheme ID, an amendment that
// changes nothing, a missing effective date, a negative cap, an
// invalid IBAN, a frequency outside the mandate vocabulary, an
// unknown mandate, and a mandate that is not effectively active
// (suspended/cancelled/expired) are all refused.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, consumes no idempotency key, and mutates
// no mandate row. Like the pain.009 export, it still emits one
// structured audit event ("pain010_exported", details msg_id/
// mandate_id/creditor_scheme_id/effective_date/amended_fields) so
// the compliance trail records every generated amendment request.
func (l *Ledger) ExportPain010(mandateID, creditorSchemeID string, amd MandateAmendment) ([]byte, error) {
	if strings.TrimSpace(creditorSchemeID) == "" {
		return nil, ErrMissingPain010CreditorScheme
	}
	if err := validateMandateAmendment(amd); err != nil {
		return nil, err
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

	msgID := pain010MsgID(m.ID, amd.EffectiveDate)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.010.001.02">` + "\n")
	b.WriteString("<MndtAmdmntReq>\n")
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
	b.WriteString("<AmdmntInf>\n")
	if amd.CreditorIBAN != "" {
		b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n",
			ofxEscape(normalizeIBAN(amd.CreditorIBAN))))
	}
	if amd.DebtorIBAN != "" {
		b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n",
			ofxEscape(normalizeIBAN(amd.DebtorIBAN))))
	}
	if amd.MaxAmountCents != 0 {
		b.WriteString(fmt.Sprintf("<MaxAmt Ccy=\"%s\">%s</MaxAmt>\n",
			m.Currency, pain001Amount(amd.MaxAmountCents)))
	}
	if amd.Frequency != "" {
		b.WriteString(fmt.Sprintf("<Frqcy>%s</Frqcy>\n", ofxEscape(amd.Frequency)))
	}
	b.WriteString(fmt.Sprintf("<FrDt>%s</FrDt>\n", amd.EffectiveDate.UTC().Format("2006-01-02")))
	b.WriteString("</AmdmntInf>\n")
	b.WriteString("</Mndt>\n")
	b.WriteString("</MndtAmdmntReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like the pain.009 export: the version brackets the
	// export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain010_exported",
		Actor:         "ExportPain010",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"msg_id":             msgID,
			"mandate_id":         mandateID,
			"creditor_scheme_id": creditorSchemeID,
			"effective_date":     amd.EffectiveDate.UTC().Format("2006-01-02"),
			"amended_fields":     pain010AmendedFields(amd),
		},
	})
	l.mu.RUnlock()
	return []byte(b.String()), nil
}
