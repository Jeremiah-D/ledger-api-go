package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pain.011.001.03 Mandate Cancellation Request inbound
// processing (LG-426): the inbound half of the pain.011 lifecycle,
// the mirror of the LG-402 export.
//
// ExportPain011 renders this ledger's own cancellation request —
// the creditor telling the debtor's bank "cancel this mandate".
// This module consumes the cancellation request a counterparty
// creditor sent to this ledger, acting as the debtor's bank: it
// cancels the mandate the request names, and nothing else. The
// two directions are orthogonal by construction:
//
//   - LG-402 exports: the ledger asks the bank to cancel one of
//     its own mandates (outbound, read-only).
//   - LG-426 imports: a counterparty creditor asks the ledger to
//     cancel a mandate (inbound, state-changing only in the
//     mandate registry: the mandate moves to cancelled, keeping
//     its ID).
//
// ParsePain011 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// The parse structs mirror the export's rendered fields exactly
// (OrgnlMndt/MndtId + CdtrSchmeId/Id/PrtryId/Id, optional
// CxlRsn/Rsn/Prtry), so a document this ledger's export renders
// parses back; no sub-element spelling is invented beyond what
// pain011.go renders. The namespace rides the Document element,
// so a document in the wrong schema version fails the unmarshal
// — the version gate is the namespace, not a string check (the
// camt.056 inbound convention, LG-414).
//
// The cancellation routes through the existing CancelMandate
// entry, never a direct mandate write: the entry owns its gates
// (unknown mandate, expired mandate, non-active mandate) and its
// own audit event ("mandate_cancelled") plus the outbox event,
// so the trail shows the cancellation exactly as an
// operator-driven one would. ApplyPain011 additionally emits
// `pain011_cancellation_applied` (details msg_id / mandate_id /
// creditor_scheme_id / reason_code) so the trail tells an inbound
// cancellation apart from a local one. The reason rides verbatim
// from CxlRsn/Rsn/Prtry — never paraphrased, never translated,
// and no reason-code allowlist is invented; a request with no
// CxlRsn carries no reason_code detail at all.
//
// The mandate gate is ExportPain011's, unmodified: active and
// suspended mandates can both be cancelled — a suspended mandate
// still exists at the bank, so it still needs a revocation
// notice; a cancelled mandate refuses with ErrMandateNotActive
// and an expired one with ErrMandateExpired.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention, LG-416): re-applying the same MsgId with the same
// mandate, scheme, and reason returns the current mandate with
// Duplicate set, cancelling nothing and emitting no second audit
// event. A MsgId reused for different terms is refused with
// ErrPain011MsgIDConflict — never silently overwritten.
// Duplicate-audit avoidance lives in that MsgId registry: a
// re-sent cancellation under a new MsgId on an already-cancelled
// mandate is refused by CancelMandate's own gate.
//
// No mandate is ever auto-created, no pain.012 receipt is
// consumed here: the cancellation is executed first, and the
// bank's verdict on it arrives separately through LG-405.

// pain.011 inbound errors.
var (
	// ErrInvalidPain011 is returned by ParsePain011 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain011 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a mandate cancellation request. The HTTP layer
	// maps it to 400.
	ErrInvalidPain011 = errors.New("ledger: invalid pain.011 mandate cancellation request")
	// ErrPain011MsgIDConflict is returned by ApplyPain011 when a
	// GrpHdr MsgId already applied carries different terms: the
	// message ID names one cancellation, and a second, different
	// cancellation under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrPain011MsgIDConflict = errors.New("ledger: pain.011 inbound: message ID already applied with different terms")
)

// Pain011Request is one parsed inbound pain.011.001.03 mandate
// cancellation request: the request's identity, the mandate it
// cancels, the creditor scheme, and the reason as given.
type Pain011Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one cancellation.
	MsgID string `json:"msg_id"`
	// MndtID is OrgnlMndt/MndtId: the mandate to cancel. Never
	// auto-created.
	MndtID string `json:"mndt_id"`
	// CreditorSchemeID is OrgnlMndt/CdtrSchmeId (PrtryId/Id):
	// the SEPA creditor identifier, required, carried for the
	// audit trail.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// Reason is CxlRsn/Rsn/Prtry verbatim when the request
	// carries one, empty otherwise: the counterparty's free-form
	// reason, never paraphrased or translated.
	Reason string `json:"reason,omitempty"`
}

// Pain011ApplyResult is the outcome of ApplyPain011.
type Pain011ApplyResult struct {
	// Mandate is the cancelled mandate (or the current one on
	// the duplicate path).
	Mandate Mandate `json:"mandate"`
	// Duplicate reports whether the cancellation was already
	// applied under the same MsgId with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain011AppliedRecord is the replay record for one applied
// pain.011 cancellation, keyed by GrpHdr MsgId in pain011Applied:
// the mandate it cancelled, the scheme and reason it carried,
// and the mandate snapshot after application, so a replay
// returns the same outcome without cancelling twice.
type pain011AppliedRecord struct {
	MsgID            string
	MandateID        string
	CreditorSchemeID string
	Reason           string
	Mandate          Mandate
}

// pain011TermsEqual reports whether a stored cancellation record
// carries exactly the terms an inbound request would apply: the
// same mandate, the same creditor scheme, and the same reason,
// verbatim.
func pain011TermsEqual(rec pain011AppliedRecord, req Pain011Request) bool {
	return rec.MandateID == req.MndtID &&
		rec.CreditorSchemeID == req.CreditorSchemeID &&
		rec.Reason == req.Reason
}

// The pain011x* structs are the inbound-side XML model for
// pain.011.001.03: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (OrgnlMndt/MndtId + CdtrSchmeId/Id/PrtryId/Id, optional
// CxlRsn/Rsn/Prtry) so a document this ledger's export renders
// parses back. The namespace on Document gates the schema
// version.
type pain011xDocument struct {
	XMLName    xml.Name           `xml:"urn:iso:std:iso:20022:tech:xsd:pain.011.001.03 Document"`
	MndtCxlReq pain011xMndtCxlReq `xml:"MndtCxlReq"`
}

type pain011xMndtCxlReq struct {
	GrpHdr pain011xGrpHdr `xml:"GrpHdr"`
	Mndt   pain011xMndt   `xml:"Mndt"`
}

type pain011xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain011xMndt struct {
	OrgnlMndt pain011xOrgnlMndt `xml:"OrgnlMndt"`
	CxlRsn    *pain011xCxlRsn   `xml:"CxlRsn,omitempty"`
}

type pain011xOrgnlMndt struct {
	MndtID      string              `xml:"MndtId"`
	CdtrSchmeId pain011xCdtrSchmeId `xml:"CdtrSchmeId"`
}

type pain011xCdtrSchmeId struct {
	ID pain011xSchemeID `xml:"Id"`
}

type pain011xSchemeID struct {
	PrtryID *pain011xPrtryID `xml:"PrtryId,omitempty"`
}

type pain011xPrtryID struct {
	ID string `xml:"Id"`
}

type pain011xCxlRsn struct {
	Rsn pain011xRsn `xml:"Rsn"`
}

type pain011xRsn struct {
	Prtry string `xml:"Prtry"`
}

// validatePain011Request fail-closes on a cancellation request
// ParsePain011 would refuse, so ApplyPain011 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy. The reason is free-form and
// optional — no allowlist is invented for it.
func validatePain011Request(req Pain011Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain011)
	}
	if strings.TrimSpace(req.MndtID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain011)
	}
	if strings.TrimSpace(req.CreditorSchemeID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain011)
	}
	return nil
}

// ParsePain011 parses one inbound pain.011.001.03 mandate
// cancellation request into a Pain011Request. It validates the
// whole document before returning anything: a missing MsgId, a
// missing OrgnlMndt MndtId or CdtrSchmeId is a parse refusal —
// ApplyPain011 never sees a half-read request. The reason, when
// present, is carried verbatim; its absence is not a refusal,
// the ExportPain011 rule (an empty reason omits the block).
func ParsePain011(data []byte) (Pain011Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain011Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain011)
	}
	var doc pain011xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain011Request{}, fmt.Errorf("%w: %v", ErrInvalidPain011, err)
	}
	req := doc.MndtCxlReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain011Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain011)
	}
	mndtID := strings.TrimSpace(req.Mndt.OrgnlMndt.MndtID)
	if mndtID == "" {
		return Pain011Request{}, fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain011)
	}
	schemeID := ""
	if req.Mndt.OrgnlMndt.CdtrSchmeId.ID.PrtryID != nil {
		schemeID = strings.TrimSpace(req.Mndt.OrgnlMndt.CdtrSchmeId.ID.PrtryID.ID)
	}
	if schemeID == "" {
		return Pain011Request{}, fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain011)
	}
	reason := ""
	if req.Mndt.CxlRsn != nil {
		reason = strings.TrimSpace(req.Mndt.CxlRsn.Rsn.Prtry)
	}
	return Pain011Request{
		MsgID:            msgID,
		MndtID:           mndtID,
		CreditorSchemeID: schemeID,
		Reason:           reason,
	}, nil
}

// ApplyPain011 applies one parsed inbound pain.011.001.03
// mandate cancellation request: it cancels the named mandate
// through the existing CancelMandate entry.
//
// The mandate must exist and be cancellable (the ExportPain011
// gate: unknown is ErrMandateNotFound, expired is
// ErrMandateExpired, other non-active states are
// ErrMandateNotActive). Active and suspended mandates can both
// be cancelled — a suspended mandate still exists at the bank.
// No mandate is ever auto-created.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention): a replay with the same mandate, scheme, and
// reason returns the current mandate with Duplicate set,
// cancelling nothing and emitting no second audit event; a MsgId
// reused for different terms is refused with
// ErrPain011MsgIDConflict. Duplicate-audit avoidance lives in
// that MsgId registry, not in CancelMandate: a re-sent
// cancellation under a new MsgId on an already-cancelled mandate
// is refused by CancelMandate's own gate.
//
// Locking: the validation runs on the parsed values alone; the
// replay read runs under the read lock; CancelMandate takes the
// write lock itself; the MsgId record and the inbound audit
// event run under a short write lock of their own.
func (l *Ledger) ApplyPain011(req Pain011Request, actor string) (Pain011ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain011"
	}
	if err := validatePain011Request(req); err != nil {
		return Pain011ApplyResult{}, err
	}

	// MsgId replay: the cancellation was already applied under
	// this MsgId. Same terms is a no-op returning the current
	// mandate; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.pain011Applied[req.MsgID]; ok {
		l.mu.RUnlock()
		if !pain011TermsEqual(rec, req) {
			return Pain011ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain011MsgIDConflict, req.MsgID)
		}
		current, ok := l.GetMandate(req.MndtID)
		if !ok {
			return Pain011ApplyResult{Mandate: rec.Mandate, Duplicate: true}, nil
		}
		return Pain011ApplyResult{Mandate: current, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The cancellation itself, through the existing entry: it
	// owns the unknown / expired / non-active gates and the
	// mandate_cancelled audit event plus the outbox event.
	cancelled, err := l.CancelMandate(req.MndtID)
	if err != nil {
		return Pain011ApplyResult{}, err
	}

	// The MsgId record and the inbound direction marker, under
	// one short write lock: the cancellation is already durable
	// when the marker lands. The reason_code detail is present
	// only when the request carried a reason — no reason, no
	// field, never a defaulted code.
	details := map[string]any{
		"msg_id":             req.MsgID,
		"mandate_id":         req.MndtID,
		"creditor_scheme_id": req.CreditorSchemeID,
	}
	if req.Reason != "" {
		details["reason_code"] = req.Reason
	}
	l.mu.Lock()
	if l.pain011Applied == nil {
		l.pain011Applied = make(map[string]pain011AppliedRecord)
	}
	l.pain011Applied[req.MsgID] = pain011AppliedRecord{
		MsgID:            req.MsgID,
		MandateID:        req.MndtID,
		CreditorSchemeID: req.CreditorSchemeID,
		Reason:           req.Reason,
		Mandate:          cancelled,
	}
	l.emitAudit(AuditEvent{
		Op:            "pain011_cancellation_applied",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{cancelled.Payer, cancelled.Payee},
		Details:       details,
	})
	l.mu.Unlock()
	return Pain011ApplyResult{Mandate: cancelled}, nil
}
