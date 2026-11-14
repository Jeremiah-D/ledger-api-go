package ledger

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pain.014.001.07 creditor payment activation request
// status report inbound processing (LG-416): the debtor's response
// to a pain.013 request for payment — the inbound half that closes
// the RfP loop LG-407 opened.
//
// ExportPain013 renders the creditor's request (pain.013.001.07);
// this module consumes the debtor's answer (pain.014.001.07) on the
// creditor's side: an ACCP verdict pays the RfP through AcceptRfP,
// an RJCT verdict rejects it through RejectRfP. The two are
// orthogonal by construction:
//
//   - LG-407 exports: the ledger tells the debtor "please pay this
//     request" (outbound, read-only, no ledger state).
//   - LG-416 imports: the debtor tells the ledger "I accept / I
//     refuse" (inbound, state-changing: issued -> paid | rejected).
//   - LG-171 owns the RfP lifecycle itself: CreateRfP registers the
//     request, AcceptRfP/RejectRfP are the local accept/reject legs.
//     This module is only the wire adapter that routes a bank
//     response into those legs — it changes no RfP semantics.
//
// Version note (checked 2026-11-14, not assumed): the registry
// knows pain.014.001.05 and .001.07; the .07 version is the
// FedNow/RTP one (CreditorPaymentActivationRequestStatusReportV07,
// root <CdtrPmtActvtnReqStsRpt>), confirmed by two sources that
// agree: the moov-io rtp20022 gen/pain_014_001_07 bindings (the TCH
// profile, with the OrigMsgNamePain01300107 = "pain.013.001.07"
// constant) and the moov-io fedwire20022
// gen/DrawdownResponse_pain_014_001_07 bindings, both naming the
// same root and namespace
// urn:iso:std:iso:20022:tech:xsd:pain.014.001.07. The parse
// therefore gates on the .001.07 namespace, not a string check (the
// camt.056 inbound convention, LG-414).
//
// Structure, verified against the moov-io rtp20022
// gen/pain_014_001_07 TCH binding:
//
//   - GrpHdr (GroupHeader87): MsgId [1..1], CreDtTm [1..1],
//     InitgPty [1..1]. The sender's MsgId is the replay key.
//   - OrgnlGrpInfAndSts (OriginalGroupInformation30): OrgnlMsgId
//     [1..1], OrgnlMsgNmId [1..1] (the OrigMsgName enum, whose only
//     value is "pain.013.001.07"), OrgnlCreDtTm [1..1],
//     OrgnlNbOfTxs [1..1]. The report answers exactly one pain.013
//     message; anything else is a parse refusal.
//   - OrgnlPmtInfAndSts (OriginalPaymentInstruction31):
//     OrgnlPmtInfId [1..1], TxInfAndSts [1..1] (PaymentTransaction104).
//   - TxInfAndSts: OrgnlEndToEndId [0..1], OrgnlUETR [0..1], TxSts
//     [1..1] (ExternalPaymentTransactionStatus1Code), StsRsnInf
//     [0..1] (StatusReasonInformation12: Rsn {Cd | Prtry} +
//     AddtlInf), PmtCondSts [0..1] (PaymentConditionStatus1:
//     AccptdAmt, GrntedPmt, EarlyPmt), OrgnlTxRef [1..1]
//     (OriginalTransactionReference29: Amt? {InstdAmt},
//     ReqdExctnDt, CdtrAgt, Cdtr).
//
// The verdict vocabulary is the two transaction statuses the debtor
// can actually return on a request for payment: ACCP (accepted — pay
// it) and RJCT (rejected — refuse it). Any other TxSts (PDNG, ACTC,
// ACSP, ...) is a parse refusal: the document carries no debtor
// verdict the ledger can act on, so it is refused rather than
// coerced into a lifecycle step.
//
// The response is associated with its RfP by OrgnlEndToEndId —
// pain.013 renders the RfP ID as the EndToEndId, so the response
// names the request it answers. An unknown EndToEndId is refused
// fail-closed (ErrRfPNotFound): a response to a request the ledger
// never issued is evidence about someone else's payment.
//
// The amount gate, when the response states an amount: OrgnlTxRef
// Amt/InstdAmt and PmtCondSts AccptdAmt, when present, must equal the
// RfP's amount and currency — the LG-163 FedNow amount-mismatch
// gate. A debtor who answers a different amount (a partial accept,
// a re-priced request) is refused, not silently paid at the RfP's
// amount: AcceptRfP pays the full registered amount, so paying on a
// mismatched response would move money the debtor never agreed to.
// Both amount elements are optional on the wire, so an amount-less
// response skips the gate rather than failing it.
//
// Idempotency: the sender's GrpHdr MsgId is the replay key, recorded
// in pain014Responses. Re-applying the same document returns the
// stored RfP with no second booking and no second inbound audit
// event (the pacs.003 replay convention). A MsgId reused for a
// different response (another EndToEndId or another verdict) is
// refused with ErrPain014MsgIDConflict — one message ID names one
// response. A second, distinct response to an already-terminal RfP
// is refused by the RfP state machine itself (ErrRfPNotIssued /
// ErrRfPExpired), not absorbed.
//
// Audit: a fresh application emits `pain014_response_applied`
// (msg_id, rfp_id, response accept/reject, actor; transfer_id on
// accept, reason code/info on reject), distinct from the
// `rfp_accepted` / `rfp_paid` / `rfp_rejected` events the RfP legs
// themselves emit, so the audit trail tells the inbound pain.014
// direction apart from an operator-driven AcceptRfP/RejectRfP (the
// pacs.003 `pacs003_debit_applied` convention, LG-415).

// pain.014 inbound errors.
var (
	// ErrPain014Parse is returned by ParsePain014 for a malformed
	// document or a document missing a required field: the wrong
	// schema version, a missing MsgId, a missing OrgnlMsgId, an
	// OrgnlMsgNmId other than pain.013.001.07, a missing
	// OrgnlEndToEndId, a TxSts outside the ACCP/RJCT vocabulary,
	// or an unreadable amount. The HTTP layer maps it to 400.
	ErrPain014Parse = errors.New("ledger: pain.014 inbound: unparseable creditor payment activation request status report")
	// ErrPain014AmountMismatch is returned when a response states
	// an amount (OrgnlTxRef Amt or PmtCondSts AccptdAmt) that
	// differs from the RfP's registered amount: it is evidence
	// about a different payment, so it must not pay against it.
	// It is a 422-class error.
	ErrPain014AmountMismatch = errors.New("ledger: pain.014 amount does not match the original request")
	// ErrPain014CurrencyMismatch is returned when a response states
	// a currency that differs from the RfP's registered currency.
	// It is a 422-class error.
	ErrPain014CurrencyMismatch = errors.New("ledger: pain.014 currency does not match the original request")
	// ErrPain014MsgIDConflict is returned when a GrpHdr MsgId
	// already recorded names a different response (another
	// EndToEndId or another verdict): one message ID names one
	// response. It is a 422-class error.
	ErrPain014MsgIDConflict = errors.New("ledger: pain.014 MsgId already applied to a different response")
)

// Pain014Response verdicts: the debtor's answer to a request for
// payment, as the ledger records it.
const (
	// Pain014ResponseAccepted is the ACCP verdict: the debtor
	// accepts and the RfP pays through AcceptRfP.
	Pain014ResponseAccepted = "accepted"
	// Pain014ResponseRejected is the RJCT verdict: the debtor
	// refuses and the RfP rejects through RejectRfP.
	Pain014ResponseRejected = "rejected"
)

// pain014TxStatusACCP / RJCT are the only TxSts values a pain.014
// response may carry: the debtor's accept / refuse verdicts.
const (
	pain014TxStatusACCP = "ACCP"
	pain014TxStatusRJCT = "RJCT"
)

// pain014OrgnlMsgName is the only OrgnlMsgNmId a pain.014 may
// answer: the pain.013.001.07 request. The moov-io rtp20022 TCH
// binding pins the same value as OrigMsgNamePain01300107.
const pain014OrgnlMsgName = "pain.013.001.07"

// Pain014Response is one parsed inbound pain.014 debtor response:
// the response's identity, the RfP it answers, the verdict, and the
// amounts the debtor stated, when it stated any.
type Pain014Response struct {
	// MsgID is the response's GrpHdr MsgId: the audit trace and
	// the replay key.
	MsgID string `json:"msg_id"`
	// OrgnlMsgID is OrgnlGrpInfAndSts/OrgnlMsgId: the pain.013
	// message this answers (deterministic "pain013-<rfp ID>").
	OrgnlMsgID string `json:"orgnl_msg_id"`
	// OrgnlMsgNmID is OrgnlGrpInfAndSts/OrgnlMsgNmId: always
	// "pain.013.001.07".
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id"`
	// EndToEndID is TxInfAndSts/OrgnlEndToEndId: the RfP ID, the
	// business identity of the request being answered.
	EndToEndID string `json:"end_to_end_id"`
	// UETR is TxInfAndSts/OrgnlUETR, when the debtor states one.
	// Informational: the association is the EndToEndId, never
	// the UETR.
	UETR string `json:"uetr,omitempty"`
	// Response is "accepted" (TxSts ACCP) or "rejected" (TxSts
	// RJCT).
	Response string `json:"response"`
	// TxStatus is the raw TxSts code: "ACCP" or "RJCT".
	TxStatus string `json:"tx_status"`
	// ReasonCode is StsRsnInf/Rsn Cd (or Prtry when Cd is empty):
	// the debtor's reason, carried verbatim, never interpreted.
	// ReasonInfo is StsRsnInf/AddtlInf free text.
	ReasonCode string `json:"reason_code,omitempty"`
	ReasonInfo string `json:"reason_info,omitempty"`
	// HasAmount reports whether OrgnlTxRef/Amt/InstdAmt was
	// stated; AmountCents/Currency are its value. When stated,
	// they must equal the RfP's amount and currency.
	HasAmount   bool   `json:"has_amount"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	Currency    string `json:"currency,omitempty"`
	// HasAcceptedAmount reports whether PmtCondSts/AccptdAmt was
	// stated; AcceptedAmountCents/AcceptedCurrency are its
	// value. When stated, it must equal the RfP's amount and
	// currency — a partial accept is refused, not paid in part.
	HasAcceptedAmount   bool   `json:"has_accepted_amount"`
	AcceptedAmountCents int64  `json:"accepted_amount_cents,omitempty"`
	AcceptedCurrency    string `json:"accepted_currency,omitempty"`
}

// pain014AppliedRecord is the replay record for one applied pain.014
// response, keyed by GrpHdr MsgId in pain014Responses: the response
// it carried and the RfP snapshot after application, so a replay
// returns the same outcome without re-booking.
type pain014AppliedRecord struct {
	MsgID      string
	EndToEndID string
	Response   string
	TransferID string
	ReasonCode string
	RfP        RfP
}

// pain014Document is the wire shape ParsePain014 accepts:
// pain.014.001.07 CdtrPmtActvtnReqStsRpt. The namespace rides the
// Document element, so a document in the wrong schema version fails
// the unmarshal — the version gate is the namespace, not a string
// check (the camt.056 inbound convention, LG-414). Only the fields
// the apply reads are modelled; unknown elements are skipped.
type pain014Document struct {
	XMLName xml.Name      `xml:"urn:iso:std:iso:20022:tech:xsd:pain.014.001.07 Document"`
	Report  pain014Report `xml:"CdtrPmtActvtnReqStsRpt"`
}

type pain014Report struct {
	GrpHdr            pain014GrpHdr   `xml:"GrpHdr"`
	OrgnlGrpInfAndSts pain014OrgnlGrp `xml:"OrgnlGrpInfAndSts"`
	OrgnlPmtInfAndSts pain014OrgnlPmt `xml:"OrgnlPmtInfAndSts"`
}

type pain014GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain014OrgnlGrp struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
	OrgnlCreDtTm string `xml:"OrgnlCreDtTm"`
	OrgnlNbOfTxs string `xml:"OrgnlNbOfTxs"`
}

type pain014OrgnlPmt struct {
	OrgnlPmtInfID string       `xml:"OrgnlPmtInfId"`
	TxInfAndSts   pain014TxSts `xml:"TxInfAndSts"`
}

type pain014TxSts struct {
	OrgnlEndToEndID string             `xml:"OrgnlEndToEndId"`
	OrgnlUETR       string             `xml:"OrgnlUETR"`
	TxSts           string             `xml:"TxSts"`
	StsRsnInf       *pain014StsRsnInf  `xml:"StsRsnInf"`
	PmtCondSts      *pain014PmtCondSts `xml:"PmtCondSts"`
	OrgnlTxRef      pain014OrgnlTxRef  `xml:"OrgnlTxRef"`
}

type pain014StsRsnInf struct {
	Rsn      pain014Rsn `xml:"Rsn"`
	AddtlInf string     `xml:"AddtlInf"`
}

type pain014Rsn struct {
	Cd    string `xml:"Cd"`
	Prtry string `xml:"Prtry"`
}

type pain014PmtCondSts struct {
	AccptdAmt *pain014Amount `xml:"AccptdAmt"`
	GrntedPmt string         `xml:"GrntedPmt"`
	EarlyPmt  string         `xml:"EarlyPmt"`
}

type pain014OrgnlTxRef struct {
	Amt *pain014AmtChoice `xml:"Amt"`
}

type pain014AmtChoice struct {
	InstdAmt *pain014Amount `xml:"InstdAmt"`
}

type pain014Amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pain014ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units, the pacs003ParseAmount rules: the integer part
// holds at most 12 digits, the fraction at most two digits and is
// right-padded ("5" means 50 cents), the value must be positive.
// Anything else is a parse refusal — a response amount the ledger
// cannot read exactly is never rounded into a comparison.
func pain014ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPain014Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPain014Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPain014Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPain014Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPain014Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPain014Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPain014Parse, body)
	}
	return cents, nil
}

// pain014ValidTxSts reports whether s is in the pain.014 verdict
// vocabulary: ACCP (accept) or RJCT (reject).
func pain014ValidTxSts(s string) bool {
	return s == pain014TxStatusACCP || s == pain014TxStatusRJCT
}

// validatePain014Response fail-closes on a response ParsePain014
// would refuse, so ApplyPain014Response stays safe on hand-built
// structs too: the parse gate and the apply gate share one check,
// never a copy.
func validatePain014Response(resp *Pain014Response) error {
	if resp == nil {
		return fmt.Errorf("%w: nil response", ErrPain014Parse)
	}
	if strings.TrimSpace(resp.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPain014Parse)
	}
	if strings.TrimSpace(resp.OrgnlMsgID) == "" {
		return fmt.Errorf("%w: missing OrgnlGrpInfAndSts/OrgnlMsgId", ErrPain014Parse)
	}
	if strings.TrimSpace(resp.OrgnlMsgNmID) != pain014OrgnlMsgName {
		return fmt.Errorf("%w: OrgnlMsgNmId %q does not answer a pain.013 request", ErrPain014Parse, resp.OrgnlMsgNmID)
	}
	if strings.TrimSpace(resp.EndToEndID) == "" {
		return fmt.Errorf("%w: missing TxInfAndSts/OrgnlEndToEndId", ErrPain014Parse)
	}
	if !pain014ValidTxSts(resp.TxStatus) {
		return fmt.Errorf("%w: TxSts %q is not ACCP or RJCT", ErrPain014Parse, resp.TxStatus)
	}
	if resp.Response != Pain014ResponseAccepted && resp.Response != Pain014ResponseRejected {
		return fmt.Errorf("%w: response %q is not accepted or rejected", ErrPain014Parse, resp.Response)
	}
	if resp.HasAmount {
		if resp.AmountCents <= 0 {
			return fmt.Errorf("%w: non-positive original amount", ErrPain014Parse)
		}
		if _, err := normalizeCurrency(resp.Currency); err != nil {
			return fmt.Errorf("%w: currency %q", ErrPain014Parse, resp.Currency)
		}
	}
	if resp.HasAcceptedAmount {
		if resp.AcceptedAmountCents <= 0 {
			return fmt.Errorf("%w: non-positive accepted amount", ErrPain014Parse)
		}
		if _, err := normalizeCurrency(resp.AcceptedCurrency); err != nil {
			return fmt.Errorf("%w: accepted currency %q", ErrPain014Parse, resp.AcceptedCurrency)
		}
	}
	return nil
}

// ParsePain014 parses one inbound pain.014 debtor response into a
// Pain014Response. It validates the whole document before returning
// anything: a missing MsgId, a missing OrgnlMsgId, an OrgnlMsgNmId
// other than pain.013.001.07, a missing OrgnlEndToEndId, a TxSts
// outside the ACCP/RJCT vocabulary, or an unreadable amount are all
// parse refusals — ApplyPain014Response never sees a half-read
// response. The OrgnlTxRef amount and the PmtCondSts accepted
// amount are read when stated; both are optional on the wire, and
// an amount-less response applies without the amount gate.
func ParsePain014(data []byte) (*Pain014Response, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrPain014Parse)
	}
	var doc pain014Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPain014Parse, err)
	}
	tx := doc.Report.OrgnlPmtInfAndSts.TxInfAndSts
	resp := &Pain014Response{
		MsgID:        strings.TrimSpace(doc.Report.GrpHdr.MsgID),
		OrgnlMsgID:   strings.TrimSpace(doc.Report.OrgnlGrpInfAndSts.OrgnlMsgID),
		OrgnlMsgNmID: strings.TrimSpace(doc.Report.OrgnlGrpInfAndSts.OrgnlMsgNmID),
		EndToEndID:   strings.TrimSpace(tx.OrgnlEndToEndID),
		UETR:         strings.TrimSpace(tx.OrgnlUETR),
		TxStatus:     strings.TrimSpace(tx.TxSts),
	}
	switch resp.TxStatus {
	case pain014TxStatusACCP:
		resp.Response = Pain014ResponseAccepted
	case pain014TxStatusRJCT:
		resp.Response = Pain014ResponseRejected
	}
	if tx.StsRsnInf != nil {
		resp.ReasonCode = strings.TrimSpace(tx.StsRsnInf.Rsn.Cd)
		if resp.ReasonCode == "" {
			resp.ReasonCode = strings.TrimSpace(tx.StsRsnInf.Rsn.Prtry)
		}
		resp.ReasonInfo = strings.TrimSpace(tx.StsRsnInf.AddtlInf)
	}
	if tx.OrgnlTxRef.Amt != nil && tx.OrgnlTxRef.Amt.InstdAmt != nil {
		amt := tx.OrgnlTxRef.Amt.InstdAmt
		cents, err := pain014ParseAmount(amt.Value)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(amt.Ccy) == "" {
			return nil, fmt.Errorf("%w: empty original amount currency", ErrPain014Parse)
		}
		currency, err := normalizeCurrency(amt.Ccy)
		if err != nil {
			return nil, fmt.Errorf("%w: original currency %q", ErrPain014Parse, amt.Ccy)
		}
		resp.HasAmount = true
		resp.AmountCents = cents
		resp.Currency = currency
	}
	if tx.PmtCondSts != nil && tx.PmtCondSts.AccptdAmt != nil {
		amt := tx.PmtCondSts.AccptdAmt
		cents, err := pain014ParseAmount(amt.Value)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(amt.Ccy) == "" {
			return nil, fmt.Errorf("%w: empty accepted amount currency", ErrPain014Parse)
		}
		currency, err := normalizeCurrency(amt.Ccy)
		if err != nil {
			return nil, fmt.Errorf("%w: accepted currency %q", ErrPain014Parse, amt.Ccy)
		}
		resp.HasAcceptedAmount = true
		resp.AcceptedAmountCents = cents
		resp.AcceptedCurrency = currency
	}
	if err := validatePain014Response(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ApplyPain014Response applies a parsed inbound pain.014 debtor
// response: the inbound half of the request-for-payment lifecycle,
// the mirror of the ExportPain013 outbound request. ACCP pays the
// RfP through AcceptRfP (the payout posts debtor -> creditor through
// the full PostTransfer gate sequence); RJCT rejects it through
// RejectRfP. The response is associated with its RfP by
// OrgnlEndToEndId — the pain.013 EndToEndId is the RfP ID.
//
// Fail-closed, the AcceptRfP/RejectRfP gates: an unknown EndToEndId
// (ErrRfPNotFound), an expired RfP (ErrRfPExpired — the lazy expiry
// marks the record and audits rfp_expired first, the AcceptRfP
// convention), a paid/rejected RfP (ErrRfPNotIssued), and a stated
// amount or currency that differs from the registered request
// (ErrPain014AmountMismatch / ErrPain014CurrencyMismatch) are all
// refused with nothing booked. A payout that cannot post — frozen
// account, overdraft, daily limit — leaves the RfP issued and the
// PostTransfer error propagates, so no money moves on a failed
// accept and the response is not recorded: a funded retry of the
// same document still applies.
//
// Idempotency is the sender's GrpHdr MsgId: re-applying the same
// document returns the stored RfP with no second booking and no
// second `pain014_response_applied` audit event. A MsgId reused
// for a different response is refused with
// ErrPain014MsgIDConflict. A second, distinct response to an
// already-terminal RfP is refused by the RfP state machine itself.
//
// The audit event is emitted after the RfP leg commits, under a
// separately acquired write lock: AcceptRfP/RejectRfP own their
// locks internally, so the inbound direction marker cannot join
// their atomic commit zone — the RfP transition itself is already
// durable when the marker lands.
func (l *Ledger) ApplyPain014Response(resp *Pain014Response, actor string) (RfP, error) {
	if err := validatePain014Response(resp); err != nil {
		return RfP{}, err
	}
	if actor == "" {
		actor = "ApplyPain014Response"
	}
	// Replay check and state gates, under one lock: the RfP row
	// must still be issued (and unexpired) when the leg is called.
	l.mu.Lock()
	if rec, ok := l.pain014Responses[resp.MsgID]; ok {
		l.mu.Unlock()
		if rec.EndToEndID != resp.EndToEndID || rec.Response != resp.Response {
			return RfP{}, fmt.Errorf("%w: MsgId %q", ErrPain014MsgIDConflict, resp.MsgID)
		}
		return rec.RfP, nil
	}
	r, ok := l.rfps[resp.EndToEndID]
	if !ok {
		l.mu.Unlock()
		return RfP{}, fmt.Errorf("%w: %q", ErrRfPNotFound, resp.EndToEndID)
	}
	now := time.Now()
	if rfpExpiredLocked(r, now) {
		r.Status = RfPStatusExpired
		l.rfps[resp.EndToEndID] = r
		l.emitAudit(AuditEvent{
			Op:            "rfp_expired",
			Actor:         "ApplyPain014Response",
			TraceID:       resp.EndToEndID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Creditor, r.Debtor},
		})
		l.mu.Unlock()
		return RfP{}, fmt.Errorf("%w: rfp %q", ErrRfPExpired, resp.EndToEndID)
	}
	switch r.Status {
	case RfPStatusIssued:
		// proceed below
	case RfPStatusExpired:
		l.mu.Unlock()
		return RfP{}, fmt.Errorf("%w: rfp %q", ErrRfPExpired, resp.EndToEndID)
	default:
		l.mu.Unlock()
		return RfP{}, fmt.Errorf("%w: rfp %q status %q", ErrRfPNotIssued, resp.EndToEndID, r.Status)
	}
	if resp.HasAmount {
		if resp.AmountCents != r.AmountCents {
			l.mu.Unlock()
			return RfP{}, fmt.Errorf("%w: response %d vs rfp %q %d",
				ErrPain014AmountMismatch, resp.AmountCents, resp.EndToEndID, r.AmountCents)
		}
		if resp.Currency != r.Currency {
			l.mu.Unlock()
			return RfP{}, fmt.Errorf("%w: response %q vs rfp %q %q",
				ErrPain014CurrencyMismatch, resp.Currency, resp.EndToEndID, r.Currency)
		}
	}
	if resp.HasAcceptedAmount {
		if resp.AcceptedAmountCents != r.AmountCents {
			l.mu.Unlock()
			return RfP{}, fmt.Errorf("%w: accepted %d vs rfp %q %d",
				ErrPain014AmountMismatch, resp.AcceptedAmountCents, resp.EndToEndID, r.AmountCents)
		}
		if resp.AcceptedCurrency != r.Currency {
			l.mu.Unlock()
			return RfP{}, fmt.Errorf("%w: accepted %q vs rfp %q %q",
				ErrPain014CurrencyMismatch, resp.AcceptedCurrency, resp.EndToEndID, r.Currency)
		}
	}
	l.mu.Unlock()

	// The RfP legs own their locks: call them unlocked, then
	// record the replay entry and the inbound audit marker under
	// a fresh lock. A duplicate receipt means a racing twin (or a
	// local accept) already paid the RfP: if the twin recorded
	// this MsgId, the replay converges; otherwise the RfP was
	// already terminal and this distinct response is refused.
	var out RfP
	var transferID string
	var entryIDs []string
	if resp.Response == Pain014ResponseAccepted {
		receipt, err := l.AcceptRfP(context.Background(), resp.EndToEndID, actor)
		if err != nil {
			return RfP{}, err
		}
		if receipt.Duplicate {
			l.mu.Lock()
			rec, ok := l.pain014Responses[resp.MsgID]
			l.mu.Unlock()
			if !ok {
				return RfP{}, fmt.Errorf("%w: rfp %q status %q", ErrRfPNotIssued, resp.EndToEndID, RfPStatusPaid)
			}
			if rec.EndToEndID != resp.EndToEndID || rec.Response != resp.Response {
				return RfP{}, fmt.Errorf("%w: MsgId %q", ErrPain014MsgIDConflict, resp.MsgID)
			}
			return rec.RfP, nil
		}
		transferID = receipt.TransferID
		for _, e := range receipt.Entries {
			entryIDs = append(entryIDs, e.ID)
		}
	} else {
		r, err := l.RejectRfP(context.Background(), resp.EndToEndID, actor)
		if err != nil {
			return RfP{}, err
		}
		out = r
	}
	l.mu.Lock()
	out, ok = l.rfps[resp.EndToEndID]
	l.mu.Unlock()
	if !ok {
		// Unreachable: the RfP existed when the leg ran, and RfPs
		// are never deleted. Fail loud rather than returning a
		// zero record for a paid/rejected request.
		return RfP{}, fmt.Errorf("%w: %q", ErrRfPNotFound, resp.EndToEndID)
	}
	details := map[string]any{
		"msg_id":       resp.MsgID,
		"rfp_id":       resp.EndToEndID,
		"response":     resp.Response,
		"amount_cents": out.AmountCents,
		"currency":     out.Currency,
	}
	if transferID != "" {
		details["transfer_id"] = transferID
	}
	if resp.ReasonCode != "" {
		details["reason_code"] = resp.ReasonCode
	}
	if resp.ReasonInfo != "" {
		details["reason_info"] = resp.ReasonInfo
	}
	l.mu.Lock()
	l.pain014Responses[resp.MsgID] = pain014AppliedRecord{
		MsgID:      resp.MsgID,
		EndToEndID: resp.EndToEndID,
		Response:   resp.Response,
		TransferID: transferID,
		ReasonCode: resp.ReasonCode,
		RfP:        out,
	}
	l.emitAudit(AuditEvent{
		Op:            "pain014_response_applied",
		Actor:         actor,
		TraceID:       resp.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{out.Creditor, out.Debtor},
		Details:       details,
	})
	l.mu.Unlock()
	return out, nil
}
