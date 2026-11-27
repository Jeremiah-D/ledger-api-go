package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT296 answer inbound registration (LG-465): the
// inbound half of the MT292 lifecycle's answer leg. The ledger
// asked a correspondent bank to cancel an MT210 notification
// with an outbound MT292 (see mt292.go, LG-435); the MT296
// coming back is the bank's answer, and ApplyMT296 registers
// it against the cancellation request its :21: names — the
// question-answer chain's return leg, the MT292 twin of the
// MT195/MT196 loop (LG-460 / LG-462).
//
// Field set, verified against the ISO 15022 MT n96 (common
// answer) table before this module was written:
//
//   - The in-repo MT196 module (LG-457, 2026-11-22), verified
//     against two independent sources: KELER RCMS via SWIFT
//     User Guide v3.0, section 1.3 "MT196 - Answers" (:20:
//     Transaction Reference 16x M, :21: Related Reference 16x
//     M, :76: Answers 6*35x M, :77A: Narrative 20*35x O,
//     :11a: MT and Date of the Original Message O), and
//     Oracle Banking Payments Cloud Service, "Answers".
//   - The ISO 15022 user handbook scope of the common answer
//     message (MT n96), as read by Payments Signal: format
//     rows 20, 21 and 76 mandatory, 77A and 11a (R or S)
//     optional, field 79 conditional under rule C1 — either
//     field 79 or a copy of at least the mandatory fields of
//     the message to which the answer relates, but not both,
//     may be present.
//   - The in-repo MT195 precedent (mt195_inbound.go): :79:
//     Narrative 50*35x optional, :11S: refused as outside
//     the core set.
//
// The MT296 core set here is :20: / :21: / :76: / :77A: /
// :79:, in that canonical order:
//
//   - :20: Transaction Reference Number 16x M — the answer's
//     own reference, the replay key.
//   - :21: Related Reference 16x M — the answered MT292's
//     :20:, the anchor (see below).
//   - :76: Answers 6*35x M — the bank's answer lines, parsed
//     verbatim and never machine-interpreted: the published
//     answer codes (/CNCL/, /RJCR/, /PDCR/) are text lines
//     inside :76:, and this ledger invents no accept/reject
//     code table and runs no status machine off them —
//     whether the bank honoured the cancellation is the
//     bank's statement, read by an operator from the
//     registered lines, not a ledger state transition.
//   - :77A: Narrative 20*35x O — the operator note.
//   - :79: Narrative 50*35x O — the free-format description
//     of the answer, registered verbatim. Rule C1's other
//     branch — an untagged copy of the original message's
//     mandatory fields — is refused, never silently dropped:
//     copy-form content surfaces as out-of-order or unknown
//     tagged lines and fails the parse, along the MT196
//     outside-core-set rule.
//
// :11S: / :11R: (MT and date of the original message) is
// refused as an unknown field, along the MT196 :11a: and
// MT195 :11S: precedents: the original message is already
// named by the :21: shape below, and a second, possibly
// disagreeing copy of its type and date would be a different
// claim — refused, never silently dropped.
//
// The :21: anchors in four tiers (LG-480, extending the LG-472
// three tiers): tier (a) is the shape of our outbound MT292's
// :20: — `MT292-` plus the first 10 lowercase hex characters
// of the SHA-256 of the run transfer ID
// (mt292MessageReference, mt292.go:152), 16 characters in
// all — and the :21: must also be registered in mt292Exports,
// the outbound MT292 cancellation registry (LG-471, consulted
// since LG-473); tier (b) is the shape of our outbound MT295's
// :20: — `MT295-` plus the first 10 lowercase hex characters of the
// SHA-256 of the queried MT292's :20: and the question body
// (mt295MessageReference, mt295_export.go) — and the :21:
// must also be registered in mt295Exports, the outbound MT295
// query registry (LG-468); tier (c) is the :20: of an outbound
// MT202 transfer this ledger rendered — the caller-supplied
// transaction reference (mt202.go), which carries no MT292-/
// MT295- shape to check, so the tier is a pure lookup in
// mt202Exports, the outbound MT202 transfer registry (LG-470);
// tier (d) is the :20: of an outbound MT296 answer this ledger
// rendered — the operator-supplied 1-16 `x` character reference
// (mt296_export.go), which carries no MT292-/MT295- shape to
// check, so the tier is a pure lookup in mt296Exports, the
// outbound MT296 answer registry (LG-467).
// A :21: that matches no tier — or a tier-(a) shape naming no
// MT292 this ledger rendered, or a tier-(b) shape naming no
// query this ledger rendered, or a :21: naming no transfer
// this ledger rendered, or a :21: naming no answer this ledger
// rendered — is fail-closed into the repair queue
// (RepairReasonUnknownRoute, the camt.055 shape, along
// ApplyMT999 and ApplyMT292: the returned result carries the
// repair ID alongside the ErrMT296UnknownReference error),
// and reported as a 422-class error. The four tiers are
// deliberately asymmetric: tier (a) is shape plus its own
// registry, tier (b) is shape plus its own registry, and tiers
// (c) and (d) have a registry and no shape at all — the lookup
// alone is the anchor, because the :20: is the caller's own
// reference, not a derived one. No tier does a :21:→schedule reverse
// lookup — the SHA-256 is irreversible, and the operator
// matches the answer to the request from the :21: and the
// audit event, not from a ledger lookup; a :21: whose shape
// overlaps tier (a) or (b) is accepted whichever tier hits,
// including a :21: registered only in mt202Exports or
// mt296Exports, which its own tier accepts alone,
// and the lookup order does not change the accepted set.
//
// The registries are in-memory (LG-471): after a restart, or
// for an MT292 rendered before registration existed, a late
// MT296 parks in the repair queue instead of registering —
// the evidence is kept (the repair queue holds the verbatim
// answer, retrievable with GetRepair), and the recovery is
// deterministic: re-export the MT292 (GET /exports/mt292 —
// the same schedule and run derive the same :20:, so the
// export re-registers it), or re-export the MT296 answer
// (POST /exports/mt296 — the same :20: with the same terms
// re-renders the same answer, so the export re-registers it),
// and re-apply the same MT296;
// parking never consumes the :20:, so the re-apply is not a
// replay.
//
// The answer is registered verbatim: the :76: answers, the
// :77A: narrative, and the :79: description ride into the
// record exactly as received, never re-derived from live
// facts, because the answer is the correspondent's statement,
// not the ledger's own view of it. A later change to the
// schedule's status never rewrites a registered answer; a
// replay returns the stored verbatim answer. The :76: /
// :77A: / :79: texts are retrievable through the getter —
// they live in the registry record, not in the audit event
// details.
//
// Nothing moves: no journal entry, no schedule status
// change, no version bump — registering an answer is
// read-only, and the audit event is the durable record, not
// a state change. The answer's audit event,
// `mt296_answer_received` (details msg_id /
// related_reference / sender_lt, trace :20:), puts the :20:
// on the durable record, along the mt999_received shape: no
// accounts, because an answer touches no ledger account.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): a replay with the same :21:, the same
// :76: answers, the same :77A: narrative, and the same :79:
// description returns the stored outcome with Duplicate set,
// registering nothing twice and emitting no second audit
// event. A :20: reused for different terms is refused with
// ErrMT296MsgIDConflict — never silently overwritten. The
// registry check runs again under the write lock after the
// anchor check, so two concurrent applies of the same answer
// still record once.

// MT296 parse/apply errors.
var (
	// ErrMT296Framing is returned when the FIN block structure
	// is broken: a missing block, a malformed block 1/2/3
	// header, a missing or unterminated block 4, a block-3
	// MUR that does not name the :20:, or a field out of the
	// canonical :20: -> :21: -> :76: -> :77A: -> :79: order.
	ErrMT296Framing = errors.New("ledger: mt296: malformed FIN framing")
	// ErrMT296Field is returned for a block-4 field problem
	// that is not a value problem: a missing required field
	// (:20:, :21:, or :76:), a duplicate field, or a field
	// outside the five-field core set.
	ErrMT296Field = errors.New("ledger: mt296: unexpected or missing field")
	// ErrMT296Reference is returned for an invalid :20: or
	// :21: reference: empty, over 16 characters, outside the
	// SWIFT `x` character set, carrying a brace or a line
	// break, or with a leading/trailing colon or slash, or a
	// double slash.
	ErrMT296Reference = errors.New("ledger: mt296: invalid reference")
	// ErrMT296LT is returned for an invalid sender or receiver
	// LT address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT296LT = errors.New("ledger: mt296: invalid SWIFT LT address")
	// ErrMT296Answers is returned for an invalid :76: answer
	// set: empty, over 6 lines, or a line that is empty, over
	// 35 characters, or outside the SWIFT `x` character set
	// (braces and line breaks refused).
	ErrMT296Answers = errors.New("ledger: mt296: invalid :76: answers")
	// ErrMT296Narrative is returned for an invalid :77A:
	// narrative: over 20 lines, or a line that is empty, over
	// 35 characters, or outside the SWIFT `x` character set
	// (braces and line breaks refused).
	ErrMT296Narrative = errors.New("ledger: mt296: invalid :77A: narrative")
	// ErrMT296Description is returned for an invalid :79:
	// description: over 50 lines, or a line that is empty,
	// over 35 characters, or outside the SWIFT `x` character
	// set (braces and line breaks refused).
	ErrMT296Description = errors.New("ledger: mt296: invalid :79: description")
	// ErrMT296UnknownReference is returned by ApplyMT296 when
	// the inbound answer's :21: names nothing this ledger
	// could have sent: neither the shape of an outbound MT292
	// :20: (`MT292-` + 10 lowercase hex) that is registered
	// in mt292Exports, nor the shape of an outbound MT295
	// :20: (`MT295-` + 10 lowercase hex) that is registered
	// in mt295Exports, nor a :20: registered in
	// mt202Exports, nor a :20: registered in
	// mt296Exports — the answer names no cancellation request,
	// query, transfer, or answer this ledger could have sent, so it is
	// parked in the repair queue fail-closed rather than
	// dropped. It is a 422-class error.
	ErrMT296UnknownReference = errors.New("ledger: mt296 inbound: unknown :21: related reference")
	// ErrMT296MsgIDConflict is returned when a :20: reference
	// already applied carries different terms: the reference
	// names one answer, and a second, different answer under
	// it is refused rather than silently overwritten (the
	// pain.014 ErrPain014MsgIDConflict convention). It is a
	// 422-class error.
	ErrMT296MsgIDConflict = errors.New("ledger: mt296: :20: reused for different terms")
)

// MT296 line caps: :76: is 6*35x, :77A: is 20*35x, :79: is
// 50*35x, each line 1-35 SWIFT `x` characters.
const (
	mt296MaxAnswersLines     = 6
	mt296MaxNarrativeLines   = 20
	mt296MaxDescriptionLines = 50
	mt296MaxTextLine         = 35
)

// MT296 is one SWIFT FIN MT296 answer message: the block-4
// field set plus the block 1/2 LT addresses. Answers is the
// :76: answer set (mandatory, 1-6 lines); Narrative is the
// :77A: operator note (optional, 0-20 lines, empty omits
// :77A:); Description is the :79: free-format description
// (optional, 0-50 lines, empty omits :79:). The struct
// deliberately has no amount, currency, value-date, or
// decision field of its own: an MT296 reports the bank's
// answer, never value — and the answer codes inside :76:
// are prose the operator reads, not a status the ledger acts
// on.
type MT296 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	RelatedReference     string
	Answers              []string
	Narrative            []string
	Description          []string
}

// mt296Reference validates a :20:/:21: reference: 1-16 SWIFT
// `x` characters, no braces, no line breaks (a single-line
// field must not smuggle a second line into block 4), no
// leading/trailing colon or slash, no double slash.
func mt296Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT296Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT296Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT296Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT296Reference, field, ref)
	}
	return nil
}

// mt296TextLines validates one multi-line text field: at most
// maxLines lines of 1-35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT199 narrative
// shape, under the MT296 field error identities.
func mt296TextLines(lines []string, tag string, maxLines int, errID error) error {
	if len(lines) == 0 || len(lines) > maxLines {
		return fmt.Errorf("%w: %s: want 1-%d lines, got %d", errID, tag, maxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > mt296MaxTextLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", errID, tag, mt296MaxTextLine, ln)
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

// mt296NormalizeLT normalizes a sender/receiver LT address with
// the MT103 rule (BIC8 -> BIC8+"XXX", BIC11 -> BIC11+"X", BIC12
// as-is), reported under the MT296 error identity.
func mt296NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT296LT, raw)
	}
	return lt, nil
}

// mt296Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses.
func mt296Validate(m MT296) (senderLT, receiverLT string, err error) {
	senderLT, err = mt296NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt296NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt296Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if err := mt296Reference(m.RelatedReference, ":21:"); err != nil {
		return "", "", err
	}
	if err := mt296TextLines(m.Answers, ":76:", mt296MaxAnswersLines, ErrMT296Answers); err != nil {
		return "", "", err
	}
	if len(m.Narrative) > 0 {
		if err := mt296TextLines(m.Narrative, ":77A:", mt296MaxNarrativeLines, ErrMT296Narrative); err != nil {
			return "", "", err
		}
	}
	if len(m.Description) > 0 {
		if err := mt296TextLines(m.Description, ":79:", mt296MaxDescriptionLines, ErrMT296Description); err != nil {
			return "", "", err
		}
	}
	return senderLT, receiverLT, nil
}

// mt296ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written: Parse returns
// the normalized form verbatim rather than re-normalizing it a
// second time (which would append another "X"). Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I296<LT><5 header characters>}` (priority U,
// delivery monitoring 3, obsolescence 003).
func mt296ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT296Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT296Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT296Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT296LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT296LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT296LT, lt)
		}
	}
	return lt, nil
}

// mt296ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The caller passes the block-4 body with the trailer already
// stripped, so the run stops at the end of the slice.
func mt296ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT296Field, tag)
	}
	return run, i, nil
}

// ParseMT296 parses a SWIFT FIN MT296 message (a
// correspondent-bank answer to an MT292 cancellation request,
// or to an MT295 query) into its business fields. Parsing is
// strict: the block structure, the field order, and every
// field value are validated, and anything outside the
// five-field core set (:20: / :21: / :76: / :77A: / :79:) is
// refused — an answer must not lose content quietly. CRLF line
// endings are accepted; a trailing newline after the `-}`
// trailer is tolerated. The block-3 Message User Reference
// must name the :20: the block 4 carries (the MT920
// precedent): the :20: is the registration key, and a MUR
// that names a different answer would mis-key it. A :77A:
// field, once started, ends at the next `:` line — only
// `:79:` is the legal next tag. A :79: field, once started,
// consumes every remaining block-4 line as a continuation (it
// is the last field, so a continuation line may itself start
// with a colon without being mistaken for a new field — the
// same rule as MT199's :79:). A bare line starting with `:`
// inside a :76: run is an unknown field, not a continuation —
// except that `:77A:` ends a :76: run, the only legal next
// tag before :79:.
func ParseMT296(text string) (MT296, error) {
	var zero MT296
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + :20: + :21: + :76: + -}), got %d", ErrMT296Framing, len(lines))
	}
	senderLT, err := mt296ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt296ParseLT(lines[1], "{2:I296", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: the Message User Reference must name the :20:
	// the block 4 carries — the MT920 precedent, because the
	// :20: is the registration key and a MUR that names a
	// different answer would mis-key it.
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT296Framing, mur)
	}
	murRef := mur[len("{3:{108:") : len(mur)-2]
	if err := mt296Reference(murRef, "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT296Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT296Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 3 {
		return zero, fmt.Errorf("%w: want at least :20:, :21:, and :76:, got %d block-4 lines", ErrMT296Field, len(body))
	}
	var m MT296
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrMT296Field, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrMT296Field)
	}
	if err := mt296Reference(ref, ":20:"); err != nil {
		return zero, err
	}
	if murRef != ref {
		return zero, fmt.Errorf("%w: {108:} %q does not match :20: %q", ErrMT296Framing, murRef, ref)
	}
	m.TransactionReference = ref
	if !strings.HasPrefix(body[1], ":21:") {
		return zero, fmt.Errorf("%w: block-4 line 2: want :21: field, got %q", ErrMT296Field, body[1])
	}
	related := body[1][len(":21:"):]
	if related == "" {
		return zero, fmt.Errorf("%w: :21: field is empty", ErrMT296Field)
	}
	if err := mt296Reference(related, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = related
	// :76:, when present: the first line carries the tag, the
	// following bare lines continue the answers until the
	// trailer or the :77A: tag. Any other `:` line there is an
	// unknown field, not a continuation.
	if !strings.HasPrefix(body[2], ":76:") {
		return zero, fmt.Errorf("%w: block-4 line 3: want :76: field, got %q", ErrMT296Field, body[2])
	}
	run, next, err := mt296ParseTextRun(body, 2, ":76:")
	if err != nil {
		return zero, err
	}
	if err := mt296TextLines(run, ":76:", mt296MaxAnswersLines, ErrMT296Answers); err != nil {
		return zero, err
	}
	m.Answers = run
	// :77A:, when present: the first line carries the tag, the
	// following bare lines continue the narrative until the
	// trailer or the :79: tag. Any other `:` line there is an
	// unknown field, not a continuation.
	if next < len(body) && strings.HasPrefix(body[next], ":77A:") {
		run, next, err = mt296ParseTextRun(body, next, ":77A:")
		if err != nil {
			return zero, err
		}
		if err := mt296TextLines(run, ":77A:", mt296MaxNarrativeLines, ErrMT296Narrative); err != nil {
			return zero, err
		}
		m.Narrative = run
	}
	// :79:, when present: the first line carries the tag, and
	// every remaining block-4 line is a continuation — it is
	// the last field, so a continuation line may itself start
	// with a colon (the MT199 :79: rule).
	if next < len(body) {
		if !strings.HasPrefix(body[next], ":79:") {
			return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT296Field, next+5, body[next])
		}
		description := []string{body[next][len(":79:"):]}
		description = append(description, body[next+1:]...)
		if description[0] == "" {
			return zero, fmt.Errorf("%w: :79: field is empty", ErrMT296Field)
		}
		if err := mt296TextLines(description, ":79:", mt296MaxDescriptionLines, ErrMT296Description); err != nil {
			return zero, err
		}
		m.Description = description
		next = len(body)
	}
	// Nothing but the trailer may follow.
	if next != len(body) {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT296Field, next+5, body[next])
	}
	// The parse gate and the apply gate share one check: every
	// field is re-validated through mt296Validate, so a
	// hand-built MT296 and a parsed one face the same rules.
	if _, _, err := mt296Validate(m); err != nil {
		return zero, err
	}
	return m, nil
}

// mt296IsMT292Reference reports whether ref has the shape of
// an outbound MT292 :20: — `MT292-` plus the first 10
// lowercase hex characters of the SHA-256 of the run transfer
// ID (mt292MessageReference, mt292.go:152), 16 characters in
// all. The shape is only half the anchor: a :21: of this shape
// must also be registered in mt292Exports (see
// mt296IsAnchoredReference), because a cancellation request
// this ledger never rendered cannot be answered — the shape
// alone is not enough (LG-473). A :21: that is not even shaped
// like an MT292 :20: names nothing this ledger could have
// sent.
func mt296IsMT292Reference(ref string) bool {
	if len(ref) != 16 || !strings.HasPrefix(ref, "MT292-") {
		return false
	}
	for _, c := range ref[len("MT292-"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// mt296IsMT295Reference reports whether ref has the shape of
// an outbound MT295 :20: — `MT295-` plus the first 10
// lowercase hex characters of the SHA-256 of the queried
// MT292's :20: and the question body
// (mt295MessageReference, mt295_export.go), 16 characters in
// all. The shape is only half the anchor: a :21: of this
// shape must also be registered in mt295Exports (see
// mt296IsAnchoredReference), because a query this ledger
// never rendered cannot be answered — along tier (a), whose
// MT292 must also be registered in mt292Exports (LG-473).
func mt296IsMT295Reference(ref string) bool {
	if len(ref) != 16 || !strings.HasPrefix(ref, "MT295-") {
		return false
	}
	for _, c := range ref[len("MT295-"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// mt296IsAnchoredReference reports whether ref names a request
// this ledger could have sent: tier (a), the shape of an
// outbound MT292 :20: that is registered in mt292Exports —
// a shape-matching :21: that names no cancellation request
// this ledger rendered is still unknown — or tier (b), the
// shape of an outbound MT295 :20: that is registered in
// mt295Exports — a shape-matching :21: that names no query
// this ledger rendered is still unknown — or tier (c), a :20:
// registered in mt202Exports: the caller-supplied MT202
// reference carries no shape to check, so the lookup alone
// anchors it — or tier (d), a :20: registered in
// mt296Exports: the operator-supplied MT296 answer reference
// carries no shape to check, so the lookup alone anchors it,
// and a :21: whose shape overlaps tier (a) or (b) is accepted
// whichever tier hits. The registry lookups run
// under one read lock; the shape checks read nothing.
// Registry entries are only ever added, never removed, so a
// lookup hit stays valid past this call.
func (l *Ledger) mt296IsAnchoredReference(ref string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if mt296IsMT292Reference(ref) {
		if _, ok := l.mt292Exports[ref]; ok {
			return true
		}
	}
	if mt296IsMT295Reference(ref) {
		if _, ok := l.mt295Exports[ref]; ok {
			return true
		}
	}
	// Tier (c) (LG-472): the :21: names an outbound MT202's
	// :20:, registered in mt202Exports. The caller-supplied
	// reference carries no MT292-/MT295- shape to check, so
	// the lookup alone anchors it — including a :21: whose
	// shape overlaps tier (a) or (b) but is registered only
	// here.
	if _, ok := l.mt202Exports[ref]; ok {
		return true
	}
	// Tier (d) (LG-480): the :21: names an outbound MT296
	// answer's :20:, registered in mt296Exports. The
	// operator-supplied reference carries no MT292-/MT295-
	// shape to check, so the lookup alone anchors it —
	// including a :21: whose shape overlaps tier (a) or (b)
	// but is registered only here.
	_, ok := l.mt296Exports[ref]
	return ok
}

// MT296ApplyResult is the outcome of ApplyMT296: the
// registered answer, verbatim. A parked answer (unknown :21:)
// carries only Reference, RelatedReference, Answers,
// Narrative, Description, and RepairID, alongside the
// ErrMT296UnknownReference error.
type MT296ApplyResult struct {
	// Reference echoes the answer's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the answered MT292's or MT295's :20: (the :21:).
	RelatedReference string `json:"related_reference"`
	// Answers is the :76: answer set, verbatim.
	Answers []string `json:"answers,omitempty"`
	// Narrative is the :77A: narrative, verbatim.
	Narrative []string `json:"narrative,omitempty"`
	// Description is the :79: description, verbatim.
	Description []string `json:"description,omitempty"`
	// RepairID is the repair queue record for a parked answer
	// (unknown :21:). Empty on a registered answer.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the answer was already
	// registered under the same :20: with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt296ReceivedRecord is the registry record for one registered
// MT296 answer, keyed by :20: in mt296Received: the message
// as received, its :21: anchor, when it was registered, and
// who registered it, so a replay returns the same outcome
// without registering twice.
type mt296ReceivedRecord struct {
	Message          MT296
	RelatedReference string
	ReceivedAt       time.Time
	Actor            string
	Result           MT296ApplyResult
}

// mt296ReceivedTermsEqual reports whether a stored answer
// record carries exactly the terms an inbound answer would
// apply: the same :21: related reference, the same :76:
// answers, the same :77A: narrative, and the same :79:
// description, verbatim. The sender/receiver LTs are
// envelope, not terms, along the MT196 precedent — a replay
// under different LTs still returns the first registration,
// because the :20: names one answer.
func mt296ReceivedTermsEqual(rec mt296ReceivedRecord, m MT296) bool {
	if rec.Message.RelatedReference != m.RelatedReference {
		return false
	}
	if len(rec.Message.Answers) != len(m.Answers) {
		return false
	}
	for i, ln := range rec.Message.Answers {
		if ln != m.Answers[i] {
			return false
		}
	}
	if len(rec.Message.Narrative) != len(m.Narrative) {
		return false
	}
	for i, ln := range rec.Message.Narrative {
		if ln != m.Narrative[i] {
			return false
		}
	}
	if len(rec.Message.Description) != len(m.Description) {
		return false
	}
	for i, ln := range rec.Message.Description {
		if ln != m.Description[i] {
			return false
		}
	}
	return true
}

// mt296LinesCopy returns a copy of lines, so a registered
// answer's slices can never be mutated through a returned
// result — the getter invariant, along the other registries.
func mt296LinesCopy(lines []string) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// mt296ReceivedResultCopy returns a copy of res with its own
// answers, narrative, and description slices.
func mt296ReceivedResultCopy(res MT296ApplyResult) MT296ApplyResult {
	res.Answers = mt296LinesCopy(res.Answers)
	res.Narrative = mt296LinesCopy(res.Narrative)
	res.Description = mt296LinesCopy(res.Description)
	return res
}

// parkMT296Answer parks a fail-closed inbound answer in the
// repair queue and returns the record ID. The snapshot names
// the anchor the answer tried to reach (:21:) as the
// instruction ID and the answer (:20:) as the destination, so
// an operator can find the answer from the record. The
// snapshot's Maker is deliberately empty (see the MT195
// package comment): the record is operator visibility, never
// a resubmittable instruction. A parking failure (a full
// queue) is returned so the caller can fail loud rather than
// silently drop the bank's answer.
func (l *Ledger) parkMT296Answer(m MT296, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          m.RelatedReference,
		Destination: "mt296-inbound:" + m.TransactionReference,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT296 applies one parsed inbound SWIFT FIN MT296 answer:
// it registers the correspondent bank's answer against the
// MT292 cancellation request its :21: names — verbatim, with
// the :76: answers, the :77A: narrative, and the :79:
// description as received, never re-derived from live facts.
//
// The :21: anchors in four tiers (LG-480): tier (a) the shape
// of an outbound MT292 :20: (`MT292-` + 10 lowercase hex)
// that is registered in mt292Exports (the outbound MT292
// cancellation registry, LG-471, consulted since LG-473),
// tier (b) the shape of an outbound MT295 :20:
// (`MT295-` + 10 lowercase hex) that is registered in
// mt295Exports, tier (c) a :20: registered in mt202Exports
// (the outbound MT202 transfer registry, LG-470) — a
// caller-supplied reference with no shape to check, so the
// lookup alone anchors it — or tier (d) a :20: registered in
// mt296Exports (the outbound MT296 answer registry, LG-467) —
// an operator-supplied reference with no shape to check, so the
// lookup alone anchors it. A :21: matching no tier — or a
// tier-(a) shape naming no rendered MT292, or a tier-(b)
// shape naming no rendered query, or a :21: naming no
// rendered transfer, or a :21: naming no rendered answer — is ErrMT296UnknownReference, parked in
// the repair queue fail-closed in the camt.055 shape: the
// returned result carries the repair ID alongside the error. A
// tier-matching :21: registers — tier (a) has no
// :21:→schedule reverse lookup (the SHA-256 is
// irreversible), so the operator matches the answer to the
// request from the :21: and the audit event. No journal
// entry is ever posted, no schedule state is ever changed,
// and no version is ever bumped: an answer is evidence, not
// an instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same :21:, the same :76:
// answers, the same :77A: narrative, and the same :79:
// description returns the stored outcome with Duplicate
// set, registering nothing twice and emitting no second
// audit event; a :20: reused for different terms is refused
// with ErrMT296MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone;
// the replay read runs under the read lock and the :21:
// anchor check takes the read lock for the four tiers'
// registry lookups (the shape checks read nothing);
// the :20: record and the
// inbound marker event run under a short write lock of their
// own, re-checking the registry first so a concurrent
// double-apply still records once; the repair parking runs
// outside any held lock because EnqueueRepair takes l.mu
// itself.
func (l *Ledger) ApplyMT296(m MT296, actor string) (MT296ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT296"
	}
	// The parse gate and the apply gate share one check.
	if _, _, err := mt296Validate(m); err != nil {
		return MT296ApplyResult{}, err
	}

	// :20: replay: the answer was already registered under
	// this reference. Same terms is a no-op returning the
	// stored outcome; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt296Received[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt296ReceivedTermsEqual(rec, m) {
			return MT296ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT296MsgIDConflict, m.TransactionReference)
		}
		dup := mt296ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The :21: anchor, four tiers (LG-480, tier (a) since
	// LG-473): the shape of an outbound MT292 :20: registered
	// in mt292Exports, the shape of an outbound MT295 :20:
	// registered in mt295Exports, a :20: registered
	// in mt202Exports (the MT202 transfer registry — a
	// caller-supplied reference with no shape to check, so
	// the lookup alone anchors it), or a :20: registered
	// in mt296Exports (the MT296 answer registry — an
	// operator-supplied reference with no shape to check, so
	// the lookup alone anchors it). Anything else names no
	// cancellation request, query, transfer, or answer this ledger
	// could have sent, so the answer parks fail-closed
	// instead of registering against nothing.
	res := MT296ApplyResult{
		Reference:        m.TransactionReference,
		RelatedReference: m.RelatedReference,
		Answers:          mt296LinesCopy(m.Answers),
		Narrative:        mt296LinesCopy(m.Narrative),
		Description:      mt296LinesCopy(m.Description),
	}
	if !l.mt296IsAnchoredReference(m.RelatedReference) {
		repairID, parkErr := l.parkMT296Answer(m, actor)
		if parkErr != nil {
			return MT296ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT296UnknownReference, m.RelatedReference, parkErr)
		}
		return MT296ApplyResult{
			Reference:        m.TransactionReference,
			RelatedReference: m.RelatedReference,
			Answers:          mt296LinesCopy(m.Answers),
			Narrative:        mt296LinesCopy(m.Narrative),
			Description:      mt296LinesCopy(m.Description),
			RepairID:         repairID,
		}, fmt.Errorf("%w: %q", ErrMT296UnknownReference, m.RelatedReference)
	}

	// The :20: record and the inbound marker event, under one
	// short write lock: the answer is already built when the
	// marker lands. A concurrent double-apply that landed
	// first returns the stored outcome instead of a second
	// marker. The audit event carries no accounts: an answer
	// touches no ledger account, and inventing one would
	// misattribute the evidence.
	l.mu.Lock()
	if rec, ok := l.mt296Received[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt296ReceivedTermsEqual(rec, m) {
			return MT296ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT296MsgIDConflict, m.TransactionReference)
		}
		dup := mt296ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt296Received == nil {
		l.mt296Received = make(map[string]mt296ReceivedRecord)
	}
	l.mt296Received[m.TransactionReference] = mt296ReceivedRecord{
		Message: MT296{
			SenderLT:             m.SenderLT,
			ReceiverLT:           m.ReceiverLT,
			TransactionReference: m.TransactionReference,
			RelatedReference:     m.RelatedReference,
			Answers:              mt296LinesCopy(m.Answers),
			Narrative:            mt296LinesCopy(m.Narrative),
			Description:          mt296LinesCopy(m.Description),
		},
		RelatedReference: m.RelatedReference,
		ReceivedAt:       time.Now().UTC(),
		Actor:            actor,
		Result:           mt296ReceivedResultCopy(res),
	}
	l.emitAudit(AuditEvent{
		Op:            "mt296_answer_received",
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

// GetMT296Received returns the registered answer for msgID
// (its :20:), or false when no answer was ever registered
// under it. The returned result is a copy — callers cannot
// mutate ledger state through it.
func (l *Ledger) GetMT296Received(msgID string) (MT296ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt296Received[msgID]
	if !ok {
		return MT296ApplyResult{}, false
	}
	return mt296ReceivedResultCopy(rec.Result), true
}
