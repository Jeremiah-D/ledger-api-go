package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// pain.007 customer payment cancellation request export (LG-401,
// fintech: ISO 20022 客户侧撤销请求面).
//
// ExportPain007 is the customer-side counterpart of the pain.001
// payment instruction (see ExportPain001): where pain.001 *orders* a
// payment, pain.007 *asks the bank to cancel* one already sent — a
// pain.007.001.02 `CustomerPaymentCancellationRequest` naming the
// original instruction by its transfer ID (the pain.001 EndToEndId)
// and, when the ledger exported it, by its original MsgId.
//
// Structure:
//
//	Document (xmlns pain.007.001.02)
//	  CstmrPmtCxlReq
//	    GrpHdr: MsgId, CreDtTm, InitgPty/Nm LEDGER
//	    OrgnlGrpInfAndSts (only when the original MsgId is known):
//	      OrgnlMsgId, OrgnlMsgNmId pain.001.001.03
//	    OrgnlPmtInfAndSts:
//	      OrgnlPmtInfId (only when the original MsgId is known:
//	        the pain.001 PmtInfId derivation, see below)
//	      TxInfAndSts:
//	        OrgnlEndToEndId (= transfer ID)
//	        CxlStsRsnInf/Rsn/Prtry (only when a reason is given)
//	        OrgnlTxRef: Amt/InstdAmt, DbtrAcct/IBAN, CdtrAcct/IBAN
//
// Semantics, made explicit:
//
//   - Read-only: the lookup holds the read lock, bumps no version,
//     books nothing, and appends no audit-chain link. The export
//     opens no record (the pacs007 precedent): repeat exports of
//     the same transfer carry the same MsgId, `<transferID>/cxl`.
//     Cancelling is a request to the bank, not a ledger state
//     change — the transfer's own state machine is untouched, and
//     whether the bank honours the request is confirmed by a
//     pain.002 / pacs.002 receipt, never by this document.
//   - Gates, in order: the transfer must be registered (unknown
//     transfer is a 404-class error); a pending-review transfer is
//     refused (422) — a frozen transfer never left the ledger, so
//     there is nothing for the bank to cancel, and the review
//     workflow is the correct way to stop it (the ExportPain001
//     "pending-review transfers never generate an instruction"
//     rule, applied to the cancellation side); a transfer whose
//     journal entry already carries a genuine full reversal is
//     refused with ErrAlreadyReversed (409, reused from
//     reversal.go) — the money is already undone in-ledger, so
//     asking the bank to cancel it would double-undo it. A partial
//     reversal does not block: the remainder is still live and
//     still cancellable at the bank.
//   - Fail-closed on identity, the pain.001 rule: debtor and
//     creditor are named by their registered IBANs; a missing IBAN
//     refuses the whole export with ErrMissingAccountIBAN rather
//     than emitting a forged identifier.
//   - Original MsgId: recovered from the pain.001 instruction
//     lifecycle registry (see pain001_status.go) — the most recent
//     instruction that exported this transfer. When the transfer
//     was never exported, the field is omitted, never invented:
//     the bank can still identify the payment by EndToEndId and
//     the OrgnlTxRef amount and accounts. The original PmtInfId is
//     re-derived the same way pain.001 builds it
//     (`<msgID>-<debtor>-<currency>`), and is omitted with the
//     MsgId.
//   - Reason: the cancellation reason rides `CxlStsRsnInf/Rsn/Prtry`
//     (the pacs007 convention) and must be one of the admitted
//     interbank cancellation-reason codes below — the machine only
//     emits real codes, it does not invent them. Empty means no
//     reason: the `Rsn` block is omitted, never defaulted.
//   - CreDtTm is the honest wall-clock creation time, so two
//     exports a second apart differ in that one line only — the
//     document is byte-identical modulo CreDtTm (the camt.053
//     HTTP-test precedent). Falsifying CreDtTm to force
//     byte-identity would mislabel when the request was created.

// pain.007 cancellation reason codes (CxlStsRsnInf): the admitted
// interbank cancellation-reason vocabulary — the same verified codes
// the pacs.007 reversal export admits (see pacs007.go), shared
// because a customer-side cancellation request and an interbank
// reversal describe the same underlying undo:
//
//	DUPL  duplicate payment: the instruction duplicates a payment
//	      already sent.
//	AM09  wrong amount: the settled amount differs from the amount
//	      the instruction ordered.
//	FRAD  fraudulent payment: the settled payment is confirmed
//	      fraud and must be undone.
//	CUST  cancelled at the customer's request: the operator asks
//	      the bank to stop a payment they no longer want sent.
const (
	Pain007ReasonDuplicate       = "DUPL"
	Pain007ReasonWrongAmount     = "AM09"
	Pain007ReasonFraudulent      = "FRAD"
	Pain007ReasonCustomerRequest = "CUST"
)

// pain.007 export errors.
var (
	// ErrInvalidPain007Export is returned by ExportPain007 and
	// RenderPain007Request when the request cannot name a valid
	// cancellation: an empty transfer ID, or an inverted window
	// (unused today, reserved for the options shape).
	ErrInvalidPain007Export = errors.New("ledger: invalid pain.007 export options")
	// ErrUnknownPain007Transfer is returned by ExportPain007 for a
	// transfer ID the ledger never registered: a cancellation can
	// only address a payment the ledger knows. Fail-closed, like
	// the gpi query path. It is a 404-class error.
	ErrUnknownPain007Transfer = errors.New("ledger: pain.007 export: unknown transfer")
	// ErrPain007PendingReview is returned by ExportPain007 when the
	// transfer is pending review: a frozen transfer never left the
	// ledger, so there is nothing for the bank to cancel. It is a
	// 422-class error: approve or reject the review first.
	ErrPain007PendingReview = errors.New("ledger: pain.007 export refused: transfer is pending review")
	// ErrInvalidPain007Reason is returned by ExportPain007 when the
	// reason code is outside the admitted vocabulary: the machine
	// only emits real interbank codes, it does not invent them.
	ErrInvalidPain007Reason = errors.New("ledger: pain.007 export: unknown cancellation reason code")
)

// pain007ValidReason reports whether code is in the admitted
// pain.007 cancellation-reason vocabulary.
func pain007ValidReason(code string) bool {
	switch code {
	case Pain007ReasonDuplicate,
		Pain007ReasonWrongAmount,
		Pain007ReasonFraudulent,
		Pain007ReasonCustomerRequest:
		return true
	}
	return false
}

// Pain007ExportOptions tunes ExportPain007.
//
// TransferID names the registered transfer to cancel (its pain.001
// EndToEndId). ReasonCode is the optional cancellation reason: empty
// omits the Rsn block entirely; anything else must be one of the
// admitted codes above.
type Pain007ExportOptions struct {
	TransferID string
	ReasonCode string
}

// pain007MsgID builds the deterministic cancellation MsgId:
// "<transferID>/cxl", the pacs007 "<uetr>/rvsl" precedent. The
// export opens no record, so the MsgId is stable across repeat
// exports of the same transfer — a statement of fact, not an attempt
// sequence.
func pain007MsgID(transferID string) string {
	return transferID + "/cxl"
}

// pain007OriginalMsgIDLocked finds the MsgId of the most recent
// pain.001 instruction that exported transferID, or "" when the
// transfer was never exported. Callers must hold l.mu (either lock
// suffices). The latest ExportedAt wins — that is the live
// instruction the bank holds; ties break on MsgId so the choice is
// deterministic.
func (l *Ledger) pain007OriginalMsgIDLocked(transferID string) string {
	best := ""
	var bestAt time.Time
	for _, rec := range l.painInstructions {
		found := false
		for _, tx := range rec.Txs {
			if tx.TxID == transferID {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if best == "" || rec.ExportedAt.After(bestAt) ||
			(rec.ExportedAt.Equal(bestAt) && rec.MsgID > best) {
			best, bestAt = rec.MsgID, rec.ExportedAt
		}
	}
	return best
}

// The pain007x* structs are the export-side XML model, the
// camt053_export.go convention: a strict writer carrying the
// pain.007.001.02 namespace on the root and omitting every empty
// element, so the emitted document is what a bank expects to receive.
type pain007xDocument struct {
	XMLName        xml.Name               `xml:"urn:iso:std:iso:20022:tech:xsd:pain.007.001.02 Document"`
	CstmrPmtCxlReq pain007xCstmrPmtCxlReq `xml:"CstmrPmtCxlReq"`
}

type pain007xCstmrPmtCxlReq struct {
	GrpHdr            pain007xGrpHdr             `xml:"GrpHdr"`
	OrgnlGrpInfAndSts *pain007xOrgnlGrpInfAndSts `xml:"OrgnlGrpInfAndSts,omitempty"`
	OrgnlPmtInfAndSts pain007xOrgnlPmtInfAndSts  `xml:"OrgnlPmtInfAndSts"`
}

type pain007xGrpHdr struct {
	MsgID    string           `xml:"MsgId"`
	CreDtTm  string           `xml:"CreDtTm"`
	InitgPty pain007xInitgPty `xml:"InitgPty"`
}

type pain007xInitgPty struct {
	Nm string `xml:"Nm"`
}

type pain007xOrgnlGrpInfAndSts struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type pain007xOrgnlPmtInfAndSts struct {
	OrgnlPmtInfID string              `xml:"OrgnlPmtInfId,omitempty"`
	TxInfAndSts   pain007xTxInfAndSts `xml:"TxInfAndSts"`
}

type pain007xTxInfAndSts struct {
	OrgnlEndToEndID string                `xml:"OrgnlEndToEndId"`
	CxlStsRsnInf    *pain007xCxlStsRsnInf `xml:"CxlStsRsnInf,omitempty"`
	OrgnlTxRef      pain007xOrgnlTxRef    `xml:"OrgnlTxRef"`
}

type pain007xCxlStsRsnInf struct {
	Rsn pain007xRsn `xml:"Rsn"`
}

type pain007xRsn struct {
	Prtry string `xml:"Prtry"`
}

type pain007xOrgnlTxRef struct {
	Amt      pain007xAmt  `xml:"Amt"`
	DbtrAcct pain007xAcct `xml:"DbtrAcct"`
	CdtrAcct pain007xAcct `xml:"CdtrAcct"`
}

type pain007xAmt struct {
	InstdAmt pain007xInstdAmt `xml:"InstdAmt"`
}

type pain007xInstdAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pain007xAcct struct {
	ID pain007xAcctID `xml:"Id"`
}

type pain007xAcctID struct {
	IBAN string `xml:"IBAN"`
}

// RenderPain007Request renders the customer payment cancellation
// request for one transfer: GrpHdr (MsgId = msgID, CreDtTm = now),
// the optional original-group reference, and one
// OrgnlPmtInfAndSts/TxInfAndSts naming the transfer by EndToEndId
// with the original amount and accounts in OrgnlTxRef. The caller
// owns the identity and gate checks; this is the pure, testable
// renderer underneath ExportPain007.
func RenderPain007Request(pt PainTransfer, debtorIBAN, creditorIBAN AccountIBAN, msgID, orgnlMsgID, reasonCode string) ([]byte, error) {
	if strings.TrimSpace(pt.TransferID) == "" {
		return nil, fmt.Errorf("%w: transfer ID required", ErrInvalidPain007Export)
	}
	if strings.TrimSpace(msgID) == "" {
		return nil, fmt.Errorf("%w: message ID required", ErrInvalidPain007Export)
	}
	if reasonCode != "" && !pain007ValidReason(reasonCode) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPain007Reason, reasonCode)
	}
	if debtorIBAN.IBAN == "" || creditorIBAN.IBAN == "" {
		return nil, fmt.Errorf("%w: debtor and creditor IBANs required", ErrMissingAccountIBAN)
	}
	if pt.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: transfer amount must be positive", ErrInvalidPain007Export)
	}
	currency := pt.Currency
	if currency == "" {
		// Unreachable for registered transfers (PostTransfer
		// normalizes an empty currency to DefaultCurrency), but
		// fail-closed rather than inventing one.
		return nil, fmt.Errorf("%w: transfer has no currency", ErrInvalidPain007Export)
	}
	var grp *pain007xOrgnlGrpInfAndSts
	orgnlPmtInfID := ""
	if orgnlMsgID != "" {
		grp = &pain007xOrgnlGrpInfAndSts{
			OrgnlMsgID:   orgnlMsgID,
			OrgnlMsgNmID: "pain.001.001.03",
		}
		// The pain.001 PmtInfId derivation: "<msgID>-<debtor>-<currency>".
		orgnlPmtInfID = fmt.Sprintf("%s-%s-%s", orgnlMsgID, pt.From, currency)
	}
	var rsn *pain007xCxlStsRsnInf
	if reasonCode != "" {
		rsn = &pain007xCxlStsRsnInf{Rsn: pain007xRsn{Prtry: reasonCode}}
	}
	doc := pain007xDocument{
		CstmrPmtCxlReq: pain007xCstmrPmtCxlReq{
			GrpHdr: pain007xGrpHdr{
				MsgID:    msgID,
				CreDtTm:  time.Now().UTC().Format(time.RFC3339),
				InitgPty: pain007xInitgPty{Nm: "LEDGER"},
			},
			OrgnlGrpInfAndSts: grp,
			OrgnlPmtInfAndSts: pain007xOrgnlPmtInfAndSts{
				OrgnlPmtInfID: orgnlPmtInfID,
				TxInfAndSts: pain007xTxInfAndSts{
					OrgnlEndToEndID: pt.TransferID,
					CxlStsRsnInf:    rsn,
					OrgnlTxRef: pain007xOrgnlTxRef{
						Amt: pain007xAmt{InstdAmt: pain007xInstdAmt{
							Ccy:   currency,
							Value: pain001Amount(pt.AmountCents),
						}},
						DbtrAcct: pain007xAcct{ID: pain007xAcctID{IBAN: debtorIBAN.IBAN}},
						CdtrAcct: pain007xAcct{ID: pain007xAcctID{IBAN: creditorIBAN.IBAN}},
					},
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPain007Export, err)
	}
	return append([]byte(xml.Header), body...), nil
}

// ExportPain007 renders a pain.007 customer payment cancellation
// request for one registered transfer.
//
// The transfer must be registered, effective (not pending review),
// and not already reversed in-ledger; debtor and creditor must both
// carry registered IBANs. The original pain.001 MsgId is recovered
// from the instruction registry when the transfer was exported, and
// omitted otherwise — never invented.
//
// Read-only: the read lock is held for the whole lookup, so the
// request describes one consistent point in time. It bumps no
// version, books nothing, appends no audit-chain link, and opens no
// cancellation record.
func (l *Ledger) ExportPain007(opts Pain007ExportOptions) ([]byte, error) {
	if strings.TrimSpace(opts.TransferID) == "" {
		return nil, fmt.Errorf("%w: transfer ID required", ErrInvalidPain007Export)
	}
	if opts.ReasonCode != "" && !pain007ValidReason(opts.ReasonCode) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPain007Reason, opts.ReasonCode)
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	pt, ok := l.painTransfers[opts.TransferID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPain007Transfer, opts.TransferID)
	}
	// Pending-review transfers never touched balances and never
	// generated an instruction: there is nothing for the bank to
	// cancel. The review workflow is the correct way to stop one.
	if e, ok := l.entries[pt.TransferID]; ok && e.PendingReview {
		return nil, fmt.Errorf("%w: %q", ErrPain007PendingReview, opts.TransferID)
	}
	// A genuine full reversal already undid the money in-ledger;
	// asking the bank to cancel it as well would double-undo it.
	// Partial reversals leave the remainder live and do not block.
	if e, taken := l.entries[reversalIDFor(pt.TransferID)]; taken && isGenuineReversalOf(e, pt.TransferID) {
		return nil, fmt.Errorf("%w: %q", ErrAlreadyReversed, opts.TransferID)
	}
	debtorIBAN, ok := l.accountIBANs[pt.From]
	if !ok {
		return nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, pt.From)
	}
	creditorIBAN, ok := l.accountIBANs[pt.To]
	if !ok {
		return nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, pt.To)
	}
	orgnlMsgID := l.pain007OriginalMsgIDLocked(opts.TransferID)
	return RenderPain007Request(pt, debtorIBAN, creditorIBAN, pain007MsgID(opts.TransferID), orgnlMsgID, opts.ReasonCode)
}

// pain007ReasonCodes returns the admitted cancellation-reason
// vocabulary in canonical order, for documentation and tests.
func pain007ReasonCodes() []string {
	codes := []string{
		Pain007ReasonDuplicate,
		Pain007ReasonWrongAmount,
		Pain007ReasonFraudulent,
		Pain007ReasonCustomerRequest,
	}
	sort.Strings(codes)
	return codes
}
