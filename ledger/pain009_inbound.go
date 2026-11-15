package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.009.001.02 Mandate Initiation Request inbound
// processing (LG-422): the inbound half of the pain.009 lifecycle,
// the mirror of the LG-199 export.
//
// ExportPain009 renders this ledger's own initiation request — the
// creditor telling the debtor's bank "recognize this mandate". This
// module consumes the initiation request a counterparty creditor
// sent to this ledger, acting as the debtor's bank: it registers
// the mandate the request initiates, and nothing else. The two
// directions are orthogonal by construction:
//
//   - LG-199 exports: the ledger initiates one of its own mandates
//     as creditor (outbound, read-only).
//   - LG-422 imports: a counterparty creditor initiates a mandate
//     against this ledger's debtor (inbound, state-changing only
//     in the mandate registry: one mandate row per MndtId).
//
// ParsePain009 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the camt.057 inbound convention does not
// apply here). The namespace rides the Document element, so a
// document in the wrong schema version fails the unmarshal —
// the version gate is the namespace, not a string check (the
// camt.056 inbound convention, LG-414).
//
// Association is the IBAN, not a transfer: the Mndt/DbtrAcct and
// Mndt/CdtrAcct IBANs must each name an account the ledger
// already registered an IBAN for. An unknown IBAN is refused
// fail-closed and parked in the repair queue, never auto-created:
// the ledger never opens an account to receive a mandate.
//
// Amounts never ride a pain.009 (see pain009.go): the initiation
// request carries the authorization, not a collection — the
// per-debit cap and the frequency stay in the ledger, not on the
// wire. They are supplied by the debtor bank's operator as local
// policy at apply time (Pain009Policy: MaxAmountCents > 0,
// Frequency one of per-day/per-week/per-month, the
// validateMandateFields gates) and are never invented from the
// wire. The wire likewise carries no currency: the registered
// mandate is booked in the ledger's default currency, the
// normalizeCurrency empty-code rule.
//
// Registration reuses CreateMandate, so the mandate row, the
// `mandate_created` audit event, and the MandateCreated outbox
// envelope are exactly the ones an operator-side registration
// produces; ApplyPain009 additionally emits
// `pain009_mandate_registered` (details msg_id/mandate_id/
// creditor_scheme_id) so the trail tells an inbound registration
// apart from a local one.
//
// Idempotency is the mandate, not the message: re-applying the
// same MndtId with the same parties, window, cap, frequency, and
// currency returns the stored mandate with Duplicate set,
// registering nothing new and emitting no second audit event. A
// MndtId reused for different terms is refused with
// ErrMandateIDConflict (along CreateMandate :404) — never
// silently overwritten.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// unknown debtor/creditor IBAN parks in the repair queue for
// operator resolution, and the typed error carries the repair
// record ID. A document the ledger cannot even parse is parked
// the same way by ParkPain009Unparsed — visibility parking,
// never a silent drop of the counterparty's message. Repair
// records parked by this module are visibility parking, not
// resubmittable instructions: their Maker is deliberately empty,
// so the CreatePayout gate can never pass them.
//
// No collection, no pain.008: registering a mandate authorizes
// future pulls; it neither pulls nor orders one.

// pain.009 inbound errors.
var (
	// ErrInvalidPain009 is returned by ParsePain009 for malformed
	// XML or a document missing a required field: the counterparty
	// sent something the ledger cannot even read as a mandate
	// initiation request. The HTTP layer parks it in the repair
	// queue and maps it to 400.
	ErrInvalidPain009 = errors.New("ledger: invalid pain.009 mandate initiation request")
	// ErrPain009UnknownAccount is returned by ApplyPain009 when
	// the request's debtor or creditor IBAN names no account the
	// ledger registered an IBAN for: the ledger never opens an
	// account to receive a mandate. The request is parked in the
	// repair queue, not dropped.
	ErrPain009UnknownAccount = errors.New("ledger: pain.009 inbound: mandate account is unknown")
)

// Pain009Request is one parsed inbound pain.009.001.02 mandate
// initiation request: the request's identity, the mandate it
// initiates, the creditor scheme, both parties and their
// accounts, and the validity window.
type Pain009Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the repair
	// record key.
	MsgID string `json:"msg_id"`
	// MndtID is Mndt/MndtId: the mandate's identity, and the
	// replay key — one MndtId names one mandate.
	MndtID string `json:"mndt_id"`
	// CreditorSchemeID is Mndt/CdtrSchmeId (PrtryId/Id, or
	// OrgId/Othr/Id): the SEPA creditor identifier, required.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// DebtorIBAN is Mndt/DbtrAcct/Id/IBAN; CreditorIBAN is
	// Mndt/CdtrAcct/Id/IBAN. Both are required, exactly as
	// stated — resolution against the ledger happens in
	// ApplyPain009.
	DebtorIBAN   string `json:"debtor_iban"`
	CreditorIBAN string `json:"creditor_iban"`
	// DebtorName is Mndt/Dbtr/Nm and CreditorName Mndt/Cdtr/Nm,
	// carried verbatim when stated; neither is required — the
	// IBANs associate the mandate, not the names.
	DebtorName   string `json:"debtor_name,omitempty"`
	CreditorName string `json:"creditor_name,omitempty"`
	// DebtorAgentBIC is Mndt/DbtrAgt/FinInstnId/BIC when stated.
	DebtorAgentBIC string `json:"debtor_agent_bic,omitempty"`
	// ValidFrom is Mndt/FrDt (YYYY-MM-DD), zero when the
	// document omits it — effective since the beginning of
	// time, the mandate.go convention. ValidUntil is
	// Mndt/ToDt (YYYY-MM-DD), always present: an authorization
	// without an end is refused at creation.
	ValidFrom  time.Time `json:"valid_from"`
	ValidUntil time.Time `json:"valid_until"`
}

// Pain009Policy is the debtor bank operator's local policy for
// one inbound mandate registration: the per-debit cap and the
// rate cap. Both are operator-supplied at apply time — the wire
// carries neither, and they are never invented from it.
type Pain009Policy struct {
	// MaxAmountCents is the per-debit cap in minor units, > 0.
	MaxAmountCents int64 `json:"max_amount_cents"`
	// Frequency is one of per-day, per-week, per-month.
	Frequency string `json:"frequency"`
}

// Pain009ApplyResult is the outcome of ApplyPain009.
type Pain009ApplyResult struct {
	// Mandate is the registered mandate (or the stored one on
	// the duplicate path).
	Mandate Mandate `json:"mandate"`
	// RepairID is set on the parked fail-closed path (unknown
	// account): the repair-queue record holding the request for
	// operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the mandate was already
	// registered under the same MndtId with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// The pain009x* structs are the inbound-side XML model for
// pain.009.001.02: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (CdtrSchmeId/Id/PrtryId/Id, Cdtr/CdtrAcct, Dbtr/DbtrAcct,
// DbtrAgt/FinInstnId/BIC, FrDt/ToDt) so a document this
// ledger's export renders parses back. CdtrSchmeId also
// accepts the OrgId/Othr/Id shape SEPA creditors commonly
// use; the namespace on Document gates the schema version.
type pain009xDocument struct {
	XMLName      xml.Name             `xml:"urn:iso:std:iso:20022:tech:xsd:pain.009.001.02 Document"`
	MndtInitnReq pain009xMndtInitnReq `xml:"MndtInitnReq"`
}

type pain009xMndtInitnReq struct {
	GrpHdr pain009xGrpHdr `xml:"GrpHdr"`
	Mndt   pain009xMndt   `xml:"Mndt"`
}

type pain009xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain009xMndt struct {
	MndtID      string              `xml:"MndtId"`
	CdtrSchmeId pain009xCdtrSchmeId `xml:"CdtrSchmeId"`
	Cdtr        pain009xParty       `xml:"Cdtr"`
	CdtrAcct    pain009xAcct        `xml:"CdtrAcct"`
	Dbtr        pain009xParty       `xml:"Dbtr"`
	DbtrAcct    pain009xAcct        `xml:"DbtrAcct"`
	DbtrAgt     *pain009xAgt        `xml:"DbtrAgt,omitempty"`
	FrDt        string              `xml:"FrDt,omitempty"`
	ToDt        string              `xml:"ToDt"`
}

type pain009xCdtrSchmeId struct {
	ID pain009xSchemeID `xml:"Id"`
}

type pain009xSchemeID struct {
	PrtryID *pain009xPrtryID `xml:"PrtryId,omitempty"`
	OrgID   *pain009xOrgID   `xml:"OrgId,omitempty"`
}

type pain009xPrtryID struct {
	ID string `xml:"Id"`
}

type pain009xOrgID struct {
	Othr *pain009xOthr `xml:"Othr,omitempty"`
}

type pain009xOthr struct {
	ID string `xml:"Id"`
}

type pain009xParty struct {
	Nm string `xml:"Nm"`
}

type pain009xAcct struct {
	ID pain009xAcctID `xml:"Id"`
}

type pain009xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type pain009xAgt struct {
	FinInstnId pain009xFinInstnID `xml:"FinInstnId"`
}

type pain009xFinInstnID struct {
	BIC string `xml:"BIC"`
}

// pain009ParseDate parses an ISO 20022 date (YYYY-MM-DD) into
// midnight UTC.
func pain009ParseDate(body, field string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(body))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q must be YYYY-MM-DD", ErrInvalidPain009, field, body)
	}
	return d, nil
}

// ParsePain009 parses one inbound pain.009.001.02 mandate
// initiation request into a Pain009Request. It validates the
// whole document before returning anything: a missing MsgId, a
// missing MndtId, a missing CdtrSchmeId, a missing debtor or
// creditor IBAN, a missing ToDt, or a malformed FrDt/ToDt are
// all parse refusals — ApplyPain009 never sees a half-read
// request. The party names and the debtor agent BIC are carried
// verbatim when stated and never gated: the IBANs associate the
// mandate, not the names.
func ParsePain009(data []byte) (Pain009Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain009Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain009)
	}
	var doc pain009xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain009Request{}, fmt.Errorf("%w: %v", ErrInvalidPain009, err)
	}
	req := doc.MndtInitnReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain009)
	}
	mndt := req.Mndt
	mndtID := strings.TrimSpace(mndt.MndtID)
	if mndtID == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing Mndt/MndtId", ErrInvalidPain009)
	}
	schemeID := ""
	if mndt.CdtrSchmeId.ID.PrtryID != nil {
		schemeID = strings.TrimSpace(mndt.CdtrSchmeId.ID.PrtryID.ID)
	}
	if schemeID == "" && mndt.CdtrSchmeId.ID.OrgID != nil && mndt.CdtrSchmeId.ID.OrgID.Othr != nil {
		schemeID = strings.TrimSpace(mndt.CdtrSchmeId.ID.OrgID.Othr.ID)
	}
	if schemeID == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing Mndt/CdtrSchmeId", ErrInvalidPain009)
	}
	debtorIBAN := strings.TrimSpace(mndt.DbtrAcct.ID.IBAN)
	if debtorIBAN == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing Mndt/DbtrAcct/Id/IBAN", ErrInvalidPain009)
	}
	creditorIBAN := strings.TrimSpace(mndt.CdtrAcct.ID.IBAN)
	if creditorIBAN == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing Mndt/CdtrAcct/Id/IBAN", ErrInvalidPain009)
	}
	toDt := strings.TrimSpace(mndt.ToDt)
	if toDt == "" {
		return Pain009Request{}, fmt.Errorf("%w: missing Mndt/ToDt", ErrInvalidPain009)
	}
	validUntil, err := pain009ParseDate(toDt, "Mndt/ToDt")
	if err != nil {
		return Pain009Request{}, err
	}
	var validFrom time.Time
	if frDt := strings.TrimSpace(mndt.FrDt); frDt != "" {
		validFrom, err = pain009ParseDate(frDt, "Mndt/FrDt")
		if err != nil {
			return Pain009Request{}, err
		}
	}
	debtorBIC := ""
	if mndt.DbtrAgt != nil {
		debtorBIC = strings.TrimSpace(mndt.DbtrAgt.FinInstnId.BIC)
	}
	return Pain009Request{
		MsgID:            msgID,
		MndtID:           mndtID,
		CreditorSchemeID: schemeID,
		DebtorIBAN:       debtorIBAN,
		CreditorIBAN:     creditorIBAN,
		DebtorName:       strings.TrimSpace(mndt.Dbtr.Nm),
		CreditorName:     strings.TrimSpace(mndt.Cdtr.Nm),
		DebtorAgentBIC:   debtorBIC,
		ValidFrom:        validFrom,
		ValidUntil:       validUntil,
	}, nil
}

// resolvePain009IBANLocked resolves one stated IBAN against the
// ledger's registered-IBAN registry (normalized, the
// SetAccountIBAN canonical form). The account must already be
// registered — the ledger never opens an account to receive a
// mandate. Callers must hold l.mu (either lock suffices).
func (l *Ledger) resolvePain009IBANLocked(iban string) (AccountID, error) {
	norm := normalizeIBAN(iban)
	for acct, reg := range l.accountIBANs {
		if reg.IBAN == norm {
			return acct, nil
		}
	}
	return "", fmt.Errorf("%w: IBAN %q", ErrPain009UnknownAccount, iban)
}

// pain009TermsEqual reports whether an already-registered
// mandate carries exactly the terms an inbound request would
// register: the same parties, cap, currency, frequency, and
// validity window. The window comparison uses Equal — both
// sides are midnight-UTC dates parsed from YYYY-MM-DD.
func pain009TermsEqual(m Mandate, payer, payee AccountID, policy Pain009Policy, currency string, req Pain009Request) bool {
	return m.Payer == payer && m.Payee == payee &&
		m.MaxAmountCents == policy.MaxAmountCents &&
		m.Currency == currency && m.Frequency == policy.Frequency &&
		m.ValidFrom.Equal(req.ValidFrom) && m.ValidUntil.Equal(req.ValidUntil)
}

// pain009LenientDocument is the namespace-agnostic wire shape
// ParkPain009Unparsed reads: a document ParsePain009 refused
// for its version may still name the request, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type pain009LenientDocument struct {
	XMLName      xml.Name          `xml:"Document"`
	MndtInitnReq pain009LenientReq `xml:"MndtInitnReq"`
}

type pain009LenientReq struct {
	GrpHdr pain009LenientGrpHdr `xml:"GrpHdr"`
	Mndt   pain009LenientMndt   `xml:"Mndt"`
}

type pain009LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type pain009LenientMndt struct {
	MndtID string `xml:"MndtId"`
}

// ParkPain009Unparsed best-effort parks a pain.009 document
// ParsePain009 refused, when the document still names a request:
// the GrpHdr MsgId or the MndtId it carries. It returns the
// repair record ID, or "" when the document names nothing
// parkable (garbage bytes, an empty shell): there is no request
// for the operator to find, so there is nothing to park. A
// parking failure (a full queue) is returned so the caller can
// fail loud rather than silently drop the counterparty's
// message. The reason code is invalid_format — the document was
// present but unreadable as a mandate initiation request.
func (l *Ledger) ParkPain009Unparsed(data []byte, actor string) (string, error) {
	var doc pain009LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.MndtInitnReq.GrpHdr.MsgID)
	if id == "" {
		id = strings.TrimSpace(doc.MndtInitnReq.Mndt.MndtID)
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "pain009-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain009Request parks a parsed pain.009 request that failed
// closed, and returns the repair record ID. The snapshot's Maker
// is deliberately empty (see the package comment).
func (l *Ledger) parkPain009Request(req Pain009Request, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: req.MsgID,
		// Maker intentionally empty: see the package comment.
		Destination: "pain009-inbound:" + req.MsgID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyPain009 registers one parsed inbound pain.009.001.02
// mandate initiation request as a mandate: the inbound half of
// the pain.009 flow, the counterparty-side mirror of the
// outbound export. The debtor and creditor IBANs must each name
// an account the ledger already registered an IBAN for —
// otherwise the request is refused fail-closed and parked in
// the repair queue, and no account is ever opened.
//
// The per-debit cap and the frequency come from the operator's
// local policy, never from the wire: the wire carries neither.
// The mandate is booked in the ledger's default currency — the
// wire carries no currency either. Registration itself reuses
// CreateMandate, so the mandate row, the `mandate_created`
// audit event, and the MandateCreated outbox envelope are
// exactly the ones an operator-side registration produces;
// ApplyPain009 additionally emits `pain009_mandate_registered`
// so the trail tells an inbound registration apart.
//
// Only the mandate registers: no collection, no pain.008, no
// journal rows, no version bump, no money moved.
//
// Idempotency is the MndtId with its terms: re-applying the
// same MndtId with the same parties, window, cap, frequency,
// and currency returns the stored mandate with Duplicate set,
// no second audit event. A MndtId reused for different terms
// is refused with ErrMandateIDConflict — never silently
// overwritten.
//
// Locking: the policy and window validation run on the parsed
// values alone; the IBAN resolution and the duplicate read run
// under the read lock; CreateMandate takes the write lock
// itself (a racing registration for the same MndtId converges
// on its ErrMandateIDConflict, re-read and compared); the
// pain009_mandate_registered audit runs under a short write
// lock of its own; the repair parking runs outside any held
// lock because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyPain009(req Pain009Request, policy Pain009Policy, actor string) (Pain009ApplyResult, error) {
	if strings.TrimSpace(req.MsgID) == "" {
		return Pain009ApplyResult{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain009)
	}
	if strings.TrimSpace(req.MndtID) == "" {
		return Pain009ApplyResult{}, fmt.Errorf("%w: missing Mndt/MndtId", ErrInvalidPain009)
	}
	if actor == "" {
		actor = "ApplyPain009"
	}
	// Policy and window validation, on the parsed values alone:
	// the operator's cap and frequency are validated by the
	// same gates CreateMandate applies, and a window without an
	// end — or an end at or before its start — cannot register.
	if policy.MaxAmountCents <= 0 {
		return Pain009ApplyResult{}, ErrMandateNonPositiveMaxAmount
	}
	if !validMandateFrequency(policy.Frequency) {
		return Pain009ApplyResult{}, fmt.Errorf("%w: %q", ErrInvalidMandateFrequency, policy.Frequency)
	}
	if req.ValidUntil.IsZero() || !req.ValidFrom.Before(req.ValidUntil) {
		return Pain009ApplyResult{}, ErrMandateInvalidWindow
	}
	// The wire carries no currency: the mandate is booked in the
	// ledger's default currency, the normalizeCurrency
	// empty-code rule.
	currency, err := normalizeCurrency("")
	if err != nil {
		return Pain009ApplyResult{}, err
	}

	// IBAN resolution and the duplicate read under the read lock.
	l.mu.RLock()
	payer, payerErr := l.resolvePain009IBANLocked(req.DebtorIBAN)
	payee, payeeErr := l.resolvePain009IBANLocked(req.CreditorIBAN)
	existing, exists := l.mandates[req.MndtID]
	l.mu.RUnlock()
	if payerErr != nil {
		// The parked fail-closed path, outside any held lock:
		// an unknown debtor IBAN goes to the repair queue.
		repairID, parkErr := l.parkPain009Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return Pain009ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", payerErr, parkErr)
		}
		return Pain009ApplyResult{RepairID: repairID}, payerErr
	}
	if payeeErr != nil {
		repairID, parkErr := l.parkPain009Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return Pain009ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", payeeErr, parkErr)
		}
		return Pain009ApplyResult{RepairID: repairID}, payeeErr
	}
	if payer == payee {
		return Pain009ApplyResult{}, ErrMandateSameParty
	}
	if exists {
		if pain009TermsEqual(existing, payer, payee, policy, currency, req) {
			return Pain009ApplyResult{Mandate: mandateWithEffectiveStatus(existing, time.Now()), Duplicate: true}, nil
		}
		return Pain009ApplyResult{}, fmt.Errorf("%w: mandate %q", ErrMandateIDConflict, req.MndtID)
	}

	// Registration reuses CreateMandate: the mandate row, the
	// mandate_created audit event, and the MandateCreated
	// outbox envelope are exactly the operator-side ones.
	candidate := Mandate{
		ID:             req.MndtID,
		Payer:          payer,
		Payee:          payee,
		MaxAmountCents: policy.MaxAmountCents,
		Currency:       currency,
		Frequency:      policy.Frequency,
		ValidFrom:      req.ValidFrom,
		ValidUntil:     req.ValidUntil,
	}
	if err := validateMandateFields(&candidate); err != nil {
		return Pain009ApplyResult{}, err
	}
	created, err := l.CreateMandate(candidate)
	if err != nil {
		if errors.Is(err, ErrMandateIDConflict) {
			// A racing registration for the same MndtId
			// converged here: same terms is a duplicate
			// no-op, different terms is the conflict.
			if m, ok := l.GetMandate(req.MndtID); ok && pain009TermsEqual(m, payer, payee, policy, currency, req) {
				return Pain009ApplyResult{Mandate: m, Duplicate: true}, nil
			}
			return Pain009ApplyResult{}, fmt.Errorf("%w: mandate %q", ErrMandateIDConflict, req.MndtID)
		}
		return Pain009ApplyResult{}, err
	}
	// The inbound registration's own audit event, under a short
	// write lock: the registration is structural (no version
	// bump), and this event is what tells it apart from a local
	// CreateMandate in the trail.
	l.mu.Lock()
	l.emitAudit(AuditEvent{
		Op:            "pain009_mandate_registered",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{payer, payee},
		Details: map[string]any{
			"msg_id":             req.MsgID,
			"mandate_id":         req.MndtID,
			"creditor_scheme_id": req.CreditorSchemeID,
		},
	})
	l.mu.Unlock()
	return Pain009ApplyResult{Mandate: created}, nil
}
