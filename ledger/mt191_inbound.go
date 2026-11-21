package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT191 request for payment of charges inbound
// processing (LG-455): the inbound half of the MT191 lifecycle.
// There is no outbound half in this ledger — the ledger never
// asks a correspondent bank to pay it a charge; it records the
// requests correspondent banks send it. The request names an
// earlier payment and states what the correspondent wants to be
// paid for it; the answer is a record of ledger facts, never a
// payment.
//
// Field set, verified against the SWIFT MT191 (Request for
// Payment of Charges, Interest and Other Expenses) definition
// before this module was written (2026-11-21, Oracle SWIFT
// Messages guide §12.1.7 and §14.1.7, two editions agreeing):
//
//   - :20: Transaction Reference Number (16x, mandatory) — the
//     request's own reference, the MsgId and the replay key.
//   - :21: Related Reference (16x, mandatory) — the original
//     message's :20:, the payout ID, the anchor ParseMT192 uses
//     for :21:.
//   - :32B: Currency Code, Amount (3!a15d, mandatory) — the
//     amount requested. A request carries no value date: nothing
//     has been taken yet, so there is no value day to anchor —
//     :32A: (6!n3!a15d, the MT190 advice's dated charge) is
//     refused, never silently accepted with its date dimension
//     dropped.
//   - :71B: Details of Charges (6*35x, mandatory) — the
//     free-format detail of what the charge is for, carried
//     verbatim and never interpreted or translated. Mandatory
//     here, unlike the MT190 advice where it is optional: a
//     request that does not state what the charge is for is not
//     a bill an operator can decide on.
//
// Outside the core set, refused: :25: (account identification —
// the ledger knows the payout's account), :32a: (the dated
// options — a request has no value date), :52a: / :57a:
// (ordering / account-with institution — routing, not evidence),
// and :72: (sender-to-receiver information). A charge request
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
// ApplyMT191 resolves the :21: against the payouts and records
// the request under the payout in payoutChargeRequests, with one
// `mt191_charges_requested` audit event (details msg_id /
// payout_id / currency / amount_cents / status, plus the :71B:
// text verbatim) putting the :20: on the durable record. The
// recorded request carries status "pending": it sits in the
// operator's decision queue — the per-payout list of pending
// charge requests — and nothing pays it. An unknown :21: is
// fail-closed into the repair queue (RepairReasonUnknownRoute,
// the camt.055 shape: the parked result carries the repair ID
// alongside the ErrMT103PayoutNotFound error); a :32B: currency
// that disagrees with the payout's currency is likewise parked
// (RepairReasonInvalidFormat — the request is well-formed but
// disagrees with the ledger's fact about the payout), never
// silently converted. A non-positive amount is refused at parse
// — it is a malformed document, not a routable one — and at the
// apply gate for hand-built requests.
//
// Nothing moves: no journal entry, no payout status change, no
// gpi advance, and no payment of any kind. A charge request is
// a bill, not a debit instruction — the correspondent asks to be
// paid, the operator decides, and the payment itself (if any)
// is a separate instruction. The payout's own amount is
// untouched: the charge is the correspondent's fee, independent
// of the payout amount, and is never checked against it. The
// CBPR+ roadmap pairs MT191 with camt.106 (Charges Payment
// Request), the ISO 20022 sibling of this same ask.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// payout, currency, amount, and details returns the stored
// request with Duplicate set, recording nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT191MsgIDConflict — never silently
// overwritten. The registry check runs again under the write
// lock after the request is built, so two concurrent applies of
// the same :20: still record once.

// MT191 inbound errors.
var (
	// ErrMT191Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :32B: → :71B: order.
	ErrMT191Framing = errors.New("ledger: mt191: malformed FIN framing")
	// ErrMT191Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, or a
	// field outside the four-field core set (including :25:,
	// :32a:, :52a:, :57a:, and :72:).
	ErrMT191Field = errors.New("ledger: mt191: unexpected or missing field")
	// ErrMT191Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT191Reference = errors.New("ledger: mt191: invalid reference")
	// ErrMT191Currency is returned for an invalid :32B:
	// currency: not exactly three uppercase ASCII letters.
	ErrMT191Currency = errors.New("ledger: mt191: invalid :32B: currency")
	// ErrMT191Amount is returned for an invalid :32B: amount:
	// not `<integer>,<2 fraction digits>`, or not positive.
	ErrMT191Amount = errors.New("ledger: mt191: invalid :32B: amount")
	// ErrMT191LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT191LT = errors.New("ledger: mt191: invalid SWIFT LT address")
	// ErrMT191MsgIDConflict is returned by ApplyMT191 when a
	// :20: reference already applied carries different terms: the
	// reference names one charge request, and a second, different
	// request under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT191MsgIDConflict = errors.New("ledger: mt191 inbound: message reference already applied with different terms")
	// ErrMT191CurrencyMismatch is returned by ApplyMT191 when
	// the request's :32B: currency disagrees with the payout's
	// currency: the request points at another currency's charge,
	// so it is parked in the repair queue rather than converted.
	// It is a 422-class error.
	ErrMT191CurrencyMismatch = errors.New("ledger: mt191 inbound: :32B: currency does not match the payout's currency")
)

// PayoutChargeRequestStatusPending is the only status a
// recorded MT191 charge request carries: the request waits for
// an operator to decide whether the charge is owed. ApplyMT191
// never moves a request past pending — paying it is a separate
// instruction, never implied by the request itself.
const PayoutChargeRequestStatusPending = "pending"

// MT191 line caps: :71B: is 6*35x, each line 1–35 SWIFT `x`
// characters.
const (
	mt191MaxDetailsLines = 6
	mt191MaxDetailsLine  = 35
)

// MT191 is one SWIFT FIN MT191 request for payment of charges:
// the block-4 core fields plus the block 1/2 LT addresses.
// AmountCents is the :32B: amount in minor units, always
// positive; Details is the :71B: text joined with "\n". There
// is no value date: a request carries :32B:, not :32A:.
type MT191 struct {
	SenderLT         string
	ReceiverLT       string
	Reference        string // :20:, this message's own reference, the replay key.
	RelatedReference string // :21:, the original MT103's :20: — the payout ID.
	Currency         string // :32B:, three uppercase letters.
	AmountCents      int64  // :32B:, minor units, always positive.
	Details          string // :71B:, verbatim, joined with "\n".
}

// PayoutChargeRequest is one recorded MT191 charge request: the
// evidence a correspondent bank's request for payment leaves on
// a payout, waiting in the operator's decision queue. Status is
// always "pending" (see PayoutChargeRequestStatusPending): the
// request never moves funds by itself, so it carries no journal
// entry ID, and none is ever posted for it.
type PayoutChargeRequest struct {
	MsgID       string    `json:"msg_id"`
	PayoutID    string    `json:"payout_id"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	Details     string    `json:"details"`
	Status      string    `json:"status"`
	Actor       string    `json:"actor"`
	AppliedAt   time.Time `json:"applied_at"`
}

// MT191ApplyResult is the outcome of ApplyMT191: the recorded
// request for the charged payout. A parked request (unknown :21:,
// or a :32B: currency that disagrees with the payout's) carries
// only Reference, PayoutID, and RepairID, alongside the error.
type MT191ApplyResult struct {
	// Request is the recorded charge request. Empty on a parked
	// request.
	Request PayoutChargeRequest `json:"request"`
	// Reference echoes the request's :20: reference.
	Reference string `json:"reference"`
	// PayoutID is the charged payout (the :21:).
	PayoutID string `json:"payout_id"`
	// RepairID is the repair queue record for a parked request
	// (unknown :21: or currency mismatch). Empty on a recorded
	// request.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the request was already recorded
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt191AppliedRecord is the replay record for one applied MT191
// charge request, keyed by :20: in mt191Applied: the request as
// recorded, so a replay returns the same outcome without
// recording twice.
type mt191AppliedRecord struct {
	Request PayoutChargeRequest
}

// mt191TermsEqual reports whether a stored request record
// carries exactly the terms an inbound request would apply: the
// same payout, the same currency, the same amount, and the same
// :71B: details, verbatim. AppliedAt is deliberately excluded:
// two applies of the same terms at different times are the same
// request.
func mt191TermsEqual(rec mt191AppliedRecord, req MT191) bool {
	r := rec.Request
	return r.PayoutID == req.RelatedReference &&
		r.Currency == req.Currency &&
		r.AmountCents == req.AmountCents &&
		r.Details == req.Details
}

// mt191Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT191 error identity.
func mt191Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT191Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT191Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT191Reference, field, ref)
	}
	return nil
}

// mt191ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I191<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT191 error identity.
func mt191ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT191Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT191Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT191Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT191LT, lt)
	}
	return lt, nil
}

// mt191Currency validates a :32B: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT191
// error identity.
func mt191Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT191Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT191Currency, code)
		}
	}
	return nil
}

// mt191ParseAmount parses a :32B: amount body
// (`<integer>,<ff>`) into cents. The integer part holds at most
// 12 digits; the fraction part holds exactly two digits; the
// value must be positive. The MT202 rule, under the MT191 error
// identity.
func mt191ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT191Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT191Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT191Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT191Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT191Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT191Amount, body)
	}
	return cents, nil
}

// mt191ParseTextLines validates one multi-line text field: at
// most maxLines lines of 1–35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT195 narrative shape,
// under the MT191 field error identity.
func mt191ParseTextLines(lines []string, tag string, maxLines int) error {
	if len(lines) > maxLines {
		return fmt.Errorf("%w: %s: at most %d lines", ErrMT191Field, tag, maxLines)
	}
	for _, ln := range lines {
		if len(ln) == 0 || len(ln) > mt191MaxDetailsLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", ErrMT191Field, tag, mt191MaxDetailsLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: line: %v", ErrMT191Field, tag, err)
		}
	}
	return nil
}

// mt191ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The run stops at the trailer. The MT195 run shape, under the
// MT191 field error identity.
func mt191ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines)-1; i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT191Field, tag)
	}
	return run, i, nil
}

// ParseMT191 parses a SWIFT FIN MT191 request for payment of
// charges message into its business fields. Parsing is strict:
// the block structure, the field order, and every field value
// are validated, and anything outside the four-field core set
// (:20: / :21: / :32B: / :71B:) is refused — a charge request
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated.
func ParseMT191(text string) (MT191, error) {
	var zero MT191
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	// Eight lines is the floor for field parsing (3 headers +
	// {4: + 3 single-line fields + -}); a document that short
	// is missing its mandatory :71B:, which the field check
	// below refuses as a missing required field, not a framing
	// error.
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + 3 fields + -}), got %d", ErrMT191Framing, len(lines))
	}
	senderLT, err := mt191ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt191ParseLT(lines[1], "{2:I191", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT191Framing, mur)
	}
	if err := mt191Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT191Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT191Framing, lines[len(lines)-1])
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
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT191Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT191Field, want.tag)
		}
	}
	var m MT191
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt191Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.Reference = fields[0].body
	if err := mt191Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :32B: = 3-letter currency + <integer>,<2 digits>. No value
	// date: a request asks to be paid, nothing has been taken.
	body32 := fields[2].body
	if len(body32) < 3+4 {
		return zero, fmt.Errorf("%w: :32B: too short: %q", ErrMT191Framing, body32)
	}
	m.Currency = body32[:3]
	if err := mt191Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt191ParseAmount(body32[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	// :71B: is mandatory in MT191 (the SWIFT field table marks
	// it M): the first line carries the tag, the following bare
	// lines continue the details until the trailer. A line
	// starting with `:` there is an unknown field, not a
	// continuation.
	i := 7
	if i >= len(lines)-1 || !strings.HasPrefix(lines[i], ":71B:") {
		return zero, fmt.Errorf("%w: line %d: want :71B: field, got %q", ErrMT191Field, i+1, lines[i])
	}
	run, next, err := mt191ParseTextRun(lines, i, ":71B:")
	if err != nil {
		return zero, err
	}
	if err := mt191ParseTextLines(run, ":71B:", mt191MaxDetailsLines); err != nil {
		return zero, err
	}
	m.Details = strings.Join(run, "\n")
	i = next
	// Nothing but the trailer may follow.
	if i != len(lines)-1 {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT191Field, i+1, lines[i])
	}
	return m, nil
}

// validateMT191Request fail-closes on a charge request
// ParseMT191 would refuse, so ApplyMT191 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT191Request(req MT191) error {
	if err := mt191Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt191Reference(req.RelatedReference, ":21:"); err != nil {
		return err
	}
	if err := mt191Currency(req.Currency); err != nil {
		return err
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: %d is not positive", ErrMT191Amount, req.AmountCents)
	}
	if req.Details == "" {
		return fmt.Errorf("%w: :71B: field is empty", ErrMT191Field)
	}
	if err := mt191ParseTextLines(strings.Split(req.Details, "\n"), ":71B:", mt191MaxDetailsLines); err != nil {
		return err
	}
	return nil
}

// parkMT191Request parks a fail-closed parsed request in the
// repair queue and returns the record ID. The snapshot names
// the charged payout (:21:) as the instruction ID and the request
// (:20:) as the destination, so an operator can find the request
// from the record. The snapshot's Maker is deliberately empty
// (see the package comment): the record is operator visibility,
// never a resubmittable instruction. A parking failure (a full
// queue) is returned so the caller can fail loud rather than
// silently drop the bank's message.
func (l *Ledger) parkMT191Request(req MT191, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.RelatedReference,
		Destination: "mt191-inbound:" + req.Reference,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT191 applies one parsed inbound SWIFT FIN MT191 request
// for payment of charges: it records the correspondent bank's
// charge request against the payout the :21: names, in
// payoutChargeRequests, with one `mt191_charges_requested` audit
// event. The recorded request is pending: it waits in the
// operator's decision queue, and nothing pays it — an inbound
// request is a bill, not a payment instruction.
//
// The payout must exist (:21: unknown is
// ErrMT103PayoutNotFound, the export's error), parked in the
// repair queue fail-closed (the camt.055 shape: the returned
// result carries the repair ID alongside the error); the :32B:
// currency must agree with the payout's currency (a mismatch is
// parked the same way, with ErrMT191CurrencyMismatch — never
// silently converted). No journal entry is ever posted, no
// payout state is ever changed, and no gpi track is ever
// advanced: a request is evidence plus a pending decision, not a
// debit instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same payout, currency, amount,
// and details returns the stored request with Duplicate set,
// recording nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT191MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the payout read run under the read lock; the
// :20: record, the payout's request list, and the inbound marker
// event run under a short write lock of their own, re-checking
// the registry first so a concurrent double-apply still records
// once.
func (l *Ledger) ApplyMT191(req MT191, actor string) (MT191ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT191"
	}
	if err := validateMT191Request(req); err != nil {
		return MT191ApplyResult{}, err
	}

	// :20: replay: the request was already recorded under this
	// reference. Same terms is a no-op returning the stored
	// request; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt191Applied[req.Reference]; ok {
		l.mu.RUnlock()
		if !mt191TermsEqual(rec, req) {
			return MT191ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT191MsgIDConflict, req.Reference)
		}
		return MT191ApplyResult{Request: rec.Request, Reference: req.Reference, PayoutID: rec.Request.PayoutID, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	// The payout itself: :21: names it, the original MT103's
	// :20:. Unknown is parked fail-closed in the repair queue
	// (the camt.055 shape) and reported as the export's error.
	l.mu.RLock()
	p, ok := l.payouts[req.RelatedReference]
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT191Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return MT191ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT103PayoutNotFound, req.RelatedReference, parkErr)
		}
		return MT191ApplyResult{Reference: req.Reference, PayoutID: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, req.RelatedReference)
	}

	// The :32B: currency must be the payout's currency: a
	// charge in another currency points at another instruction's
	// fee, so it is parked rather than converted.
	if req.Currency != p.Currency {
		repairID, parkErr := l.parkMT191Request(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return MT191ApplyResult{}, fmt.Errorf("%w: request holds %q, payout holds %q (repair parking failed: %v)", ErrMT191CurrencyMismatch, req.Currency, p.Currency, parkErr)
		}
		return MT191ApplyResult{Reference: req.Reference, PayoutID: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: request holds %q, payout holds %q", ErrMT191CurrencyMismatch, req.Currency, p.Currency)
	}

	// The request, from the parsed fields only: the amount the
	// :32B: amount, the details the :71B: text verbatim, the
	// status pending — an operator decides, this ledger never
	// pays on a request alone.
	request := PayoutChargeRequest{
		MsgID:       req.Reference,
		PayoutID:    req.RelatedReference,
		Currency:    req.Currency,
		AmountCents: req.AmountCents,
		Details:     req.Details,
		Status:      PayoutChargeRequestStatusPending,
		Actor:       actor,
		AppliedAt:   time.Now().UTC(),
	}

	// The :20: record, the payout's request list, and the
	// inbound marker event, under one short write lock: the
	// request is already built when the marker lands. A
	// concurrent double-apply that landed first returns the
	// stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.mt191Applied[req.Reference]; ok {
		l.mu.Unlock()
		if !mt191TermsEqual(rec, req) {
			return MT191ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT191MsgIDConflict, req.Reference)
		}
		return MT191ApplyResult{Request: rec.Request, Reference: req.Reference, PayoutID: rec.Request.PayoutID, Duplicate: true}, nil
	}
	if l.mt191Applied == nil {
		l.mt191Applied = make(map[string]mt191AppliedRecord)
	}
	l.mt191Applied[req.Reference] = mt191AppliedRecord{Request: request}
	if l.payoutChargeRequests == nil {
		l.payoutChargeRequests = make(map[string][]PayoutChargeRequest)
	}
	l.payoutChargeRequests[req.RelatedReference] = append(l.payoutChargeRequests[req.RelatedReference], request)
	l.emitAudit(AuditEvent{
		Op:            "mt191_charges_requested",
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
			"details":      req.Details,
			"status":       PayoutChargeRequestStatusPending,
		},
	})
	l.mu.Unlock()
	return MT191ApplyResult{Request: request, Reference: req.Reference, PayoutID: req.RelatedReference}, nil
}

// GetPayoutChargeRequests returns the charge requests recorded
// against payoutID, in apply order, or false when none was ever
// recorded for it. Every returned request is pending an operator
// decision — the list is the operator's decision queue for that
// payout. The returned slice is a copy — callers cannot mutate
// ledger state through it.
func (l *Ledger) GetPayoutChargeRequests(payoutID string) ([]PayoutChargeRequest, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	list, ok := l.payoutChargeRequests[payoutID]
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]PayoutChargeRequest, len(list))
	copy(out, list)
	return out, true
}
