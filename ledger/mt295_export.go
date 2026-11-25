package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT295 queries export (LG-468): the outbound half of
// the Category 2 query leg. A correspondent bank asked this ledger
// about a Category 2 message it holds — an MT210 notification, an
// inbound MT292 cancellation request, or an inbound MT202 transfer
// — and ApplyMT295 registered the query (see mt295_inbound.go,
// LG-466); this is the mirror direction — the ledger itself asks a
// correspondent bank about a cancellation request it sent ("did you
// honour this MT292?"), because the inbound half alone only proves
// the ledger can answer questions, never that it can ask one. The
// answer comes back as an MT296 (see mt296_inbound.go, LG-465),
// which refers back to this export by its :20: through the :21: —
// the "ask—answer" loop closes across the two modules, the
// Category 2 twin of the Category 1 MT195/MT196 loop (LG-460 /
// LG-457).
//
// Field set: the same five-field core set as the inbound MT295
// (mt295_inbound.go, LG-466) — :20: / :21: / :75: / :77A: / :79:,
// in that canonical order — rendered with the MT196 framing
// (block 1 sender LT, block 2 `I295`, block 3 `{108:MUR}` carrying
// the :20:, block 4 the field set, no block 5). The :75: is
// mandatory here, along the inbound call: a query with no question
// text asks nothing. The :77A: narrative and :79: description ride
// verbatim from the operator — the codewords inside :75: (STAT /
// DUPL / PRTY and friends) are the operator's words, read by the
// correspondent's operator, never interpreted by this ledger, along
// the inbound rule. Rule C1's other branch — a copy of the original
// message's mandatory fields — is never rendered: the :79: branch
// is the only description, and :11S: / :11R: are refused as
// unknown fields, along the inbound rule.
//
// The :21: anchors in a single tier — the queried cancellation
// request is the one this ledger's own outbound MT292 named, so
// the :21: is that MT292's :20:, derived deterministically from
// the schedule and run (see mt292MessageReference): "MT292-" plus
// the first 10 lowercase hex characters of the SHA-256 of the run
// transfer ID ("<ID>/run/<seq>", the MT210's :21:). The anchor
// check has two steps, along ExportMT292 (mt292.go): the schedule
// must exist (unknown is ErrScheduleNotFound), and it must be in
// MT292-exportable state (a still-active schedule with a live next
// run is ErrMT292ScheduleStillActive — an active schedule's MT292
// cannot have been sent, so the gate itself is the proof that the
// MT292 the :21: names could exist). Either step failing parks the
// export fail-closed in the repair queue (RepairReasonUnknownRoute,
// the camt.055 shape, along parkMT195Export) and reports a
// 422-class error.
//
// Scope honesty, the same limit ExportMT292 carries: the outbound
// MT292 :20: is registered on export (mt292Exports, LG-471), but
// this module does not consult it, so the anchor can only verify
// that the schedule could produce an MT292 with this :20: — it
// cannot verify that the MT292 was actually sent. The gate is
// shape-plus-state, never claimed as a sent record check. No MT202
// tier is offered: an outbound MT202's :20:
// is caller-supplied at render time and is registered on export
// (mt202Exports, LG-470), but no MT296 tier anchors against it
// yet — that tier is a later item.
//
// Identity wiring — scope honesty, the ledger is not a SWIFT member
// (see mt192.go, LG-434):
//   - The sender's and receiver's LT addresses are
//     operator-supplied on every export (unlike the MT195 export,
//     whose sender BIC comes from the payee account's registered
//     IBAN BIC): the ledger records neither where an MT292 was
//     sent nor which correspondent to ask, so the caller names
//     both ends. The ledger keeps no routing table, so it never
//     picks a correspondent on its own.
//   - :20: is a deterministic derivation — "MT295-" plus the first
//     10 lowercase hex characters of the SHA-256 of the queried
//     MT292's :20: and the question body — 16 characters, 16x
//     legal (the MT195 :20: precedent, LG-460), so repeat exports
//     render byte-identical and the reference is stable per
//     queried request and question without inventing a sequence
//     number. Asking the same question about the same cancellation
//     request always derives the same reference, and a different
//     question derives a different one.
//
// The export renders only: no journal entry, no schedule status
// change, no ExpectedReceipt cancellation, no version bump —
// rendering a question is read-only, and the audit event is the
// durable record, not a state change. No query snapshot is built:
// the :75: is the operator's question, not a fact computed at
// registration time, so a replay returns the verbatim question
// from the export registry, never a re-read of live facts (the
// MT296 export rule, LG-467).
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416, along the inbound mt295ReceivedTermsEqual):
// the export's own reference names one export. Re-exporting the
// same schedule and run with the same question returns the stored
// rendered message with Duplicate set, rendering nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT295MsgIDConflict — never silently
// overwritten. The sender/receiver LTs are envelope, not terms,
// along the MT196 precedent: a replay under different LTs still
// returns the first rendering, because the :20: names one export.
// The registry check runs again under the write lock after the
// message is rendered, so two concurrent exports of the same :20:
// still record once.

// MT295ExportOptions names the question of one MT295 query
// export: the correspondent to ask and the question itself. The
// schedule and run are the first arguments of
// ExportMT295ForCancelled, not options: they are the anchor
// (:21:), and keeping them out of the options makes the anchor
// explicit at the call site. The :20: is not an option: it is
// derived from the queried MT292 and the question (see
// mt295MessageReference).
type MT295ExportOptions struct {
	SenderLT    string
	ReceiverLT  string
	Queries     []string // :75:, the coded question, mandatory, 1-6 lines.
	Narrative   []string // :77A:, the operator note, optional, 0-20 lines.
	Description []string // :79:, the free-format description, optional, 0-50 lines.
}

// MT295ExportResult is the outcome of ExportMT295ForCancelled:
// the rendered query message and the facts it was rendered from.
// A parked export (unknown schedule, or a schedule still active)
// carries only Reference, RelatedReference, ScheduleID, RunSeq,
// and RepairID, alongside the anchor error.
type MT295ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the queried MT292's :20: (the :21:).
	RelatedReference string `json:"related_reference"`
	// ScheduleID is the transfer schedule whose MT292 is queried.
	ScheduleID string `json:"schedule_id"`
	// RunSeq is the run whose MT292 is queried.
	RunSeq uint64 `json:"run_seq"`
	// Message is the rendered SWIFT FIN MT295 text.
	Message string `json:"message"`
	// RepairID is the repair queue record for a parked export
	// (unknown schedule, or a schedule still active). Empty on a
	// rendered export.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// MT295QueriedKind names which kind of ledger object a query's
// :21: anchored to: the outbound MT292 cancellation request
// this ledger rendered for the schedule's run.
type MT295QueriedKind string

const (
	// MT295QueriedMT292 is the :21: tier for an outbound MT292
	// cancellation request (mt292MessageReference).
	MT295QueriedMT292 MT295QueriedKind = "mt292"
)

// mt295ExportRecord is the registry record for one rendered MT295
// query, keyed by :20: in mt295Exports: the MT292 it asks about,
// its kind, the options it was rendered with, and the result, so
// a replay returns the same message without rendering twice.
type mt295ExportRecord struct {
	QueriedRef  string
	QueriedKind MT295QueriedKind
	Options     MT295ExportOptions
	Result      MT295ExportResult
}

// mt295ExportTermsEqual reports whether a stored export record
// carries exactly the terms an export request would render: the
// same queried MT292 (:21:) and the same question (:75: / :77A: /
// :79:, verbatim). The sender/receiver LTs are envelope, not
// terms, along the MT196 precedent — a replay under different LTs
// still returns the first rendering, because the :20: names one
// export.
func mt295ExportTermsEqual(rec mt295ExportRecord, relatedRef string, opts MT295ExportOptions) bool {
	if rec.QueriedRef != relatedRef {
		return false
	}
	if len(rec.Options.Queries) != len(opts.Queries) {
		return false
	}
	for i, ln := range rec.Options.Queries {
		if ln != opts.Queries[i] {
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

// mt295MessageReference derives the deterministic :20: reference:
// "MT295-" plus the first 10 lowercase hex characters of the
// SHA-256 of the queried MT292's :20: and the question body,
// joined with "\n" — 16 characters, 16x legal (the MT195 :20:
// precedent, LG-460). The separator keeps "MT292-abcdef1234" +
// "STAT" distinct from splits that would otherwise hash the same;
// the question body is the :75:, :77A:, and :79: verbatim, so the
// same cancellation request asked the same question always
// derives the same reference, and a different question derives a
// different one.
func mt295MessageReference(relatedRef string, queries, narrative, description []string) string {
	sum := sha256.Sum256([]byte(relatedRef + "\n" + strings.Join(queries, "\n") + "\n" + strings.Join(narrative, "\n") + "\n" + strings.Join(description, "\n")))
	return "MT295-" + hex.EncodeToString(sum[:])[:10]
}

// validateMT295ExportOptions fail-closes on options
// ExportMT295ForCancelled would refuse, so the render gate and
// the export gate share one check, never a copy. It returns the
// normalized sender/receiver LT addresses.
func validateMT295ExportOptions(opts MT295ExportOptions) (senderLT, receiverLT string, err error) {
	senderLT, err = mt295NormalizeLT(opts.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt295NormalizeLT(opts.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	// The :75: question is mandatory here — a query with no
	// question text asks nothing, along the inbound call
	// (mt295_inbound.go). mt295TextLines refuses an empty set.
	if err := mt295TextLines(opts.Queries, ":75:", mt295MaxQueriesLines, ErrMT295Queries); err != nil {
		return "", "", err
	}
	if len(opts.Narrative) > 0 {
		if err := mt295TextLines(opts.Narrative, ":77A:", mt295MaxNarrativeLines, ErrMT295Narrative); err != nil {
			return "", "", err
		}
	}
	if len(opts.Description) > 0 {
		if err := mt295TextLines(opts.Description, ":79:", mt295MaxDescriptionLines, ErrMT295Description); err != nil {
			return "", "", err
		}
	}
	return senderLT, receiverLT, nil
}

// ExportMT295 renders m as a SWIFT FIN MT295 message: blocks 1-4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT295(m MT295) (string, error) {
	senderLT, receiverLT, err := mt295Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I295. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I295%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the field set, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	fmt.Fprintf(&b, ":21:%s\n", m.RelatedReference)
	fmt.Fprintf(&b, ":75:%s\n", m.Queries[0])
	for _, ln := range m.Queries[1:] {
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

// parkMT295Export parks a fail-closed export request in the repair
// queue and returns the record ID. The snapshot names the queried
// MT292 (:21:) as the instruction ID and the export (:20:) as the
// destination, so an operator can find the export from the record.
// The snapshot's Maker is deliberately empty (see the MT195
// package comment): the record is operator visibility, never a
// resubmittable instruction. A parking failure (a full queue) is
// returned so the caller can fail loud rather than silently drop
// the bank's question.
func (l *Ledger) parkMT295Export(ref, relatedRef, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          relatedRef,
		Destination: "mt295-export:" + ref,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ExportMT295ForCancelled renders the SWIFT FIN MT295 query about
// the outbound MT292 cancellation request the schedule and run
// name: the :21: is that MT292's :20: (see
// mt292MessageReference), the :20: (see mt295MessageReference) is
// this export's own reference, and the :75: / :77A: / :79: carry
// the operator's question, verbatim — never interpreted, never
// derived from live facts, and never snapshotted, because the
// question is the operator's words, not a ledger fact.
//
// The schedule must exist (an unknown schedule is
// ErrScheduleNotFound, parked in the repair queue fail-closed in
// the camt.055 shape: the returned result carries the repair ID
// alongside the error), and it must be in MT292-exportable state
// (a still-active schedule with a live next run is
// ErrMT292ScheduleStillActive, parked the same way — the gate
// itself is the proof that the MT292 the :21: names could exist).
// The anchor is shape-plus-state, never a sent-record check: this
// module does not consult the outbound MT292 registry
// (mt292Exports, LG-471), so it cannot verify that the MT292 was
// actually sent. Nothing moves: no journal
// entry, no schedule status change, no ExpectedReceipt
// cancellation, no version bump — rendering a question is
// read-only, and the audit event is the durable record, not a
// state change.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same schedule, run, and question
// returns the stored rendered message with Duplicate set,
// rendering nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT295MsgIDConflict.
//
// Locking: the validation runs on the supplied values alone; the
// replay read and the schedule read run under the read lock; the
// :20: record and the export marker event run under a short write
// lock of their own, re-checking the registry first so a
// concurrent double-export still records once; the repair parking
// runs outside any held lock because EnqueueRepair takes l.mu
// itself.
func (l *Ledger) ExportMT295ForCancelled(scheduleID string, runSeq uint64, opts MT295ExportOptions, actor string) (MT295ExportResult, error) {
	if actor == "" {
		actor = "ExportMT295ForCancelled"
	}
	senderLT, _, err := validateMT295ExportOptions(opts)
	if err != nil {
		return MT295ExportResult{}, err
	}

	// :21: — the MT292 :20: this query asks about, derived
	// deterministically from the schedule and run (the MT210's
	// :21: is "<ID>/run/<seq>", and the MT292 :20: hashes it).
	relatedRef := mt292MessageReference(scheduleID, runSeq)
	// :20: — the deterministic message reference, also the MUR.
	ref := mt295MessageReference(relatedRef, opts.Queries, opts.Narrative, opts.Description)

	// :20: replay: the query was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt295Exports[ref]; ok {
		l.mu.RUnlock()
		if !mt295ExportTermsEqual(rec, relatedRef, opts) {
			return MT295ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT295MsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The anchor, two steps along ExportMT292: the schedule must
	// exist, and it must be in MT292-exportable state. A
	// still-active schedule's MT292 cannot have been sent, so the
	// gate itself is the proof that the MT292 the :21: names
	// could exist — and an unknown schedule, or one still active,
	// parks fail-closed in the repair queue (the camt.055 shape)
	// and is reported as the export's error.
	sched, err := l.GetTransferSchedule(scheduleID)
	if err != nil {
		repairID, parkErr := l.parkMT295Export(ref, relatedRef, actor)
		if parkErr != nil {
			return MT295ExportResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", err, scheduleID, parkErr)
		}
		return MT295ExportResult{Reference: ref, RelatedReference: relatedRef, ScheduleID: scheduleID, RunSeq: runSeq, RepairID: repairID}, err
	}
	if sched.Status == ScheduleActive && (sched.EndsAt.IsZero() || !sched.NextRunAt.After(sched.EndsAt)) {
		repairID, parkErr := l.parkMT295Export(ref, relatedRef, actor)
		if parkErr != nil {
			return MT295ExportResult{}, fmt.Errorf("%w: schedule %q is still active with a next run at %s (repair parking failed: %v)", ErrMT292ScheduleStillActive, scheduleID, sched.NextRunAt.UTC().Format(time.RFC3339), parkErr)
		}
		return MT295ExportResult{Reference: ref, RelatedReference: relatedRef, ScheduleID: scheduleID, RunSeq: runSeq, RepairID: repairID},
			fmt.Errorf("%w: schedule %q is still active with a next run at %s", ErrMT292ScheduleStillActive, scheduleID, sched.NextRunAt.UTC().Format(time.RFC3339))
	}

	// The question, verbatim from the operator — never from live
	// facts, so the question is the same question the operator
	// asked.
	msg, err := ExportMT295(MT295{
		SenderLT:             opts.SenderLT,
		ReceiverLT:           opts.ReceiverLT,
		TransactionReference: ref,
		RelatedReference:     relatedRef,
		Queries:              opts.Queries,
		Narrative:            opts.Narrative,
		Description:          opts.Description,
	})
	if err != nil {
		return MT295ExportResult{}, err
	}
	res := MT295ExportResult{
		Reference:        ref,
		RelatedReference: relatedRef,
		ScheduleID:       scheduleID,
		RunSeq:           runSeq,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// event carries no accounts: a query touches no ledger
	// account, and inventing one would misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt295Exports[ref]; ok {
		l.mu.Unlock()
		if !mt295ExportTermsEqual(rec, relatedRef, opts) {
			return MT295ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT295MsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt295Exports == nil {
		l.mt295Exports = make(map[string]mt295ExportRecord)
	}
	// The options' slices are copied: the :75: / :77A: / :79:
	// lines are the registered verbatim question, and a caller
	// mutating its options after the call must never rewrite
	// them — the mt295Received copy invariant, applied here.
	storedOpts := opts
	storedOpts.Queries = mt295LinesCopy(opts.Queries)
	storedOpts.Narrative = mt295LinesCopy(opts.Narrative)
	storedOpts.Description = mt295LinesCopy(opts.Description)
	l.mt295Exports[ref] = mt295ExportRecord{
		QueriedRef:  relatedRef,
		QueriedKind: MT295QueriedMT292,
		Options:     storedOpts,
		Result:      res,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt295_query_exported",
		Actor:         actor,
		TraceID:       ref,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            ref,
			"related_reference": relatedRef,
			"schedule_id":       scheduleID,
			"sender_lt":         senderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT295Export returns the recorded export for msgID (its :20:),
// or false when no query was ever rendered under it. The returned
// result is a copy — callers cannot mutate ledger state through
// it.
func (l *Ledger) GetMT295Export(msgID string) (MT295ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt295Exports[msgID]
	if !ok {
		return MT295ExportResult{}, false
	}
	return rec.Result, true
}
