package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT999 free-format message, parse + export (LG-458,
// fintech: SWIFT 自由格式报文).
//
// An MT999 is the category 9 (Cash Management and Customer Status)
// free-format message: the category 9 twin of the category 1 MT199
// (LG-380). The two share one field set and one shape — :20: /
// :21: / :79:, block 1/2/3/4 framing, 50*35x narrative — and differ
// only in the category the message speaks about: MT199 is the
// general free-format note, MT999 is the cash-management free-format
// note (balance, status, and liquidity prose between banks). The
// implementation therefore mirrors mt199.go field for field, with
// the block-2 type swapped to I999 and the error identities renamed.
//
// Like its MT199 twin, an MT999 carries no amount, no instruction,
// and no settlement semantics — only text. That is what separates it
// from its structured category 9 neighbours:
//
//   - MT900/MT910 (see mt900.go, LG-209) are post-settlement
//     *confirmations* of a valued movement.
//   - MT920 (see the LG-459 backlog item) is a *request* for a
//     statement.
//   - MT999 notifies, queries, or explains in prose. A parser that
//     found a :32A: in an MT999 would be reading a different message
//     type: this struct has no amount field at all, so an MT999 can
//     never be mistaken for a payment, and a payment can never be
//     smuggled through a free-format note.
//
// This module covers the block-4 field set the backlog names:
//
//	:20: transaction reference, :21: related reference,
//	:79: narrative.
//
// The `:21:` field is optional in the free-format network rules,
// and it is optional here too — but it is the context anchor the
// backlog asks for: it carries the transfer, payout, or statement
// ID the notification is about. The `:79:` narrative is mandatory:
// a free-format message with nothing to say is not a message. It
// carries 1–50 lines of at most 35 `x` characters each, the
// MT900/MT910 `:72:` convention with the MT199 line cap.
//
// Field table (verified against the Standards MT November 2017
// Category 9 Message Reference Guide, which lists "MT 999 Free
// Format Message" among the category 9 types, and the Oracle SWIFT
// Message Library Category 9 table, which lists "MT 999 | Free
// Format Message"): :20: 16x M, :21: 16x O, :79: 50*35x M. The
// :21: optionality follows the MT199 precedent, not the MT900
// :25: account rule — a free-format note about nothing in
// particular is still a note.
//
// Framing follows the MT103/MT202 convention: block 1 (basic header,
// sender LT, session/sequence zero-filled and interface-assigned),
// block 2 (application header, receiver LT, `I999`), block 3
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
// The pure value object is separate from the ledger-bound export
// below. The value is pure: no ledger lock, no version bump, no
// journal rows, no audit event, and export is deterministic per
// message (repeat exports render byte-identical output). Sending an
// MT999 changes nothing on the ledger — that is the point of a
// free-format note, and the reason it must never carry an amount.
// The ledger-bound method adds only evidence: a :20: registry entry
// (the idempotency guard) and one `mt999_exported` audit event, the
// durable record. No journal entry, no balance or status change, no
// version bump.
//
// Field discipline is fail-loud, the nacha.go convention: every field
// is validated on both parse and export, and any violation is an
// error, never a silent truncation or default — except the
// session/sequence zeros in block 1, which the MT103 convention
// documents as interface-assigned.

// MT999 parse/export errors.
var (
	// ErrMT999Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :79: order.
	ErrMT999Framing = errors.New("ledger: mt999: malformed FIN framing")
	// ErrMT999Reference is returned for an invalid :20: or :21:
	// reference: empty (:20: only — :21: may be omitted), over 16
	// characters, outside the SWIFT `x` character set, carrying a
	// brace or a line break, or with a leading/trailing colon or
	// slash, or a double slash.
	ErrMT999Reference = errors.New("ledger: mt999: invalid reference")
	// ErrMT999Narrative is returned for an invalid :79: narrative:
	// empty, over 50 lines, or a line that is empty, over 35
	// characters, or outside the SWIFT `x` character set (braces
	// and line breaks refused).
	ErrMT999Narrative = errors.New("ledger: mt999: invalid :79: narrative")
	// ErrMT999LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT999LT = errors.New("ledger: mt999: invalid SWIFT LT address")
	// ErrMT999Field is returned for a block-4 field problem that is
	// not a value problem: a missing required field (:20: or :79:),
	// a duplicate field, or a field outside the three-field set
	// (including any valued field such as :32A: before :79:).
	ErrMT999Field = errors.New("ledger: mt999: unexpected or missing field")
	// ErrMT999MsgIDConflict is returned when a :20: reference is
	// reused for different terms: the first export stands, and the
	// conflicting re-export is refused instead of silently
	// overwriting it (the pain.014 convention, along MT196).
	ErrMT999MsgIDConflict = errors.New("ledger: mt999: :20: reused for different terms")
)

// MT999NarrativeMaxLines is the MT999 :79: line cap (50 lines of at
// most 35 `x` characters each).
const MT999NarrativeMaxLines = 50

// MT999NarrativeMaxLineLen is the MT999 :79: per-line cap.
const MT999NarrativeMaxLineLen = 35

// MT999 is one SWIFT FIN MT999 free-format message: the block-4
// field set plus the block 1/2 LT addresses. RelatedReference is
// optional (empty omits :21:); Narrative is mandatory, 1–50 lines.
// The struct deliberately has no amount, currency, or value-date
// field: an MT999 carries prose, never value.
type MT999 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	RelatedReference     string
	Narrative            []string
}

// mt999Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash.
func mt999Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT999Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT999Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT999Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT999Reference, field, ref)
	}
	return nil
}

// mt999Narrative validates a :79: narrative: 1–50 lines, each 1–35
// SWIFT `x` characters, no braces, no line breaks inside a line.
func mt999Narrative(lines []string) error {
	if len(lines) == 0 || len(lines) > MT999NarrativeMaxLines {
		return fmt.Errorf("%w: want 1-%d lines, got %d", ErrMT999Narrative, MT999NarrativeMaxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > MT999NarrativeMaxLineLen {
			return fmt.Errorf("%w: line exceeds 1-%d characters: %q", ErrMT999Narrative, MT999NarrativeMaxLineLen, ln)
		}
		if strings.ContainsAny(ln, "\r\n") {
			return fmt.Errorf("%w: line break inside a :79: line: %q", ErrMT999Narrative, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %w", ErrMT999Narrative, err)
		}
	}
	return nil
}

// mt999NormalizeLT normalizes a sender/receiver LT address with the
// MT103 rule (BIC8 → BIC8+"XXX", BIC11 → BIC11+"X", BIC12 as-is),
// reported under the MT999 error identity.
func mt999NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT999LT, raw)
	}
	return lt, nil
}

// mt999Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses for rendering.
func mt999Validate(m MT999) (senderLT, receiverLT string, err error) {
	senderLT, err = mt999NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt999NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt999Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if m.RelatedReference != "" {
		if err := mt999Reference(m.RelatedReference, ":21:"); err != nil {
			return "", "", err
		}
	}
	if err := mt999Narrative(m.Narrative); err != nil {
		return "", "", err
	}
	return senderLT, receiverLT, nil
}

// ExportMT999 renders m as a SWIFT FIN MT999 message: blocks 1–4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT999(m MT999) (string, error) {
	senderLT, receiverLT, err := mt999Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I999. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I999%sU3003}\n", receiverLT)
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

// mt999ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export ∘ Parse
// is stable: Export normalizes (BIC8 → BIC8+"XXX", the MT103 rule),
// and Parse returns that normalized form verbatim rather than
// re-normalizing it a second time (which would append another "X").
// Block 1 has the form `{1:F01<LT><10 session/sequence digits>}`;
// block 2 has the form `{2:I999<LT><5 header characters>}`
// (priority U, delivery monitoring 3, obsolescence 003).
func mt999ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT999Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT999Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT999Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT999LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT999LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT999LT, lt)
		}
	}
	return lt, nil
}

// ParseMT999 parses a SWIFT FIN MT999 message produced by
// ExportMT999 (or an equivalent correspondent-bank message) into
// its business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the three-field set before :79: is refused — a valued
// field such as :32A: in an MT999 is a different message type, not
// a narrative line. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated. A :79: field, once
// started, consumes every remaining block-4 line as a continuation
// (it is the last field, so a continuation line may itself start
// with a colon without being mistaken for a new field — the same
// rule as MT900/MT910's :72:).
func ParseMT999(text string) (MT999, error) {
	var zero MT999
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 7 {
		return zero, fmt.Errorf("%w: want at least 7 lines (3 headers + {4: + :20: + :79: + -}), got %d", ErrMT999Framing, len(lines))
	}
	senderLT, err := mt999ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt999ParseLT(lines[1], "{2:I999", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT999Framing, mur)
	}
	if err := mt999Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT999Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT999Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 2 {
		return zero, fmt.Errorf("%w: want at least :20: and :79:, got %d block-4 lines", ErrMT999Field, len(body))
	}
	var m MT999
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrMT999Field, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrMT999Field)
	}
	if err := mt999Reference(ref, ":20:"); err != nil {
		return zero, err
	}
	m.TransactionReference = ref
	idx := 1
	if idx < len(body) && strings.HasPrefix(body[idx], ":21:") {
		related := body[idx][len(":21:"):]
		if related == "" {
			return zero, fmt.Errorf("%w: :21: field is empty", ErrMT999Field)
		}
		if err := mt999Reference(related, ":21:"); err != nil {
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
		return zero, fmt.Errorf("%w: want :79: field, got %q", ErrMT999Field, got)
	}
	narrative := []string{body[idx][len(":79:"):]}
	narrative = append(narrative, body[idx+1:]...)
	if err := mt999Narrative(narrative); err != nil {
		return zero, err
	}
	m.Narrative = narrative
	return m, nil
}

// MT999ExportResult is the recorded outcome of one MT999 export:
// the rendered message and its references. Duplicate reports
// whether the export was already rendered under the same :20: with
// the same terms: a no-op success, not an error.
type MT999ExportResult struct {
	// Reference is this export's own :20: transaction reference.
	Reference string `json:"reference"`
	// RelatedReference is the :21: related reference, empty when
	// the export carries none.
	RelatedReference string `json:"related_reference,omitempty"`
	// Message is the rendered SWIFT FIN MT999 text.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt999ExportRecord is the registry record for one rendered MT999
// message, keyed by :20: in mt999Exports: the message it was
// rendered from and the result, so a replay returns the same
// message without rendering twice.
type mt999ExportRecord struct {
	Message MT999
	Result  MT999ExportResult
}

// mt999TermsEqual reports whether a stored export record carries
// exactly the terms an export request would render: the same :21:
// related reference and the same :79: narrative, verbatim. The
// sender/receiver LTs are envelope, not terms, along the MT196
// precedent — a replay under different LTs still returns the first
// rendering, because the :20: names one export.
func mt999TermsEqual(rec mt999ExportRecord, m MT999) bool {
	if rec.Message.RelatedReference != m.RelatedReference {
		return false
	}
	if len(rec.Message.Narrative) != len(m.Narrative) {
		return false
	}
	for i, ln := range rec.Message.Narrative {
		if ln != m.Narrative[i] {
			return false
		}
	}
	return true
}

// ExportMT999 renders m as a SWIFT FIN MT999 free-format message
// and records the export as evidence: the :20: registry entry and
// one `mt999_exported` audit event (details msg_id /
// related_reference, trace :20:). Nothing moves: no journal entry,
// no balance or status change, no version bump — rendering a note
// is evidence-only, and the audit event is the durable record, not
// a state change.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, along MT196): a replay with the same related
// reference and narrative returns the stored rendered message with
// Duplicate set, rendering nothing twice and emitting no second
// audit event; a :20: reused for different terms is refused with
// ErrMT999MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read runs under the read lock; the :20: record and the
// export marker event run under a short write lock of their own,
// re-checking the registry first so a concurrent double-export
// still records once.
//
// (This is the Ledger method of the same name as the package-level
// ExportMT999 above: the unqualified call below is the pure
// renderer, not recursion — methods are not bare identifiers.)
func (l *Ledger) ExportMT999(m MT999, actor string) (MT999ExportResult, error) {
	if actor == "" {
		actor = "ExportMT999"
	}
	if _, _, err := mt999Validate(m); err != nil {
		return MT999ExportResult{}, err
	}

	// :20: replay: the message was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt999Exports[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt999TermsEqual(rec, m) {
			return MT999ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT999MsgIDConflict, m.TransactionReference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	msg, err := ExportMT999(m)
	if err != nil {
		return MT999ExportResult{}, err
	}
	res := MT999ExportResult{
		Reference:        m.TransactionReference,
		RelatedReference: m.RelatedReference,
		Message:          msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The audit
	// event carries no accounts: a free-format note touches no
	// ledger account, and inventing one would misattribute the
	// evidence.
	l.mu.Lock()
	if rec, ok := l.mt999Exports[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt999TermsEqual(rec, m) {
			return MT999ExportResult{}, fmt.Errorf("%w: :20: %q", ErrMT999MsgIDConflict, m.TransactionReference)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt999Exports == nil {
		l.mt999Exports = make(map[string]mt999ExportRecord)
	}
	l.mt999Exports[m.TransactionReference] = mt999ExportRecord{Message: m, Result: res}
	l.emitAudit(AuditEvent{
		Op:            "mt999_exported",
		Actor:         actor,
		TraceID:       m.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            m.TransactionReference,
			"related_reference": m.RelatedReference,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT999Export returns the recorded export for msgID, or false
// when no message was ever rendered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT999Export(msgID string) (MT999ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt999Exports[msgID]
	if !ok {
		return MT999ExportResult{}, false
	}
	return rec.Result, true
}
