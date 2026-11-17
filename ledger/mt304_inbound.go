package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN MT304 third-party FX deal instruction inbound processing
// (LG-443): the inbound half of the MT304 lifecycle, the mirror of
// the LG-390 export.
//
// ExportMT304 renders this ledger's own instruction — the ledger
// telling the custodian "settle this booked forward for the fund".
// This module consumes the instruction the fund manager sent to
// this ledger, acting as the custodian: the manager dealt for the
// fund with an executing broker and instructs the custodian to
// settle the deal on the fund's behalf. The two directions are
// orthogonal by construction:
//
//   - LG-390 exports: the ledger instructs settlement of one of
//     its own booked forwards (outbound, read-only).
//   - LG-443 imports: the fund manager instructs this ledger to
//     settle a third-party deal (inbound, booking by scope).
//
// Where MT300 (LG-440) is the bilateral confirmation — the two
// parties confirming their own trade, matched and never booked —
// MT304 is the third-party instruction, and the :94A: scope splits
// the handling in two:
//
//   - ADVI (advice): the instruction is recorded only — the
//     mt304Applied registry entry plus one
//     `mt304_advice_recorded` audit event. No forward is booked,
//     nothing settles, and the settlement stays where it belongs:
//     SettleForward remains the only settlement entry (LG-110).
//   - INST (instruction): the instructed deal is booked as a
//     forward contract through BookForward — the :20: is the
//     forward ID and the idempotency key — and one
//     `mt304_deal_booked` audit event is emitted (BookForward's
//     own `fx_forward_booked` event lands first, as on any
//     booking).
//
// ParseMT304 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT300: the block structure, the
// field order, and every field value are validated, and anything
// outside the fifteen-field core set (:15A: :20: :22A: :94A:
// :83a: :82a: :87a: :15B: :30T: :30V: :36: :32B: :53a: :33B:
// :57a:) is refused — an instruction must not lose content
// quietly. CRLF line endings are accepted; a trailing newline
// after the `-}` trailer is tolerated. The :83a: / :82a: /
// :87a: / :53a: / :57a: `a` option means "BIC or name-and-address":
// this module implements the A (BIC) variant and the D
// (account-ID) variant the export renders, and refuses any other
// option fail-closed rather than silently dropping the party
// identity.
//
// Only :22A: NEWT is accepted: this endpoint instructs new deals
// only (AMND/CANC variants are a later round, the export's rule).
// Only :94A: ADVI/INST is accepted: any other scope reports
// ErrMT304Scope, a 422-class error (not a 400-class parse error —
// the scope is a semantic rejection of an otherwise well-formed
// instruction).
//
// ApplyMT304 resolves the parties before anything books or
// records: the fund (:83a:) and the fund manager (:82a:) must
// both name the same From account — the ledger holds no separate
// fund/manager identity and invents no manager BIC, along the
// export — the executing broker (:87a:) must name a known To
// account, and the delivery bank (:53a:) and receiving bank
// (:57a:) must name that same To account. A party is known when
// its BIC is registered on a ledger account (see SetAccountIBAN)
// or, for the D form, when the account ID is registered or has
// journal activity: an unknown party reports
// ErrMT304UnknownAccount, fail-closed — the ledger never opens
// an account from a wire identifier, and never guesses one.
//
// The economics are verified exactly before booking: the :36:
// rate is restored exactly as a *big.Rat (12-decimal half-up,
// the mt300Rate rule — a :36: carries at most 12 fraction
// digits, so the scaled value is exact), and the :32B: bought
// amount must equal floor(NotionalCents*RateNum/RateDen) (the
// mt300ConvertedCents rule), or the whole instruction is refused
// with ErrMT304TermsMismatch. The :30T: trade date is parsed and
// validated as a calendar date but not compared — the MT300 rule.
//
// A past :30V: refuses INST with ErrForwardInvalid (BookForward
// requires SettleAt in the future), while ADVI is still
// recorded: advice about a past-value-date deal is still advice,
// and advice books no contract.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// scope, parties, dates, rate, and amounts returns the stored
// outcome with Duplicate set, recording nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT304TermsMismatch — never silently
// overwritten. The registry check runs under the write lock, so
// two concurrent applies of the same :20: still emit exactly one
// event.
//
// A :20: that already names a booked forward through another
// path is adopted, not re-booked: the stored contract's terms
// must agree exactly with the instruction (parties, currencies,
// notional, rate, value date) or the instruction is refused with
// ErrMT304TermsMismatch, and an adopted deal returns Duplicate
// with no audit event. A settled or cancelled contract under
// that :20: reports ErrForwardNotBooked (or ErrForwardExpired
// when it lapsed), mirroring the export's gate.
//
// No settlement runs here: the deal is booked (INST) or recorded
// (ADVI), and settlement still arrives separately through
// SettleForward. One MT304 instructs exactly one deal — no batch
// instruction.

// MT304 inbound errors.
var (
	// ErrMT304Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :15A: → :20: → :22A: → :94A: → :83a: → :82a:
	// → :87a: → :15B: → :30T: → :30V: → :36: → :32B: → :53a:
	// → :33B: → :57a: order.
	ErrMT304Framing = errors.New("ledger: mt304: malformed FIN framing")
	// ErrMT304Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a field
	// outside the fifteen-field core set, a non-empty :15A: /
	// :15B: sequence marker, or a :22A: other than NEWT.
	ErrMT304Field = errors.New("ledger: mt304: unexpected or missing field")
	// ErrMT304Reference is returned for an invalid :20:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, with a leading/trailing colon or slash,
	// or a double slash.
	ErrMT304Reference = errors.New("ledger: mt304: invalid reference")
	// ErrMT304ValueDate is returned for an invalid :30T: or :30V:
	// value date: not a YYMMDD calendar date.
	ErrMT304ValueDate = errors.New("ledger: mt304: invalid :30T:/:30V: value date")
	// ErrMT304Currency is returned for an invalid :32B: or :33B:
	// currency: not exactly three uppercase ASCII letters (the
	// LG-18 currency rule — no lowercase, no digits, no ISO
	// lookup table the ledger does not own).
	ErrMT304Currency = errors.New("ledger: mt304: invalid :32B:/:33B: currency")
	// ErrMT304Amount is returned for an invalid :32B: or :33B:
	// amount: non-positive, not in `<integer>,<2 fraction
	// digits>` form, or with more than 12 integer digits (the
	// MT103 15d rule).
	ErrMT304Amount = errors.New("ledger: mt304: invalid :32B:/:33B: amount")
	// ErrMT304Rate is returned for an invalid :36: rate: not a
	// positive decimal with a comma separator and 1–12 fraction
	// digits.
	ErrMT304Rate = errors.New("ledger: mt304: invalid :36: rate")
	// ErrMT304LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT304LT = errors.New("ledger: mt304: invalid SWIFT LT address")
	// ErrMT304Scope is returned for a :94A: scope other than
	// ADVI or INST. It is a 422-class error: the instruction is
	// well-formed, its purpose is not one this endpoint serves.
	ErrMT304Scope = errors.New("ledger: mt304 inbound: :94A: scope must be ADVI or INST")
	// ErrMT304UnknownAccount is returned when a :83a: / :82a: /
	// :87a: / :53a: / :57a: party names no known ledger account:
	// the ledger never opens an account from a wire identifier.
	// It is a 422-class error.
	ErrMT304UnknownAccount = errors.New("ledger: mt304 inbound: party names no known ledger account")
	// ErrMT304TermsMismatch is returned by ApplyMT304 when the
	// instruction's economics disagree with the deal it names —
	// the :32B: bought amount against
	// floor(NotionalCents*RateNum/RateDen), the fund and manager
	// naming different From accounts, the delivery or receiving
	// bank naming another account than the broker, or a :20:
	// reused for different terms: the instruction points at
	// another deal, so the whole instruction is refused. It is a
	// 422-class error.
	ErrMT304TermsMismatch = errors.New("ledger: mt304 inbound: instruction terms do not match the deal it names")
)

// MT304Instruction is one parsed inbound SWIFT FIN MT304
// third-party FX deal instruction: the block-4 core fields plus
// the block 1/2 LT addresses. TradeDate and ValueDate are UTC
// calendar dates (time-of-day is not carried by :30T:/:30V:);
// BoughtCents and SoldCents are the :32B:/:33B: amounts in minor
// units, always positive; Rate is the :36: rate exactly as
// written.
type MT304Instruction struct {
	SenderLT   string
	ReceiverLT string
	// Reference is :20:, this message's own reference — the
	// forward ID on an INST instruction, and the replay key.
	Reference string
	// Variant is :22A:: always NEWT on a message ParseMT304
	// accepts.
	Variant string
	// Scope is :94A:: ADVI (advice) or INST (instruction).
	Scope string
	// FundParty is the :83A: BIC or :83D: account ID naming the
	// fund; ManagerParty is the :82A: BIC or :82D: account ID
	// naming the fund manager. Both are the From account: the
	// ledger holds no separate fund/manager identity.
	FundParty       string
	FundPartyBIC    bool
	ManagerParty    string
	ManagerPartyBIC bool
	// BrokerParty is the :87A: BIC or :87D: account ID naming
	// the executing broker, the To account.
	BrokerParty    string
	BrokerPartyBIC bool
	// TradeDate is :30T:, the trade date; ValueDate is :30V:,
	// the value date, which becomes the forward's SettleAt on
	// an INST instruction.
	TradeDate time.Time
	ValueDate time.Time
	// Rate is the :36: exchange rate as written (comma decimal
	// separator).
	Rate string
	// BoughtCurrency/BoughtCents is :32B:, the bought amount;
	// SoldCurrency/SoldCents is :33B:, the sold amount (the
	// notional on an INST instruction).
	BoughtCurrency string
	BoughtCents    int64
	// DeliveryParty is the :53A: BIC or :53D: account ID naming
	// the delivery bank, the To account.
	DeliveryParty    string
	DeliveryPartyBIC bool
	SoldCurrency     string
	SoldCents        int64
	// AccountParty is the :57A: BIC or :57D: account ID naming
	// the receiving bank, the To account.
	AccountParty    string
	AccountPartyBIC bool
}

// MT304ApplyResult is the outcome of ApplyMT304.
type MT304ApplyResult struct {
	// Forward is the booked forward contract on an INST
	// instruction (or the current one on the duplicate path);
	// nil on an ADVI instruction, which books nothing.
	Forward *FXForward `json:"forward,omitempty"`
	// Scope echoes the request's :94A: scope.
	Scope string `json:"scope"`
	// Duplicate reports whether the instruction was already
	// applied under the same :20: with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt304AppliedRecord is the replay record for one applied MT304
// instruction, keyed by :20: in mt304Applied: the instruction as
// applied, the forward it booked (nil on ADVI scope), and the
// forward snapshot at application, so a replay returns the same
// outcome without booking or recording twice. For ADVI scope
// this record IS the advice registry: the recorded advice.
type mt304AppliedRecord struct {
	Instruction MT304Instruction
	ForwardID   string
	Forward     *FXForward
}

// mt304TermsEqual reports whether a stored instruction record
// carries exactly the terms an inbound request would apply: the
// same scope, the same parties (as written, BIC flag included),
// the same trade and value dates (by calendar day, as :30T:/
// :30V: carry only a day), the same rate (by scaled value, so
// "1,1" and "1,10" are the same term), and the same bought and
// sold amounts.
func mt304TermsEqual(rec mt304AppliedRecord, req MT304Instruction) bool {
	got := rec.Instruction
	if got.Scope != req.Scope {
		return false
	}
	if got.FundParty != req.FundParty || got.FundPartyBIC != req.FundPartyBIC ||
		got.ManagerParty != req.ManagerParty || got.ManagerPartyBIC != req.ManagerPartyBIC ||
		got.BrokerParty != req.BrokerParty || got.BrokerPartyBIC != req.BrokerPartyBIC ||
		got.DeliveryParty != req.DeliveryParty || got.DeliveryPartyBIC != req.DeliveryPartyBIC ||
		got.AccountParty != req.AccountParty || got.AccountPartyBIC != req.AccountPartyBIC {
		return false
	}
	if mt103ValueDate(got.TradeDate) != mt103ValueDate(req.TradeDate) ||
		mt103ValueDate(got.ValueDate) != mt103ValueDate(req.ValueDate) {
		return false
	}
	wantRate, err := mt304ParseRateScaled(got.Rate)
	if err != nil {
		return false
	}
	reqRate, err := mt304ParseRateScaled(req.Rate)
	if err != nil {
		return false
	}
	if wantRate.Cmp(reqRate) != 0 {
		return false
	}
	return got.BoughtCurrency == req.BoughtCurrency &&
		got.BoughtCents == req.BoughtCents &&
		got.SoldCurrency == req.SoldCurrency &&
		got.SoldCents == req.SoldCents
}

// mt304Reference validates a :20: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT300
// rule, under the MT304 error identity.
func mt304Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT304Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT304Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT304Reference, field, ref)
	}
	return nil
}

// mt304Currency validates a :32B:/:33B: currency code: exactly
// three uppercase ASCII letters. The MT300 rule, under the
// MT304 error identity.
func mt304Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT304Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT304Currency, code)
		}
	}
	return nil
}

// mt304ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I304<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT300 rule, under
// the MT304 error identity.
func mt304ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT304Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT304Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT304Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT304LT, lt)
	}
	return lt, nil
}

// mt304ParsePartyField parses a party line carrying either the A
// or the D option of tag (":83:", ":82:", ":87:", ":53:", or
// ":57:"): the line must start with tag+"A:" or tag+"D:", and the
// body must pass the form's check — a BIC for A, the SWIFT `x`
// set for D.
func mt304ParsePartyField(line, tag, field string) (party string, isBIC bool, err error) {
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
				return "", false, fmt.Errorf("%w: %s field is empty", ErrMT304Field, field)
			}
			if opt.isBIC {
				if !validBIC(body) {
					return "", false, fmt.Errorf("%w: %s: %q is not a valid BIC", ErrMT304Field, field, body)
				}
			} else if err := mt103CheckX(body); err != nil {
				return "", false, fmt.Errorf("%w: %s: %w", ErrMT304Field, field, err)
			}
			return body, opt.isBIC, nil
		}
	}
	return "", false, fmt.Errorf("%w: want %sA: or %sD: field, got %q", ErrMT304Field, tag, tag, line)
}

// mt304ParseRateScaled parses a :36: rate body ("<integer>,<1-12
// fraction digits>") into its value scaled by 10^12, exact
// big.Int arithmetic, never float. The integer part holds only
// digits; the fraction part is right-padded with zeros to 12
// places. The value must be positive.
func mt304ParseRateScaled(body string) (*big.Int, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: want <integer>,<fraction digits>, got %q", ErrMT304Rate, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) > 12 {
		return nil, fmt.Errorf("%w: %q", ErrMT304Rate, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %q", ErrMT304Rate, body)
		}
	}
	scaled, ok := new(big.Int).SetString(intPart+fracPart+strings.Repeat("0", 12-len(fracPart)), 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMT304Rate, body)
	}
	if scaled.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %q is not positive", ErrMT304Rate, body)
	}
	return scaled, nil
}

// mt304Rate restores a :36: rate body exactly as a *big.Rat:
// the scaled value over 10^12, reduced by big.Rat — so "1,1"
// and "1,10" are the same rate, and the locked ratio a forward
// stores renders back through mt300Rate to the same :36:.
func mt304Rate(body string) (*big.Rat, error) {
	scaled, err := mt304ParseRateScaled(body)
	if err != nil {
		return nil, err
	}
	return new(big.Rat).SetFrac(scaled, big.NewInt(1000000000000)), nil
}

// mt304ParseAmountField parses a :32B:/:33B: body (3-letter
// currency + `<integer>,<2 fraction digits>`) into its currency
// and minor-unit amount. The 15d amount rule rides
// mt202ParseAmount, under the MT304 error identity.
func mt304ParseAmountField(body, field string) (currency string, cents int64, err error) {
	if len(body) < 3+4 {
		return "", 0, fmt.Errorf("%w: %s too short: %q", ErrMT304Framing, field, body)
	}
	currency = body[:3]
	if err := mt304Currency(currency); err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}
	amount, err := mt202ParseAmount(body[3:])
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w: %v", field, ErrMT304Amount, err)
	}
	return currency, amount, nil
}

// mt304ParseDate parses a :30T:/:30V: YYMMDD body into a UTC
// calendar date.
func mt304ParseDate(body, field string) (time.Time, error) {
	if len(body) != 6 {
		return time.Time{}, fmt.Errorf("%w: %s want YYMMDD, got %q", ErrMT304ValueDate, field, body)
	}
	vd, err := time.Parse("060102", body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q: %w", ErrMT304ValueDate, field, body, err)
	}
	return vd.UTC(), nil
}

// mt304FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt304FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT304Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT304Field, tag)
	}
	return body, nil
}

// ParseMT304 parses a SWIFT FIN MT304 message produced by
// ExportMT304 (or an equivalent fund-manager message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the fifteen-field core set is refused — an instruction
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated.
func ParseMT304(text string) (MT304Instruction, error) {
	var zero MT304Instruction
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 20 {
		return zero, fmt.Errorf("%w: want 20 lines (3 headers + {4: + 15 fields + -}), got %d", ErrMT304Framing, len(lines))
	}
	senderLT, err := mt304ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt304ParseLT(lines[1], "{2:I304", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT300 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT304Framing, mur)
	}
	if err := mt304Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT304Framing, lines[3])
	}
	if lines[19] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT304Framing, lines[19])
	}
	// Sequence markers carry no body.
	if lines[4] != ":15A:" {
		return zero, fmt.Errorf("%w: line 5: want :15A: field, got %q", ErrMT304Field, lines[4])
	}
	if lines[11] != ":15B:" {
		return zero, fmt.Errorf("%w: line 12: want :15B: field, got %q", ErrMT304Field, lines[11])
	}
	var m MT304Instruction
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20: — the message's own reference, the replay key and the
	// forward ID on an INST instruction.
	if !strings.HasPrefix(lines[5], ":20:") {
		return zero, fmt.Errorf("%w: line 6: want :20: field, got %q", ErrMT304Field, lines[5])
	}
	if err := mt304Reference(lines[5][len(":20:"):], ":20:"); err != nil {
		return zero, err
	}
	m.Reference = lines[5][len(":20:"):]
	// :22A: — only NEWT is accepted: this endpoint instructs new
	// deals only.
	if lines[6] != ":22A:NEWT" {
		if !strings.HasPrefix(lines[6], ":22A:") {
			return zero, fmt.Errorf("%w: line 7: want :22A: field, got %q", ErrMT304Field, lines[6])
		}
		return zero, fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT304Field, lines[6][len(":22A:"):])
	}
	m.Variant = "NEWT"
	// :94A: — only ADVI/INST is accepted, under the scope
	// sentinel so the handler maps it to 422, not 400.
	if !strings.HasPrefix(lines[7], ":94A:") {
		return zero, fmt.Errorf("%w: line 8: want :94A: field, got %q", ErrMT304Field, lines[7])
	}
	switch lines[7][len(":94A:"):] {
	case "ADVI", "INST":
		m.Scope = lines[7][len(":94A:"):]
	default:
		return zero, fmt.Errorf("%w: got %q", ErrMT304Scope, lines[7][len(":94A:"):])
	}
	// :83a: / :82a: / :87a: — the parties, BIC or account-ID form.
	fundParty, fundBIC, err := mt304ParsePartyField(lines[8], ":83", ":83a:")
	if err != nil {
		return zero, fmt.Errorf("line 9: %w", err)
	}
	m.FundParty, m.FundPartyBIC = fundParty, fundBIC
	managerParty, managerBIC, err := mt304ParsePartyField(lines[9], ":82", ":82a:")
	if err != nil {
		return zero, fmt.Errorf("line 10: %w", err)
	}
	m.ManagerParty, m.ManagerPartyBIC = managerParty, managerBIC
	brokerParty, brokerBIC, err := mt304ParsePartyField(lines[10], ":87", ":87a:")
	if err != nil {
		return zero, fmt.Errorf("line 11: %w", err)
	}
	m.BrokerParty, m.BrokerPartyBIC = brokerParty, brokerBIC
	// :30T: / :30V: — trade date and value date.
	tradeBody, err := mt304FieldBody(lines[12], ":30T:", 13)
	if err != nil {
		return zero, err
	}
	tradeDate, err := mt304ParseDate(tradeBody, ":30T:")
	if err != nil {
		return zero, err
	}
	m.TradeDate = tradeDate
	valueBody, err := mt304FieldBody(lines[13], ":30V:", 14)
	if err != nil {
		return zero, err
	}
	valueDate, err := mt304ParseDate(valueBody, ":30V:")
	if err != nil {
		return zero, err
	}
	m.ValueDate = valueDate
	// :36: — the rate, exactly as written; validated as a
	// positive decimal with 1–12 fraction digits.
	rateBody, err := mt304FieldBody(lines[14], ":36:", 15)
	if err != nil {
		return zero, err
	}
	if _, err := mt304ParseRateScaled(rateBody); err != nil {
		return zero, err
	}
	m.Rate = rateBody
	// :32B: — the bought amount.
	boughtBody, err := mt304FieldBody(lines[15], ":32B:", 16)
	if err != nil {
		return zero, err
	}
	boughtCcy, bought, err := mt304ParseAmountField(boughtBody, ":32B:")
	if err != nil {
		return zero, err
	}
	m.BoughtCurrency, m.BoughtCents = boughtCcy, bought
	// :53a: — the delivery bank.
	deliveryParty, deliveryBIC, err := mt304ParsePartyField(lines[16], ":53", ":53a:")
	if err != nil {
		return zero, fmt.Errorf("line 17: %w", err)
	}
	m.DeliveryParty, m.DeliveryPartyBIC = deliveryParty, deliveryBIC
	// :33B: — the sold amount, the notional.
	soldBody, err := mt304FieldBody(lines[17], ":33B:", 18)
	if err != nil {
		return zero, err
	}
	soldCcy, sold, err := mt304ParseAmountField(soldBody, ":33B:")
	if err != nil {
		return zero, err
	}
	m.SoldCurrency, m.SoldCents = soldCcy, sold
	// :57a: — the receiving bank.
	accountParty, accountBIC, err := mt304ParsePartyField(lines[18], ":57", ":57a:")
	if err != nil {
		return zero, fmt.Errorf("line 19: %w", err)
	}
	m.AccountParty, m.AccountPartyBIC = accountParty, accountBIC
	return m, nil
}

// validateMT304Instruction fail-closes on an instruction
// ParseMT304 would refuse, so ApplyMT304 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT304Instruction(req MT304Instruction) error {
	if err := mt304Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if req.Variant != "NEWT" {
		return fmt.Errorf("%w: :22A: only NEWT is accepted, got %q", ErrMT304Field, req.Variant)
	}
	switch req.Scope {
	case "ADVI", "INST":
	default:
		return fmt.Errorf("%w: got %q", ErrMT304Scope, req.Scope)
	}
	for field, party := range map[string]string{
		":83a:": req.FundParty, ":82a:": req.ManagerParty, ":87a:": req.BrokerParty,
		":53a:": req.DeliveryParty, ":57a:": req.AccountParty,
	} {
		if party == "" {
			return fmt.Errorf("%w: %s party is required", ErrMT304Field, field)
		}
	}
	if req.TradeDate.IsZero() {
		return fmt.Errorf("%w: :30T: trade date is required", ErrMT304ValueDate)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: :30V: value date is required", ErrMT304ValueDate)
	}
	if _, err := mt304ParseRateScaled(req.Rate); err != nil {
		return err
	}
	if err := mt304Currency(req.BoughtCurrency); err != nil {
		return fmt.Errorf(":32B:: %w", err)
	}
	if req.BoughtCents <= 0 {
		return fmt.Errorf("%w: :32B: %d is not positive", ErrMT304Amount, req.BoughtCents)
	}
	if err := mt304Currency(req.SoldCurrency); err != nil {
		return fmt.Errorf(":33B:: %w", err)
	}
	if req.SoldCents <= 0 {
		return fmt.Errorf("%w: :33B: %d is not positive", ErrMT304Amount, req.SoldCents)
	}
	if req.BoughtCurrency == req.SoldCurrency {
		return fmt.Errorf("%w: :32B: and :33B: name the same currency %q", ErrMT304TermsMismatch, req.BoughtCurrency)
	}
	return nil
}

// mt304ResolveAccountLocked resolves one parsed party to the
// ledger account it names. A BIC (the A form) resolves through
// the IBAN registry: the account whose registered BIC matches
// exactly, uppercased (the MT101 BIC-agreement convention — a
// counterparty BIC the ledger never registered names no
// account). An account ID (the D form) resolves when the account
// is registered in the IBAN registry or has journal activity:
// both are the ledger's own records of a known account. A party
// that resolves neither way is unknown — the ledger never opens
// an account from a wire identifier, and never guesses one.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) mt304ResolveAccountLocked(party string, isBIC bool) (AccountID, error) {
	if isBIC {
		want := strings.ToUpper(party)
		for acct, reg := range l.accountIBANs {
			if reg.BIC != "" && strings.ToUpper(reg.BIC) == want {
				return acct, nil
			}
		}
		return "", fmt.Errorf("%w: BIC %q names no registered account", ErrMT304UnknownAccount, party)
	}
	acct := AccountID(party)
	if _, ok := l.accountIBANs[acct]; ok {
		return acct, nil
	}
	if len(l.byAccount[acct]) > 0 {
		return acct, nil
	}
	return "", fmt.Errorf("%w: account %q is not a known ledger account", ErrMT304UnknownAccount, party)
}

// mt304ConvertedCents computes floor(sold * num / den) in exact
// big.Int arithmetic: the :32B: bought amount at the :36: rate.
// All three inputs are positive, so big.Int Quo — truncation
// toward zero — is the floor.
func mt304ConvertedCents(soldCents int64, rate *big.Rat) *big.Int {
	converted := new(big.Int).Mul(big.NewInt(soldCents), rate.Num())
	return converted.Quo(converted, rate.Denom())
}

// ApplyMT304 applies one parsed inbound SWIFT FIN MT304
// third-party FX deal instruction.
//
// ADVI scope records the advice only: the mt304Applied :20:
// registry entry plus one `mt304_advice_recorded` audit event
// (details msg_id / scope). No forward is booked, nothing
// settles, and a past :30V: still records — advice about a
// past-value-date deal is still advice.
//
// INST scope books the instructed deal as a forward contract
// through BookForward — the :20: is the forward ID and the
// idempotency key — and emits one `mt304_deal_booked` audit
// event (details msg_id / forward_id / rate / amounts, actor
// `ApplyMT304`). The fund (:83a:) and manager (:82a:) must name
// the same From account, the broker (:87a:) a known To account,
// and the delivery (:53a:) and receiving (:57a:) banks that same
// To account; an unknown party fails closed with
// ErrMT304UnknownAccount. The :32B: bought amount must equal
// floor(NotionalCents*RateNum/RateDen) at the :36: rate, or the
// whole instruction is refused with ErrMT304TermsMismatch. A
// past :30V: is refused with ErrForwardInvalid — BookForward
// requires SettleAt in the future — and settlement still runs
// only through SettleForward.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same scope, parties, dates,
// rate, and amounts returns the stored outcome with Duplicate
// set, recording nothing twice and emitting no second audit
// event; a :20: reused for different terms is refused with
// ErrMT304TermsMismatch.
//
// Locking: the whole apply runs under the write lock — the
// replay read, the party resolution, the term verification, the
// booking, the registries, and the audit event land atomically,
// so two concurrent applies of the same :20: still emit exactly
// one event.
func (l *Ledger) ApplyMT304(req MT304Instruction, actor string) (MT304ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT304"
	}
	if err := validateMT304Instruction(req); err != nil {
		return MT304ApplyResult{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.applyMT304Locked(req, actor, time.Now())
}

// applyMT304Locked is the lock-held core of ApplyMT304; now is
// the application time (time.Now() from ApplyMT304, pinned in
// tests). Callers must hold the write lock.
func (l *Ledger) applyMT304Locked(req MT304Instruction, actor string, now time.Time) (MT304ApplyResult, error) {
	// :20: replay: the instruction was already applied under
	// this reference. Same terms is a no-op returning the stored
	// outcome; different terms is a mismatch. The replay check
	// runs before the lazy-expiry sweep, so an instruction that
	// already applied stays applied even after its forward
	// lapses.
	if rec, ok := l.mt304Applied[req.Reference]; ok {
		if !mt304TermsEqual(rec, req) {
			return MT304ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT304TermsMismatch, req.Reference)
		}
		out := MT304ApplyResult{Scope: rec.Instruction.Scope, Duplicate: true}
		if rec.ForwardID != "" {
			if current, ok := l.forwards[rec.ForwardID]; ok {
				out.Forward = current.copy()
			} else {
				out.Forward = rec.Forward
			}
		}
		return out, nil
	}

	// The parties resolve before anything books or records: a
	// document naming an unknown account books nothing. The
	// fund and the manager are both the From account; the
	// broker, the delivery bank, and the receiving bank are all
	// the To account, along the export.
	from, err := l.mt304ResolveAccountLocked(req.FundParty, req.FundPartyBIC)
	if err != nil {
		return MT304ApplyResult{}, fmt.Errorf(":83a: %w", err)
	}
	manager, err := l.mt304ResolveAccountLocked(req.ManagerParty, req.ManagerPartyBIC)
	if err != nil {
		return MT304ApplyResult{}, fmt.Errorf(":82a: %w", err)
	}
	if manager != from {
		return MT304ApplyResult{}, fmt.Errorf("%w: :83a: names %q, :82a: names %q — fund and manager must be the same From account",
			ErrMT304TermsMismatch, from, manager)
	}
	to, err := l.mt304ResolveAccountLocked(req.BrokerParty, req.BrokerPartyBIC)
	if err != nil {
		return MT304ApplyResult{}, fmt.Errorf(":87a: %w", err)
	}
	delivery, err := l.mt304ResolveAccountLocked(req.DeliveryParty, req.DeliveryPartyBIC)
	if err != nil {
		return MT304ApplyResult{}, fmt.Errorf(":53a: %w", err)
	}
	if delivery != to {
		return MT304ApplyResult{}, fmt.Errorf("%w: :53a: names %q, :87a: names %q — the delivery bank must be the To account",
			ErrMT304TermsMismatch, delivery, to)
	}
	receiving, err := l.mt304ResolveAccountLocked(req.AccountParty, req.AccountPartyBIC)
	if err != nil {
		return MT304ApplyResult{}, fmt.Errorf(":57a: %w", err)
	}
	if receiving != to {
		return MT304ApplyResult{}, fmt.Errorf("%w: :57a: names %q, :87a: names %q — the receiving bank must be the To account",
			ErrMT304TermsMismatch, receiving, to)
	}
	if from == to {
		return MT304ApplyResult{}, fmt.Errorf("%w: fund %q and broker %q are the same account", ErrMT304TermsMismatch, from, to)
	}

	// The economics, exactly: the :36: rate restored as a
	// *big.Rat, and the :32B: bought amount against the floored
	// conversion at that rate. Any disagreement refuses the
	// whole instruction — this check runs for both scopes, so
	// advice with internally inconsistent amounts is never
	// recorded.
	rate, err := mt304Rate(req.Rate)
	if err != nil {
		return MT304ApplyResult{}, err
	}
	if new(big.Int).SetInt64(req.BoughtCents).Cmp(mt304ConvertedCents(req.SoldCents, rate)) != 0 {
		want := mt304ConvertedCents(req.SoldCents, rate)
		return MT304ApplyResult{}, fmt.Errorf("%w: :32B: holds %d cents, the rate converts to %s cents",
			ErrMT304TermsMismatch, req.BoughtCents, want.String())
	}

	if l.mt304Applied == nil {
		l.mt304Applied = make(map[string]mt304AppliedRecord)
	}

	// ADVI scope: record the advice only — the registry entry
	// plus one audit event. No forward is booked, nothing
	// settles, and a past :30V: still records.
	if req.Scope == "ADVI" {
		l.mt304Applied[req.Reference] = mt304AppliedRecord{Instruction: req}
		l.emitAudit(AuditEvent{
			Op:            "mt304_advice_recorded",
			Actor:         actor,
			TraceID:       req.Reference,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{from, to},
			Details: map[string]any{
				"msg_id": req.Reference,
				"scope":  req.Scope,
			},
		})
		return MT304ApplyResult{Scope: req.Scope}, nil
	}

	// INST scope: lazy expiry first, the export's pattern — a
	// booked contract past SettleAt + the live grace period
	// lapses to cancelled before anything books, so a dead
	// agreement is never re-instructed.
	l.expireForwardsLocked(now)

	// A :20: that already names a booked forward through
	// another path is adopted, not re-booked: the stored
	// contract's terms must agree exactly with the instruction,
	// or the instruction is refused. An adopted deal returns
	// Duplicate with no audit event — the deal already exists.
	if existing, ok := l.forwards[req.Reference]; ok {
		if err := l.mt304AdoptTermsLocked(existing, req, from, to); err != nil {
			return MT304ApplyResult{}, err
		}
		switch existing.Status {
		case ForwardBooked:
			l.mt304Applied[req.Reference] = mt304AppliedRecord{
				Instruction: req,
				ForwardID:   existing.ID,
				Forward:     existing.copy(),
			}
			return MT304ApplyResult{Forward: existing.copy(), Scope: req.Scope, Duplicate: true}, nil
		case ForwardCancelled:
			if existing.CancelReason == ForwardCancelExpired {
				return MT304ApplyResult{}, fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, existing.ID, existing.SettleAt.UTC().Format(time.RFC3339))
			}
			return MT304ApplyResult{}, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, existing.ID, existing.Status)
		default:
			return MT304ApplyResult{}, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, existing.ID, existing.Status)
		}
	}

	// The booking: BookForward's own gates apply — a past
	// :30V: is refused here with ErrForwardInvalid, since
	// SettleAt must be in the future. Its `fx_forward_booked`
	// event lands first, as on any booking.
	fwd, err := l.bookForwardLocked(ForwardBooking{
		ID:            req.Reference,
		From:          from,
		To:            to,
		FromCurrency:  req.SoldCurrency,
		ToCurrency:    req.BoughtCurrency,
		Rate:          rate,
		NotionalCents: req.SoldCents,
		SettleAt:      req.ValueDate,
		FXSide:        FXSideSell,
	}, now)
	if err != nil {
		return MT304ApplyResult{}, err
	}

	// The application: the :20: record plus one audit event,
	// under the one write lock. The deal is already booked when
	// the marker lands.
	snap := fwd.copy()
	l.mt304Applied[req.Reference] = mt304AppliedRecord{
		Instruction: req,
		ForwardID:   fwd.ID,
		Forward:     snap,
	}
	l.emitAudit(AuditEvent{
		Op:            "mt304_deal_booked",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{from, to},
		Details: map[string]any{
			"msg_id":          req.Reference,
			"forward_id":      fwd.ID,
			"scope":           req.Scope,
			"rate":            req.Rate,
			"sold_currency":   req.SoldCurrency,
			"sold_cents":      req.SoldCents,
			"bought_currency": req.BoughtCurrency,
			"bought_cents":    req.BoughtCents,
		},
	})
	return MT304ApplyResult{Forward: snap, Scope: req.Scope}, nil
}

// mt304AdoptTermsLocked verifies an already-booked forward
// against the instruction that names its :20:: the parties,
// the currencies, the notional, the locked rate (half-up rounded
// to 12 decimals, big-number exact), and the :30V: value date
// must all agree. The :30T: trade date is not compared — the
// MT300 rule. The FXSide is not compared either: MT304 carries
// no side, so it cannot disagree with the stored one. Callers
// must hold the write lock.
func (l *Ledger) mt304AdoptTermsLocked(c *FXForward, req MT304Instruction, from, to AccountID) error {
	if c.FromAccount != from {
		return fmt.Errorf("%w: :83a:/:82a: name %q, forward books from %q", ErrMT304TermsMismatch, from, c.FromAccount)
	}
	if c.ToAccount != to {
		return fmt.Errorf("%w: :87a: names %q, forward books to %q", ErrMT304TermsMismatch, to, c.ToAccount)
	}
	if c.FromCurrency != req.SoldCurrency {
		return fmt.Errorf("%w: :33B: holds currency %q, forward sells %q", ErrMT304TermsMismatch, req.SoldCurrency, c.FromCurrency)
	}
	if c.ToCurrency != req.BoughtCurrency {
		return fmt.Errorf("%w: :32B: holds currency %q, forward buys %q", ErrMT304TermsMismatch, req.BoughtCurrency, c.ToCurrency)
	}
	if c.NotionalCents != req.SoldCents {
		return fmt.Errorf("%w: :33B: holds %d cents, forward notional is %d cents", ErrMT304TermsMismatch, req.SoldCents, c.NotionalCents)
	}
	gotRate, err := mt304ParseRateScaled(req.Rate)
	if err != nil {
		return err
	}
	if gotRate.Cmp(mt300RateScaled(c.RateNum, c.RateDen)) != 0 {
		want, _ := mt300Rate(c.RateNum, c.RateDen)
		return fmt.Errorf("%w: :36: holds %q, forward locks %q", ErrMT304TermsMismatch, req.Rate, want)
	}
	if mt103ValueDate(req.ValueDate) != mt103ValueDate(c.SettleAt) {
		return fmt.Errorf("%w: :30V: holds %q, forward settles %q",
			ErrMT304TermsMismatch, mt103ValueDate(req.ValueDate), mt103ValueDate(c.SettleAt))
	}
	return nil
}

// MT304Advice returns the recorded advice instruction for a :20:
// reference applied with ADVI scope, and whether one was
// recorded. The registry is the advice record: an INST
// application under the same :20: is not advice, and an unknown
// reference reports nothing.
func (l *Ledger) MT304Advice(reference string) (MT304Instruction, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt304Applied[reference]
	if !ok || rec.Instruction.Scope != "ADVI" {
		return MT304Instruction{}, false
	}
	return rec.Instruction, true
}
