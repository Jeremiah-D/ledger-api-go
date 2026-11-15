package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pain.010.001.02 Mandate Amendment Request inbound
// processing (LG-425): the inbound half of the pain.010 lifecycle,
// the mirror of the LG-207 export.
//
// ExportPain010 renders this ledger's own amendment request — the
// creditor telling the debtor's bank "change this mandate". This
// module consumes the amendment request a counterparty creditor
// sent to this ledger, acting as the debtor's bank: it registers
// the amendment on the mandate the request names, and nothing
// else. The two directions are orthogonal by construction:
//
//   - LG-207 exports: the ledger asks the bank to amend one of
//     its own mandates (outbound, read-only).
//   - LG-425 imports: a counterparty creditor asks the ledger to
//     amend a mandate (inbound, state-changing only in the
//     mandate registry: the amended fields move in place, the
//     mandate keeps its ID).
//
// ParsePain010 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// The parse structs mirror the export's rendered fields exactly
// (OrgnlMndt/MndtId + CdtrSchmeId, AmdmntInf/CdtrAcct + DbtrAcct
// + MaxAmt + Frqcy + FrDt), so a document this ledger's export
// renders parses back; no sub-element spelling is invented
// beyond what pain010.go renders. The namespace rides the
// Document element, so a document in the wrong schema version
// fails the unmarshal — the version gate is the namespace, not a
// string check (the camt.056 inbound convention, LG-414).
//
// The amendment's fields, and what they route to:
//
//   - CdtrAcct / DbtrAcct (IBANs): the mandate's payee / payer.
//     Each IBAN must name an account the ledger already
//     registered an IBAN for — the ledger never opens an account
//     to receive an amendment — and the resolved accounts route
//     through AmendMandatePayee / AmendMandatePayer.
//   - MaxAmt: the new per-debit cap, through
//     AmendMandateMaxAmount. Its Ccy attribute rides along: a
//     currency that differs from the mandate's routes through
//     AmendMandateCurrency; an equal one is no change.
//   - Frqcy: the new collection frequency, through
//     AmendMandateFrequency.
//   - FrDt: the amendment's effective date — the takeover point,
//     required. It is not a mandate field: it does not move the
//     mandate's ValidFrom, and the wire carries no validity-window
//     change (the export renders none), so ApplyPain010 routes no
//     ValidFrom / ValidUntil amendment. A past effective date is
//     not refused — backdated amendments are a bank decision, the
//     ExportPain010 rule.
//
// Every field routes through the existing per-field amend
// entries, never a direct mandate write: each entry owns its own
// gates (unknown mandate, non-active mandate, same-party,
// invalid value) and its own audit event, so the trail shows
// each field move exactly as an operator-driven amendment would.
// ApplyPain010 additionally emits `pain010_amendment_applied`
// (details msg_id / mandate_id / changed_fields) so the trail
// tells an inbound amendment apart from a local one.
//
// The mandate gate is ExportPain010's, unmodified: the mandate
// must exist and be effectively active — unknown, suspended,
// cancelled, or expired mandates refuse the amendment, an
// amendment to a dead mandate having nothing to amend.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention, LG-416): re-applying the same MsgId with the same
// mandate and the same amendment terms returns the current
// mandate with Duplicate set, amending nothing and emitting no
// second audit event. A MsgId reused for different terms is
// refused with ErrPain010MsgIDConflict — never silently
// overwritten. Duplicate-audit avoidance lives in that MsgId
// registry: a re-sent amendment under a new MsgId runs the
// entries again, and the cap/frequency entries record the
// amendment even at an unchanged value (their established
// semantics — only the payer/payee/currency entries no-op on a
// same value), so only the same-MsgId replay is audit-free.
//
// A contradictory request — a payer and payee that would end up
// the same account — is refused before anything moves (the
// combined outcome is validated first, so no field can apply
// while a later one fails). No mandate is ever auto-created, no
// pain.012 receipt is consumed here: the amendment is
// registered first, and the bank's verdict on it arrives
// separately through LG-405.

// pain.010 inbound errors.
var (
	// ErrInvalidPain010 is returned by ParsePain010 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain010 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a mandate amendment request. The HTTP layer
	// maps it to 400.
	ErrInvalidPain010 = errors.New("ledger: invalid pain.010 mandate amendment request")
	// ErrPain010UnknownAccount is returned by ApplyPain010 when
	// the request's creditor or debtor IBAN names no account the
	// ledger registered an IBAN for: the ledger never opens an
	// account to receive an amendment. It is a 404-class error.
	ErrPain010UnknownAccount = errors.New("ledger: pain.010 inbound: mandate account is unknown")
	// ErrPain010MsgIDConflict is returned by ApplyPain010 when a
	// GrpHdr MsgId already applied carries different terms: the
	// message ID names one amendment, and a second, different
	// amendment under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrPain010MsgIDConflict = errors.New("ledger: pain.010 inbound: message ID already applied with different terms")
)

// Pain010Request is one parsed inbound pain.010.001.02 mandate
// amendment request: the request's identity, the mandate it
// amends, the creditor scheme, and the amendment it carries.
type Pain010Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one amendment.
	MsgID string `json:"msg_id"`
	// MndtID is OrgnlMndt/MndtId: the mandate to amend. Never
	// auto-created.
	MndtID string `json:"mndt_id"`
	// CreditorSchemeID is OrgnlMndt/CdtrSchmeId (PrtryId/Id, or
	// OrgId/Othr/Id): the SEPA creditor identifier, required,
	// carried for the audit trail.
	CreditorSchemeID string `json:"creditor_scheme_id"`
	// Amendment is the AmdmntInf content as a MandateAmendment:
	// the changed fields and the effective date.
	Amendment MandateAmendment `json:"amendment"`
	// Currency is AmdmntInf/MaxAmt's Ccy attribute when a MaxAmt
	// is stated, empty otherwise: the currency the new cap is
	// denominated in.
	Currency string `json:"currency,omitempty"`
}

// Pain010ApplyResult is the outcome of ApplyPain010.
type Pain010ApplyResult struct {
	// Mandate is the amended mandate (or the current one on the
	// duplicate path).
	Mandate Mandate `json:"mandate"`
	// Duplicate reports whether the amendment was already applied
	// under the same MsgId with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain010AppliedRecord is the replay record for one applied
// pain.010 amendment, keyed by GrpHdr MsgId in pain010Applied:
// the mandate it amended, the amendment terms it carried, and
// the mandate snapshot after application, so a replay returns
// the same outcome without re-amending.
type pain010AppliedRecord struct {
	MsgID            string
	MandateID        string
	CreditorSchemeID string
	Amendment        MandateAmendment
	Currency         string
	Mandate          Mandate
}

// pain010TermsEqual reports whether a stored amendment record
// carries exactly the terms an inbound request would apply: the
// same mandate, the same changed fields, and the same MaxAmt
// currency. The effective-date comparison uses Equal — both
// sides are midnight-UTC dates parsed from YYYY-MM-DD.
func pain010TermsEqual(rec pain010AppliedRecord, req Pain010Request) bool {
	return rec.MandateID == req.MndtID &&
		rec.Amendment == req.Amendment &&
		rec.Currency == req.Currency
}

// The pain010x* structs are the inbound-side XML model for
// pain.010.001.02: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (OrgnlMndt/MndtId + CdtrSchmeId/Id/PrtryId/Id, AmdmntInf/
// CdtrAcct + DbtrAcct + MaxAmt + Frqcy + FrDt) so a document
// this ledger's export renders parses back. CdtrSchmeId also
// accepts the OrgId/Othr/Id shape SEPA creditors commonly use;
// the namespace on Document gates the schema version.
type pain010xDocument struct {
	XMLName       xml.Name              `xml:"urn:iso:std:iso:20022:tech:xsd:pain.010.001.02 Document"`
	MndtAmdmntReq pain010xMndtAmdmntReq `xml:"MndtAmdmntReq"`
}

type pain010xMndtAmdmntReq struct {
	GrpHdr pain010xGrpHdr `xml:"GrpHdr"`
	Mndt   pain010xMndt   `xml:"Mndt"`
}

type pain010xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain010xMndt struct {
	OrgnlMndt pain010xOrgnlMndt `xml:"OrgnlMndt"`
	AmdmntInf pain010xAmdmntInf `xml:"AmdmntInf"`
}

type pain010xOrgnlMndt struct {
	MndtID      string              `xml:"MndtId"`
	CdtrSchmeId pain010xCdtrSchmeId `xml:"CdtrSchmeId"`
}

type pain010xCdtrSchmeId struct {
	ID pain010xSchemeID `xml:"Id"`
}

type pain010xSchemeID struct {
	PrtryID *pain010xPrtryID `xml:"PrtryId,omitempty"`
	OrgID   *pain010xOrgID   `xml:"OrgId,omitempty"`
}

type pain010xPrtryID struct {
	ID string `xml:"Id"`
}

type pain010xOrgID struct {
	Othr *pain010xOthr `xml:"Othr,omitempty"`
}

type pain010xOthr struct {
	ID string `xml:"Id"`
}

type pain010xAmdmntInf struct {
	CdtrAcct *pain010xAcct   `xml:"CdtrAcct,omitempty"`
	DbtrAcct *pain010xAcct   `xml:"DbtrAcct,omitempty"`
	MaxAmt   *pain010xMaxAmt `xml:"MaxAmt,omitempty"`
	Frqcy    string          `xml:"Frqcy,omitempty"`
	FrDt     string          `xml:"FrDt"`
}

type pain010xAcct struct {
	ID pain010xAcctID `xml:"Id"`
}

type pain010xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type pain010xMaxAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pain010ParseAmount parses an ISO 20022 decimal amount
// ("1234.56") into minor units, the pacs010ParseAmount shape:
// the integer part holds at most 12 digits, the fraction at most
// two digits and is right-padded ("5" means 50 cents), the value
// must be positive. Anything else is a parse refusal — a cap the
// ledger cannot read exactly is never rounded into an amendment.
func pain010ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidPain010)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidPain010, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain010, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidPain010, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain010, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain010, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrInvalidPain010, body)
	}
	return cents, nil
}

// pain010ParseDate parses an ISO 20022 date (YYYY-MM-DD) into
// midnight UTC.
func pain010ParseDate(body, field string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(body))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q must be YYYY-MM-DD", ErrInvalidPain010, field, body)
	}
	return d, nil
}

// validatePain010Request fail-closes on an amendment request
// ParsePain010 would refuse, so ApplyPain010 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy. The amendment itself is
// validated by validateMandateAmendment (no changed field, no
// effective date, negative cap, invalid IBAN, invalid
// frequency); the currency rides MaxAmt's Ccy attribute and is
// validated here.
func validatePain010Request(req Pain010Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain010)
	}
	if strings.TrimSpace(req.MndtID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain010)
	}
	if strings.TrimSpace(req.CreditorSchemeID) == "" {
		return fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain010)
	}
	if err := validateMandateAmendment(req.Amendment); err != nil {
		return err
	}
	if req.Currency != "" {
		if _, err := normalizeCurrency(req.Currency); err != nil {
			return fmt.Errorf("%w: currency %q", ErrInvalidPain010, req.Currency)
		}
	}
	return nil
}

// ParsePain010 parses one inbound pain.010.001.02 mandate
// amendment request into a Pain010Request. It validates the
// whole document before returning anything: a missing MsgId, a
// missing OrgnlMndt MndtId or CdtrSchmeId, a missing or
// malformed FrDt, no changed field, a negative cap, an invalid
// IBAN, an invalid frequency, or a MaxAmt without a currency are
// all parse refusals — ApplyPain010 never sees a half-read
// request. The MaxAmt currency is carried alongside the
// amendment; a past effective date is not refused, the
// ExportPain010 rule.
func ParsePain010(data []byte) (Pain010Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain010Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain010)
	}
	var doc pain010xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain010Request{}, fmt.Errorf("%w: %v", ErrInvalidPain010, err)
	}
	req := doc.MndtAmdmntReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain010Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain010)
	}
	mndtID := strings.TrimSpace(req.Mndt.OrgnlMndt.MndtID)
	if mndtID == "" {
		return Pain010Request{}, fmt.Errorf("%w: missing OrgnlMndt/MndtId", ErrInvalidPain010)
	}
	schemeID := ""
	if req.Mndt.OrgnlMndt.CdtrSchmeId.ID.PrtryID != nil {
		schemeID = strings.TrimSpace(req.Mndt.OrgnlMndt.CdtrSchmeId.ID.PrtryID.ID)
	}
	if schemeID == "" && req.Mndt.OrgnlMndt.CdtrSchmeId.ID.OrgID != nil && req.Mndt.OrgnlMndt.CdtrSchmeId.ID.OrgID.Othr != nil {
		schemeID = strings.TrimSpace(req.Mndt.OrgnlMndt.CdtrSchmeId.ID.OrgID.Othr.ID)
	}
	if schemeID == "" {
		return Pain010Request{}, fmt.Errorf("%w: missing OrgnlMndt/CdtrSchmeId", ErrInvalidPain010)
	}
	inf := req.Mndt.AmdmntInf
	frDt := strings.TrimSpace(inf.FrDt)
	if frDt == "" {
		return Pain010Request{}, fmt.Errorf("%w: missing AmdmntInf/FrDt", ErrPain010EffectiveDateRequired)
	}
	effectiveDate, err := pain010ParseDate(frDt, "AmdmntInf/FrDt")
	if err != nil {
		return Pain010Request{}, err
	}
	var creditorIBAN, debtorIBAN string
	if inf.CdtrAcct != nil {
		creditorIBAN = strings.TrimSpace(inf.CdtrAcct.ID.IBAN)
	}
	if inf.DbtrAcct != nil {
		debtorIBAN = strings.TrimSpace(inf.DbtrAcct.ID.IBAN)
	}
	var maxAmount int64
	var currency string
	if inf.MaxAmt != nil {
		maxAmount, err = pain010ParseAmount(inf.MaxAmt.Value)
		if err != nil {
			return Pain010Request{}, err
		}
		currency = strings.TrimSpace(inf.MaxAmt.Ccy)
		if currency == "" {
			return Pain010Request{}, fmt.Errorf("%w: empty AmdmntInf/MaxAmt currency", ErrInvalidPain010)
		}
		if _, err := normalizeCurrency(currency); err != nil {
			return Pain010Request{}, fmt.Errorf("%w: currency %q", ErrInvalidPain010, currency)
		}
	}
	amd := MandateAmendment{
		CreditorIBAN:   creditorIBAN,
		DebtorIBAN:     debtorIBAN,
		MaxAmountCents: maxAmount,
		Frequency:      strings.TrimSpace(inf.Frqcy),
		EffectiveDate:  effectiveDate,
	}
	if err := validateMandateAmendment(amd); err != nil {
		return Pain010Request{}, err
	}
	return Pain010Request{
		MsgID:            msgID,
		MndtID:           mndtID,
		CreditorSchemeID: schemeID,
		Amendment:        amd,
		Currency:         currency,
	}, nil
}

// resolvePain010IBANLocked resolves one stated IBAN against the
// ledger's registered-IBAN registry (normalized, the
// SetAccountIBAN canonical form). The account must already be
// registered — the ledger never opens an account to receive an
// amendment. Callers must hold l.mu (either lock suffices).
func (l *Ledger) resolvePain010IBANLocked(iban string) (AccountID, error) {
	norm := normalizeIBAN(iban)
	for acct, reg := range l.accountIBANs {
		if reg.IBAN == norm {
			return acct, nil
		}
	}
	return "", fmt.Errorf("%w: IBAN %q", ErrPain010UnknownAccount, iban)
}

// pain010ChangedFields lists the fields an applied amendment
// changed, in document order, for the audit event: the amended
// fields the request asked for, with the currency only when the
// stated MaxAmt currency differs from the mandate's — an equal
// currency is no change, not a restated one.
func pain010ChangedFields(req Pain010Request, before Mandate) []string {
	fields := pain010AmendedFields(req.Amendment)
	if req.Currency != "" {
		if normalized, err := normalizeCurrency(req.Currency); err == nil && normalized != before.Currency {
			// The currency rides MaxAmt, so it sorts with the
			// cap in document order.
			inserted := make([]string, 0, len(fields)+1)
			for _, f := range fields {
				inserted = append(inserted, f)
				if f == "max_amount_cents" {
					inserted = append(inserted, "currency")
				}
			}
			if len(inserted) == len(fields) {
				inserted = append(inserted, "currency")
			}
			fields = inserted
		}
	}
	return fields
}

// ApplyPain010 applies one parsed inbound pain.010.001.02
// mandate amendment request: it amends the named mandate in
// place, field by field through the existing amend entries.
//
// The mandate must exist and be effectively active (the
// ExportPain010 gate: unknown is ErrMandateNotFound, expired is
// ErrMandateExpired, other non-active states are
// ErrMandateNotActive). The creditor and debtor IBANs must each
// name an account the ledger already registered an IBAN for.
// The combined outcome is validated before anything moves, so a
// request whose payer and payee would end up the same account
// fails whole instead of applying one field and refusing the
// next.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention): a replay with the same mandate and the same
// amendment terms returns the current mandate with Duplicate
// set, amending nothing and emitting no second audit event; a
// MsgId reused for different terms is refused with
// ErrPain010MsgIDConflict. Duplicate-audit avoidance lives in
// that MsgId registry, not in the entries: a re-sent amendment
// under a new MsgId runs the entries again.
//
// Locking: the validation runs on the parsed values alone; the
// replay read, the mandate gate, and the IBAN resolutions run
// under the read lock; each amend entry takes the write lock
// itself; the MsgId record and the inbound audit event run under
// a short write lock of their own.
func (l *Ledger) ApplyPain010(req Pain010Request, actor string) (Pain010ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain010"
	}
	if err := validatePain010Request(req); err != nil {
		return Pain010ApplyResult{}, err
	}

	// MsgId replay: the amendment was already applied under
	// this MsgId. Same terms is a no-op returning the current
	// mandate; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.pain010Applied[req.MsgID]; ok {
		l.mu.RUnlock()
		if !pain010TermsEqual(rec, req) {
			return Pain010ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain010MsgIDConflict, req.MsgID)
		}
		current, ok := l.GetMandate(req.MndtID)
		if !ok {
			return Pain010ApplyResult{Mandate: rec.Mandate, Duplicate: true}, nil
		}
		return Pain010ApplyResult{Mandate: current, Duplicate: true}, nil
	}
	// The mandate gate (ExportPain010's) and the IBAN
	// resolutions, under the read lock: everything is resolved
	// before anything moves.
	m, ok := l.mandates[req.MndtID]
	l.mu.RUnlock()
	if !ok {
		return Pain010ApplyResult{}, fmt.Errorf("%w: %q", ErrMandateNotFound, req.MndtID)
	}
	now := time.Now()
	if eff := mandateEffectiveStatusLocked(m, now); eff != MandateStatusActive {
		if eff == MandateStatusExpired {
			return Pain010ApplyResult{}, fmt.Errorf("%w: mandate %q", ErrMandateExpired, req.MndtID)
		}
		return Pain010ApplyResult{}, fmt.Errorf("%w: mandate %q status %q", ErrMandateNotActive, req.MndtID, eff)
	}
	l.mu.RLock()
	var payee, payer AccountID
	var payeeErr, payerErr error
	if req.Amendment.CreditorIBAN != "" {
		payee, payeeErr = l.resolvePain010IBANLocked(req.Amendment.CreditorIBAN)
	}
	if req.Amendment.DebtorIBAN != "" {
		payer, payerErr = l.resolvePain010IBANLocked(req.Amendment.DebtorIBAN)
	}
	l.mu.RUnlock()
	if payeeErr != nil {
		return Pain010ApplyResult{}, payeeErr
	}
	if payerErr != nil {
		return Pain010ApplyResult{}, payerErr
	}
	// The combined outcome: the payer and the payee after this
	// amendment must still be two different accounts. Checking
	// the combination up front keeps a contradictory request
	// from applying one field and refusing the next.
	finalPayer, finalPayee := m.Payer, m.Payee
	if req.Amendment.DebtorIBAN != "" {
		finalPayer = payer
	}
	if req.Amendment.CreditorIBAN != "" {
		finalPayee = payee
	}
	if finalPayer == finalPayee {
		return Pain010ApplyResult{}, ErrMandateSameParty
	}

	// Field by field, in document order, through the existing
	// amend entries: each owns its gates and its audit event.
	amended := m
	var err error
	if req.Amendment.CreditorIBAN != "" {
		if amended, err = l.AmendMandatePayee(req.MndtID, payee); err != nil {
			return Pain010ApplyResult{}, err
		}
	}
	if req.Amendment.DebtorIBAN != "" {
		if amended, err = l.AmendMandatePayer(req.MndtID, payer); err != nil {
			return Pain010ApplyResult{}, err
		}
	}
	if req.Amendment.MaxAmountCents != 0 {
		if amended, err = l.AmendMandateMaxAmount(req.MndtID, req.Amendment.MaxAmountCents); err != nil {
			return Pain010ApplyResult{}, err
		}
	}
	if req.Currency != "" {
		if amended, err = l.AmendMandateCurrency(req.MndtID, req.Currency); err != nil {
			return Pain010ApplyResult{}, err
		}
	}
	if req.Amendment.Frequency != "" {
		if amended, err = l.AmendMandateFrequency(req.MndtID, req.Amendment.Frequency); err != nil {
			return Pain010ApplyResult{}, err
		}
	}

	// The MsgId record and the inbound direction marker, under
	// one short write lock: the amendment is already durable
	// when the marker lands.
	changed := pain010ChangedFields(req, m)
	l.mu.Lock()
	if l.pain010Applied == nil {
		l.pain010Applied = make(map[string]pain010AppliedRecord)
	}
	l.pain010Applied[req.MsgID] = pain010AppliedRecord{
		MsgID:            req.MsgID,
		MandateID:        req.MndtID,
		CreditorSchemeID: req.CreditorSchemeID,
		Amendment:        req.Amendment,
		Currency:         req.Currency,
		Mandate:          amended,
	}
	l.emitAudit(AuditEvent{
		Op:            "pain010_amendment_applied",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{amended.Payer, amended.Payee},
		Details: map[string]any{
			"msg_id":         req.MsgID,
			"mandate_id":     req.MndtID,
			"changed_fields": changed,
		},
	})
	l.mu.Unlock()
	return Pain010ApplyResult{Mandate: amended}, nil
}
