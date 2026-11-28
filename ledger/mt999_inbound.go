package ledger

import (
	"errors"
	"fmt"
	"time"
)

// SWIFT FIN MT999 free-format notification inbound processing
// (LG-463): the inbound half of the MT999 lifecycle. The ledger
// sent a correspondent bank a free-format cash-management note
// with an outbound MT999 (see mt999.go, LG-458); the MT999 coming
// back is the correspondent's reply, and ApplyMT999 registers it
// against what its :21: names — the question-answer chain's
// return leg, the MT999 twin of the MT195/MT196 loop (LG-460 /
// LG-462).
//
// The :21: anchors in three tiers, in order:
//
//  1. Our outbound MT999: the :21: is our outbound message's
//     :20:, looked up in mt999Exports — the correspondent
//     answers the free-format note we sent (the MT196 :21:
//     back-reference shape).
//  2. A payout: the :21: is a transfer/payout ID, looked up in
//     payouts — the :21: can also carry a transfer/payout ID
//     (see the mt999.go header note), the ApplyMT190
//     (mt190_inbound.go, LG-454) :21:→payout shape.
//  3. An MT202 transfer: the :21: is an MT202 :20:, looked up
//     in mt202Applied first (an inbound MT202 the ledger
//     applied, LG-442), then in mt202Exports (our outbound
//     MT202, LG-470) — the transfer half of the :21:'s
//     "transfer/payout ID" note (see the mt999.go header note),
//     the MT290 (LG-484) two-tier shape reused as the third
//     tier here. Either tier hit accepts the notification;
//     on an overlap the inbound tier wins, along the MT290
//     (a)/(b) order. A pure lookup: no amount, currency, or
//     date is checked — a free-format reply carries no
//     amount to check against.
//
// A :21: that matches none of the three tiers is fail-closed into the
// repair queue (RepairReasonUnknownRoute, the camt.055 shape,
// along ApplyMT292 and ApplyCAMT058: the returned result carries
// the repair ID alongside the ErrMT999UnknownReference error),
// and reported as a 422-class error. An empty :21: registers a
// standalone notification: a free-format note about nothing in
// particular is still a note (the mt999.go header note), so it
// needs no anchor at all.
//
// The notification is registered verbatim: the :79: narrative
// rides into the record exactly as received, never re-derived
// from live facts, because the notification is the
// correspondent's statement, not the ledger's own view of it. A
// later change to a payout's status or gpi track never rewrites
// a registered notification; a replay returns the stored
// verbatim notification. The :79: text is retrievable through
// the getter — it lives in the registry record, not in the audit
// event details.
//
// Nothing moves: no journal entry, no payout status change, no
// gpi advance, no version bump — registering a notification is
// read-only, and the audit event is the durable record, not a
// state change. The notification's audit event,
// `mt999_received` (details msg_id / related_reference /
// sender_lt, trace :20:), puts the :20: on the durable record,
// along the mt999_exported shape: no accounts, because a
// free-format note touches no ledger account.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): a replay with the same :21: and the same
// :79: narrative returns the stored outcome with Duplicate set,
// registering nothing twice and emitting no second audit event.
// A :20: reused for different terms is refused with
// ErrMT999MsgIDConflict — never silently overwritten. The
// registry check runs again under the write lock after the
// anchor lookup, so two concurrent applies of the same
// notification still record once.

// ErrMT999UnknownReference is returned by ApplyMT999 when the
// inbound notification's :21: names neither an outbound MT999
// export, nor a payout, nor an MT202 (inbound applied or
// outbound exported): the notification has no anchor, so it is
// parked in the repair queue fail-closed rather than dropped.
// It is a 422-class error.
var ErrMT999UnknownReference = errors.New("ledger: mt999 inbound: unknown :21: related reference")

// MT999ApplyResult is the outcome of ApplyMT999: the registered
// notification, verbatim. A parked notification (unknown :21:)
// carries only Reference, RelatedReference, Narrative, and
// RepairID, alongside the ErrMT999UnknownReference error.
type MT999ApplyResult struct {
	// Reference echoes the notification's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the :21: related reference, empty
	// when the notification carries none.
	RelatedReference string `json:"related_reference,omitempty"`
	// OutboundReference is the outbound MT999 the notification
	// answers — the :21: resolved against mt999Exports. Empty
	// when the notification anchors to a payout instead, or
	// carries no :21: at all.
	OutboundReference string `json:"outbound_reference,omitempty"`
	// PayoutID is the payout the notification is about — the
	// :21: resolved against the payouts. Empty when the
	// notification answers an outbound MT999 instead, or
	// carries no :21: at all.
	PayoutID string `json:"payout_id,omitempty"`
	// MT202Ref is the MT202 transfer the notification is about
	// — the :21: resolved against mt202Applied (an inbound
	// MT202 the ledger applied) or mt202Exports (our outbound
	// MT202). Empty when the notification anchors to an
	// outbound MT999 or a payout instead, or carries no :21:
	// at all.
	MT202Ref string `json:"mt202_ref,omitempty"`
	// Narrative is the :79: narrative, verbatim.
	Narrative []string `json:"narrative,omitempty"`
	// RepairID is the repair queue record for a parked
	// notification (unknown :21:). Empty on a registered
	// notification.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the notification was already
	// registered under the same :20: with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt999ReceivedRecord is the registry record for one registered
// MT999 notification, keyed by :20: in mt999Received: the
// message as received, its :21: anchor, when it was registered,
// and who registered it, so a replay returns the same outcome
// without registering twice.
type mt999ReceivedRecord struct {
	Message          MT999
	RelatedReference string
	ReceivedAt       time.Time
	Actor            string
	Result           MT999ApplyResult
}

// mt999ReceivedTermsEqual reports whether a stored notification
// record carries exactly the terms an inbound notification
// would apply: the same :21: related reference and the same
// :79: narrative, verbatim. The sender/receiver LTs are
// envelope, not terms, along the MT999 export precedent — a
// replay under different LTs still returns the first
// registration, because the :20: names one notification.
func mt999ReceivedTermsEqual(rec mt999ReceivedRecord, m MT999) bool {
	return mt999TermsEqual(mt999ExportRecord{Message: rec.Message}, m)
}

// mt999ReceivedLinesCopy returns a copy of lines, so a
// registered notification's slices can never be mutated through
// a returned result — the getter invariant, along the other
// registries.
func mt999ReceivedLinesCopy(lines []string) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// mt999ReceivedResultCopy returns a copy of res with its own
// narrative slice.
func mt999ReceivedResultCopy(res MT999ApplyResult) MT999ApplyResult {
	res.Narrative = mt999ReceivedLinesCopy(res.Narrative)
	return res
}

// parkMT999Notification parks a fail-closed inbound notification
// in the repair queue and returns the record ID. The snapshot
// names the anchor the notification tried to reach (:21:) as
// the instruction ID and the notification (:20:) as the
// destination, so an operator can find the notification from
// the record. The snapshot's Maker is deliberately empty (see
// the MT195 package comment): the record is operator
// visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's notification.
func (l *Ledger) parkMT999Notification(m MT999, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          m.RelatedReference,
		Destination: "mt999-inbound:" + m.TransactionReference,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT999 applies one parsed inbound SWIFT FIN MT999
// free-format notification: it registers the correspondent
// bank's notification against what its :21: names — first our
// outbound MT999's :20:, then a payout ID, then an MT202 :20:
// (inbound applied, then outbound exported) — verbatim, with the
// :79: narrative as received, never re-derived from live facts.
//
// An empty :21: registers a standalone notification with no
// anchor. An unknown :21: is ErrMT999UnknownReference, parked
// in the repair queue fail-closed in the camt.055 shape: the
// returned result carries the repair ID alongside the error. No
// journal entry is ever posted, no payout state is ever changed,
// no gpi track is ever advanced, and no version is ever bumped:
// a notification is evidence, not an instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same :21: and the same :79:
// narrative returns the stored outcome with Duplicate set,
// registering nothing twice and emitting no second audit event;
// a :20: reused for different terms is refused with
// ErrMT999MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the anchor reads run under the read lock; the
// :20: record and the inbound marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-apply still records once.
func (l *Ledger) ApplyMT999(m MT999, actor string) (MT999ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT999"
	}
	// The parse gate and the apply gate share one check; the
	// sender LT recorded in the audit event is the one the
	// message carries as received, not a re-normalized form —
	// ParseMT999 already returns the normalized LT verbatim,
	// and normalizing it a second time would append another
	// "X" (the mt999ParseLT note).
	if _, _, err := mt999Validate(m); err != nil {
		return MT999ApplyResult{}, err
	}

	// :20: replay: the notification was already registered
	// under this reference. Same terms is a no-op returning
	// the stored outcome; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt999Received[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt999ReceivedTermsEqual(rec, m) {
			return MT999ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT999MsgIDConflict, m.TransactionReference)
		}
		dup := mt999ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The :21: anchor, three tiers in order: our outbound MT999
	// first, then a payout, then an MT202 (inbound applied
	// before outbound exported, along the MT290 (a)/(b) order).
	// An empty :21: needs no anchor at all — a standalone
	// notification registers directly.
	res := MT999ApplyResult{
		Reference:        m.TransactionReference,
		RelatedReference: m.RelatedReference,
		Narrative:        mt999ReceivedLinesCopy(m.Narrative),
	}
	if m.RelatedReference != "" {
		l.mu.RLock()
		_, outbound := l.mt999Exports[m.RelatedReference]
		_, payout := l.payouts[m.RelatedReference]
		_, inMT202 := l.mt202Applied[m.RelatedReference]
		_, outMT202 := l.mt202Exports[m.RelatedReference]
		l.mu.RUnlock()
		switch {
		case outbound:
			res.OutboundReference = m.RelatedReference
		case payout:
			res.PayoutID = m.RelatedReference
		case inMT202, outMT202:
			res.MT202Ref = m.RelatedReference
		default:
			repairID, parkErr := l.parkMT999Notification(m, actor)
			if parkErr != nil {
				return MT999ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT999UnknownReference, m.RelatedReference, parkErr)
			}
			return MT999ApplyResult{
				Reference:        m.TransactionReference,
				RelatedReference: m.RelatedReference,
				Narrative:        mt999ReceivedLinesCopy(m.Narrative),
				RepairID:         repairID,
			}, fmt.Errorf("%w: %q", ErrMT999UnknownReference, m.RelatedReference)
		}
	}

	// The :20: record and the inbound marker event, under one
	// short write lock: the notification is already built when
	// the marker lands. A concurrent double-apply that landed
	// first returns the stored outcome instead of a second
	// marker. The audit event carries no accounts: a free-format
	// note touches no ledger account, and inventing one would
	// misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt999Received[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt999ReceivedTermsEqual(rec, m) {
			return MT999ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT999MsgIDConflict, m.TransactionReference)
		}
		dup := mt999ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt999Received == nil {
		l.mt999Received = make(map[string]mt999ReceivedRecord)
	}
	l.mt999Received[m.TransactionReference] = mt999ReceivedRecord{
		Message: MT999{
			SenderLT:             m.SenderLT,
			ReceiverLT:           m.ReceiverLT,
			TransactionReference: m.TransactionReference,
			RelatedReference:     m.RelatedReference,
			Narrative:            mt999ReceivedLinesCopy(m.Narrative),
		},
		RelatedReference: m.RelatedReference,
		ReceivedAt:       time.Now().UTC(),
		Actor:            actor,
		Result:           mt999ReceivedResultCopy(res),
	}
	l.emitAudit(AuditEvent{
		Op:            "mt999_received",
		Actor:         actor,
		TraceID:       m.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            m.TransactionReference,
			"related_reference": m.RelatedReference,
			"sender_lt":         m.SenderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT999Received returns the registered notification for
// msgID (its :20:), or false when no notification was ever
// registered under it. The returned result is a copy — callers
// cannot mutate ledger state through it.
func (l *Ledger) GetMT999Received(msgID string) (MT999ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt999Received[msgID]
	if !ok {
		return MT999ApplyResult{}, false
	}
	return mt999ReceivedResultCopy(rec.Result), true
}
