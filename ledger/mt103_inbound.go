package ledger

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT103 single customer credit transfer INBOUND
// processing (LG-441): the inbound half of the MT103 lifecycle,
// the mirror of the LG-183 export.
//
// ExportMT103 renders this ledger's own approved payout as the
// instruction the ledger's bank hands to its correspondent — the
// ledger telling the correspondent "pay this customer". This
// module consumes the instruction a correspondent sent to this
// ledger, acting as the beneficiary's bank: it books the
// instructed credit transfer through PostTransfer, and nothing
// else. The two directions are orthogonal by construction:
//
//   - LG-183 exports: the ledger renders its own approved or
//     submitted payout for the correspondent (outbound,
//     read-only).
//   - LG-441 imports: a correspondent file asks the ledger to
//     book a customer credit transfer (inbound, state-changing:
//     one journal entry per instruction, plus the :20: replay
//     record).
//
// ParseMT103 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.001 inbound convention, LG-430).
// Parsing is strict, along ParseMT202 (LG-204): the block
// structure, the field order, and every field value are
// validated, and anything outside the nine-field core set
// (:20: :23B: :32A: :50K: :52A: :57A: :59: :70: :71A:) is
// refused — a payment instruction must not lose content
// quietly. CRLF line endings are accepted; a trailing newline
// after the `-}` trailer is tolerated. Fields outside blocks
// (before `{4:`, after `-}`) are refused, as is an empty line
// inside block 4.
//
// The wire fields, and what they route to:
//
//   - :20: is the sender's reference: the audit trace and the
//     replay key — one :20: names one instruction. It is also
//     the journal entry ID and the idempotency key's tail
//     ("mt103-"+:20:), so the correspondent can dedupe on it
//     exactly like an exported payout.
//   - :23B: is the bank operation code (CRED/SPAY/SSTD/SPRI,
//     the export's vocabulary, mt103.go); it is carried and
//     validated, and plays no routing role.
//   - :32A: is the value date, currency, and amount. The value
//     date must be today's UTC date (the LG-430 pain.001 rule):
//     the ledger has no future-date (or past-date) execution
//     scheduling, so an instruction dated any other day parks
//     in the repair queue instead of inventing a holding
//     position. The amount must be positive (the :32A: 15d
//     rule).
//   - :50K: is the ordering customer as "/<IBAN>" plus up to 3
//     name/address lines; :59: is the beneficiary the same
//     way. Both account lines must be registered IBANs,
//     resolved by the reverse-lookup helper
//     (accountByIBANLocked, over the registered IBAN table):
//     an IBAN no account registered parks the instruction in
//     the repair queue — the ledger never opens an account from
//     a wire identifier (the pain.001 convention, LG-430). A
//     :50K:/:59: account line that is not a well-formed IBAN
//     at all is a parse refusal, the MT101 convention (LG-439):
//     the field names an IBAN, so a non-IBAN value is a
//     malformed message, not an unregistered one.
//   - :52A: (ordering institution BIC) and :57A: (beneficiary
//     institution BIC) are optional, and are recorded in the
//     audit details only — never used for routing. The ledger
//     keeps no routing table (the mt103.go comment), so the
//     BICs name the correspondent path the instruction arrived
//     on, not a leg the ledger picks.
//   - :70: is the remittance information, up to 4 lines of 35
//     `x` characters, carried verbatim.
//   - :71A: is the details of charges (OUR/BEN/SHA, the
//     export's vocabulary); it is carried and validated, and
//     plays no routing role.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// instruction that fails any apply gate parks in the repair
// queue for operator resolution — an unregistered IBAN, a
// value date that is not today, or any PostTransfer refusal
// (same-party, frozen/overdraft, daily limit, accounting
// period, dual-control review). Repair records parked by this
// module are visibility parking, not resubmittable
// instructions: their Maker is deliberately empty, so the
// CreatePayout gate can never pass them.
//
// Idempotency is the :20: with its terms (the pain.011
// convention, LG-426): re-applying the same :20: with the same
// terms returns the stored outcome with Duplicate set, booking
// nothing new and emitting no second audit event. A :20:
// reused for different terms is refused with
// ErrMT103MsgIDConflict — never silently overwritten. The
// per-instruction idempotency key is a second layer: even if
// the in-memory replay record is lost (a restart),
// PostTransfer replays the original receipt instead of booking
// twice.
//
// No MT103 export change; no PostTransfer semantic change; no
// automatic account opening; no nostro credit (:52A:/:57A: are
// recorded, not routed); no future-date scheduling; no MT192
// cancellation handling.

// MT103 inbound errors.
var (
	// ErrMT103Framing is returned when the FIN block structure
	// is broken: a missing block, a malformed block 1/2/3
	// header, a missing or unterminated block 4, an empty line
	// inside block 4, or a field out of the canonical :20: →
	// :23B: → :32A: → :50K: → :52A: → :57A: → :59: → :70: →
	// :71A: order.
	ErrMT103Framing = errors.New("ledger: mt103 inbound: malformed FIN framing")
	// ErrMT103Field is returned for a block-4 field problem
	// that is not a value problem: a missing required field, a
	// field outside the nine-field core set, a malformed
	// :50K:/:59: account line, or too many name/remittance
	// lines.
	ErrMT103Field = errors.New("ledger: mt103 inbound: unexpected or missing field")
	// ErrMT103ValueDate is returned for an invalid :32A: value
	// date: not a YYMMDD calendar date.
	ErrMT103ValueDate = errors.New("ledger: mt103 inbound: invalid :32A: value date")
	// ErrMT103Currency is returned for an invalid :32A:
	// currency: not exactly three uppercase ASCII letters (the
	// LG-18 currency rule — no lowercase, no digits, no ISO
	// lookup table the ledger does not own).
	ErrMT103Currency = errors.New("ledger: mt103 inbound: invalid :32A: currency")
	// ErrMT103ChargeBearer <redacted> returned for an invalid :71A: details
	// of charges: anything outside OUR/BEN/SHA (the export's
	// vocabulary, mt103DetailsOfCharges).
	ErrMT103ChargeBearer = errors.New("ledger: mt103 inbound: invalid :71A: details of charges")
	// ErrMT103MsgIDConflict is returned by ApplyMT103 when a
	// :20: reference already applied carries different terms:
	// the reference names one instruction, and a second,
	// different instruction under it is refused rather than
	// silently overwritten (the pain.011
	// ErrPain011MsgIDConflict convention). It is a 422-class
	// error.
	ErrMT103MsgIDConflict = errors.New("ledger: mt103 inbound: message reference already applied with different terms")
	// ErrMT103UnknownIBAN is returned per instruction when a
	// :50K: or :59: IBAN names no account the ledger
	// registered an IBAN for: the ledger never opens an account
	// from a wire identifier — the instruction parks in the
	// repair queue.
	ErrMT103UnknownIBAN = errors.New("ledger: mt103 inbound: IBAN names no registered account")
	// ErrMT103ValueDateNotToday is returned per instruction
	// when the :32A: value date is not today's UTC date: the
	// ledger has no future-date (or past-date) execution
	// scheduling, so the instruction parks in the repair queue.
	ErrMT103ValueDateNotToday = errors.New("ledger: mt103 inbound: value date is not today (UTC)")
)

// MT103Inbound is one parsed inbound SWIFT FIN MT103 single
// customer credit transfer: the block-4 core fields plus the
// block 1/2 LT addresses. ValueDate is a UTC calendar date
// (time-of-day is not carried by :32A:); AmountCents is the
// :32A: amount in minor units, always positive.
type MT103Inbound struct {
	SenderLT   string
	ReceiverLT string
	// Reference is :20:, this message's own reference, the
	// replay key — the journal entry ID and the idempotency
	// key's tail ("mt103-"+:20:).
	Reference string `json:"msg_id"`
	// BankOperationCode is :23B: (CRED/SPAY/SSTD/SPRI).
	BankOperationCode string `json:"bank_operation_code"`
	// ValueDate is :32A:, the value date, which must be
	// today's UTC date; Currency and AmountCents are the
	// :32A: currency and amount.
	ValueDate   time.Time `json:"value_date"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	// OrderingAccount is :50K:, the ordering customer's
	// IBAN, normalized; OrderingName is its name/address
	// lines (up to 3).
	OrderingAccount string   `json:"ordering_account"`
	OrderingName    []string `json:"ordering_name,omitempty"`
	// OrderingInstitutionBIC is :52A: when the message
	// carries it, recorded in the audit details only, never
	// used for routing.
	OrderingInstitutionBIC string `json:"ordering_institution_bic,omitempty"`
	// BeneficiaryInstitutionBIC is :57A: when the message
	// carries it, recorded in the audit details only, never
	// used for routing.
	BeneficiaryInstitutionBIC string `json:"beneficiary_institution_bic,omitempty"`
	// BeneficiaryAccount is :59:, the beneficiary IBAN,
	// normalized; BeneficiaryName is its name/address lines
	// (up to 3).
	BeneficiaryAccount string   `json:"beneficiary_account"`
	BeneficiaryName    []string `json:"beneficiary_name,omitempty"`
	// RemittanceInfo is :70:, newline-joined, up to 4 lines.
	RemittanceInfo string `json:"remittance_info,omitempty"`
	// DetailsOfCharges is :71A: (OUR/BEN/SHA).
	DetailsOfCharges string `json:"details_of_charges"`
}

// MT103ApplyResult is the outcome of ApplyMT103.
type MT103ApplyResult struct {
	// MsgID is the :20: reference of the applied instruction.
	MsgID string `json:"msg_id"`
	// From and To are the ledger accounts the wire IBANs
	// resolved to, empty when the instruction was rejected
	// before the lookup (or the lookup itself failed).
	From AccountID `json:"from_account,omitempty"`
	To   AccountID `json:"to_account,omitempty"`
	// Receipt is the PostTransfer receipt on success or
	// replay; empty when rejected.
	Receipt TransferReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the instruction's
	// :20:), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same
	// instruction was applied before, so the earlier entry is
	// returned and nothing new was booked — no new audit
	// event.
	Duplicate bool `json:"duplicate,omitempty"`
	// ReviewStatus carries the PostTransfer dual-control state
	// ("pending_review" when the transfer was routed into
	// operator review instead of settling); empty otherwise.
	ReviewStatus string `json:"review_status,omitempty"`
	// RepairID is the repair-queue record holding a rejected
	// instruction for operator resolution, empty on success.
	RepairID string `json:"repair_id,omitempty"`
	// Err is the per-instruction rejection reason, nil on
	// success.
	Err error `json:"-"`
}

// mt103AppliedRecord is the replay record for one applied MT103
// instruction, keyed by :20: in mt103Applied: the instruction
// terms it carried and the apply outcome, so a replay returns
// the same outcome without re-booking or re-parking.
type mt103AppliedRecord struct {
	Reference string
	Request   MT103Inbound
	Result    MT103ApplyResult
}

// mt103TermsEqual reports whether a stored instruction record
// carries exactly the terms an inbound request would apply: the
// same bank operation code, value date (by calendar day, as
// :32A: carries only a day), currency and amount, parties (by
// normalized IBAN and name lines), institution BICs,
// remittance, and charge bearer. The block 1/2 LT addresses are
// envelope, not terms, and are not compared.
func mt103TermsEqual(rec mt103AppliedRecord, req MT103Inbound) bool {
	a, b := rec.Request, req
	return a.BankOperationCode == b.BankOperationCode &&
		mt103ValueDate(a.ValueDate) == mt103ValueDate(b.ValueDate) &&
		a.Currency == b.Currency &&
		a.AmountCents == b.AmountCents &&
		normalizeIBAN(a.OrderingAccount) == normalizeIBAN(b.OrderingAccount) &&
		slices.Equal(a.OrderingName, b.OrderingName) &&
		a.OrderingInstitutionBIC == b.OrderingInstitutionBIC &&
		a.BeneficiaryInstitutionBIC == b.BeneficiaryInstitutionBIC &&
		normalizeIBAN(a.BeneficiaryAccount) == normalizeIBAN(b.BeneficiaryAccount) &&
		slices.Equal(a.BeneficiaryName, b.BeneficiaryName) &&
		a.RemittanceInfo == b.RemittanceInfo &&
		a.DetailsOfCharges == b.DetailsOfCharges
}

// mt103ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I103<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT103 inbound error identity.
func mt103ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT103Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT103Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT103Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT103LT, lt)
	}
	return lt, nil
}

// mt103ParseAmount parses a :32A: amount body
// (`<integer>,<2 fraction digits>`) into cents. The integer part
// holds at most 12 digits; the fraction part holds exactly two
// digits; the value must be positive. The MT202 rule, under the
// MT103 error identity.
func mt103ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT103Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT103Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT103Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT103Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT103Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT103Amount, body)
	}
	return cents, nil
}

// mt103ParseIBANField parses a :50K: or :59: account line:
// "/<IBAN>", single line, the IBAN normalized and format-checked
// (a malformed IBAN is a malformed field — whether it names a
// registered account is the apply gate's question, not the
// parser's). The MT101 rule, under the MT103 inbound error
// identity.
func mt103ParseIBANField(body, field string) (string, error) {
	if !strings.HasPrefix(body, "/") {
		return "", fmt.Errorf("%w: %s: want /<IBAN>, got %q", ErrMT103Field, field, body)
	}
	iban := normalizeIBAN(body[1:])
	if iban == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT103Field, field)
	}
	if err := mt103CheckX(iban); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrMT103Field, field, err)
	}
	if !validIBAN(iban) {
		return "", fmt.Errorf("%w: %s: %q is not a valid IBAN", ErrMT103Field, field, iban)
	}
	return iban, nil
}

// mt103Currency validates a :32A: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT103
// inbound error identity.
func mt103Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT103Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT103Currency, code)
		}
	}
	return nil
}

// ParseMT103 parses a SWIFT FIN MT103 message produced by a
// correspondent (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the nine-field core set is refused — a payment
// instruction must not lose content quietly. CRLF line endings
// are accepted; a trailing newline after the `-}` trailer is
// tolerated.
func ParseMT103(text string) (MT103Inbound, error) {
	var zero MT103Inbound
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	// Minimum shape: 3 headers + {4: + 6 single-line fields
	// (:20: :23B: :32A: :50K: :59: :71A:, with :52A:/:57A:/:70:
	// absent and no name lines) + the -} trailer = 11 lines.
	if len(lines) < 11 {
		return zero, fmt.Errorf("%w: want at least 11 lines (3 headers + {4: + 6 fields + -}), got %d", ErrMT103Framing, len(lines))
	}
	senderLT, err := mt103ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt103ParseLT(lines[1], "{2:I103", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT103Framing, mur)
	}
	if err := mt103SenderReference(mur[len("{3:{108:") : len(mur)-2]); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT103Framing, lines[3])
	}
	i := 4
	// expect reads the next line as tag's single-line field: the
	// line must carry the tag and a non-empty body, and must not
	// be empty (an empty line inside block 4 is a framing
	// error, the MT202 rule).
	expect := func(tag string) (string, error) {
		if i >= len(lines) {
			return "", fmt.Errorf("%w: missing %s field", ErrMT103Field, tag)
		}
		line := lines[i]
		if line == "" {
			return "", fmt.Errorf("%w: empty line inside block 4", ErrMT103Framing)
		}
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT103Field, i+1, tag, line)
		}
		body := line[len(tag):]
		if body == "" {
			return "", fmt.Errorf("%w: %s field is empty", ErrMT103Field, tag)
		}
		i++
		return body, nil
	}
	// peek reports whether the next line carries tag's field.
	peek := func(tag string) bool {
		return i < len(lines) && strings.HasPrefix(lines[i], tag)
	}
	// continuations consumes up to max bare lines after a
	// multi-line field (:50K:/:59: name lines, :70: remittance
	// lines): a bare line carries no tag and is not the `-}`
	// trailer. Each line holds at most 35 `x` characters, the
	// party/remittance line rule.
	continuations := func(max int, field string) ([]string, error) {
		var out []string
		for i < len(lines) {
			line := lines[i]
			if line == "-}" || strings.HasPrefix(line, ":") {
				break
			}
			if line == "" {
				return nil, fmt.Errorf("%w: empty line inside block 4", ErrMT103Framing)
			}
			if len(out) >= max {
				return nil, fmt.Errorf("%w: %s: at most %d lines", ErrMT103Field, field, max)
			}
			if len(line) > mt103MaxNameLine {
				return nil, fmt.Errorf("%w: %s line exceeds %d characters: %q", ErrMT103Config, field, mt103MaxNameLine, line)
			}
			if err := mt103CheckX(line); err != nil {
				return nil, fmt.Errorf("%s line: %w", field, err)
			}
			out = append(out, line)
			i++
		}
		return out, nil
	}

	var m MT103Inbound
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20: — the sender's reference, the replay key.
	body, err := expect(":20:")
	if err != nil {
		return zero, err
	}
	if err := mt103SenderReference(body); err != nil {
		return zero, err
	}
	m.Reference = body
	// :23B: — the bank operation code, the export's vocabulary.
	body, err = expect(":23B:")
	if err != nil {
		return zero, err
	}
	if !mt103BankOperationCodes[body] {
		return zero, fmt.Errorf("%w: unknown bank operation code %q", ErrMT103Config, body)
	}
	m.BankOperationCode = body
	// :32A: = YYMMDD + 3-letter currency + <integer>,<2 digits>.
	body, err = expect(":32A:")
	if err != nil {
		return zero, err
	}
	if len(body) < 6+3+4 {
		return zero, fmt.Errorf("%w: :32A: too short: %q", ErrMT103Framing, body)
	}
	datePart, rest := body[:6], body[6:]
	vd, err := time.Parse("060102", datePart)
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT103ValueDate, datePart, err)
	}
	m.ValueDate = vd.UTC()
	if len(rest) < 3 {
		return zero, fmt.Errorf("%w: :32A: missing currency: %q", ErrMT103Framing, body)
	}
	m.Currency = rest[:3]
	if err := mt103Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt103ParseAmount(rest[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	// :50K: — the ordering customer: "/<IBAN>" plus up to 3
	// name/address lines.
	body, err = expect(":50K:")
	if err != nil {
		return zero, err
	}
	iban, err := mt103ParseIBANField(body, ":50K:")
	if err != nil {
		return zero, err
	}
	m.OrderingAccount = iban
	names, err := continuations(mt103MaxNameLines, ":50K:")
	if err != nil {
		return zero, err
	}
	m.OrderingName = names
	// :52A: — optional, the ordering institution BIC:
	// recorded in the audit details only, never routed.
	if peek(":52A:") {
		body, err = expect(":52A:")
		if err != nil {
			return zero, err
		}
		if err := mt103BICField(body, ":52A:"); err != nil {
			return zero, err
		}
		m.OrderingInstitutionBIC = strings.ToUpper(body)
	}
	// :57A: — optional, the beneficiary institution BIC:
	// recorded in the audit details only, never routed.
	if peek(":57A:") {
		body, err = expect(":57A:")
		if err != nil {
			return zero, err
		}
		if err := mt103BICField(body, ":57A:"); err != nil {
			return zero, err
		}
		m.BeneficiaryInstitutionBIC = strings.ToUpper(body)
	}
	// :59: — the beneficiary: "/<IBAN>" plus up to 3
	// name/address lines.
	body, err = expect(":59:")
	if err != nil {
		return zero, err
	}
	iban, err = mt103ParseIBANField(body, ":59:")
	if err != nil {
		return zero, err
	}
	m.BeneficiaryAccount = iban
	names, err = continuations(mt103MaxNameLines, ":59:")
	if err != nil {
		return zero, err
	}
	m.BeneficiaryName = names
	// :70: — optional, the remittance information: the first
	// line rides the tag, up to 3 bare continuation lines
	// follow (4 lines total).
	if peek(":70:") {
		body, err = expect(":70:")
		if err != nil {
			return zero, err
		}
		if len(body) > mt103MaxRemitLine {
			return zero, fmt.Errorf("%w: :70: line exceeds %d characters: %q", ErrMT103Config, mt103MaxRemitLine, body)
		}
		if err := mt103CheckX(body); err != nil {
			return zero, fmt.Errorf(":70: line: %w", err)
		}
		remit := []string{body}
		extra, err := continuations(mt103MaxRemitLines-1, ":70:")
		if err != nil {
			return zero, err
		}
		remit = append(remit, extra...)
		m.RemittanceInfo = strings.Join(remit, "\n")
	}
	// :71A: — the details of charges, the export's vocabulary.
	body, err = expect(":71A:")
	if err != nil {
		return zero, err
	}
	if !mt103DetailsOfCharges[body] {
		return zero, fmt.Errorf("%w: %q: want OUR, BEN, or SHA", ErrMT103ChargeBearer, body)
	}
	m.DetailsOfCharges = body
	// The `-}` trailer must be the last line: anything after
	// it is a field outside the block.
	if i >= len(lines) || lines[i] != "-}" {
		got := ""
		if i < len(lines) {
			got = lines[i]
		}
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT103Framing, got)
	}
	i++
	if i != len(lines) {
		return zero, fmt.Errorf("%w: content after -} trailer: %q", ErrMT103Framing, lines[i])
	}
	return m, nil
}

// mt103NormalizeInbound returns req with its IBANs normalized
// and its BICs and currency upper-cased, so hand-built requests
// compare equal to parsed ones.
func mt103NormalizeInbound(req MT103Inbound) MT103Inbound {
	req.OrderingAccount = normalizeIBAN(req.OrderingAccount)
	req.BeneficiaryAccount = normalizeIBAN(req.BeneficiaryAccount)
	req.OrderingInstitutionBIC = strings.ToUpper(strings.TrimSpace(req.OrderingInstitutionBIC))
	req.BeneficiaryInstitutionBIC = strings.ToUpper(strings.TrimSpace(req.BeneficiaryInstitutionBIC))
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	return req
}

// validateMT103Inbound fail-closes on an instruction ParseMT103
// would refuse, so ApplyMT103 stays safe on hand-built requests
// too: the parse gate and the apply gate share one check, never
// a copy. The business gates (value date is today, IBANs
// resolve to registered accounts, and PostTransfer's own gates)
// are deliberately NOT here: they are per-instruction apply
// decisions, and a refusal parks the instruction in the repair
// queue instead of refusing the call.
func validateMT103Inbound(req MT103Inbound) error {
	req = mt103NormalizeInbound(req)
	if err := mt103SenderReference(req.Reference); err != nil {
		return err
	}
	if !mt103BankOperationCodes[req.BankOperationCode] {
		return fmt.Errorf("%w: unknown bank operation code %q", ErrMT103Config, req.BankOperationCode)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: value date is required", ErrMT103ValueDate)
	}
	if err := mt103Currency(req.Currency); err != nil {
		return err
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: %d is not positive", ErrMT103Amount, req.AmountCents)
	}
	if !validIBAN(req.OrderingAccount) {
		return fmt.Errorf("%w: :50K: %q is not a valid IBAN", ErrMT103Field, req.OrderingAccount)
	}
	if err := mt103PartyLines(req.OrderingName, ":50K:"); err != nil {
		return err
	}
	if err := mt103BICField(req.OrderingInstitutionBIC, ":52A:"); err != nil {
		return err
	}
	if err := mt103BICField(req.BeneficiaryInstitutionBIC, ":57A:"); err != nil {
		return err
	}
	if !validIBAN(req.BeneficiaryAccount) {
		return fmt.Errorf("%w: :59: %q is not a valid IBAN", ErrMT103Field, req.BeneficiaryAccount)
	}
	if err := mt103PartyLines(req.BeneficiaryName, ":59:"); err != nil {
		return err
	}
	if req.RemittanceInfo != "" {
		remitLines := strings.Split(req.RemittanceInfo, "\n")
		if len(remitLines) > mt103MaxRemitLines {
			return fmt.Errorf("%w: :70: at most %d lines", ErrMT103Config, mt103MaxRemitLines)
		}
		for _, ln := range remitLines {
			if len(ln) > mt103MaxRemitLine {
				return fmt.Errorf("%w: :70: line exceeds %d characters: %q", ErrMT103Config, mt103MaxRemitLine, ln)
			}
			if err := mt103CheckX(ln); err != nil {
				return fmt.Errorf(":70: line: %w", err)
			}
		}
	}
	if !mt103DetailsOfCharges[req.DetailsOfCharges] {
		return fmt.Errorf("%w: %q: want OUR, BEN, or SHA", ErrMT103ChargeBearer, req.DetailsOfCharges)
	}
	return nil
}

// parkMT103 parks one parsed MT103 instruction that failed
// closed, and returns the repair record ID. The snapshot's ID is
// the instruction's idempotency key, so the parked item is
// traceable to the exact instruction it refused; its Maker is
// deliberately empty (see the package comment).
func (l *Ledger) parkMT103(req MT103Inbound, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          "mt103-" + req.Reference,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
		// Maker intentionally empty: see the package comment.
		Destination: "mt103-inbound:" + req.Reference,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT103 books one parsed inbound SWIFT FIN MT103 single
// customer credit transfer: it moves the instructed amount from
// the ordering-customer account the :50K: IBAN resolves to, to
// the beneficiary account the :59: IBAN resolves to, through
// PostTransfer, with the :20: as both the journal entry ID and
// the idempotency key's tail ("mt103-"+:20:).
//
// The whole instruction is validated before anything applies.
// Then the gates run in order, each fail-closed into the repair
// queue: the :32A: value date must be today's UTC date; the wire
// IBANs must each resolve to a registered account; and
// PostTransfer's own gates (field validation — same-party,
// non-positive amount — idempotency replay, frozen/overdraft,
// daily limit, accounting period, dual-control review) apply
// last. A refusal parks the instruction and returns its outcome
// with Err set and RepairID set, not an error return — the
// repair record is the trail, exactly like the pain.001 batch
// path. The :52A:/:57A: BICs are recorded in the audit details
// only and never used for routing.
//
// A booked instruction emits one `mt103_inbound_applied` audit
// event (details msg_id / amount_cents / currency, plus the
// :52A:/:57A: BICs when the message carries them). An
// instruction that books nothing emits no booked event: the
// repair record is the trail.
//
// Idempotency is the :20: with its terms (the pain.011
// convention): re-applying the same :20: with the same terms
// returns the stored outcome with Duplicate set, booking
// nothing new and emitting no second audit event. A :20:
// reused for different terms is refused with
// ErrMT103MsgIDConflict.
//
// Locking: the structural validation runs on the parsed values
// alone; the replay read and the IBAN resolution run under the
// read lock; the booking itself runs outside it, because
// PostTransfer takes the write lock; the repair parking runs
// outside any held lock because EnqueueRepair takes l.mu
// itself; the :20: record and the inbound audit event run under
// one short write lock of their own, after the booking is
// durable — re-checking the registry first so a concurrent
// double-apply still records once.
func (l *Ledger) ApplyMT103(req MT103Inbound, actor string) (MT103ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT103"
	}
	req = mt103NormalizeInbound(req)
	if err := validateMT103Inbound(req); err != nil {
		return MT103ApplyResult{}, err
	}
	res := MT103ApplyResult{MsgID: req.Reference}

	// :20: replay: the instruction was already applied under
	// this reference. Same terms is a no-op returning the
	// stored outcome; different terms is a conflict.
	l.mu.RLock()
	rec, ok := l.mt103Applied[req.Reference]
	l.mu.RUnlock()
	if ok {
		if !mt103TermsEqual(rec, req) {
			return MT103ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT103MsgIDConflict, req.Reference)
		}
		out := rec.Result
		out.Duplicate = true
		return out, nil
	}

	// The value date is today's UTC date — the ledger has no
	// future-date (or past-date) execution scheduling.
	today := time.Now().UTC().Format("2006-01-02")
	if req.ValueDate.UTC().Format("2006-01-02") != today {
		res.Err = fmt.Errorf("%w: :32A: value date %q is not today (UTC)", ErrMT103ValueDateNotToday, mt103ValueDate(req.ValueDate))
		repairID, parkErr := l.parkMT103(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT103Applied(req, res, actor)
	}

	// The wire IBANs must each resolve to a registered account.
	// No account is ever opened from a wire identifier.
	l.mu.RLock()
	from, fromOK := l.accountByIBANLocked(req.OrderingAccount)
	to, toOK := l.accountByIBANLocked(req.BeneficiaryAccount)
	l.mu.RUnlock()
	if !fromOK {
		res.Err = fmt.Errorf("%w: :50K: IBAN %q", ErrMT103UnknownIBAN, req.OrderingAccount)
		repairID, parkErr := l.parkMT103(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT103Applied(req, res, actor)
	}
	if !toOK {
		res.Err = fmt.Errorf("%w: :59: IBAN %q", ErrMT103UnknownIBAN, req.BeneficiaryAccount)
		repairID, parkErr := l.parkMT103(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT103Applied(req, res, actor)
	}
	res.From = from
	res.To = to

	// Booking reuses PostTransfer: its gates (field validation,
	// idempotency replay, frozen/overdraft, daily limit,
	// accounting period, dual-control review) apply, and a
	// refusal parks the instruction in the repair queue. The
	// journal entry ID is the :20: and the idempotency key is
	// "mt103-"+:20:, so the correspondent can dedupe on it
	// exactly like an exported payout. The transfer is
	// single-currency by construction: ToCurrency stays empty,
	// so a cross-currency payment can never be booked as one
	// MT103 instruction.
	receipt, err := l.PostTransfer(Transfer{
		ID:             req.Reference,
		From:           from,
		To:             to,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: "mt103-" + req.Reference,
	})
	if err != nil {
		res.Err = err
		repairID, parkErr := l.parkMT103(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT103Applied(req, res, actor)
	}
	res.Receipt = receipt
	res.EntryID = receipt.TransferID
	if len(receipt.Entries) > 0 {
		res.EntryID = receipt.Entries[0].ID
	}
	res.Duplicate = receipt.Duplicate
	res.ReviewStatus = receipt.ReviewStatus
	return l.recordMT103Applied(req, res, actor)
}

// recordMT103Applied stores the :20: replay record for one
// applied MT103 instruction and, when the instruction booked,
// emits the one `mt103_inbound_applied` audit event — under one
// short write lock, after the booking (or parking) is durable.
// A concurrent double-apply that landed first returns the
// stored outcome instead of a second record: the stored
// outcome wins, so exactly one marker event is ever emitted.
func (l *Ledger) recordMT103Applied(req MT103Inbound, res MT103ApplyResult, actor string) (MT103ApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mt103Applied == nil {
		l.mt103Applied = make(map[string]mt103AppliedRecord)
	}
	if rec, ok := l.mt103Applied[req.Reference]; ok {
		if !mt103TermsEqual(rec, req) {
			return MT103ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT103MsgIDConflict, req.Reference)
		}
		out := rec.Result
		out.Duplicate = true
		return out, nil
	}
	l.mt103Applied[req.Reference] = mt103AppliedRecord{
		Reference: req.Reference,
		Request:   req,
		Result:    res,
	}
	if res.Err == nil {
		details := map[string]any{
			"msg_id":       req.Reference,
			"amount_cents": req.AmountCents,
			"currency":     req.Currency,
		}
		if req.OrderingInstitutionBIC != "" {
			details["ordering_institution_bic"] = req.OrderingInstitutionBIC
		}
		if req.BeneficiaryInstitutionBIC != "" {
			details["beneficiary_institution_bic"] = req.BeneficiaryInstitutionBIC
		}
		l.emitAudit(AuditEvent{
			Op:            "mt103_inbound_applied",
			Actor:         actor,
			TraceID:       req.Reference,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{res.From, res.To},
			Details:       details,
		})
	}
	return res, nil
}
