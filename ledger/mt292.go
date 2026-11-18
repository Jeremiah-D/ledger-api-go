package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT292 cancellation of notification to receive export
// (LG-435, fintech: SWIFT 收款通知撤销请求).
//
// ExportMT292 renders the cancellation request for the MT210 the
// ledger already sent (see mt210.go, LG-393) — the FIN twin of the
// camt.058 cancellation advice (LG-404): same source (the same
// transfer schedule's next run, see schedule.go, LG-41), same
// state gate, same RunSeq reference, one FIN, one MX. The field
// set follows the SWIFT MT292 table: :20: this message's own
// reference, :21: the related reference (the original MT210's
// :20:, i.e. the schedule ID), :11S: the original message type
// (210) and its value date, and :79: an optional free-text
// narrative.
//
// The export is the cancellation *request*, not the cancellation
// itself: the schedule keeps its status until the bank confirms,
// and the ledger parses no reply to this export — whether the
// bank honours the request is confirmed by its own reply,
// outside this ledger. The reverse direction is a different
// flow: a counterparty's MT292 cancelling one of its MT210
// notices to this ledger is parsed and applied by ParseMT292 /
// ApplyMT292 (see mt292_inbound.go, LG-452) — that one withdraws
// a notification this ledger registered, it is not a reply to
// this export. Cancelling the notification is not cancelling the plan:
// the schedule's state machine does not move, and a paused plan
// resumes with its next run intact. To correct a notice instead
// of withdrawing it, pause the plan, cancel the old notice,
// resume the plan, and re-issue a new MT210 (the camt.058
// operator flow, LG-404).
//
// Identity wiring — scope honesty, the ledger is not a SWIFT member:
//   - The sender is the To account (the beneficiary), like the
//     MT210: its registered BIC when one exists, else the internal
//     account ID, uppercased (see mt300PartyIdentifierLocked) —
//     through mt103NormalizeLT, so a To account without a
//     registered BIC needs an LT-form account ID (8, 11, or 12
//     alphanumeric characters) to export at all.
//   - The receiver is the servicing bank named by the caller
//     (ServicingBIC): the ledger records neither whether an MT210
//     was actually sent nor where it went, so the caller names
//     the receiving line on every export. The ledger keeps no
//     routing table, so it never picks a correspondent on its own.
//   - :20: is a deterministic derivation — "MT292-" plus the first
//     10 lowercase hex characters of the SHA-256 of the run
//     transfer ID ("<ID>/run/<seq>", the MT210's :21:) — 16
//     characters, 16x legal (the MT101 :20: precedent, LG-391),
//     so repeat exports render byte-identical and the reference is
//     stable per schedule and run without inventing a sequence
//     number.
//   - :11S: reuses mt103ValueDate (mt103.go): "210" plus the
//     original MT210's issue date in YYMMDD. The ledger records
//     no MT210 issue date (the camt.057 limitation, LG-404), so
//     the caller supplies OriginalDate on every export — it is
//     required, never defaulted, and never substituted with
//     NextRunAt or any derived date.
//   - :79: is free text — no reason-code table is invented — but
//     it is still validated (at most 35 lines of 35 `x`
//     characters, the SWIFT :79: shape, the MT192 precedent,
//     LG-434).
//
// Lifecycle honesty: only a schedule whose notification premise
// is dead can be cancelled — paused, cancelled, completed, and
// an active schedule whose NextRunAt has already passed its
// EndsAt. An active schedule with a live next run reports
// ErrMT292ScheduleStillActive: the money is still coming, so
// withdrawing the notice would mislead liquidity planning. An FX
// schedule (ToCurrency set) is revocable like any other: the
// MT292 carries no amount, so the unknowable-until-fire-time FX
// amount is never written down (the camt.058 precedent, LG-404).
//
// Wire honesty notes:
//   - Block 3 carries `{108:<MUR>}` with the :20: reference, tying
//     the message back to the schedule and run (the MT103
//     convention).
//   - Block 5 (trailer) is omitted: CHK is computed by the SWIFT
//     network on receipt; a generator emitting a fake checksum would
//     be dishonest.
//   - Block 1 session/sequence are zero-filled: they are assigned by
//     the sender's SWIFT interface on transmission, and the export
//     documents them as interface-assigned rather than inventing
//     plausible numbers.
//   - Lines are joined with "\n", the nacha.go convention; convert to
//     CRLF for the bank if the uploader requires it.
//
// Read-only: the schedule and the party identifiers are read as
// snapshot copies under the read lock — no version bump, no journal
// rows, no audit event — the same contract as ExportMT210 and
// ExportCAMT058. The export is deterministic per schedule and
// options: repeat exports render byte-identical output, a
// statement of fact, not an attempt sequence.

// MT292 outbound export errors.
var (
	// ErrInvalidMT292Export is returned when a schedule cannot be
	// rendered as an MT292: a missing or invalid ServicingBIC, a
	// missing OriginalDate, a :21: reference outside 16x, a party
	// identifier that is not a valid LT form, or a reason outside
	// the :79: shape. It is a 400-class error: the stored schedule
	// cannot be expressed as a valid cancellation request.
	ErrInvalidMT292Export = errors.New("ledger: invalid MT292 export")
	// ErrMT292ScheduleStillActive is returned when an MT292 is
	// requested for a schedule whose next run is still live: an
	// active schedule whose NextRunAt has not passed its EndsAt.
	// It is a 422-class error: the schedule exists, but there is
	// still money coming, so withdrawing the notice would be a
	// false statement.
	ErrMT292ScheduleStillActive = errors.New("ledger: transfer schedule is still active for MT292 cancellation request")
)

// MT292 :79: narrative shape (SWIFT field 79: 35x * 35 lines, the
// MT192 precedent, LG-434).
const (
	mt292MaxReasonLine  = 35
	mt292MaxReasonLines = 35
)

// MT292ExportOptions tunes ExportMT292.
//
// ScheduleID names the transfer schedule whose next run's MT210
// notification is cancelled. ServicingBIC is the BIC of the bank
// holding the account to be credited (required, 8 or 11
// characters): the receiving line of the original MT210, named by
// the caller on every export. OriginalDate is the original MT210's
// issue date (required): the ledger records no MT210 issue date,
// so the caller supplies it — it is never defaulted from
// NextRunAt or any other stored date. Reason is the operator's
// free-form cancellation reason (optional): when empty, the :79:
// field is omitted.
type MT292ExportOptions struct {
	ScheduleID   string
	ServicingBIC string
	OriginalDate time.Time
	Reason       string
}

// mt292MessageReference derives the deterministic :20: reference:
// "MT292-" plus the first 10 lowercase hex characters of the
// SHA-256 of the run transfer ID ("<ID>/run/<seq>", the MT210's
// :21:) — 16 characters, 16x legal (the MT101 :20: precedent,
// LG-391).
func mt292MessageReference(scheduleID string, runSeq uint64) string {
	runID := fmt.Sprintf("%s/run/%d", scheduleID, runSeq)
	sum := sha256.Sum256([]byte(runID))
	return "MT292-" + hex.EncodeToString(sum[:])[:10]
}

// mt292RelatedReference validates the :21: related reference — the
// schedule ID, i.e. the original MT210's :20:: 1–16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash. It mirrors
// mt210TransactionReference but wraps ErrInvalidMT292Export — the
// MT210 helper's error would map to the wrong message type in the
// handler.
func mt292RelatedReference(field, ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s want 1-%d characters, got %q", ErrInvalidMT292Export, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s line break in a single-line field: %q", ErrInvalidMT292Export, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s %w", ErrInvalidMT292Export, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s leading/trailing colon or slash, or double slash: %q", ErrInvalidMT292Export, field, ref)
	}
	return nil
}

// mt292ReasonLines validates the :79: narrative: at most 35 lines
// of at most 35 SWIFT `x` characters each (braces refused, the
// block-4 framing rule). It returns the lines to render.
func mt292ReasonLines(reason string) ([]string, error) {
	lines := strings.Split(reason, "\n")
	if len(lines) > mt292MaxReasonLines {
		return nil, fmt.Errorf("%w: :79: at most %d lines", ErrInvalidMT292Export, mt292MaxReasonLines)
	}
	for _, ln := range lines {
		if len(ln) > mt292MaxReasonLine {
			return nil, fmt.Errorf("%w: :79: line exceeds %d characters: %q", ErrInvalidMT292Export, mt292MaxReasonLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return nil, fmt.Errorf("%w: :79: line: %v", ErrInvalidMT292Export, err)
		}
	}
	return lines, nil
}

// ExportMT292 renders the next run of the transfer schedule as a
// SWIFT FIN MT292 cancellation of notification to receive,
// asking the servicing bank to withdraw the MT210 already sent
// for that run. Unknown IDs report ErrScheduleNotFound; a
// still-active schedule with a live next run reports
// ErrMT292ScheduleStillActive. Only a paused, cancelled,
// completed, or exhausted schedule exports — the deliberate
// opposite of MT210's active-only gate, and the same gate as
// camt.058 (LG-404).
func (l *Ledger) ExportMT292(opts MT292ExportOptions) (string, error) {
	// Caller-given options fail fast, before any schedule lookup:
	// a servicing bank that is not a valid BIC can never be a
	// valid cancellation request, and an original date the caller
	// never gave cannot be invented from the schedule — the
	// ledger records no MT210 issue date.
	servicing := strings.ToUpper(strings.TrimSpace(opts.ServicingBIC))
	if servicing == "" || !validBIC(servicing) {
		return "", fmt.Errorf("%w: servicing BIC %q: want a valid 8/11-character BIC", ErrInvalidMT292Export, opts.ServicingBIC)
	}
	if opts.OriginalDate.IsZero() {
		return "", fmt.Errorf("%w: original date is required: the ledger records no MT210 issue date", ErrInvalidMT292Export)
	}
	sched, err := l.GetTransferSchedule(opts.ScheduleID)
	if err != nil {
		return "", err
	}
	// The gate is the deliberate opposite of MT210's: only a
	// schedule whose notification premise is dead can be
	// cancelled. An active schedule is revocable only once its
	// next run has passed EndsAt — zero EndsAt means "no end",
	// so such a schedule always still has a live next run.
	// (Identical to the camt.058 gate, LG-404.)
	if sched.Status == ScheduleActive && (sched.EndsAt.IsZero() || !sched.NextRunAt.After(sched.EndsAt)) {
		return "", fmt.Errorf("%w: schedule %q is still active with a next run at %s", ErrMT292ScheduleStillActive, opts.ScheduleID, sched.NextRunAt.UTC().Format(time.RFC3339))
	}
	// :21: is the schedule ID — the original MT210's :20:. It
	// must itself fit 16x, or the request cannot be rendered.
	if err := mt292RelatedReference(":21:", sched.ID); err != nil {
		return "", err
	}
	// The To party identifier feeds block 1 — read under the
	// lock, like the MT210 identifier lookup. An FX schedule
	// needs no amount here, so ToCurrency is never consulted.
	l.mu.RLock()
	toID, _ := l.mt300PartyIdentifierLocked(sched.To)
	l.mu.RUnlock()

	// Blocks 1/2 LTs derive from the To identifier and the
	// servicing BIC: a To account without a registered BIC needs
	// an LT-form account ID (8, 11, or 12 alphanumeric
	// characters) — anything else fails loud here rather than
	// emitting a malformed header.
	senderLT, err := mt103NormalizeLT(toID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT292Export, toID, err)
	}
	receiverLT, err := mt103NormalizeLT(servicing)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT292Export, servicing, err)
	}

	// :79: — the narrative, only when the caller gives a reason.
	var reasonLines []string
	if opts.Reason != "" {
		reasonLines, err = mt292ReasonLines(opts.Reason)
		if err != nil {
			return "", err
		}
	}

	// :20: — the deterministic message reference, also the MUR.
	ref := mt292MessageReference(sched.ID, sched.RunSeq)

	var b strings.Builder
	// Block 1: basic header. Session/sequence are zero-filled —
	// assigned by the sender's SWIFT interface on transmission.
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I292. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I292%sU3003}\n", receiverLT)
	// Block 3: user header — the Message User Reference ties the
	// message back to the schedule and run.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", ref)
	// Block 4: text.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", ref)
	fmt.Fprintf(&b, ":21:%s\n", sched.ID)
	fmt.Fprintf(&b, ":11S:210%s\n", mt103ValueDate(opts.OriginalDate))
	if len(reasonLines) > 0 {
		b.WriteString(":79:")
		b.WriteString(strings.Join(reasonLines, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("-}")
	return b.String(), nil
}
