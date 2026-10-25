package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT900/MT910 debit/credit confirmations, parse + export
// (LG-209, fintech: SWIFT 借记/贷记确认报文).
//
// An MT900 confirms a debit to an account; an MT910 confirms a credit
// to an account. Both are *post-settlement* confirmations, and that
// tense is the whole point of this module:
//
//   - MT103 (see mt103.go, LG-183) is a *customer instruction*: it
//     orders a movement that has not happened yet, so its gate is
//     "approved or submitted".
//   - MT202 (see mt202.go, LG-204) is a *bank-to-bank instruction*:
//     it orders a movement that has not happened yet either.
//   - MT900/MT910 report a movement that *already settled*. Issuing
//     one for a payout that has not settled would tell the account
//     holder money moved when it did not, so the ledger-bound export
//     below refuses every non-settled payout fail-closed — pending
//     (draft/approved/submitted), failed, cancelled, and returned
//     alike. A returned payout is refused too: its funds came back
//     (see ApplyPacs004Return, LG-206), so confirming the original
//     debit as still standing would be a lie.
//
// This module covers the block-4 field set the backlog names:
//
//	:20: transaction reference, :21: related reference,
//	:25: account identification, :32A: value date / currency /
//	amount, :52a: ordering institution, :72: sender-to-receiver
//	information.
//
// The `:52a:` / `:72:` fields follow the real MT900/MT910 network
// rules: both are optional. `:52a:` is implemented in the A (BIC)
// variant only, the MT202 convention — a D (name/address) variant is
// refused on parse rather than silently dropped or guessed into a
// BIC. `:72:` carries up to 6 lines of at most 35 `x` characters
// each, the MT103 `:70:` convention.
//
// Framing follows the MT103/MT202 convention: block 1 (basic header,
// sender LT, session/sequence zero-filled and interface-assigned),
// block 2 (application header, receiver LT, `I900` or `I910`),
// block 3 (`{108:MUR}` carrying the :20: reference), block 4 (the
// field set above, in canonical order), no block 5 (CHK is computed
// by the SWIFT network on receipt; a generator emitting a fake
// checksum would be dishonest). Lines are joined with "\n", the
// nacha.go convention; parsing accepts CRLF as well.
//
// Block 3 is envelope framing, validated for shape only: it must be
// present and well-formed (`{3:{108:<1-16 x characters>}}`), but its
// content is not part of the business record and is not carried in
// the returned struct — the :20: reference is the business key.
// Export always writes the :20: reference as the MUR, the MT103
// convention.
//
// The value object is pure: no ledger lock, no version bump, no
// journal rows, no audit event, and export is deterministic per
// message (repeat exports render byte-identical output). The
// ledger-bound export is read-only in the same way: it copies the
// payout under a read lock and renders from the copy, so exporting a
// confirmation can never move money or change the payout's status.
// A confirmation is a report about a settlement, never a settlement
// itself — and it never carries a fee, a reversal, or a second
// posting of the confirmed amount.
//
// Field discipline is fail-loud, the nacha.go convention: every field
// is validated on both parse and export, and any violation is an
// error, never a silent truncation or default — except the
// session/sequence zeros in block 1, which the MT103 convention
// documents as interface-assigned, and the documented defaults of
// the ledger-bound export (:21: "NONREF", :25: the payee account,
// the value date from the settlement timestamp).

// MT900/MT910 parse/export errors.
var (
	// ErrMT900910Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, an empty line inside block 4
	// outside a :72: continuation, or a field out of the canonical
	// :20: → :21: → :25: → :32A: → :52A: → :72: order.
	ErrMT900910Framing = errors.New("ledger: mt900910: malformed FIN framing")
	// ErrMT900910Type is returned when the message type is not
	// "900" (debit confirmation) or "910" (credit confirmation).
	ErrMT900910Type = errors.New("ledger: mt900910: message type must be 900 or 910")
	// ErrMT900910Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT `x`
	// character set, carrying a brace or a line break, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT900910Reference = errors.New("ledger: mt900910: invalid reference")
	// ErrMT900910Account is returned for an invalid :25: account
	// identification: empty, over 35 characters, or outside the
	// SWIFT `x` character set (braces and line breaks refused).
	ErrMT900910Account = errors.New("ledger: mt900910: invalid :25: account identification")
	// ErrMT900910ValueDate is returned for an invalid :32A: value
	// date: not a YYMMDD calendar date.
	ErrMT900910ValueDate = errors.New("ledger: mt900910: invalid :32A: value date")
	// ErrMT900910Currency is returned for an invalid :32A:
	// currency: not exactly three uppercase ASCII letters (the LG-18
	// currency rule — no lowercase, no digits, no ISO lookup table
	// the ledger does not own).
	ErrMT900910Currency = errors.New("ledger: mt900910: invalid :32A: currency")
	// ErrMT900910Amount is returned for an invalid :32A: amount:
	// non-positive, not in `<integer>,<2 fraction digits>` form, or
	// with more than 12 integer digits (the MT103 15d rule).
	ErrMT900910Amount = errors.New("ledger: mt900910: invalid :32A: amount")
	// ErrMT900910BIC is returned for an invalid :52A: ordering
	// institution: not a valid ISO 9362 BIC (see validBIC in
	// pain001.go).
	ErrMT900910BIC = errors.New("ledger: mt900910: invalid :52A: ordering institution BIC")
	// ErrMT900910Info is returned for an invalid :72:
	// sender-to-receiver information line: empty, over 35
	// characters, outside the SWIFT `x` character set, or more than
	// 6 lines in total.
	ErrMT900910Info = errors.New("ledger: mt900910: invalid :72: sender-to-receiver information")
	// ErrMT900910LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT900910LT = errors.New("ledger: mt900910: invalid SWIFT LT address")
	// ErrMT900910Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a duplicate
	// field, or a field outside the six-field set (including the D
	// variant :52D: and any field after :72:, which is last).
	ErrMT900910Field = errors.New("ledger: mt900910: unexpected or missing field")
	// ErrMT900910BadStatus is returned by the ledger-bound export
	// when the payout is not settled: a draft, approved, submitted,
	// failed, cancelled, or returned payout must never produce a
	// debit/credit confirmation — only a settled payout evidences a
	// movement that actually happened.
	ErrMT900910BadStatus = errors.New("ledger: mt900910 export: payout is not settled")
	// ErrMT900910PayoutNotFound is returned by the ledger-bound
	// export for an unknown payout ID.
	ErrMT900910PayoutNotFound = errors.New("ledger: mt900910 export: payout not found")
	// ErrMT900910NoSenderBIC is returned by the ledger-bound export
	// when no sender can be named: neither an explicit SenderLT nor
	// a registered IBAN BIC on the payee account.
	ErrMT900910NoSenderBIC = errors.New("ledger: mt900910 export: no sender BIC")
)

// MT900910InfoMaxLines is the MT900/MT910 :72: line cap (6 lines of
// at most 35 `x` characters each).
const MT900910InfoMaxLines = 6

// MT900910 is one SWIFT FIN MT900 (debit confirmation) or MT910
// (credit confirmation): the block-4 field set plus the block 1/2 LT
// addresses. MessageType is "900" or "910". ValueDate is a UTC
// calendar date (time-of-day is not carried by :32A:); AmountCents
// is the :32A: amount in minor units, always positive.
// OrderingInstitutionBIC is optional (empty omits :52A:);
// SenderToReceiverInfo is optional (empty omits :72:).
type MT900910 struct {
	MessageType            string
	SenderLT               string
	ReceiverLT             string
	TransactionReference   string
	RelatedReference       string
	Account                string
	ValueDate              time.Time
	Currency               string
	AmountCents            int64
	OrderingInstitutionBIC string
	SenderToReceiverInfo   []string
}

// mt900910Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash.
func mt900910Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT900910Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT900910Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT900910Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT900910Reference, field, ref)
	}
	return nil
}

// mt900910Account validates a :25: account identification: 1–35
// SWIFT `x` characters, no braces, no line breaks.
func mt900910Account(acct string) error {
	if acct == "" || len(acct) > 35 {
		return fmt.Errorf("%w: want 1-35 characters, got %q", ErrMT900910Account, acct)
	}
	if strings.ContainsAny(acct, "\r\n") {
		return fmt.Errorf("%w: line break in a single-line field: %q", ErrMT900910Account, acct)
	}
	if err := mt103CheckX(acct); err != nil {
		return fmt.Errorf("%w: %w", ErrMT900910Account, err)
	}
	return nil
}

// mt900910Currency validates a :32A: currency code: exactly three
// uppercase ASCII letters.
func mt900910Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT900910Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT900910Currency, code)
		}
	}
	return nil
}

// mt900910Amount formats cents as the :32A: 15d amount: up to 12
// integer digits, a comma decimal separator, two fraction digits —
// e.g. "2500,00". Mirrors mt103Amount, with the MT900/MT910 error
// identity.
func mt900910Amount(cents int64) (string, error) {
	if cents <= 0 {
		return "", fmt.Errorf("%w: %d", ErrMT900910Amount, cents)
	}
	whole, frac := cents/100, cents%100
	if whole > 999999999999 {
		return "", fmt.Errorf("%w: %d cents exceeds :32A: 15d", ErrMT900910Amount, cents)
	}
	return fmt.Sprintf("%d,%02d", whole, frac), nil
}

// mt900910ParseAmount parses a :32A: amount body
// (`<integer>,<2 fraction digits>`) into cents.
func mt900910ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT900910Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT900910Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT900910Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT900910Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT900910Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT900910Amount, body)
	}
	return cents, nil
}

// mt900910NormalizeLT normalizes a sender/receiver LT address with
// the MT103 rule (BIC8 → BIC8+"XXX", BIC11 → BIC11+"X", BIC12
// as-is), reported under the MT900/MT910 error identity.
func mt900910NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT900910LT, raw)
	}
	return lt, nil
}

// mt900910Info validates :72: sender-to-receiver information: at
// most MT900910InfoMaxLines lines, each 1–35 SWIFT `x` characters,
// no braces, no line breaks inside a line.
func mt900910Info(lines []string) error {
	if len(lines) > MT900910InfoMaxLines {
		return fmt.Errorf("%w: at most %d lines, got %d", ErrMT900910Info, MT900910InfoMaxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > 35 {
			return fmt.Errorf("%w: line exceeds 1-35 characters: %q", ErrMT900910Info, ln)
		}
		if strings.ContainsAny(ln, "\r\n") {
			return fmt.Errorf("%w: line break inside a :72: line: %q", ErrMT900910Info, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %w", ErrMT900910Info, err)
		}
	}
	return nil
}

// mt900910Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses for rendering.
func mt900910Validate(m MT900910) (senderLT, receiverLT string, err error) {
	if m.MessageType != "900" && m.MessageType != "910" {
		return "", "", fmt.Errorf("%w: %q", ErrMT900910Type, m.MessageType)
	}
	senderLT, err = mt900910NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt900910NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt900910Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if err := mt900910Reference(m.RelatedReference, ":21:"); err != nil {
		return "", "", err
	}
	if err := mt900910Account(m.Account); err != nil {
		return "", "", err
	}
	if m.ValueDate.IsZero() {
		return "", "", fmt.Errorf("%w: value date is required", ErrMT900910ValueDate)
	}
	if err := mt900910Currency(m.Currency); err != nil {
		return "", "", err
	}
	if _, err := mt900910Amount(m.AmountCents); err != nil {
		return "", "", err
	}
	if m.OrderingInstitutionBIC != "" && !validBIC(m.OrderingInstitutionBIC) {
		return "", "", fmt.Errorf("%w: :52A: %q", ErrMT900910BIC, m.OrderingInstitutionBIC)
	}
	if err := mt900910Info(m.SenderToReceiverInfo); err != nil {
		return "", "", err
	}
	return senderLT, receiverLT, nil
}

// ExportMT900910 renders m as a SWIFT FIN MT900/MT910 message:
// blocks 1–4, lines joined with "\n", no trailing newline (the
// nacha.go convention). The export is deterministic per message:
// repeat exports render byte-identical output.
func ExportMT900910(m MT900910) (string, error) {
	senderLT, receiverLT, err := mt900910Validate(m)
	if err != nil {
		return "", err
	}
	amount, err := mt900910Amount(m.AmountCents)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I900/I910.
	// Priority U, delivery monitoring 3, obsolescence period 003 —
	// the MT103 application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I%s%sU3003}\n", m.MessageType, receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the field set, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	fmt.Fprintf(&b, ":21:%s\n", m.RelatedReference)
	fmt.Fprintf(&b, ":25:%s\n", m.Account)
	fmt.Fprintf(&b, ":32A:%s%s%s\n", mt103ValueDate(m.ValueDate), m.Currency, amount)
	if m.OrderingInstitutionBIC != "" {
		fmt.Fprintf(&b, ":52A:%s\n", strings.ToUpper(m.OrderingInstitutionBIC))
	}
	if len(m.SenderToReceiverInfo) > 0 {
		fmt.Fprintf(&b, ":72:%s\n", m.SenderToReceiverInfo[0])
		for _, ln := range m.SenderToReceiverInfo[1:] {
			fmt.Fprintf(&b, "%s\n", ln)
		}
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt900910ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export ∘ Parse
// is stable: Export normalizes (BIC8 → BIC8+"XXX", the MT103 rule),
// and Parse returns that normalized form verbatim rather than
// re-normalizing it a second time (which would append another "X").
// Block 1 has the form `{1:F01<LT><10 session/sequence digits>}`;
// block 2 has the form `{2:I900<LT><5 header characters>}` (or
// I910, priority U, delivery monitoring 3, obsolescence 003).
func mt900910ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT900910Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT900910Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT900910Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT900910LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT900910LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT900910LT, lt)
		}
	}
	return lt, nil
}

// ParseMT900910 parses a SWIFT FIN MT900/MT910 message produced by
// ExportMT900910 (or an equivalent correspondent-bank message) into
// its business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the six-field set is refused — a confirmation must not
// lose content quietly. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated. A :72: field, once
// started, consumes every remaining block-4 line as a continuation
// (it is the last field, so a continuation line may itself start
// with a colon without being mistaken for a new field).
func ParseMT900910(text string) (MT900910, error) {
	var zero MT900910
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 9 {
		return zero, fmt.Errorf("%w: want at least 9 lines (3 headers + {4: + 4 required fields + -}), got %d", ErrMT900910Framing, len(lines))
	}
	senderLT, err := mt900910ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	// Block 2: the message type is carried by the application
	// header itself (I900 vs I910); anything else is not a
	// confirmation at all.
	var msgType string
	switch {
	case strings.HasPrefix(lines[1], "{2:I900"):
		msgType = "900"
	case strings.HasPrefix(lines[1], "{2:I910"):
		msgType = "910"
	default:
		return zero, fmt.Errorf("%w: block 2 is not I900/I910: %q", ErrMT900910Framing, lines[1])
	}
	receiverLT, err := mt900910ParseLT(lines[1], "{2:I"+msgType, 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT900910Framing, mur)
	}
	if err := mt900910Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT900910Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT900910Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	// Block 4, canonical order: the four required single-line
	// fields first, then the optional :52A:, then the optional
	// :72: (which consumes the rest as continuations).
	required := []string{":20:", ":21:", ":25:", ":32A:"}
	if len(body) < len(required) {
		return zero, fmt.Errorf("%w: want at least %d block-4 fields, got %d", ErrMT900910Field, len(required), len(body))
	}
	var m MT900910
	m.MessageType, m.SenderLT, m.ReceiverLT = msgType, senderLT, receiverLT
	var fieldBodies [4]string
	for i, tag := range required {
		line := body[i]
		if !strings.HasPrefix(line, tag) {
			return zero, fmt.Errorf("%w: block-4 line %d: want %s field, got %q", ErrMT900910Field, i+1, tag, line)
		}
		fieldBodies[i] = line[len(tag):]
		if fieldBodies[i] == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT900910Field, tag)
		}
	}
	if err := mt900910Reference(fieldBodies[0], ":20:"); err != nil {
		return zero, err
	}
	m.TransactionReference = fieldBodies[0]
	if err := mt900910Reference(fieldBodies[1], ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fieldBodies[1]
	if err := mt900910Account(fieldBodies[2]); err != nil {
		return zero, err
	}
	m.Account = fieldBodies[2]
	// :32A: = YYMMDD + 3-letter currency + <integer>,<2 digits>.
	body32 := fieldBodies[3]
	if len(body32) < 6+3+4 {
		return zero, fmt.Errorf("%w: :32A: too short: %q", ErrMT900910Framing, body32)
	}
	datePart, rest := body32[:6], body32[6:]
	vd, err := time.Parse("060102", datePart)
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT900910ValueDate, datePart, err)
	}
	m.ValueDate = vd.UTC()
	if len(rest) < 3 {
		return zero, fmt.Errorf("%w: :32A: missing currency: %q", ErrMT900910Framing, body32)
	}
	m.Currency = rest[:3]
	if err := mt900910Currency(m.Currency); err != nil {
		return zero, err
	}
	amount, err := mt900910ParseAmount(rest[3:])
	if err != nil {
		return zero, err
	}
	m.AmountCents = amount
	// Optional tail: at most one :52A:, then at most one :72:
	// (consuming the rest). Anything else is an unexpected field —
	// including :52D:, which is refused rather than silently
	// downgraded, and any field after :72:.
	rest_lines := body[len(required):]
	if len(rest_lines) > 0 && strings.HasPrefix(rest_lines[0], ":52A:") {
		bic := rest_lines[0][len(":52A:"):]
		if bic == "" || !validBIC(bic) {
			return zero, fmt.Errorf("%w: :52A: %q", ErrMT900910BIC, bic)
		}
		m.OrderingInstitutionBIC = bic
		rest_lines = rest_lines[1:]
	}
	if len(rest_lines) > 0 {
		if !strings.HasPrefix(rest_lines[0], ":72:") {
			return zero, fmt.Errorf("%w: unexpected field after :32A:/:52A:: %q", ErrMT900910Field, rest_lines[0])
		}
		info := []string{rest_lines[0][len(":72:"):]}
		info = append(info, rest_lines[1:]...)
		if err := mt900910Info(info); err != nil {
			return zero, err
		}
		m.SenderToReceiverInfo = info
	}
	return m, nil
}

// MT900910PayoutOptions tunes ExportMT900910ForPayout.
//
// MessageType is "900" (debit confirmation, the default for a
// payout: money left the payee account) or "910" (credit
// confirmation, when the confirmation is issued from the
// beneficiary side). The ledger does not guess the direction from
// the payout — the caller names it — but the default is the
// payout-natural debit side.
// ReceiverLT is required: the correspondent's LT address for block
// 2. SenderLT is optional: the sender's own LT address for block 1,
// defaulting to the payee account's registered IBAN BIC (the MT103
// convention); missing everywhere is a fail-closed refusal.
// RelatedReference is optional: the :21: reference, defaulting to
// "NONREF" (the SWIFT convention when there is no related
// reference to cite).
// Account is optional: the :25: account identification, defaulting
// to the payout's payee account — the account the settlement leg
// debited (see SettlePayout). Pass an explicit external account
// reference when the confirmation goes to a different account view.
// OrderingInstitutionBIC is optional: :52A:.
// SenderToReceiverInfo is optional: :72:, up to 6 lines.
// ValueDate is optional: the :32A: value date, defaulting to the
// payout's settlement timestamp, then submission, then creation —
// the date the money actually moved.
type MT900910PayoutOptions struct {
	MessageType            string
	ReceiverLT             string
	SenderLT               string
	RelatedReference       string
	Account                string
	OrderingInstitutionBIC string
	SenderToReceiverInfo   []string
	ValueDate              time.Time
}

// ExportMT900910ForPayout renders a settled payout as a SWIFT FIN
// MT900/MT910 debit/credit confirmation. The payout must be settled
// (see ErrMT900910BadStatus): a confirmation asserts money moved,
// so it may only ever be issued for a movement that did. The export
// is read-only and deterministic per payout: repeat exports render
// byte-identical output, and no ledger state is mutated.
func (l *Ledger) ExportMT900910ForPayout(payoutID string, opts MT900910PayoutOptions) (string, error) {
	if payoutID == "" {
		return "", fmt.Errorf("%w: %q", ErrMT900910PayoutNotFound, payoutID)
	}
	l.mu.RLock()
	p, ok := l.payouts[payoutID]
	var senderBIC string
	if reg, ok2 := l.accountIBANs[p.PayeeAccount]; ok2 {
		senderBIC = reg.BIC
	}
	l.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrMT900910PayoutNotFound, payoutID)
	}
	// p and senderBIC are value copies out of the maps: the render
	// below cannot mutate ledger state even by accident.
	if p.Status != PayoutSettled {
		return "", fmt.Errorf("%w: payout %q is %s", ErrMT900910BadStatus, p.ID, p.Status)
	}

	msgType := opts.MessageType
	if msgType == "" {
		msgType = "900"
	}
	if opts.SenderLT != "" {
		senderBIC = ""
	}
	senderSrc := firstNonEmpty(opts.SenderLT, senderBIC)
	if senderSrc == "" {
		return "", fmt.Errorf("%w: register the payee account's BIC (SetAccountIBAN) or pass SenderLT", ErrMT900910NoSenderBIC)
	}
	related := opts.RelatedReference
	if related == "" {
		related = "NONREF"
	}
	account := opts.Account
	if account == "" {
		account = string(p.PayeeAccount)
	}
	valueDate := opts.ValueDate
	if valueDate.IsZero() {
		valueDate = p.SettledAt
		if valueDate.IsZero() {
			valueDate = p.SubmittedAt
		}
		if valueDate.IsZero() {
			valueDate = p.CreatedAt
		}
	}
	if p.Currency == "" {
		return "", fmt.Errorf("%w: payout %q has no currency", ErrMT900910Field, p.ID)
	}
	return ExportMT900910(MT900910{
		MessageType:            msgType,
		SenderLT:               senderSrc,
		ReceiverLT:             opts.ReceiverLT,
		TransactionReference:   p.ID,
		RelatedReference:       related,
		Account:                account,
		ValueDate:              valueDate,
		Currency:               p.Currency,
		AmountCents:            p.AmountCents,
		OrderingInstitutionBIC: opts.OrderingInstitutionBIC,
		SenderToReceiverInfo:   opts.SenderToReceiverInfo,
	})
}
