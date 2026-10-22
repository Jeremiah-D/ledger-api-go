package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pacs.004.001.02 Payment Return export (LG-169).
//
// ExportPacs004 renders a *settled* payout as an ISO 20022
// `pacs.004.001.02` `PaymentReturn` — the bank-to-bank return of funds
// message: the money left and the counterparty (or the ledger's
// operator) is sending it back. The original instruction is modelled
// as `pacs.008.001.02` (the FI-to-FI customer credit transfer the
// payout settles) in `OrgnlGrpInf`, the pacs002.go convention, and
// `OrgnlUETR` ties the return to the original payment — the UETR is
// the end-to-end key of the whole pacs chain.
//
// This is the settled-side mirror of the camt.056 cancellation
// (LG-168): camt.056 recalls an instruction *before* settlement, the
// money never left; pacs.004 returns funds *after* settlement, the
// money came back. The two messages are complementary, never
// overlapping — ExportPacs004 refuses any payout that is not
// settled (draft/approved/submitted/cancelled/failed are all refused
// fail-closed).
//
// The return reason (RtrRsn) rides `RtrRsnInf/Rsn/Prtry`, the
// pacs002.go reason-passthrough convention, but the admitted
// vocabulary is the small set of real interbank return codes defined
// below: a return must say why, and the ledger refuses to learn a new
// reason code on the fly — that is an operator vocabulary decision,
// not a wire-format decision.
//
// MsgId is deterministic: `<uetr>/rtr`. The export opens no record,
// so repeat exports of the same payout with the same reason carry the
// same MsgId; the MsgId is stable across identical exports rather
// than unique per call.
//
// Read-only: the payout and its gpi track are read as snapshot copies
// under the read lock — no version bump, no journal rows, no audit
// event — the same contract as ExportPacs002, ExportCAMT053 and
// GenerateStatement. The durable record of the return stays with the
// bank that issued the pacs.004; the ledger keeps no copy because a
// return report is a statement of fact, not an operator attestation —
// like the pain.002/pacs.002 reports, it never drives the payout
// instruction.

// pacs.004 return reason codes (RtrRsn): the admitted interbank
// return-reason vocabulary. Real ISO external return-reason codes,
// each with its settlement meaning:
//
//	DUPL  duplicate payment: the returned leg duplicates a payment
//	      already credited — the classic double-pay return.
//	AM09  wrong amount: the settled amount differs from the amount
//	      the instruction ordered.
//	AC03  invalid creditor account number: the beneficiary account is
//	      closed or mistyped — the funds cannot be applied, so they
//	      come back to the sender.
//	CUST  returned at the creditor's request: the beneficiary refused
//	      the funds (e.g. overpayment they do not want to keep).
const (
	Pacs004ReturnDuplicate       = "DUPL"
	Pacs004ReturnWrongAmount     = "AM09"
	Pacs004ReturnInvalidAccount  = "AC03"
	Pacs004ReturnCustomerRequest = "CUST"
)

// pacs.004 export errors.
var (
	// ErrPacs004UnknownUETR is returned by ExportPacs004 for a UETR
	// the ledger never tracked: a return can only address a payment
	// the ledger knows. Fail-closed, like the gpi query path.
	ErrPacs004UnknownUETR = errors.New("ledger: pacs.004 export: unknown gpi UETR")
	// ErrPacs004NotSettled is returned when the payout behind the
	// UETR is not settled: unsettled money cannot be returned — the
	// camt.056 recall (see camt056.go) is the honest path before
	// settlement.
	ErrPacs004NotSettled = errors.New("ledger: pacs.004 export: payout not settled")
	// ErrPacs004ReasonRequired is returned when no return reason
	// code is supplied: a return must say why, the gpi RJCT
	// convention.
	ErrPacs004ReasonRequired = errors.New("ledger: pacs.004 export requires a return reason code")
	// ErrPacs004InvalidReason is returned when the return reason
	// code is outside the admitted vocabulary: the machine only
	// emits real interbank codes, it does not invent them.
	ErrPacs004InvalidReason = errors.New("ledger: pacs.004 export: unknown return reason code")
)

// pacs004ValidReason reports whether code is in the admitted
// pacs.004 return-reason vocabulary.
func pacs004ValidReason(code string) bool {
	switch code {
	case Pacs004ReturnDuplicate,
		Pacs004ReturnWrongAmount,
		Pacs004ReturnInvalidAccount,
		Pacs004ReturnCustomerRequest:
		return true
	}
	return false
}

// pacs004MsgID builds the deterministic return MsgId: "<uetr>/rtr".
// The export opens no record, so the MsgId is stable across repeat
// exports of the same payout — a statement of fact, not an attempt
// sequence.
func pacs004MsgID(uetr string) string {
	return uetr + "/rtr"
}

// pacs.004 wire structs: Document/PmtRtr. The namespace rides the
// Document element, the camt053_export.go convention, so real
// pacs.004 readers accept the document.
type pacs004Document struct {
	XMLName xml.Name  `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.004.001.02 Document"`
	Return  pacs004Rtr `xml:"PmtRtr"`
}

type pacs004Rtr struct {
	GrpHdr      pacs004GrpHdr     `xml:"GrpHdr"`
	OrgnlGrpInf pacs004OrgnlGrpInf `xml:"OrgnlGrpInf"`
	TxInf       pacs004TxInf      `xml:"TxInf"`
}

type pacs004GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pacs004OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type pacs004TxInf struct {
	RtrdIntrBkSttlmAmt pacs004Amt       `xml:"RtrdIntrBkSttlmAmt"`
	OrgnlEndToEndID    string           `xml:"OrgnlEndToEndId"`
	OrgnlUETR          string           `xml:"OrgnlUETR"`
	RtrRsnInf          pacs004RtrRsnInf `xml:"RtrRsnInf"`
}

type pacs004Amt struct {
	// Ccy rides the amount element as an attribute, the pacs.008
	// intrBkSttlmAmt convention.
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pacs004RtrRsnInf struct {
	Rsn *pacs004Rsn `xml:"Rsn"`
}

type pacs004Rsn struct {
	// Prtry carries the return reason code verbatim: return reasons
	// are the bank's vocabulary — the pacs002.go StsRsnInf
	// convention — but unlike the gpi passthrough the vocabulary is
	// the closed admitted set above.
	Prtry string `xml:"Prtry"`
}

// ExportPacs004 renders the settled payout behind uetr as an ISO
// 20022 pacs.004.001.02 PaymentReturn document, the bank-to-bank
// return-of-funds message for a settled cross-border payment.
//
// The original instruction is located by UETR through the gpi track
// (the camt056.go convention): the track's PayoutID names the payout,
// and the payout's settled state, amount, currency and UETR feed the
// return. The operator supplies the return reason code (RtrRsn).
//
// Fail-closed: an unknown UETR, a payout that is not settled
// (draft/approved/submitted/cancelled/failed), an empty reason code,
// and a reason code outside the admitted vocabulary are all refused —
// never silently dropped or coerced.
//
// Read-only: the track and payout are read as snapshot copies under
// the read lock — no version bump, no journal rows, no audit event.
func (l *Ledger) ExportPacs004(uetr, reasonCode string) ([]byte, error) {
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode == "" {
		return nil, ErrPacs004ReasonRequired
	}
	if !pacs004ValidReason(reasonCode) {
		return nil, fmt.Errorf("%w: %q", ErrPacs004InvalidReason, reasonCode)
	}
	l.mu.RLock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrPacs004UnknownUETR, uetr)
	}
	p, ok := l.payouts[t.PayoutID]
	l.mu.RUnlock()
	if !ok {
		// Unreachable: tracks open only from CreatePayout, and
		// payouts are never deleted. Fail loud rather than
		// returning a ghost instruction's funds.
		return nil, fmt.Errorf("%w: payout %q", ErrPayoutNotFound, t.PayoutID)
	}
	if p.Status != PayoutSettled {
		return nil, fmt.Errorf("%w: payout %q is %s", ErrPacs004NotSettled, p.ID, p.Status)
	}
	// p and t are value copies out of the maps: the render below
	// cannot mutate ledger state even by accident.
	doc := pacs004Document{
		Return: pacs004Rtr{
			GrpHdr: pacs004GrpHdr{
				MsgID:   pacs004MsgID(p.UETR),
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
			},
			OrgnlGrpInf: pacs004OrgnlGrpInf{
				OrgnlMsgID:   p.ID,
				OrgnlMsgNmID: pacs002OrgnlMsgName,
			},
			TxInf: pacs004TxInf{
				RtrdIntrBkSttlmAmt: pacs004Amt{
					Ccy:   p.Currency,
					Value: pain001Amount(p.AmountCents),
				},
				OrgnlEndToEndID: p.ID,
				OrgnlUETR:       p.UETR,
				RtrRsnInf: pacs004RtrRsnInf{
					Rsn: &pacs004Rsn{Prtry: reasonCode},
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPacs004InvalidReason, err)
	}
	return append([]byte(xml.Header), body...), nil
}
