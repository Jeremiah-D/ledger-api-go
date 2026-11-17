package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN/ISO 15022 MT321 third-party loan/deposit settlement
// instruction inbound processing (LG-447): the inbound half of the
// MT321 lifecycle, the mirror of the LG-396 export.
//
// ExportMT321 renders this ledger's own instruction — the ledger
// telling the fund's custodian "settle this booked term deposit
// with the counterparty". This module consumes the instruction the
// fund manager sent to this ledger, acting as the custodian side:
// it matches the instruction against the booked term deposit the
// :20C::SEME// reference names, verifies every economic term
// exactly, and registers the instruction — and nothing else. The
// two directions are orthogonal by construction:
//
//   - LG-396 exports: the ledger instructs settlement of one of
//     its own booked term deposits (outbound, read-only).
//   - LG-447 imports: the fund manager instructs the same deposit
//     settled (inbound, read-only apart from the instruction
//     registry: no cash movement, no new deposit, no lifecycle
//     transition — execution goes through the existing settlement
//     path, MatureTermDeposit remains the only settlement entry,
//     LG-90).
//
// ParseMT321 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT320 (LG-446) and ParseMT304
// (LG-443): the block structure, the field order, and every field
// value are validated, and anything outside the thirty-line core
// set (3 headers + {4: + 25 fields + -}) is refused — an
// instruction must not lose content quietly. CRLF line endings
// are accepted; a trailing newline after the `-}` trailer is
// tolerated. The :95a: `a` option means "BIC or name-and-address":
// this module implements the P (BIC) variant and the Q
// (account-ID) variant the export renders, and refuses any other
// option fail-closed rather than silently dropping the party
// identity.
//
// ApplyMT321 resolves the deposit by the :20C::SEME// reference —
// the deposit ID, the addressing key this message carries directly
// — and verifies every economic term exactly against the booked
// contract: the :20C::LDCR// common reference against
// mt320CommonReference recomputed from the stored depositor
// identifier, the message's :95P::LDCO// counterparty BIC, and
// the :20C::SEME// deposit ID; the :98A::TRAD// and :98A::VALU//
// dates against the deposit's OpenedAt value date and the
// :98A::MATU// date against MaturesAt; the :19A::PRIN// principal
// against AmountCents; the :19A::SETT// settlement amount against
// the principal (interest settles separately at maturity); and
// the :92A::INTR// rate against RateBps (half-up rounded to 12
// decimal places, the mt320RateBps rule). An unknown deposit
// reports ErrTermDepositNotFound, fail-closed — an instruction
// never opens a deposit. Any mismatch reports
// ErrMT321TermsMismatch and the whole instruction is refused:
// there is no partial instruction. An active or matured deposit
// can be instructed; an early-withdrawn one reports
// ErrTermDepositNotActive, mirroring the export's gate. A
// tiered-rate deposit (LG-145) has no single :92A::INTR: to state
// and fails loud with ErrInvalidMT321Export, the export's rule.
//
// Only :23G: NEWM and :22H::TLDE// CONF are accepted: this
// endpoint instructs settlement of first confirmations only
// (CANC and the TLDE liquidation/rollover codes are a later
// round, the export's rule). The :95a: settlement legs must name
// the same parties as the trade details — :95a::ACCW// the
// counterparty, :95a::CDEA// the depositor — and :97A::SAFE//
// must name the deposit's safekeeping account, or the instruction
// is internally contradictory and refused.
//
// A matched instruction is registered in the mt321Applied SEME
// registry (the replay guard, the pain011Applied pattern) plus
// one `mt321_settlement_instruction_registered` audit event
// (details msg_id / common_ref / deposit_id, actor
// `ApplyMT321`). Registration moves no cash and no lifecycle
// state: the contract stays active (or matured), and execution
// still arrives separately through the existing settlement path.
//
// Idempotency is the SEME with its terms (the pain.014
// convention, LG-416): re-applying the same SEME with the same
// LDCR, economics, and settlement lines returns the stored
// instruction with Duplicate set, recording nothing twice and
// emitting no second audit event. A SEME reused for different
// terms is refused with ErrMT321MsgIDConflict — never silently
// overwritten. The registry check runs under the write lock, so
// two concurrent applies of the same SEME still emit exactly one
// event.
//
// No deposit is ever auto-created, no cash ever moves here: the
// instruction is registered first, and settlement still arrives
// separately through the existing settlement path. One MT321
// instructs exactly one deposit — no batch instruction.

// MT321 inbound errors.
var (
	// ErrMT321Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :16R:GENL → :20C::SEME// → :20C::LDCR// → :23G:
	// → :22H::TLDE// → :16S:GENL → :16R:TRADDET → :20C::CONU//
	// → :22H::DPLO// → :22H::MICO// → :98A::TRAD// →
	// :98A::VALU// → :98A::MATU// → :19A::PRIN// →
	// :19A::SETT// → :92A::INTR// → :95a::LDCO// →
	// :95a::INVE// → :97A::SAFE// → :16S:TRADDET → :16R:SETT →
	// :22H::PRIT// → :95a::ACCW// → :95a::CDEA// → :16S:SETT
	// order.
	ErrMT321Framing = errors.New("ledger: mt321: malformed FIN framing")
	// ErrMT321Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a field
	// outside the thirty-line core set, a non-empty :16R: /
	// :16S: sequence marker, a :23G: other than NEWM, a
	// :22H::TLDE// other than CONF, a :22H::DPLO// other than
	// LEND, a :22H::MICO// other than A009, or a :22H::PRIT//
	// other than APFM.
	ErrMT321Field = errors.New("ledger: mt321: unexpected or missing field")
	// ErrMT321Reference is returned for an invalid :20C::SEME//,
	// :20C::CONU//, or :20C::LDCR// reference: empty, over 16
	// characters (for SEME/CONU), outside the SWIFT `x`
	// character set, with a leading/trailing colon or slash, or
	// a double slash — or a :20C::LDCR// outside the
	// 4!a2!c4!n4!a2!c shape.
	ErrMT321Reference = errors.New("ledger: mt321: invalid reference")
	// ErrMT321ValueDate is returned for an invalid :98A::TRAD//,
	// :98A::VALU//, or :98A::MATU// date: not a YYYYMMDD
	// calendar date.
	ErrMT321ValueDate = errors.New("ledger: mt321: invalid :98A::TRAD//:VALU//:MATU// date")
	// ErrMT321Currency is returned for an invalid :19A::PRIN//
	// or :19A::SETT// currency: not exactly three uppercase
	// ASCII letters (the LG-18 currency rule — no lowercase, no
	// digits, no ISO lookup table the ledger does not own).
	ErrMT321Currency = errors.New("ledger: mt321: invalid :19A::PRIN//:SETT// currency")
	// ErrMT321Amount is returned for an invalid :19A::PRIN// or
	// :19A::SETT// amount: non-positive, not in
	// `<integer>,<2 fraction digits>` form, or with more than 12
	// integer digits (the MT103 15d rule).
	ErrMT321Amount = errors.New("ledger: mt321: invalid :19A::PRIN//:SETT// amount")
	// ErrMT321Rate is returned for an invalid :92A::INTR// rate:
	// not a positive decimal with a comma separator and 1–12
	// fraction digits.
	ErrMT321Rate = errors.New("ledger: mt321: invalid :92A::INTR// rate")
	// ErrMT321LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT321LT = errors.New("ledger: mt321: invalid SWIFT LT address")
	// ErrMT321MsgIDConflict is returned by ApplyMT321 when a
	// SEME reference already applied carries different terms:
	// the reference names one instruction, and a second,
	// different instruction under it is refused rather than
	// silently overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT321MsgIDConflict = errors.New("ledger: mt321 inbound: message reference already applied with different terms")
	// ErrMT321TermsMismatch is returned by ApplyMT321 when the
	// instruction's economic terms disagree with the booked
	// deposit — the :20C::LDCR// common reference, a party, a
	// settlement leg, a date, the principal, the settlement
	// amount, or the rate. It is a 422-class error.
	ErrMT321TermsMismatch = errors.New("ledger: mt321 inbound: instruction terms do not match the booked deposit")
)

// MT321Instruction is one parsed inbound SWIFT FIN/ISO 15022
// MT321 third-party loan/deposit settlement instruction: the
// block-4 core fields plus the block 1/2 LT addresses. TradeDate,
// ValueDate, and MaturityDate are UTC calendar dates (time-of-day
// is not carried by :98A::TRAD// / :98A::VALU// / :98A::MATU//);
// PrincipalCents and SettlementCents are the :19A::PRIN// /
// :19A::SETT// amounts in minor units, always positive; Rate is
// the :92A::INTR// rate exactly as written.
type MT321Instruction struct {
	SenderLT   string `json:"sender_lt"`
	ReceiverLT string `json:"receiver_lt"`
	// Reference is :20C::SEME//, the deposit ID — this message's
	// own reference and the replay key.
	Reference string `json:"reference"`
	// CommonReference is :20C::LDCR//, the 16-character common
	// reference: first 4 of the depositor identifier + first 2
	// of the counterparty BIC + the last 4 digits of the deposit
	// ID + first 4 of the counterparty BIC + first 2 of the
	// depositor identifier.
	CommonReference string `json:"common_ref"`
	// Variant is :23G:: always NEWM on a message ParseMT321
	// accepts.
	Variant string `json:"variant"`
	// TLDE is :22H::TLDE//: always CONF on a message ParseMT321
	// accepts — an instruction to settle a first confirmation.
	TLDE string `json:"tlde"`
	// ContractNumber is :20C::CONU//, the contract number — the
	// deposit ID, the same value as SEME with a different
	// meaning.
	ContractNumber string `json:"contract_number"`
	// DepositOrLoan is :22H::DPLO//: always LEND on a message
	// ParseMT321 accepts — the depositor is the lender.
	DepositOrLoan string `json:"dplo"`
	// DayCount is :22H::MICO//: always A009 on a message
	// ParseMT321 accepts — Actual/365 (fixed).
	DayCount string `json:"mico"`
	// TradeDate is :98A::TRAD//, the trade date; ValueDate is
	// :98A::VALU//, the value date; MaturityDate is
	// :98A::MATU//, the maturity date. :98A::TRAD// and
	// :98A::VALU// both equal the deposit's OpenedAt value date;
	// :98A::MATU// equals MaturesAt.
	TradeDate    time.Time `json:"trade_date"`
	ValueDate    time.Time `json:"value_date"`
	MaturityDate time.Time `json:"maturity_date"`
	// PrincipalCurrency/PrincipalCents is :19A::PRIN//, the
	// principal; SettlementCurrency/SettlementCents is
	// :19A::SETT//, the amount to settle now — the principal
	// alone, interest settles separately at maturity.
	PrincipalCurrency  string `json:"principal_currency"`
	PrincipalCents     int64  `json:"principal_cents"`
	SettlementCurrency string `json:"settlement_currency"`
	SettlementCents    int64  `json:"settlement_cents"`
	// Rate is the :92A::INTR// rate as written (comma decimal
	// separator).
	Rate string `json:"rate"`
	// Counterparty is the :95a::LDCO// counterparty; the BIC
	// flag reports whether it is the P (BIC) form. The export
	// always renders the P form.
	Counterparty    string `json:"counterparty"`
	CounterpartyBIC bool   `json:"counterparty_bic"`
	// Investor is the :95a::INVE// investor, i.e. the fund, i.e.
	// the depositor; the BIC flag reports whether it is the P
	// (BIC) form.
	Investor    string `json:"investor"`
	InvestorBIC bool   `json:"investor_bic"`
	// SafekeepingAccount is :97A::SAFE//, the safekeeping
	// account — the registered IBAN when one exists, else the
	// account ID.
	SafekeepingAccount string `json:"safekeeping_account"`
	// SettlementPriority is :22H::PRIT//: always APFM on a
	// message ParseMT321 accepts — settlement instructions for
	// amounts payable by the investor.
	SettlementPriority string `json:"prit"`
	// AccountWith is the :95a::ACCW// account-with institution,
	// the payee bank; the BIC flag reports whether it is the P
	// (BIC) form. The export always renders the P form.
	AccountWith    string `json:"account_with"`
	AccountWithBIC bool   `json:"account_with_bic"`
	// CashDeliveryAgent is the :95a::CDEA// cash delivery agent,
	// the depositor side; the BIC flag reports whether it is the
	// P (BIC) form.
	CashDeliveryAgent    string `json:"cash_delivery_agent"`
	CashDeliveryAgentBIC bool   `json:"cash_delivery_agent_bic"`
}

// MT321ApplyResult is the outcome of ApplyMT321.
type MT321ApplyResult struct {
	// Instruction is the registered settlement instruction (or
	// the stored one on the duplicate path).
	Instruction *MT321Instruction `json:"instruction"`
	// DepositID is the term deposit the instruction settles.
	DepositID string `json:"deposit_id"`
	// Duplicate reports whether the instruction was already
	// applied under the same SEME with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt321AppliedRecord is the registry record for one applied MT321
// settlement instruction, keyed by :20C::SEME// in mt321Applied:
// the instruction as received (LDCR, economics, settlement
// lines), the deposit it settles, the actor that applied it, and
// the application time, so a replay returns the same outcome
// without registering twice.
type mt321AppliedRecord struct {
	Reference   string
	Instruction MT321Instruction
	DepositID   string
	Actor       string
	ReceivedAt  time.Time
}

// mt321TermsEqual reports whether a stored instruction record
// carries exactly the terms an inbound request would apply: the
// same deposit, the same common reference and contract number,
// the same trade, value, and maturity dates (by calendar day, as
// :98A::TRAD// / :98A::VALU// / :98A::MATU// carry only a day),
// the same rate (by scaled value, so "0,05" and "0,050" are the
// same term), the same principal and settlement amounts, and the
// same parties and settlement legs in the same form.
func mt321TermsEqual(rec mt321AppliedRecord, req MT321Instruction) bool {
	stored := rec.Instruction
	if rec.DepositID == "" {
		return false
	}
	if stored.CommonReference != req.CommonReference ||
		stored.ContractNumber != req.ContractNumber ||
		stored.DepositOrLoan != req.DepositOrLoan ||
		stored.DayCount != req.DayCount ||
		stored.SettlementPriority != req.SettlementPriority {
		return false
	}
	if mt321Day(stored.TradeDate) != mt321Day(req.TradeDate) ||
		mt321Day(stored.ValueDate) != mt321Day(req.ValueDate) ||
		mt321Day(stored.MaturityDate) != mt321Day(req.MaturityDate) {
		return false
	}
	wantRate, err := mt321ParseRateScaled(stored.Rate)
	if err != nil {
		return false
	}
	gotRate, err := mt321ParseRateScaled(req.Rate)
	if err != nil {
		return false
	}
	if wantRate.Cmp(gotRate) != 0 {
		return false
	}
	return stored.PrincipalCurrency == req.PrincipalCurrency &&
		stored.PrincipalCents == req.PrincipalCents &&
		stored.SettlementCurrency == req.SettlementCurrency &&
		stored.SettlementCents == req.SettlementCents &&
		stored.Counterparty == req.Counterparty &&
		stored.CounterpartyBIC == req.CounterpartyBIC &&
		stored.Investor == req.Investor &&
		stored.InvestorBIC == req.InvestorBIC &&
		stored.SafekeepingAccount == req.SafekeepingAccount &&
		stored.AccountWith == req.AccountWith &&
		stored.AccountWithBIC == req.AccountWithBIC &&
		stored.CashDeliveryAgent == req.CashDeliveryAgent &&
		stored.CashDeliveryAgentBIC == req.CashDeliveryAgentBIC
}

// mt321Reference validates a :20C::SEME// or :20C::CONU//
// reference: 1–16 SWIFT `x` characters, no braces in block-4
// content (see mt103CheckX), no leading/trailing colon or slash,
// no double slash. The MT320 rule, under the MT321 error
// identity.
func mt321Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT321Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT321Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT321Reference, field, ref)
	}
	return nil
}

// mt321CommonReferenceShape validates a :20C::LDCR// common
// reference: 4!a2!c4!n4!a2!c — 4 letters, 2 alphanumeric
// characters, 4 digits, 4 letters, 2 alphanumeric characters.
// The MT320 :22C: rule, under the MT321 error identity.
func mt321CommonReferenceShape(ref string) error {
	if len(ref) != 16 {
		return fmt.Errorf("%w: :20C::LDCR//: want 4!a2!c4!n4!a2!c (16 characters), got %q", ErrMT321Reference, ref)
	}
	for i := 0; i < 4; i++ {
		if ref[i] < 'A' || ref[i] > 'Z' {
			return fmt.Errorf("%w: :20C::LDCR//: want 4 letters first, got %q", ErrMT321Reference, ref)
		}
	}
	for i := 4; i < 6; i++ {
		c := ref[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: :20C::LDCR//: want 2 alphanumeric characters next, got %q", ErrMT321Reference, ref)
		}
	}
	for i := 6; i < 10; i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return fmt.Errorf("%w: :20C::LDCR//: want 4 digits next, got %q", ErrMT321Reference, ref)
		}
	}
	for i := 10; i < 14; i++ {
		if ref[i] < 'A' || ref[i] > 'Z' {
			return fmt.Errorf("%w: :20C::LDCR//: want 4 letters next, got %q", ErrMT321Reference, ref)
		}
	}
	for i := 14; i < 16; i++ {
		c := ref[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return fmt.Errorf("%w: :20C::LDCR//: want 2 alphanumeric characters last, got %q", ErrMT321Reference, ref)
		}
	}
	return nil
}

// mt321Currency validates a :19A::PRIN// / :19A::SETT// currency
// code: exactly three uppercase ASCII letters. The MT320 rule,
// under the MT321 error identity.
func mt321Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT321Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT321Currency, code)
		}
	}
	return nil
}

// mt321ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I321<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT320 rule, under
// the MT321 error identity.
func mt321ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT321Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT321Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT321Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT321LT, lt)
	}
	return lt, nil
}

// mt321ParsePartyField parses a party line carrying either the P
// or the Q option of a :95a: field (":95P:" / ":95Q:" followed by
// "::" + qualifier + "//"): the body must pass the form's check —
// a BIC for P, the SWIFT `x` set for Q.
func mt321ParsePartyField(line, qualifier, field string) (party string, isBIC bool, err error) {
	for _, opt := range []struct {
		suffix string
		isBIC  bool
	}{
		{"P:", true}, {"Q:", false},
	} {
		prefix := ":95" + opt.suffix + ":" + qualifier + "//"
		if strings.HasPrefix(line, prefix) {
			body := line[len(prefix):]
			if body == "" {
				return "", false, fmt.Errorf("%w: %s field is empty", ErrMT321Field, field)
			}
			if opt.isBIC {
				if !validBIC(body) {
					return "", false, fmt.Errorf("%w: %s: %q is not a valid BIC", ErrMT321Field, field, body)
				}
			} else if err := mt103CheckX(body); err != nil {
				return "", false, fmt.Errorf("%w: %s: %w", ErrMT321Field, field, err)
			}
			return body, opt.isBIC, nil
		}
	}
	return "", false, fmt.Errorf("%w: want :95P:/:95Q: %s field, got %q", ErrMT321Field, field, line)
}

// mt321ParseRateScaled parses a :92A::INTR// rate body
// ("<integer>,<1-12 fraction digits>") into its value scaled by
// 10^12, exact big.Int arithmetic, never float. The integer part
// holds only digits; the fraction part is right-padded with zeros
// to 12 places. The value must be positive. The MT320 :37G:
// rule, under the MT321 error identity.
func mt321ParseRateScaled(body string) (*big.Int, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: want <integer>,<fraction digits>, got %q", ErrMT321Rate, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) > 12 {
		return nil, fmt.Errorf("%w: %q", ErrMT321Rate, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%w: %q", ErrMT321Rate, body)
		}
	}
	scaled, ok := new(big.Int).SetString(intPart+fracPart+strings.Repeat("0", 12-len(fracPart)), 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrMT321Rate, body)
	}
	if scaled.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %q is not positive", ErrMT321Rate, body)
	}
	return scaled, nil
}

// mt321ParseAmountField parses a :19A::PRIN// / :19A::SETT// body
// (3-letter currency + `<integer>,<2 fraction digits>`) into its
// currency and minor-unit amount. The 15d amount rule rides
// mt202ParseAmount, under the MT321 error identity.
func mt321ParseAmountField(body, field string) (currency string, cents int64, err error) {
	if len(body) < 3+4 {
		return "", 0, fmt.Errorf("%w: %s too short: %q", ErrMT321Framing, field, body)
	}
	currency = body[:3]
	if err := mt321Currency(currency); err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}
	amount, err := mt202ParseAmount(body[3:])
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w: %v", field, ErrMT321Amount, err)
	}
	return currency, amount, nil
}

// mt321ParseDate parses a :98A::TRAD// / :98A::VALU// /
// :98A::MATU// YYYYMMDD body into a UTC calendar date.
func mt321ParseDate(body, field string) (time.Time, error) {
	if len(body) != 8 {
		return time.Time{}, fmt.Errorf("%w: %s want YYYYMMDD, got %q", ErrMT321ValueDate, field, body)
	}
	d, err := time.Parse("20060102", body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q: %w", ErrMT321ValueDate, field, body, err)
	}
	return d.UTC(), nil
}

// mt321Day renders a time as its YYYYMMDD calendar day (UTC), the
// :98A::TRAD// / :98A::VALU// / :98A::MATU// form, so a parsed
// date compares against a stored timestamp by day, never by
// instant.
func mt321Day(t time.Time) string {
	return t.UTC().Format("20060102")
}

// mt321QualifiedFieldBody extracts the body of a single-line
// qualified generic field at a fixed position: the line must start
// with the tag and carry a non-empty body.
func mt321QualifiedFieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT321Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT321Field, tag)
	}
	return body, nil
}

// mt321FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt321FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT321Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT321Field, tag)
	}
	return body, nil
}

// ParseMT321 parses a SWIFT FIN/ISO 15022 MT321 message produced
// by ExportMT321 (or an equivalent fund-manager message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the thirty-line core set is refused — an instruction
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated.
func ParseMT321(text string) (MT321Instruction, error) {
	var zero MT321Instruction
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 30 {
		return zero, fmt.Errorf("%w: want 30 lines (3 headers + {4: + 25 fields + -}), got %d", ErrMT321Framing, len(lines))
	}
	senderLT, err := mt321ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt321ParseLT(lines[1], "{2:I321", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT320 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT321Framing, mur)
	}
	if err := mt321Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT321Framing, lines[3])
	}
	if lines[29] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT321Framing, lines[29])
	}
	// Sequence markers carry no body.
	if lines[4] != ":16R:GENL" {
		return zero, fmt.Errorf("%w: line 5: want :16R:GENL field, got %q", ErrMT321Field, lines[4])
	}
	if lines[9] != ":16S:GENL" {
		return zero, fmt.Errorf("%w: line 10: want :16S:GENL field, got %q", ErrMT321Field, lines[9])
	}
	if lines[10] != ":16R:TRADDET" {
		return zero, fmt.Errorf("%w: line 11: want :16R:TRADDET field, got %q", ErrMT321Field, lines[10])
	}
	if lines[23] != ":16S:TRADDET" {
		return zero, fmt.Errorf("%w: line 24: want :16S:TRADDET field, got %q", ErrMT321Field, lines[23])
	}
	if lines[24] != ":16R:SETT" {
		return zero, fmt.Errorf("%w: line 25: want :16R:SETT field, got %q", ErrMT321Field, lines[24])
	}
	if lines[28] != ":16S:SETT" {
		return zero, fmt.Errorf("%w: line 29: want :16S:SETT field, got %q", ErrMT321Field, lines[28])
	}
	var m MT321Instruction
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20C::SEME// — the deposit ID, the addressing key and
	// replay key.
	seme, err := mt321QualifiedFieldBody(lines[5], ":20C::SEME//", 6)
	if err != nil {
		return zero, err
	}
	if err := mt321Reference(seme, ":20C::SEME//"); err != nil {
		return zero, err
	}
	m.Reference = seme
	// :20C::LDCR// — the common reference, the verification key.
	ldcr, err := mt321QualifiedFieldBody(lines[6], ":20C::LDCR//", 7)
	if err != nil {
		return zero, err
	}
	if err := mt321CommonReferenceShape(ldcr); err != nil {
		return zero, err
	}
	m.CommonReference = ldcr
	// :23G: — only NEWM is accepted: this endpoint instructs
	// settlement of new instructions only.
	if lines[7] != ":23G:NEWM" {
		if !strings.HasPrefix(lines[7], ":23G:") {
			return zero, fmt.Errorf("%w: line 8: want :23G: field, got %q", ErrMT321Field, lines[7])
		}
		return zero, fmt.Errorf("%w: :23G: only NEWM is accepted, got %q", ErrMT321Field, lines[7][len(":23G:"):])
	}
	m.Variant = "NEWM"
	// :22H::TLDE// — only CONF is accepted: this endpoint
	// instructs settlement of first confirmations only.
	tlde, err := mt321QualifiedFieldBody(lines[8], ":22H::TLDE//", 9)
	if err != nil {
		return zero, err
	}
	if tlde != "CONF" {
		return zero, fmt.Errorf("%w: :22H::TLDE// only CONF is accepted, got %q", ErrMT321Field, tlde)
	}
	m.TLDE = "CONF"
	// :20C::CONU// — the contract number, the deposit ID under
	// a second meaning.
	conu, err := mt321QualifiedFieldBody(lines[11], ":20C::CONU//", 12)
	if err != nil {
		return zero, err
	}
	if err := mt321Reference(conu, ":20C::CONU//"); err != nil {
		return zero, err
	}
	m.ContractNumber = conu
	// :22H::DPLO// — only LEND is accepted: the depositor is
	// the lender.
	dplo, err := mt321QualifiedFieldBody(lines[12], ":22H::DPLO//", 13)
	if err != nil {
		return zero, err
	}
	if dplo != "LEND" {
		return zero, fmt.Errorf("%w: :22H::DPLO// only LEND is accepted, got %q", ErrMT321Field, dplo)
	}
	m.DepositOrLoan = "LEND"
	// :22H::MICO// — only A009 is accepted: Actual/365 (fixed),
	// the same day-count as MT320's :14D:ACT/365.
	mico, err := mt321QualifiedFieldBody(lines[13], ":22H::MICO//", 14)
	if err != nil {
		return zero, err
	}
	if mico != "A009" {
		return zero, fmt.Errorf("%w: :22H::MICO// only A009 is accepted, got %q", ErrMT321Field, mico)
	}
	m.DayCount = "A009"
	// :98A::TRAD// / :98A::VALU// / :98A::MATU// — trade,
	// value, and maturity dates.
	tradeBody, err := mt321QualifiedFieldBody(lines[14], ":98A::TRAD//", 15)
	if err != nil {
		return zero, err
	}
	tradeDate, err := mt321ParseDate(tradeBody, ":98A::TRAD//")
	if err != nil {
		return zero, err
	}
	m.TradeDate = tradeDate
	valueBody, err := mt321QualifiedFieldBody(lines[15], ":98A::VALU//", 16)
	if err != nil {
		return zero, err
	}
	valueDate, err := mt321ParseDate(valueBody, ":98A::VALU//")
	if err != nil {
		return zero, err
	}
	m.ValueDate = valueDate
	maturityBody, err := mt321QualifiedFieldBody(lines[16], ":98A::MATU//", 17)
	if err != nil {
		return zero, err
	}
	maturityDate, err := mt321ParseDate(maturityBody, ":98A::MATU//")
	if err != nil {
		return zero, err
	}
	m.MaturityDate = maturityDate
	// :19A::PRIN// / :19A::SETT// — the principal and the
	// amount to settle now.
	principalBody, err := mt321QualifiedFieldBody(lines[17], ":19A::PRIN//", 18)
	if err != nil {
		return zero, err
	}
	principalCcy, principal, err := mt321ParseAmountField(principalBody, ":19A::PRIN//")
	if err != nil {
		return zero, err
	}
	m.PrincipalCurrency, m.PrincipalCents = principalCcy, principal
	settlementBody, err := mt321QualifiedFieldBody(lines[18], ":19A::SETT//", 19)
	if err != nil {
		return zero, err
	}
	settlementCcy, settlement, err := mt321ParseAmountField(settlementBody, ":19A::SETT//")
	if err != nil {
		return zero, err
	}
	m.SettlementCurrency, m.SettlementCents = settlementCcy, settlement
	// :92A::INTR// — the rate, exactly as written; validated as
	// a positive decimal with 1–12 fraction digits.
	rateBody, err := mt321QualifiedFieldBody(lines[19], ":92A::INTR//", 20)
	if err != nil {
		return zero, err
	}
	if _, err := mt321ParseRateScaled(rateBody); err != nil {
		return zero, err
	}
	m.Rate = rateBody
	// :95a::LDCO// / :95a::INVE// — the counterparty and the
	// investor (the depositor), BIC or account-ID form.
	counterparty, counterpartyBIC, err := mt321ParsePartyField(lines[20], "LDCO", ":95a::LDCO//")
	if err != nil {
		return zero, fmt.Errorf("line 21: %w", err)
	}
	m.Counterparty, m.CounterpartyBIC = counterparty, counterpartyBIC
	investor, investorBIC, err := mt321ParsePartyField(lines[21], "INVE", ":95a::INVE//")
	if err != nil {
		return zero, fmt.Errorf("line 22: %w", err)
	}
	m.Investor, m.InvestorBIC = investor, investorBIC
	// :97A::SAFE// — the safekeeping account.
	safeBody, err := mt321QualifiedFieldBody(lines[22], ":97A::SAFE//", 23)
	if err != nil {
		return zero, err
	}
	if len(safeBody) > 35 {
		return zero, fmt.Errorf("%w: :97A::SAFE// account %q exceeds 35x", ErrMT321Field, safeBody)
	}
	if err := mt103CheckX(safeBody); err != nil {
		return zero, fmt.Errorf("%w: :97A::SAFE//: %w", ErrMT321Field, err)
	}
	m.SafekeepingAccount = safeBody
	// :22H::PRIT// — only APFM is accepted: settlement
	// instructions for amounts payable by the investor.
	prit, err := mt321QualifiedFieldBody(lines[25], ":22H::PRIT//", 26)
	if err != nil {
		return zero, err
	}
	if prit != "APFM" {
		return zero, fmt.Errorf("%w: :22H::PRIT// only APFM is accepted, got %q", ErrMT321Field, prit)
	}
	m.SettlementPriority = "APFM"
	// :95a::ACCW// / :95a::CDEA// — the settlement legs: the
	// account-with institution (the payee bank) and the cash
	// delivery agent (the depositor side).
	accountWith, accountWithBIC, err := mt321ParsePartyField(lines[26], "ACCW", ":95a::ACCW//")
	if err != nil {
		return zero, fmt.Errorf("line 27: %w", err)
	}
	m.AccountWith, m.AccountWithBIC = accountWith, accountWithBIC
	cdea, cdeaBIC, err := mt321ParsePartyField(lines[27], "CDEA", ":95a::CDEA//")
	if err != nil {
		return zero, fmt.Errorf("line 28: %w", err)
	}
	m.CashDeliveryAgent, m.CashDeliveryAgentBIC = cdea, cdeaBIC
	return m, nil
}

// validateMT321Instruction fail-closes on an instruction
// ParseMT321 would refuse, so ApplyMT321 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validateMT321Instruction(req MT321Instruction) error {
	if err := mt321Reference(req.Reference, ":20C::SEME//"); err != nil {
		return err
	}
	if err := mt321CommonReferenceShape(req.CommonReference); err != nil {
		return err
	}
	if req.Variant != "NEWM" {
		return fmt.Errorf("%w: :23G: only NEWM is accepted, got %q", ErrMT321Field, req.Variant)
	}
	if req.TLDE != "CONF" {
		return fmt.Errorf("%w: :22H::TLDE// only CONF is accepted, got %q", ErrMT321Field, req.TLDE)
	}
	if err := mt321Reference(req.ContractNumber, ":20C::CONU//"); err != nil {
		return err
	}
	if req.ContractNumber != req.Reference {
		return fmt.Errorf("%w: :20C::CONU// holds %q, :20C::SEME// holds %q — the contract number names the same deposit", ErrMT321TermsMismatch, req.ContractNumber, req.Reference)
	}
	if req.DepositOrLoan != "LEND" {
		return fmt.Errorf("%w: :22H::DPLO// only LEND is accepted, got %q", ErrMT321Field, req.DepositOrLoan)
	}
	if req.DayCount != "A009" {
		return fmt.Errorf("%w: :22H::MICO// only A009 is accepted, got %q", ErrMT321Field, req.DayCount)
	}
	if req.TradeDate.IsZero() {
		return fmt.Errorf("%w: :98A::TRAD// trade date is required", ErrMT321ValueDate)
	}
	if req.ValueDate.IsZero() {
		return fmt.Errorf("%w: :98A::VALU// value date is required", ErrMT321ValueDate)
	}
	if req.MaturityDate.IsZero() {
		return fmt.Errorf("%w: :98A::MATU// maturity date is required", ErrMT321ValueDate)
	}
	if _, err := mt321ParseRateScaled(req.Rate); err != nil {
		return err
	}
	if err := mt321Currency(req.PrincipalCurrency); err != nil {
		return fmt.Errorf(":19A::PRIN//: %w", err)
	}
	if req.PrincipalCents <= 0 {
		return fmt.Errorf("%w: :19A::PRIN// %d is not positive", ErrMT321Amount, req.PrincipalCents)
	}
	if err := mt321Currency(req.SettlementCurrency); err != nil {
		return fmt.Errorf(":19A::SETT//: %w", err)
	}
	if req.SettlementCents <= 0 {
		return fmt.Errorf("%w: :19A::SETT// %d is not positive", ErrMT321Amount, req.SettlementCents)
	}
	if req.SettlementCurrency != req.PrincipalCurrency || req.SettlementCents != req.PrincipalCents {
		return fmt.Errorf("%w: :19A::SETT// holds %s %d, :19A::PRIN// holds %s %d — the settlement amount is the principal alone",
			ErrMT321TermsMismatch, req.SettlementCurrency, req.SettlementCents, req.PrincipalCurrency, req.PrincipalCents)
	}
	if req.SettlementPriority != "APFM" {
		return fmt.Errorf("%w: :22H::PRIT// only APFM is accepted, got %q", ErrMT321Field, req.SettlementPriority)
	}
	for field, party := range map[string]string{
		":95a::LDCO//": req.Counterparty,
		":95a::INVE//": req.Investor,
		":95a::ACCW//": req.AccountWith,
		":95a::CDEA//": req.CashDeliveryAgent,
	} {
		if party == "" {
			return fmt.Errorf("%w: %s party is required", ErrMT321Field, field)
		}
	}
	if req.SafekeepingAccount == "" {
		return fmt.Errorf("%w: :97A::SAFE// safekeeping account is required", ErrMT321Field)
	}
	return nil
}

// ApplyMT321 applies one parsed inbound SWIFT FIN/ISO 15022 MT321
// third-party loan/deposit settlement instruction: it matches the
// instruction against the booked term deposit the :20C::SEME//
// reference names, verifies every economic term exactly, and
// registers the instruction.
//
// The deposit must exist (SEME unknown is ErrTermDepositNotFound,
// fail-closed — an instruction never opens a deposit) and be
// active or matured (an early-withdrawn deposit reports
// ErrTermDepositNotActive, the export's gate). Every economic term
// must agree exactly with the booked contract — the :20C::LDCR//
// common reference against mt320CommonReference recomputed from
// the stored depositor identifier, the message's :95P::LDCO//
// counterparty BIC, and the :20C::SEME// deposit ID; the
// :98A::TRAD// and :98A::VALU// dates against the deposit's
// OpenedAt value date and the :98A::MATU// date against
// MaturesAt; the :19A::PRIN// principal against AmountCents; the
// :19A::SETT// settlement amount against the principal (interest
// settles separately at maturity); and the :92A::INTR// rate
// against RateBps (half-up rounded to 12 decimal places) — or
// the whole instruction is refused with ErrMT321TermsMismatch:
// there is no partial instruction. The :95a::INVE// investor and
// :95a::CDEA// cash delivery agent must name the stored
// depositor in the same form, the :95a::LDCO// counterparty must
// be a BIC, the :95a::ACCW// account-with institution must name
// that same counterparty BIC, and the :97A::SAFE// safekeeping
// account must name the deposit's safekeeping account. No cash
// moves, no deposit is created, and the contract's lifecycle
// does not move: the instruction is registered in the
// mt321Applied SEME registry and one
// `mt321_settlement_instruction_registered` audit event (details
// msg_id / common_ref / deposit_id) is emitted. Execution still
// arrives separately through the existing settlement path.
//
// Idempotency is the SEME with its terms (the pain.014
// convention): a replay with the same LDCR, economics, and
// settlement lines returns the stored instruction with Duplicate
// set, recording nothing twice and emitting no second audit
// event; a SEME reused for different terms is refused with
// ErrMT321MsgIDConflict.
//
// Locking: the whole apply runs under the write lock — the
// replay read, the deposit lookup, the term verification, the
// registry, and the audit event land atomically, so two
// concurrent applies of the same SEME still emit exactly one
// event.
func (l *Ledger) ApplyMT321(req MT321Instruction, actor string) (MT321ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT321"
	}
	if err := validateMT321Instruction(req); err != nil {
		return MT321ApplyResult{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// SEME replay: the instruction was already applied under
	// this reference. Same terms is a no-op returning the
	// stored instruction; different terms is a conflict.
	if rec, ok := l.mt321Applied[req.Reference]; ok {
		if !mt321TermsEqual(rec, req) {
			return MT321ApplyResult{}, fmt.Errorf("%w: :20C::SEME// %q", ErrMT321MsgIDConflict, req.Reference)
		}
		stored := rec.Instruction
		return MT321ApplyResult{Instruction: &stored, DepositID: rec.DepositID, Duplicate: true}, nil
	}

	// The deposit itself: :20C::SEME// names it. Unknown is
	// fail-closed — an instruction never opens a deposit.
	td, ok := l.termDeposits[req.Reference]
	if !ok {
		return MT321ApplyResult{}, fmt.Errorf("%w: no term deposit with ID %q", ErrTermDepositNotFound, req.Reference)
	}
	switch td.Status {
	case TermDepositActive, TermDepositMatured:
		// Instructable.
	case TermDepositWithdrawnEarly:
		return MT321ApplyResult{}, fmt.Errorf("%w: term deposit %q was withdrawn early", ErrTermDepositNotActive, req.Reference)
	default:
		return MT321ApplyResult{}, fmt.Errorf("%w: term deposit %q is %s", ErrTermDepositNotActive, req.Reference, td.Status)
	}
	// A tiered-rate deposit has no single rate to state in
	// :92A::INTR:: the instruction cannot express a ladder, so
	// it fails loud instead of picking one band's rate.
	if len(td.RateTiers) > 0 {
		return MT321ApplyResult{}, fmt.Errorf("%w: term deposit %q carries a tiered rate ladder, no single :92A::INTR: exists", ErrInvalidMT321Export, req.Reference)
	}

	// The parties: the :95a::INVE// investor and :95a::CDEA//
	// cash delivery agent must name the stored depositor in the
	// same form, the :95a::LDCO// counterparty must be a BIC
	// (the export always renders :95P::LDCO//), the :95a::ACCW//
	// account-with institution must name that same counterparty
	// BIC, and the :97A::SAFE// safekeeping account must name
	// the deposit's safekeeping account (the registered IBAN
	// when one exists, else the account ID, uppercased).
	depositorID, depositorBIC := l.mt300PartyIdentifierLocked(td.Account)
	if req.Investor != depositorID || req.InvestorBIC != depositorBIC {
		return MT321ApplyResult{}, fmt.Errorf("%w: :95a::INVE// holds %q, deposit names %q", ErrMT321TermsMismatch, req.Investor, depositorID)
	}
	if req.CashDeliveryAgent != depositorID || req.CashDeliveryAgentBIC != depositorBIC {
		return MT321ApplyResult{}, fmt.Errorf("%w: :95a::CDEA// holds %q, deposit names %q", ErrMT321TermsMismatch, req.CashDeliveryAgent, depositorID)
	}
	if !req.CounterpartyBIC {
		return MT321ApplyResult{}, fmt.Errorf("%w: :95a::LDCO// must be a BIC (:95P::LDCO//), got %q", ErrMT321TermsMismatch, req.Counterparty)
	}
	if req.AccountWith != req.Counterparty || !req.AccountWithBIC {
		return MT321ApplyResult{}, fmt.Errorf("%w: :95a::ACCW// holds %q, counterparty is %q", ErrMT321TermsMismatch, req.AccountWith, req.Counterparty)
	}
	safeAccount := strings.ToUpper(string(td.Account))
	if reg, ok := l.accountIBANs[td.Account]; ok && reg.IBAN != "" {
		safeAccount = reg.IBAN
	}
	if req.SafekeepingAccount != safeAccount {
		return MT321ApplyResult{}, fmt.Errorf("%w: :97A::SAFE// holds %q, deposit safekeeping account is %q", ErrMT321TermsMismatch, req.SafekeepingAccount, safeAccount)
	}

	// Every economic term, exactly: the :20C::LDCR// common
	// reference against the stored derivation, the
	// :98A::TRAD// and :98A::VALU// dates against the opening
	// day and the :98A::MATU// date against the maturity day,
	// the :19A::PRIN// principal against the stored principal,
	// the :19A::SETT// settlement amount against the principal,
	// and the :92A::INTR// rate against the locked rate. Any
	// disagreement refuses the whole instruction.
	wantCommonRef, err := mt320CommonReference(depositorID, req.Counterparty, req.Reference)
	if err != nil {
		// The derivation cannot fail on a deposit the export
		// could render — but if the stored identifiers do not
		// fit the shape, the claimed LDCR can never equal a
		// value that does not exist.
		return MT321ApplyResult{}, fmt.Errorf("%w: :20C::LDCR// cannot be derived for deposit %q: %v", ErrMT321TermsMismatch, req.Reference, err)
	}
	if req.CommonReference != wantCommonRef {
		return MT321ApplyResult{}, fmt.Errorf("%w: :20C::LDCR// holds %q, deposit derives %q", ErrMT321TermsMismatch, req.CommonReference, wantCommonRef)
	}
	if mt321Day(req.TradeDate) != mt321Day(td.OpenedAt) {
		return MT321ApplyResult{}, fmt.Errorf("%w: :98A::TRAD// holds %q, deposit opened %q",
			ErrMT321TermsMismatch, mt321Day(req.TradeDate), mt321Day(td.OpenedAt))
	}
	if mt321Day(req.ValueDate) != mt321Day(td.OpenedAt) {
		return MT321ApplyResult{}, fmt.Errorf("%w: :98A::VALU// holds %q, deposit opened %q",
			ErrMT321TermsMismatch, mt321Day(req.ValueDate), mt321Day(td.OpenedAt))
	}
	if mt321Day(req.MaturityDate) != mt321Day(td.MaturesAt) {
		return MT321ApplyResult{}, fmt.Errorf("%w: :98A::MATU// holds %q, deposit matures %q",
			ErrMT321TermsMismatch, mt321Day(req.MaturityDate), mt321Day(td.MaturesAt))
	}
	if req.PrincipalCurrency != td.Currency {
		return MT321ApplyResult{}, fmt.Errorf("%w: :19A::PRIN// holds currency %q, deposit is %q",
			ErrMT321TermsMismatch, req.PrincipalCurrency, td.Currency)
	}
	if req.PrincipalCents != td.AmountCents {
		return MT321ApplyResult{}, fmt.Errorf("%w: :19A::PRIN// holds %d cents, deposit principal is %d cents",
			ErrMT321TermsMismatch, req.PrincipalCents, td.AmountCents)
	}
	if req.SettlementCurrency != td.Currency {
		return MT321ApplyResult{}, fmt.Errorf("%w: :19A::SETT// holds currency %q, deposit is %q",
			ErrMT321TermsMismatch, req.SettlementCurrency, td.Currency)
	}
	if req.SettlementCents != td.AmountCents {
		return MT321ApplyResult{}, fmt.Errorf("%w: :19A::SETT// holds %d cents, deposit principal is %d cents",
			ErrMT321TermsMismatch, req.SettlementCents, td.AmountCents)
	}
	gotRate, err := mt321ParseRateScaled(req.Rate)
	if err != nil {
		return MT321ApplyResult{}, err
	}
	if gotRate.Cmp(mt320RateScaled(td.RateBps)) != 0 {
		want, _ := mt320RateBps(td.RateBps)
		return MT321ApplyResult{}, fmt.Errorf("%w: :92A::INTR// holds %q, deposit locks %q", ErrMT321TermsMismatch, req.Rate, want)
	}

	// The registration: the SEME record plus one audit event,
	// under the one write lock. The instruction is already
	// verified when the record lands.
	stored := req
	if l.mt321Applied == nil {
		l.mt321Applied = make(map[string]mt321AppliedRecord)
	}
	l.mt321Applied[req.Reference] = mt321AppliedRecord{
		Reference:   req.Reference,
		Instruction: stored,
		DepositID:   td.ID,
		Actor:       actor,
		ReceivedAt:  time.Now(),
	}
	l.emitAudit(AuditEvent{
		Op:            "mt321_settlement_instruction_registered",
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
	return MT321ApplyResult{Instruction: &stored, DepositID: td.ID}, nil
}

// GetMT321Instruction returns the registered settlement
// instruction for a :20C::SEME// reference, and whether one was
// registered. The registry is the instruction record: an unknown
// reference reports nothing.
func (l *Ledger) GetMT321Instruction(reference string) (MT321Instruction, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt321Applied[reference]
	if !ok {
		return MT321Instruction{}, false
	}
	return rec.Instruction, true
}
