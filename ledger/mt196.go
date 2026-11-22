package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT196 answers export (LG-457): the outbound half of
// the MT195 lifecycle. A correspondent bank asked about a payment
// with an MT195 query (see mt195_inbound.go, LG-453); the MT196
// is the ledger's answer, rendered from the answer snapshot the
// query's first application recorded — never from live facts, and
// never from caller-supplied prose. The answer reports ledger
// facts; it moves nothing: no journal entry, no payout status
// change, no gpi advance, no value date. An answer that moved
// money would be a payment, not an answer.
//
// Field set, verified against two independent sources before this
// module was written (2026-11-22):
//
//   - KELER RCMS via SWIFT User Guide v3.0, section 1.3 "MT196 -
//     Answers": :20: Transaction Reference 16x M, :21: Related
//     Reference 16x M, :76: Answers 6*35x M, :77A: Narrative
//     20*35x O, :11a: MT and Date of the Original Message O.
//   - Oracle Banking Payments Cloud Service, "Answers" (Table
//     6-5): :76: Answers with the line-1 answer-number form and
//     lines 2-6 narrative or answer-number form, :77A: Narrative
//     up to 700 characters (20*35x).
//
// The backlog item named the narrative field :77E:; both sources
// say the MT196 narrative tag is :77A:, so :77E: is refused as
// an unknown field (locked by TestParseMT196_RejectsUnknownField)
// and :77A: is the narrative. :11a: is outside the core set: the
// original message's MT and date are already named by the :21:
// reference into the registered query, and a second, possibly
// disagreeing copy of them would be a different claim — refused,
// never silently dropped, along the MT195 :11S: precedent.
//
// The :76: answers are rendered from the snapshot, one ledger
// fact per line, in a fixed order:
//
//	PAYOUT STATUS: <status>
//	VALUE DATE: <YYMMDD>
//	GPI STATUS: <status>
//
// The gpi line is omitted when the snapshot carries no gpi status:
// a missing track is no fact to report, and inventing a "none"
// status would answer a question nobody asked. The lines carry
// the snapshot verbatim — the payout status exactly as recorded
// ("submitted", "settled", ...), the MT103 value date in YYMMDD —
// so the answer is the same answer the query received, in wire
// form. A coded /CNCL/ style decision code is not used: those
// codes answer an MT192 cancellation request, and an MT195 status
// query asks for facts, not a cancel decision.
//
// Framing follows the MT199 convention: block 1 (basic header,
// sender LT, session/sequence zero-filled and interface-assigned),
// block 2 (application header, receiver LT, `I196`), block 3
// (`{108:MUR}` carrying the :20: reference), block 4 (the field
// set above, in canonical order), no block 5 (CHK is computed by
// the SWIFT network on receipt). Lines are joined with "\n", the
// nacha.go convention; parsing accepts CRLF as well.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416, along mt195TermsEqual): the export's own
// reference names one export. Re-exporting the same :20: with the
// same related reference and the same :77A: narrative returns the
// stored rendered message with Duplicate set, rendering nothing
// twice and emitting no second audit event. A :20: reused for a
// different query or narrative is refused with
// ErrMT196MsgIDConflict — never silently overwritten. The
// sender/receiver LTs are envelope, not terms, along the MT195
// precedent: a replay under different LTs still returns the first
// rendering, because the :20: names one export. The registry
// check runs again under the write lock after the message is
// rendered, so two concurrent exports of the same :20: still
// record once.
//
// An unknown :21: is fail-closed into the repair queue
// (RepairReasonUnknownRoute, the camt.055 shape, along the MT195
// unknown-:21: precedent): the returned result carries the repair
// ID alongside the ErrMT196QueryNotFound error, and no export is
// recorded — a re-sent export parks again, or renders, once the
// query exists. The export's audit event, `mt196_answer_exported`
// (details msg_id / related_reference / payout_id), puts the :20:
// on the durable record; like the MT195 marker it does not bump
// the ledger version, because rendering an answer changes no
// ledger state.

// MT196 parse/export errors.
var (
	// ErrMT196Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: -> :21: -> :76: -> :77A: order.
	ErrMT196Framing = errors.New("ledger: mt196: malformed FIN framing")
	// ErrMT196Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field (:20:,
	// :21:, or :76:), a duplicate field, or a field outside the
	// four-field core set.
	ErrMT196Field = errors.New("ledger: mt196: unexpected or missing field")
	// ErrMT196Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace or a line break, or with
	// a leading/trailing colon or slash, or a double slash.
	ErrMT196Reference = errors.New("ledger: mt196: invalid reference")
	// ErrMT196LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT196LT = errors.New("ledger: mt196: invalid SWIFT LT address")
	// ErrMT196Answers is returned for an invalid :76: answer set:
	// empty, over 6 lines, or a line that is empty, over 35
	// characters, or outside the SWIFT `x` character set (braces
	// and line breaks refused).
	ErrMT196Answers = errors.New("ledger: mt196: invalid :76: answers")
	// ErrMT196Narrative is returned for an invalid :77A:
	// narrative: over 20 lines, or a line that is empty, over 35
	// characters, or outside the SWIFT `x` character set (braces
	// and line breaks refused).
	ErrMT196Narrative = errors.New("ledger: mt196: invalid :77A: narrative")
	// ErrMT196QueryNotFound is returned by ExportMT196ForQuery
	// when the :21: names no registered MT195 query: the export
	// is parked fail-closed in the repair queue and reported as
	// a 422-class error, along the MT195 unknown-:21: precedent.
	ErrMT196QueryNotFound = errors.New("ledger: mt196: unknown MT195 query reference")
	// ErrMT196MsgIDConflict is returned by ExportMT196ForQuery
	// when a :20: reference already exported carries different
	// terms: the reference names one export, and a second,
	// different export under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT196MsgIDConflict = errors.New("ledger: mt196: message reference already exported with different terms")
)

// MT196 line caps: :76: is 6*35x, :77A: is 20*35x, each line 1-35
// SWIFT `x` characters.
const (
	mt196MaxAnswersLines   = 6
	mt196MaxNarrativeLines = 20
	mt196MaxTextLine       = 35
)

// MT196 is one SWIFT FIN MT196 answer message: the block-4 field
// set plus the block 1/2 LT addresses. Answers is the :76:
// answer set (mandatory, 1-6 lines); Narrative is the :77A:
// operator note (optional, 0-20 lines, empty omits :77A:). The
// struct deliberately has no amount, currency, or value-date
// field of its own: an MT196 reports ledger facts, never value —
// the value date it carries is prose inside :76:, rendered from
// a snapshot, not a field the ledger acts on.
type MT196 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	RelatedReference     string
	Answers              []string
	Narrative            []string
}

// mt196Reference validates a :20:/:21: reference: 1-16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash.
func mt196Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT196Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT196Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT196Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT196Reference, field, ref)
	}
	return nil
}

// mt196TextLines validates one multi-line text field: at most
// maxLines lines of 1-35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT199 narrative shape,
// under the MT196 field error identities.
func mt196TextLines(lines []string, tag string, maxLines int, errID error) error {
	if len(lines) == 0 || len(lines) > maxLines {
		return fmt.Errorf("%w: %s: want 1-%d lines, got %d", errID, tag, maxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > mt196MaxTextLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", errID, tag, mt196MaxTextLine, ln)
		}
		if strings.ContainsAny(ln, "\r\n") {
			return fmt.Errorf("%w: %s: line break inside a line: %q", errID, tag, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: %w", errID, tag, err)
		}
	}
	return nil
}

// mt196NormalizeLT normalizes a sender/receiver LT address with
// the MT103 rule (BIC8 -> BIC8+"XXX", BIC11 -> BIC11+"X", BIC12
// as-is), reported under the MT196 error identity.
func mt196NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT196LT, raw)
	}
	return lt, nil
}

// mt196Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses for
// rendering.
func mt196Validate(m MT196) (senderLT, receiverLT string, err error) {
	senderLT, err = mt196NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt196NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt196Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if err := mt196Reference(m.RelatedReference, ":21:"); err != nil {
		return "", "", err
	}
	if err := mt196TextLines(m.Answers, ":76:", mt196MaxAnswersLines, ErrMT196Answers); err != nil {
		return "", "", err
	}
	if len(m.Narrative) > 0 {
		if err := mt196TextLines(m.Narrative, ":77A:", mt196MaxNarrativeLines, ErrMT196Narrative); err != nil {
			return "", "", err
		}
	}
	return senderLT, receiverLT, nil
}

// ExportMT196 renders m as a SWIFT FIN MT196 message: blocks 1-4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT196(m MT196) (string, error) {
	senderLT, receiverLT, err := mt196Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I196. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I196%sU3003}\n", receiverLT)
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
	b.WriteString("-}")
	return b.String(), nil
}

// mt196ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export o
// Parse is stable: Export normalizes (BIC8 -> BIC8+"XXX", the
// MT103 rule), and Parse returns that normalized form verbatim
// rather than re-normalizing it a second time (which would append
// another "X"). Block 1 has the form
// `{1:F01<LT><10 session/sequence digits>}`; block 2 has the form
// `{2:I196<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence 003).
func mt196ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT196Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT196Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT196Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT196LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT196LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT196LT, lt)
		}
	}
	return lt, nil
}

// mt196ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The caller passes the block-4 body with the trailer already
// stripped, so the run stops at the end of the slice.
func mt196ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT196Field, tag)
	}
	return run, i, nil
}

// ParseMT196 parses a SWIFT FIN MT196 message produced by
// ExportMT196 (or an equivalent correspondent-bank message) into
// its business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the four-field core set (:20: / :21: / :76: / :77A:) is
// refused — an answer must not lose content quietly. CRLF line
// endings are accepted; a trailing newline after the `-}` trailer
// is tolerated. A :77A: field, once started, consumes every
// remaining block-4 line as a continuation (it is the last field,
// so a continuation line may itself start with a colon without
// being mistaken for a new field — the same rule as MT199's
// :79:). A bare line starting with `:` inside a :76: run is an
// unknown field, not a continuation — except that `:77A:` ends a
// :76: run, the only legal next tag.
func ParseMT196(text string) (MT196, error) {
	var zero MT196
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + :20: + :21: + :76: + -}), got %d", ErrMT196Framing, len(lines))
	}
	senderLT, err := mt196ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt196ParseLT(lines[1], "{2:I196", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT196Framing, mur)
	}
	if err := mt196Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT196Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT196Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 3 {
		return zero, fmt.Errorf("%w: want at least :20:, :21:, and :76:, got %d block-4 lines", ErrMT196Field, len(body))
	}
	var m MT196
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrMT196Field, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrMT196Field)
	}
	if err := mt196Reference(ref, ":20:"); err != nil {
		return zero, err
	}
	m.TransactionReference = ref
	if !strings.HasPrefix(body[1], ":21:") {
		return zero, fmt.Errorf("%w: block-4 line 2: want :21: field, got %q", ErrMT196Field, body[1])
	}
	related := body[1][len(":21:"):]
	if related == "" {
		return zero, fmt.Errorf("%w: :21: field is empty", ErrMT196Field)
	}
	if err := mt196Reference(related, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = related
	// :76:, when present: the first line carries the tag, the
	// following bare lines continue the answers until the trailer
	// or the :77A: tag. Any other `:` line there is an unknown
	// field, not a continuation.
	if !strings.HasPrefix(body[2], ":76:") {
		return zero, fmt.Errorf("%w: block-4 line 3: want :76: field, got %q", ErrMT196Field, body[2])
	}
	run, next, err := mt196ParseTextRun(body, 2, ":76:")
	if err != nil {
		return zero, err
	}
	if err := mt196TextLines(run, ":76:", mt196MaxAnswersLines, ErrMT196Answers); err != nil {
		return zero, err
	}
	m.Answers = run
	// :77A:, when present: the first line carries the tag, the
	// following bare lines continue the narrative until the
	// trailer. A line starting with `:` there is a continuation,
	// not a new field — :77A: is the last field.
	if next < len(body) {
		if !strings.HasPrefix(body[next], ":77A:") {
			return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT196Field, next+5, body[next])
		}
		run, next, err = mt196ParseTextRun(body, next, ":77A:")
		if err != nil {
			return zero, err
		}
		if err := mt196TextLines(run, ":77A:", mt196MaxNarrativeLines, ErrMT196Narrative); err != nil {
			return zero, err
		}
		m.Narrative = run
	}
	// Nothing but the trailer may follow.
	if next != len(body) {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT196Field, next+5, body[next])
	}
	return m, nil
}

// MT196ExportOptions names the envelope of one MT196 answer
// export: the sender/receiver LT addresses, the export's own
// :20: reference, and the optional :77A: operator note. The
// :21: (the MT195 query's :20:) is the first argument of
// ExportMT196ForQuery, not an option: it is the lookup key, and
// keeping it out of the options makes the snapshot source
// explicit at the call site. The :76: answers are not options
// at all: they are rendered from the registered snapshot, never
// supplied by the caller.
type MT196ExportOptions struct {
	SenderLT   string
	ReceiverLT string
	Reference  string   // :20:, this export's own reference, the replay key.
	Narrative  []string // :77A:, the operator note, optional.
}

// MT196ExportResult is the outcome of ExportMT196ForQuery: the
// rendered answer message and the facts it was rendered from. A
// parked export (unknown :21:) carries only Reference,
// RelatedReference, and RepairID, alongside the
// ErrMT196QueryNotFound error.
type MT196ExportResult struct {
	// Reference echoes the export's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the answered MT195 query's :20:.
	RelatedReference string `json:"related_reference"`
	// PayoutID is the payout the answered query was about.
	PayoutID string `json:"payout_id"`
	// Message is the rendered SWIFT FIN MT196 text.
	Message string `json:"message"`
	// RepairID is the repair queue record for a parked export
	// (unknown :21:). Empty on a rendered export.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt196ExportRecord is the registry record for one rendered MT196
// answer, keyed by :20: in mt196Exports: the query it answered,
// the options it was rendered with, and the result, so a replay
// returns the same message without rendering twice.
type mt196ExportRecord struct {
	QueryRef string
	Options  MT196ExportOptions
	Result   MT196ExportResult
}

// mt196TermsEqual reports whether a stored export record carries
// exactly the terms an export request would render: the same
// answered query and the same :77A: narrative, verbatim. The
// sender/receiver LTs are envelope, not terms, along the MT195
// precedent — a replay under different LTs still returns the
// first rendering, because the :20: names one export.
func mt196TermsEqual(rec mt196ExportRecord, queryRef string, opts MT196ExportOptions) bool {
	if rec.QueryRef != queryRef {
		return false
	}
	if len(rec.Options.Narrative) != len(opts.Narrative) {
		return false
	}
	for i, ln := range rec.Options.Narrative {
		if ln != opts.Narrative[i] {
			return false
		}
	}
	return true
}

// validateMT196AnswerOptions fail-closes on options
// ExportMT196 would refuse, so ExportMT196ForQuery stays safe on
// hand-built requests too: the render gate and the export gate
// share one check, never a copy.
func validateMT196AnswerOptions(queryRef string, opts MT196ExportOptions) error {
	if err := mt196Reference(opts.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt196Reference(queryRef, ":21:"); err != nil {
		return err
	}
	if _, err := mt196NormalizeLT(opts.SenderLT); err != nil {
		return fmt.Errorf("sender: %w", err)
	}
	if _, err := mt196NormalizeLT(opts.ReceiverLT); err != nil {
		return fmt.Errorf("receiver: %w", err)
	}
	if len(opts.Narrative) > 0 {
		if err := mt196TextLines(opts.Narrative, ":77A:", mt196MaxNarrativeLines, ErrMT196Narrative); err != nil {
			return err
		}
	}
	return nil
}

// mt196AnswersFromSnapshot renders the :76: answer lines from a
// registered MT195 answer snapshot: the payout's status, its
// MT103 value date, and its current gpi status when the snapshot
// carries one. A missing gpi status is no fact to report, so the
// line is omitted rather than invented — the snapshot's empty
// GPIStatus means the payout was never tracked, not that it has
// a status called "none".
func mt196AnswersFromSnapshot(ans MT195ApplyResult) []string {
	lines := []string{
		"PAYOUT STATUS: " + ans.PayoutStatus,
		"VALUE DATE: " + ans.ValueDate,
	}
	if ans.GPIStatus != "" {
		lines = append(lines, "GPI STATUS: "+ans.GPIStatus)
	}
	return lines
}

// parkMT196Export parks a fail-closed export request in the repair
// queue and returns the record ID. The snapshot names the queried
// MT195 reference (:21:) as the instruction ID and the export
// (:20:) as the destination, so an operator can find the export
// from the record. The snapshot's Maker is deliberately empty
// (see the MT195 package comment): the record is operator
// visibility, never a resubmittable instruction. A parking failure
// (a full queue) is returned so the caller can fail loud rather
// than silently drop the bank's answer.
func (l *Ledger) parkMT196Export(ref, queryRef, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          queryRef,
		Destination: "mt196-export:" + ref,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ExportMT196ForQuery renders the SWIFT FIN MT196 answer to the
// MT195 query queryRef, from the answer snapshot that query's
// first application recorded: the :21: names the query, the
// :20: (opts.Reference) is this export's own reference, and the
// :76: answers are the snapshot's payout status, MT103 value
// date, and gpi status when one exists. The optional :77A:
// narrative rides verbatim from opts.
//
// The query must exist (an unknown :21: is
// ErrMT196QueryNotFound, parked in the repair queue fail-closed
// in the camt.055 shape: the returned result carries the repair
// ID alongside the error). Nothing moves: no journal entry, no
// payout status change, no gpi advance, no version bump —
// rendering an answer is read-only, and the audit event is the
// durable record, not a state change.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same query and narrative returns
// the stored rendered message with Duplicate set, rendering
// nothing twice and emitting no second audit event; a :20:
// reused for different terms is refused with
// ErrMT196MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read, the snapshot read, and the payout read for the
// audit account run under the read lock; the :20: record and the
// export marker event run under a short write lock of their own,
// re-checking the registry first so a concurrent double-export
// still records once.
func (l *Ledger) ExportMT196ForQuery(queryRef string, opts MT196ExportOptions, actor string) (MT196ExportResult, error) {
	if actor == "" {
		actor = "ExportMT196ForQuery"
	}
	if err := validateMT196AnswerOptions(queryRef, opts); err != nil {
		return MT196ExportResult{}, err
	}

	// :20: replay: the answer was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt196Exports[opts.Reference]; ok {
		l.mu.RUnlock()
		if !mt196TermsEqual(rec, queryRef, opts) {
			return MT196ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT196MsgIDConflict, opts.Reference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The answered query: :21: names it, the MT195 query's :20:.
	// Unknown is parked fail-closed in the repair queue (the
	// camt.055 shape) and reported as the export's error.
	l.mu.RLock()
	ans, ok := l.mt195Queries[queryRef]
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT196Export(opts.Reference, queryRef, actor)
		if parkErr != nil {
			return MT196ExportResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT196QueryNotFound, queryRef, parkErr)
		}
		return MT196ExportResult{Reference: opts.Reference, RelatedReference: queryRef, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT196QueryNotFound, queryRef)
	}

	// The answers, from the registered snapshot only — never from
	// live facts, so the answer is the same answer the query
	// received.
	msg, err := ExportMT196(MT196{
		SenderLT:             opts.SenderLT,
		ReceiverLT:           opts.ReceiverLT,
		TransactionReference: opts.Reference,
		RelatedReference:     queryRef,
		Answers:              mt196AnswersFromSnapshot(ans.Answer),
		Narrative:            opts.Narrative,
	})
	if err != nil {
		return MT196ExportResult{}, err
	}
	res := MT196ExportResult{
		Reference:        opts.Reference,
		RelatedReference: queryRef,
		PayoutID:         ans.Answer.PayoutID,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// account is the payout's payee account, read under the same
	// lock — the payout cannot have vanished (payouts are never
	// deleted), but a missing payout is an empty account list,
	// not an export failure.
	l.mu.Lock()
	if rec, ok := l.mt196Exports[opts.Reference]; ok {
		l.mu.Unlock()
		if !mt196TermsEqual(rec, queryRef, opts) {
			return MT196ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT196MsgIDConflict, opts.Reference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt196Exports == nil {
		l.mt196Exports = make(map[string]mt196ExportRecord)
	}
	l.mt196Exports[opts.Reference] = mt196ExportRecord{QueryRef: queryRef, Options: opts, Result: res}
	var accounts []AccountID
	if p, ok := l.payouts[ans.Answer.PayoutID]; ok {
		accounts = []AccountID{p.PayeeAccount}
	}
	l.emitAudit(AuditEvent{
		Op:            "mt196_answer_exported",
		Actor:         actor,
		TraceID:       opts.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"msg_id":            opts.Reference,
			"related_reference": queryRef,
			"payout_id":         ans.Answer.PayoutID,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT196Export returns the recorded export for msgID, or false
// when no answer was ever rendered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT196Export(msgID string) (MT196ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt196Exports[msgID]
	if !ok {
		return MT196ExportResult{}, false
	}
	return rec.Result, true
}
