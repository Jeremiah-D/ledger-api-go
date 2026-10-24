package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pacs.007.001.02 FI-to-FI Payment Reversal export (LG-198).
//
// ExportPacs007 renders a *settled* payout as an ISO 20022
// `pacs.007.001.02` `FIToFIPaymentReversal` — the reversal of a
// payment that already settled, initiated by the credit side (the
// party that received the funds asks for the settled payment to be
// undone). The original instruction is modelled as
// `pacs.008.001.02` (the FI-to-FI customer credit transfer the
// payout settles) in `OrgnlGrpInf`, the pacs002.go convention, and
// `OrgnlUETR` ties the reversal to the original payment.
//
// Orthogonal to its two neighbours, never overlapping either:
//
//   - pacs.004 (LG-169) is the *return* of settled funds: the
//     beneficiary sends the money back. pacs.007 is the *reversal*
//     of the settled payment itself: the instruction is undone, not
//     refunded as a new leg.
//   - camt.056 (LG-168) is the *request* to cancel an instruction
//     before settlement: the money never left. pacs.007 executes
//     the reversal after settlement: the money left and the
//     reversal undoes it.
//
// The reversal reason (RtrRsn) rides `RtrRsnInf/Rsn/Prtry`, the
// pacs002.go reason-passthrough convention, but the admitted
// vocabulary is the small set of real interbank reversal codes
// defined below: a reversal must say why, and the ledger refuses
// to learn a new reason code on the fly — that is an operator
// vocabulary decision, not a wire-format decision.
//
// MsgId is deterministic: `<uetr>/rvsl`. The export opens no
// record, so repeat exports of the same payout with the same reason
// carry the same MsgId; the MsgId is stable across identical
// exports rather than unique per call.
//
// Read-only: the payout and its gpi track are read as snapshot
// copies under the read lock — no version bump, no journal rows, no
// audit event — the same contract as ExportPacs004. The durable
// record of the reversal stays with the bank that issued the
// pacs.007; the ledger keeps no copy because a reversal report is
// a statement of fact, not an operator attestation. The reversal
// never drives the payout instruction, and it carries no party
// names: `OrgnlTxRef` references the original instruction by
// amount, currency, and end-to-end ID only.

// pacs.007 reversal reason codes (RtrRsn): the admitted interbank
// reversal-reason vocabulary. Real ISO external reversal-reason
// codes, each with its settlement meaning:
//
//	DUPL  duplicate payment: the reversed leg duplicates a payment
//	      already credited — the classic double-pay reversal.
//	AM09  wrong amount: the settled amount differs from the amount
//	      the instruction ordered.
//	FRAD  fraudulent payment: the settled payment is confirmed
//	      fraud and must be undone at the creditor's request.
//	CUST  reversed at the creditor's request: the beneficiary
//	      asks for the settled payment to be undone (e.g. an
//	      overpayment they cannot keep).
const (
	Pacs007ReversalDuplicate       = "DUPL"
	Pacs007ReversalWrongAmount     = "AM09"
	Pacs007ReversalFraudulent      = "FRAD"
	Pacs007ReversalCustomerRequest = "CUST"
)

// pacs.007 export errors.
var (
	// ErrPacs007UnknownUETR is returned by ExportPacs007 for a UETR
	// the ledger never tracked: a reversal can only address a
	// payment the ledger knows. Fail-closed, like the gpi query path.
	ErrPacs007UnknownUETR = errors.New("ledger: pacs.007 export: unknown gpi UETR")
	// ErrPacs007NotSettled is returned when the payout behind the
	// UETR is not settled: unsettled money cannot be reversed — the
	// camt.056 recall (see camt056.go) is the honest path before
	// settlement.
	ErrPacs007NotSettled = errors.New("ledger: pacs.007 export: payout not settled")
	// ErrPacs007ReasonRequired is returned when no reversal reason
	// code is supplied: a reversal must say why, the gpi RJCT
	// convention.
	ErrPacs007ReasonRequired = errors.New("ledger: pacs.007 export requires a reversal reason code")
	// ErrPacs007InvalidReason is returned when the reversal reason
	// code is outside the admitted vocabulary: the machine only
	// emits real interbank codes, it does not invent them.
	ErrPacs007InvalidReason = errors.New("ledger: pacs.007 export: unknown reversal reason code")
)

// pacs007ValidReason reports whether code is in the admitted
// pacs.007 reversal-reason vocabulary.
func pacs007ValidReason(code string) bool {
	switch code {
	case Pacs007ReversalDuplicate,
		Pacs007ReversalWrongAmount,
		Pacs007ReversalFraudulent,
		Pacs007ReversalCustomerRequest:
		return true
	}
	return false
}

// pacs007MsgID builds the deterministic reversal MsgId:
// "<uetr>/rvsl". The export opens no record, so the MsgId is
// stable across repeat exports of the same payout — a statement of
// fact, not an attempt sequence.
func pacs007MsgID(uetr string) string {
	return uetr + "/rvsl"
}

// pacs.007 wire structs: Document/FIToFIPmtRvsl. The namespace
// rides the Document element, the camt053_export.go convention, so
// real pacs.007 readers accept the document.
type pacs007Document struct {
	XMLName  xml.Name    `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.007.001.02 Document"`
	Reversal pacs007Rvsl `xml:"FIToFIPmtRvsl"`
}

type pacs007Rvsl struct {
	GrpHdr      pacs007GrpHdr      `xml:"GrpHdr"`
	OrgnlGrpInf pacs007OrgnlGrpInf `xml:"OrgnlGrpInf"`
	TxInf       pacs007TxInf       `xml:"TxInf"`
}

type pacs007GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pacs007OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type pacs007TxInf struct {
	RvslID          string            `xml:"RvslId"`
	OrgnlEndToEndID string            `xml:"OrgnlEndToEndId"`
	OrgnlUETR       string            `xml:"OrgnlUETR"`
	RvsdAmt         pacs007Amt        `xml:"RvsdIntrBkSttlmAmt"`
	RtrRsnInf       pacs007RtrRsnInf  `xml:"RtrRsnInf"`
	OrgnlTxRef      pacs007OrgnlTxRef `xml:"OrgnlTxRef"`
}

type pacs007Amt struct {
	// Ccy rides the amount element as an attribute, the pacs.008
	// intrBkSttlmAmt convention.
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pacs007RtrRsnInf struct {
	Rsn *pacs007Rsn `xml:"Rsn"`
}

type pacs007Rsn struct {
	// Prtry carries the reversal reason code verbatim: reversal
	// reasons are the bank's vocabulary — the pacs002.go StsRsnInf
	// convention — but unlike the gpi passthrough the vocabulary
	// is the closed admitted set above.
	Prtry string `xml:"Prtry"`
}

// pacs007OrgnlTxRef references the original instruction without
// party data: the amount, currency, and end-to-end ID are enough
// for the receiving bank to match the reversal to the settled
// payment, and no account holder name ever leaves the ledger in a
// reversal report.
type pacs007OrgnlTxRef struct {
	OrgnlAmt        pacs007Amt `xml:"OrgnlIntrBkSttlmAmt"`
	OrgnlEndToEndID string     `xml:"OrgnlEndToEndId"`
}

// ExportPacs007 renders the settled payout behind uetr as an ISO
// 20022 pacs.007.001.02 FIToFIPaymentReversal document, the
// bank-to-bank reversal of a settled cross-border payment,
// initiated by the credit side.
//
// The original instruction is located by UETR through the gpi track
// (the camt056.go convention): the track's PayoutID names the
// payout, and the payout's settled state, amount, currency and
// UETR feed the reversal. The operator supplies the reversal reason
// code (RtrRsn).
//
// Fail-closed: an unknown UETR, a payout that is not settled
// (draft/approved/submitted/cancelled/failed), an empty reason code,
// and a reason code outside the admitted vocabulary are all refused
// — never silently dropped or coerced.
//
// Read-only: the track and payout are read as snapshot copies under
// the read lock — no version bump, no journal rows, no audit event.
func (l *Ledger) ExportPacs007(uetr, reasonCode string) ([]byte, error) {
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode == "" {
		return nil, ErrPacs007ReasonRequired
	}
	if !pacs007ValidReason(reasonCode) {
		return nil, fmt.Errorf("%w: %q", ErrPacs007InvalidReason, reasonCode)
	}
	l.mu.RLock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrPacs007UnknownUETR, uetr)
	}
	p, ok := l.payouts[t.PayoutID]
	l.mu.RUnlock()
	if !ok {
		// Unreachable: tracks open only from CreatePayout, and
		// payouts are never deleted. Fail loud rather than
		// returning a ghost instruction's reversal.
		return nil, fmt.Errorf("%w: payout %q", ErrPayoutNotFound, t.PayoutID)
	}
	if p.Status != PayoutSettled {
		return nil, fmt.Errorf("%w: payout %q is %s", ErrPacs007NotSettled, p.ID, p.Status)
	}
	// p and t are value copies out of the maps: the render below
	// cannot mutate ledger state even by accident.
	doc := pacs007Document{
		Reversal: pacs007Rvsl{
			GrpHdr: pacs007GrpHdr{
				MsgID:   pacs007MsgID(p.UETR),
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
			},
			OrgnlGrpInf: pacs007OrgnlGrpInf{
				OrgnlMsgID:   p.ID,
				OrgnlMsgNmID: pacs002OrgnlMsgName,
			},
			TxInf: pacs007TxInf{
				RvslID:          pacs007MsgID(p.UETR),
				OrgnlEndToEndID: p.ID,
				OrgnlUETR:       p.UETR,
				RvsdAmt: pacs007Amt{
					Ccy:   p.Currency,
					Value: pain001Amount(p.AmountCents),
				},
				RtrRsnInf: pacs007RtrRsnInf{
					Rsn: &pacs007Rsn{Prtry: reasonCode},
				},
				OrgnlTxRef: pacs007OrgnlTxRef{
					OrgnlAmt: pacs007Amt{
						Ccy:   p.Currency,
						Value: pain001Amount(p.AmountCents),
					},
					OrgnlEndToEndID: p.ID,
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPacs007InvalidReason, err)
	}
	return append([]byte(xml.Header), body...), nil
}
