package ledger

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pain.013.001.07 Creditor Payment Activation Request
// inbound processing (LG-427): the inbound half of the pain.013
// lifecycle, the mirror of the LG-407 export.
//
// ExportPain013 renders this ledger's own request for payment —
// the creditor telling the debtor "please pay this request"
// (outbound, read-only). This module consumes the request a
// counterparty creditor sent to this ledger, acting as the
// debtor's bank: it registers the request as an RfP, and nothing
// else. The two directions are orthogonal by construction:
//
//   - LG-407 exports: the ledger asks the debtor to pay one of
//     its own RfPs (outbound, read-only, no ledger state).
//   - LG-427 imports: a counterparty creditor asks the ledger to
//     register their request (inbound, state-changing only in
//     the RfP registry: one RfP row per EndToEndId).
//   - LG-416 imports: the debtor's pain.014 answer to a request
//     (inbound, state-changing: issued -> paid | rejected).
//
// ParsePain013 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// The parse structs mirror the export's rendered fields exactly
// (GrpHdr/MsgId + PmtInf/PmtInfId/ReqdExctnDt/XpryDt/Dbtr/
// DbtrAcct/DbtrAgt + CdtTrfTx/PmtId/EndToEndId/Amt/InstdAmt/
// Cdtr/CdtrAcct), so a document this ledger's export renders
// parses back; no sub-element spelling is invented beyond what
// pain013.go renders. The namespace rides the Document element,
// so a document in the wrong schema version fails the unmarshal
// — the version gate is the namespace, not a string check (the
// camt.056 inbound convention, LG-414).
//
// The request's fields, and what they route to:
//
//   - EndToEndId: the RfP's ID — the wire's natural key, and
//     the key AcceptRfP later pays through (rfp/<id>/payout).
//     No whitespace, per rfp.go ErrRfPIDWhitespace.
//   - CdtrAcct / DbtrAcct (IBANs): the creditor / debtor. Each
//     IBAN must name an account the ledger already registered
//     an IBAN for — the ledger never opens an account to
//     receive a request — and the resolved accounts route
//     through CreateRfP as Creditor / Debtor.
//   - InstdAmt (+ Ccy): the amount and currency, through
//     CreateRfP's AmountCents / Currency.
//   - XpryDt: the expiry, through CreateRfP's ExpiresAt.
//     ReqdExctnDt is the requested execution date (the export
//     renders the RfP's creation date there); it is carried
//     verbatim and format-checked, but it does not set
//     CreatedAt — CreateRfP stamps the registration time, the
//     same rule as an operator-side registration.
//   - The party names and the agent BICs are carried verbatim
//     when stated and never gated: the IBANs associate the
//     request, not the names (the pain.009 convention).
//
// Registration reuses CreateRfP, so the RfP row and the
// rfp_issued audit event are exactly the operator-side ones;
// ApplyPain013 additionally emits `pain013_request_received`
// (details msg_id / rfp_id / creditor / debtor / amount_cents /
// currency / expires_at) so the trail tells an inbound
// registration apart from a local one. Registration is not
// acceptance: the RfP stays issued, no funds move, acceptance
// still goes through AcceptRfP and expiry through the existing
// ExpireRfPs sweep.
//
// One deliberate divergence from the pain.014 amount parser:
// pain013ParseAmount accepts a signed value ("0.00", "-5.00")
// instead of refusing non-positive amounts at parse time, so
// the wire reaches CreateRfP's non-positive gate — the spec's
// invalid terms (same-party, non-positive amount,
// expiry-not-in-future) are CreateRfP gates, and they park in
// the repair queue from ApplyPain013 rather than failing the
// parse. A malformed amount (two decimal points, three
// fraction digits, non-numeric) is still a parse refusal.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// unknown debtor/creditor IBAN parks in the repair queue for
// operator resolution, and a CreateRfP validation refusal
// parks the same way; both typed errors carry the repair
// record ID. A document the ledger cannot even parse is parked
// the same way by ParkPain013Unparsed — visibility parking,
// never a silent drop of the counterparty's message. Repair
// records parked by this module are visibility parking, not
// resubmittable instructions: their Maker is deliberately
// empty, so the CreatePayout gate can never pass them.
//
// Idempotency is the GrpHdr MsgId with its terms (the pain.014
// convention, LG-416): re-applying the same MsgId with the same
// EndToEndId, parties, amount, currency, and expiry returns the
// stored RfP with Duplicate set, registering nothing new and
// emitting no second audit event. A MsgId reused for different
// terms is refused with ErrPain013MsgIDConflict — never
// silently overwritten. A MsgId that names an RfP the ledger
// already holds (registered locally, or by an earlier message
// under another MsgId) converges on CreateRfP's own duplicate
// path and returns the stored RfP with Duplicate set.
//
// No fund movement; no pain.014 response; no RfP state-machine
// change; no pain.013 export change.

// pain.013 inbound errors.
var (
	// ErrInvalidPain013 is returned by ParsePain013 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain013 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a creditor payment activation request. The
	// HTTP layer parks it in the repair queue and maps it to 400.
	ErrInvalidPain013 = errors.New("ledger: invalid pain.013 creditor payment activation request")
	// ErrPain013UnknownAccount is returned by ApplyPain013 when
	// the request's debtor or creditor IBAN names no account the
	// ledger registered an IBAN for: the ledger never opens an
	// account to receive a request. The request is parked in the
	// repair queue, not dropped. It is a 404-class error.
	ErrPain013UnknownAccount = errors.New("ledger: pain.013 inbound: request account is unknown")
	// ErrPain013MsgIDConflict is returned by ApplyPain013 when a
	// GrpHdr MsgId already applied carries different terms: the
	// message ID names one request, and a second, different
	// request under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrPain013MsgIDConflict = errors.New("ledger: pain.013 inbound: message ID already applied with different terms")
)

// Pain013Request is one parsed inbound pain.013.001.07 creditor
// payment activation request: the request's identity, the RfP it
// registers, both parties and their accounts, and the amount,
// currency, and expiry.
type Pain013Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one request.
	MsgID string `json:"msg_id"`
	// PmtInfID is PmtInf/PmtInfId, carried verbatim when stated;
	// the export renders it equal to the MsgId.
	PmtInfID string `json:"pmt_inf_id,omitempty"`
	// ReqdExctnDt is PmtInf/ReqdExctnDt (<Dt> or <DtTm>): the
	// requested execution date, carried and format-checked,
	// never the RfP's CreatedAt.
	ReqdExctnDt time.Time `json:"reqd_exctn_dt,omitempty"`
	// ExpiresAt is PmtInf/XpryDt (<DtTm> in the export, <Dt>
	// also accepted): the RfP's deadline.
	ExpiresAt time.Time `json:"expires_at"`
	// DebtorName is PmtInf/Dbtr/Nm and CreditorName is
	// CdtTrfTx/Cdtr/Nm, carried verbatim when stated; neither
	// is required — the IBANs associate the request, not the
	// names.
	DebtorName   string `json:"debtor_name,omitempty"`
	CreditorName string `json:"creditor_name,omitempty"`
	// DebtorIBAN is PmtInf/DbtrAcct/Id/IBAN; CreditorIBAN is
	// CdtTrfTx/CdtrAcct/Id/IBAN. Both are required, normalized
	// (see normalizeIBAN) — resolution against the ledger
	// happens in ApplyPain013.
	DebtorIBAN   string `json:"debtor_iban"`
	CreditorIBAN string `json:"creditor_iban"`
	// DebtorBIC is PmtInf/DbtrAgt/FinInstnId/BIC and CreditorBIC
	// is CdtTrfTx/CdtrAgt/FinInstnId/BIC, carried verbatim when
	// stated; neither is gated — the IBANs associate the
	// request, not the agents.
	DebtorBIC   string `json:"debtor_bic,omitempty"`
	CreditorBIC string `json:"creditor_bic,omitempty"`
	// EndToEndID is CdtTrfTx/PmtId/EndToEndId: the RfP's ID,
	// the wire's natural key.
	EndToEndID string `json:"end_to_end_id"`
	// AmountCents is CdtTrfTx/Amt/InstdAmt in minor units;
	// Currency is its Ccy attribute, normalized. A
	// non-positive amount parses (see the package comment) and
	// is refused by CreateRfP, parked in the repair queue.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// Pain013ApplyResult is the outcome of ApplyPain013.
type Pain013ApplyResult struct {
	// RfP is the registered request (or the stored one on the
	// duplicate path).
	RfP RfP `json:"rfp"`
	// RepairID is set on the parked fail-closed path (unknown
	// account, CreateRfP validation refusal): the repair-queue
	// record holding the request for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the request was already
	// registered under the same MsgId with the same terms, or
	// the RfP already existed: a no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain013AppliedRecord is the replay record for one applied
// pain.013 request, keyed by GrpHdr MsgId in pain013Applied:
// the request terms it carried and the RfP snapshot after
// registration, so a replay returns the same outcome without
// re-registering.
type pain013AppliedRecord struct {
	MsgID        string
	EndToEndID   string
	DebtorIBAN   string
	CreditorIBAN string
	AmountCents  int64
	Currency     string
	ExpiresAt    time.Time
	RfP          RfP
}

// pain013TermsEqual reports whether a stored request record
// carries exactly the terms an inbound request would register:
// the same RfP ID, the same parties (by normalized IBAN), the
// same amount and currency, and the same expiry. The expiry
// comparison uses Equal — the wire carries a <DtTm> timestamp
// (or a <Dt> date), and both sides are UTC instants.
func pain013TermsEqual(rec pain013AppliedRecord, req Pain013Request) bool {
	return rec.EndToEndID == req.EndToEndID &&
		rec.DebtorIBAN == normalizeIBAN(req.DebtorIBAN) &&
		rec.CreditorIBAN == normalizeIBAN(req.CreditorIBAN) &&
		rec.AmountCents == req.AmountCents &&
		rec.Currency == req.Currency &&
		rec.ExpiresAt.Equal(req.ExpiresAt)
}

// The pain013x* structs are the inbound-side XML model for
// pain.013.001.07: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (GrpHdr/MsgId, PmtInf/PmtInfId/PmtMtd/ReqdExctnDt/XpryDt/
// Dbtr/DbtrAcct/DbtrAgt, CdtTrfTx/PmtId/EndToEndId/Amt/InstdAmt/
// ChrgBr/CdtrAgt/Cdtr/CdtrAcct) so a document this ledger's
// export renders parses back. ReqdExctnDt and XpryDt are
// DateAndDateTime2Choice (<Dt> or <DtTm>); the namespace on
// Document gates the schema version.
type pain013xDocument struct {
	XMLName xml.Name    `xml:"urn:iso:std:iso:20022:tech:xsd:pain.013.001.07 Document"`
	Req     pain013xReq `xml:"CdtrPmtActvtnReq"`
}

type pain013xReq struct {
	GrpHdr pain013xGrpHdr `xml:"GrpHdr"`
	PmtInf pain013xPmtInf `xml:"PmtInf"`
}

type pain013xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain013xPmtInf struct {
	PmtInfID    string             `xml:"PmtInfId"`
	PmtMtd      string             `xml:"PmtMtd"`
	ReqdExctnDt pain013xDateChoice `xml:"ReqdExctnDt"`
	XpryDt      pain013xDateChoice `xml:"XpryDt"`
	Dbtr        pain013xParty      `xml:"Dbtr"`
	DbtrAcct    pain013xAcct       `xml:"DbtrAcct"`
	DbtrAgt     *pain013xAgt       `xml:"DbtrAgt,omitempty"`
	CdtTrfTx    pain013xCdtTrfTx   `xml:"CdtTrfTx"`
}

type pain013xDateChoice struct {
	Dt   string `xml:"Dt"`
	DtTm string `xml:"DtTm"`
}

type pain013xParty struct {
	Nm string `xml:"Nm"`
}

type pain013xAcct struct {
	ID pain013xAcctID `xml:"Id"`
}

type pain013xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type pain013xAgt struct {
	FinInstnId pain013xFinInstnID `xml:"FinInstnId"`
}

type pain013xFinInstnID struct {
	BIC string `xml:"BIC"`
}

type pain013xCdtTrfTx struct {
	PmtID    pain013xPmtID     `xml:"PmtId"`
	Amt      pain013xAmtChoice `xml:"Amt"`
	ChrgBr   string            `xml:"ChrgBr"`
	CdtrAgt  *pain013xAgt      `xml:"CdtrAgt,omitempty"`
	Cdtr     pain013xParty     `xml:"Cdtr"`
	CdtrAcct pain013xAcct      `xml:"CdtrAcct"`
}

type pain013xPmtID struct {
	EndToEndID string `xml:"EndToEndId"`
}

type pain013xAmtChoice struct {
	InstdAmt *pain013Amount `xml:"InstdAmt"`
}

type pain013Amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pain013ParseAmount parses an ISO 20022 decimal amount
// ("1234.56") into minor units, the pacs003ParseAmount rules:
// the integer part holds at most 12 digits, the fraction at most
// two digits and is right-padded ("5" means 50 cents). Unlike
// pain014ParseAmount it accepts a signed value ("0.00",
// "-5.00"): a non-positive amount is a business refusal for
// CreateRfP to make (and park), not a parse refusal — see the
// package comment. Anything malformed is a parse refusal — an
// amount the ledger cannot read exactly is never rounded into a
// registration.
func pain013ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidPain013)
	}
	neg := false
	if strings.HasPrefix(body, "-") {
		neg = true
		body = body[1:]
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidPain013, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain013, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidPain013, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain013, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain013, body)
		}
	}
	cents := whole*100 + frac
	if neg {
		cents = -cents
	}
	return cents, nil
}

// pain013ParseDateChoice parses one ISO 20022
// DateAndDateTime2Choice (<Dt> YYYY-MM-DD or <DtTm> RFC3339)
// into an instant. A choice with neither child is a parse
// refusal — the export renders both dates [1..1].
func pain013ParseDateChoice(c pain013xDateChoice, field string) (time.Time, error) {
	if s := strings.TrimSpace(c.DtTm); s != "" {
		d, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %s/DtTm %q must be RFC3339", ErrInvalidPain013, field, s)
		}
		return d, nil
	}
	if s := strings.TrimSpace(c.Dt); s != "" {
		d, err := time.Parse("2006-01-02", strings.TrimSpace(s))
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %s/Dt %q must be YYYY-MM-DD", ErrInvalidPain013, field, s)
		}
		return d, nil
	}
	return time.Time{}, fmt.Errorf("%w: missing %s date", ErrInvalidPain013, field)
}

// validatePain013Request fail-closes on a request ParsePain013
// would refuse, so ApplyPain013 stays safe on hand-built
// requests too: the parse gate and the apply gate share one
// check, never a copy. The business gates (same-party,
// non-positive amount, expiry-not-in-future) are deliberately
// NOT here: they belong to CreateRfP, which refuses them and
// parks them in the repair queue.
func validatePain013Request(req Pain013Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain013)
	}
	if strings.TrimSpace(req.EndToEndID) == "" {
		return fmt.Errorf("%w: missing CdtTrfTx/PmtId/EndToEndId", ErrInvalidPain013)
	}
	if strings.TrimSpace(req.DebtorIBAN) == "" {
		return fmt.Errorf("%w: missing PmtInf/DbtrAcct/Id/IBAN", ErrInvalidPain013)
	}
	if strings.TrimSpace(req.CreditorIBAN) == "" {
		return fmt.Errorf("%w: missing CdtTrfTx/CdtrAcct/Id/IBAN", ErrInvalidPain013)
	}
	if _, err := normalizeCurrency(req.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrInvalidPain013, req.Currency)
	}
	if req.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: missing PmtInf/XpryDt", ErrInvalidPain013)
	}
	return nil
}

// ParsePain013 parses one inbound pain.013.001.07 creditor
// payment activation request into a Pain013Request. It validates
// the whole document before returning anything: a missing MsgId,
// a missing EndToEndId, a missing debtor or creditor IBAN, a
// missing or malformed ReqdExctnDt/XpryDt, or a missing or
// malformed InstdAmt (or its currency) are all parse refusals —
// ApplyPain013 never sees a half-read request. The party names
// and the agent BICs are carried verbatim when stated and never
// gated: the IBANs associate the request, not the names. A
// non-positive amount and a past expiry parse fine: they are
// CreateRfP refusals, parked by ApplyPain013.
func ParsePain013(data []byte) (Pain013Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain013Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain013)
	}
	var doc pain013xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain013Request{}, fmt.Errorf("%w: %v", ErrInvalidPain013, err)
	}
	req := doc.Req
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain013Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain013)
	}
	pmt := req.PmtInf
	endToEndID := strings.TrimSpace(pmt.CdtTrfTx.PmtID.EndToEndID)
	if endToEndID == "" {
		return Pain013Request{}, fmt.Errorf("%w: missing CdtTrfTx/PmtId/EndToEndId", ErrInvalidPain013)
	}
	debtorIBAN := normalizeIBAN(strings.TrimSpace(pmt.DbtrAcct.ID.IBAN))
	if debtorIBAN == "" {
		return Pain013Request{}, fmt.Errorf("%w: missing PmtInf/DbtrAcct/Id/IBAN", ErrInvalidPain013)
	}
	creditorIBAN := normalizeIBAN(strings.TrimSpace(pmt.CdtTrfTx.CdtrAcct.ID.IBAN))
	if creditorIBAN == "" {
		return Pain013Request{}, fmt.Errorf("%w: missing CdtTrfTx/CdtrAcct/Id/IBAN", ErrInvalidPain013)
	}
	reqdExctnDt, err := pain013ParseDateChoice(pmt.ReqdExctnDt, "PmtInf/ReqdExctnDt")
	if err != nil {
		return Pain013Request{}, err
	}
	expiresAt, err := pain013ParseDateChoice(pmt.XpryDt, "PmtInf/XpryDt")
	if err != nil {
		return Pain013Request{}, err
	}
	if pmt.CdtTrfTx.Amt.InstdAmt == nil {
		return Pain013Request{}, fmt.Errorf("%w: missing CdtTrfTx/Amt/InstdAmt", ErrInvalidPain013)
	}
	amt := pmt.CdtTrfTx.Amt.InstdAmt
	cents, err := pain013ParseAmount(amt.Value)
	if err != nil {
		return Pain013Request{}, err
	}
	ccy := strings.TrimSpace(amt.Ccy)
	if ccy == "" {
		return Pain013Request{}, fmt.Errorf("%w: empty CdtTrfTx/Amt/InstdAmt currency", ErrInvalidPain013)
	}
	currency, err := normalizeCurrency(ccy)
	if err != nil {
		return Pain013Request{}, fmt.Errorf("%w: currency %q", ErrInvalidPain013, ccy)
	}
	debtorBIC := ""
	if pmt.DbtrAgt != nil {
		debtorBIC = strings.TrimSpace(pmt.DbtrAgt.FinInstnId.BIC)
	}
	creditorBIC := ""
	if pmt.CdtTrfTx.CdtrAgt != nil {
		creditorBIC = strings.TrimSpace(pmt.CdtTrfTx.CdtrAgt.FinInstnId.BIC)
	}
	return Pain013Request{
		MsgID:        msgID,
		PmtInfID:     strings.TrimSpace(pmt.PmtInfID),
		ReqdExctnDt:  reqdExctnDt,
		ExpiresAt:    expiresAt,
		DebtorName:   strings.TrimSpace(pmt.Dbtr.Nm),
		DebtorIBAN:   debtorIBAN,
		DebtorBIC:    debtorBIC,
		CreditorName: strings.TrimSpace(pmt.CdtTrfTx.Cdtr.Nm),
		CreditorIBAN: creditorIBAN,
		CreditorBIC:  creditorBIC,
		EndToEndID:   endToEndID,
		AmountCents:  cents,
		Currency:     currency,
	}, nil
}

// resolvePain013IBANLocked resolves one stated IBAN against the
// ledger's registered-IBAN registry (normalized, the
// SetAccountIBAN canonical form). The account must already be
// registered — the ledger never opens an account to receive a
// request. Callers must hold l.mu (either lock suffices).
func (l *Ledger) resolvePain013IBANLocked(iban string) (AccountID, error) {
	norm := normalizeIBAN(iban)
	for acct, reg := range l.accountIBANs {
		if reg.IBAN == norm {
			return acct, nil
		}
	}
	return "", fmt.Errorf("%w: IBAN %q", ErrPain013UnknownAccount, iban)
}

// pain013LenientDocument is the namespace-agnostic wire shape
// ParkPain013Unparsed reads: a document ParsePain013 refused
// for its version may still name the request, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type pain013LenientDocument struct {
	XMLName xml.Name          `xml:"Document"`
	Req     pain013LenientReq `xml:"CdtrPmtActvtnReq"`
}

type pain013LenientReq struct {
	GrpHdr pain013LenientGrpHdr `xml:"GrpHdr"`
}

type pain013LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

// ParkPain013Unparsed best-effort parks a pain.013 document
// ParsePain013 refused, when the document still names a request:
// the GrpHdr MsgId it carries. It returns the repair record ID,
// or "" when the document names nothing parkable (garbage bytes,
// an empty shell): there is no request for the operator to find,
// so there is nothing to park. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the counterparty's message. The reason code is
// invalid_format — the document was present but unreadable as a
// creditor payment activation request.
func (l *Ledger) ParkPain013Unparsed(data []byte, actor string) (string, error) {
	var doc pain013LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.Req.GrpHdr.MsgID)
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "pain013-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain013Request parks a parsed pain.013 request that failed
// closed, and returns the repair record ID. The snapshot's Maker
// is deliberately empty (see the package comment).
func (l *Ledger) parkPain013Request(req Pain013Request, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: req.MsgID,
		// Maker intentionally empty: see the package comment.
		Destination: "pain013-inbound:" + req.MsgID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyPain013 registers one parsed inbound pain.013.001.07
// creditor payment activation request as an RfP: the inbound
// half of the pain.013 flow, the counterparty-side mirror of the
// outbound export. The EndToEndId is the RfP's ID; the debtor
// and creditor IBANs must each name an account the ledger
// already registered an IBAN for — otherwise the request is
// refused fail-closed and parked in the repair queue, and no
// account is ever opened.
//
// Registration reuses CreateRfP, so the RfP row and the
// rfp_issued audit event are exactly the ones an operator-side
// registration produces; ApplyPain013 additionally emits
// `pain013_request_received` so the trail tells an inbound
// registration apart. Only the RfP registers: no journal rows,
// no version bump, no money moved — acceptance still goes
// through AcceptRfP, expiry through ExpireRfPs.
//
// Idempotency is the MsgId with its terms: re-applying the same
// MsgId with the same EndToEndId, parties, amount, currency,
// and expiry returns the stored RfP with Duplicate set, no
// second audit event. A MsgId reused for different terms is
// refused with ErrPain013MsgIDConflict — never silently
// overwritten. A MsgId naming an RfP the ledger already holds
// converges on CreateRfP's own duplicate path.
//
// Locking: the structural validation runs on the parsed values
// alone; the replay read and the IBAN resolutions run under the
// read lock; CreateRfP takes the write lock itself; the MsgId
// record and the inbound audit event run under a short write
// lock of their own; the repair parking runs outside any held
// lock because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyPain013(req Pain013Request, actor string) (Pain013ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain013"
	}
	if err := validatePain013Request(req); err != nil {
		return Pain013ApplyResult{}, err
	}

	// MsgId replay: the request was already registered under
	// this MsgId. Same terms is a no-op returning the stored
	// RfP; different terms is a conflict.
	l.mu.RLock()
	rec, ok := l.pain013Applied[req.MsgID]
	l.mu.RUnlock()
	if ok {
		if !pain013TermsEqual(rec, req) {
			return Pain013ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain013MsgIDConflict, req.MsgID)
		}
		current, ok := l.GetRfP(rec.EndToEndID)
		if !ok {
			return Pain013ApplyResult{RfP: rec.RfP, Duplicate: true}, nil
		}
		return Pain013ApplyResult{RfP: current, Duplicate: true}, nil
	}

	// IBAN resolution under the read lock: both parties must
	// already be registered, and neither resolution opens an
	// account.
	l.mu.RLock()
	debtor, debtorErr := l.resolvePain013IBANLocked(req.DebtorIBAN)
	creditor, creditorErr := l.resolvePain013IBANLocked(req.CreditorIBAN)
	l.mu.RUnlock()
	if debtorErr != nil {
		// The parked fail-closed path, outside any held lock:
		// an unknown debtor IBAN goes to the repair queue.
		repairID, parkErr := l.parkPain013Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return Pain013ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", debtorErr, parkErr)
		}
		return Pain013ApplyResult{RepairID: repairID}, debtorErr
	}
	if creditorErr != nil {
		repairID, parkErr := l.parkPain013Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return Pain013ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", creditorErr, parkErr)
		}
		return Pain013ApplyResult{RepairID: repairID}, creditorErr
	}

	// Registration reuses CreateRfP: its gates (same-party,
	// non-positive amount, expiry-not-in-future) apply, and a
	// refusal parks in the repair queue — the request as a whole
	// is invalid as a registration.
	created, dup, err := l.CreateRfP(context.Background(), RfP{
		ID:          req.EndToEndID,
		Creditor:    creditor,
		Debtor:      debtor,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
		ExpiresAt:   req.ExpiresAt,
	})
	if err != nil {
		repairID, parkErr := l.parkPain013Request(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return Pain013ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
		}
		return Pain013ApplyResult{RepairID: repairID}, err
	}

	// The MsgId record and the inbound direction marker, under
	// one short write lock: the registration is already durable
	// when the marker lands. A CreateRfP duplicate (the RfP
	// already existed) records the MsgId but emits no second
	// audit event — the registration was not this message's.
	l.mu.Lock()
	if l.pain013Applied == nil {
		l.pain013Applied = make(map[string]pain013AppliedRecord)
	}
	l.pain013Applied[req.MsgID] = pain013AppliedRecord{
		MsgID:        req.MsgID,
		EndToEndID:   req.EndToEndID,
		DebtorIBAN:   normalizeIBAN(req.DebtorIBAN),
		CreditorIBAN: normalizeIBAN(req.CreditorIBAN),
		AmountCents:  req.AmountCents,
		Currency:     req.Currency,
		ExpiresAt:    req.ExpiresAt,
		RfP:          created,
	}
	if !dup {
		l.emitAudit(AuditEvent{
			Op:            "pain013_request_received",
			Actor:         actor,
			TraceID:       req.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{creditor, debtor},
			Details: map[string]any{
				"msg_id":       req.MsgID,
				"rfp_id":       req.EndToEndID,
				"creditor":     string(creditor),
				"debtor":       string(debtor),
				"amount_cents": req.AmountCents,
				"currency":     created.Currency,
				"expires_at":   req.ExpiresAt.UTC().Format(time.RFC3339),
			},
		})
	}
	l.mu.Unlock()
	return Pain013ApplyResult{RfP: created, Duplicate: dup}, nil
}
