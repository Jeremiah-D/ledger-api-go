package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT199 free-format message, parse + export (LG-210,
// fintech: SWIFT 自由格式报文).
//
// An MT199 is a free-format notification: it carries no amount, no
// instruction, and no settlement semantics — only text. That is what
// separates it from its structured neighbours:
//
//   - MT103 (see mt103.go, LG-183) is a customer *instruction* with
//     a value (:32A: moves money).
//   - MT202 (see mt202.go, LG-204) is a bank-to-bank *instruction*,
//     likewise valued.
//   - MT900/MT910 (see mt900.go, LG-209) are post-settlement
//     *confirmations* of a valued movement.
//   - MT199 notifies, queries, or explains in prose. A parser that
//     found a :32A: in an MT199 would be reading a different message
//     type: this struct has no amount field at all, so an MT199 can
//     never be mistaken for a payment, and a payment can never be
//     smuggled through a free-format note.
//
// This module covers the block-4 field set the backlog names:
//
//	:20: transaction reference, :21: related reference,
//	:79: narrative.
//
// The `:21:` field is optional in the MT199 network rules, and it
// is optional here too — but it is the context anchor the backlog
// asks for: it carries the transfer or payout ID the notification
// is about (see below for the UETR note). The `:79:` narrative is
// mandatory: a free-format message with nothing to say is not a
// message. It carries 1–50 lines of at most 35 `x` characters each,
// the MT900/MT910 `:72:` convention with the MT199 line cap.
//
// UETR anchoring, stated honestly: a gpi UETR (see gpi.go, LG-165)
// is a 36-character UUID, and it cannot fit the :21: field's 16x
// limit — so :21: anchors with the transfer/payout ID (at most 16
// characters), and the full UETR, when it must be cited, goes in
// the :79: narrative text (wrapped at 35 characters per line by the
// caller, or by WrapMT199Narrative below), with the UETR↔transfer
// link itself living in the gpi track (LinkGPITransfer). The MT199
// never invents, truncates, or hashes a UETR to force it into :21:
// a truncated identifier would anchor the notification to the wrong
// payment, or to no payment at all.
//
// Framing follows the MT103/MT202 convention: block 1 (basic header,
// sender LT, session/sequence zero-filled and interface-assigned),
// block 2 (application header, receiver LT, `I199`), block 3
// (`{108:MUR}` carrying the :20: reference), block 4 (the field set
// above, in canonical order), no block 5 (CHK is computed by the
// SWIFT network on receipt; a generator emitting a fake checksum
// would be dishonest). Lines are joined with "\n", the nacha.go
// convention; parsing accepts CRLF as well.
//
// Block 3 is envelope framing, validated for shape only: it must be
// present and well-formed (`{3:{108:<1-16 x characters>}}`), but its
// content is not part of the business record and is not carried in
// the returned struct — the :20: reference is the business key.
// Export always writes the :20: reference as the MUR, the MT103
// convention.
//
// This is a standalone message value object, not a ledger-bound
// export: an MT199 is a notification about a transfer, not a view of
// one — there is no stored transfer object to render from (payouts
// are the ledger's transfer instructions, and their MT199 anchor is
// just their ID in :21:), so the caller supplies the fields. The
// value is therefore pure: no ledger lock, no version bump, no
// journal rows, no audit event, and export is deterministic per
// message (repeat exports render byte-identical output). Sending an
// MT199 changes nothing on the ledger — that is the point of a
// free-format note, and the reason it must never carry an amount.
//
// Field discipline is fail-loud, the nacha.go convention: every field
// is validated on both parse and export, and any violation is an
// error, never a silent truncation or default — except the
// session/sequence zeros in block 1, which the MT103 convention
// documents as interface-assigned.

// MT199 parse/export errors.
var (
	// ErrMT199Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :79: order.
	ErrMT199Framing = errors.New("ledger: mt199: malformed FIN framing")
	// ErrMT199Reference is returned for an invalid :20: or :21:
	// reference: empty (:20: only — :21: may be omitted), over 16
	// characters, outside the SWIFT `x` character set, carrying a
	// brace or a line break, or with a leading/trailing colon or
	// slash, or a double slash.
	ErrMT199Reference = errors.New("ledger: mt199: invalid reference")
	// ErrMT199Narrative is returned for an invalid :79: narrative:
	// empty, over 50 lines, or a line that is empty, over 35
	// characters, or outside the SWIFT `x` character set (braces
	// and line breaks refused).
	ErrMT199Narrative = errors.New("ledger: mt199: invalid :79: narrative")
	// ErrMT199LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT199LT = errors.New("ledger: mt199: invalid SWIFT LT address")
	// ErrMT199Field is returned for a block-4 field problem that is
	// not a value problem: a missing required field (:20: or :79:),
	// a duplicate field, or a field outside the three-field set
	// (including any valued field such as :32A: before :79:).
	ErrMT199Field = errors.New("ledger: mt199: unexpected or missing field")
)

// MT199NarrativeMaxLines is the MT199 :79: line cap (50 lines of at
// most 35 `x` characters each).
const MT199NarrativeMaxLines = 50

// MT199NarrativeMaxLineLen is the MT199 :79: per-line cap.
const MT199NarrativeMaxLineLen = 35

// MT199 is one SWIFT FIN MT199 free-format message: the block-4
// field set plus the block 1/2 LT addresses. RelatedReference is
// optional (empty omits :21:); Narrative is mandatory, 1–50 lines.
// The struct deliberately has no amount, currency, or value-date
// field: an MT199 carries prose, never value.
type MT199 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	RelatedReference     string
	Narrative            []string
}

// mt199Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash.
func mt199Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT199Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT199Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT199Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT199Reference, field, ref)
	}
	return nil
}

// mt199Narrative validates a :79: narrative: 1–50 lines, each 1–35
// SWIFT `x` characters, no braces, no line breaks inside a line.
func mt199Narrative(lines []string) error {
	if len(lines) == 0 || len(lines) > MT199NarrativeMaxLines {
		return fmt.Errorf("%w: want 1-%d lines, got %d", ErrMT199Narrative, MT199NarrativeMaxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > MT199NarrativeMaxLineLen {
			return fmt.Errorf("%w: line exceeds 1-%d characters: %q", ErrMT199Narrative, MT199NarrativeMaxLineLen, ln)
		}
		if strings.ContainsAny(ln, "\r\n") {
			return fmt.Errorf("%w: line break inside a :79: line: %q", ErrMT199Narrative, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %w", ErrMT199Narrative, err)
		}
	}
	return nil
}

// mt199NormalizeLT normalizes a sender/receiver LT address with the
// MT103 rule (BIC8 → BIC8+"XXX", BIC11 → BIC11+"X", BIC12 as-is),
// reported under the MT199 error identity.
func mt199NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT199LT, raw)
	}
	return lt, nil
}

// mt199Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses for rendering.
func mt199Validate(m MT199) (senderLT, receiverLT string, err error) {
	senderLT, err = mt199NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt199NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt199Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if m.RelatedReference != "" {
		if err := mt199Reference(m.RelatedReference, ":21:"); err != nil {
			return "", "", err
		}
	}
	if err := mt199Narrative(m.Narrative); err != nil {
		return "", "", err
	}
	return senderLT, receiverLT, nil
}

// ExportMT199 renders m as a SWIFT FIN MT199 message: blocks 1–4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT199(m MT199) (string, error) {
	senderLT, receiverLT, err := mt199Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I199. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I199%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the field set, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	if m.RelatedReference != "" {
		fmt.Fprintf(&b, ":21:%s\n", m.RelatedReference)
	}
	fmt.Fprintf(&b, ":79:%s\n", m.Narrative[0])
	for _, ln := range m.Narrative[1:] {
		fmt.Fprintf(&b, "%s\n", ln)
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt199ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export ∘ Parse
// is stable: Export normalizes (BIC8 → BIC8+"XXX", the MT103 rule),
// and Parse returns that normalized form verbatim rather than
// re-normalizing it a second time (which would append another "X").
// Block 1 has the form `{1:F01<LT><10 session/sequence digits>}`;
// block 2 has the form `{2:I199<LT><5 header characters>}`
// (priority U, delivery monitoring 3, obsolescence 003).
func mt199ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT199Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT199Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT199Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT199LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT199LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT199LT, lt)
		}
	}
	return lt, nil
}

// ParseMT199 parses a SWIFT FIN MT199 message produced by
// ExportMT199 (or an equivalent correspondent-bank message) into
// its business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the three-field set before :79: is refused — a valued
// field such as :32A: in an MT199 is a different message type, not
// a narrative line. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated. A :79: field, once
// started, consumes every remaining block-4 line as a continuation
// (it is the last field, so a continuation line may itself start
// with a colon without being mistaken for a new field — the same
// rule as MT900/MT910's :72:).
func ParseMT199(text string) (MT199, error) {
	var zero MT199
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 7 {
		return zero, fmt.Errorf("%w: want at least 7 lines (3 headers + {4: + :20: + :79: + -}), got %d", ErrMT199Framing, len(lines))
	}
	senderLT, err := mt199ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt199ParseLT(lines[1], "{2:I199", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT199Framing, mur)
	}
	if err := mt199Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT199Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT199Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 2 {
		return zero, fmt.Errorf("%w: want at least :20: and :79:, got %d block-4 lines", ErrMT199Field, len(body))
	}
	var m MT199
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrMT199Field, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrMT199Field)
	}
	if err := mt199Reference(ref, ":20:"); err != nil {
		return zero, err
	}
	m.TransactionReference = ref
	idx := 1
	if idx < len(body) && strings.HasPrefix(body[idx], ":21:") {
		related := body[idx][len(":21:"):]
		if related == "" {
			return zero, fmt.Errorf("%w: :21: field is empty", ErrMT199Field)
		}
		if err := mt199Reference(related, ":21:"); err != nil {
			return zero, err
		}
		m.RelatedReference = related
		idx++
	}
	if idx >= len(body) || !strings.HasPrefix(body[idx], ":79:") {
		got := ""
		if idx < len(body) {
			got = body[idx]
		}
		return zero, fmt.Errorf("%w: want :79: field, got %q", ErrMT199Field, got)
	}
	narrative := []string{body[idx][len(":79:"):]}
	narrative = append(narrative, body[idx+1:]...)
	if err := mt199Narrative(narrative); err != nil {
		return zero, err
	}
	m.Narrative = narrative
	return m, nil
}

// WrapMT199Narrative wraps free prose into :79: narrative lines:
// greedy word-boundary wrapping at 35 characters per line, at most
// 50 lines. A single word longer than 35 characters is refused
// rather than split mid-token — splitting a UETR, an account
// number, or a reference across lines would corrupt the token, so
// the caller breaks such tokens deliberately instead. Empty text
// is refused: a narrative with nothing to say is not a message.
// The result is validated exactly as ExportMT199 validates it, so
// Wrap ∘ Export never fails on the wrapped lines.
func WrapMT199Narrative(text string) ([]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: narrative text is empty", ErrMT199Narrative)
	}
	words := strings.Fields(text)
	var lines []string
	cur := ""
	for _, w := range words {
		if len(w) > MT199NarrativeMaxLineLen {
			return nil, fmt.Errorf("%w: word exceeds %d characters: %q", ErrMT199Narrative, MT199NarrativeMaxLineLen, w)
		}
		if cur == "" {
			cur = w
			continue
		}
		if len(cur)+1+len(w) > MT199NarrativeMaxLineLen {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	lines = append(lines, cur)
	if err := mt199Narrative(lines); err != nil {
		return nil, err
	}
	return lines, nil
}
