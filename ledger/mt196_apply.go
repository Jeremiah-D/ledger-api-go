package ledger

import (
	"fmt"
)

// SWIFT FIN MT196 answers inbound processing (LG-462): the inbound
// half of the MT195 lifecycle's answer leg. The ledger asked a
// correspondent bank about a payment with an outbound MT195 query
// (see mt195.go, LG-460); the MT196 is the correspondent's answer
// coming back, and ApplyMT196 registers it against the query its
// :21: names — the :21: is our outbound query's :20:, looked up
// in mt195Exports. The answer is registered verbatim: the :76:
// answers and the :77A: narrative ride into the record exactly as
// received, never re-derived from live facts, because the answer
// is the correspondent's statement about the payment, not the
// ledger's own view of it. A later change to the payout's status
// or gpi track never rewrites a registered answer; a replay
// returns the stored verbatim answer.
//
// The field set is the MT196 core set (see mt196.go, LG-457):
// :20: Transaction Reference 16x M (the answer's own reference,
// the replay key), :21: Related Reference 16x M (our outbound
// query's :20:, the anchor), :76: Answers 6*35x M, :77A:
// Narrative 20*35x O. Parsing is strict along ParseMT196, and
// ApplyMT196 re-validates hand-built requests through the same
// gate (mt196Validate), so the parse gate and the apply gate
// share one check, never a copy.
//
// Nothing moves: no journal entry, no payout status change, no
// gpi advance, no version bump — registering an answer is
// read-only, and the audit event is the durable record, not a
// state change. The answer's audit event,
// `mt196_answer_applied` (details msg_id / related_reference /
// query_ref / payout_id), puts the :20: on the durable record.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416), scoped to one answer per query: the
// registry is keyed by the :21: (the outbound query), so a
// re-applied answer with the same :20:, answers, and narrative
// returns the stored outcome with Duplicate set, registering
// nothing twice and emitting no second audit event. A :20:
// reused for different terms is refused with
// ErrMT196MsgIDConflict — never silently overwritten — and a
// second, different answer to an already-answered query is the
// same conflict: one query, one answer. The registry check runs
// again under the write lock after the query lookup, so two
// concurrent applies of the same answer still record once.
//
// An unknown :21: is fail-closed into the repair queue
// (RepairReasonUnknownRoute, the camt.055 shape, along the
// ApplyMT195 and ExportMT196ForQuery precedents: the returned
// result carries the repair ID alongside the
// ErrMT196QueryNotFound error), and reported as a 422-class
// error. The 404 alternative was considered and rejected at
// implementation time (LG-462): an inbound bank answer must
// never vanish without an operator-visible record, and the
// outbound registry is in-memory — a restart empties it, so a
// legitimate late answer arriving after a restart would
// otherwise be dropped with no trace. A parked answer records
// nothing, so a re-sent answer parks again — or registers, once
// the query exists.

// The conflict sentinel is shared with the outbound half:
// ErrMT196MsgIDConflict (see mt196.go) is returned by both
// ExportMT196ForQuery and ApplyMT196 when a :20: reference
// already recorded carries different terms — one sentinel for
// ":20: reused for different terms" in both directions, along
// the MT195 precedent (ErrMT195MsgIDConflict serves the inbound
// ApplyMT195 and the outbound ExportMT195 alike).

// MT196ApplyResult is the outcome of ApplyMT196: the registered
// answer, verbatim. A parked answer (unknown :21:) carries only
// Reference, RelatedReference, QueryRef, and RepairID,
// alongside the ErrMT196QueryNotFound error.
type MT196ApplyResult struct {
	// Reference echoes the answer's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the answered outbound query's :20:
	// (the :21:).
	RelatedReference string `json:"related_reference"`
	// QueryRef is the outbound query the answer belongs to —
	// the same value as RelatedReference, named as the
	// registry key so a query-status lookup reads it directly.
	QueryRef string `json:"query_ref"`
	// PayoutID is the payout the answered query was about.
	PayoutID string `json:"payout_id"`
	// Answers is the :76: answer set, verbatim.
	Answers []string `json:"answers,omitempty"`
	// Narrative is the :77A: operator note, verbatim.
	Narrative []string `json:"narrative,omitempty"`
	// RepairID is the repair queue record for a parked answer
	// (unknown :21:). Empty on a registered answer.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the answer was already
	// registered under the same :20: with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt196AnswerRecord is the registry record for one registered
// MT196 answer, keyed by the outbound query's :20: (the
// answer's :21:) in mt196Answers: the answer as parsed and the
// result its first application recorded, so a replay returns
// the same outcome without registering twice.
type mt196AnswerRecord struct {
	Answer MT196
	Result MT196ApplyResult
}

// mt196AnswerTermsEqual reports whether a stored answer record
// carries exactly the terms an inbound answer would apply: the
// same :20:, the same :76: answers, and the same :77A:
// narrative, verbatim. The sender/receiver LTs are envelope,
// not terms, along the MT195/MT196 precedent — a replay under
// different LTs still returns the first registration, because
// the :20: names one answer.
func mt196AnswerTermsEqual(rec mt196AnswerRecord, ans MT196) bool {
	if rec.Answer.TransactionReference != ans.TransactionReference {
		return false
	}
	if len(rec.Answer.Answers) != len(ans.Answers) {
		return false
	}
	for i, ln := range rec.Answer.Answers {
		if ln != ans.Answers[i] {
			return false
		}
	}
	if len(rec.Answer.Narrative) != len(ans.Narrative) {
		return false
	}
	for i, ln := range rec.Answer.Narrative {
		if ln != ans.Narrative[i] {
			return false
		}
	}
	return true
}

// mt196AnswerLinesCopy returns a copy of lines, so a registered
// answer's slices can never be mutated through a returned
// result — the getter invariant, along the other registries.
func mt196AnswerLinesCopy(lines []string) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// mt196AnswerResultCopy returns a copy of res with its own
// answer and narrative slices.
func mt196AnswerResultCopy(res MT196ApplyResult) MT196ApplyResult {
	res.Answers = mt196AnswerLinesCopy(res.Answers)
	res.Narrative = mt196AnswerLinesCopy(res.Narrative)
	return res
}

// parkMT196Answer parks a fail-closed inbound answer in the
// repair queue and returns the record ID. The snapshot names the
// answered query (:21:) as the instruction ID and the answer
// (:20:) as the destination, so an operator can find the answer
// from the record. The snapshot's Maker is deliberately empty
// (see the MT195 package comment): the record is operator
// visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's answer.
func (l *Ledger) parkMT196Answer(ans MT196, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          ans.RelatedReference,
		Destination: "mt196-inbound:" + ans.TransactionReference,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT196 applies one parsed inbound SWIFT FIN MT196 answer:
// it registers the correspondent's answer against the outbound
// MT195 query its :21: names, verbatim — the :76: answers and
// the :77A: narrative as received, never re-derived from live
// facts.
//
// The query must exist (an unknown :21: is
// ErrMT196QueryNotFound, parked in the repair queue fail-closed
// in the camt.055 shape: the returned result carries the repair
// ID alongside the error). No payout state is ever changed, no
// gpi track is ever advanced, and no journal entry is ever
// posted: an answer is evidence, not an instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention), scoped to one answer per query: a replay with
// the same :20:, answers, and narrative returns the stored
// outcome with Duplicate set, registering nothing twice and
// emitting no second audit event; a :20: reused for different
// terms, or a second answer to an already-answered query, is
// refused with ErrMT196MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the query read run under the read lock; the
// :20: record and the inbound marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-apply still records once.
func (l *Ledger) ApplyMT196(ans MT196, actor string) (MT196ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT196"
	}
	if _, _, err := mt196Validate(ans); err != nil {
		return MT196ApplyResult{}, err
	}

	// :21: replay: the query was already answered under this
	// reference. Same :20: and same terms is a no-op returning
	// the stored outcome; different terms, or a different
	// answer to the same query, is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt196Answers[ans.RelatedReference]; ok {
		l.mu.RUnlock()
		if !mt196AnswerTermsEqual(rec, ans) {
			return MT196ApplyResult{}, fmt.Errorf("%w: :21: %q", ErrMT196MsgIDConflict, ans.RelatedReference)
		}
		dup := mt196AnswerResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The answered query: :21: names it, our outbound MT195
	// query's :20:. Unknown is parked fail-closed in the repair
	// queue (the camt.055 shape) and reported as the apply's
	// error.
	l.mu.RLock()
	qrec, ok := l.mt195Exports[ans.RelatedReference]
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT196Answer(ans, actor)
		if parkErr != nil {
			return MT196ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT196QueryNotFound, ans.RelatedReference, parkErr)
		}
		return MT196ApplyResult{Reference: ans.TransactionReference, RelatedReference: ans.RelatedReference, QueryRef: ans.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT196QueryNotFound, ans.RelatedReference)
	}

	// The answer, verbatim: the correspondent's :76: answers
	// and :77A: narrative as received. Nothing is re-derived
	// from live facts.
	res := MT196ApplyResult{
		Reference:        ans.TransactionReference,
		RelatedReference: ans.RelatedReference,
		QueryRef:         ans.RelatedReference,
		PayoutID:         qrec.PayoutID,
		Answers:          mt196AnswerLinesCopy(ans.Answers),
		Narrative:        mt196AnswerLinesCopy(ans.Narrative),
	}

	// The :21: record and the inbound marker event, under one
	// short write lock: the answer is already built when the
	// marker lands. A concurrent double-apply that landed first
	// returns the stored outcome instead of a second marker. The
	// audit account is the payout's payee account, read under
	// the same lock — the payout cannot have vanished (payouts
	// are never deleted), but a missing payout is an empty
	// account list, not an apply failure.
	l.mu.Lock()
	if rec, ok := l.mt196Answers[ans.RelatedReference]; ok {
		l.mu.Unlock()
		if !mt196AnswerTermsEqual(rec, ans) {
			return MT196ApplyResult{}, fmt.Errorf("%w: :21: %q", ErrMT196MsgIDConflict, ans.RelatedReference)
		}
		dup := mt196AnswerResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt196Answers == nil {
		l.mt196Answers = make(map[string]mt196AnswerRecord)
	}
	l.mt196Answers[ans.RelatedReference] = mt196AnswerRecord{
		Answer: MT196{
			SenderLT:             ans.SenderLT,
			ReceiverLT:           ans.ReceiverLT,
			TransactionReference: ans.TransactionReference,
			RelatedReference:     ans.RelatedReference,
			Answers:              mt196AnswerLinesCopy(ans.Answers),
			Narrative:            mt196AnswerLinesCopy(ans.Narrative),
		},
		Result: mt196AnswerResultCopy(res),
	}
	var accounts []AccountID
	if p, ok := l.payouts[qrec.PayoutID]; ok {
		accounts = []AccountID{p.PayeeAccount}
	}
	l.emitAudit(AuditEvent{
		Op:            "mt196_answer_applied",
		Actor:         actor,
		TraceID:       ans.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"msg_id":            ans.TransactionReference,
			"related_reference": ans.RelatedReference,
			"query_ref":         ans.RelatedReference,
			"payout_id":         qrec.PayoutID,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT196Answer returns the registered answer for the outbound
// query queryRef (the answer's :21:), or false when no answer
// was ever registered for it. The returned result is a copy —
// callers cannot mutate ledger state through it.
func (l *Ledger) GetMT196Answer(queryRef string) (MT196ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt196Answers[queryRef]
	if !ok {
		return MT196ApplyResult{}, false
	}
	return mt196AnswerResultCopy(rec.Result), true
}

// GetMT196AnswerByReference returns the registered answer whose
// own :20: is msgID, or false when no answer was ever registered
// under it. The returned result is a copy — callers cannot
// mutate ledger state through it.
func (l *Ledger) GetMT196AnswerByReference(msgID string) (MT196ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, rec := range l.mt196Answers {
		if rec.Result.Reference == msgID {
			return mt196AnswerResultCopy(rec.Result), true
		}
	}
	return MT196ApplyResult{}, false
}
