package ledger

import (
	"fmt"
	"strings"
)

// SWIFT FIN MT202 outbound export registration (LG-470): the
// outbound half of the Category 2 transfer leg's paper trail. The
// pure render (ExportMT202, LG-204) proves nothing about what this
// ledger has already sent: the same :20: rendered twice is two
// indistinguishable messages, and a correspondent answering with an
// MT296 names a :20: the ledger cannot confirm it ever rendered.
// Registering the export closes that gap for the outbound side —
// the :20: names one export, a replay returns the stored message,
// and a :20: reused for different terms is refused.
//
// The :20: is caller-supplied at render time (mt202.go): unlike
// the MT292/MT295 references it is not derived, so the registry is
// keyed by the caller's own reference and the terms check is the
// only guard against two different transfers sharing it. The terms
// are the MT202 full field set except the sender/receiver LTs,
// which are envelope, along the MT195/MT196 precedent: a replay
// under different LTs still returns the first rendering, because
// the :20: names one export. The :52A:/:58A: BICs compare
// case-insensitively: the render uppercases them, so a replay
// differing only in BIC case renders the byte-identical message
// and is the same terms.
//
// Scope honesty, the same limit the MT292 registry (LG-471) will
// carry: registration proves only that this ledger rendered the
// message — not that it was sent (block 1 session/sequence are
// zero-filled, assigned by the sender's SWIFT interface on
// transmission, mt202.go) and not that the beneficiary bank
// honoured it (that is confirmed by its reply, the inbound MT296).
// The registry is in-memory only, like mt295Exports: the
// hash-chained mt202_exported audit events are the durable record,
// and a restart clears it — a late MT296 answer arriving after a
// restart falls back to the shape gate, exactly as before.
//
// The export renders only: no journal entry, no version bump —
// rendering a transfer is read-only, and the audit event is the
// durable record, not a state change. No MT296 tier anchors
// against this registry yet: a :21: naming an outbound MT202's
// :20: is still fail-closed by the inbound MT295 module
// (mt295_inbound.go), and the tier that would consult this
// registry is a later item.

// MT202ExportResult is the outcome of ExportMT202Registered: the
// rendered transfer message and the facts it was rendered from.
type MT202ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the :21: related reference.
	RelatedReference string `json:"related_reference"`
	// Message is the rendered SWIFT FIN MT202 text.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt202ExportTermsEqual reports whether a stored export record
// carries exactly the terms an export request would render: the
// same :21:, value date, currency, amount, and institution BICs.
// The sender/receiver LTs are envelope, not terms, along the
// MT196 precedent — a replay under different LTs still returns
// the first rendering, because the :20: names one export. The
// BICs compare case-insensitively: the render uppercases them,
// so a case-only difference renders the same message.
func mt202ExportTermsEqual(rec mt202ExportRecord, m MT202) bool {
	if rec.Request.RelatedReference != m.RelatedReference {
		return false
	}
	if !rec.Request.ValueDate.Equal(m.ValueDate) {
		return false
	}
	if rec.Request.Currency != m.Currency {
		return false
	}
	if rec.Request.AmountCents != m.AmountCents {
		return false
	}
	if !strings.EqualFold(rec.Request.OrderingInstitutionBIC, m.OrderingInstitutionBIC) {
		return false
	}
	if !strings.EqualFold(rec.Request.BeneficiaryInstitutionBIC, m.BeneficiaryInstitutionBIC) {
		return false
	}
	return true
}

// ExportMT202Registered renders m as a SWIFT FIN MT202 message and
// registers the export under its :20: (see mt202Exports), so the
// ledger can later confirm it rendered that transfer.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same reference and the same terms
// returns the stored rendered message with Duplicate set,
// rendering nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT202ExportMsgIDConflict.
//
// Nothing moves: no journal entry, no version bump — rendering a
// transfer is read-only, and the audit event is the durable
// record, not a state change. The audit event is mt202_exported
// (details msg_id / related_reference / sender_lt, trace :20:,
// no accounts: a transfer touches no ledger account).
//
// Locking: the validation runs on the supplied values alone; the
// replay read runs under the read lock; the :20: record and the
// export marker event run under a short write lock of their own,
// re-checking the registry first so a concurrent double-export
// still records once.
func (l *Ledger) ExportMT202Registered(m MT202, actor string) (MT202ExportResult, error) {
	if actor == "" {
		actor = "ExportMT202Registered"
	}
	senderLT, _, err := mt202Validate(m)
	if err != nil {
		return MT202ExportResult{}, err
	}

	// :20: — the caller-supplied transaction reference, also the
	// MUR and the registry key.
	ref := m.TransactionReference

	// :20: replay: the transfer was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt202Exports[ref]; ok {
		l.mu.RUnlock()
		if !mt202ExportTermsEqual(rec, m) {
			return MT202ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT202ExportMsgIDConflict, ref)
		}
		return MT202ExportResult{
			Reference:        ref,
			RelatedReference: rec.Request.RelatedReference,
			Message:          rec.Message,
			Duplicate:        true,
		}, nil
	}
	l.mu.RUnlock()

	msg, err := ExportMT202(m)
	if err != nil {
		return MT202ExportResult{}, err
	}
	res := MT202ExportResult{
		Reference:        ref,
		RelatedReference: m.RelatedReference,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// event carries no accounts: a transfer touches no ledger
	// account, and inventing one would misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt202Exports[ref]; ok {
		l.mu.Unlock()
		if !mt202ExportTermsEqual(rec, m) {
			return MT202ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT202ExportMsgIDConflict, ref)
		}
		return MT202ExportResult{
			Reference:        ref,
			RelatedReference: rec.Request.RelatedReference,
			Message:          rec.Message,
			Duplicate:        true,
		}, nil
	}
	if l.mt202Exports == nil {
		l.mt202Exports = make(map[string]mt202ExportRecord)
	}
	// The request is stored verbatim: the :20: names one export,
	// and a replay compares against the registered terms, never a
	// re-read of live facts.
	l.mt202Exports[ref] = mt202ExportRecord{
		Message: msg,
		Request: m,
		Actor:   actor,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt202_exported",
		Actor:         actor,
		TraceID:       ref,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            ref,
			"related_reference": m.RelatedReference,
			"sender_lt":         senderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT202Export returns the recorded export for msgID (its :20:),
// or false when no transfer was ever rendered under it. The
// returned result is a copy — callers cannot mutate ledger state
// through it.
func (l *Ledger) GetMT202Export(msgID string) (MT202ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt202Exports[msgID]
	if !ok {
		return MT202ExportResult{}, false
	}
	return MT202ExportResult{
		Reference:        msgID,
		RelatedReference: rec.Request.RelatedReference,
		Message:          rec.Message,
	}, true
}
