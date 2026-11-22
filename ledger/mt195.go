package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT195 queries export (LG-460): the outbound half of
// the MT195 lifecycle. A correspondent bank asked about a payment
// with an MT195 query (see mt195_inbound.go, LG-453); this is the
// mirror direction — the ledger itself asks a correspondent bank
// about a payment it sent ("where is this MT103?"), because the
// inbound half alone only proves the ledger can answer questions,
// never that it can ask one. The query names the payout and asks
// where it stands; the answer comes back as an MT196 (see mt196.go,
// LG-457), which refers back to this export by its :20: through
// the :21: — the "ask—answer" loop closes across the two modules.
//
// Field set, verified against the inbound side's already-checked
// core set (see mt195_inbound.go, LG-453):
//
//   - :20: Transaction Reference Number (16x, mandatory) — this
//     export's own reference, derived deterministically (see
//     mt195MessageReference), the replay key.
//   - :21: Related Reference (16x, mandatory) — the original
//     MT103's :20:, the payout ID, the anchor ParseMT192 uses
//     for :21:.
//   - :75: Queries (6*35x, optional) — the coded question (STAT /
//     DUPL / PRTY and friends, carried verbatim, never
//     interpreted).
//   - :79: Narrative (50*35x, optional) — the free-format
//     description of the question, the MT199 :79: shape (1–50
//     lines of 1–35 `x` characters each).
//
// At least one of :75: / :79: is required: a query with no
// question text asks nothing. :77A: (new priority, for a PRTY
// change) and :11S: (MT and date of the original message) are
// outside the core set, along the inbound precedent: a priority
// change is a different action from a status query, and the value
// date is taken from the ledger, not the wire — both are refused,
// never silently dropped. A copy of the original message's
// mandatory fields (rule C1's other arm) is likewise not rendered.
//
// Identity wiring — scope honesty, the ledger is not a SWIFT member
// (see mt192.go, LG-434):
//   - The sender's BIC comes from the payee account's registered
//     IBAN BIC (see SetAccountIBAN, LG-150): the operator registers
//     the bank's own BIC once, instead of the export inventing one.
//     No BIC anywhere is a fail-closed refusal, not a guessed value.
//   - The receiver's LT address is always operator-supplied (the
//     correspondent the original MT103 went to); the ledger records
//     nowhere where an MT103 was sent, so the caller names the
//     correspondent on every export. The ledger keeps no routing
//     table, so it never picks a correspondent on its own.
//   - :20: is a deterministic derivation — "MT195-" plus the first
//     10 lowercase hex characters of the SHA-256 of the payout ID
//     and the question body — 16 characters, 16x legal (the MT192
//     :20: precedent, LG-434), so repeat exports render
//     byte-identical and the reference is stable per payout and
//     question without inventing a sequence number. A different
//     question about the same payout hashes differently, so it is
//     a different export under a different :20:.
//
// Framing follows the MT196 convention: block 1 (basic header,
// sender LT, session/sequence zero-filled and interface-assigned),
// block 2 (application header, receiver LT, `I195`), block 3
// (`{108:MUR}` carrying the :20: reference), block 4 (the field
// set above, in canonical order), no block 5 (CHK is computed by
// the SWIFT network on receipt). Lines are joined with "\n", the
// nacha.go convention.
//
// The export renders only: no journal entry, no payout status
// change, no gpi advance, no version bump — the audit event is the
// durable record, not a state change. An unknown :21: is
// fail-closed into the repair queue (RepairReasonUnknownRoute, the
// camt.055 shape, along the MT195 unknown-:21: precedent): the
// returned result carries the repair ID alongside the
// ErrMT103PayoutNotFound error, and no export is recorded — a
// re-sent export parks again, or renders, once the payout exists.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416, along the inbound mt195TermsEqual): the export's own
// reference names one export. Re-exporting the same payout with
// the same question returns the stored rendered message with
// Duplicate set, rendering nothing twice and emitting no second
// audit event. A :20: reused for different terms is refused with
// ErrMT195MsgIDConflict — never silently overwritten. The
// receiver LT is envelope, not terms, along the MT196 precedent:
// a replay under a different LT still returns the first rendering,
// because the :20: names one export. The registry check runs again
// under the write lock after the message is rendered, so two
// concurrent exports of the same :20: still record once.

// MT195 outbound export errors.
var (
	// ErrMT195Export is returned when the export cannot be
	// rendered for a reason outside the field errors: no sender
	// BIC registered for the payee account, or a missing or
	// invalid receiver LT.
	ErrMT195Export = errors.New("ledger: invalid MT195 export")
)

// MT195ExportOptions names the question of one MT195 query
// export: the correspondent to ask and the question itself. The
// payout is the first argument of ExportMT195, not an option: it
// is the lookup key (:21:), and keeping it out of the options
// makes the anchor explicit at the call site. The sender LT is
// not an option either: it is derived from the payee account's
// registered IBAN BIC (see the package doc). The :20: is not an
// option: it is derived from the payout and the question (see
// mt195MessageReference).
type MT195ExportOptions struct {
	ReceiverLT string
	Queries    string // :75:, the coded question, "\n"-joined, 1-6 lines.
	Narrative  string // :79:, the question in words, "\n"-joined, 1-50 lines.
}

// MT195ExportResult is the outcome of ExportMT195: the rendered
// query message and the facts it was rendered from. A parked
// export (unknown :21:) carries only Reference,
// RelatedReference, PayoutID, and RepairID, alongside the
// ErrMT103PayoutNotFound error.
type MT195ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the queried payout's ID (:21:).
	RelatedReference string `json:"related_reference"`
	// PayoutID is the payout the query is about.
	PayoutID string `json:"payout_id"`
	// Message is the rendered SWIFT FIN MT195 text.
	Message string `json:"message"`
	// RepairID is the repair queue record for a parked export
	// (unknown :21:). Empty on a rendered export.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt195ExportRecord is the registry record for one rendered MT195
// query, keyed by :20: in mt195Exports: the payout it asks about,
// the options it was rendered with, and the result, so a replay
// returns the same message without rendering twice.
type mt195ExportRecord struct {
	PayoutID string
	Options  MT195ExportOptions
	Result   MT195ExportResult
}

// mt195ExportTermsEqual reports whether a stored export record carries
// exactly the terms an export request would render: the same
// payout and the same question (:75: and :79:, verbatim). The
// receiver LT is envelope, not terms, along the MT196 precedent —
// a replay under a different LT still returns the first rendering,
// because the :20: names one export.
func mt195ExportTermsEqual(rec mt195ExportRecord, payoutID string, opts MT195ExportOptions) bool {
	return rec.PayoutID == payoutID &&
		rec.Options.Queries == opts.Queries &&
		rec.Options.Narrative == opts.Narrative
}

// mt195MessageReference derives the deterministic :20: reference:
// "MT195-" plus the first 10 lowercase hex characters of the
// SHA-256 of the payout ID and the question body, joined with
// "\n" — 16 characters, 16x legal (the MT192 :20: precedent,
// LG-434). The separator keeps "po-1" + "STAT" distinct from
// "po-1\nSTAT" splits that would otherwise hash the same; the
// question body is the :75: and :79: verbatim, so the same payout
// asked the same question always derives the same reference, and
// a different question derives a different one.
func mt195MessageReference(payoutID, queries, narrative string) string {
	sum := sha256.Sum256([]byte(payoutID + "\n" + queries + "\n" + narrative))
	return "MT195-" + hex.EncodeToString(sum[:])[:10]
}

// validateMT195ExportOptions fail-closes on options ExportMT195
// would refuse, so the render gate and the export gate share one
// check, never a copy. It returns the normalized receiver LT.
func validateMT195ExportOptions(payoutID string, opts MT195ExportOptions) (string, error) {
	if err := mt195Reference(payoutID, ":21:"); err != nil {
		return "", err
	}
	if opts.ReceiverLT == "" {
		return "", fmt.Errorf("%w: ReceiverLT is required", ErrMT195Export)
	}
	receiverLT, err := mt103NormalizeLT(opts.ReceiverLT)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT: %v", ErrMT195Export, err)
	}
	if strings.TrimSpace(opts.Queries) == "" && strings.TrimSpace(opts.Narrative) == "" {
		return "", fmt.Errorf("%w: a query must carry :75: or :79:", ErrMT195Field)
	}
	if opts.Queries != "" {
		if err := mt195ParseTextLines(strings.Split(opts.Queries, "\n"), ":75:", mt195MaxQueriesLines); err != nil {
			return "", err
		}
	}
	if opts.Narrative != "" {
		if err := mt195ParseTextLines(strings.Split(opts.Narrative, "\n"), ":79:", mt195MaxNarrativeLines); err != nil {
			return "", err
		}
	}
	return receiverLT, nil
}

// parkMT195Export parks a fail-closed export request in the repair
// queue and returns the record ID. The snapshot names the queried
// payout (:21:) as the instruction ID and the export (:20:) as
// the destination, so an operator can find the export from the
// record. The snapshot's Maker is deliberately empty (see the
// MT195 package comment): the record is operator visibility, never
// a resubmittable instruction. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the bank's question.
func (l *Ledger) parkMT195Export(ref, payoutID, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          payoutID,
		Destination: "mt195-export:" + ref,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ExportMT195 renders the SWIFT FIN MT195 query about payoutID,
// asking a correspondent bank where the payout's MT103 stands:
// the :21: names the payout, the :20: (see
// mt195MessageReference) is this export's own reference, and the
// :75: / :79: carry the question. The sender LT is the payee
// account's registered IBAN BIC; the receiver LT is named by the
// caller on every export.
//
// The payout must exist (an unknown :21: is
// ErrMT103PayoutNotFound, parked in the repair queue fail-closed
// in the camt.055 shape: the returned result carries the repair
// ID alongside the error). Nothing moves: no journal entry, no
// payout status change, no gpi advance, no version bump —
// rendering a question is read-only, and the audit event is the
// durable record, not a state change. No payout status gate is
// applied: a query asks about a payment, it does not move it, so
// any known payout may be asked about, along the inbound
// ApplyMT195 precedent (which answers any existing payout).
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same payout and question returns
// the stored rendered message with Duplicate set, rendering
// nothing twice and emitting no second audit event; a :20:
// reused for different terms is refused with
// ErrMT195MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the payout/IBAN read run under the read lock;
// the :20: record and the export marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-export still records once.
func (l *Ledger) ExportMT195(payoutID string, opts MT195ExportOptions, actor string) (MT195ExportResult, error) {
	if actor == "" {
		actor = "ExportMT195"
	}
	receiverLT, err := validateMT195ExportOptions(payoutID, opts)
	if err != nil {
		return MT195ExportResult{}, err
	}

	// :20: — the deterministic message reference, also the MUR.
	ref := mt195MessageReference(payoutID, opts.Queries, opts.Narrative)

	// :20: replay: the query was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt195Exports[ref]; ok {
		l.mu.RUnlock()
		if !mt195ExportTermsEqual(rec, payoutID, opts) {
			return MT195ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT195MsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The payout itself: :21: names it, the original MT103's
	// :20:. Unknown is parked fail-closed in the repair queue
	// (the camt.055 shape) and reported as the export's error.
	// The sender BIC is the payee account's registered IBAN BIC,
	// read with the payout under the same lock.
	l.mu.RLock()
	p, ok := l.payouts[payoutID]
	var senderBIC string
	if ok {
		if reg, ok := l.accountIBANs[p.PayeeAccount]; ok {
			senderBIC = reg.BIC
		}
	}
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT195Export(ref, payoutID, actor)
		if parkErr != nil {
			return MT195ExportResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT103PayoutNotFound, payoutID, parkErr)
		}
		return MT195ExportResult{Reference: ref, RelatedReference: payoutID, PayoutID: payoutID, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, payoutID)
	}

	// Sender BIC: the payee account's registered IBAN BIC. A SWIFT
	// query without a sender is refused, never guessed.
	if senderBIC == "" {
		return MT195ExportResult{}, fmt.Errorf("%w: sender BIC is not configured: register the payee account's BIC (SetAccountIBAN)", ErrMT195Export)
	}
	senderLT, err := mt103NormalizeLT(senderBIC)
	if err != nil {
		return MT195ExportResult{}, fmt.Errorf("%w: sender LT: %v", ErrMT195Export, err)
	}

	var b strings.Builder
	// Block 1: basic header. Session/sequence are zero-filled —
	// assigned by the sender's SWIFT interface on transmission.
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT. Priority U
	// (urgent), delivery monitoring 3 (non-delivery warning and
	// delivery notification), obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I195%sU3003}\n", receiverLT)
	// Block 3: user header — the Message User Reference ties the
	// message back to the payout and the question.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", ref)
	// Block 4: text, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", ref)
	fmt.Fprintf(&b, ":21:%s\n", payoutID)
	if opts.Queries != "" {
		fmt.Fprintf(&b, ":75:%s\n", opts.Queries)
	}
	if opts.Narrative != "" {
		fmt.Fprintf(&b, ":79:%s\n", opts.Narrative)
	}
	b.WriteString("-}")
	msg := b.String()
	res := MT195ExportResult{
		Reference:        ref,
		RelatedReference: payoutID,
		PayoutID:         payoutID,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.mt195Exports[ref]; ok {
		l.mu.Unlock()
		if !mt195ExportTermsEqual(rec, payoutID, opts) {
			return MT195ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT195MsgIDConflict, ref)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt195Exports == nil {
		l.mt195Exports = make(map[string]mt195ExportRecord)
	}
	l.mt195Exports[ref] = mt195ExportRecord{PayoutID: payoutID, Options: opts, Result: res}
	l.emitAudit(AuditEvent{
		Op:            "mt195_query_exported",
		Actor:         actor,
		TraceID:       ref,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"msg_id":            ref,
			"related_reference": payoutID,
			"payout_id":         payoutID,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT195Export returns the recorded export for msgID, or false
// when no query was ever rendered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT195Export(msgID string) (MT195ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt195Exports[msgID]
	if !ok {
		return MT195ExportResult{}, false
	}
	return rec.Result, true
}
