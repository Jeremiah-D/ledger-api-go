package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ISO 20022 pacs.007.001.02 FI-to-FI payment reversal inbound
// processing (LG-411): the inbound half of the pacs.007 reversal
// lifecycle.
//
// ExportPacs007 (LG-198) renders a settled payout as an outbound
// reversal request; this module is the mirror — it consumes a
// pacs.007 the *bank* sent back, reporting that a payment that
// already settled has been reversed, and books the consequence. The
// two are orthogonal by construction:
//
//   - LG-198 exports: the ledger tells the bank "undo this settled
//     payment" (outbound, read-only, no ledger state).
//   - LG-411 imports: the bank tells the ledger "this settled
//     payment is undone" (inbound, state-changing: a reversal entry
//     plus a payout status move).
//
// The original payment is matched by OrgnlUETR, which joins straight
// to the gpi track (GetGPITrack) and its PayoutID; OrgnlGrpInf's
// OrgnlMsgId must be that track's payout's ID (the LG-409 OrgnlMsgId
// pairing gate), and OrgnlMsgNmId only accepts pacs.008.001.02
// (pacs002OrgnlMsgName, the export's convention). The admitted
// reversal-reason vocabulary is shared with the export
// (pacs007ValidReason): a reversal must say why in a code the ledger
// already understands; an unknown reason is a parse refusal, never a
// silent accept.
//
// ApplyPacs007Reversal is full-reversal only: both the
// RvsdIntrBkSttlmAmt and the OrgnlTxRef amount must equal the
// settled payout amount in the same currency. A partial reversal is
// a different commercial event and the ledger refuses to guess which
// one — it parks the reversal in the repair queue (LG-177) for an
// operator to resolve with the bank, exactly like an unmatched
// original. Both fail-closed paths return a typed error *and* the
// repair record ID, so the caller can surface the parking, never
// silently drop the bank's message.
//
// Repair records parked by this module are visibility parking, not
// resubmittable instructions: their Maker is deliberately empty, so
// the CreatePayout gate can never pass them — resubmitting a parked
// reversal must never fabricate a payout that never existed. The
// operator resolves them outside the ledger (with the bank), and
// the record expires via the repair TTL. The snapshot still carries
// the reversal's key data (original reference, amount, currency) so
// the operator can find and work it.
//
// Idempotency: a reversal MsgId applied once is recorded in the
// per-ledger applied-reversal registry (the repair.go registry
// pattern: package-level, keyed by *Ledger, lock order l.mu then the
// registry mutex). Re-applying the same MsgId is a no-op success
// returning the original reversal entry — the bank re-sending a
// reversal can never double-book. A *different* MsgId for an already
// reversed payout fails with ErrPacs007AlreadyReversed: the payment
// is undone, a second reversal has nothing left to reverse.
//
// The reversal entry mirrors the settle leg (debit clearing /
// credit payee, the submit leg's direction): settlement moved the
// payee credit out through clearing, the reversal puts it back. Its
// ID is deterministic, "payout/<id>/reversal", so even a replay
// through postLocked's idempotency key converges on the same row
// instead of double-booking.
//
// Status move: settled -> reversed, via the new PayoutReversed
// status. Only settled payouts can be reversed — money that never
// left cannot be undone (draft/approved/submitted refuse with
// ErrPacs007OriginalNotSettled), and terminal payouts (failed,
// cancelled, already reversed, or pacs.004-returned) refuse with
// their own errors. The gpi track is deliberately untouched:
// pacs.007 is a payout-level reversal, not a settlement-progress
// verdict, so it never advances the track (the LG-409 dividing
// line).
//
// Orthogonal to the neighbouring apply endpoints, never overlapping
// either:
//
//   - pacs.004 inbound (LG-206) is the *return* of settled funds:
//     settled -> returned, "payout/<id>/return". pacs.007 is the
//     *reversal* of the settled payment itself: settled ->
//     reversed, "payout/<id>/reversal", a different terminal state
//     and a different entry ID, so a return and a reversal can
//     never be confused.
//   - pacs.002 apply (LG-409) advances the gpi track and never
//     moves funds or touches the payout state machine; this module
//     moves the payout state machine and never touches the track.

// pacs.007 inbound errors.
var (
	// ErrPacs007Parse is returned by ParsePacs007 for malformed XML
	// or a document missing a required field: the bank sent
	// something the ledger cannot even read as a reversal.
	ErrPacs007Parse = errors.New("ledger: pacs.007 inbound: unparseable reversal document")
	// ErrPacs007UnexpectedMsgName is returned when OrgnlMsgNmId is
	// not "pacs.008.001.02": the reversal answers a different
	// message type, and applying it to a payout would misattribute
	// the bank's reversal.
	ErrPacs007UnexpectedMsgName = errors.New("ledger: pacs.007 reversal is not for a pacs.008 message")
	// ErrPacs007ForeignMsgID is returned when OrgnlMsgId is not the
	// ID of the payout the reversal's UETR names — either another
	// payout's ID, or an OrgnlEndToEndId naming another payout. A
	// reversal for another payment is a category error, not a
	// reversal.
	ErrPacs007ForeignMsgID = errors.New("ledger: pacs.007 reversal answers a message of another payment")
	// ErrPacs007OriginalNotSettled is returned when the named payout
	// is not settled: unsettled money cannot be reversed.
	ErrPacs007OriginalNotSettled = errors.New("ledger: pacs.007 inbound: original payout is not settled")
	// ErrPacs007AlreadyReversed is returned when the named payout is
	// already reversed by a different reversal: the payment is
	// undone, a second reversal has nothing left to reverse.
	ErrPacs007AlreadyReversed = errors.New("ledger: pacs.007 inbound: payout already reversed")
	// ErrPacs007AmountMismatch is returned when the reversed amount
	// or currency differs from the settled payout: a partial
	// reversal is a different commercial event, parked in the repair
	// queue for operator resolution, never coerced to a full
	// reversal.
	ErrPacs007AmountMismatch = errors.New("ledger: pacs.007 inbound: reversal amount differs from settled payout")
)

// Pacs007Reversal is one parsed inbound pacs.007 payment reversal:
// the reversal's identity, the original payment it addresses, the
// amount being undone, and why.
type Pacs007Reversal struct {
	// MsgID is the reversal's GrpHdr MsgId: the idempotency key for
	// ApplyPacs007Reversal.
	MsgID string `json:"msg_id"`
	// OrgnlMsgID and OrgnlMsgNmID name the original message group
	// (the pacs.008 the payout settled, per the LG-198 export):
	// OrgnlMsgId is the payout ID itself.
	OrgnlMsgID   string `json:"orgnl_msg_id"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id"`
	// UETR is the original payment's gpi UETR: the join key to the
	// gpi track and its payout.
	UETR string `json:"uetr"`
	// ReversedAmountCents is the RvsdIntrBkSttlmAmt in minor units.
	ReversedAmountCents int64 `json:"reversed_amount_cents"`
	// Currency is the RvsdIntrBkSttlmAmt currency, normalized to a
	// 3-letter uppercase ISO 4217 code.
	Currency string `json:"currency"`
	// OrgnlEndToEndID names the original payment: the payout ID, per
	// the LG-198 export convention. It is cross-checked against the
	// payout when present, never used as the join key.
	OrgnlEndToEndID string `json:"orgnl_end_to_end_id,omitempty"`
	// OrgnlTxRefAmountCents and OrgnlTxRefCurrency are the
	// OrgnlTxRef's original amount: the second amount that must
	// equal the settled payout, so a reversal whose reference
	// disagrees with its own reversed amount cannot apply.
	OrgnlTxRefAmountCents int64  `json:"orgnl_tx_ref_amount_cents"`
	OrgnlTxRefCurrency    string `json:"orgnl_tx_ref_currency"`
	// ReasonCode is the RtrRsnInf reason, one of the admitted
	// pacs007ValidReason codes.
	ReasonCode string `json:"reason_code"`
}

// Pacs007ReversalResult is the outcome of ApplyPacs007Reversal.
type Pacs007ReversalResult struct {
	// Applied reports whether this call booked the reversal. False
	// on both the duplicate no-op path and the error paths.
	Applied bool `json:"applied"`
	// Duplicate reports whether the reversal was already applied
	// under the same MsgId: a no-op success, not an error.
	Duplicate bool `json:"duplicate"`
	// PayoutID is the payout the reversal addressed.
	PayoutID string `json:"payout_id,omitempty"`
	// ReversalEntryID is the booked reversal journal row
	// ("payout/<id>/reversal"), set on the applied and duplicate
	// paths.
	ReversalEntryID string `json:"reversal_entry_id,omitempty"`
	// RepairID is set on the parked fail-closed paths (unknown
	// original, amount mismatch): the repair-queue record holding
	// the reversal for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
}

// pacs007ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units, the pacs004ParseAmount rules: the integer part
// holds at most 12 digits; the fraction holds at most two digits and
// is right-padded ("5" means 50 cents); the value must be positive.
// Anything else is a parse refusal — a reversal amount the ledger
// cannot read exactly is never rounded into a booking.
func pacs007ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPacs007Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPacs007Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs007Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPacs007Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs007Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPacs007Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPacs007Parse, body)
	}
	return cents, nil
}

// validatePacs007Reversal fail-closes on a reversal that
// ParsePacs007 would refuse, so ApplyPacs007Reversal stays safe on
// hand-built structs too: the parse gate and the apply gate share one
// check, never a copy.
func validatePacs007Reversal(rvsl *Pacs007Reversal) error {
	if rvsl == nil {
		return fmt.Errorf("%w: nil reversal", ErrPacs007Parse)
	}
	if strings.TrimSpace(rvsl.MsgID) == "" {
		return fmt.Errorf("%w: missing MsgId", ErrPacs007Parse)
	}
	if strings.TrimSpace(rvsl.OrgnlMsgID) == "" {
		return fmt.Errorf("%w: missing OrgnlMsgId", ErrPacs007Parse)
	}
	if strings.TrimSpace(rvsl.UETR) == "" {
		return fmt.Errorf("%w: missing OrgnlUETR", ErrPacs007Parse)
	}
	if rvsl.ReversedAmountCents <= 0 {
		return fmt.Errorf("%w: non-positive reversal amount", ErrPacs007Parse)
	}
	if _, err := normalizeCurrency(rvsl.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrPacs007Parse, rvsl.Currency)
	}
	if rvsl.OrgnlTxRefAmountCents <= 0 {
		return fmt.Errorf("%w: non-positive OrgnlTxRef amount", ErrPacs007Parse)
	}
	if _, err := normalizeCurrency(rvsl.OrgnlTxRefCurrency); err != nil {
		return fmt.Errorf("%w: OrgnlTxRef currency %q", ErrPacs007Parse, rvsl.OrgnlTxRefCurrency)
	}
	if !pacs007ValidReason(rvsl.ReasonCode) {
		return fmt.Errorf("%w: reason %q", ErrPacs007Parse, rvsl.ReasonCode)
	}
	return nil
}

// ParsePacs007 parses one inbound pacs.007 payment reversal
// document into a Pacs007Reversal. It reuses the export wire struct
// (see pacs007.go): the document carries FIToFIPmtRvsl under the
// pacs.007.001.02 namespace, OrgnlMsgNmId == "pacs.008.001.02", and
// the reversal reason in RtrRsnInf/Rsn/Prtry. It validates the whole
// document before returning anything: a missing MsgId, a missing
// original reference, an unreadable amount, a bad currency, or a
// reason outside the admitted vocabulary are all parse refusals —
// ApplyPacs007Reversal never sees a half-read reversal.
func ParsePacs007(data []byte) (*Pacs007Reversal, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrPacs007Parse)
	}
	var doc pacs007Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPacs007Parse, err)
	}
	rvsl := doc.Reversal
	if strings.TrimSpace(rvsl.GrpHdr.MsgID) == "" &&
		strings.TrimSpace(rvsl.OrgnlGrpInf.OrgnlMsgID) == "" &&
		strings.TrimSpace(rvsl.TxInf.OrgnlUETR) == "" {
		// Nothing answered: either the document carries no
		// FIToFIPmtRvsl at all or it is under a different
		// namespace than pacs.007.001.02.
		return nil, fmt.Errorf("%w: missing FIToFIPmtRvsl", ErrPacs007Parse)
	}
	msgID := strings.TrimSpace(rvsl.GrpHdr.MsgID)
	if msgID == "" {
		return nil, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs007Parse)
	}
	msgNmID := strings.TrimSpace(rvsl.OrgnlGrpInf.OrgnlMsgNmID)
	if msgNmID != pacs002OrgnlMsgName {
		return nil, fmt.Errorf("%w: got %q", ErrPacs007UnexpectedMsgName, rvsl.OrgnlGrpInf.OrgnlMsgNmID)
	}
	orgnlMsgID := strings.TrimSpace(rvsl.OrgnlGrpInf.OrgnlMsgID)
	if orgnlMsgID == "" {
		return nil, fmt.Errorf("%w: missing OrgnlMsgId", ErrPacs007Parse)
	}
	uetr := strings.TrimSpace(rvsl.TxInf.OrgnlUETR)
	if uetr == "" {
		return nil, fmt.Errorf("%w: missing OrgnlUETR", ErrPacs007Parse)
	}
	amount, err := pacs007ParseAmount(rvsl.TxInf.RvsdAmt.Value)
	if err != nil {
		return nil, err
	}
	currency, err := normalizeCurrency(rvsl.TxInf.RvsdAmt.Ccy)
	if err != nil {
		return nil, fmt.Errorf("%w: currency %q", ErrPacs007Parse, rvsl.TxInf.RvsdAmt.Ccy)
	}
	refAmount, err := pacs007ParseAmount(rvsl.TxInf.OrgnlTxRef.OrgnlAmt.Value)
	if err != nil {
		return nil, err
	}
	refCurrency, err := normalizeCurrency(rvsl.TxInf.OrgnlTxRef.OrgnlAmt.Ccy)
	if err != nil {
		return nil, fmt.Errorf("%w: OrgnlTxRef currency %q", ErrPacs007Parse, rvsl.TxInf.OrgnlTxRef.OrgnlAmt.Ccy)
	}
	reason := ""
	if rvsl.TxInf.RtrRsnInf.Rsn != nil {
		reason = strings.TrimSpace(rvsl.TxInf.RtrRsnInf.Rsn.Prtry)
	}
	if !pacs007ValidReason(reason) {
		return nil, fmt.Errorf("%w: reason %q", ErrPacs007Parse, reason)
	}
	return &Pacs007Reversal{
		MsgID:                 msgID,
		OrgnlMsgID:            orgnlMsgID,
		OrgnlMsgNmID:          msgNmID,
		UETR:                  uetr,
		ReversedAmountCents:   amount,
		Currency:              currency,
		OrgnlEndToEndID:       strings.TrimSpace(rvsl.TxInf.OrgnlEndToEndID),
		OrgnlTxRefAmountCents: refAmount,
		OrgnlTxRefCurrency:    refCurrency,
		ReasonCode:            reason,
	}, nil
}

// pacs007AppliedReversal is one applied reversal: the payout it
// reversed and the reversal row it booked.
type pacs007AppliedReversal struct {
	PayoutID        string
	ReversalEntryID string
}

// pacs007AppliedReversals is the per-ledger applied-reversal
// registry (see the package comment): the idempotency record for
// ApplyPacs007Reversal. Callers must hold l.mu before taking
// pacs007AppliedReversals.Mutex — the repair.go lock order.
var pacs007AppliedReversals = struct {
	sync.Mutex
	m map[*Ledger]map[string]pacs007AppliedReversal
}{m: make(map[*Ledger]map[string]pacs007AppliedReversal)}

// pacs007AppliedForLocked returns l's applied-reversal map, creating
// it on first use. Callers must hold l.mu and
// pacs007AppliedReversals.Mutex.
func pacs007AppliedForLocked(l *Ledger) map[string]pacs007AppliedReversal {
	applied, ok := pacs007AppliedReversals.m[l]
	if !ok {
		applied = make(map[string]pacs007AppliedReversal)
		pacs007AppliedReversals.m[l] = applied
	}
	return applied
}

// payoutReversalEntryID builds the deterministic reversal entry ID:
// "payout/<id>/reversal", the payoutReturnEntryID convention. One
// payout admits at most one reversal, so at most one reversal row
// exists; a replayed apply converges on it instead of double-booking.
func payoutReversalEntryID(id string) string {
	return "payout/" + id + "/reversal"
}

// parkPacs007Reversal parks a fail-closed reversal in the repair
// queue and returns the record ID. The snapshot's Maker is
// deliberately empty (see the package comment): the record is
// operator visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's message. The snapshot names
// the reversal's OrgnlMsgId — the payout ID the bank claims — so the
// operator can find the payment the reversal was meant for.
func (l *Ledger) parkPacs007Reversal(rvsl *Pacs007Reversal, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          rvsl.OrgnlMsgID,
		AmountCents: rvsl.ReversedAmountCents,
		Currency:    rvsl.Currency,
		Destination: "pacs007-reversal:" + rvsl.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyPacs007Reversal books one parsed inbound pacs.007 reversal:
// it matches the original payout by OrgnlUETR through the gpi track,
// cross-checks OrgnlMsgId against that track's payout, verifies the
// full reversal (both the reversed amount and the OrgnlTxRef amount,
// in the same currency, equal the settled payout), posts the
// reversal entry, and moves the payout settled -> reversed.
//
// The duplicate no-op path (same MsgId applied before) returns
// success with Duplicate set and no new booking. The parked
// fail-closed paths (unknown UETR, amount mismatch) return the typed
// error with RepairID set. The plain fail-closed paths (foreign
// MsgId, unsettled original, already reversed by another MsgId)
// return the typed error with no repair record: there is nothing for
// an operator to repair through the queue in those states.
//
// Locking: the duplicate read runs under the read lock; the booking
// section holds the write lock (postLocked, the status move, the
// registry write, and the audit event are one atomic section); the
// repair parking runs outside any held lock because EnqueueRepair
// takes l.mu itself.
func (l *Ledger) ApplyPacs007Reversal(rvsl *Pacs007Reversal, actor string) (Pacs007ReversalResult, error) {
	if err := validatePacs007Reversal(rvsl); err != nil {
		return Pacs007ReversalResult{}, err
	}
	if actor == "" {
		actor = "ApplyPacs007Reversal"
	}

	// Duplicate read: the same MsgId applied before is a no-op
	// success, whatever the payout's current state.
	l.mu.RLock()
	pacs007AppliedReversals.Lock()
	applied, dup := pacs007AppliedForLocked(l)[rvsl.MsgID]
	pacs007AppliedReversals.Unlock()
	l.mu.RUnlock()
	if dup {
		return Pacs007ReversalResult{
			Duplicate:       true,
			PayoutID:        applied.PayoutID,
			ReversalEntryID: applied.ReversalEntryID,
		}, nil
	}

	// The UETR is the join key: it names the gpi track, and the
	// track names the payout. An unknown UETR parks the reversal
	// for the operator — the bank is reversing a payment the ledger
	// never tracked — and the 404-typed error carries the repair
	// record with it.
	track, ok := l.GetGPITrack(rvsl.UETR)
	if !ok {
		repairID, err := l.parkPacs007Reversal(rvsl, RepairReasonUnknownRoute, actor)
		if err != nil {
			return Pacs007ReversalResult{}, fmt.Errorf("%w: UETR %q (repair parking failed: %v)", ErrGPIUnknownUETR, rvsl.UETR, err)
		}
		return Pacs007ReversalResult{RepairID: repairID}, fmt.Errorf("%w: UETR %q", ErrGPIUnknownUETR, rvsl.UETR)
	}
	// The MsgId is the cross-check: it must be the payout ID the
	// track names. A track always carries its payout (see
	// registerGPITrackLocked), so the expected ID is always
	// defined — an empty payout fails closed rather than matching
	// anything.
	if track.PayoutID == "" || rvsl.OrgnlMsgID != track.PayoutID {
		return Pacs007ReversalResult{}, fmt.Errorf("%w: %q is not the payout ID of UETR %q", ErrPacs007ForeignMsgID, rvsl.OrgnlMsgID, rvsl.UETR)
	}
	// The end-to-end reference is the second address check: when the
	// bank carries one, it must name the same payout. (The LG-198
	// export sets it to the payout ID, so a reversal of our own
	// export always carries it.)
	if rvsl.OrgnlEndToEndID != "" && rvsl.OrgnlEndToEndID != track.PayoutID {
		return Pacs007ReversalResult{}, fmt.Errorf("%w: OrgnlEndToEndId %q is not the payout ID of UETR %q", ErrPacs007ForeignMsgID, rvsl.OrgnlEndToEndID, rvsl.UETR)
	}
	p, ok := l.GetPayout(track.PayoutID)
	if !ok {
		// Unreachable: tracks open only from CreatePayout, and
		// payouts are never deleted. Fail loud rather than
		// reversing a ghost instruction.
		return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q", ErrPayoutNotFound, track.PayoutID)
	}
	if p.Status == PayoutReversed {
		return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q", ErrPacs007AlreadyReversed, p.ID)
	}
	if p.Status != PayoutSettled {
		return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q is %s", ErrPacs007OriginalNotSettled, p.ID, p.Status)
	}
	if p.AmountCents != rvsl.ReversedAmountCents || p.Currency != rvsl.Currency ||
		p.AmountCents != rvsl.OrgnlTxRefAmountCents || p.Currency != rvsl.OrgnlTxRefCurrency {
		repairID, err := l.parkPacs007Reversal(rvsl, RepairReasonInvalidFormat, actor)
		if err != nil {
			return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q settled %d %s, reversal %d %s / ref %d %s (repair parking failed: %v)", ErrPacs007AmountMismatch, p.ID, p.AmountCents, p.Currency, rvsl.ReversedAmountCents, rvsl.Currency, rvsl.OrgnlTxRefAmountCents, rvsl.OrgnlTxRefCurrency, err)
		}
		return Pacs007ReversalResult{RepairID: repairID}, fmt.Errorf("%w: payout %q settled %d %s, reversal %d %s / ref %d %s", ErrPacs007AmountMismatch, p.ID, p.AmountCents, p.Currency, rvsl.ReversedAmountCents, rvsl.Currency, rvsl.OrgnlTxRefAmountCents, rvsl.OrgnlTxRefCurrency)
	}

	// Booking section: re-check the duplicate and the status under
	// the write lock — a racing apply for the same MsgId, or a
	// racing state move, converges here instead of double-booking.
	// The registry mutex is held for the whole section (lock order
	// l.mu then the registry mutex, the repair.go order); nothing in
	// the post path takes the registry mutex.
	l.mu.Lock()
	defer l.mu.Unlock()
	pacs007AppliedReversals.Lock()
	defer pacs007AppliedReversals.Unlock()
	appliedMap := pacs007AppliedForLocked(l)
	if applied, dup := appliedMap[rvsl.MsgID]; dup {
		return Pacs007ReversalResult{
			Duplicate:       true,
			PayoutID:        applied.PayoutID,
			ReversalEntryID: applied.ReversalEntryID,
		}, nil
	}
	p, ok = l.payouts[rvsl.OrgnlMsgID]
	if !ok || p.Status != PayoutSettled {
		if !ok {
			return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q", ErrPayoutNotFound, rvsl.OrgnlMsgID)
		}
		if p.Status == PayoutReversed {
			return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q", ErrPacs007AlreadyReversed, p.ID)
		}
		return Pacs007ReversalResult{}, fmt.Errorf("%w: payout %q is %s", ErrPacs007OriginalNotSettled, p.ID, p.Status)
	}

	now := time.Now()
	entryID := payoutReversalEntryID(p.ID)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   l.payoutClearingAccount,
		CreditAccount:  p.PayeeAccount,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: entryID,
		Memo:           "payout:" + p.ID + ":reversal",
		CreatedAt:      now,
	}
	posted, _, err := l.postLocked(e, true)
	if err != nil {
		return Pacs007ReversalResult{}, err
	}
	p.Status = PayoutReversed
	p.ReversalEntryID = posted.ID
	l.payouts[p.ID] = p
	appliedMap[rvsl.MsgID] = pacs007AppliedReversal{PayoutID: p.ID, ReversalEntryID: posted.ID}
	l.emitAudit(AuditEvent{
		Op:            "pacs007_reversal_applied",
		Actor:         actor,
		TraceID:       rvsl.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount, l.payoutClearingAccount},
		Details: map[string]any{
			"msg_id":            rvsl.MsgID,
			"payout_id":         p.ID,
			"reversal_reason":   rvsl.ReasonCode,
			"amount_cents":      p.AmountCents,
			"currency":          p.Currency,
			"reversal_entry_id": posted.ID,
		},
	})
	return Pacs007ReversalResult{
		Applied:         true,
		PayoutID:        p.ID,
		ReversalEntryID: posted.ID,
	}, nil
}
