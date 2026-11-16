package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN MT300 FX forward confirmation inbound processing
// (LG-440): the inbound half of the MT300 lifecycle, the mirror
// of the LG-388 export.
//
// ExportMT300 renders this ledger's own confirmation — the ledger
// telling the counterparty "this booked forward is confirmed".
// This module consumes the confirmation the counterparty sent to
// this ledger, acting as the other side of the trade: it matches
// the confirmation against the booked forward the :22C: common
// reference names, and records the match — and nothing else. The
// two directions are orthogonal by construction:
//
//   - LG-388 exports: the ledger confirms one of its own booked
//     forwards (outbound, read-only).
//   - LG-440 imports: the counterparty confirms the same trade
//     back (inbound, read-only apart from the confirmation
//     marker: no settlement, no new forward, no lifecycle
//     transition — SettleForward remains the only settlement
//     entry, LG-110).
//
// ParseMT300 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT202 (LG-204): the block
// structure, the field order, and every field value are
// validated, and anything outside the thirteen-field core set
// (:15A: :20: :22A: :22C: :82a: :87a: :15B: :30T: :30V: :36:
// :32B: :33B: :57a:) is refused — a confirmation must not lose
// content quietly. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated. The :82a: /
// :87a: / :57a: `a` option means "BIC or name-and-address": this
// module implements the A (BIC) variant and the D (account-ID)
// variant the export renders, and refuses any other option
// fail-closed rather than silently dropping the party identity.
//
// ApplyMT300 resolves the forward by the :22C: common reference
// — the deterministic derivation ExportMT300 renders (first 4 of
// the From identifier + first 2 of the To identifier + the last
// 4 digits of the forward ID) — and verifies every economic term
// exactly against the booked contract: the :36: rate against the
// locked RateNum/RateDen (big-number exact after half-up rounding
// to 12 decimal places, the mt300Rate rule), the :32B: bought
// amount against floor(NotionalCents*RateNum/RateDen), the :33B:
// sold amount against NotionalCents, and the :30V: value date
// against the forward's SettleAt value date. An unknown forward
// reports ErrForwardNotFound, fail-closed — a confirmation never
// books a forward. Any mismatch reports ErrMT300TermsMismatch
// and the whole confirmation is refused: there is no partial
// confirmation. A booked or settled forward can be confirmed; a
// cancelled one reports ErrForwardNotBooked (or ErrForwardExpired
// when it lapsed), mirroring the export's gate.
//
// Only :22A: NEWT is accepted: this endpoint confirms new trades
// only (AMND/CANC variants are a later round, the export's rule).
// The :30T: trade date is parsed and validated as a calendar date
// but not compared — the spec's verification list is the rate,
// the two amounts, and the :30V: value date.
//
// A matched confirmation is recorded twice, both read-only with
// respect to the contract: the mt300Applied :20: registry (the
// replay guard, the pain011Applied pattern) and the
// mt300Confirmed marker (forward ID → confirming :20:), so a
// forward matched once reads as confirmed without any lifecycle
// effect. Success emits `mt300_confirmation_matched` (details
// msg_id / common_ref / forward_id, actor `ApplyMT300`).
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// forward, dates, rate, and amounts returns the current forward
// with Duplicate set, recording nothing twice and emitting no
// second audit event. A :20: reused for different terms is
// refused with ErrMT300MsgIDConflict — never silently
// overwritten. The registry check runs under the write lock, so
// two concurrent applies of the same :20: still emit exactly
// one marker event.
//
// No forward is ever auto-created, no settlement runs here: the
// confirmation is matched first, and settlement still arrives
// separately through SettleForward. One MT300 confirms exactly
// one forward — no batch confirmation.

// MT300 inbound errors.
var (
	// ErrMT300Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :15A: → :20: → :22A: → :22C: → :82a: → :87a:
	// → :15B: → :30T: → :30V: → :36: → :32B: → :33B: → :57a:
	// order.
	ErrMT300Framing = errors.New("ledger: mt300: malformed FIN framing")
	// ErrMT300Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a field
	// outside the thirteen-field core set, a non-empty :15A: /
	// :15B: sequence marker, or a :22A: other than NEWT.
	ErrMT300Field = errors.New("ledger: mt300: unexpected or missing field")
	// ErrMT300Reference is returned for an invalid :20: or :22C:
	// reference: empty, over 16 characters (for :20:), outside
	// the SWIFT `x` character set, with a leading/trailing colon
	// or slash, or a double slash — or a :22C: outside the
	// 4!a2!c4!n shape.
	ErrMT300Reference = errors.New("ledger: mt300: invalid reference")
	// ErrMT300ValueDate is returned for an invalid :30T: or :30V:
	// value date: not a YYMMDD calendar date.
	ErrMT300ValueDate = errors.New("ledger: mt300: invalid :30T:/:30V: value date")
	// ErrMT300Currency is returned for an invalid :32B: or :33B:
	// currency: not exactly three uppercase ASCII letters (the
	// LG-18 currency rule — no lowercase, no digits, no ISO
	// lookup table the ledger does not own).
	ErrMT300Currency = errors.New("ledger: mt300: invalid :32B:/:33B: currency")
	// ErrMT300Amount is returned for an invalid :32B: or :33B:
	// amount: non-positive, not in `<integer>,<2 fraction
	// digits>` form, or with more than 12 integer digits (the
	// MT103 15d rule).
	ErrMT300Amount = errors.New("ledger: mt300: invalid :32B:/:33B: amount")
	// ErrMT300Rate is returned for an invalid :36: rate: not a
	// positive decimal with a comma separator and 1–12 fraction
	// digits.
	ErrMT300Rate = errors.New("ledger: mt300: invalid :36: rate")
	// ErrMT300LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT300LT = errors.New("ledger: mt300: invalid SWIFT LT address")
	// ErrMT300MsgIDConflict is returned by ApplyMT300 when a
	// :20: reference already applied carries different terms: the
	// reference names one confirmation, and a second, different
	// confirmation under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT300MsgIDConflict = errors.New("ledger: mt300 inbound: message reference already applied with different terms")
	// ErrMT300TermsMismatch is returned by ApplyMT300 when the
	// confirmation's economic terms disagree with the booked
	// forward — the rate, either amount, or the :30V: value date
	// — or when the :22C: common reference matches more than one
	// forward: the confirmation points at another trade, so the
	// whole confirmation is refused. It is a 422-class error.
	ErrMT300TermsMismatch = errors.New("ledger: mt300 inbound: confirmation terms do not match the booked forward")
)

// MT300Confirmation is one parsed inbound SWIFT FIN MT300 FX
// forward confirmation: the block-4 core fields plus the block
// 1/2 LT addresses. TradeDate and ValueDate are UTC calendar dates
// (time-of-day is not carried by :30T:/:30V:); BoughtCents and
// SoldCents are the :32B:/:33B: amounts in minor units, always
// positive; Rate is the :36: rate exactly as written.
type MT300Confirmation struct {
	SenderLT   string
	ReceiverLT string
	// Reference is :20:, this message's own reference, the replay
	// key — the forward ID on a confirmation this ledger's
	// export rendered.
	Reference string
	// Variant is :22A:: always NEWT on a message ParseMT300
	// accepts.
	Variant string
	// CommonReference is :22C:, the addressing key: first 4 of
	// the From identifier + first 2 of the To identifier + the
	// last 4 digits of the forward ID.
	CommonReference string
	// SenderParty is the :82A: BIC or :82D: account ID; the BIC
	// flag reports which form it is.
	SenderParty    string
	SenderPartyBIC bool
	// ReceiverParty is the :87A: BIC or :87D: account ID.
	ReceiverParty    string
	ReceiverPartyBIC bool
	// TradeDate is :30T:, the trade date; ValueDate is :30V:,
	// the value date, which must equal the forward's SettleAt
	// value date.
	TradeDate time.Time
	ValueDate time.Time
	// Rate is the :36: exchange rate as written (comma decimal
	// separator).
	Rate string
	// BoughtCurrency/BoughtCents is :32B:, the bought amount;
	// SoldCurrency/SoldCents is :33B:, the sold amount.
	BoughtCurrency string
	BoughtCents    int64
	SoldCurrency   string
	SoldCents      int64
	// AccountParty is the :57A: BIC or :57D: account ID naming
	// the account the bought currency lands in.
	AccountParty    string
	AccountPartyBIC bool
}

// MT300ApplyResult is the outcome of ApplyMT300.
type MT300ApplyResult struct {
	// Forward is the matched forward contract (or the current
	// one on the duplicate path).
	Forward *FXForward `json:"forward"`
	// CommonReference echoes the request's :22C: common
	// reference, the key the forward was resolved by.
	CommonReference string `json:"common_ref"`
	// Duplicate reports whether the confirmation was already
	// applied under the same :20: with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt300AppliedRecord is the replay record for one applied MT300
// confirmation, keyed by :20: in mt300Applied: the forward it
// matched and every term it carried, plus the forward snapshot
// at application, so a replay returns the same outcome without
// matching twice.
type mt300AppliedRecord struct {
	Reference       string
	CommonReference string
	ForwardID       string
	TradeDate       time.Time
	ValueDate       time.Time
	Rate            string
	BoughtCurrency  string
	BoughtCents     int64
	SoldCurrency    string
	SoldCents       int64
	Forward         *FXForward
}

// mt300ConfirmationRecord is the read-only confirmation marker
// for one forward, keyed by forward ID in mt300Confirmed: the
// :20: reference and :22C: common reference of the counterparty
// confirmation that matched it. It carries no lifecycle effect:
// the contract stays booked (or settled), and SettleForward
// remains the only settlement entry.
type mt300ConfirmationRecord struct {
	ForwardID       string
	Reference       string
	CommonReference string
}

// mt300TermsEqual reports whether a stored confirmation record
// carries exactly the terms an inbound request would apply: the
// same forward, the same common reference, the same trade and
// value dates (by calendar day, as :30T:/:30V: carry only a
// day), the same rate (by scaled value, so "1,1" and "1,10" are
// the same term), and the same bought and sold amounts.
func mt300TermsEqual(rec mt300AppliedRecord, req MT300Confirmation) bool {
	if rec.ForwardID == "" {
		return false
	}
	if rec.CommonReference != req.CommonReference {
		return false
	}
	if mt103ValueDate(rec.TradeDate) != mt103ValueDate(req.TradeDate) ||
		mt103ValueDate(rec.ValueDate) != mt103ValueDate(req.ValueDate) {
		return false
	}
	wantRate, err := mt300ParseRateScaled(rec.Rate)
	if err != nil {
		return false
	}
	gotRate, err := mt300ParseRateScaled(req.Rate)
	if err != nil {
		return false
	}
	if wantRate.Cmp(gotRate) != 0 {
		return false
	}
	return rec.BoughtCurrency == req.BoughtCurrency &&
		rec.BoughtCents == req.BoughtCents &&
		rec.SoldCurrency == req.SoldCurrency &&
		rec.SoldCents == req.SoldCents
}

// mt300Reference validates a :20: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT300 error identity.
func mt300Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT300Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT300Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT300Reference, field, ref)
	}
	return nil
}

// mt300CommonReferenceShape validates a :22C: common reference:
// 4!a2!c4!n — 4 letters, 2 alphanumeric characters, 4 digits.
func mt300CommonReferenceShape(ref string) error {
	if len(ref) != 10 {
		return fmt.Errorf("%w: :22C: want 4!a2!c4!n (10 characters), got %q", ErrMT300Reference, ref)
	}
	for i := 0; i < 4; i++ {
		if ref[i] < 'A' || ref[i] > 'Z' {
			return fmt.Errorf("%w: :22C: want 4 letters first, got %q", ErrMT300Reference, ref)
		}
	}
	for i := 4; i < 6; i++ {
		c := ref[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: :22C: want 2 alphanumeric characters next, got %q", ErrMT300Reference, ref)
		}
	}
	for i := 6; i < 10; i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return fmt.Errorf("%w: :22C: want 4 digits last, got %q", ErrMT300Reference, ref)
		}
	}
	return nil
}

// mt300Currency validates a :32B:/:33B: currency code: exactly
// three uppercase ASCII letters. The MT202 rule, under the
// MT300 error identity.
func mt300Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT300Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT300Currency, code)
		}
	}
	return nil
}

// mt300ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I300<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT300 error identity.
func mt300ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT300Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT300Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT300Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT300LT, lt)
	}
	return lt, nil
}

// mt300ParsePartyField parses a party line carrying either the A
// or the D option of tag (":82:", ":87:", or ":57:"): the line
// must start with tag+"A:" or tag+"D:", and the body must pass
// the form's check — a BIC for A, the SWIFT `x` set for D.
func mt300ParsePartyField(line, tag, field string) (party string, isBIC bool, err error) {
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
				return "", false, fmt.Errorf("%w: %s field is empty", ErrMT300Field, field)
			}
			if opt.isBIC {
				if !validBIC(body) {
					return "", false, fmt.Errorf("%w: %s: %q is not a valid BIC", ErrMT300Field, field, body)
				}
			} else if err := mt103CheckX(body); err != nil {
				return "", false, fmt.Errorf("%w: %s: %w", ErrMT300Field, field, err)
			}
			return body, opt.isBIC, nil
		}
	}
	return "", false, fmt.Errorf("%w: want %sA: or %sD: field, got %q", ErrMT300Field, tag, tag, line)
}

// mt300ParseRateScaled parses a :36: rate body ("<integer>,<1-12
// fraction digits>") into its value scaled by 10^12, exact
// big.Int arithmetic, never float. The integer part holds only
// digits; the fraction part is right-padded with zeros to 12
// places. The value must be positive.
func mt300ParseRateScaled(body string) (*big.Int, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: want <integer>,<fraction digits>, got %q", ErrMT300Rate, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) > 12 {
		return nil, fmt.Errorf("%w: %q", ErrMT300Rate, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %q", ErrMT300Rate, body)
		}
	}
	scaled, ok := new(big.Int).SetString(intPart+fracPart+strings.Repeat("0", 12-len(fracPart)), 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMT300Rate, body)
	}
	if scaled.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %q is not positive", ErrMT300Rate, body)
	}
	return scaled, nil
}

// mt300RateScaled renders RateNum/RateDen scaled by 10^12 with
// the mt300Rate half-up rounding at the 12th decimal place, exact
// big.Int arithmetic: scaled = (2*num*10^12 + den) / (2*den) for
// positive rationals.
func mt300RateScaled(num, den int64) *big.Int {
	const scale = 1000000000000 // 10^12
	n := big.NewInt(num)
	d := big.NewInt(den)
	scaled := new(big.Int).Mul(n, big.NewInt(scale))
	scaled.Mul(scaled, big.NewInt(2))
	scaled.Add(scaled, d)
	return scaled.Quo(scaled, new(big.Int).Mul(d, big.NewInt(2)))
}

// mt300ParseAmountField parses a :32B:/:33B: body (3-letter
// currency + `<integer>,<2 fraction digits>`) into its currency
// and minor-unit amount. The 15d amount rule rides
// mt202ParseAmount, under the MT300 error identity.
func mt300ParseAmountField(body, field string) (currency string, cents int64, err error) {
	if len(body) < 3+4 {
		return "", 0, fmt.Errorf("%w: %s too short: %q", ErrMT300Framing, field, body)
	}
	currency = body[:3]
	if err := mt300Currency(currency); err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}
	amount, err := mt202ParseAmount(body[3:])
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w: %v", field, ErrMT300Amount, err)
	}
	return currency, amount, nil
}

// mt300ParseDate parses a :30T:/:30V: YYMMDD body into a UTC
// calendar date.
func mt300ParseDate(body, field string) (time.Time, error) {
	if len(body) != 6 {
		return time.Time{}, fmt.Errorf("%w: %s want YYMMDD, got %q", ErrMT300ValueDate, field, body)
	}
	vd, err := time.Parse("060102", body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q: %w", ErrMT300ValueDate, field, body, err)
	}
	return vd.UTC(), nil
}

// ParseMT300 parses a SWIFT FIN MT300 message produced by
// ExportMT300 (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the thirteen-field core set is refused — a confirmation
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated.
func ParseMT300(text string) (MT300Confirmation, error) {
	var zero MT300Confirmation
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 18 {
		return zero, fmt.Errorf("%w: want 18 lines (3 headers + {4: + 13 fields + -}), got %d", ErrMT300Framing, len(lines))
	}
	senderLT, err := mt300ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt300ParseLT(lines[1], "{2:I300", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT300Framing, mur)
	}
	if err := mt300Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT300Framing, lines[3])
	}
	if lines[17] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT300Framing, lines[17])
	}
	// Sequence markers carry no body.
	if lines[4] != ":15A:" {
		return zero, fmt.Errorf("%w: line 5: want :15A: field, got %q", ErrMT300Field, lines[4])
	}
	if lines[10] != ":15B:" {
		return zero, fmt.Errorf("%w: line 11: want :15B: field, got %q", ErrMT300Field, lines[10])
	}
	var m MT300Confirmation
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20: — the message's own reference, the replay key.
	if !strings.HasPrefix(lines[5], ":20:") {
		return zero, fmt.Errorf("%w: line 6: want :20: field, got %q", ErrMT300Field, lines[5])
	}
	if err := mt300Reference(lines[5][len(":20:"):], ":20:"); err != nil {
		return zero, err
	}
	m.Reference = lines[5][len(":20:"):]
	// :22A: — only NEWT is accepted: this endpoint confirms new
	// trades only.
	if lines[6] != ":22A:NEWT" {
		if !strings.HasPrefix(lines[6], ":22A:") {
			return zero, fmt.Errorf("%w: line 7: want :22A: field, got %q", ErrMT300Field, lines[6])
		}
		return zero, fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT300Field, lines[6][len(":22A:"):])
	}
	m.Variant = "NEWT"
	// :22C: — the common reference, the addressing key.
	if !strings.HasPrefix(lines[7], ":22C:") {
		return zero, fmt.Errorf("%w: line 8: want :22C: field, got %q", ErrMT300Field, lines[7])
	}
	if err := mt300CommonReferenceShape(lines[7][len(":22C:"):]); err != nil {
		return zero, err
	}
	m.CommonReference = lines[7][len(":22C:"):]
	// :82a: / :87a: — the parties, BIC or account-ID form.
	senderParty, senderBIC, err := mt300ParsePartyField(lines[8], ":82", ":82a:")
	if err != nil {
		return zero, fmt.Errorf("line 9: %w", err)
	}
	m.SenderParty, m.SenderPartyBIC = senderParty, senderBIC
	receiverParty, receiverBIC, err := mt300ParsePartyField(lines[9], ":87", ":87a:")
	if err != nil {
		return zero, fmt.Errorf("line 10: %w", err)
	}
	m.ReceiverParty, m.ReceiverPartyBIC = receiverParty, receiverBIC
	// :30T: / :30V: — trade date and value date.
	tradeBody, err := mt300FieldBody(lines[11], ":30T:", 12)
	if err != nil {
		return zero, err
	}
	tradeDate, err := mt300ParseDate(tradeBody, ":30T:")
	if err != nil {
		return zero, err
	}
	m.TradeDate = tradeDate
	valueBody, err := mt300FieldBody(lines[12], ":30V:", 13)
	if err != nil {
		return zero, err
	}
	valueDate, err := mt300ParseDate(valueBody, ":30V:")
	if err != nil {
		return zero, err
	}
	m.ValueDate = valueDate
	// :36: — the rate, exactly as written; validated as a
	// positive decimal with 1–12 fraction digits.
	rateBody, err := mt300FieldBody(lines[13], ":36:", 14)
	if err != nil {
		return zero, err
	}
	if _, err := mt300ParseRateScaled(rateBody); err != nil {
		return zero, err
	}
	m.Rate = rateBody
	// :32B: / :33B: — the bought and sold amounts.
	boughtBody, err := mt300FieldBody(lines[14], ":32B:", 15)
	if err != nil {
		return zero, err
	}
	boughtCcy, bought, err := mt300ParseAmountField(boughtBody, ":32B:")
	if err != nil {
		return zero, err
	}
	m.BoughtCurrency, m.BoughtCents = boughtCcy, bought
	soldBody, err := mt300FieldBody(lines[15], ":33B:", 16)
	if err != nil {
		return zero, err
	}
	soldCcy, sold, err := mt300ParseAmountField(soldBody, ":33B:")
	if err != nil {
		return zero, err
	}
	m.SoldCurrency, m.SoldCents = soldCcy, sold
	// :57a: — the account the bought currency lands in.
	accountParty, accountBIC, err := mt300ParsePartyField(lines[16], ":57", ":57a:")
	if err != nil {
		return zero, fmt.Errorf("line 17: %w", err)
	}
	m.AccountParty, m.AccountPartyBIC = accountParty, accountBIC
	return m, nil
}

// mt300FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt300FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT300Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT300Field, tag)
	}
	return body, nil
}

// validateMT300Confirmation fail-closes on a confirmation
// ParseMT300 would refuse, so ApplyMT300 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT300Confirmation(req MT300Confirmation) error {
	if err := mt300Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if req.Variant != "NEWT" {
		return fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT300Field, req.Variant)
	}
	if err := mt300CommonReferenceShape(req.CommonReference); err != nil {
		return err
	}
	if req.TradeDate.IsZero() {
		return fmt.Errorf("%w: :30T: trade date is required", ErrMT300ValueDate)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: :30V: value date is required", ErrMT300ValueDate)
	}
	if _, err := mt300ParseRateScaled(req.Rate); err != nil {
		return err
	}
	if err := mt300Currency(req.BoughtCurrency); err != nil {
		return fmt.Errorf(":32B:: %w", err)
	}
	if req.BoughtCents <= 0 {
		return fmt.Errorf("%w: :32B: %d is not positive", ErrMT300Amount, req.BoughtCents)
	}
	if err := mt300Currency(req.SoldCurrency); err != nil {
		return fmt.Errorf(":33B:: %w", err)
	}
	if req.SoldCents <= 0 {
		return fmt.Errorf("%w: :33B: %d is not positive", ErrMT300Amount, req.SoldCents)
	}
	return nil
}

// mt300ResolveLocked resolves the forward a :22C: common
// reference names: every stored contract's deterministic
// derivation is recomputed and compared, the same derivation
// ExportMT300 renders. A contract that cannot render a :22C:
// (a party identifier or forward ID outside 4!a2!c4!n) can never
// match and is skipped. Callers must hold the write lock.
func (l *Ledger) mt300ResolveLocked(commonRef string) []*FXForward {
	var matched []*FXForward
	for _, c := range l.forwards {
		fromID, _ := l.mt300PartyIdentifierLocked(c.FromAccount)
		toID, _ := l.mt300PartyIdentifierLocked(c.ToAccount)
		derived, err := mt300CommonReference(fromID, toID, c.ID)
		if err != nil {
			continue
		}
		if derived == commonRef {
			matched = append(matched, c)
		}
	}
	return matched
}

// ApplyMT300 applies one parsed inbound SWIFT FIN MT300 FX
// forward confirmation: it matches the confirmation against the
// booked forward the :22C: common reference names and records
// the match.
//
// The forward must exist (:22C: unknown is ErrForwardNotFound,
// fail-closed — a confirmation never books a forward) and be
// booked or settled (a cancelled contract reports
// ErrForwardNotBooked, a lapsed one ErrForwardExpired, the
// export's gate). Every economic term must agree exactly with
// the booked contract — the :36: rate against the locked
// RateNum/RateDen, the :32B: bought amount against
// floor(NotionalCents*RateNum/RateDen), the :33B: sold amount
// against NotionalCents, and the :30V: value date against the
// forward's SettleAt value date — or the whole confirmation is
// refused with ErrMT300TermsMismatch: there is no partial
// confirmation. No settlement runs, no forward is created, and
// the contract's lifecycle does not move: the match is recorded
// in the mt300Applied :20: registry and the mt300Confirmed
// forward marker, and one `mt300_confirmation_matched` audit
// event (details msg_id / common_ref / forward_id) is emitted.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same forward, dates, rate, and
// amounts returns the current forward with Duplicate set,
// recording nothing twice and emitting no second audit event; a
// :20: reused for different terms is refused with
// ErrMT300MsgIDConflict.
//
// Locking: the whole apply runs under the write lock — the
// replay read, the lazy-expiry sweep, the :22C: resolution, the
// term verification, the registries, and the audit event land
// atomically, so two concurrent applies of the same :20: still
// emit exactly one marker event.
func (l *Ledger) ApplyMT300(req MT300Confirmation, actor string) (MT300ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT300"
	}
	if err := validateMT300Confirmation(req); err != nil {
		return MT300ApplyResult{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// :20: replay: the confirmation was already applied under
	// this reference. Same terms is a no-op returning the
	// current forward; different terms is a conflict. The replay
	// check runs before the lazy-expiry sweep, so a confirmation
	// that already matched stays matched even after the forward
	// lapses.
	if rec, ok := l.mt300Applied[req.Reference]; ok {
		if !mt300TermsEqual(rec, req) {
			return MT300ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT300MsgIDConflict, req.Reference)
		}
		if current, ok := l.forwards[rec.ForwardID]; ok {
			return MT300ApplyResult{Forward: current.copy(), CommonReference: req.CommonReference, Duplicate: true}, nil
		}
		return MT300ApplyResult{Forward: rec.Forward, CommonReference: req.CommonReference, Duplicate: true}, nil
	}

	// Lazy expiry first, the export's pattern: a booked contract
	// past SettleAt + the live grace period lapses to cancelled
	// before anything matches, so a dead agreement is never
	// confirmed.
	l.expireForwardsLocked(time.Now())

	// The forward itself: :22C: names it. Unknown is
	// fail-closed; ambiguous (more than one contract deriving
	// the same reference) is a terms mismatch — the
	// confirmation does not identify one trade.
	matched := l.mt300ResolveLocked(req.CommonReference)
	if len(matched) == 0 {
		return MT300ApplyResult{}, fmt.Errorf("%w: no forward derives :22C: %q", ErrForwardNotFound, req.CommonReference)
	}
	if len(matched) > 1 {
		return MT300ApplyResult{}, fmt.Errorf("%w: :22C: %q matches %d forwards", ErrMT300TermsMismatch, req.CommonReference, len(matched))
	}
	fwd := matched[0]
	switch fwd.Status {
	case ForwardBooked, ForwardSettled:
		// Confirmable.
	case ForwardCancelled:
		// A contract the lazy sweep just lapsed (or one a
		// previous sweep lapsed) reports expired, not merely
		// "not booked".
		if fwd.CancelReason == ForwardCancelExpired {
			return MT300ApplyResult{}, fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, fwd.ID, fwd.SettleAt.UTC().Format(time.RFC3339))
		}
		return MT300ApplyResult{}, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, fwd.ID, fwd.Status)
	default:
		return MT300ApplyResult{}, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, fwd.ID, fwd.Status)
	}

	// Every economic term, exactly: the :36: rate against the
	// locked ratio (half-up rounded to 12 decimals, big-number
	// exact), the :32B: bought amount against the floored
	// conversion, the :33B: sold amount against the notional,
	// and the :30V: value date against the SettleAt value date.
	// Any disagreement refuses the whole confirmation.
	gotRate, err := mt300ParseRateScaled(req.Rate)
	if err != nil {
		return MT300ApplyResult{}, err
	}
	if gotRate.Cmp(mt300RateScaled(fwd.RateNum, fwd.RateDen)) != 0 {
		want, _ := mt300Rate(fwd.RateNum, fwd.RateDen)
		return MT300ApplyResult{}, fmt.Errorf("%w: :36: holds %q, forward locks %q", ErrMT300TermsMismatch, req.Rate, want)
	}
	if mt103ValueDate(req.ValueDate) != mt103ValueDate(fwd.SettleAt) {
		return MT300ApplyResult{}, fmt.Errorf("%w: :30V: holds %q, forward settles %q",
			ErrMT300TermsMismatch, mt103ValueDate(req.ValueDate), mt103ValueDate(fwd.SettleAt))
	}
	if req.BoughtCurrency != fwd.ToCurrency {
		return MT300ApplyResult{}, fmt.Errorf("%w: :32B: holds currency %q, forward buys %q",
			ErrMT300TermsMismatch, req.BoughtCurrency, fwd.ToCurrency)
	}
	if new(big.Int).SetInt64(req.BoughtCents).Cmp(mt300ConvertedCents(fwd.NotionalCents, fwd.RateNum, fwd.RateDen)) != 0 {
		want := mt300ConvertedCents(fwd.NotionalCents, fwd.RateNum, fwd.RateDen)
		return MT300ApplyResult{}, fmt.Errorf("%w: :32B: holds %d cents, forward converts to %s cents",
			ErrMT300TermsMismatch, req.BoughtCents, want.String())
	}
	if req.SoldCurrency != fwd.FromCurrency {
		return MT300ApplyResult{}, fmt.Errorf("%w: :33B: holds currency %q, forward sells %q",
			ErrMT300TermsMismatch, req.SoldCurrency, fwd.FromCurrency)
	}
	if req.SoldCents != fwd.NotionalCents {
		return MT300ApplyResult{}, fmt.Errorf("%w: :33B: holds %d cents, forward notional is %d cents",
			ErrMT300TermsMismatch, req.SoldCents, fwd.NotionalCents)
	}

	// The match: the :20: record, the forward's confirmation
	// marker, and one audit event, under the one write lock.
	// The confirmation is already verified when the marker
	// lands.
	snap := fwd.copy()
	if l.mt300Applied == nil {
		l.mt300Applied = make(map[string]mt300AppliedRecord)
	}
	l.mt300Applied[req.Reference] = mt300AppliedRecord{
		Reference:       req.Reference,
		CommonReference: req.CommonReference,
		ForwardID:       fwd.ID,
		TradeDate:       req.TradeDate,
		ValueDate:       req.ValueDate,
		Rate:            req.Rate,
		BoughtCurrency:  req.BoughtCurrency,
		BoughtCents:     req.BoughtCents,
		SoldCurrency:    req.SoldCurrency,
		SoldCents:       req.SoldCents,
		Forward:         snap,
	}
	if l.mt300Confirmed == nil {
		l.mt300Confirmed = make(map[string]mt300ConfirmationRecord)
	}
	l.mt300Confirmed[fwd.ID] = mt300ConfirmationRecord{
		ForwardID:       fwd.ID,
		Reference:       req.Reference,
		CommonReference: req.CommonReference,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt300_confirmation_matched",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{fwd.FromAccount, fwd.ToAccount},
		Details: map[string]any{
			"msg_id":     req.Reference,
			"common_ref": req.CommonReference,
			"forward_id": fwd.ID,
		},
	})
	return MT300ApplyResult{Forward: snap, CommonReference: req.CommonReference}, nil
}

// ForwardConfirmed reports whether the forward has a matched
// inbound MT300 confirmation, and the confirming :20: reference
// when it does. The marker is read-only: it moves no lifecycle
// state.
func (l *Ledger) ForwardConfirmed(forwardID string) (reference string, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt300Confirmed[forwardID]
	if !ok {
		return "", false
	}
	return rec.Reference, true
}
