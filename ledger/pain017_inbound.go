package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pain.017.001.03 Mandate Copy Request inbound
// processing (LG-437): the inbound half of the pain.017 lifecycle,
// the mirror of the LG-433 export.
//
// ExportPain017 renders this ledger's own copy request — the
// creditor telling the debtor's bank "show me the copy of the
// mandate you hold". This module answers the copy request a
// counterparty creditor sent to this ledger, acting as the
// debtor's bank: it serves the copy of the mandate the request
// names, and nothing else. The two directions are orthogonal by
// construction:
//
//   - LG-433 exports: the ledger asks the bank for the copy of
//     one of its own mandates (outbound, read-only).
//   - LG-437 imports: a counterparty creditor asks the ledger
//     for the copy of a mandate (inbound, read-only too: the
//     mandate row is never touched).
//
// ParsePain017 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// The parse structs mirror the export's rendered fields exactly
// (GrpHdr MsgId/CreDtTm, UndrlygCpyReqDtls OrgnlMndt/MndtId +
// CdtrSchmeId/Id/PrtryId/Id, MndtSts/Cd), so a document this
// ledger's export renders parses back; no sub-element spelling
// is invented beyond what pain017.go renders. The namespace
// rides the Document element, so a document in the wrong schema
// version fails the unmarshal — the version gate is the
// namespace, not a string check (the camt.056 inbound
// convention, LG-414).
//
// The copy is served through the existing GetMandate view, never
// a direct mandate read: the view already carries the effective
// status (an active mandate past its ValidUntil reads as
// expired), so the served copy is honest about the mandate's
// standing at serve time. ApplyPain017 additionally emits
// `pain017_copy_served` (details msg_id / mandate_id /
// creditor_scheme_id / status) so the trail tells an inbound
// copy service apart from a local read. There is deliberately
// no state gate, mirroring the export: active, suspended,
// cancelled, and expired mandates are all servable — a
// cancelled mandate's copy is precisely the evidence a dispute
// needs. The only mandate gate is existence: an unknown mandate
// is refused with ErrMandateNotFound.
//
// MndtSts, when the request carries it, must agree with the
// mandate's effective status (pain017MandateStatusCode): a
// requester holding a stale view of the mandate — asking for
// the copy of a mandate it believes cancelled when the ledger
// holds it active, or vice versa — is refused with
// ErrPain017StaleStatusView rather than served a copy under a
// disputed status. The requester re-reads the mandate, then
// asks again. An MndtSts code outside the four-code
// ExternalMandateStatus1Code set can never agree with an
// effective status, so it is refused the same way.
//
// The creditor scheme ID is recorded in the audit details only
// and never validated against a stored value: the ledger stores
// no creditor scheme ID, so there is nothing to check it
// against.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention, LG-416): re-applying the same MsgId with the same
// mandate, scheme, and status view returns the current mandate
// copy with Duplicate set, serving nothing twice and emitting
// no second audit event. A MsgId reused for different terms is
// refused with ErrPain017MsgIDConflict — never silently
// overwritten. The replay record carries the terms as
// requested, so a replay that restates the same status view is
// a no-op even if the mandate has since moved on: the copy was
// already served under that MsgId.
//
// No mandate is ever auto-created, no pain.012 receipt is
// consumed here, and the copy served is the ledger's own
// mandate row — never the bank-returned copy document the
// pain017.go header notes as a later round's item (the opposite
// direction: parsing a copy the bank sent, not answering a
// request for one).

// pain.017 inbound errors.
var (
	// ErrInvalidPain017 is returned by ParsePain017 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain017 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a mandate copy request. The HTTP layer maps
	// it to 400.
	ErrInvalidPain017 = errors.New("ledger: invalid pain.017 mandate copy request")
	// ErrPain017MsgIDConflict is returned by ApplyPain017 when a
	// GrpHdr MsgId already applied carries different terms: the
	// message ID names one copy request, and a second, different
	// request under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrPain017MsgIDConflict = errors.New("ledger: pain.017 inbound: message ID already applied with different terms")
	// ErrPain017StaleStatusView is returned by ApplyPain017 when
	// the request's MndtSts disagrees with the mandate's
	// effective status: the requester holds a stale view of the
	// mandate, and serving a copy under a disputed status would
	// be dishonest. It is a 422-class error.
	ErrPain017StaleStatusView = errors.New("ledger: pain.017 inbound: requester mandate status view disagrees with the ledger")
)

// Pain017Request is one parsed inbound pain.017.001.03 mandate
// copy request: the request's identity, the mandate whose copy
// is asked for, the creditor scheme, and the requester's view of
// the mandate's status, when given.
type Pain017Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one copy request.
	MsgID string `json:"msg_id"`
	// MndtID is OrgnlMndt/MndtId: the mandate whose copy is
	// served. Never auto-created.
	MndtID string `json:"mndt_id"`
	// CreditorSchemeID is OrgnlMndt/CdtrSchmeId (PrtryId/Id):
	// the SEPA creditor identifier, required, carried for the
	// audit trail and never validated against a stored value.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// StatusCode is UndrlygCpyReqDtls/MndtSts/Cd when present,
	// empty when the request carries no status view: the
	// requester's view of the mandate's status, checked against
	// the effective status at apply time.
	StatusCode string `json:"status_code,omitempty"`
}

// Pain017ApplyResult is the outcome of ApplyPain017.
type Pain017ApplyResult struct {
	// Mandate is the served copy: the GetMandate view, with the
	// effective status at serve time (or the current one on the
	// duplicate path).
	Mandate Mandate `json:"mandate"`
	// CreditorSchemeID echoes the request's creditor scheme ID,
	// the identity the copy was served under.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// Duplicate reports whether the copy was already served
	// under the same MsgId with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain017AppliedRecord is the replay record for one applied
// pain.017 copy request, keyed by GrpHdr MsgId in pain017Applied:
// the mandate it served, the scheme and status view it carried,
// and the mandate snapshot at serve time, so a replay returns
// the same outcome without serving twice.
type pain017AppliedRecord struct {
	MsgID            string
	MandateID        string
	CreditorSchemeID string
	StatusCode       string
	Mandate          Mandate
}

// pain017TermsEqual reports whether a stored copy record carries
// exactly the terms an inbound request would apply: the same
// mandate, the same creditor scheme, and the same status view
// (empty matches empty).
func pain017TermsEqual(rec pain017AppliedRecord, req Pain017Request) bool {
	return rec.MandateID == req.MndtID &&
		rec.CreditorSchemeID == req.CreditorSchemeID &&
		rec.StatusCode == req.StatusCode
}

// The pain017x* structs are the inbound-side XML model for
// pain.017.001.03: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (GrpHdr MsgId/CreDtTm, UndrlygCpyReqDtls OrgnlMndt/MndtId +
// CdtrSchmeId/Id/PrtryId/Id, MndtSts/Cd) so a document this
// ledger's export renders parses back. The namespace on Document
// gates the schema version.
type pain017xDocument struct {
	XMLName    xml.Name           `xml:"urn:iso:std:iso:20022:tech:xsd:pain.017.001.03 Document"`
	MndtCpyReq pain017xMndtCpyReq `xml:"MndtCpyReq"`
}

type pain017xMndtCpyReq struct {
	GrpHdr            pain017xGrpHdr     `xml:"GrpHdr"`
	UndrlygCpyReqDtls pain017xCpyReqDtls `xml:"UndrlygCpyReqDtls"`
}

type pain017xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain017xCpyReqDtls struct {
	OrgnlMndt pain017xOrgnlMndt `xml:"OrgnlMndt"`
	MndtSts   *pain017xMndtSts  `xml:"MndtSts,omitempty"`
}

type pain017xOrgnlMndt struct {
	MndtID      string              `xml:"MndtId"`
	CdtrSchmeId pain017xCdtrSchmeId `xml:"CdtrSchmeId"`
}

type pain017xCdtrSchmeId struct {
	ID pain017xSchemeID `xml:"Id"`
}

type pain017xSchemeID struct {
	PrtryID *pain017xPrtryID `xml:"PrtryId,omitempty"`
}

type pain017xPrtryID struct {
	ID string `xml:"Id"`
}

type pain017xMndtSts struct {
	Cd string `xml:"Cd"`
}

// validatePain017Request fail-closes on a copy request
// ParsePain017 would refuse, so ApplyPain017 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy. MndtSts is optional here —
// the requester need not state a status view at all — so no
// status check lives in this function; the view agreement is
// checked against the live mandate in ApplyPain017.
func validatePain017Request(req Pain017Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain017)
	}
	if strings.TrimSpace(req.MndtID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain017)
	}
	if strings.TrimSpace(req.CreditorSchemeID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain017)
	}
	return nil
}

// ParsePain017 parses one inbound pain.017.001.03 mandate copy
// request into a Pain017Request. It validates the whole document
// before returning anything: a missing MsgId, a missing
// OrgnlMndt MndtId or CdtrSchmeId is a parse refusal —
// ApplyPain017 never sees a half-read request. MndtSts is
// optional: when present its Cd is carried verbatim (agreement
// with the effective status is ApplyPain017's job, not the
// parser's — the parser has no mandate to check it against).
func ParsePain017(data []byte) (Pain017Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain017Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain017)
	}
	var doc pain017xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain017Request{}, fmt.Errorf("%w: %v", ErrInvalidPain017, err)
	}
	req := doc.MndtCpyReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain017Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain017)
	}
	mndtID := strings.TrimSpace(req.UndrlygCpyReqDtls.OrgnlMndt.MndtID)
	if mndtID == "" {
		return Pain017Request{}, fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain017)
	}
	schemeID := ""
	if req.UndrlygCpyReqDtls.OrgnlMndt.CdtrSchmeId.ID.PrtryID != nil {
		schemeID = strings.TrimSpace(req.UndrlygCpyReqDtls.OrgnlMndt.CdtrSchmeId.ID.PrtryID.ID)
	}
	if schemeID == "" {
		return Pain017Request{}, fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain017)
	}
	statusCode := ""
	if req.UndrlygCpyReqDtls.MndtSts != nil {
		statusCode = strings.TrimSpace(req.UndrlygCpyReqDtls.MndtSts.Cd)
	}
	return Pain017Request{
		MsgID:            msgID,
		MndtID:           mndtID,
		CreditorSchemeID: schemeID,
		StatusCode:       statusCode,
	}, nil
}

// ApplyPain017 applies one parsed inbound pain.017.001.03
// mandate copy request: it serves the copy of the named mandate
// through the existing GetMandate view.
//
// The mandate must exist (unknown is ErrMandateNotFound). There
// is deliberately no state gate: active, suspended, cancelled,
// and expired mandates are all servable, mirroring the export.
// When the request carries an MndtSts, it must agree with the
// mandate's effective status (pain017MandateStatusCode), or the
// request is refused with ErrPain017StaleStatusView.
//
// The copy is read-only: no mandate row moves, no version bump,
// no journal row. The only write is the MsgId replay record
// plus one `pain017_copy_served` audit event, so the compliance
// trail records every copy served.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention): a replay with the same mandate, scheme, and
// status view returns the current mandate copy with Duplicate
// set, serving nothing twice and emitting no second audit
// event; a MsgId reused for different terms is refused with
// ErrPain017MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the mandate read run under the read lock;
// the MsgId record and the inbound audit event run under a
// short write lock of their own.
func (l *Ledger) ApplyPain017(req Pain017Request, actor string) (Pain017ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain017"
	}
	if err := validatePain017Request(req); err != nil {
		return Pain017ApplyResult{}, err
	}

	// MsgId replay: the copy was already served under this
	// MsgId. Same terms is a no-op returning the current copy;
	// different terms is a conflict. The replay check runs
	// before the status-view check: the terms as originally
	// requested already passed it once.
	l.mu.RLock()
	if rec, ok := l.pain017Applied[req.MsgID]; ok {
		l.mu.RUnlock()
		if !pain017TermsEqual(rec, req) {
			return Pain017ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain017MsgIDConflict, req.MsgID)
		}
		current, ok := l.GetMandate(req.MndtID)
		if !ok {
			return Pain017ApplyResult{Mandate: rec.Mandate, CreditorSchemeID: req.CreditorSchemeID, Duplicate: true}, nil
		}
		return Pain017ApplyResult{Mandate: current, CreditorSchemeID: req.CreditorSchemeID, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The copy itself, through the existing view: it owns the
	// unknown-mandate gate and the effective-status reading.
	m, ok := l.GetMandate(req.MndtID)
	if !ok {
		return Pain017ApplyResult{}, fmt.Errorf("%w: %q", ErrMandateNotFound, req.MndtID)
	}
	if req.StatusCode != "" {
		if want := pain017MandateStatusCode(m.Status); req.StatusCode != want {
			return Pain017ApplyResult{}, fmt.Errorf("%w: requester holds %q, ledger holds %q",
				ErrPain017StaleStatusView, req.StatusCode, want)
		}
	}

	// The MsgId record and the inbound direction marker, under
	// one short write lock. The audit event brackets the
	// ledger version without moving it: serving a copy is
	// read-only, like the pain.017 export.
	details := map[string]any{
		"msg_id":             req.MsgID,
		"mandate_id":         req.MndtID,
		"creditor_scheme_id": req.CreditorSchemeID,
		"status":             m.Status,
	}
	l.mu.Lock()
	if l.pain017Applied == nil {
		l.pain017Applied = make(map[string]pain017AppliedRecord)
	}
	l.pain017Applied[req.MsgID] = pain017AppliedRecord{
		MsgID:            req.MsgID,
		MandateID:        req.MndtID,
		CreditorSchemeID: req.CreditorSchemeID,
		StatusCode:       req.StatusCode,
		Mandate:          m,
	}
	l.emitAudit(AuditEvent{
		Op:            "pain017_copy_served",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details:       details,
	})
	l.mu.Unlock()
	return Pain017ApplyResult{Mandate: m, CreditorSchemeID: req.CreditorSchemeID}, nil
}
