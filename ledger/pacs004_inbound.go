package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ISO 20022 pacs.004.001.10 Payment Return inbound processing
// (LG-206): the inbound half of the pacs.004 return lifecycle.
//
// ExportPacs004 (LG-169) renders a settled payout as an outbound
// return; this module is the mirror — it consumes a pacs.004 the
// *bank* sent back, reporting that funds previously settled out have
// come back, and books the consequence. The two are orthogonal by
// construction:
//
//   - LG-169 exports: the ledger tells the bank "these settled funds
//     are being returned" (outbound, read-only, no ledger state).
//   - LG-206 imports: the bank tells the ledger "these settled funds
//     came back" (inbound, state-changing: a reversal entry plus a
//     payout status move).
//
// The original payment is matched by OrgnlEndToEndId, which the
// LG-169 export sets to the payout ID — the same key the outbound
// side uses, so a return the ledger itself emitted round-trips onto
// the payout it names. The admitted return-reason vocabulary is
// shared with the export (pacs004ValidReason): a return must say why
// in a code the ledger already understands; an unknown reason is a
// parse refusal, never a silent accept.
//
// ApplyPacs004Return is full-return only: the returned amount must
// equal the settled payout amount in the same currency. A partial
// return is a different commercial event (a part-refund, a fee
// deduction, a dispute slice) and the ledger refuses to guess which
// one — it parks the return in the repair queue (LG-177) for an
// operator to resolve with the bank, exactly like an unmatched
// original. Both fail-closed paths return a typed error *and* the
// repair record ID, so the caller can surface the parking, never
// silently drop the bank's message.
//
// Repair records parked by this module are visibility parking, not
// resubmittable instructions: their Maker is deliberately empty, so
// the CreatePayout gate can never pass them — resubmitting a parked
// return must never fabricate a payout that never existed. The
// operator resolves them outside the ledger (with the bank), and the
// record expires via the repair TTL. The snapshot still carries the
// return's key data (original reference, amount, currency) so the
// operator can find and work it.
//
// Idempotency: a return MsgId applied once is recorded in the
// per-ledger applied-returns registry (the repair.go registry
// pattern: package-level, keyed by *Ledger, lock order l.mu then the
// registry mutex). Re-applying the same MsgId is a no-op success
// returning the original reversal entry — the bank re-sending a
// return can never double-book. A *different* MsgId for an already
// returned payout fails with ErrPacs004AlreadyReturned: the money is
// back, a second return has nothing left to reverse.
//
// The reversal entry mirrors the settle leg (debit clearing /
// credit payee, the submit leg's direction): settlement moved the
// payee credit out through clearing, the return puts it back. Its ID
// is deterministic, "payout/<id>/return", so even a replay through
// postLocked's idempotency key converges on the same row instead of
// double-booking.
//
// Status move: settled -> returned, via the new PayoutReturned
// status. Only settled payouts can be returned — money that never
// left cannot come back (draft/approved/submitted refuse with
// ErrPacs004OriginalNotSettled), and terminal payouts (failed,
// cancelled, already returned) refuse with their own errors.

// pacs.004 inbound errors.
var (
	// ErrPacs004Parse is returned by ParsePacs004 for malformed XML
	// or a document missing a required field: the bank sent
	// something the ledger cannot even read as a return.
	ErrPacs004Parse = errors.New("ledger: pacs.004 inbound: unparseable return document")
	// ErrPacs004UnknownOriginal is returned by ApplyPacs004Return
	// when OrgnlEndToEndId names no payout the ledger knows: a
	// return can only address a payment the ledger sent. The return
	// is parked in the repair queue, not dropped.
	ErrPacs004UnknownOriginal = errors.New("ledger: pacs.004 inbound: unknown original payment")
	// ErrPacs004OriginalNotSettled is returned when the named payout
	// is not settled: unsettled money cannot be returned.
	ErrPacs004OriginalNotSettled = errors.New("ledger: pacs.004 inbound: original payout is not settled")
	// ErrPacs004AlreadyReturned is returned when the named payout is
	// already returned by a different return: the money is back, a
	// second return has nothing left to reverse.
	ErrPacs004AlreadyReturned = errors.New("ledger: pacs.004 inbound: payout already returned")
	// ErrPacs004AmountMismatch is returned when the returned amount
	// or currency differs from the settled payout: a partial return
	// is a different commercial event, parked in the repair queue
	// for operator resolution, never coerced to a full return.
	ErrPacs004AmountMismatch = errors.New("ledger: pacs.004 inbound: return amount differs from settled payout")
)

// Pacs004Return is one parsed inbound pacs.004 PaymentReturn: the
// return's identity, the original payment it addresses, the amount
// coming back, and why.
type Pacs004Return struct {
	// MsgID is the return's GrpHdr MsgId: the idempotency key for
	// ApplyPacs004Return.
	MsgID string
	// OrgnlMsgID and OrgnlMsgNmID name the original message group
	// (the pacs.008 the payout settled, per the LG-169 export).
	OrgnlMsgID   string
	OrgnlMsgNmID string
	// ReturnedAmountCents is the RtrdIntrBkSttlmAmt in minor units.
	ReturnedAmountCents int64
	// Currency is the RtrdIntrBkSttlmAmt currency, normalized to a
	// 3-letter uppercase ISO 4217 code.
	Currency string
	// OrgnlEndToEndID names the original payment: the payout ID, per
	// the LG-169 export convention.
	OrgnlEndToEndID string
	// OrgnlUETR is the original payment's gpi UETR, when the bank
	// carries it. It is informational here — matching is by
	// OrgnlEndToEndId, the key the ledger controls.
	OrgnlUETR string
	// ReasonCode is the RtrRsnInf reason, one of the admitted
	// pacs004ValidReason codes.
	ReasonCode string
}

// Pacs004ReturnResult is the outcome of ApplyPacs004Return.
type Pacs004ReturnResult struct {
	// Applied reports whether this call booked the reversal. False
	// on both the duplicate no-op path and the error paths.
	Applied bool
	// Duplicate reports whether the return was already applied under
	// the same MsgId: a no-op success, not an error.
	Duplicate bool
	// PayoutID is the payout the return addressed.
	PayoutID string
	// ReversalEntryID is the booked reversal journal row
	// ("payout/<id>/return"), set on the applied and duplicate paths.
	ReversalEntryID string
	// RepairID is set on the parked fail-closed paths (unknown
	// original, amount mismatch): the repair-queue record holding
	// the return for operator resolution.
	RepairID string
}

// pacs004InboundDocument is the wire shape ParsePacs004 accepts.
// Element matching is by local name (the encoding/xml default), so
// both pacs.004.001.02 (the LG-169 export version) and
// pacs.004.001.10 (the version banks send) parse: the return
// semantics are identical, only the schema version differs.
type pacs004InboundDocument struct {
	XMLName xml.Name             `xml:"Document"`
	Return  pacs004InboundPmtRtr `xml:"PmtRtr"`
}

type pacs004InboundPmtRtr struct {
	GrpHdr      pacs004InboundGrpHdr      `xml:"GrpHdr"`
	OrgnlGrpInf pacs004InboundOrgnlGrpInf `xml:"OrgnlGrpInf"`
	TxInf       pacs004InboundTxInf       `xml:"TxInf"`
}

type pacs004InboundGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type pacs004InboundOrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type pacs004InboundTxInf struct {
	RtrdIntrBkSttlmAmt pacs004InboundAmt       `xml:"RtrdIntrBkSttlmAmt"`
	OrgnlEndToEndID    string                  `xml:"OrgnlEndToEndId"`
	OrgnlUETR          string                  `xml:"OrgnlUETR"`
	RtrRsnInf          pacs004InboundRtrRsnInf `xml:"RtrRsnInf"`
}

type pacs004InboundAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pacs004InboundRtrRsnInf struct {
	Rsn *pacs004InboundRsn `xml:"Rsn"`
}

type pacs004InboundRsn struct {
	Prtry string `xml:"Prtry"`
}

// pacs004ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units. The integer part holds at most 12 digits (the
// mt202ParseAmount cap); the fraction holds at most two digits and
// is right-padded ("5" means 50 cents); the value must be positive.
// Anything else is a parse refusal — a return amount the ledger
// cannot read exactly is never rounded into a booking.
func pacs004ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPacs004Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPacs004Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs004Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPacs004Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs004Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPacs004Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPacs004Parse, body)
	}
	return cents, nil
}

// validatePacs004Return fail-closes on a return that ParsePacs004
// would refuse, so ApplyPacs004Return stays safe on hand-built
// structs too: the parse gate and the apply gate share one check,
// never a copy.
func validatePacs004Return(ret Pacs004Return) error {
	if strings.TrimSpace(ret.MsgID) == "" {
		return fmt.Errorf("%w: missing MsgId", ErrPacs004Parse)
	}
	if strings.TrimSpace(ret.OrgnlEndToEndID) == "" {
		return fmt.Errorf("%w: missing OrgnlEndToEndId", ErrPacs004Parse)
	}
	if ret.ReturnedAmountCents <= 0 {
		return fmt.Errorf("%w: non-positive return amount", ErrPacs004Parse)
	}
	if _, err := normalizeCurrency(ret.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrPacs004Parse, ret.Currency)
	}
	if !pacs004ValidReason(ret.ReasonCode) {
		return fmt.Errorf("%w: reason %q", ErrPacs004Parse, ret.ReasonCode)
	}
	return nil
}

// ParsePacs004 parses one inbound pacs.004 PaymentReturn document
// into a Pacs004Return. It validates the whole document before
// returning anything: a missing MsgId, a missing original reference,
// an unreadable amount, a bad currency, or a reason outside the
// admitted vocabulary are all parse refusals — ApplyPacs004Return
// never sees a half-read return.
func ParsePacs004(data []byte) (Pacs004Return, error) {
	var doc pacs004InboundDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pacs004Return{}, fmt.Errorf("%w: %v", ErrPacs004Parse, err)
	}
	rtr := doc.Return
	if strings.TrimSpace(rtr.GrpHdr.MsgID) == "" {
		return Pacs004Return{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs004Parse)
	}
	if strings.TrimSpace(rtr.TxInf.OrgnlEndToEndID) == "" {
		return Pacs004Return{}, fmt.Errorf("%w: missing TxInf/OrgnlEndToEndId", ErrPacs004Parse)
	}
	amount, err := pacs004ParseAmount(rtr.TxInf.RtrdIntrBkSttlmAmt.Value)
	if err != nil {
		return Pacs004Return{}, err
	}
	currency, err := normalizeCurrency(rtr.TxInf.RtrdIntrBkSttlmAmt.Ccy)
	if err != nil {
		return Pacs004Return{}, fmt.Errorf("%w: currency %q", ErrPacs004Parse, rtr.TxInf.RtrdIntrBkSttlmAmt.Ccy)
	}
	reason := ""
	if rtr.TxInf.RtrRsnInf.Rsn != nil {
		reason = strings.TrimSpace(rtr.TxInf.RtrRsnInf.Rsn.Prtry)
	}
	if !pacs004ValidReason(reason) {
		return Pacs004Return{}, fmt.Errorf("%w: reason %q", ErrPacs004Parse, reason)
	}
	ret := Pacs004Return{
		MsgID:               strings.TrimSpace(rtr.GrpHdr.MsgID),
		OrgnlMsgID:          strings.TrimSpace(rtr.OrgnlGrpInf.OrgnlMsgID),
		OrgnlMsgNmID:        strings.TrimSpace(rtr.OrgnlGrpInf.OrgnlMsgNmID),
		ReturnedAmountCents: amount,
		Currency:            currency,
		OrgnlEndToEndID:     strings.TrimSpace(rtr.TxInf.OrgnlEndToEndID),
		OrgnlUETR:           strings.TrimSpace(rtr.TxInf.OrgnlUETR),
		ReasonCode:          reason,
	}
	return ret, nil
}

// pacs004AppliedReturn is one applied return: the payout it reversed
// and the reversal row it booked.
type pacs004AppliedReturn struct {
	PayoutID        string
	ReversalEntryID string
}

// pacs004AppliedReturns is the per-ledger applied-return registry
// (see the package comment): the idempotency record for
// ApplyPacs004Return. Callers must hold l.mu before taking
// pacs004AppliedReturns.Mutex — the repair.go lock order.
var pacs004AppliedReturns = struct {
	sync.Mutex
	m map[*Ledger]map[string]pacs004AppliedReturn
}{m: make(map[*Ledger]map[string]pacs004AppliedReturn)}

// pacs004AppliedForLocked returns l's applied-return map, creating it
// on first use. Callers must hold l.mu and
// pacs004AppliedReturns.Mutex.
func pacs004AppliedForLocked(l *Ledger) map[string]pacs004AppliedReturn {
	applied, ok := pacs004AppliedReturns.m[l]
	if !ok {
		applied = make(map[string]pacs004AppliedReturn)
		pacs004AppliedReturns.m[l] = applied
	}
	return applied
}

// payoutReturnEntryID builds the deterministic reversal entry ID:
// "payout/<id>/return", the payoutSettlementEntryID convention. One
// payout admits at most one return, so at most one reversal row
// exists; a replayed apply converges on it instead of double-booking.
func payoutReturnEntryID(id string) string {
	return "payout/" + id + "/return"
}

// parkPacs004Return parks a fail-closed return in the repair queue
// and returns the record ID. The snapshot's Maker is deliberately
// empty (see the package comment): the record is operator
// visibility, never a resubmittable instruction. A parking failure
// (a full queue) is returned so the caller can fail loud rather than
// silently drop the bank's message.
func (l *Ledger) parkPacs004Return(ret Pacs004Return, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          ret.OrgnlEndToEndID,
		AmountCents: ret.ReturnedAmountCents,
		Currency:    ret.Currency,
		Destination: "pacs004-return:" + ret.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyPacs004Return books one parsed inbound pacs.004 return: it
// matches the original payout by OrgnlEndToEndId, verifies the full
// return (amount and currency equal the settled payout), posts the
// reversal entry, and moves the payout settled -> returned.
//
// The duplicate no-op path (same MsgId applied before) returns
// success with Duplicate set and no new booking. The parked
// fail-closed paths (unknown original, amount mismatch) return the
// typed error with RepairID set. The plain fail-closed paths
// (unsettled original, already returned by another MsgId) return the
// typed error with no repair record: there is nothing for an
// operator to repair through the queue in those states.
//
// Locking: the duplicate read runs under the read lock; the booking
// section holds the write lock (postLocked, the status move, the
// registry write, and the audit event are one atomic section); the
// repair parking runs outside any held lock because EnqueueRepair
// takes l.mu itself.
func (l *Ledger) ApplyPacs004Return(ret Pacs004Return, actor string) (Pacs004ReturnResult, error) {
	if err := validatePacs004Return(ret); err != nil {
		return Pacs004ReturnResult{}, err
	}
	if actor == "" {
		actor = "ApplyPacs004Return"
	}

	// Duplicate read: the same MsgId applied before is a no-op
	// success, whatever the payout's current state.
	l.mu.RLock()
	pacs004AppliedReturns.Lock()
	applied, dup := pacs004AppliedForLocked(l)[ret.MsgID]
	pacs004AppliedReturns.Unlock()
	p, payoutOK := l.payouts[ret.OrgnlEndToEndID]
	l.mu.RUnlock()
	if dup {
		return Pacs004ReturnResult{
			Duplicate:       true,
			PayoutID:        applied.PayoutID,
			ReversalEntryID: applied.ReversalEntryID,
		}, nil
	}
	if !payoutOK {
		repairID, err := l.parkPacs004Return(ret, RepairReasonUnknownRoute, actor)
		if err != nil {
			return Pacs004ReturnResult{}, fmt.Errorf("%w: original %q (repair parking failed: %v)", ErrPacs004UnknownOriginal, ret.OrgnlEndToEndID, err)
		}
		return Pacs004ReturnResult{RepairID: repairID}, fmt.Errorf("%w: original %q", ErrPacs004UnknownOriginal, ret.OrgnlEndToEndID)
	}
	if p.Status == PayoutReturned {
		return Pacs004ReturnResult{}, fmt.Errorf("%w: payout %q", ErrPacs004AlreadyReturned, p.ID)
	}
	if p.Status != PayoutSettled {
		return Pacs004ReturnResult{}, fmt.Errorf("%w: payout %q is %s", ErrPacs004OriginalNotSettled, p.ID, p.Status)
	}
	if p.AmountCents != ret.ReturnedAmountCents || p.Currency != ret.Currency {
		repairID, err := l.parkPacs004Return(ret, RepairReasonInvalidFormat, actor)
		if err != nil {
			return Pacs004ReturnResult{}, fmt.Errorf("%w: payout %q settled %d %s, return %d %s (repair parking failed: %v)", ErrPacs004AmountMismatch, p.ID, p.AmountCents, p.Currency, ret.ReturnedAmountCents, ret.Currency, err)
		}
		return Pacs004ReturnResult{RepairID: repairID}, fmt.Errorf("%w: payout %q settled %d %s, return %d %s", ErrPacs004AmountMismatch, p.ID, p.AmountCents, p.Currency, ret.ReturnedAmountCents, ret.Currency)
	}

	// Booking section: re-check the duplicate and the status under
	// the write lock — a racing apply for the same MsgId, or a
	// racing state move, converges here instead of double-booking.
	// The registry mutex is held for the whole section (lock order
	// l.mu then the registry mutex, the repair.go order); nothing in
	// the post path takes the registry mutex.
	l.mu.Lock()
	defer l.mu.Unlock()
	pacs004AppliedReturns.Lock()
	defer pacs004AppliedReturns.Unlock()
	appliedMap := pacs004AppliedForLocked(l)
	if applied, dup := appliedMap[ret.MsgID]; dup {
		return Pacs004ReturnResult{
			Duplicate:       true,
			PayoutID:        applied.PayoutID,
			ReversalEntryID: applied.ReversalEntryID,
		}, nil
	}
	p, ok := l.payouts[ret.OrgnlEndToEndID]
	if !ok || p.Status != PayoutSettled {
		if !ok {
			return Pacs004ReturnResult{}, fmt.Errorf("%w: original %q", ErrPacs004UnknownOriginal, ret.OrgnlEndToEndID)
		}
		if p.Status == PayoutReturned {
			return Pacs004ReturnResult{}, fmt.Errorf("%w: payout %q", ErrPacs004AlreadyReturned, p.ID)
		}
		return Pacs004ReturnResult{}, fmt.Errorf("%w: payout %q is %s", ErrPacs004OriginalNotSettled, p.ID, p.Status)
	}

	now := time.Now()
	entryID := payoutReturnEntryID(p.ID)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   l.payoutClearingAccount,
		CreditAccount:  p.PayeeAccount,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: entryID,
		Memo:           "payout:" + p.ID + ":return",
		CreatedAt:      now,
	}
	posted, _, err := l.postLocked(e, true)
	if err != nil {
		return Pacs004ReturnResult{}, err
	}
	p.Status = PayoutReturned
	p.ReturnEntryID = posted.ID
	l.payouts[p.ID] = p
	appliedMap[ret.MsgID] = pacs004AppliedReturn{PayoutID: p.ID, ReversalEntryID: posted.ID}
	l.emitAudit(AuditEvent{
		Op:            "pacs004_return_applied",
		Actor:         actor,
		TraceID:       ret.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount, l.payoutClearingAccount},
		Details: map[string]any{
			"msg_id":            ret.MsgID,
			"payout_id":         p.ID,
			"return_reason":     ret.ReasonCode,
			"amount_cents":      p.AmountCents,
			"currency":          p.Currency,
			"reversal_entry_id": posted.ID,
		},
	})
	return Pacs004ReturnResult{
		Applied:         true,
		PayoutID:        p.ID,
		ReversalEntryID: posted.ID,
	}, nil
}
