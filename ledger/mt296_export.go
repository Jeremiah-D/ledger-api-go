package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT296 answers export (LG-467): the outbound half of
// the Category 2 question-answer chain. A correspondent bank asked
// this ledger about a Category 2 message it holds — with an
// inbound MT292 cancellation request (see mt292_inbound.go,
// LG-452) or an inbound MT295 query (see mt295_inbound.go,
// LG-466) — and the MT296 is the ledger's answer, rendered from
// the operator's own answer lines, never from live facts and never
// from a snapshot. The answer reports the operator's decision; it
// moves nothing: no journal entry, no ExpectedReceipt cancellation
// flip, no schedule status change, no version bump. An answer that
// moved money or cancelled a notification would be a payment or a
// cancellation, not an answer.
//
// The :76: source is the opposite of the MT196 export (LG-457),
// and the difference is the whole point of this module. An MT196
// answers an MT195 status query, and the query asks for ledger
// facts — so the MT196's :76: is rendered from the snapshot the
// query's first application recorded (payout status, MT103 value
// date, gpi status). An MT296 answers a cancellation request or a
// query, and whether a cancellation is honoured is the operator's
// (or the correspondent bank's) decision, not a ledger state — the
// MT292 header says it outright ("whether the bank honours the
// request is confirmed by its own reply, outside this ledger"), and
// the inbound MT296 header says the same for the other direction
// (the answer is the bank's statement, not the ledger's own view
// of it). So the :76: here is supplied by the operator, organised
// per answered object, validated (1-6 lines, 1-35 SWIFT `x`
// characters each, along ErrMT296Answers) and rendered verbatim —
// the published answer codes (/CNCL/, /RJCR/, /PDCR/) are text
// lines inside :76:, and this ledger invents no accept/reject
// code table and runs no status machine off them. There is no
// answer snapshot to take: a replay returns the verbatim lines
// from the export registry, never a re-read of live facts.
//
// The :21: anchors in two tiers (the MT999 two-tier shape,
// mt999_inbound.go:8-22): (a) the :20: of a registered inbound
// MT292 cancellation request, looked up in mt292Applied; (b) the
// :20: of a registered inbound MT295 query, looked up in
// mt295Received. Either tier anchors; neither tier is consulted
// for the other direction's records — an outbound MT292's :20:
// (`MT292-` + 10 lowercase hex, mt292MessageReference) names
// nothing here: it is registered in mt292Exports (LG-471), but
// this export answers registered inbound queries, and our own
// MT292's answer comes back as an inbound MT296 (LG-465),
// never as an export anchor. A :21: in neither registry is
// fail-closed into the repair queue (RepairReasonUnknownRoute, the
// camt.055 shape, along parkMT196Export) and reported as
// ErrMT296AnsweredNotFound, a 422-class error.
//
// Field set: the same five-field core set as the inbound MT296
// (mt296_inbound.go, LG-465) — :20: / :21: / :76: / :77A: / :79:,
// in that canonical order — rendered with the MT196 framing
// (block 1 sender LT, block 2 `I296`, block 3 `{108:MUR}` carrying
// the :20:, block 4 the field set, no block 5). Rule C1's other
// branch — a copy of the original message's mandatory fields —
// is never rendered: the :79: branch is the only description, and
// :11S: / :11R: are refused as unknown fields, along the inbound
// rule.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416, along mt196TermsEqual): the export's own
// reference names one export. Re-exporting the same :20: with the
// same answered reference, the same :76: answers, and the same
// :77A: / :79: text returns the stored rendered message with
// Duplicate set, rendering nothing twice and emitting no second
// audit event. A :20: reused for different terms is refused with
// ErrMT296MsgIDConflict — never silently overwritten. The
// sender/receiver LTs are envelope, not terms, along the MT196
// precedent: a replay under different LTs still returns the first
// rendering, because the :20: names one export. The registry
// check runs again under the write lock after the message is
// rendered, so two concurrent exports of the same :20: still
// record once.
//
// The export's audit event, `mt296_answer_exported` (details
// msg_id / related_reference / answered_kind / sender_lt, trace
// :20:), puts the :20: on the durable record, along the
// mt196_answer_exported shape: no accounts, because an answer
// touches no ledger account.

// MT296 export errors.
var (
	// ErrMT296AnsweredNotFound is returned by
	// ExportMT296ForAnswered when the :21: names no registered
	// inbound MT292 cancellation request and no registered
	// inbound MT295 query: the export is parked fail-closed in
	// the repair queue and reported as a 422-class error. It is
	// distinct from the inbound ErrMT296UnknownReference: that
	// one is an inbound answer whose :21: has the wrong shape
	// for an outbound MT292 :20:; this one is an outbound
	// answer with no answered object at all.
	ErrMT296AnsweredNotFound = errors.New("ledger: mt296: unknown answered reference")
)

// MT296AnsweredKind names which ledger registry a :21: anchored
// in: the inbound MT292 registry or the inbound MT295 registry.
type MT296AnsweredKind string

const (
	// MT296AnsweredMT292 is the :21: tier for a registered
	// inbound MT292 cancellation request (mt292Applied).
	MT296AnsweredMT292 MT296AnsweredKind = "mt292"
	// MT296AnsweredMT295 is the :21: tier for a registered
	// inbound MT295 query (mt295Received).
	MT296AnsweredMT295 MT296AnsweredKind = "mt295"
)

// MT296ExportOptions names the envelope of one MT296 answer
// export: the sender/receiver LT addresses, the export's own
// :20: reference, and the operator's answer. The :21: (the
// answered MT292's or MT295's :20:) is the first argument of
// ExportMT296ForAnswered, not an option: it is the lookup key,
// and keeping it out of the options makes the anchor explicit
// at the call site. The :76: answers are options here — unlike
// the MT196 export, where they are rendered from a snapshot —
// because the answer is the operator's statement, organised per
// answered object, never a ledger fact.
type MT296ExportOptions struct {
	SenderLT    string
	ReceiverLT  string
	Reference   string   // :20:, this export's own reference, the replay key.
	Answers     []string // :76:, the operator's answer lines, mandatory.
	Narrative   []string // :77A:, the operator note, optional.
	Description []string // :79:, the free-format description, optional.
}

// MT296ExportResult is the outcome of ExportMT296ForAnswered:
// the rendered answer message and the facts it was rendered
// from. A parked export (unknown :21:) carries only Reference,
// RelatedReference, and RepairID, alongside the
// ErrMT296AnsweredNotFound error.
type MT296ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the answered request's :20: (the :21:).
	RelatedReference string `json:"related_reference"`
	// AnsweredKind names which registry the :21: anchored in:
	// "mt292" or "mt295".
	AnsweredKind MT296AnsweredKind `json:"answered_kind,omitempty"`
	// Message is the rendered SWIFT FIN MT296 text.
	Message string `json:"message"`
	// RepairID is the repair queue record for a parked export
	// (unknown :21:). Empty on a rendered export.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt296ExportRecord is the registry record for one rendered MT296
// answer, keyed by :20: in mt296Exports: the request it answered,
// its kind, the options it was rendered with, and the result, so
// a replay returns the same message without rendering twice.
type mt296ExportRecord struct {
	AnsweredRef  string
	AnsweredKind MT296AnsweredKind
	Options      MT296ExportOptions
	Result       MT296ExportResult
}

// mt296TermsEqual reports whether a stored export record carries
// exactly the terms an export request would render: the same
// answered reference, the same :76: answers, and the same :77A:
// / :79: text, verbatim. The sender/receiver LTs are envelope,
// not terms, along the MT196 precedent — a replay under different
// LTs still returns the first rendering, because the :20: names
// one export.
func mt296TermsEqual(rec mt296ExportRecord, relatedRef string, opts MT296ExportOptions) bool {
	if rec.AnsweredRef != relatedRef {
		return false
	}
	if len(rec.Options.Answers) != len(opts.Answers) {
		return false
	}
	for i, ln := range rec.Options.Answers {
		if ln != opts.Answers[i] {
			return false
		}
	}
	if len(rec.Options.Narrative) != len(opts.Narrative) {
		return false
	}
	for i, ln := range rec.Options.Narrative {
		if ln != opts.Narrative[i] {
			return false
		}
	}
	if len(rec.Options.Description) != len(opts.Description) {
		return false
	}
	for i, ln := range rec.Options.Description {
		if ln != opts.Description[i] {
			return false
		}
	}
	return true
}

// validateMT296AnswerOptions fail-closes on options ExportMT296
// would refuse, so ExportMT296ForAnswered stays safe on
// hand-built requests too: the render gate and the export gate
// share one check, never a copy.
func validateMT296AnswerOptions(relatedRef string, opts MT296ExportOptions) error {
	if err := mt296Reference(opts.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt296Reference(relatedRef, ":21:"); err != nil {
		return err
	}
	if _, err := mt296NormalizeLT(opts.SenderLT); err != nil {
		return fmt.Errorf("sender: %w", err)
	}
	if _, err := mt296NormalizeLT(opts.ReceiverLT); err != nil {
		return fmt.Errorf("receiver: %w", err)
	}
	// The :76: answers are mandatory here — the operator's
	// statement is the answer, and an answer with no answer
	// lines is a malformed export, not an empty one.
	if err := mt296TextLines(opts.Answers, ":76:", mt296MaxAnswersLines, ErrMT296Answers); err != nil {
		return err
	}
	if len(opts.Narrative) > 0 {
		if err := mt296TextLines(opts.Narrative, ":77A:", mt296MaxNarrativeLines, ErrMT296Narrative); err != nil {
			return err
		}
	}
	if len(opts.Description) > 0 {
		if err := mt296TextLines(opts.Description, ":79:", mt296MaxDescriptionLines, ErrMT296Description); err != nil {
			return err
		}
	}
	return nil
}

// ExportMT296 renders m as a SWIFT FIN MT296 message: blocks 1-4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT296(m MT296) (string, error) {
	senderLT, receiverLT, err := mt296Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I296. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I296%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the field set, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	fmt.Fprintf(&b, ":21:%s\n", m.RelatedReference)
	fmt.Fprintf(&b, ":76:%s\n", m.Answers[0])
	for _, ln := range m.Answers[1:] {
		fmt.Fprintf(&b, "%s\n", ln)
	}
	if len(m.Narrative) > 0 {
		fmt.Fprintf(&b, ":77A:%s\n", m.Narrative[0])
		for _, ln := range m.Narrative[1:] {
			fmt.Fprintf(&b, "%s\n", ln)
		}
	}
	if len(m.Description) > 0 {
		fmt.Fprintf(&b, ":79:%s\n", m.Description[0])
		for _, ln := range m.Description[1:] {
			fmt.Fprintf(&b, "%s\n", ln)
		}
	}
	b.WriteString("-}")
	return b.String(), nil
}

// parkMT296Export parks a fail-closed export request in the repair
// queue and returns the record ID. The snapshot names the answered
// request (:21:) as the instruction ID and the export (:20:) as
// the destination, so an operator can find the export from the
// record. The snapshot's Maker is deliberately empty (see the
// MT195 package comment): the record is operator visibility, never
// a resubmittable instruction. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the bank's answer.
func (l *Ledger) parkMT296Export(ref, relatedRef, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          relatedRef,
		Destination: "mt296-export:" + ref,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ExportMT296ForAnswered renders the SWIFT FIN MT296 answer to
// the inbound MT292 cancellation request or inbound MT295 query
// relatedRef names: the :21: names the answered request, the :20:
// (opts.Reference) is this export's own reference, and the :76:
// answers are the operator's lines from opts, rendered verbatim
// — never derived from live facts, and never from a snapshot,
// because the answer is the operator's decision, not a ledger
// fact. The optional :77A: narrative and :79: description ride
// verbatim from opts; the :79: branch is the only description —
// no copy of the original message is ever rendered.
//
// The answered request must exist (an unknown :21: is
// ErrMT296AnsweredNotFound, parked in the repair queue fail-closed
// in the camt.055 shape: the returned result carries the repair
// ID alongside the error). Nothing moves: no journal entry, no
// ExpectedReceipt cancellation flip, no schedule status change,
// no version bump — rendering an answer is read-only, and the
// audit event is the durable record, not a state change.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same answered reference, the same
// :76: answers, and the same :77A: / :79: text returns the stored
// rendered message with Duplicate set, rendering nothing twice and
// emitting no second audit event; a :20: reused for different
// terms is refused with ErrMT296MsgIDConflict.
//
// Locking: the validation runs on the supplied values alone; the
// replay read and the anchor reads run under the read lock; the
// :20: record and the export marker event run under a short write
// lock of their own, re-checking the registry first so a
// concurrent double-export still records once; the repair parking
// runs outside any held lock because EnqueueRepair takes l.mu
// itself.
func (l *Ledger) ExportMT296ForAnswered(relatedRef string, opts MT296ExportOptions, actor string) (MT296ExportResult, error) {
	if actor == "" {
		actor = "ExportMT296ForAnswered"
	}
	if err := validateMT296AnswerOptions(relatedRef, opts); err != nil {
		return MT296ExportResult{}, err
	}

	// :20: replay: the answer was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt296Exports[opts.Reference]; ok {
		l.mu.RUnlock()
		if !mt296TermsEqual(rec, relatedRef, opts) {
			return MT296ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT296MsgIDConflict, opts.Reference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The answered request, two tiers: an inbound MT292's :20:
	// in mt292Applied, or an inbound MT295's :20: in
	// mt295Received. The MT292 tier is consulted first — a
	// reference cannot be both, but the order is fixed so the
	// kind is deterministic. Unknown is parked fail-closed in
	// the repair queue (the camt.055 shape) and reported as the
	// export's error. A :21: shaped like our own outbound MT292
	// :20: parks the same way: the ledger holds no outbound
	// MT292 registry, so the shape alone names nothing.
	l.mu.RLock()
	_, ok292 := l.mt292Applied[relatedRef]
	_, ok295 := l.mt295Received[relatedRef]
	l.mu.RUnlock()
	var kind MT296AnsweredKind
	switch {
	case ok292:
		kind = MT296AnsweredMT292
	case ok295:
		kind = MT296AnsweredMT295
	default:
		repairID, parkErr := l.parkMT296Export(opts.Reference, relatedRef, actor)
		if parkErr != nil {
			return MT296ExportResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT296AnsweredNotFound, relatedRef, parkErr)
		}
		return MT296ExportResult{Reference: opts.Reference, RelatedReference: relatedRef, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT296AnsweredNotFound, relatedRef)
	}

	// The answers, verbatim from the operator — never from live
	// facts, so the answer is the same answer the operator gave.
	msg, err := ExportMT296(MT296{
		SenderLT:             opts.SenderLT,
		ReceiverLT:           opts.ReceiverLT,
		TransactionReference: opts.Reference,
		RelatedReference:     relatedRef,
		Answers:              opts.Answers,
		Narrative:            opts.Narrative,
		Description:          opts.Description,
	})
	if err != nil {
		return MT296ExportResult{}, err
	}
	senderLT, _ := mt296NormalizeLT(opts.SenderLT)
	res := MT296ExportResult{
		Reference:        opts.Reference,
		RelatedReference: relatedRef,
		AnsweredKind:     kind,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// event carries no accounts: an answer touches no ledger
	// account, and inventing one would misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt296Exports[opts.Reference]; ok {
		l.mu.Unlock()
		if !mt296TermsEqual(rec, relatedRef, opts) {
			return MT296ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT296MsgIDConflict, opts.Reference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt296Exports == nil {
		l.mt296Exports = make(map[string]mt296ExportRecord)
	}
	// The options' slices are copied: the :76: / :77A: / :79:
	// lines are the registered verbatim answer, and a caller
	// mutating its options after the call must never rewrite
	// them — the mt296Received copy invariant, applied here.
	storedOpts := opts
	storedOpts.Answers = mt296LinesCopy(opts.Answers)
	storedOpts.Narrative = mt296LinesCopy(opts.Narrative)
	storedOpts.Description = mt296LinesCopy(opts.Description)
	l.mt296Exports[opts.Reference] = mt296ExportRecord{
		AnsweredRef:  relatedRef,
		AnsweredKind: kind,
		Options:      storedOpts,
		Result:       res,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt296_answer_exported",
		Actor:         actor,
		TraceID:       opts.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            opts.Reference,
			"related_reference": relatedRef,
			"answered_kind":     string(kind),
			"sender_lt":         senderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT296Export returns the recorded export for msgID, or false
// when no answer was ever rendered under it. The returned result
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT296Export(msgID string) (MT296ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt296Exports[msgID]
	if !ok {
		return MT296ExportResult{}, false
	}
	return rec.Result, true
}
