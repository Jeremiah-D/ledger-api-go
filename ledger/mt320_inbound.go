package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN MT320 fixed loan/deposit confirmation inbound processing
// (LG-446): the inbound half of the MT320 lifecycle, the mirror
// of the LG-392 export.
//
// ExportMT320 renders this ledger's own confirmation — the ledger
// telling the counterparty "this booked term deposit is confirmed".
// This module consumes the confirmation the counterparty sent to
// this ledger, acting as the other side of the loan: it matches
// the confirmation against the booked term deposit the :20:
// reference names, and records the match — and nothing else. The
// two directions are orthogonal by construction:
//
//   - LG-392 exports: the ledger confirms one of its own booked
//     term deposits (outbound, read-only).
//   - LG-446 imports: the counterparty confirms the same deposit
//     back (inbound, read-only apart from the confirmation
//     marker: no settlement, no new deposit, no lifecycle
//     transition — MatureTermDeposit remains the only settlement
//     entry, LG-90).
//
// ParseMT320 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT202 (LG-204) and ParseMT300
// (LG-440): the block structure, the field order, and every field
// value are validated, and anything outside the twenty-field core
// set (:15A: :20: :22A: :22B: :22C: :82a: :87a: :15B: :17R:
// :30T: :30V: :30P: :32B: :34E: :37G: :14D: :15C: :57a: :15D:
// :57a:) is refused — a confirmation must not lose content
// quietly. CRLF line endings are accepted; a trailing newline
// after the `-}` trailer is tolerated. The :82a: / :87a: /
// :57a: `a` option means "BIC or name-and-address": this module
// implements the A (BIC) variant and the D (account-ID) variant
// the export renders, and refuses any other option fail-closed
// rather than silently dropping the party identity.
//
// ApplyMT320 resolves the deposit by the :20: reference — the
// deposit ID, the addressing key this message carries directly
// (unlike MT300, whose :20: is a message reference and whose
// :22C: does the resolving) — and verifies every economic term
// exactly against the booked contract: the :22C: common reference
// against mt320CommonReference recomputed from the stored
// depositor identifier, the message's :87A: counterparty BIC, and
// the :20: deposit ID; the :30T:/:30V: dates against the deposit's
// OpenedAt value date and the :30P: date against MaturesAt; the
// :32B: principal against AmountCents; the :34E: contracted
// interest total against TermDays x the daily interest (big-number
// exact); and the :37G: rate against RateBps (half-up rounded to
// 12 decimal places, the mt320RateBps rule). An unknown deposit
// reports ErrTermDepositNotFound, fail-closed — a confirmation
// never opens a deposit. Any mismatch reports
// ErrMT320TermsMismatch and the whole confirmation is refused:
// there is no partial confirmation. An active or matured deposit
// can be confirmed; an early-withdrawn one reports
// ErrTermDepositNotActive, mirroring the export's gate. A
// tiered-rate deposit (LG-145) has no single :37G: to state and
// fails loud with ErrInvalidMT320Export, the export's rule.
//
// Only :22A: NEWT and :22B: CONF are accepted: this endpoint
// confirms new contracts only (AMND/CANC variants are a later
// round, the export's rule). The :57a: settlement legs must name
// the same banks as the parties — sequence C the counterparty,
// sequence D the depositor — or the confirmation is internally
// contradictory and refused.
//
// A matched confirmation is recorded twice, both read-only with
// respect to the contract: the mt320Applied :20: registry (the
// replay guard, the pain011Applied pattern) and the
// mt320Confirmed marker (deposit ID → confirming :20:), so a
// deposit matched once reads as confirmed without any lifecycle
// effect. Success emits `mt320_confirmation_matched` (details
// msg_id / common_ref / deposit_id, actor `ApplyMT320`).
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// deposit, dates, rate, and amounts returns the current deposit
// with Duplicate set, recording nothing twice and emitting no
// second audit event. A :20: reused for different terms is
// refused with ErrMT320MsgIDConflict — never silently
// overwritten. The registry check runs under the write lock, so
// two concurrent applies of the same :20: still emit exactly
// one marker event.
//
// No deposit is ever auto-created, no settlement runs here: the
// confirmation is matched first, and settlement still arrives
// separately through MatureTermDeposit. One MT320 confirms exactly
// one deposit — no batch confirmation.

// MT320 inbound errors.
var (
	// ErrMT320Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :15A: → :20: → :22A: → :22B: → :22C: → :82a:
	// → :87a: → :15B: → :17R: → :30T: → :30V: → :30P: → :32B:
	// → :34E: → :37G: → :14D: → :15C: → :57a: → :15D: → :57a:
	// order.
	ErrMT320Framing = errors.New("ledger: mt320: malformed FIN framing")
	// ErrMT320Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a field
	// outside the twenty-field core set, a non-empty :15A: /
	// :15B: / :15C: / :15D: sequence marker, a :22A: other than
	// NEWT, a :22B: other than CONF, a :17R: other than L, or a
	// :14D: other than ACT/365.
	ErrMT320Field = errors.New("ledger: mt320: unexpected or missing field")
	// ErrMT320Reference is returned for an invalid :20: or :22C:
	// reference: empty, over 16 characters (for :20:), outside
	// the SWIFT `x` character set, with a leading/trailing colon
	// or slash, or a double slash — or a :22C: outside the
	// 4!a2!c4!n4!a2!c shape.
	ErrMT320Reference = errors.New("ledger: mt320: invalid reference")
	// ErrMT320ValueDate is returned for an invalid :30T:, :30V:,
	// or :30P: date: not a YYYYMMDD calendar date.
	ErrMT320ValueDate = errors.New("ledger: mt320: invalid :30T:/:30V:/:30P: date")
	// ErrMT320Currency is returned for an invalid :32B: or :34E:
	// currency: not exactly three uppercase ASCII letters (the
	// LG-18 currency rule — no lowercase, no digits, no ISO
	// lookup table the ledger does not own).
	ErrMT320Currency = errors.New("ledger: mt320: invalid :32B:/:34E: currency")
	// ErrMT320Amount is returned for an invalid :32B: or :34E:
	// amount: non-positive, not in `<integer>,<2 fraction
	// digits>` form, or with more than 12 integer digits (the
	// MT103 15d rule).
	ErrMT320Amount = errors.New("ledger: mt320: invalid :32B:/:34E: amount")
	// ErrMT320Rate is returned for an invalid :37G: rate: not a
	// positive decimal with a comma separator and 1–12 fraction
	// digits.
	ErrMT320Rate = errors.New("ledger: mt320: invalid :37G: rate")
	// ErrMT320LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT320LT = errors.New("ledger: mt320: invalid SWIFT LT address")
	// ErrMT320MsgIDConflict is returned by ApplyMT320 when a
	// :20: reference already applied carries different terms: the
	// reference names one confirmation, and a second, different
	// confirmation under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT320MsgIDConflict = errors.New("ledger: mt320 inbound: message reference already applied with different terms")
	// ErrMT320TermsMismatch is returned by ApplyMT320 when the
	// confirmation's economic terms disagree with the booked
	// deposit — the :22C: common reference, a party, a
	// settlement leg, a date, the principal, the contracted
	// interest total, or the rate. It is a 422-class error.
	ErrMT320TermsMismatch = errors.New("ledger: mt320 inbound: confirmation terms do not match the booked deposit")
)

// MT320Confirmation is one parsed inbound SWIFT FIN MT320 fixed
// loan/deposit confirmation: the block-4 core fields plus the
// block 1/2 LT addresses. TradeDate, ValueDate, and MaturityDate
// are UTC calendar dates (time-of-day is not carried by :30T: /
// :30V: / :30P:); PrincipalCents and InterestCents are the
// :32B: / :34E: amounts in minor units, always positive; Rate is
// the :37G: rate exactly as written.
type MT320Confirmation struct {
	SenderLT   string
	ReceiverLT string
	// Reference is :20:, the deposit ID — this message's own
	// reference and the replay key.
	Reference string
	// Variant is :22A:: always NEWT on a message ParseMT320
	// accepts.
	Variant string
	// ConfirmationType is :22B:: always CONF on a message
	// ParseMT320 accepts.
	ConfirmationType string
	// CommonReference is :22C:, the 16-character common
	// reference: first 4 of the depositor identifier + first 2
	// of the counterparty BIC + the last 4 digits of the deposit
	// ID + first 4 of the counterparty BIC + first 2 of the
	// depositor identifier.
	CommonReference string
	// Depositor is the :82A: BIC or :82D: account ID; the BIC
	// flag reports which form it is.
	Depositor    string
	DepositorBIC bool
	// Counterparty is the :87A: BIC; the export always renders
	// the A form, so a D form is refused at apply time.
	Counterparty    string
	CounterpartyBIC bool
	// Role is :17R:: always L on a message ParseMT320 accepts —
	// the depositor is the lender.
	Role string
	// TradeDate is :30T:, the trade date; ValueDate is :30V:,
	// the value date; MaturityDate is :30P:, the maturity date.
	// :30T: and :30V: both equal the deposit's OpenedAt value
	// date; :30P: equals MaturesAt.
	TradeDate    time.Time
	ValueDate    time.Time
	MaturityDate time.Time
	// PrincipalCurrency/PrincipalCents is :32B:, the principal;
	// InterestCurrency/InterestCents is :34E:, the total
	// contracted interest.
	PrincipalCurrency string
	PrincipalCents    int64
	InterestCurrency  string
	InterestCents     int64
	// Rate is the :37G: rate as written (comma decimal
	// separator).
	Rate string
	// DayCount is :14D:: always ACT/365 on a message ParseMT320
	// accepts.
	DayCount string
	// ReceivingBank is the :57A: BIC or :57D: account ID of
	// sequence C, the bank receiving Party A's principal.
	ReceivingBank    string
	ReceivingBankBIC bool
	// PayingBank is the :57A: BIC or :57D: account ID of
	// sequence D, the bank receiving Party B's
	// principal-plus-interest.
	PayingBank    string
	PayingBankBIC bool
}

// MT320ApplyResult is the outcome of ApplyMT320.
type MT320ApplyResult struct {
	// Deposit is the matched term deposit contract (or the
	// current one on the duplicate path).
	Deposit *TermDeposit `json:"deposit"`
	// CommonReference echoes the request's :22C: common
	// reference, the key the confirmation was verified by.
	CommonReference string `json:"common_ref"`
	// Duplicate reports whether the confirmation was already
	// applied under the same :20: with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt320AppliedRecord is the replay record for one applied MT320
// confirmation, keyed by :20: in mt320Applied: the deposit it
// matched and every term it carried, plus the deposit snapshot
// at application, so a replay returns the same outcome without
// matching twice.
type mt320AppliedRecord struct {
	Reference         string
	CommonReference   string
	DepositID         string
	TradeDate         time.Time
	ValueDate         time.Time
	MaturityDate      time.Time
	PrincipalCurrency string
	PrincipalCents    int64
	InterestCurrency  string
	InterestCents     int64
	Rate              string
	Deposit           *TermDeposit
}

// mt320ConfirmationRecord is the read-only confirmation marker
// for one deposit, keyed by deposit ID in mt320Confirmed: the
// :20: reference and :22C: common reference of the counterparty
// confirmation that matched it. It carries no lifecycle effect:
// the contract stays active (or matured), and MatureTermDeposit
// remains the only settlement entry.
type mt320ConfirmationRecord struct {
	DepositID       string
	Reference       string
	CommonReference string
}

// mt320TermsEqual reports whether a stored confirmation record
// carries exactly the terms an inbound request would apply: the
// same deposit, the same common reference, the same trade, value,
// and maturity dates (by calendar day, as :30T: / :30V: / :30P:
// carry only a day), the same rate (by scaled value, so "0,05"
// and "0,050" are the same term), and the same principal and
// contracted-interest amounts.
func mt320TermsEqual(rec mt320AppliedRecord, req MT320Confirmation) bool {
	if rec.DepositID == "" {
		return false
	}
	if rec.CommonReference != req.CommonReference {
		return false
	}
	if mt320Day(rec.TradeDate) != mt320Day(req.TradeDate) ||
		mt320Day(rec.ValueDate) != mt320Day(req.ValueDate) ||
		mt320Day(rec.MaturityDate) != mt320Day(req.MaturityDate) {
		return false
	}
	wantRate, err := mt320ParseRateScaled(rec.Rate)
	if err != nil {
		return false
	}
	gotRate, err := mt320ParseRateScaled(req.Rate)
	if err != nil {
		return false
	}
	if wantRate.Cmp(gotRate) != 0 {
		return false
	}
	return rec.PrincipalCurrency == req.PrincipalCurrency &&
		rec.PrincipalCents == req.PrincipalCents &&
		rec.InterestCurrency == req.InterestCurrency &&
		rec.InterestCents == req.InterestCents
}

// mt320Reference validates a :20: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT320 error identity.
func mt320Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT320Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT320Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT320Reference, field, ref)
	}
	return nil
}

// mt320CommonReferenceShape validates a :22C: common reference:
// 4!a2!c4!n4!a2!c — 4 letters, 2 alphanumeric characters, 4
// digits, 4 letters, 2 alphanumeric characters.
func mt320CommonReferenceShape(ref string) error {
	if len(ref) != 16 {
		return fmt.Errorf("%w: :22C: want 4!a2!c4!n4!a2!c (16 characters), got %q", ErrMT320Reference, ref)
	}
	for i := 0; i < 4; i++ {
		if ref[i] < 'A' || ref[i] > 'Z' {
			return fmt.Errorf("%w: :22C: want 4 letters first, got %q", ErrMT320Reference, ref)
		}
	}
	for i := 4; i < 6; i++ {
		c := ref[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: :22C: want 2 alphanumeric characters next, got %q", ErrMT320Reference, ref)
		}
	}
	for i := 6; i < 10; i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return fmt.Errorf("%w: :22C: want 4 digits next, got %q", ErrMT320Reference, ref)
		}
	}
	for i := 10; i < 14; i++ {
		if ref[i] < 'A' || ref[i] > 'Z' {
			return fmt.Errorf("%w: :22C: want 4 letters next, got %q", ErrMT320Reference, ref)
		}
	}
	for i := 14; i < 16; i++ {
		c := ref[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: :22C: want 2 alphanumeric characters last, got %q", ErrMT320Reference, ref)
		}
	}
	return nil
}

// mt320Currency validates a :32B: / :34E: currency code: exactly
// three uppercase ASCII letters. The MT202 rule, under the
// MT320 error identity.
func mt320Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT320Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT320Currency, code)
		}
	}
	return nil
}

// mt320ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I320<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT320 error identity.
func mt320ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT320Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT320Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT320Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT320LT, lt)
	}
	return lt, nil
}

// mt320ParsePartyField parses a party line carrying either the A
// or the D option of tag (":82:", ":87:", or ":57:"): the line
// must start with tag+"A:" or tag+"D:", and the body must pass
// the form's check — a BIC for A, the SWIFT `x` set for D.
func mt320ParsePartyField(line, tag, field string) (party string, isBIC bool, err error) {
	for _, opt := range []struct {
		suffix string
		isBIC  bool
	}{
		{"A:", true}, {"D:", false},
	} {
		prefix := tag + opt.suffix
		if strings.HasPrefix(line, prefix) {
			body := line[len(prefix):]
			if body == "" {
				return "", false, fmt.Errorf("%w: %s field is empty", ErrMT320Field, field)
			}
			if opt.isBIC {
				if !validBIC(body) {
					return "", false, fmt.Errorf("%w: %s: %q is not a valid BIC", ErrMT320Field, field, body)
				}
			} else if err := mt103CheckX(body); err != nil {
				return "", false, fmt.Errorf("%w: %s: %w", ErrMT320Field, field, err)
			}
			return body, opt.isBIC, nil
		}
	}
	return "", false, fmt.Errorf("%w: want %sA: or %sD: field, got %q", ErrMT320Field, tag, tag, line)
}

// mt320ParseRateScaled parses a :37G: rate body ("<integer>,<1-12
// fraction digits>") into its value scaled by 10^12, exact
// big.Int arithmetic, never float. The integer part holds only
// digits; the fraction part is right-padded with zeros to 12
// places. The value must be positive.
func mt320ParseRateScaled(body string) (*big.Int, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: want <integer>,<fraction digits>, got %q", ErrMT320Rate, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) > 12 {
		return nil, fmt.Errorf("%w: %q", ErrMT320Rate, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %q", ErrMT320Rate, body)
		}
	}
	scaled, ok := new(big.Int).SetString(intPart+fracPart+strings.Repeat("0", 12-len(fracPart)), 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMT320Rate, body)
	}
	if scaled.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %q is not positive", ErrMT320Rate, body)
	}
	return scaled, nil
}

// mt320RateScaled renders RateBps/10000 scaled by 10^12 with the
// mt320RateBps half-up rounding at the 12th decimal place, exact
// big.Int arithmetic: scaled = (2*bps*10^12 + 10000) / (2*10000)
// for a positive rate.
func mt320RateScaled(bps int64) *big.Int {
	const scale = 1000000000000 // 10^12
	const den = 10000
	n := big.NewInt(bps)
	d := big.NewInt(den)
	scaled := new(big.Int).Mul(n, big.NewInt(scale))
	scaled.Mul(scaled, big.NewInt(2))
	scaled.Add(scaled, d)
	return scaled.Quo(scaled, new(big.Int).Mul(d, big.NewInt(2)))
}

// mt320ParseAmountField parses a :32B: / :34E: body (3-letter
// currency + `<integer>,<2 fraction digits>`) into its currency
// and minor-unit amount. The 15d amount rule rides
// mt202ParseAmount, under the MT320 error identity.
func mt320ParseAmountField(body, field string) (currency string, cents int64, err error) {
	if len(body) < 3+4 {
		return "", 0, fmt.Errorf("%w: %s too short: %q", ErrMT320Framing, field, body)
	}
	currency = body[:3]
	if err := mt320Currency(currency); err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}
	amount, err := mt202ParseAmount(body[3:])
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w: %v", field, ErrMT320Amount, err)
	}
	return currency, amount, nil
}

// mt320ParseDate parses a :30T: / :30V: / :30P: YYYYMMDD body into
// a UTC calendar date.
func mt320ParseDate(body, field string) (time.Time, error) {
	if len(body) != 8 {
		return time.Time{}, fmt.Errorf("%w: %s want YYYYMMDD, got %q", ErrMT320ValueDate, field, body)
	}
	d, err := time.Parse("20060102", body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q: %w", ErrMT320ValueDate, field, body, err)
	}
	return d.UTC(), nil
}

// mt320Day renders a time as its YYYYMMDD calendar day (UTC), the
// :30T: / :30V: / :30P: form, so a parsed date compares against a
// stored timestamp by day, never by instant.
func mt320Day(t time.Time) string {
	return t.UTC().Format("20060102")
}

// ParseMT320 parses a SWIFT FIN MT320 message produced by
// ExportMT320 (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the twenty-field core set is refused — a confirmation
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated.
func ParseMT320(text string) (MT320Confirmation, error) {
	var zero MT320Confirmation
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 25 {
		return zero, fmt.Errorf("%w: want 25 lines (3 headers + {4: + 20 fields + -}), got %d", ErrMT320Framing, len(lines))
	}
	senderLT, err := mt320ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt320ParseLT(lines[1], "{2:I320", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT320Framing, mur)
	}
	if err := mt320Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT320Framing, lines[3])
	}
	if lines[24] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT320Framing, lines[24])
	}
	// Sequence markers carry no body.
	if lines[4] != ":15A:" {
		return zero, fmt.Errorf("%w: line 5: want :15A: field, got %q", ErrMT320Field, lines[4])
	}
	if lines[11] != ":15B:" {
		return zero, fmt.Errorf("%w: line 12: want :15B: field, got %q", ErrMT320Field, lines[11])
	}
	if lines[20] != ":15C:" {
		return zero, fmt.Errorf("%w: line 21: want :15C: field, got %q", ErrMT320Field, lines[20])
	}
	if lines[22] != ":15D:" {
		return zero, fmt.Errorf("%w: line 23: want :15D: field, got %q", ErrMT320Field, lines[22])
	}
	var m MT320Confirmation
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20: — the deposit ID, the addressing key and replay key.
	if !strings.HasPrefix(lines[5], ":20:") {
		return zero, fmt.Errorf("%w: line 6: want :20: field, got %q", ErrMT320Field, lines[5])
	}
	if err := mt320Reference(lines[5][len(":20:"):], ":20:"); err != nil {
		return zero, err
	}
	m.Reference = lines[5][len(":20:"):]
	// :22A: — only NEWT is accepted: this endpoint confirms new
	// contracts only.
	if lines[6] != ":22A:NEWT" {
		if !strings.HasPrefix(lines[6], ":22A:") {
			return zero, fmt.Errorf("%w: line 7: want :22A: field, got %q", ErrMT320Field, lines[6])
		}
		return zero, fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT320Field, lines[6][len(":22A:"):])
	}
	m.Variant = "NEWT"
	// :22B: — only CONF is accepted: this endpoint confirms new
	// contracts only.
	if lines[7] != ":22B:CONF" {
		if !strings.HasPrefix(lines[7], ":22B:") {
			return zero, fmt.Errorf("%w: line 8: want :22B: field, got %q", ErrMT320Field, lines[7])
		}
		return zero, fmt.Errorf("%w: :22B: only CONF is accepted, got %q", ErrMT320Field, lines[7][len(":22B:"):])
	}
	m.ConfirmationType = "CONF"
	// :22C: — the common reference, the verification key.
	if !strings.HasPrefix(lines[8], ":22C:") {
		return zero, fmt.Errorf("%w: line 9: want :22C: field, got %q", ErrMT320Field, lines[8])
	}
	if err := mt320CommonReferenceShape(lines[8][len(":22C:"):]); err != nil {
		return zero, err
	}
	m.CommonReference = lines[8][len(":22C:"):]
	// :82a: / :87a: — the parties, BIC or account-ID form.
	depositor, depositorBIC, err := mt320ParsePartyField(lines[9], ":82", ":82a:")
	if err != nil {
		return zero, fmt.Errorf("line 10: %w", err)
	}
	m.Depositor, m.DepositorBIC = depositor, depositorBIC
	counterparty, counterpartyBIC, err := mt320ParsePartyField(lines[10], ":87", ":87a:")
	if err != nil {
		return zero, fmt.Errorf("line 11: %w", err)
	}
	m.Counterparty, m.CounterpartyBIC = counterparty, counterpartyBIC
	// :17R: — only L is accepted: the depositor is the lender.
	if lines[12] != ":17R:L" {
		if !strings.HasPrefix(lines[12], ":17R:") {
			return zero, fmt.Errorf("%w: line 13: want :17R: field, got %q", ErrMT320Field, lines[12])
		}
		return zero, fmt.Errorf("%w: :17R: only L is accepted, got %q", ErrMT320Field, lines[12][len(":17R:"):])
	}
	m.Role = "L"
	// :30T: / :30V: / :30P: — trade, value, and maturity dates.
	tradeBody, err := mt320FieldBody(lines[13], ":30T:", 14)
	if err != nil {
		return zero, err
	}
	tradeDate, err := mt320ParseDate(tradeBody, ":30T:")
	if err != nil {
		return zero, err
	}
	m.TradeDate = tradeDate
	valueBody, err := mt320FieldBody(lines[14], ":30V:", 15)
	if err != nil {
		return zero, err
	}
	valueDate, err := mt320ParseDate(valueBody, ":30V:")
	if err != nil {
		return zero, err
	}
	m.ValueDate = valueDate
	maturityBody, err := mt320FieldBody(lines[15], ":30P:", 16)
	if err != nil {
		return zero, err
	}
	maturityDate, err := mt320ParseDate(maturityBody, ":30P:")
	if err != nil {
		return zero, err
	}
	m.MaturityDate = maturityDate
	// :32B: / :34E: — the principal and the total contracted
	// interest.
	principalBody, err := mt320FieldBody(lines[16], ":32B:", 17)
	if err != nil {
		return zero, err
	}
	principalCcy, principal, err := mt320ParseAmountField(principalBody, ":32B:")
	if err != nil {
		return zero, err
	}
	m.PrincipalCurrency, m.PrincipalCents = principalCcy, principal
	interestBody, err := mt320FieldBody(lines[17], ":34E:", 18)
	if err != nil {
		return zero, err
	}
	interestCcy, interest, err := mt320ParseAmountField(interestBody, ":34E:")
	if err != nil {
		return zero, err
	}
	m.InterestCurrency, m.InterestCents = interestCcy, interest
	// :37G: — the rate, exactly as written; validated as a
	// positive decimal with 1–12 fraction digits.
	rateBody, err := mt320FieldBody(lines[18], ":37G:", 19)
	if err != nil {
		return zero, err
	}
	if _, err := mt320ParseRateScaled(rateBody); err != nil {
		return zero, err
	}
	m.Rate = rateBody
	// :14D: — only ACT/365 is accepted.
	if lines[19] != ":14D:ACT/365" {
		if !strings.HasPrefix(lines[19], ":14D:") {
			return zero, fmt.Errorf("%w: line 20: want :14D: field, got %q", ErrMT320Field, lines[19])
		}
		return zero, fmt.Errorf("%w: :14D: only ACT/365 is accepted, got %q", ErrMT320Field, lines[19][len(":14D:"):])
	}
	m.DayCount = "ACT/365"
	// :57a: — the settlement legs: sequence C the bank receiving
	// Party A's principal, sequence D the bank receiving Party
	// B's principal-plus-interest.
	receivingBank, receivingBIC, err := mt320ParsePartyField(lines[21], ":57", ":57a:")
	if err != nil {
		return zero, fmt.Errorf("line 22: %w", err)
	}
	m.ReceivingBank, m.ReceivingBankBIC = receivingBank, receivingBIC
	payingBank, payingBIC, err := mt320ParsePartyField(lines[23], ":57", ":57a:")
	if err != nil {
		return zero, fmt.Errorf("line 24: %w", err)
	}
	m.PayingBank, m.PayingBankBIC = payingBank, payingBIC
	return m, nil
}

// mt320FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt320FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT320Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT320Field, tag)
	}
	return body, nil
}

// validateMT320Confirmation fail-closes on a confirmation
// ParseMT320 would refuse, so ApplyMT320 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT320Confirmation(req MT320Confirmation) error {
	if err := mt320Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if req.Variant != "NEWT" {
		return fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT320Field, req.Variant)
	}
	if req.ConfirmationType != "CONF" {
		return fmt.Errorf("%w: :22B: only CONF is accepted, got %q", ErrMT320Field, req.ConfirmationType)
	}
	if err := mt320CommonReferenceShape(req.CommonReference); err != nil {
		return err
	}
	if req.Role != "L" {
		return fmt.Errorf("%w: :17R: only L is accepted, got %q", ErrMT320Field, req.Role)
	}
	if req.TradeDate.IsZero() {
		return fmt.Errorf("%w: :30T: trade date is required", ErrMT320ValueDate)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: :30V: value date is required", ErrMT320ValueDate)
	}
	if req.MaturityDate.IsZero() {
		return fmt.Errorf("%w: :30P: maturity date is required", ErrMT320ValueDate)
	}
	if _, err := mt320ParseRateScaled(req.Rate); err != nil {
		return err
	}
	if err := mt320Currency(req.PrincipalCurrency); err != nil {
		return fmt.Errorf(":32B:: %w", err)
	}
	if req.PrincipalCents <= 0 {
		return fmt.Errorf("%w: :32B: %d is not positive", ErrMT320Amount, req.PrincipalCents)
	}
	if err := mt320Currency(req.InterestCurrency); err != nil {
		return fmt.Errorf(":34E:: %w", err)
	}
	if req.InterestCents <= 0 {
		return fmt.Errorf("%w: :34E: %d is not positive", ErrMT320Amount, req.InterestCents)
	}
	if req.DayCount != "ACT/365" {
		return fmt.Errorf("%w: :14D: only ACT/365 is accepted, got %q", ErrMT320Field, req.DayCount)
	}
	return nil
}

// ApplyMT320 applies one parsed inbound SWIFT FIN MT320 fixed
// loan/deposit confirmation: it matches the confirmation against
// the booked term deposit the :20: reference names and records
// the match.
//
// The deposit must exist (:20: unknown is ErrTermDepositNotFound,
// fail-closed — a confirmation never opens a deposit) and be
// active or matured (an early-withdrawn deposit reports
// ErrTermDepositNotActive, the export's gate). Every economic term
// must agree exactly with the booked contract — the :22C: common
// reference against mt320CommonReference recomputed from the
// stored depositor identifier, the message's :87A: counterparty
// BIC, and the :20: deposit ID; the :30T:/:30V: dates against the
// deposit's OpenedAt value date and the :30P: date against
// MaturesAt; the :32B: principal against AmountCents; the :34E:
// contracted interest total against TermDays x one day's interest
// (big-number exact); and the :37G: rate against RateBps (half-up
// rounded to 12 decimal places) — or the whole confirmation is
// refused with ErrMT320TermsMismatch: there is no partial
// confirmation. The :82a: depositor must name the stored
// depositor in the same form, the :87a: counterparty must be a
// BIC, and the :57a: settlement legs must name the same banks as
// the parties. No settlement runs, no deposit is created, and the
// contract's lifecycle does not move: the match is recorded in
// the mt320Applied :20: registry and the mt320Confirmed deposit
// marker, and one `mt320_confirmation_matched` audit event
// (details msg_id / common_ref / deposit_id) is emitted.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same deposit, dates, rate, and
// amounts returns the current deposit with Duplicate set,
// recording nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT320MsgIDConflict.
//
// Locking: the whole apply runs under the write lock — the
// replay read, the deposit lookup, the term verification, the
// registries, and the audit event land atomically, so two
// concurrent applies of the same :20: still emit exactly one
// marker event.
func (l *Ledger) ApplyMT320(req MT320Confirmation, actor string) (MT320ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT320"
	}
	if err := validateMT320Confirmation(req); err != nil {
		return MT320ApplyResult{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// :20: replay: the confirmation was already applied under
	// this reference. Same terms is a no-op returning the
	// current deposit; different terms is a conflict.
	if rec, ok := l.mt320Applied[req.Reference]; ok {
		if !mt320TermsEqual(rec, req) {
			return MT320ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT320MsgIDConflict, req.Reference)
		}
		if current, ok := l.termDeposits[rec.DepositID]; ok {
			snap := current
			return MT320ApplyResult{Deposit: &snap, CommonReference: req.CommonReference, Duplicate: true}, nil
		}
		return MT320ApplyResult{Deposit: rec.Deposit, CommonReference: req.CommonReference, Duplicate: true}, nil
	}

	// The deposit itself: :20: names it. Unknown is
	// fail-closed — a confirmation never opens a deposit.
	td, ok := l.termDeposits[req.Reference]
	if !ok {
		return MT320ApplyResult{}, fmt.Errorf("%w: no term deposit with ID %q", ErrTermDepositNotFound, req.Reference)
	}
	switch td.Status {
	case TermDepositActive, TermDepositMatured:
		// Confirmable.
	case TermDepositWithdrawnEarly:
		return MT320ApplyResult{}, fmt.Errorf("%w: term deposit %q was withdrawn early", ErrTermDepositNotActive, req.Reference)
	default:
		return MT320ApplyResult{}, fmt.Errorf("%w: term deposit %q is %s", ErrTermDepositNotActive, req.Reference, td.Status)
	}
	// A tiered-rate deposit has no single rate to state in
	// :37G:: the confirmation cannot express a ladder, so it
	// fails loud instead of picking one band's rate.
	if len(td.RateTiers) > 0 {
		return MT320ApplyResult{}, fmt.Errorf("%w: term deposit %q carries a tiered rate ladder, no single :37G: exists", ErrInvalidMT320Export, req.Reference)
	}

	// The parties: the :82a: depositor must name the stored
	// depositor in the same form, the :87a: counterparty must be
	// a BIC (the export always renders :87A:), and the :57a:
	// settlement legs must name the same banks as the parties —
	// sequence C the counterparty, sequence D the depositor.
	depositorID, depositorBIC := l.mt300PartyIdentifierLocked(td.Account)
	if req.Depositor != depositorID || req.DepositorBIC != depositorBIC {
		return MT320ApplyResult{}, fmt.Errorf("%w: :82a: holds %q, deposit names %q", ErrMT320TermsMismatch, req.Depositor, depositorID)
	}
	if !req.CounterpartyBIC {
		return MT320ApplyResult{}, fmt.Errorf("%w: :87a: must be a BIC (:87A:), got %q", ErrMT320TermsMismatch, req.Counterparty)
	}
	if req.ReceivingBank != req.Counterparty || !req.ReceivingBankBIC {
		return MT320ApplyResult{}, fmt.Errorf("%w: :57a: in sequence C holds %q, counterparty is %q", ErrMT320TermsMismatch, req.ReceivingBank, req.Counterparty)
	}
	if req.PayingBank != depositorID || req.PayingBankBIC != depositorBIC {
		return MT320ApplyResult{}, fmt.Errorf("%w: :57a: in sequence D holds %q, depositor is %q", ErrMT320TermsMismatch, req.PayingBank, depositorID)
	}

	// Every economic term, exactly: the :22C: common reference
	// against the stored derivation, the :30T:/:30V: dates
	// against the opening day and the :30P: date against the
	// maturity day, the :32B: principal against the stored
	// principal, the :34E: contracted interest total against
	// TermDays x one day's interest, and the :37G: rate against
	// the locked rate. Any disagreement refuses the whole
	// confirmation.
	wantCommonRef, err := mt320CommonReference(depositorID, req.Counterparty, req.Reference)
	if err != nil {
		return MT320ApplyResult{}, err
	}
	if req.CommonReference != wantCommonRef {
		return MT320ApplyResult{}, fmt.Errorf("%w: :22C: holds %q, deposit derives %q", ErrMT320TermsMismatch, req.CommonReference, wantCommonRef)
	}
	if mt320Day(req.TradeDate) != mt320Day(td.OpenedAt) {
		return MT320ApplyResult{}, fmt.Errorf("%w: :30T: holds %q, deposit opened %q",
			ErrMT320TermsMismatch, mt320Day(req.TradeDate), mt320Day(td.OpenedAt))
	}
	if mt320Day(req.ValueDate) != mt320Day(td.OpenedAt) {
		return MT320ApplyResult{}, fmt.Errorf("%w: :30V: holds %q, deposit opened %q",
			ErrMT320TermsMismatch, mt320Day(req.ValueDate), mt320Day(td.OpenedAt))
	}
	if mt320Day(req.MaturityDate) != mt320Day(td.MaturesAt) {
		return MT320ApplyResult{}, fmt.Errorf("%w: :30P: holds %q, deposit matures %q",
			ErrMT320TermsMismatch, mt320Day(req.MaturityDate), mt320Day(td.MaturesAt))
	}
	if req.PrincipalCurrency != td.Currency {
		return MT320ApplyResult{}, fmt.Errorf("%w: :32B: holds currency %q, deposit is %q",
			ErrMT320TermsMismatch, req.PrincipalCurrency, td.Currency)
	}
	if req.PrincipalCents != td.AmountCents {
		return MT320ApplyResult{}, fmt.Errorf("%w: :32B: holds %d cents, deposit principal is %d cents",
			ErrMT320TermsMismatch, req.PrincipalCents, td.AmountCents)
	}
	if req.InterestCurrency != td.Currency {
		return MT320ApplyResult{}, fmt.Errorf("%w: :34E: holds currency %q, deposit is %q",
			ErrMT320TermsMismatch, req.InterestCurrency, td.Currency)
	}
	daily, err := termDepositDailyInterestCents(td)
	if err != nil {
		return MT320ApplyResult{}, fmt.Errorf("%w: daily interest for term deposit %q: %w", ErrMT320TermsMismatch, td.ID, err)
	}
	wantInterest := new(big.Int).Mul(big.NewInt(daily), big.NewInt(td.TermDays))
	if new(big.Int).SetInt64(req.InterestCents).Cmp(wantInterest) != 0 {
		return MT320ApplyResult{}, fmt.Errorf("%w: :34E: holds %d cents, deposit contracts %s cents",
			ErrMT320TermsMismatch, req.InterestCents, wantInterest.String())
	}
	gotRate, err := mt320ParseRateScaled(req.Rate)
	if err != nil {
		return MT320ApplyResult{}, err
	}
	if gotRate.Cmp(mt320RateScaled(td.RateBps)) != 0 {
		want, _ := mt320RateBps(td.RateBps)
		return MT320ApplyResult{}, fmt.Errorf("%w: :37G: holds %q, deposit locks %q", ErrMT320TermsMismatch, req.Rate, want)
	}

	// The match: the :20: record, the deposit's confirmation
	// marker, and one audit event, under the one write lock.
	// The confirmation is already verified when the marker
	// lands.
	snap := td
	if l.mt320Applied == nil {
		l.mt320Applied = make(map[string]mt320AppliedRecord)
	}
	l.mt320Applied[req.Reference] = mt320AppliedRecord{
		Reference:         req.Reference,
		CommonReference:   req.CommonReference,
		DepositID:         td.ID,
		TradeDate:         req.TradeDate,
		ValueDate:         req.ValueDate,
		MaturityDate:      req.MaturityDate,
		PrincipalCurrency: req.PrincipalCurrency,
		PrincipalCents:    req.PrincipalCents,
		InterestCurrency:  req.InterestCurrency,
		InterestCents:     req.InterestCents,
		Rate:              req.Rate,
		Deposit:           &snap,
	}
	if l.mt320Confirmed == nil {
		l.mt320Confirmed = make(map[string]mt320ConfirmationRecord)
	}
	l.mt320Confirmed[td.ID] = mt320ConfirmationRecord{
		DepositID:       td.ID,
		Reference:       req.Reference,
		CommonReference: req.CommonReference,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt320_confirmation_matched",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"msg_id":     req.Reference,
			"common_ref": req.CommonReference,
			"deposit_id": td.ID,
		},
	})
	return MT320ApplyResult{Deposit: &snap, CommonReference: req.CommonReference}, nil
}

// DepositConfirmed reports whether the term deposit has a matched
// inbound MT320 confirmation, and the confirming :20: reference
// when it does. The marker is read-only: it moves no lifecycle
// state.
func (l *Ledger) DepositConfirmed(depositID string) (reference string, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt320Confirmed[depositID]
	if !ok {
		return "", false
	}
	return rec.Reference, true
}
