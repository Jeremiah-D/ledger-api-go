package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pain.018.001.03 Mandate Suspension Request inbound
// processing (LG-436): the inbound half of the pain.018 lifecycle,
// the mirror of the LG-432 export.
//
// ExportPain018 renders this ledger's own suspension request —
// the creditor telling the debtor's bank "suspend this mandate".
// This module consumes the suspension request a counterparty
// creditor sent to this ledger, acting as the debtor's bank: it
// suspends the mandate the request names, and nothing else. The
// two directions are orthogonal by construction:
//
//   - LG-432 exports: the ledger asks the bank to suspend one of
//     its own mandates (outbound, read-only).
//   - LG-436 imports: a counterparty creditor asks the ledger to
//     suspend a mandate (inbound, state-changing only in the
//     mandate registry: the mandate moves to suspended, keeping
//     its ID).
//
// ParsePain018 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// The parse structs mirror the export's rendered fields exactly
// (GrpHdr MsgId/CreDtTm, SspnsnReqId, OrgnlMndt/MndtId +
// CdtrSchmeId/Id/PrtryId/Id, SspnsnRsn/Rsn/Prtry), so a document
// this ledger's export renders parses back; no sub-element
// spelling is invented beyond what pain018.go renders. The
// namespace rides the Document element, so a document in the
// wrong schema version fails the unmarshal — the version gate is
// the namespace, not a string check (the camt.056 inbound
// convention, LG-414).
//
// The suspension routes through the existing SuspendMandate
// entry, never a direct mandate write: the entry owns its gates
// (unknown mandate, expired mandate, non-active mandate) and its
// own audit event ("mandate_suspended") plus the outbox event,
// so the trail shows the suspension exactly as an
// operator-driven one would. ApplyPain018 additionally emits
// `pain018_suspension_applied` (details msg_id / mandate_id /
// creditor_scheme_id / reason_code) so the trail tells an inbound
// suspension apart from a local one. The reason rides verbatim
// from SspnsnRsn/Rsn/Prtry — never paraphrased, never translated,
// and no reason-code allowlist is invented.
//
// The mandate gate is ExportPain018's, unmodified: only active
// mandates can be suspended — a suspended or cancelled mandate
// refuses with ErrMandateNotActive and an expired one with
// ErrMandateExpired. No new mandate-gate error is introduced:
// the three existing sentinels cover every refusal.
//
// The creditor scheme ID is recorded in the audit details only
// and never validated against a stored value: the ledger stores
// no creditor scheme ID, so there is nothing to check it
// against.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention, LG-416): re-applying the same MsgId with the same
// mandate, scheme, and reason returns the current mandate with
// Duplicate set, suspending nothing and emitting no second audit
// event. A MsgId reused for different terms is refused with
// ErrPain018MsgIDConflict — never silently overwritten.
// Duplicate-audit avoidance lives in that MsgId registry: a
// re-sent suspension under a new MsgId on an already-suspended
// mandate is refused by SuspendMandate's own gate.
//
// No mandate is ever auto-created, no pain.012 receipt is
// consumed here: the suspension is executed first, and the
// bank's verdict on it arrives separately through LG-405. The
// full V03 UndrlygSspnsnDtls shape is not adopted here, per the
// LG-432 simplification precedent.

// pain.018 inbound errors.
var (
	// ErrInvalidPain018 is returned by ParsePain018 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain018 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a mandate suspension request. The HTTP layer
	// maps it to 400.
	ErrInvalidPain018 = errors.New("ledger: invalid pain.018 mandate suspension request")
	// ErrPain018MsgIDConflict is returned by ApplyPain018 when a
	// GrpHdr MsgId already applied carries different terms: the
	// message ID names one suspension, and a second, different
	// suspension under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrPain018MsgIDConflict = errors.New("ledger: pain.018 inbound: message ID already applied with different terms")
)

// Pain018Request is one parsed inbound pain.018.001.03 mandate
// suspension request: the request's identity, the mandate it
// suspends, the creditor scheme, and the reason as given.
type Pain018Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one suspension.
	MsgID string `json:"msg_id"`
	// MndtID is OrgnlMndt/MndtId: the mandate to suspend. Never
	// auto-created.
	MndtID string `json:"mndt_id"`
	// CreditorSchemeID is OrgnlMndt/CdtrSchmeId (PrtryId/Id):
	// the SEPA creditor identifier, required, carried for the
	// audit trail and never validated against a stored value.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// Reason is SspnsnRsn/Rsn/Prtry verbatim: the counterparty's
	// free-form reason, never paraphrased or translated. It is
	// required — SspnsnRsn is [1..1] in the full V03 definition,
	// the outbound ExportPain018 gate.
	Reason string `json:"reason"`
}

// Pain018ApplyResult is the outcome of ApplyPain018.
type Pain018ApplyResult struct {
	// Mandate is the suspended mandate (or the current one on
	// the duplicate path).
	Mandate Mandate `json:"mandate"`
	// Duplicate reports whether the suspension was already
	// applied under the same MsgId with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain018AppliedRecord is the replay record for one applied
// pain.018 suspension, keyed by GrpHdr MsgId in pain018Applied:
// the mandate it suspended, the scheme and reason it carried,
// and the mandate snapshot after application, so a replay
// returns the same outcome without suspending twice.
type pain018AppliedRecord struct {
	MsgID            string
	MandateID        string
	CreditorSchemeID string
	Reason           string
	Mandate          Mandate
}

// pain018TermsEqual reports whether a stored suspension record
// carries exactly the terms an inbound request would apply: the
// same mandate, the same creditor scheme, and the same reason,
// verbatim.
func pain018TermsEqual(rec pain018AppliedRecord, req Pain018Request) bool {
	return rec.MandateID == req.MndtID &&
		rec.CreditorSchemeID == req.CreditorSchemeID &&
		rec.Reason == req.Reason
}

// The pain018x* structs are the inbound-side XML model for
// pain.018.001.03: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (GrpHdr MsgId/CreDtTm, SspnsnReqId, OrgnlMndt/MndtId +
// CdtrSchmeId/Id/PrtryId/Id, SspnsnRsn/Rsn/Prtry) so a document
// this ledger's export renders parses back. The namespace on
// Document gates the schema version.
type pain018xDocument struct {
	XMLName       xml.Name              `xml:"urn:iso:std:iso:20022:tech:xsd:pain.018.001.03 Document"`
	MndtSspnsnReq pain018xMndtSspnsnReq `xml:"MndtSspnsnReq"`
}

type pain018xMndtSspnsnReq struct {
	GrpHdr      pain018xGrpHdr     `xml:"GrpHdr"`
	SspnsnReqId string             `xml:"SspnsnReqId"`
	OrgnlMndt   pain018xOrgnlMndt  `xml:"OrgnlMndt"`
	SspnsnRsn   *pain018xSspnsnRsn `xml:"SspnsnRsn,omitempty"`
}

type pain018xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain018xOrgnlMndt struct {
	MndtID      string              `xml:"MndtId"`
	CdtrSchmeId pain018xCdtrSchmeId `xml:"CdtrSchmeId"`
}

type pain018xCdtrSchmeId struct {
	ID pain018xSchemeID `xml:"Id"`
}

type pain018xSchemeID struct {
	PrtryID *pain018xPrtryID `xml:"PrtryId,omitempty"`
}

type pain018xPrtryID struct {
	ID string `xml:"Id"`
}

type pain018xSspnsnRsn struct {
	Rsn pain018xRsn `xml:"Rsn"`
}

type pain018xRsn struct {
	Prtry string `xml:"Prtry"`
}

// validatePain018Request fail-closes on a suspension request
// ParsePain018 would refuse, so ApplyPain018 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy. The reason is required here —
// unlike pain.011's optional CxlRsn — because SspnsnRsn is
// [1..1] in the full V03 definition and ExportPain018 refuses a
// reasonless request.
func validatePain018Request(req Pain018Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain018)
	}
	if strings.TrimSpace(req.MndtID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain018)
	}
	if strings.TrimSpace(req.CreditorSchemeID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain018)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: missing SspnsnRsn/Rsn/Prtry", ErrInvalidPain018)
	}
	return nil
}

// ParsePain018 parses one inbound pain.018.001.03 mandate
// suspension request into a Pain018Request. It validates the
// whole document before returning anything: a missing MsgId, a
// missing OrgnlMndt MndtId or CdtrSchmeId, or a missing
// SspnsnRsn reason is a parse refusal — ApplyPain018 never sees
// a half-read request. The reason, when present, is carried
// verbatim; its absence is a refusal, the ExportPain018 rule
// (SspnsnRsn [1..1], never a defaulted code and never a silently
// omitted block).
func ParsePain018(data []byte) (Pain018Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain018Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain018)
	}
	var doc pain018xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain018Request{}, fmt.Errorf("%w: %v", ErrInvalidPain018, err)
	}
	req := doc.MndtSspnsnReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain018Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain018)
	}
	mndtID := strings.TrimSpace(req.OrgnlMndt.MndtID)
	if mndtID == "" {
		return Pain018Request{}, fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain018)
	}
	schemeID := ""
	if req.OrgnlMndt.CdtrSchmeId.ID.PrtryID != nil {
		schemeID = strings.TrimSpace(req.OrgnlMndt.CdtrSchmeId.ID.PrtryID.ID)
	}
	if schemeID == "" {
		return Pain018Request{}, fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain018)
	}
	reason := ""
	if req.SspnsnRsn != nil {
		reason = strings.TrimSpace(req.SspnsnRsn.Rsn.Prtry)
	}
	if reason == "" {
		return Pain018Request{}, fmt.Errorf("%w: missing SspnsnRsn/Rsn/Prtry", ErrInvalidPain018)
	}
	return Pain018Request{
		MsgID:            msgID,
		MndtID:           mndtID,
		CreditorSchemeID: schemeID,
		Reason:           reason,
	}, nil
}

// ApplyPain018 applies one parsed inbound pain.018.001.03
// mandate suspension request: it suspends the named mandate
// through the existing SuspendMandate entry.
//
// The mandate must exist and be active (the ExportPain018
// gate: unknown is ErrMandateNotFound, expired is
// ErrMandateExpired, other non-active states are
// ErrMandateNotActive). No new mandate-gate error is
// introduced. No mandate is ever auto-created, and the creditor
// scheme ID is recorded in the audit details only — never
// validated against a stored value, because the ledger stores
// no creditor scheme ID.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention): a replay with the same mandate, scheme, and
// reason returns the current mandate with Duplicate set,
// suspending nothing and emitting no second audit event; a MsgId
// reused for different terms is refused with
// ErrPain018MsgIDConflict. Duplicate-audit avoidance lives in
// that MsgId registry, not in SuspendMandate: a re-sent
// suspension under a new MsgId on an already-suspended mandate
// is refused by SuspendMandate's own gate.
//
// Locking: the validation runs on the parsed values alone; the
// replay read runs under the read lock; SuspendMandate takes the
// write lock itself; the MsgId record and the inbound audit
// event run under a short write lock of their own.
func (l *Ledger) ApplyPain018(req Pain018Request, actor string) (Pain018ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain018"
	}
	if err := validatePain018Request(req); err != nil {
		return Pain018ApplyResult{}, err
	}

	// MsgId replay: the suspension was already applied under
	// this MsgId. Same terms is a no-op returning the current
	// mandate; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.pain018Applied[req.MsgID]; ok {
		l.mu.RUnlock()
		if !pain018TermsEqual(rec, req) {
			return Pain018ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain018MsgIDConflict, req.MsgID)
		}
		current, ok := l.GetMandate(req.MndtID)
		if !ok {
			return Pain018ApplyResult{Mandate: rec.Mandate, Duplicate: true}, nil
		}
		return Pain018ApplyResult{Mandate: current, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The suspension itself, through the existing entry: it
	// owns the unknown / expired / non-active gates and the
	// mandate_suspended audit event plus the outbox event.
	suspended, err := l.SuspendMandate(req.MndtID)
	if err != nil {
		return Pain018ApplyResult{}, err
	}

	// The MsgId record and the inbound direction marker, under
	// one short write lock: the suspension is already durable
	// when the marker lands. The reason is always present — a
	// reasonless request never reaches this point — so the
	// reason_code detail is unconditional.
	details := map[string]any{
		"msg_id":             req.MsgID,
		"mandate_id":         req.MndtID,
		"creditor_scheme_id": req.CreditorSchemeID,
		"reason_code":        req.Reason,
	}
	l.mu.Lock()
	if l.pain018Applied == nil {
		l.pain018Applied = make(map[string]pain018AppliedRecord)
	}
	l.pain018Applied[req.MsgID] = pain018AppliedRecord{
		MsgID:            req.MsgID,
		MandateID:        req.MndtID,
		CreditorSchemeID: req.CreditorSchemeID,
		Reason:           req.Reason,
		Mandate:          suspended,
	}
	l.emitAudit(AuditEvent{
		Op:            "pain018_suspension_applied",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{suspended.Payer, suspended.Payee},
		Details:       details,
	})
	l.mu.Unlock()
	return Pain018ApplyResult{Mandate: suspended}, nil
}
