package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT202 general financial institution transfer, parse +
// export (LG-204, fintech: SWIFT 银行间汇款).
//
// MT202 is the bank-to-bank leg of a correspondent payment: one
// financial institution instructing another to move its own funds.
// That is what separates it from the two neighbours it is most often
// confused with:
//
//   - MT103 (see mt103.go, LG-183) is a *customer* credit transfer:
//     the ordering party is a customer (:50K:), the instruction moves
//     the customer's money. MT202 has no customer fields at all — the
//     ordering institution (:52a:) and the beneficiary institution
//     (:58a:) are both banks, and the value moves between their own
//     accounts.
//   - MT940 (see mt940.go) is an account *statement*: it reports
//     movements that already happened. MT202 is an *instruction*: it
//     orders a movement that has not happened yet. Parsing an MT940
//     as an MT202 (or vice versa) would confuse a report with an
//     order, so the two parsers share nothing but the FIN framing
//     helpers.
//
// This module covers the block-4 core field set only:
//
//	:20: transaction reference, :21: related reference,
//	:32A: value date / currency / amount,
//	:52A: ordering institution (BIC form), :58A: beneficiary
//	institution (BIC form).
//
// The `:52a:` / `:58a:` `a` option means "BIC or name-and-address":
// this module implements the A (BIC) variant and refuses the D
// (name/address) variant fail-closed, rather than silently dropping
// the institution identity or guessing a BIC from a name. Likewise
// the optional `:72:` sender-to-receiver information field is out of
// scope: a message carrying it is refused, not parsed with the field
// quietly discarded — a payment instruction must not lose content
// quietly.
//
// Framing follows the MT103 convention: block 1 (basic header, sender
// LT, session/sequence zero-filled and interface-assigned), block 2
// (application header, receiver LT, `I202`), block 3 (`{108:MUR}`
// message user reference), block 4 (the field set above), no block 5
// (CHK is computed by the SWIFT network on receipt; a generator
// emitting a fake checksum would be dishonest). Lines are joined
// with "\n", the nacha.go convention; convert to CRLF for the bank
// if the uploader requires it. Parsing accepts CRLF as well.
//
// Block 3 is envelope framing, validated for shape only: it must be
// present and well-formed (`{3:{108:<1-16 x characters>}}`), but its
// content is not part of the MT202 business record and is not
// carried in the returned struct — the :20: reference is the
// business key. Export always writes the :20: reference as the MUR,
// the MT103 convention.
//
// This is a standalone message value object, not a ledger-bound
// export: unlike MT103 there is no payout (customer instruction) to
// render from, and unlike pacs.028 there is no gpi track to query —
// an MT202 names two banks and a value, and the caller supplies
// them. The value is therefore pure: no ledger lock, no version
// bump, no journal rows, no audit event, and export is deterministic
// per message (repeat exports render byte-identical output).
//
// Field discipline is fail-loud, the nacha.go convention: every field
// is validated on both parse and export, and any violation is an
// error, never a silent truncation or default — except the
// session/sequence zeros in block 1, which the MT103 convention
// documents as interface-assigned.

// MT202 parse/export errors.
var (
	// ErrMT202Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, an empty line inside block 4,
	// or a field out of the canonical :20: → :21: → :32A: → :52A:
	// → :58A: order.
	ErrMT202Framing = errors.New("ledger: mt202: malformed FIN framing")
	// ErrMT202Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT `x`
	// character set, carrying a brace, or with a leading/trailing
	// colon or slash, or a double slash.
	ErrMT202Reference = errors.New("ledger: mt202: invalid reference")
	// ErrMT202ValueDate is returned for an invalid :32A: value date:
	// not a YYMMDD calendar date.
	ErrMT202ValueDate = errors.New("ledger: mt202: invalid :32A: value date")
	// ErrMT202Currency is returned for an invalid :32A: currency:
	// not exactly three uppercase ASCII letters (the LG-18 currency
	// rule — no lowercase, no digits, no ISO lookup table the
	// ledger does not own).
	ErrMT202Currency = errors.New("ledger: mt202: invalid :32A: currency")
	// ErrMT202Amount is returned for an invalid :32A: amount:
	// non-positive, not in `<integer>,<2 fraction digits>` form, or
	// with more than 12 integer digits (the MT103 15d rule).
	ErrMT202Amount = errors.New("ledger: mt202: invalid :32A: amount")
	// ErrMT202BIC is returned for an invalid :52A: or :58A:
	// institution: empty, or not a valid ISO 9362 BIC (see
	// validBIC in pain001.go).
	ErrMT202BIC = errors.New("ledger: mt202: invalid institution BIC")
	// ErrMT202LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT202LT = errors.New("ledger: mt202: invalid SWIFT LT address")
	// ErrMT202Field is returned for a block-4 field problem that is
	// not a value problem: a missing required field, a duplicate
	// field, or a field outside the five-field core set (including
	// the D variants :52D:/:58D: and the optional :72: field).
	ErrMT202Field = errors.New("ledger: mt202: unexpected or missing field")
)

// MT202 is one SWIFT FIN MT202 general financial institution
// transfer: the block-4 core fields plus the block 1/2 LT addresses.
// ValueDate is a UTC calendar date (time-of-day is not carried by
// :32A:); AmountCents is the :32A: amount in minor units, always
// positive.
type MT202 struct {
	SenderLT                  string
	ReceiverLT                string
	TransactionReference      string
	RelatedReference          string
	ValueDate                 time.Time
	Currency                  string
	AmountCents               int64
	OrderingInstitutionBIC    string
	BeneficiaryInstitutionBIC string
}

// mt202Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash.
func mt202Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT202Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT202Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT202Reference, field, ref)
	}
	return nil
}

// mt202Currency validates a :32A: currency code: exactly three
// uppercase ASCII letters.
func mt202Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT202Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT202Currency, code)
		}
	}
	return nil
}

// mt202Amount formats cents as the :32A: 15d amount: up to 12 integer
// digits, a comma decimal separator, two fraction digits — e.g.
// "2500,00". Mirrors mt103Amount, with the MT202 error identity.
func mt202Amount(cents int64) (string, error) {
	if cents <= 0 {
		return "", fmt.Errorf("%w: %d", ErrMT202Amount, cents)
	}
	whole, frac := cents/100, cents%100
	if whole > 999999999999 {
		return "", fmt.Errorf("%w: %d cents exceeds :32A: 15d", ErrMT202Amount, cents)
	}
	return fmt.Sprintf("%d,%02d", whole, frac), nil
}

// mt202ParseAmount parses a :32A: amount body (`<integer>,<ff>`)
// into cents. The integer part holds at most 12 digits; the fraction
// part holds exactly two digits; the value must be positive.
func mt202ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT202Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT202Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT202Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT202Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT202Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT202Amount, body)
	}
	return cents, nil
}

// mt202NormalizeLT normalizes a sender/receiver LT address with the
// MT103 rule (BIC8 → BIC8+"XXX", BIC11 → BIC11+"X", BIC12 as-is),
// reported under the MT202 error identity.
func mt202NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT202LT, raw)
	}
	return lt, nil
}

// mt202Validate checks every field of m, in field order, and returns
// the normalized sender/receiver LT addresses for rendering.
func mt202Validate(m MT202) (senderLT, receiverLT string, err error) {
	senderLT, err = mt202NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt202NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt202Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if err := mt202Reference(m.RelatedReference, ":21:"); err != nil {
		return "", "", err
	}
	if m.ValueDate.IsZero() {
		return "", "", fmt.Errorf("%w: value date is required", ErrMT202ValueDate)
	}
	if err := mt202Currency(m.Currency); err != nil {
		return "", "", err
	}
	if _, err := mt202Amount(m.AmountCents); err != nil {
		return "", "", err
	}
	if m.OrderingInstitutionBIC == "" || !validBIC(m.OrderingInstitutionBIC) {
		return "", "", fmt.Errorf("%w: :52A: %q", ErrMT202BIC, m.OrderingInstitutionBIC)
	}
	if m.BeneficiaryInstitutionBIC == "" || !validBIC(m.BeneficiaryInstitutionBIC) {
		return "", "", fmt.Errorf("%w: :58A: %q", ErrMT202BIC, m.BeneficiaryInstitutionBIC)
	}
	return senderLT, receiverLT, nil
}

// ExportMT202 renders m as a SWIFT FIN MT202 message: blocks 1–4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT202(m MT202) (string, error) {
	senderLT, receiverLT, err := mt202Validate(m)
	if err != nil {
		return "", err
	}
	amount, err := mt202Amount(m.AmountCents)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I202. Priority U,
	// delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I202%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the core field set, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	fmt.Fprintf(&b, ":21:%s\n", m.RelatedReference)
	fmt.Fprintf(&b, ":32A:%s%s%s\n", mt103ValueDate(m.ValueDate), m.Currency, amount)
	fmt.Fprintf(&b, ":52A:%s\n", strings.ToUpper(m.OrderingInstitutionBIC))
	fmt.Fprintf(&b, ":58A:%s\n", strings.ToUpper(m.BeneficiaryInstitutionBIC))
	b.WriteString("-}")
	return b.String(), nil
}

// mt202ValidLT reports whether s is a plausible SWIFT LT address:
// a BIC8, BIC11, or 12-character LT (BIC11 plus terminal character),
// alphanumeric with a 4-letter bank code and 2-letter country code.
func mt202ValidLT(s string) bool {
	if len(s) != 8 && len(s) != 11 && len(s) != 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return false
		}
		if i < 6 && !isLetter {
			return false
		}
	}
	return true
}

// mt202ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export ∘ Parse
// is stable: Export normalizes (BIC8 → BIC8+"XXX", the MT103 rule),
// and Parse returns that normalized form verbatim rather than
// re-normalizing it a second time (which would append another "X").
// Block 1 has the form `{1:F01<LT><10 session/sequence digits>}`;
// block 2 has the form `{2:I202<LT><5 header characters>}`
// (priority U, delivery monitoring 3, obsolescence period 003).
func mt202ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT202Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT202Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT202Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT202LT, lt)
	}
	return lt, nil
}

// ParseMT202 parses a SWIFT FIN MT202 message produced by
// ExportMT202 (or an equivalent correspondent-bank message) into its
// business fields. Parsing is strict: the block structure, the field
// order, and every field value are validated, and anything outside
// the five-field core set is refused — a payment instruction must
// not lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated.
func ParseMT202(text string) (MT202, error) {
	var zero MT202
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 10 {
		return zero, fmt.Errorf("%w: want 10 lines (3 headers + {4: + 5 fields + -}), got %d", ErrMT202Framing, len(lines))
	}
	senderLT, err := mt202ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt202ParseLT(lines[1], "{2:I202", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT202Framing, mur)
	}
	if err := mt202Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT202Framing, lines[3])
	}
	if lines[9] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT202Framing, lines[9])
	}
	// Block 4, canonical order, single-line fields only: a field
	// body never spans lines in this core set, so any line not
	// starting with its expected tag is a framing error, not a
	// continuation.
	fields := []struct {
		tag, body string
	}{
		{":20:", ""}, {":21:", ""}, {":32A:", ""}, {":52A:", ""}, {":58A:", ""},
	}
	for i, want := range fields {
		line := lines[4+i]
		if !strings.HasPrefix(line, want.tag) {
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT202Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT202Field, want.tag)
		}
	}
	var m MT202
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt202Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.TransactionReference = fields[0].body
	if err := mt202Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :32A: = YYMMDD + 3-letter currency + <integer>,<2 digits>.
	body32 := fields[2].body
	if len(body32) < 6+3+4 {
		return zero, fmt.Errorf("%w: :32A: too short: %q", ErrMT202Framing, body32)
	}
	datePart, rest := body32[:6], body32[6:]
	vd, err := time.Parse("060102", datePart)
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT202ValueDate, datePart, err)
	}
	m.ValueDate = vd.UTC()
	if len(rest) < 3 {
		return zero, fmt.Errorf("%w: :32A: missing currency: %q", ErrMT202Framing, body32)
	}
	m.Currency = rest[:3]
	if err := mt202Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt202ParseAmount(rest[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	m.OrderingInstitutionBIC = fields[3].body
	if m.OrderingInstitutionBIC == "" || !validBIC(m.OrderingInstitutionBIC) {
		return zero, fmt.Errorf("%w: :52A: %q", ErrMT202BIC, m.OrderingInstitutionBIC)
	}
	m.BeneficiaryInstitutionBIC = fields[4].body
	if m.BeneficiaryInstitutionBIC == "" || !validBIC(m.BeneficiaryInstitutionBIC) {
		return zero, fmt.Errorf("%w: :58A: %q", ErrMT202BIC, m.BeneficiaryInstitutionBIC)
	}
	return m, nil
}
