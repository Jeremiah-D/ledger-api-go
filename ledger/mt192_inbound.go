package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT192 request for cancellation inbound processing
// (LG-438): the inbound half of the MT192 lifecycle, the mirror
// of the LG-434 export.
//
// ExportMT192 renders this ledger's own cancellation request —
// the ledger telling the correspondent bank "cancel the MT103
// you already hold". This module consumes the cancellation
// request a correspondent bank sent to this ledger, acting as
// the receiving bank: it cancels the payout the request names,
// and nothing else. The two directions are orthogonal by
// construction:
//
//   - LG-434 exports: the ledger asks the correspondent to
//     cancel one of its own issued MT103s (outbound).
//   - LG-438 imports: a correspondent bank asks the ledger to
//     cancel a payout (inbound, executing through the same
//     RequestPayoutCancellation entry the camt.056 inbound path
//     uses, LG-414).
//
// ParseMT192 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT202 (LG-204): the block
// structure, the field order, and every field value are
// validated, and anything outside the four-field core set
// (:20: / :21: / :11S: / :79:) is refused — a cancellation
// instruction must not lose content quietly. CRLF line endings
// are accepted; a trailing newline after the `-}` trailer is
// tolerated. The :79: narrative may span lines: its first line
// carries the `:79:` tag and the following bare lines continue
// it, the export's own rendering, so a document this ledger's
// export renders parses back. A bare line starting with `:` inside
// the narrative run is an unknown field, not a continuation.
//
// The cancellation routes through the existing
// RequestPayoutCancellation entry, never a direct payout write:
// the entry owns its gates (unknown UETR, settled or terminal
// gpi track, already-cancelled payout, unissued payout, empty
// reason) and its own `cancellation_requested` audit event, so
// the trail shows the cancellation exactly as an operator-driven
// one would. ApplyMT192 additionally emits `mt192_request_applied`
// (details msg_id / payout_id / uetr / reason_code / value_date)
// so the trail tells an inbound MT192 apart from a local
// cancellation — and so the MT192 :20: reference itself is on
// the durable record, which RequestPayoutCancellation (written
// for the operator path, before any MT192 existed) cannot
// carry. The :79: narrative rides verbatim as the reason code —
// never paraphrased, never translated, and no reason-code
// allowlist is invented.
//
// The gate difference from the export is deliberate and stated
// in the LG-438 spec: the export allows approved or submitted
// payouts (the instruction is out there, or about to be), while
// the inbound path cancels only an issued submitted payout —
// an approved payout never left the ledger, so it goes through
// CancelPayout instead, and RequestPayoutCancellation's
// CancellationNotIssued gate refuses it here. A settled payout
// goes via pacs.007 (LG-411), never through this path.
//
// The :11S: value date must agree with the payout's MT103 value
// date (the export's default rule: submit time, then approval
// time, then creation time, via mt103ValueDate): a cancellation
// request naming another value date points at another
// instruction, so it is refused with
// ErrMT192ValueDateMismatch rather than cancelling the wrong
// one. The check compares dates, not instants — :11S: carries
// only a YYMMDD day.
//
// The :20: reference is the MsgId and the replay key — one :20:
// names one cancellation request.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// payout, value date, and reason returns the current
// cancellation record with Duplicate set, cancelling nothing
// twice and emitting no second audit event. A :20: reused for
// different terms is refused with ErrMT192MsgIDConflict — never
// silently overwritten. The registry check runs again under the
// write lock after RequestPayoutCancellation returns, so two
// concurrent applies of the same :20: still emit exactly one
// marker event: RequestPayoutCancellation's own idempotency
// returns the open record without an event, and the second
// applier finds the registry already filled.
//
// No payout is ever auto-created, no MT192 receipt is consumed
// here: the cancellation is executed first, and the bank's
// verdict on it arrives separately through the camt.029 path
// (LG-174). One MT192 cancels exactly one payout — no batch
// cancellation, no nostro credit, no pacs.007.

// MT192 inbound errors.
var (
	// ErrMT192Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :11S: → :79: order.
	ErrMT192Framing = errors.New("ledger: mt192: malformed FIN framing")
	// ErrMT192Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, or a
	// field outside the four-field core set (including the D
	// variants and the optional :72: field).
	ErrMT192Field = errors.New("ledger: mt192: unexpected or missing field")
	// ErrMT192Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT192Reference = errors.New("ledger: mt192: invalid reference")
	// ErrMT192ValueDate is returned for an invalid :11S: value
	// date: not "103" plus a YYMMDD calendar date.
	ErrMT192ValueDate = errors.New("ledger: mt192: invalid :11S: value date")
	// ErrMT192LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT192LT = errors.New("ledger: mt192: invalid SWIFT LT address")
	// ErrMT192MsgIDConflict is returned by ApplyMT192 when a
	// :20: reference already applied carries different terms: the
	// reference names one cancellation request, and a second,
	// different request under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT192MsgIDConflict = errors.New("ledger: mt192 inbound: message reference already applied with different terms")
	// ErrMT192ValueDateMismatch is returned by ApplyMT192 when
	// the request's :11S: value date disagrees with the payout's
	// MT103 value date: the request points at another
	// instruction, not this payout. It is a 422-class error.
	ErrMT192ValueDateMismatch = errors.New("ledger: mt192 inbound: :11S: value date does not match the payout's MT103 value date")
)

// MT192 is one SWIFT FIN MT192 request for cancellation: the
// block-4 core fields plus the block 1/2 LT addresses. ValueDate
// is a UTC calendar date (time-of-day is not carried by :11S:);
// Reason is the :79: narrative joined with "\n", empty when the
// message carries no :79: block.
type MT192 struct {
	SenderLT         string
	ReceiverLT       string
	Reference        string // :20:, this message's own reference, the replay key.
	RelatedReference string // :21:, the original MT103's :20: — the payout ID.
	ValueDate        time.Time
	Reason           string
}

// MT192ApplyResult is the outcome of ApplyMT192.
type MT192ApplyResult struct {
	// Cancellation is the cancellation record opened (or already
	// open) for the payout's UETR.
	Cancellation PayoutCancellation `json:"cancellation"`
	// Reference echoes the request's :20: reference, the
	// identity the cancellation was requested under.
	Reference string `json:"reference"`
	// Duplicate reports whether the cancellation was already
	// requested under the same :20: with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt192AppliedRecord is the replay record for one applied MT192
// cancellation request, keyed by :20: in mt192Applied: the payout
// it cancelled, the value date and reason it carried, and the
// cancellation record, so a replay returns the same outcome
// without cancelling twice.
type mt192AppliedRecord struct {
	Reference    string
	PayoutID     string
	ValueDate    time.Time
	Reason       string
	Cancellation PayoutCancellation
}

// mt192TermsEqual reports whether a stored cancellation record
// carries exactly the terms an inbound request would apply: the
// same payout, the same value date (by calendar day, as :11S:
// carries only a day), and the same reason, verbatim.
func mt192TermsEqual(rec mt192AppliedRecord, req MT192) bool {
	return rec.PayoutID == req.RelatedReference &&
		mt103ValueDate(rec.ValueDate) == mt103ValueDate(req.ValueDate) &&
		rec.Reason == req.Reason
}

// mt192Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT192 error identity.
func mt192Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT192Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT192Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT192Reference, field, ref)
	}
	return nil
}

// mt192ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I192<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT192 error identity.
func mt192ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT192Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT192Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT192Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT192LT, lt)
	}
	return lt, nil
}

// mt192ParseReasonLines validates the :79: narrative: at most 35 lines
// of at most 35 SWIFT `x` characters each (braces refused, the
// block-4 framing rule). The export's shape, under the MT192
// field error identity.
func mt192ParseReasonLines(lines []string) error {
	if len(lines) > mt192MaxReasonLines {
		return fmt.Errorf("%w: :79: at most %d lines", ErrMT192Field, mt192MaxReasonLines)
	}
	for _, ln := range lines {
		if len(ln) > mt192MaxReasonLine {
			return fmt.Errorf("%w: :79: line exceeds %d characters: %q", ErrMT192Field, mt192MaxReasonLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: :79: line: %v", ErrMT192Field, err)
		}
	}
	return nil
}

// ParseMT192 parses a SWIFT FIN MT192 message produced by
// ExportMT192 (or an equivalent correspondent-bank message) into
// its business fields. Parsing is strict: the block structure,
// the field order, and every field value are validated, and
// anything outside the four-field core set is refused — a
// cancellation instruction must not lose content quietly. CRLF
// line endings are accepted; a trailing newline after the `-}`
// trailer is tolerated.
func ParseMT192(text string) (MT192, error) {
	var zero MT192
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + 3 fields + -}), got %d", ErrMT192Framing, len(lines))
	}
	senderLT, err := mt192ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt192ParseLT(lines[1], "{2:I192", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT192Framing, mur)
	}
	if err := mt192Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT192Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT192Framing, lines[len(lines)-1])
	}
	// Block 4, canonical order, single-line fields for :20: /
	// :21: / :11S:: a field body never spans lines in this core
	// set, so any line not starting with its expected tag is a
	// framing error, not a continuation.
	fields := []struct {
		tag, body string
	}{
		{":20:", ""}, {":21:", ""}, {":11S:", ""},
	}
	for i, want := range fields {
		line := lines[4+i]
		if !strings.HasPrefix(line, want.tag) {
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT192Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT192Field, want.tag)
		}
	}
	var m MT192
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt192Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.Reference = fields[0].body
	if err := mt192Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :11S: = "103" + YYMMDD value date.
	body11S := fields[2].body
	if len(body11S) != 3+6 || !strings.HasPrefix(body11S, "103") {
		return zero, fmt.Errorf("%w: :11S: want 103 + YYMMDD, got %q", ErrMT192ValueDate, body11S)
	}
	vd, err := time.Parse("060102", body11S[3:])
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT192ValueDate, body11S[3:], err)
	}
	m.ValueDate = vd.UTC()
	// :79:, when present: the first line carries the tag, the
	// following bare lines continue the narrative until the
	// trailer. A line starting with `:` there is an unknown
	// field, not a continuation.
	i := 7
	var reasonLines []string
	if i < len(lines)-1 {
		line := lines[i]
		if !strings.HasPrefix(line, ":79:") {
			return zero, fmt.Errorf("%w: line %d: want :79: field or -} trailer, got %q", ErrMT192Field, i+1, line)
		}
		reasonLines = append(reasonLines, line[len(":79:"):])
		i++
		for ; i < len(lines)-1; i++ {
			line := lines[i]
			if strings.HasPrefix(line, ":") {
				return zero, fmt.Errorf("%w: line %d: unknown field %q", ErrMT192Field, i+1, line)
			}
			reasonLines = append(reasonLines, line)
		}
		if err := mt192ParseReasonLines(reasonLines); err != nil {
			return zero, err
		}
		m.Reason = strings.Join(reasonLines, "\n")
	}
	return m, nil
}

// mt192PayoutValueDate returns the payout's MT103 value date by
// the export's default rule: submit time, then approval time,
// then creation time.
func mt192PayoutValueDate(p PayoutInstruction) time.Time {
	vd := p.SubmittedAt
	if vd.IsZero() {
		vd = p.ApprovedAt
	}
	if vd.IsZero() {
		vd = p.CreatedAt
	}
	return vd
}

// validateMT192Request fail-closes on a cancellation request
// ParseMT192 would refuse, so ApplyMT192 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy. The reason is required here —
// :79: is optional on the wire, but a cancellation without a
// reason cannot be executed (the camt.056 reason-required gate).
func validateMT192Request(req MT192) error {
	if err := mt192Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt192Reference(req.RelatedReference, ":21:"); err != nil {
		return err
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: value date is required", ErrMT192ValueDate)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return ErrCancellationReasonRequired
	}
	return nil
}

// ApplyMT192 applies one parsed inbound SWIFT FIN MT192 request
// for cancellation: it cancels the payout the :21: names through
// the existing RequestPayoutCancellation entry.
//
// The payout must exist (:21: unknown is
// ErrMT103PayoutNotFound, the export's error), and its MT103
// value date must agree with the request's :11S:
// (ErrMT192ValueDateMismatch). The cancellation itself runs
// through RequestPayoutCancellation, which owns the issued-only
// gate (approved is ErrCancellationNotIssued, settled is
// ErrCancellationSettled, already-cancelled is
// ErrCancellationAlreadyCancelled, failed/terminal is
// ErrCancellationTerminal) and its own `cancellation_requested`
// audit event. No new payout-gate error is introduced.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same payout, value date, and
// reason returns the current cancellation record with Duplicate
// set, cancelling nothing twice and emitting no second audit
// event; a :20: reused for different terms is refused with
// ErrMT192MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the payout read run under the read lock;
// RequestPayoutCancellation takes the write lock itself; the
// :20: record and the inbound marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-apply still records once.
func (l *Ledger) ApplyMT192(req MT192, actor string) (MT192ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT192"
	}
	if err := validateMT192Request(req); err != nil {
		return MT192ApplyResult{}, err
	}

	// :20: replay: the cancellation was already requested under
	// this reference. Same terms is a no-op returning the current
	// cancellation record; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt192Applied[req.Reference]; ok {
		l.mu.RUnlock()
		if !mt192TermsEqual(rec, req) {
			return MT192ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT192MsgIDConflict, req.Reference)
		}
		if current, ok := l.GetPayoutCancellation(rec.Cancellation.UETR); ok {
			return MT192ApplyResult{Cancellation: current, Reference: req.Reference, Duplicate: true}, nil
		}
		return MT192ApplyResult{Cancellation: rec.Cancellation, Reference: req.Reference, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The payout itself: :21: names it, the original MT103's
	// :20:. Unknown is the export's error, by the spec.
	l.mu.RLock()
	p, ok := l.payouts[req.RelatedReference]
	l.mu.RUnlock()
	if !ok {
		return MT192ApplyResult{}, fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, req.RelatedReference)
	}
	// The :11S: value date must be the payout's MT103 value
	// date: a request naming another day points at another
	// instruction.
	wantDate := mt103ValueDate(mt192PayoutValueDate(p))
	if gotDate := mt103ValueDate(req.ValueDate); gotDate != wantDate {
		return MT192ApplyResult{}, fmt.Errorf("%w: request holds %q, payout holds %q",
			ErrMT192ValueDateMismatch, gotDate, wantDate)
	}

	// The cancellation itself, through the existing entry: it
	// owns the unknown-UETR / settled / terminal /
	// already-cancelled / unissued gates and the
	// cancellation_requested audit event. The :79: narrative
	// rides verbatim as the reason code.
	c, err := l.RequestPayoutCancellation(p.UETR, req.Reason, actor)
	if err != nil {
		return MT192ApplyResult{}, err
	}

	// The :20: record and the inbound direction marker, under
	// one short write lock: the cancellation is already durable
	// when the marker lands. A concurrent double-apply that
	// landed first returns the stored outcome instead of a
	// second marker.
	l.mu.Lock()
	if rec, ok := l.mt192Applied[req.Reference]; ok {
		l.mu.Unlock()
		if !mt192TermsEqual(rec, req) {
			return MT192ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT192MsgIDConflict, req.Reference)
		}
		// The stored record is the outcome: no
		// GetPayoutCancellation refresh here — the write lock
		// is held, and it takes the read lock.
		return MT192ApplyResult{Cancellation: rec.Cancellation, Reference: req.Reference, Duplicate: true}, nil
	}
	if l.mt192Applied == nil {
		l.mt192Applied = make(map[string]mt192AppliedRecord)
	}
	l.mt192Applied[req.Reference] = mt192AppliedRecord{
		Reference:    req.Reference,
		PayoutID:     req.RelatedReference,
		ValueDate:    req.ValueDate,
		Reason:       req.Reason,
		Cancellation: c,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt192_request_applied",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"msg_id":      req.Reference,
			"payout_id":   req.RelatedReference,
			"uetr":        p.UETR,
			"reason_code": req.Reason,
			"value_date":  wantDate,
		},
	})
	l.mu.Unlock()
	return MT192ApplyResult{Cancellation: c, Reference: req.Reference}, nil
}
