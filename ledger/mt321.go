package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// SWIFT FIN/ISO 15022 MT321 third-party loan/deposit settlement
// instruction export (LG-396, fintech: SWIFT 第三方存贷结算指示
// 报文).
//
// Where MT320 (LG-392) confirms a term deposit bilaterally, MT321
// instructs the fund's custodian to settle one: the fund manager
// tells the custodian the trade details and instructs settlement
// of a fixed term loan/deposit done with a third-party financial
// institution (iotafinance MT321 scope: "sent by a fund manager
// to the fund's custodian to advise the trade details and instruct
// the settlement of a fixed term loan/deposit done with a third
// party financial institution"). It is the deposit-side counterpart
// of MT304 (LG-390), which instructs the same custodian to settle
// a third-party FX deal — same "fund manager -> custodian"
// direction, different subject.
//
// This is the repo's first ISO 15022 syntax export: the message is
// built from 16R blocks and qualified generic fields
// (`:4!c//...`), not from the FIN `:15A:` sequence structure the
// MT300/MT304/MT320 exports use. The three blocks are GENL
// (identification), TRADDET (trade details), and SETT (settlement
// instructions):
//
//   - GENL identifies the instruction: `:20C::SEME//` the deposit
//     ID (the sender's reference), `:20C::LDCR//` the MT320
//     `:22C:` common reference — re-derived here so the custodian
//     can match this instruction against the MT320 confirmation —
//     `:23G:NEWM` (new messages only; CANC is a later round), and
//     `:22H::TLDE//CONF`, the instruction to settle a first
//     confirmation (liquidation and rollover codes are later
//     rounds).
//   - TRADDET states the economics: `:20C::CONU//` the contract
//     number (the deposit ID — the ledger holds no separate
//     message-number or contract-number field, so SEME and CONU
//     take the same value with different meanings, like the MT304
//     `:83a:`/`:82a:` shared identifier), `:22H::DPLO//LEND`
//     (Party A, the depositor, is the lender, like MT320's
//     `:17R:L`), `:22H::MICO//A009` (Actual/365 fixed — the same
//     day-count as MT320's `:14D:ACT/365`), `:98A::TRAD//` and
//     `:98A::VALU//` the opening date, `:98A::MATU//` the maturity
//     date, `:19A::PRIN//` the principal, `:19A::SETT//` the
//     amount to settle now — the principal alone, interest
//     settles separately at maturity — `:92A::INTR//` the flat
//     rate, `:95a::LDCO//` the counterparty (always the P option,
//     its BIC), `:95a::INVE//` the investor, i.e. the fund, i.e.
//     the depositor (P option with a registered BIC, else the Q
//     option name-and-address line with the account ID), and
//     `:97A::SAFE//` the safekeeping account (the registered IBAN
//     when one exists, else the account ID).
//   - SETT states how the principal moves: `:22H::PRIT//APFM`
//     (settlement instructions for amounts payable by the
//     investor — the fund pays the principal to the counterparty),
//     `:95a::ACCW//` the account-with institution (always the P
//     option: the counterparty BIC, the payee bank) and
//     `:95a::CDEA//` the cash delivery agent (the depositor side:
//     P option with a registered BIC, else the Q option).
//
// Omitted on purpose: `:99B:`/`:13a:` (no linked-instruction
// numbering), `:20C::POOL/PREV/RELA` (a single instruction, no
// linked message), `:19A::CINT:/NINT:/RODI:` (no current/next
// interest or rollover increase/decrease at first confirmation),
// `:99B::DAAC:` (no accrued days at first confirmation),
// `:95a::EXBO:/MEOR:` (no separate executing broker or originator
// — the counterparty is the executing party), `:97A::CASH:` (no
// cash account separate from the safekeeping account), and
// `:70C::PACO:` (no contact narrative data).
//
// The 4!c indicator codes are pinned to the SWIFT UHB MT321 field
// rules (as mirrored by iotafinance's MT321 structure table, whose
// code cells name the values the crawl's text view drops —
// verified from the page's raw HTML on 2026-11-12): TLDE CONF is
// "an instruction to settle a first confirmation" (MATU =
// liquidation, ROLL = rollover); DPLO LEND is "the lender (pays
// the principal amount and receives the interest)" (BORR = the
// borrower); PRIT APFM is "Settlement Instructions for Amounts
// Payable by the Investor" (APCP = amounts payable by the
// counterparty, IPCP/IPFM = interests payable by the counterparty
// / investor); MICO A009 is "Actual/365 (fixed)" in MT321's
// five-code 22H list (A001 30/360, A004 Actual/360, A007 30E/360,
// A008 Actual/365 or Actual/Actual, A009 Actual/365 fixed) —
// note the expanded 22F MICO list used by MT558/MT569 assigns the
// same meaning to A005, so the MT321-specific list governs here.
//
// Blocks 1/2/3 follow the MT300 framing (mt300.go:167-173) with
// the message type swapped to I321: block 1 carries the
// depositor's LT, block 2 `{2:I321<custodianLT>U3003}` the
// custodian's, block 3 `{108:MUR}` the deposit ID. There is no
// block 5, lines join with "\n", and there is no trailing newline.
//
// The export is read-only: it takes the read lock only (via
// GetTermDeposit and the identifier lookup), bumps no version,
// books nothing, and appends no audit event. Active and matured
// deposits instruct; an early-withdrawn deposit reports
// ErrTermDepositNotActive. A tiered-rate deposit (LG-145) cannot
// be instructed: no single `:92A::INTR:` exists to write, so it
// fails loud with ErrInvalidMT321Export rather than stating a
// rate the contract does not have. Repeat exports are
// byte-identical: every field is pinned to the stored deposit and
// the caller-given options, no wall clock enters the output.

// ErrInvalidMT321Export is returned when a term deposit cannot be
// rendered as an MT321: a :20C: reference outside 16x, a :20C:
// LDCR component that does not fit 4!a2!c4!n4!a2!c, a custodian or
// counterparty BIC that is not a valid 8/11 BIC, a party
// identifier that is not a valid LT form, a :95Q:/:97A: account ID
// outside the SWIFT `x` character set or over 35x, a tiered-rate
// deposit with no single rate, or a non-positive rate. It is a
// 400-class error: the stored deposit cannot be expressed as a
// valid settlement instruction.
var ErrInvalidMT321Export = errors.New("ledger: invalid MT321 export")

// mt321TLDEFirstConfirmation is the :22H::TLDE// indicator for an
// instruction to settle a first confirmation (SWIFT UHB MT321,
// field 22H qualifier TLDE: CONF first confirmation, MATU
// liquidation, ROLL rollover/renewal).
const mt321TLDEFirstConfirmation = "CONF"

// mt321DPLOLender is the :22H::DPLO// indicator for the sender
// being the lender (SWIFT UHB MT321, field 22H qualifier DPLO:
// LEND lender, BORR borrower).
const mt321DPLOLender = "LEND"

// mt321MICOActual365Fixed is the :22H::MICO// indicator for the
// Actual/365 (fixed) day-count (SWIFT UHB MT321, field 22H
// qualifier MICO: A001 30/360, A004 Actual/360, A007 30E/360,
// A008 Actual/365 or Actual/Actual, A009 Actual/365 fixed).
const mt321MICOActual365Fixed = "A009"

// mt321PRITAmountsPayableByInvestor is the :22H::PRIT//
// indicator for settlement instructions for amounts payable by
// the investor (SWIFT UHB MT321, field 22H qualifier PRIT: APCP
// amounts payable by the counterparty, APFM amounts payable by
// the investor, IPCP interests payable by the counterparty, IPFM
// interests payable by the investor).
const mt321PRITAmountsPayableByInvestor = "APFM"

// MT321ExportOptions tunes ExportMT321.
//
// DepositID names the term deposit to settle. CustodianBIC is the
// fund custodian's BIC (required, 8 or 11 characters): the
// instruction's receiver — the ledger holds no bank-identity
// registry, so the caller names the custodian on every export.
// CounterpartyBIC is the loan/deposit counterparty's BIC
// (required, 8 or 11 characters): Party B of the deposit, the
// institution the principal is paid to.
type MT321ExportOptions struct {
	DepositID       string
	CustodianBIC    string
	CounterpartyBIC string
}

// ExportMT321 renders the term deposit as a SWIFT FIN/ISO 15022
// MT321 third-party loan/deposit settlement instruction. Unknown
// IDs report ErrTermDepositNotFound; an early-withdrawn deposit
// reports ErrTermDepositNotActive. Active and matured deposits
// export.
func (l *Ledger) ExportMT321(opts MT321ExportOptions) (string, error) {
	// Caller-given options fail fast, before any deposit lookup:
	// a custodian or counterparty that is not a valid BIC can
	// never be a valid instruction.
	custodian := strings.ToUpper(strings.TrimSpace(opts.CustodianBIC))
	if custodian == "" || !validBIC(custodian) {
		return "", fmt.Errorf("%w: custodian BIC %q: want a valid 8/11-character BIC", ErrInvalidMT321Export, opts.CustodianBIC)
	}
	counterparty := strings.ToUpper(strings.TrimSpace(opts.CounterpartyBIC))
	if counterparty == "" || !validBIC(counterparty) {
		return "", fmt.Errorf("%w: counterparty BIC %q: want a valid 8/11-character BIC", ErrInvalidMT321Export, opts.CounterpartyBIC)
	}
	td, ok := l.GetTermDeposit(opts.DepositID)
	if !ok {
		return "", ErrTermDepositNotFound
	}
	switch td.Status {
	case TermDepositActive, TermDepositMatured:
		// Exportable.
	case TermDepositWithdrawnEarly:
		return "", fmt.Errorf("%w: term deposit %q was withdrawn early", ErrTermDepositNotActive, opts.DepositID)
	default:
		return "", fmt.Errorf("%w: term deposit %q is %s", ErrTermDepositNotActive, opts.DepositID, td.Status)
	}
	// A tiered-rate deposit has no single rate to state in
	// :92A::INTR:: the instruction cannot express a ladder, so
	// it fails loud instead of picking one band's rate.
	if len(td.RateTiers) > 0 {
		return "", fmt.Errorf("%w: term deposit %q carries a tiered rate ladder, no single :92A::INTR: exists", ErrInvalidMT321Export, opts.DepositID)
	}
	// The depositor's best-known identifier (registered BIC, else
	// the internal account ID, uppercased) and the safekeeping
	// account (the registered IBAN when one exists, else the
	// account ID, uppercased) — read under the lock, like the
	// MT300 identifier lookup.
	l.mu.RLock()
	depositorID, depositorBIC := l.mt300PartyIdentifierLocked(td.Account)
	safeAccount := strings.ToUpper(string(td.Account))
	if reg, ok := l.accountIBANs[td.Account]; ok && reg.IBAN != "" {
		safeAccount = reg.IBAN
	}
	l.mu.RUnlock()

	// :20C::SEME// and :20C::CONU// are the deposit ID, 16x.
	if err := mt321TransactionReference(td.ID); err != nil {
		return "", err
	}
	// :20C::LDCR// re-derives the MT320 :22C: common reference
	// from the depositor, the counterparty, and the deposit ID's
	// last 4 digits — the key the custodian matches this
	// instruction against the MT320 confirmation with.
	commonRef, err := mt321CommonReference(depositorID, counterparty, td.ID)
	if err != nil {
		return "", err
	}
	// Blocks 1/2 LTs derive from the same identifiers: the sender
	// LT is the depositor, the receiver LT the custodian BIC — a
	// depositor without a registered BIC needs an LT-form account
	// ID (8, 11, or 12 alphanumeric characters) to export at all;
	// anything else fails loud here rather than emitting a
	// malformed header.
	senderLT, err := mt103NormalizeLT(depositorID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT321Export, depositorID, err)
	}
	receiverLT, err := mt103NormalizeLT(custodian)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT321Export, custodian, err)
	}
	// A :95Q:/:97A: account ID must be SWIFT `x` and fit 35x.
	if !depositorBIC {
		if err := mt103CheckX(depositorID); err != nil {
			return "", fmt.Errorf("%w: :95Q: account ID %q: %w", ErrInvalidMT321Export, depositorID, err)
		}
	}
	if len(safeAccount) > 35 {
		return "", fmt.Errorf("%w: :97A::SAFE// account %q exceeds 35x", ErrInvalidMT321Export, safeAccount)
	}
	if err := mt103CheckX(safeAccount); err != nil {
		return "", fmt.Errorf("%w: :97A::SAFE// account %q: %w", ErrInvalidMT321Export, safeAccount, err)
	}
	// :92A::INTR// is the flat rate, exact to 12 decimals (see
	// mt321RateBps).
	rate, err := mt321RateBps(td.RateBps)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I321. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I321%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20C::SEME//
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", td.ID)
	// Block 4: the field set, in canonical order. Sequence GENL
	// identifies the instruction; sequence TRADDET states the
	// economics (the depositor lends, so :22H::DPLO// is LEND);
	// sequence SETT states how the principal moves (the investor
	// pays, so :22H::PRIT// is APFM).
	b.WriteString("{4:\n")
	b.WriteString(":16R:GENL\n")
	fmt.Fprintf(&b, ":20C::SEME//%s\n", td.ID)
	fmt.Fprintf(&b, ":20C::LDCR//%s\n", commonRef)
	b.WriteString(":23G:NEWM\n")
	fmt.Fprintf(&b, ":22H::TLDE//%s\n", mt321TLDEFirstConfirmation)
	b.WriteString(":16S:GENL\n")
	b.WriteString(":16R:TRADDET\n")
	fmt.Fprintf(&b, ":20C::CONU//%s\n", td.ID)
	fmt.Fprintf(&b, ":22H::DPLO//%s\n", mt321DPLOLender)
	fmt.Fprintf(&b, ":22H::MICO//%s\n", mt321MICOActual365Fixed)
	fmt.Fprintf(&b, ":98A::TRAD//%s\n", mt320Date(td.OpenedAt))
	fmt.Fprintf(&b, ":98A::VALU//%s\n", mt320Date(td.OpenedAt))
	fmt.Fprintf(&b, ":98A::MATU//%s\n", mt320Date(td.MaturesAt))
	fmt.Fprintf(&b, ":19A::PRIN//%s%s\n", td.Currency, mt940Amount(td.AmountCents))
	fmt.Fprintf(&b, ":19A::SETT//%s%s\n", td.Currency, mt940Amount(td.AmountCents))
	fmt.Fprintf(&b, ":92A::INTR//%s\n", rate)
	fmt.Fprintf(&b, ":95P::LDCO//%s\n", counterparty)
	if depositorBIC {
		fmt.Fprintf(&b, ":95P::INVE//%s\n", depositorID)
	} else {
		fmt.Fprintf(&b, ":95Q::INVE//%s\n", depositorID)
	}
	fmt.Fprintf(&b, ":97A::SAFE//%s\n", safeAccount)
	b.WriteString(":16S:TRADDET\n")
	b.WriteString(":16R:SETT\n")
	fmt.Fprintf(&b, ":22H::PRIT//%s\n", mt321PRITAmountsPayableByInvestor)
	fmt.Fprintf(&b, ":95P::ACCW//%s\n", counterparty)
	if depositorBIC {
		fmt.Fprintf(&b, ":95P::CDEA//%s\n", depositorID)
	} else {
		fmt.Fprintf(&b, ":95Q::CDEA//%s\n", depositorID)
	}
	b.WriteString(":16S:SETT\n")
	b.WriteString("-}")
	return b.String(), nil
}

// mt321TransactionReference validates the :20C: references (the
// deposit ID, used for both SEME and CONU): 1–16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash. It mirrors
// mt300TransactionReference but wraps ErrInvalidMT321Export —
// the MT300 helper's error would map to the wrong message type in
// the handler.
func mt321TransactionReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :20C: want 1-%d characters, got %q", ErrInvalidMT321Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :20C: line break in a single-line field: %q", ErrInvalidMT321Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :20C: %w", ErrInvalidMT321Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :20C: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT321Export, ref)
	}
	return nil
}

// mt321CommonReference derives the :20C::LDCR// deposit common
// reference: the MT300 common reference (first 4 of the depositor
// identifier, 4!a letters, + first 2 of the counterparty BIC, 2!c,
// + the last 4 digits of the deposit ID, 4!n) + the first 4 of the
// counterparty BIC (4!a) + the first 2 of the depositor identifier
// (2!c) — 16 characters in the SWIFT Category 3 MRG shape (e.g.
// CHAS335928GRIN2L). It is the same derivation as
// mt320CommonReference (the MT320 :22C:), re-wrapped in
// ErrInvalidMT321Export — the MT320 helper must never be reused
// here, its error would map to the wrong message type in the
// handler. Identifiers are uppercased before slicing; any
// component that does not fit fails loud — an instruction must
// never carry a reference the custodian cannot match against the
// MT320 confirmation.
func mt321CommonReference(depositorID, counterpartyBIC, depositID string) (string, error) {
	base, err := mt300CommonReference(depositorID, counterpartyBIC, depositID)
	if err != nil {
		return "", fmt.Errorf("%w: :20C::LDCR//: %v", ErrInvalidMT321Export, err)
	}
	counterpartyBIC = strings.ToUpper(counterpartyBIC)
	depositorID = strings.ToUpper(depositorID)
	if len(counterpartyBIC) < 4 || len(depositorID) < 2 {
		return "", fmt.Errorf("%w: :20C::LDCR//: needs a counterparty BIC of at least 4 characters and a depositor identifier of at least 2 (got %q, %q)", ErrInvalidMT321Export, counterpartyBIC, depositorID)
	}
	first4 := counterpartyBIC[:4]
	for i := 0; i < 4; i++ {
		if first4[i] < 'A' || first4[i] > 'Z' {
			return "", fmt.Errorf("%w: :20C::LDCR//: counterparty BIC %q does not start with 4 letters", ErrInvalidMT321Export, counterpartyBIC)
		}
	}
	first2 := depositorID[:2]
	for i := 0; i < 2; i++ {
		c := first2[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", fmt.Errorf("%w: :20C::LDCR//: depositor identifier %q does not start with 2 alphanumeric characters", ErrInvalidMT321Export, depositorID)
		}
	}
	return base + first4 + first2, nil
}

// mt321RateBps renders RateBps/10000 as a decimal with a comma
// separator, half-up rounded at the 12th decimal place. The
// rounding is exact big.Int arithmetic, never float — the same
// math as mt320RateBps, wrapped in ErrInvalidMT321Export instead
// of ErrInvalidMT320Export (see mt320RateBps for the derivation).
// A flat rate of at most 10000 bps renders within the 15d field
// length (whole part at most "1" plus a comma plus 12 fraction
// digits).
func mt321RateBps(bps int64) (string, error) {
	if bps <= 0 {
		return "", fmt.Errorf("%w: rate %d bps must be positive", ErrInvalidMT321Export, bps)
	}
	const scale = 1000000000000 // 10^12
	const den = 10000
	n := big.NewInt(bps)
	d := big.NewInt(den)
	scaled := new(big.Int).Mul(n, big.NewInt(scale))
	scaled.Mul(scaled, big.NewInt(2))
	scaled.Add(scaled, d)
	scaled.Quo(scaled, new(big.Int).Mul(d, big.NewInt(2)))
	whole, frac := new(big.Int).QuoRem(scaled, big.NewInt(scale), new(big.Int))
	fracStr := fmt.Sprintf("%012d", frac)
	fracStr = strings.TrimRight(fracStr, "0")
	if fracStr == "" {
		fracStr = "0"
	}
	return whole.String() + "," + fracStr, nil
}
