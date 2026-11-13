package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.013.001.07 creditor payment activation request
// export (LG-407): the outbound half of the request-for-payment
// lifecycle's opening move.
//
// CreateRfP (see rfp.go, LG-171) registers the creditor's request
// in the ledger; ExportPain013 renders that registered request as
// the pain.013.001.07 `CreditorPaymentActivationRequest` the
// creditor sends (directly or through its bank) to ask the debtor
// to pay — the FedNow/RTP "request for payment" message. The two
// are orthogonal by construction:
//
//   - pain.013 asks for the payment (EndToEndId + Amt + XpryDt):
//     no money moves, no collection is ordered.
//   - AcceptRfP (rfp.go) is the debtor's acceptance: the payout
//     posts debtor -> creditor through the full PostTransfer gate
//     sequence — the money moves.
//   - pain.014 (left for a later round) is the debtor's response
//     to the request: this export generates only the request.
//
// Version note (checked 2026-11-13, not assumed): the registry
// knows pain.013.001.05 and .001.07; the .07 version is the
// FedNow/RTP one (CreditorPaymentActivationRequestV07, root
// <CdtrPmtActvtnReq>), confirmed by three sources that agree: the
// moov-io rtp20022 gen/pain_013_001_07 bindings, the IBM TCH
// pain.013.001.05/.001.07 mapper docs, and the TCH RTP Customer
// Display Requirements (pain.013.001.07 is the Request for Payment
// message). The export therefore renders .001.07.
//
// Structure, verified against the moov-io rtp20022
// gen/pain_013_001_07 XSD-generated bindings (CreditorPayment-
// ActivationRequestV07 = GrpHdr + PmtInf) and cross-checked against
// the full-XSD pain_013_001_05 binding (mgruener/iso20022tpl),
// which confirms the base-XSD shapes where the RTP profile prunes
// them:
//
//   - GrpHdr (GroupHeader78): MsgId [1..1], CreDtTm [1..1],
//     NbOfTxs [1..1], InitgPty [1..1].
//   - PmtInf (PaymentInstruction31), in XSD order: PmtInfId
//     [1..1], PmtMtd [1..1] (TRF, the only PaymentMethod7Code),
//     ReqdExctnDt [1..1], XpryDt [1..1], Dbtr [1..1], DbtrAcct
//     [1..1], DbtrAgt [1..1], CdtTrfTx [1..1]. UltmtDbtr is
//     [0..1] and is not rendered: the ledger holds no ultimate
//     debtor distinct from the debtor.
//   - CdtTrfTx (CreditTransferTransaction35), in XSD order:
//     PmtId [1..1] (EndToEndId [1..1]; InstrId/UETR [0..1],
//     not rendered), Amt [1..1] (AmountType4Choice, only
//     InstdAmt), ChrgBr [1..1], CdtrAgt [1..1], Cdtr [1..1],
//     CdtrAcct [1..1]. PmtCond, UltmtCdtr, InstrForCdtrAgt,
//     RltdRmtInf and RmtInf are [0..1]/[0..*] and are not
//     rendered: the ledger holds none of them.
//   - ReqdExctnDt and XpryDt are DateAndDateTime2Choice (a Dt or
//     a DtTm child), not bare dates: ReqdExctnDt renders <Dt>
//     (the RfP's creation date — see below) and XpryDt renders
//     <DtTm> (the RfP's full ExpiresAt timestamp).
//
// Two deliberate divergences from the moov RTP-profile binding,
// both documented rather than silently papered over:
//
//   - PmtTpInf is omitted from CdtTrfTx. The moov V07 binding
//     marks it [1..1], but the full-XSD V05 binding leaves it
//     [0..1], and the ledger asserts no scheme service level,
//     local instrument, or category purpose — inventing one
//     (say SvcLvl/Cd SDVA, a same-day-ACH code) for an RTP
//     request would forge scheme data. A bank-facing profile
//     that requires PmtTpInf can add it in a later round.
//   - Account and agent identification render the base-XSD
//     forms, not the RTP-profile prunings: <Id><IBAN> (the base
//     AccountIdentification4Choice carries IBAN; moov's RTP
//     profile keeps only Othr) and <FinInstnId><BIC> (the base
//     FinancialInstitutionIdentification18 carries BICFI;
//     moov's RTP profile keeps only ClrSysMmbId), following the
//     pain008/pain009 precedent in this repo.
//
// The agents are mandatory in V07 (DbtrAgt and CdtrAgt [1..1]),
// so both parties need a registered BIC as well as an IBAN: a
// missing IBAN refuses with ErrMissingAccountIBAN and a missing
// BIC with ErrMissingPain013AgentBIC, both 422-class state gaps
// (the pain.009 precedent — a state gap in the account, not a
// malformed parameter).
//
// ReqdExctnDt is the RfP's creation date (UTC). The request asks
// for execution on the date the creditor raised it, and deriving
// it from the RfP — rather than from the wall clock — keeps a
// repeat export byte-identical apart from CreDtTm.
//
// The state gate is issued-only: paid/rejected refuse with
// ErrRfPNotIssued and expired (lazy or swept) with ErrRfPExpired,
// both 422-class. Exporting is not accepting: the RfP stays
// issued and the debtor still answers through AcceptRfP/RejectRfP.
//
// MsgId is deterministic: `pain013-<rfp ID>`. The same RfP
// re-renders byte-identical apart from CreDtTm.
//
// The export is read-only: it takes the read lock, bumps no
// version, appends no chain link, and consumes no idempotency key.
// Like Reconcile and the pain.009 export, it still emits one
// structured audit event ("pain013_exported", details msg_id/
// rfp_id) so the compliance trail records every generated request.

// pain.013 export errors.
var (
	// ErrMissingPain013AgentBIC is returned by ExportPain013 when
	// the debtor's or creditor's registered IBAN carries no BIC.
	// pain.013.001.07 makes DbtrAgt and CdtrAgt mandatory, so a
	// message without both agents cannot be rendered faithfully —
	// the export is refused instead of emitting an agent block
	// with an invented identifier. It is a 422-class error.
	ErrMissingPain013AgentBIC = errors.New("ledger: pain.013 export refused: debtor and creditor agent BICs are required")
)

// pain013MsgID derives the deterministic message ID from the RfP —
// the pain009MsgID convention: the same request re-renders with
// the same ID, and a different RfP changes it.
func pain013MsgID(rfpID string) string {
	return "pain013-" + rfpID
}

// ExportPain013 renders an ISO 20022 pain.013.001.07 creditor
// payment activation request for one issued request for payment
// (see CreateRfP).
//
// Fail-closed: an unknown RfP, a paid or rejected RfP, an expired
// RfP, and a party without a registered IBAN or BIC are all
// refused. The export moves no funds and changes no RfP state.
//
// The export is read-only: it takes the read lock, bumps no
// version, appends no chain link, consumes no idempotency key, and
// mutates no RfP row. Like the pain.009 export, it still emits one
// structured audit event ("pain013_exported", details msg_id/
// rfp_id) so the compliance trail records every generated request.
func (l *Ledger) ExportPain013(rfpID string) ([]byte, error) {
	l.mu.RLock()
	r, ok := l.rfps[rfpID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrRfPNotFound, rfpID)
	}
	now := time.Now()
	// Lazy expiry, like AcceptRfP/RejectRfP: an issued RfP past
	// its expiry reads as expired. The export does not persist
	// the transition — the ExpireRfPs sweep owns that — it only
	// refuses to render a request whose deadline has passed.
	if rfpExpiredLocked(r, now) {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q", ErrRfPExpired, rfpID)
	}
	switch r.Status {
	case RfPStatusIssued:
		// proceed below
	case RfPStatusExpired:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q", ErrRfPExpired, rfpID)
	default:
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: rfp %q status %q", ErrRfPNotIssued, rfpID, r.Status)
	}
	debtorIBAN, ok := l.accountIBANs[r.Debtor]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, r.Debtor)
	}
	creditorIBAN, ok := l.accountIBANs[r.Creditor]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, r.Creditor)
	}
	if debtorIBAN.BIC == "" {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: debtor %q", ErrMissingPain013AgentBIC, r.Debtor)
	}
	if creditorIBAN.BIC == "" {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: creditor %q", ErrMissingPain013AgentBIC, r.Creditor)
	}

	msgID := pain013MsgID(r.ID)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.013.001.07">` + "\n")
	b.WriteString("<CdtrPmtActvtnReq>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("<NbOfTxs>1</NbOfTxs>\n")
	b.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n")
	b.WriteString("</GrpHdr>\n")
	b.WriteString("<PmtInf>\n")
	b.WriteString(fmt.Sprintf("<PmtInfId>%s</PmtInfId>\n", ofxEscape(msgID)))
	b.WriteString("<PmtMtd>TRF</PmtMtd>\n")
	b.WriteString(fmt.Sprintf("<ReqdExctnDt><Dt>%s</Dt></ReqdExctnDt>\n", r.CreatedAt.UTC().Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("<XpryDt><DtTm>%s</DtTm></XpryDt>\n", r.ExpiresAt.UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("<Dbtr><Nm>%s</Nm></Dbtr>\n", ofxEscape(string(r.Debtor))))
	b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", ofxEscape(debtorIBAN.IBAN)))
	b.WriteString(fmt.Sprintf("<DbtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></DbtrAgt>\n", ofxEscape(debtorIBAN.BIC)))
	b.WriteString("<CdtTrfTx>\n")
	b.WriteString(fmt.Sprintf("<PmtId><EndToEndId>%s</EndToEndId></PmtId>\n", ofxEscape(r.ID)))
	b.WriteString(fmt.Sprintf("<Amt><InstdAmt Ccy=\"%s\">%s</InstdAmt></Amt>\n",
		ofxEscape(r.Currency), pain001Amount(r.AmountCents)))
	b.WriteString("<ChrgBr>SLEV</ChrgBr>\n")
	b.WriteString(fmt.Sprintf("<CdtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></CdtrAgt>\n", ofxEscape(creditorIBAN.BIC)))
	b.WriteString(fmt.Sprintf("<Cdtr><Nm>%s</Nm></Cdtr>\n", ofxEscape(string(r.Creditor))))
	b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", ofxEscape(creditorIBAN.IBAN)))
	b.WriteString("</CdtTrfTx>\n")
	b.WriteString("</PmtInf>\n")
	b.WriteString("</CdtrPmtActvtnReq>\n")
	b.WriteString("</Document>\n")

	// Read-only, like the pain.009 export: the version brackets the
	// export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain013_exported",
		Actor:         "ExportPain013",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details: map[string]any{
			"msg_id": msgID,
			"rfp_id": rfpID,
		},
	})
	l.mu.RUnlock()
	return []byte(b.String()), nil
}
