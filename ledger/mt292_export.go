package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT292 outbound export registration (LG-471): the
// outbound half of the Category 2 cancellation leg's paper trail.
// The pure render (ExportMT292, LG-435) proves nothing about what
// this ledger has already rendered: the same :20: rendered twice
// is two indistinguishable messages, and a correspondent answering
// with an MT296 names a :20: the ledger cannot confirm it ever
// rendered — the MT296 tier-(a) gate is shape-only for exactly
// this reason (mt296_inbound.go, LG-465), and the inbound MT295
// module fail-closes a :21: naming our outbound MT292's :20:
// for the same reason (mt295_inbound.go). Registering the export
// closes that gap for the outbound side — the :20: names one
// export, a replay returns the stored message, and a :20: reused
// for different terms is refused — so the (a) tier and the MT295
// outbound tier can later anchor against this registry instead
// of the shape alone. Those tiers are later items: this item only
// builds the registry they will consult.
//
// The :20: is a deterministic derivation — "MT292-" plus the first
// 10 lowercase hex characters of the SHA-256 of the run transfer
// ID ("<ID>/run/<seq>", mt292MessageReference) — so the schedule
// ID and run sequence are pinned by the key itself (a SHA-256
// collision aside), and the terms check only has to cover what
// can still vary under one :20:: the servicing BIC (compared in
// its rendered form, uppercased and trimmed, along the MT202
// precedent), the original date, and the reason verbatim. The
// sender/receiver LTs are envelope, not terms, along the MT196
// precedent — a replay under different LTs still returns the
// first rendering, because the :20: names one export. (In
// practice the LTs cannot vary here anyway: the sender LT derives
// from the schedule's To party and the receiver LT from the
// servicing BIC, both of which are terms.)
//
// Scope honesty, the same limit the MT202 registry (LG-470)
// carries: registration proves only that this ledger rendered
// the message — not that it was sent (block 1 session/sequence
// are zero-filled, assigned by the sender's SWIFT interface on
// transmission, mt292.go) and not that the beneficiary bank
// honoured it (that is confirmed by its reply, the inbound
// MT296). The registry is in-memory only, like mt295Exports: the
// hash-chained mt292_exported audit events are the durable
// record, and a restart clears it — a late MT296 answer arriving
// after a restart falls back to the shape gate, exactly as
// before.
//
// The export renders only: no journal entry, no version bump —
// rendering a cancellation request is read-only, and the audit
// event is the durable record, not a state change. No MT296 tier
// anchors against this registry yet: a :21: naming an outbound
// MT292's :20: is still shape-only in tier (a), and the MT295
// inbound module still consults no outbound registry — those
// tiers are later items.

// MT292 outbound export errors.
var (
	// ErrMT292ExportMsgIDConflict is returned when a :20:
	// reference already registered for an outbound MT292 export
	// is reused for different terms. It is a 422-class error:
	// the export was already rendered once, and the :20: names
	// that one export. It is not the inbound
	// ErrMT292MsgIDConflict (mt292_inbound.go): that sentinel's
	// wording is the inbound reading ("already applied for a
	// different original notification"), and reusing it for an
	// export conflict would mislead the operator.
	ErrMT292ExportMsgIDConflict = errors.New("ledger: mt292: :20: already exported with different terms")
)

// MT292ExportResult is the outcome of ExportMT292Registered: the
// rendered cancellation request and the facts it was rendered
// from.
type MT292ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the :21: related reference — the
	// schedule ID, i.e. the original MT210's :20:.
	RelatedReference string `json:"related_reference"`
	// ScheduleID is the transfer schedule whose next run's MT210
	// notification is cancelled.
	ScheduleID string `json:"schedule_id"`
	// RunSeq is the run whose MT210 notification is cancelled.
	RunSeq uint64 `json:"run_seq"`
	// Message is the rendered SWIFT FIN MT292 text.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt292ExportRecord is the registry record for one rendered MT292
// cancellation request, keyed by :20: in mt292Exports: the
// schedule and run it cancels, the options it was rendered with,
// and the result, so a replay returns the same message without
// rendering twice.
type mt292ExportRecord struct {
	ScheduleID string
	RunSeq     uint64
	Options    MT292ExportOptions
	Result     MT292ExportResult
}

// mt292ExportTermsEqual reports whether a stored export record
// carries exactly the terms an export request would render: the
// same servicing BIC (in its rendered form — uppercased and
// trimmed, so a replay differing only in BIC case or
// surrounding space is the same terms), the same original date,
// and the same reason verbatim. The schedule ID and run sequence
// are pinned by the :20: key itself and are not compared.
func mt292ExportTermsEqual(rec mt292ExportRecord, opts MT292ExportOptions) bool {
	if strings.ToUpper(strings.TrimSpace(rec.Options.ServicingBIC)) != strings.ToUpper(strings.TrimSpace(opts.ServicingBIC)) {
		return false
	}
	if !rec.Options.OriginalDate.Equal(opts.OriginalDate) {
		return false
	}
	if rec.Options.Reason != opts.Reason {
		return false
	}
	return true
}

// ExportMT292Registered renders the next run of the transfer
// schedule as a SWIFT FIN MT292 cancellation of notification to
// receive and registers the export under its :20: (see
// mt292Exports), so the ledger can later confirm it rendered
// that cancellation request.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same reference and the same
// terms returns the stored rendered message with Duplicate set,
// rendering nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT292ExportMsgIDConflict.
//
// Nothing moves: no journal entry, no version bump — rendering a
// cancellation request is read-only, and the audit event is the
// durable record, not a state change. The audit event is
// mt292_exported (details msg_id / related_reference /
// schedule_id / run_seq / sender_lt, trace :20:, no accounts: a
// cancellation request touches no ledger account).
//
// Locking: the run sequence is read first (the :20: is derived
// from it, so the replay check needs it before anything is
// rendered); the replay read runs under the read lock; the render
// runs outside any held lock (it takes the read lock itself for
// the party lookup); the :20: record and the export marker event
// run under a short write lock of their own, re-checking the
// registry first so a concurrent double-export still records
// once, and re-checking the run sequence by reading l.schedules
// directly — GetTransferSchedule takes the read lock, which
// would deadlock under the write lock held here, and there is
// no locked accessor. The run-sequence recheck is defence in
// depth only: the sequence is frozen while a schedule is in
// MT292-exportable state (paused/cancelled/completed schedules
// never fire, and an exhausted active schedule fires no new
// run), so it can only trip if the schedule moved between the
// render and this lock.
func (l *Ledger) ExportMT292Registered(opts MT292ExportOptions, actor string) (MT292ExportResult, error) {
	if actor == "" {
		actor = "ExportMT292Registered"
	}

	// :20: — the deterministic message reference for this
	// schedule and run. Read the run sequence first: the replay
	// check below needs the key before anything is rendered.
	sched, err := l.GetTransferSchedule(opts.ScheduleID)
	if err != nil {
		return MT292ExportResult{}, err
	}
	runSeq := sched.RunSeq
	ref := mt292MessageReference(opts.ScheduleID, runSeq)

	// :20: replay: the cancellation request was already rendered
	// under this reference. Same terms is a no-op returning the
	// stored message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt292Exports[ref]; ok {
		l.mu.RUnlock()
		if !mt292ExportTermsEqual(rec, opts) {
			return MT292ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT292ExportMsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The render (pure render, unchanged): the internal form also
	// returns the sender LT for the audit event and the run
	// sequence it rendered for.
	msg, senderLT, renderedSeq, err := l.exportMT292(opts)
	if err != nil {
		return MT292ExportResult{}, err
	}
	// A render for a different run means the schedule moved
	// between the read above and the render: the rendered message
	// names a run that is no longer the next one, and registering
	// it would record a stale export. Refuse and let the caller
	// retry with fresh state.
	if renderedSeq != runSeq {
		return MT292ExportResult{}, fmt.Errorf("ledger: mt292 export for schedule %q: run sequence changed during export", opts.ScheduleID)
	}
	res := MT292ExportResult{
		Reference:        ref,
		RelatedReference: opts.ScheduleID,
		ScheduleID:       opts.ScheduleID,
		RunSeq:           runSeq,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// event carries no accounts: a cancellation request touches no
	// ledger account, and inventing one would misattribute the
	// evidence.
	l.mu.Lock()
	if rec, ok := l.mt292Exports[ref]; ok {
		l.mu.Unlock()
		if !mt292ExportTermsEqual(rec, opts) {
			return MT292ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT292ExportMsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if s, ok := l.schedules[opts.ScheduleID]; !ok || s.RunSeq != runSeq {
		l.mu.Unlock()
		return MT292ExportResult{}, fmt.Errorf("ledger: mt292 export for schedule %q: run sequence changed during export", opts.ScheduleID)
	}
	if l.mt292Exports == nil {
		l.mt292Exports = make(map[string]mt292ExportRecord)
	}
	// The options are stored verbatim: the :20: names one export,
	// and a replay compares against the registered terms, never a
	// re-read of live facts.
	l.mt292Exports[ref] = mt292ExportRecord{
		ScheduleID: opts.ScheduleID,
		RunSeq:     runSeq,
		Options:    opts,
		Result:     res,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt292_exported",
		Actor:         actor,
		TraceID:       ref,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            ref,
			"related_reference": opts.ScheduleID,
			"schedule_id":       opts.ScheduleID,
			"run_seq":           runSeq,
			"sender_lt":         senderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT292Export returns the recorded export for msgID (its :20:),
// or false when no cancellation request was ever rendered under
// it. The returned result is a copy — callers cannot mutate ledger
// state through it.
func (l *Ledger) GetMT292Export(msgID string) (MT292ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt292Exports[msgID]
	if !ok {
		return MT292ExportResult{}, false
	}
	return rec.Result, true
}
