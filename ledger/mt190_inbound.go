package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT190 advice of charges inbound processing (LG-454):
// the inbound half of the MT190 lifecycle. There is no outbound
// half in this ledger — the ledger never advises a correspondent
// bank of charges it took; it records the advices correspondent
// banks send it. The advice names an earlier payment and states
// what the correspondent charged for it; the answer is a record
// of ledger facts, never a new instruction.
//
// Field set, verified against the SWIFT MT190 (Advice of Charges,
// Interest and Other Adjustments) definition before this module
// was written (2026-11-21):
//
//   - :20: Transaction Reference Number (16x, mandatory) — the
//     advice's own reference, the MsgId and the replay key.
//   - :21: Related Reference (16x, mandatory) — the original
//     message's :20:, the payout ID, the anchor ParseMT192 uses
//     for :21:.
//   - :32A: Value Date, Currency Code, Amount (6!n3!a15d,
//     mandatory in this ledger's core set) — the dated charge.
//     Oracle FLEXCUBE's Advice of Charges table lists the amount
//     field as "Value Date, Currency, Amount (32)", the
//     value-date-carrying option; the SWIFT field list for MT190
//     (Payments Signal, citing the SWIFT Standards MT category
//     1 message list) carries both :32a: and :32B:. This ledger
//     accepts only :32A:: a charge advice without a value date
//     cannot be anchored to a value day, and the evidence record
//     needs the date — :32B: (3!a15d, currency and amount only)
//     is refused as outside the core set, never silently
//     accepted with its date dimension dropped.
//   - :71B: Details of Charges (6*35x, optional) — the free-format
//     detail of what the charge is for, carried verbatim and
//     never interpreted or translated (Payments Signal: ":71B:,
//     the details of charges"; Oracle FLEXCUBE lists Details of
//     Charges (71B) in the Advice of Charges table).
//
// Outside the core set, refused: :25: (account identification —
// the ledger knows the payout's account), :52a: / :57a:
// (ordering / account-with institution — routing, not evidence),
// :72: (sender-to-receiver information), and :32B: (see above).
// A charge advice must not lose content quietly.
//
// Parsing is strict, along ParseMT192 (LG-438): the block
// structure, the field order (:20: → :21: → :32A: → :71B:), and
// every field value are validated, and anything outside the
// four-field core set is refused. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated. A bare
// line starting with `:` inside the :71B: run is an unknown
// field, not a continuation.
//
// ApplyMT190 resolves the :21: against the payouts and records
// the advice under the payout in payoutCharges, with one
// `mt190_charges_advised` audit event (details msg_id /
// payout_id / currency / amount_cents / value_date, plus the
// :71B: text verbatim when present) putting the :20: on the
// durable record. An unknown :21: is fail-closed into the repair
// queue (RepairReasonUnknownRoute, the camt.055 shape: the parked
// result carries the repair ID alongside the
// ErrMT103PayoutNotFound error); a :32A: currency that disagrees
// with the payout's currency is likewise parked
// (RepairReasonInvalidFormat — the advice is well-formed but
// disagrees with the ledger's fact about the payout), never
// silently converted. A non-positive amount is refused at parse
// — it is a malformed document, not a routable one — and at the
// apply gate for hand-built requests.
//
// Nothing moves: no journal entry, no payout status change, no
// gpi advance. A charge advice is evidence, not a debit
// instruction — the debit itself is evidenced separately by
// camt.054 / the bank statement, and the payout's own amount is
// untouched: the charge is the correspondent's fee, independent
// of the payout amount, and is never checked against it.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// payout, value date, currency, amount, and details returns the
// stored advice with Duplicate set, recording nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT190MsgIDConflict — never silently
// overwritten. The registry check runs again under the write
// lock after the advice is built, so two concurrent applies of
// the same :20: still record once.

// MT190 inbound errors.
var (
	// ErrMT190Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :32A: → :71B: order.
	ErrMT190Framing = errors.New("ledger: mt190: malformed FIN framing")
	// ErrMT190Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, or a
	// field outside the four-field core set (including :25:,
	// :32B:, :52a:, :57a:, and :72:).
	ErrMT190Field = errors.New("ledger: mt190: unexpected or missing field")
	// ErrMT190Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT190Reference = errors.New("ledger: mt190: invalid reference")
	// ErrMT190ValueDate is returned for an invalid :32A: value
	// date: not a YYMMDD calendar date.
	ErrMT190ValueDate = errors.New("ledger: mt190: invalid :32A: value date")
	// ErrMT190Currency is returned for an invalid :32A:
	// currency: not exactly three uppercase ASCII letters.
	ErrMT190Currency = errors.New("ledger: mt190: invalid :32A: currency")
	// ErrMT190Amount is returned for an invalid :32A: amount:
	// not `<integer>,<2 fraction digits>`, or not positive.
	ErrMT190Amount = errors.New("ledger: mt190: invalid :32A: amount")
	// ErrMT190LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT190LT = errors.New("ledger: mt190: invalid SWIFT LT address")
	// ErrMT190MsgIDConflict is returned by ApplyMT190 when a
	// :20: reference already applied carries different terms: the
	// reference names one charge advice, and a second, different
	// advice under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT190MsgIDConflict = errors.New("ledger: mt190 inbound: message reference already applied with different terms")
	// ErrMT190CurrencyMismatch is returned by ApplyMT190 when
	// the advice's :32A: currency disagrees with the payout's
	// currency: the advice points at another currency's charge,
	// so it is parked in the repair queue rather than converted.
	// It is a 422-class error.
	ErrMT190CurrencyMismatch = errors.New("ledger: mt190 inbound: :32A: currency does not match the payout's currency")
)

// MT190 line caps: :71B: is 6*35x, each line 1–35 SWIFT `x`
// characters.
const (
	mt190MaxDetailsLines = 6
	mt190MaxDetailsLine  = 35
)

// MT190 is one SWIFT FIN MT190 advice of charges: the block-4
// core fields plus the block 1/2 LT addresses. ValueDate is a
// UTC calendar date (time-of-day is not carried by :32A:);
// AmountCents is the :32A: amount in minor units, always
// positive; Details is the :71B: text joined with "\n", empty
// when the message carries no :71B: block.
type MT190 struct {
	SenderLT         string
	ReceiverLT       string
	Reference        string // :20:, this message's own reference, the replay key.
	RelatedReference string // :21:, the original MT103's :20: — the payout ID.
	ValueDate        time.Time
	Currency         string // :32A:, three uppercase letters.
	AmountCents      int64  // :32A:, minor units, always positive.
	Details          string // :71B:, verbatim, joined with "\n".
}

// PayoutChargeAdvice is one recorded MT190 charge advice: the
// evidence a correspondent bank's fee advice leaves on a payout.
// ValueDate is the :32A: value date as YYMMDD; Details is the
// :71B: text verbatim; AppliedAt is when this ledger recorded
// it. The advice never moves funds: it carries no journal entry
// ID, and none is ever posted for it.
type PayoutChargeAdvice struct {
	MsgID       string    `json:"msg_id"`
	PayoutID    string    `json:"payout_id"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	ValueDate   string    `json:"value_date"`
	Details     string    `json:"details,omitempty"`
	Actor       string    `json:"actor"`
	AppliedAt   time.Time `json:"applied_at"`
}

// MT190ApplyResult is the outcome of ApplyMT190: the recorded
// advice for the charged payout. A parked advice (unknown :21:,
// or a :32A: currency that disagrees with the payout's) carries
// only Reference, PayoutID, and RepairID, alongside the error.
type MT190ApplyResult struct {
	// Advice is the recorded charge advice. Empty on a parked
	// advice.
	Advice PayoutChargeAdvice `json:"advice"`
	// Reference echoes the advice's :20: reference.
	Reference string `json:"reference"`
	// PayoutID is the charged payout (the :21:).
	PayoutID string `json:"payout_id"`
	// RepairID is the repair queue record for a parked advice
	// (unknown :21: or currency mismatch). Empty on a recorded
	// advice.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the advice was already recorded
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt190AppliedRecord is the replay record for one applied MT190
// charge advice, keyed by :20: in mt190Applied: the advice as
// recorded, so a replay returns the same outcome without
// recording twice.
type mt190AppliedRecord struct {
	Advice PayoutChargeAdvice
}

// mt190TermsEqual reports whether a stored advice record
// carries exactly the terms an inbound advice would apply: the
// same payout, the same value date (by calendar day, as :32A:
// carries only a day), the same currency, the same amount, and
// the same :71B: details, verbatim. AppliedAt is deliberately
// excluded: two applies of the same terms at different times
// are the same advice.
func mt190TermsEqual(rec mt190AppliedRecord, req MT190) bool {
	a := rec.Advice
	return a.PayoutID == req.RelatedReference &&
		a.ValueDate == mt103ValueDate(req.ValueDate) &&
		a.Currency == req.Currency &&
		a.AmountCents == req.AmountCents &&
		a.Details == req.Details
}

// mt190Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT190 error identity.
func mt190Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT190Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT190Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT190Reference, field, ref)
	}
	return nil
}

// mt190ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I190<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT190 error identity.
func mt190ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT190Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT190Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT190Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT190LT, lt)
	}
	return lt, nil
}

// mt190Currency validates a :32A: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT190
// error identity.
func mt190Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT190Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT190Currency, code)
		}
	}
	return nil
}

// mt190ParseAmount parses a :32A: amount body
// (`<integer>,<ff>`) into cents. The integer part holds at most
// 12 digits; the fraction part holds exactly two digits; the
// value must be positive. The MT202 rule, under the MT190 error
// identity.
func mt190ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT190Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT190Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT190Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT190Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT190Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT190Amount, body)
	}
	return cents, nil
}

// mt190ParseTextLines validates one multi-line text field: at
// most maxLines lines of 1–35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT195 narrative shape,
// under the MT190 field error identity.
func mt190ParseTextLines(lines []string, tag string, maxLines int) error {
	if len(lines) > maxLines {
		return fmt.Errorf("%w: %s: at most %d lines", ErrMT190Field, tag, maxLines)
	}
	for _, ln := range lines {
		if len(ln) == 0 || len(ln) > mt190MaxDetailsLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", ErrMT190Field, tag, mt190MaxDetailsLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: line: %v", ErrMT190Field, tag, err)
		}
	}
	return nil
}

// mt190ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The run stops at the trailer. The MT195 run shape, under the
// MT190 field error identity.
func mt190ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines)-1; i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT190Field, tag)
	}
	return run, i, nil
}

// ParseMT190 parses a SWIFT FIN MT190 advice of charges message
// into its business fields. Parsing is strict: the block
// structure, the field order, and every field value are
// validated, and anything outside the four-field core set (:20: /
// :21: / :32A: / :71B:) is refused — a charge advice must not
// lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated.
func ParseMT190(text string) (MT190, error) {
	var zero MT190
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + 3 fields + -}), got %d", ErrMT190Framing, len(lines))
	}
	senderLT, err := mt190ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt190ParseLT(lines[1], "{2:I190", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT190Framing, mur)
	}
	if err := mt190Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT190Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT190Framing, lines[len(lines)-1])
	}
	// Block 4, canonical order, single-line fields for :20: /
	// :21: / :32A:: a field body never spans lines in this core
	// set, so any line not starting with its expected tag is a
	// framing error, not a continuation.
	fields := []struct {
		tag, body string
	}{
		{":20:", ""}, {":21:", ""}, {":32A:", ""},
	}
	for i, want := range fields {
		line := lines[4+i]
		if !strings.HasPrefix(line, want.tag) {
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT190Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT190Field, want.tag)
		}
	}
	var m MT190
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt190Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.Reference = fields[0].body
	if err := mt190Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :32A: = YYMMDD + 3-letter currency + <integer>,<2 digits>.
	body32 := fields[2].body
	if len(body32) < 6+3+4 {
		return zero, fmt.Errorf("%w: :32A: too short: %q", ErrMT190Framing, body32)
	}
	datePart, rest := body32[:6], body32[6:]
	vd, err := time.Parse("060102", datePart)
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT190ValueDate, datePart, err)
	}
	m.ValueDate = vd.UTC()
	if len(rest) < 3 {
		return zero, fmt.Errorf("%w: :32A: missing currency: %q", ErrMT190Framing, body32)
	}
	m.Currency = rest[:3]
	if err := mt190Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt190ParseAmount(rest[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	// :71B:, when present: the first line carries the tag, the
	// following bare lines continue the details until the
	// trailer. A line starting with `:` there is an unknown
	// field, not a continuation.
	i := 7
	if i < len(lines)-1 && strings.HasPrefix(lines[i], ":71B:") {
		run, next, err := mt190ParseTextRun(lines, i, ":71B:")
		if err != nil {
			return zero, err
		}
		if err := mt190ParseTextLines(run, ":71B:", mt190MaxDetailsLines); err != nil {
			return zero, err
		}
		m.Details = strings.Join(run, "\n")
		i = next
	}
	// Nothing but the trailer may follow.
	if i != len(lines)-1 {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT190Field, i+1, lines[i])
	}
	return m, nil
}

// validateMT190Request fail-closes on a charge advice
// ParseMT190 would refuse, so ApplyMT190 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT190Request(req MT190) error {
	if err := mt190Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt190Reference(req.RelatedReference, ":21:"); err != nil {
		return err
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: value date is required", ErrMT190ValueDate)
	}
	if err := mt190Currency(req.Currency); err != nil {
		return err
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: %d is not positive", ErrMT190Amount, req.AmountCents)
	}
	if req.Details != "" {
		if err := mt190ParseTextLines(strings.Split(req.Details, "\n"), ":71B:", mt190MaxDetailsLines); err != nil {
			return err
		}
	}
	return nil
}

// parkMT190Advice parks a fail-closed parsed advice in the
// repair queue and returns the record ID. The snapshot names
// the charged payout (:21:) as the instruction ID and the advice
// (:20:) as the destination, so an operator can find the advice
// from the record. The snapshot's Maker is deliberately empty
// (see the package comment): the record is operator visibility,
// never a resubmittable instruction. A parking failure (a full
// queue) is returned so the caller can fail loud rather than
// silently drop the bank's message.
func (l *Ledger) parkMT190Advice(req MT190, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.RelatedReference,
		Destination: "mt190-inbound:" + req.Reference,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT190 applies one parsed inbound SWIFT FIN MT190 advice
// of charges: it records the correspondent bank's fee advice
// against the payout the :21: names, in payoutCharges, with one
// `mt190_charges_advised` audit event.
//
// The payout must exist (:21: unknown is
// ErrMT103PayoutNotFound, the export's error), parked in the
// repair queue fail-closed (the camt.055 shape: the returned
// result carries the repair ID alongside the error); the :32A:
// currency must agree with the payout's currency (a mismatch is
// parked the same way, with ErrMT190CurrencyMismatch — never
// silently converted). No journal entry is ever posted, no
// payout state is ever changed, and no gpi track is ever
// advanced: an advice is evidence, not a debit instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same payout, value date,
// currency, amount, and details returns the stored advice with
// Duplicate set, recording nothing twice and emitting no second
// audit event; a :20: reused for different terms is refused
// with ErrMT190MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the payout read run under the read lock; the
// :20: record, the payout's advice list, and the inbound marker
// event run under a short write lock of their own, re-checking
// the registry first so a concurrent double-apply still records
// once.
func (l *Ledger) ApplyMT190(req MT190, actor string) (MT190ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT190"
	}
	if err := validateMT190Request(req); err != nil {
		return MT190ApplyResult{}, err
	}

	// :20: replay: the advice was already recorded under this
	// reference. Same terms is a no-op returning the stored
	// advice; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt190Applied[req.Reference]; ok {
		l.mu.RUnlock()
		if !mt190TermsEqual(rec, req) {
			return MT190ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT190MsgIDConflict, req.Reference)
		}
		return MT190ApplyResult{Advice: rec.Advice, Reference: req.Reference, PayoutID: rec.Advice.PayoutID, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The payout itself: :21: names it, the original MT103's
	// :20:. Unknown is parked fail-closed in the repair queue
	// (the camt.055 shape) and reported as the export's error.
	l.mu.RLock()
	p, ok := l.payouts[req.RelatedReference]
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT190Advice(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return MT190ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT103PayoutNotFound, req.RelatedReference, parkErr)
		}
		return MT190ApplyResult{Reference: req.Reference, PayoutID: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, req.RelatedReference)
	}

	// The :32A: currency must be the payout's currency: a
	// charge in another currency points at another instruction's
	// fee, so it is parked rather than converted.
	if req.Currency != p.Currency {
		repairID, parkErr := l.parkMT190Advice(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return MT190ApplyResult{}, fmt.Errorf("%w: advice holds %q, payout holds %q (repair parking failed: %v)", ErrMT190CurrencyMismatch, req.Currency, p.Currency, parkErr)
		}
		return MT190ApplyResult{Reference: req.Reference, PayoutID: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: advice holds %q, payout holds %q", ErrMT190CurrencyMismatch, req.Currency, p.Currency)
	}

	// The advice, from the parsed fields only: the value date
	// is the :32A: day, the amount the :32A: amount, the
	// details the :71B: text verbatim.
	advice := PayoutChargeAdvice{
		MsgID:       req.Reference,
		PayoutID:    req.RelatedReference,
		Currency:    req.Currency,
		AmountCents: req.AmountCents,
		ValueDate:   mt103ValueDate(req.ValueDate),
		Details:     req.Details,
		Actor:       actor,
		AppliedAt:   time.Now().UTC(),
	}

	// The :20: record, the payout's advice list, and the
	// inbound marker event, under one short write lock: the
	// advice is already built when the marker lands. A
	// concurrent double-apply that landed first returns the
	// stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.mt190Applied[req.Reference]; ok {
		l.mu.Unlock()
		if !mt190TermsEqual(rec, req) {
			return MT190ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT190MsgIDConflict, req.Reference)
		}
		return MT190ApplyResult{Advice: rec.Advice, Reference: req.Reference, PayoutID: rec.Advice.PayoutID, Duplicate: true}, nil
	}
	if l.mt190Applied == nil {
		l.mt190Applied = make(map[string]mt190AppliedRecord)
	}
	l.mt190Applied[req.Reference] = mt190AppliedRecord{Advice: advice}
	if l.payoutCharges == nil {
		l.payoutCharges = make(map[string][]PayoutChargeAdvice)
	}
	l.payoutCharges[req.RelatedReference] = append(l.payoutCharges[req.RelatedReference], advice)
	l.emitAudit(AuditEvent{
		Op:            "mt190_charges_advised",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"msg_id":       req.Reference,
			"payout_id":    req.RelatedReference,
			"currency":     req.Currency,
			"amount_cents": req.AmountCents,
			"value_date":   advice.ValueDate,
			"details":      req.Details,
		},
	})
	l.mu.Unlock()
	return MT190ApplyResult{Advice: advice, Reference: req.Reference, PayoutID: req.RelatedReference}, nil
}

// GetPayoutCharges returns the charge advices recorded against
// payoutID, in apply order, or false when none was ever recorded
// for it. The returned slice is a copy — callers cannot mutate
// ledger state through it.
func (l *Ledger) GetPayoutCharges(payoutID string) ([]PayoutChargeAdvice, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	list, ok := l.payoutCharges[payoutID]
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]PayoutChargeAdvice, len(list))
	copy(out, list)
	return out, true
}
