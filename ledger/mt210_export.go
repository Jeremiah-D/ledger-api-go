package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT210 outbound export registration (LG-479): the
// outbound half of the Category 2 notice-to-receive leg's paper
// trail, and the anchor the future outbound MT295 MT210 tier
// will consult (LG-477/LG-478 family).
//
// The pure render (ExportMT210, LG-393) proves nothing about what
// this ledger has already rendered: the same :20: rendered twice
// is two indistinguishable messages, and a correspondent answering
// with an MT292 names a :20: the ledger cannot confirm it ever
// rendered — the MT292 gate can only check the shape and the
// schedule state (README.md), never the rendering record. Today
// no :20: can be proven to be ours: the MT210 notice is the
// premise the MT292 cancels and the MT295 asks about, yet the
// ledger keeps no record of having rendered it. Registering the
// export closes that gap for the outbound side — the :20: names
// one latest notice, a replay returns the stored message, and a
// :20: reused for different terms is refused — so the MT292
// cancellation gate and the future MT295 MT210 tier can anchor
// against this registry instead of the shape alone. Those tiers
// are later items: this item only builds the registry they will
// consult.
//
// The :20: is the schedule ID itself, stable across runs — the
// one semantic difference from the MT202 (LG-470) and MT292
// (LG-471) registries, whose :20: values are caller-supplied and
// run-derived respectively. The key therefore does not pin the
// run, and the registry holds the latest issue, not one run:
// the same :20: with the same run sequence and the same terms is
// a Duplicate (the stored message, no second audit event); the
// same :20: with a new run sequence supersedes (the old record is
// overwritten by the new run's notice, with a fresh
// mt210_exported audit event, Duplicate=false — the notice's
// meaning is "the next issue", so a run advancing is not a
// conflict); the same :20: with the same run sequence but
// different terms is refused with ErrMT210ExportMsgIDConflict.
// The terms check only has to cover what can still vary under one
// :20: and one run: the servicing BIC (compared in its rendered
// form, uppercased and trimmed, along the MT202/MT292
// precedent). The sender/receiver LTs are envelope, not terms,
// along the MT196 precedent.
//
// Scope honesty, the same limit the MT202/MT292 registries carry:
// registration proves only that this ledger rendered the message
// — not that it was sent (block 1 session/sequence are
// zero-filled, assigned by the sender's SWIFT interface on
// transmission, mt210.go) and not that the beneficiary bank
// honoured it (that is confirmed by its reply, the inbound
// MT296). The registry is in-memory only, like mt292Exports: the
// hash-chained mt210_exported audit events are the durable
// record, and a restart clears it — a late MT292 answer arriving
// after a restart falls back to the shape gate, exactly as
// before.
//
// The export renders only: no journal entry, no version bump —
// rendering a notice to receive is read-only, and the audit event
// is the durable record, not a state change.

// MT210 outbound export errors.
var (
	// ErrMT210ExportMsgIDConflict is returned when a :20:
	// reference already registered for an outbound MT210 export
	// is reused for different terms. It is a 422-class error:
	// the export was already rendered once, and the :20: names
	// that one latest notice. It is not the inbound
	// ErrMT210MsgIDConflict (mt210_inbound.go): that sentinel's
	// wording is the inbound reading ("already registered for a
	// different notification"), and reusing it for an export
	// conflict would mislead the operator.
	ErrMT210ExportMsgIDConflict = errors.New("ledger: mt210: :20: already exported with different terms")
)

// MT210ExportResult is the outcome of ExportMT210Registered: the
// rendered notice to receive and the facts it was rendered from.
type MT210ExportResult struct {
	// Reference echoes the export's :20: reference — the
	// schedule ID.
	Reference string `json:"reference"`
	// ScheduleID is the transfer schedule whose next run is
	// notified.
	ScheduleID string `json:"schedule_id"`
	// RunSeq is the run notified by this issue of the notice.
	RunSeq uint64 `json:"run_seq"`
	// Message is the rendered SWIFT FIN MT210 text.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt210ExportRecord is the registry record for one rendered MT210
// notice, keyed by :20: (the schedule ID) in mt210Exports: the
// schedule and run it notifies, the options it was rendered with,
// and the result, so a replay returns the same message without
// rendering twice — and so a later run's export can supersede it
// with the next issue.
type mt210ExportRecord struct {
	ScheduleID string
	RunSeq     uint64
	Options    MT210ExportOptions
	Result     MT210ExportResult
}

// mt210ExportTermsEqual reports whether a stored export record
// carries exactly the terms an export request would render: the
// same servicing BIC (in its rendered form — uppercased and
// trimmed, so a replay differing only in BIC case or
// surrounding space is the same terms). The schedule ID is the
// :20: key itself and is not compared; the run sequence is
// compared by the caller, not here, because a new run supersedes
// rather than conflicts.
func mt210ExportTermsEqual(rec mt210ExportRecord, opts MT210ExportOptions) bool {
	return strings.ToUpper(strings.TrimSpace(rec.Options.ServicingBIC)) ==
		strings.ToUpper(strings.TrimSpace(opts.ServicingBIC))
}

// ExportMT210Registered renders the next run of the transfer
// schedule as a SWIFT FIN MT210 notice to receive and registers
// the export under its :20: (the schedule ID, see mt210Exports),
// so the ledger can later confirm it rendered that notice.
//
// Idempotency is the :20: with its run and terms (the pain.014
// convention): a replay with the same reference, the same run
// sequence, and the same terms returns the stored rendered
// message with Duplicate set, rendering nothing twice and
// emitting no second audit event; a :20: reused for different
// terms at the same run is refused with
// ErrMT210ExportMsgIDConflict; a :20: re-exported after its run
// fired supersedes the stored notice with the new run's issue,
// Duplicate=false.
//
// Nothing moves: no journal entry, no version bump — rendering a
// notice to receive is read-only, and the audit event is the
// durable record, not a state change. The audit event is
// mt210_exported (details msg_id / schedule_id / run_seq /
// sender_lt, trace :20:, no accounts: a notice to receive
// touches no ledger account).
//
// Locking: the run sequence is read first (the replay check needs
// it before anything is rendered); the replay read runs under the
// read lock; the render runs outside any held lock (it takes the
// read lock itself for the party lookup); the :20: record and the
// export marker event run under a short write lock of their own,
// re-checking the registry first so a concurrent double-export
// still records once, and re-checking the run sequence by reading
// l.schedules directly — GetTransferSchedule takes the read lock,
// which would deadlock under the write lock held here, and there
// is no locked accessor. The run-sequence recheck is defence in
// depth only: the sequence is frozen while a schedule is in
// MT210-exportable state (paused/cancelled/completed schedules
// never fire, and an exhausted active schedule fires no new
// run), so it can only trip if the schedule moved between the
// render and this lock.
func (l *Ledger) ExportMT210Registered(opts MT210ExportOptions, actor string) (MT210ExportResult, error) {
	if actor == "" {
		actor = "ExportMT210Registered"
	}

	// :20: — the schedule ID. Read the run sequence first: the
	// replay check below needs it before anything is rendered.
	sched, err := l.GetTransferSchedule(opts.ScheduleID)
	if err != nil {
		return MT210ExportResult{}, err
	}
	runSeq := sched.RunSeq
	ref := opts.ScheduleID

	// :20: replay: the notice was already rendered under this
	// reference for this run. Same terms is a no-op returning the
	// stored message; different terms is a conflict. A stored
	// record for an older run is not a replay: the run has fired
	// since, and the render below supersedes it.
	l.mu.RLock()
	rec, ok := l.mt210Exports[ref]
	l.mu.RUnlock()
	if ok && rec.RunSeq == runSeq {
		if !mt210ExportTermsEqual(rec, opts) {
			return MT210ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT210ExportMsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}

	// The render (pure render, unchanged): the internal form also
	// returns the sender LT for the audit event and the run
	// sequence it rendered for.
	msg, senderLT, renderedSeq, err := l.exportMT210(opts)
	if err != nil {
		return MT210ExportResult{}, err
	}
	// A render for a different run means the schedule moved
	// between the read above and the render: the rendered message
	// names a run that is no longer the next one, and registering
	// it would record a stale export. Refuse and let the caller
	// retry with fresh state.
	if renderedSeq != runSeq {
		return MT210ExportResult{}, fmt.Errorf("ledger: mt210 export for schedule %q: run sequence changed during export", opts.ScheduleID)
	}
	res := MT210ExportResult{
		Reference:  ref,
		ScheduleID: opts.ScheduleID,
		RunSeq:     runSeq,
		Message:    msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker; a concurrent
	// export for a newer run cannot have landed first without the
	// schedule having advanced, which the run-sequence recheck
	// below refuses. The audit event carries no accounts: a notice
	// to receive touches no ledger account, and inventing one
	// would misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt210Exports[ref]; ok && rec.RunSeq == runSeq {
		l.mu.Unlock()
		if !mt210ExportTermsEqual(rec, opts) {
			return MT210ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT210ExportMsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if s, ok := l.schedules[opts.ScheduleID]; !ok || s.RunSeq != runSeq {
		l.mu.Unlock()
		return MT210ExportResult{}, fmt.Errorf("ledger: mt210 export for schedule %q: run sequence changed during export", opts.ScheduleID)
	}
	if l.mt210Exports == nil {
		l.mt210Exports = make(map[string]mt210ExportRecord)
	}
	// The options are stored verbatim: the :20: names one latest
	// notice, and a replay compares against the registered terms,
	// never a re-read of live facts. A record for an older run is
	// overwritten here: the registry holds the latest issue.
	l.mt210Exports[ref] = mt210ExportRecord{
		ScheduleID: opts.ScheduleID,
		RunSeq:     runSeq,
		Options:    opts,
		Result:     res,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt210_exported",
		Actor:         actor,
		TraceID:       ref,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":      ref,
			"schedule_id": opts.ScheduleID,
			"run_seq":     runSeq,
			"sender_lt":   senderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT210Export returns the recorded export for msgID (its :20:,
// the schedule ID), or false when no notice to receive was ever
// rendered under it. The returned result is a copy — callers
// cannot mutate ledger state through it.
func (l *Ledger) GetMT210Export(msgID string) (MT210ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt210Exports[msgID]
	if !ok {
		return MT210ExportResult{}, false
	}
	return rec.Result, true
}
