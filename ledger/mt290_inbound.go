package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT290 advice of charges inbound processing (LG-456):
// the inbound half of the MT290 lifecycle. There is no outbound
// half in this ledger — the ledger never advises a correspondent
// bank of charges it took; it records the advices correspondent
// banks send it. The advice names an earlier bank-to-bank
// transfer and states what the correspondent charged for it; the
// answer is a record of ledger facts, never a new instruction.
//
// MT290 is the MT202-side twin of MT190 (LG-454): MT190 advises
// charges on a customer payment (MT103), MT290 advises charges
// on a financial-institution transfer (MT202). The shape is the
// MT190 shape with the anchor moved: the :21: names an MT202
// instruction instead of a payout, and the amount field is
// :32B: (currency and amount only) instead of :32A: — a charge
// advice for a bank-to-bank transfer carries no value date of
// its own; the value date lives on the MT202's :32A:. The CBPR+
// roadmap pairs MT190 and MT290 alike with camt.105.
//
// Field set, verified against the SWIFT MT290 definition before
// this module was written (2026-11-21, ISO 15022 User Handbook,
// MT n90 Advice of Charges, Interest and Other Adjustments,
// iso20022.org/15022/uhb/finmtn90.htm — the n90 family table
// covering MT190, MT290 and MT990):
//
//   - :20: Transaction Reference Number (16x, mandatory) — the
//     advice's own reference, the MsgId and the replay key.
//   - :21: Related Reference (16x, mandatory) — the original
//     message's :20:, the MT202 instruction ID, the anchor
//     ParseMT192 uses for :21:.
//   - :32B: Currency Code, Amount (3!a15d, mandatory) — the
//     amount charged. The n90 table's field 32a carries options
//     C or D; MT290 uses option D (:32B:, no value date).
//     :32A: (option C, the MT190 advice's dated charge) is
//     refused, never silently accepted with its date dimension
//     dropped — an MT290 has no value-date field to put it in.
//   - :71B: Details of Charges (6*35x, mandatory) — the
//     free-format detail of what the charge is for, carried
//     verbatim and never interpreted or translated. Mandatory
//     here: the n90 table marks it M, and a charge advice that
//     does not state what the charge is for is not evidence an
//     operator can use. This differs from the MT190 advice,
//     where LG-454 treats :71B: as optional; it matches the
//     MT191 request, where the Oracle MT191 table likewise
//     marks :71B: M and LG-455 made it mandatory.
//
// Outside the core set, refused: :25: (account identification —
// the ledger knows the MT202's accounts), :52a: / :57a:
// (ordering / account-with institution — routing, not evidence),
// and :72: (sender-to-receiver information). A charge advice
// must not lose content quietly.
//
// Parsing is strict, along ParseMT192 (LG-438): the block
// structure, the field order (:20: → :21: → :32B: → :71B:), and
// every field value are validated, and anything outside the
// four-field core set is refused. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated. A bare
// line starting with `:` inside the :71B: run is an unknown
// field, not a continuation.
//
// ApplyMT290 resolves the :21: in two tiers (LG-484, the
// LG-482/LG-483 anchor-tier family): (a) the applied MT202
// instructions (mt202Applied, keyed by the MT202's :20:), (b)
// the ledger's own rendered MT202 exports (mt202Exports,
// keyed by the export's :20:, LG-470) — a charge advice may
// name a transfer the ledger sent, not only one it received.
// It records the advice under the MT202 in mt202Charges, with
// one `mt290_charges_advised` audit event (details msg_id /
// mt202_ref / currency / amount_cents / anchored, plus the
// :71B: text verbatim) putting the :20: on the durable record.
// Registry presence is the resolution test: a parked or failed
// MT202 is still known to the ledger and still has terms —
// its currency — to check the advice against, so the advice
// records against the instruction reference either way. An
// unknown :21: is fail-closed into the repair queue
// (RepairReasonUnknownRoute, the camt.055 shape: the parked
// result carries the repair ID alongside the
// ErrMT290MT202NotFound error); a :32B: currency that
// disagrees with the MT202's currency is likewise parked
// (RepairReasonInvalidFormat — the advice is well-formed but
// disagrees with the ledger's fact about the instruction),
// never silently converted. A non-positive amount is refused
// at parse — it is a malformed document, not a routable one —
// and at the apply gate for hand-built requests.
//
// Nothing moves: no journal entry, no balance change, no MT202
// status change. A charge advice is evidence, not a debit
// instruction — the debit itself is evidenced separately by
// camt.054 / the bank statement, and the MT202's own amount is
// untouched: the charge is the correspondent's fee, independent
// of the transfer amount, and is never checked against it.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// MT202, currency, amount, and details returns the stored
// advice with Duplicate set, recording nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT290MsgIDConflict — never silently
// overwritten. The registry check runs again under the write
// lock after the advice is built, so two concurrent applies of
// the same :20: still record once.

// MT290 inbound errors.
var (
	// ErrMT290Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :32B: → :71B: order.
	ErrMT290Framing = errors.New("ledger: mt290: malformed FIN framing")
	// ErrMT290Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, or a
	// field outside the four-field core set (including :25:,
	// :32A:, :52a:, :57a:, and :72:).
	ErrMT290Field = errors.New("ledger: mt290: unexpected or missing field")
	// ErrMT290Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT290Reference = errors.New("ledger: mt290: invalid reference")
	// ErrMT290Currency is returned for an invalid :32B:
	// currency: not exactly three uppercase ASCII letters.
	ErrMT290Currency = errors.New("ledger: mt290: invalid :32B: currency")
	// ErrMT290Amount is returned for an invalid :32B: amount:
	// not `<integer>,<2 fraction digits>`, or not positive.
	ErrMT290Amount = errors.New("ledger: mt290: invalid :32B: amount")
	// ErrMT290LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT290LT = errors.New("ledger: mt290: invalid SWIFT LT address")
	// ErrMT290MsgIDConflict is returned by ApplyMT290 when a
	// :20: reference already applied carries different terms: the
	// reference names one charge advice, and a second, different
	// advice under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT290MsgIDConflict = errors.New("ledger: mt290 inbound: message reference already applied with different terms")
	// ErrMT290CurrencyMismatch is returned by ApplyMT290 when
	// the advice's :32B: currency disagrees with the MT202's
	// currency: the advice points at another currency's charge,
	// so it is parked in the repair queue rather than converted.
	// It is a 422-class error.
	ErrMT290CurrencyMismatch = errors.New("ledger: mt290 inbound: :32B: currency does not match the MT202's currency")
	// ErrMT290MT202NotFound is returned by ApplyMT290 when the
	// advice's :21: names no MT202 instruction the ledger ever
	// applied or rendered: the advice points at a transfer the
	// ledger does not know, so it is parked in the repair queue
	// rather than recorded against nothing. It is a 422-class
	// error.
	ErrMT290MT202NotFound = errors.New("ledger: mt290 inbound: :21: names no applied or rendered MT202 instruction")
)

// MT290 line caps: :71B: is 6*35x, each line 1–35 SWIFT `x`
// characters.
const (
	mt290MaxDetailsLines = 6
	mt290MaxDetailsLine  = 35
)

// MT290 is one SWIFT FIN MT290 advice of charges: the block-4
// core fields plus the block 1/2 LT addresses. AmountCents is
// the :32B: amount in minor units, always positive; Details is
// the :71B: text joined with "\n". There is no value date: an
// advice carries :32B:, not :32A: — the value date lives on the
// MT202 the :21: names.
type MT290 struct {
	SenderLT         string
	ReceiverLT       string
	Reference        string // :20:, this message's own reference, the replay key.
	RelatedReference string // :21:, the original MT202's :20: — the MT202 ID.
	Currency         string // :32B:, three uppercase letters.
	AmountCents      int64  // :32B:, minor units, always positive.
	Details          string // :71B:, verbatim, joined with "\n".
}

// MT202ChargeAdvice is one recorded MT290 charge advice: the
// evidence a correspondent bank's fee advice leaves on an MT202
// instruction. Details is the :71B: text verbatim; AppliedAt is
// when this ledger recorded it. The advice never moves funds:
// it carries no journal entry ID, and none is ever posted for
// it. There is no value date: :32B: carries none, and the
// MT202's own :32A: is the dated leg.
type MT202ChargeAdvice struct {
	MsgID       string    `json:"msg_id"`
	MT202Ref    string    `json:"mt202_ref"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	Details     string    `json:"details"`
	Actor       string    `json:"actor"`
	AppliedAt   time.Time `json:"applied_at"`
}

// MT290ApplyResult is the outcome of ApplyMT290: the recorded
// advice for the charged MT202. A parked advice (unknown :21:,
// or a :32B: currency that disagrees with the MT202's) carries
// only Reference, MT202Ref, and RepairID, alongside the error.
type MT290ApplyResult struct {
	// Advice is the recorded charge advice. Empty on a parked
	// advice.
	Advice MT202ChargeAdvice `json:"advice"`
	// Reference echoes the advice's :20: reference.
	Reference string `json:"reference"`
	// MT202Ref is the charged MT202 instruction (the :21:).
	MT202Ref string `json:"mt202_ref"`
	// RepairID is the repair queue record for a parked advice
	// (unknown :21: or currency mismatch). Empty on a recorded
	// advice.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the advice was already recorded
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt290AppliedRecord is the replay record for one applied MT290
// charge advice, keyed by :20: in mt290Applied: the advice as
// recorded, so a replay returns the same outcome without
// recording twice.
type mt290AppliedRecord struct {
	Advice MT202ChargeAdvice
}

// mt290TermsEqual reports whether a stored advice record
// carries exactly the terms an inbound advice would apply: the
// same MT202, the same currency, the same amount, and the same
// :71B: details, verbatim. AppliedAt is deliberately excluded:
// two applies of the same terms at different times are the same
// advice.
func mt290TermsEqual(rec mt290AppliedRecord, req MT290) bool {
	a := rec.Advice
	return a.MT202Ref == req.RelatedReference &&
		a.Currency == req.Currency &&
		a.AmountCents == req.AmountCents &&
		a.Details == req.Details
}

// mt290Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT290 error identity.
func mt290Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT290Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT290Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT290Reference, field, ref)
	}
	return nil
}

// mt290ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I290<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT290 error identity.
func mt290ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT290Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT290Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT290Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT290LT, lt)
	}
	return lt, nil
}

// mt290Currency validates a :32B: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT290
// error identity.
func mt290Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT290Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT290Currency, code)
		}
	}
	return nil
}

// mt290ParseAmount parses a :32B: amount body
// (`<integer>,<ff>`) into cents. The integer part holds at most
// 12 digits; the fraction part holds exactly two digits; the
// value must be positive. The MT202 rule, under the MT290 error
// identity.
func mt290ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT290Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT290Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT290Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT290Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT290Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT290Amount, body)
	}
	return cents, nil
}

// mt290ParseTextLines validates one multi-line text field: at
// most maxLines lines of 1–35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT195 narrative shape,
// under the MT290 field error identity.
func mt290ParseTextLines(lines []string, tag string, maxLines int) error {
	if len(lines) > maxLines {
		return fmt.Errorf("%w: %s: at most %d lines", ErrMT290Field, tag, maxLines)
	}
	for _, ln := range lines {
		if len(ln) == 0 || len(ln) > mt290MaxDetailsLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", ErrMT290Field, tag, mt290MaxDetailsLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: line: %v", ErrMT290Field, tag, err)
		}
	}
	return nil
}

// mt290ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The run stops at the trailer. The MT195 run shape, under the
// MT290 field error identity.
func mt290ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines)-1; i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT290Field, tag)
	}
	return run, i, nil
}

// ParseMT290 parses a SWIFT FIN MT290 advice of charges message
// into its business fields. Parsing is strict: the block
// structure, the field order, and every field value are
// validated, and anything outside the four-field core set (:20: /
// :21: / :32B: / :71B:) is refused — a charge advice must not
// lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated.
func ParseMT290(text string) (MT290, error) {
	var zero MT290
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	// Eight lines is the floor for field parsing (3 headers +
	// {4: + 3 single-line fields + -}); a document that short
	// is missing its mandatory :71B:, which the field check
	// below refuses as a missing required field, not a framing
	// error.
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + 3 fields + -}), got %d", ErrMT290Framing, len(lines))
	}
	senderLT, err := mt290ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt290ParseLT(lines[1], "{2:I290", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT290Framing, mur)
	}
	if err := mt290Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT290Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT290Framing, lines[len(lines)-1])
	}
	// Block 4, canonical order, single-line fields for :20: /
	// :21: / :32B:: a field body never spans lines in this core
	// set, so any line not starting with its expected tag is a
	// framing error, not a continuation.
	fields := []struct {
		tag, body string
	}{
		{":20:", ""}, {":21:", ""}, {":32B:", ""},
	}
	for i, want := range fields {
		line := lines[4+i]
		if !strings.HasPrefix(line, want.tag) {
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT290Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT290Field, want.tag)
		}
	}
	var m MT290
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt290Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.Reference = fields[0].body
	if err := mt290Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :32B: = 3-letter currency + <integer>,<2 digits>. No value
	// date: an advice carries :32B:, not :32A: — the value date
	// lives on the MT202 the :21: names.
	body32 := fields[2].body
	if len(body32) < 3+4 {
		return zero, fmt.Errorf("%w: :32B: too short: %q", ErrMT290Framing, body32)
	}
	m.Currency = body32[:3]
	if err := mt290Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt290ParseAmount(body32[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	// :71B: is mandatory in MT290 (the SWIFT n90 field table
	// marks it M): the first line carries the tag, the
	// following bare lines continue the details until the
	// trailer. A line starting with `:` there is an unknown
	// field, not a continuation.
	i := 7
	if i >= len(lines)-1 || !strings.HasPrefix(lines[i], ":71B:") {
		return zero, fmt.Errorf("%w: line %d: want :71B: field, got %q", ErrMT290Field, i+1, lines[i])
	}
	run, next, err := mt290ParseTextRun(lines, i, ":71B:")
	if err != nil {
		return zero, err
	}
	if err := mt290ParseTextLines(run, ":71B:", mt290MaxDetailsLines); err != nil {
		return zero, err
	}
	m.Details = strings.Join(run, "\n")
	i = next
	// Nothing but the trailer may follow.
	if i != len(lines)-1 {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT290Field, i+1, lines[i])
	}
	return m, nil
}

// validateMT290Request fail-closes on a charge advice
// ParseMT290 would refuse, so ApplyMT290 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT290Request(req MT290) error {
	if err := mt290Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt290Reference(req.RelatedReference, ":21:"); err != nil {
		return err
	}
	if err := mt290Currency(req.Currency); err != nil {
		return err
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: %d is not positive", ErrMT290Amount, req.AmountCents)
	}
	if req.Details == "" {
		return fmt.Errorf("%w: :71B: field is empty", ErrMT290Field)
	}
	if err := mt290ParseTextLines(strings.Split(req.Details, "\n"), ":71B:", mt290MaxDetailsLines); err != nil {
		return err
	}
	return nil
}

// parkMT290Advice parks a fail-closed parsed advice in the
// repair queue and returns the record ID. The snapshot names
// the charged MT202 (:21:) as the instruction ID and the advice
// (:20:) as the destination, so an operator can find the advice
// from the record. The snapshot's Maker is deliberately empty
// (see the package comment): the record is operator visibility,
// never a resubmittable instruction. A parking failure (a full
// queue) is returned so the caller can fail loud rather than
// silently drop the bank's message.
func (l *Ledger) parkMT290Advice(req MT290, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.RelatedReference,
		Destination: "mt290-inbound:" + req.Reference,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT290 applies one parsed inbound SWIFT FIN MT290 advice
// of charges: it records the correspondent bank's fee advice
// against the MT202 instruction the :21: names, in
// mt202Charges, with one `mt290_charges_advised` audit event.
//
// The MT202 must be known: its :20: must sit in mt202Applied
// or in mt202Exports — two tiers, in order (LG-484): (a) an
// inbound MT202 the ledger applied, (b) an outbound MT202 the
// ledger itself rendered. A :21: hitting either tier is
// accepted, "accepted whichever tier hits" along LG-482/LG-483;
// on an overlap the inbound tier wins: its currency is the
// check and its legs anchor the audit event. A :21: unknown
// in both tiers is ErrMT290MT202NotFound, parked in the repair
// queue fail-closed (the camt.055 shape: the returned result
// carries the repair ID alongside the error); the :32B:
// currency must agree with the hit tier's currency (a mismatch
// is parked the same way, with ErrMT290CurrencyMismatch —
// never silently converted). The audit event names the hit
// tier in its `anchored` detail ("mt202" / "outbound-mt202",
// the MT295AnchorKind vocabulary). No journal entry is ever
// posted, no balance is ever changed, and no MT202 state is
// ever changed: an advice is evidence, not a debit
// instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same MT202, currency, amount,
// and details returns the stored advice with Duplicate set,
// recording nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT290MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the MT202 read run under the read lock; the
// :20: record, the MT202's advice list, and the inbound marker
// event run under a short write lock of their own, re-checking
// the registry first so a concurrent double-apply still records
// once.
func (l *Ledger) ApplyMT290(req MT290, actor string) (MT290ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT290"
	}
	if err := validateMT290Request(req); err != nil {
		return MT290ApplyResult{}, err
	}

	// :20: replay: the advice was already recorded under this
	// reference. Same terms is a no-op returning the stored
	// advice; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt290Applied[req.Reference]; ok {
		l.mu.RUnlock()
		if !mt290TermsEqual(rec, req) {
			return MT290ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT290MsgIDConflict, req.Reference)
		}
		return MT290ApplyResult{Advice: rec.Advice, Reference: req.Reference, MT202Ref: rec.Advice.MT202Ref, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The MT202 itself: :21: names it, the original MT202's
	// :20:. Two tiers, read under one read lock (LG-484): (a)
	// an inbound MT202 the ledger applied (mt202Applied), (b)
	// an outbound MT202 the ledger itself rendered
	// (mt202Exports, registration alone — no shape to check,
	// an outbound MT202 :20: is caller-supplied, 1-16 `x`
	// characters). Unknown in both tiers is parked fail-closed
	// in the repair queue (the camt.055 shape) and reported as
	// the instruction's error. The records are copied out under
	// the read lock: the hit tier's currency is the check, the
	// inbound tier's legs anchor the audit event.
	l.mu.RLock()
	mt202rec, inMT202 := l.mt202Applied[req.RelatedReference]
	mt202exp, outMT202 := l.mt202Exports[req.RelatedReference]
	l.mu.RUnlock()
	if !inMT202 && !outMT202 {
		repairID, parkErr := l.parkMT290Advice(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return MT290ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT290MT202NotFound, req.RelatedReference, parkErr)
		}
		return MT290ApplyResult{Reference: req.Reference, MT202Ref: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT290MT202NotFound, req.RelatedReference)
	}

	// The :32B: currency must be the MT202's currency, taken
	// from whichever tier hit — the inbound tier on an overlap:
	// a charge in another currency points at another
	// instruction's fee, so it is parked rather than converted.
	mt202Currency := mt202exp.Request.Currency
	if inMT202 {
		mt202Currency = mt202rec.Request.Currency
	}
	if req.Currency != mt202Currency {
		repairID, parkErr := l.parkMT290Advice(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return MT290ApplyResult{}, fmt.Errorf("%w: advice holds %q, MT202 holds %q (repair parking failed: %v)", ErrMT290CurrencyMismatch, req.Currency, mt202Currency, parkErr)
		}
		return MT290ApplyResult{Reference: req.Reference, MT202Ref: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: advice holds %q, MT202 holds %q", ErrMT290CurrencyMismatch, req.Currency, mt202Currency)
	}

	// The advice, from the parsed fields only: the amount the
	// :32B: amount, the details the :71B: text verbatim. There
	// is no value date to carry: :32B: has none.
	advice := MT202ChargeAdvice{
		MsgID:       req.Reference,
		MT202Ref:    req.RelatedReference,
		Currency:    req.Currency,
		AmountCents: req.AmountCents,
		Details:     req.Details,
		Actor:       actor,
		AppliedAt:   time.Now().UTC(),
	}
	// The audit event anchors the MT202's own legs, the same
	// pair its `mt202_inbound_applied` event anchors: the
	// charge was taken on that transfer. A failed MT202 leaves
	// its legs empty, and the event carries none — the advice
	// still records, against the instruction reference. Only
	// the inbound tier has legs: an outbound export touches
	// no ledger account, so its event carries none. The event
	// also names the hit tier in `anchored` (LG-484): "mt202"
	// for an applied inbound MT202, "outbound-mt202" for a
	// rendered outbound one — the MT295AnchorKind vocabulary.
	var accounts []AccountID
	anchored := "outbound-mt202"
	if inMT202 {
		anchored = "mt202"
		for _, leg := range []AccountID{mt202rec.Result.From, mt202rec.Result.To} {
			if leg != "" {
				accounts = append(accounts, leg)
			}
		}
	}

	// The :20: record, the MT202's advice list, and the
	// inbound marker event, under one short write lock: the
	// advice is already built when the marker lands. A
	// concurrent double-apply that landed first returns the
	// stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.mt290Applied[req.Reference]; ok {
		l.mu.Unlock()
		if !mt290TermsEqual(rec, req) {
			return MT290ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT290MsgIDConflict, req.Reference)
		}
		return MT290ApplyResult{Advice: rec.Advice, Reference: req.Reference, MT202Ref: rec.Advice.MT202Ref, Duplicate: true}, nil
	}
	if l.mt290Applied == nil {
		l.mt290Applied = make(map[string]mt290AppliedRecord)
	}
	l.mt290Applied[req.Reference] = mt290AppliedRecord{Advice: advice}
	if l.mt202Charges == nil {
		l.mt202Charges = make(map[string][]MT202ChargeAdvice)
	}
	l.mt202Charges[req.RelatedReference] = append(l.mt202Charges[req.RelatedReference], advice)
	l.emitAudit(AuditEvent{
		Op:            "mt290_charges_advised",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"msg_id":       req.Reference,
			"mt202_ref":    req.RelatedReference,
			"currency":     req.Currency,
			"amount_cents": req.AmountCents,
			"details":      req.Details,
			"anchored":     anchored,
		},
	})
	l.mu.Unlock()
	return MT290ApplyResult{Advice: advice, Reference: req.Reference, MT202Ref: req.RelatedReference}, nil
}

// GetMT202Charges returns the charge advices recorded against
// mt202Ref, in apply order, or false when none was ever recorded
// for it. The returned slice is a copy — callers cannot mutate
// ledger state through it.
func (l *Ledger) GetMT202Charges(mt202Ref string) ([]MT202ChargeAdvice, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	list, ok := l.mt202Charges[mt202Ref]
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]MT202ChargeAdvice, len(list))
	copy(out, list)
	return out, true
}
