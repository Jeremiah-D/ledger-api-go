package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN MT320 fixed loan/deposit confirmation export (LG-392,
// fintech: SWIFT 定期贷款/存款确认报文).
//
// The MT300 (LG-388) confirms an FX forward and the MT304 (LG-390)
// instructs a custodian to settle one; this module confirms the
// ledger's other standing contract — a term deposit (see
// termdeposit.go, LG-90) — as a Category 3 MT320 "Fixed
// Loan/Deposit Confirmation". A term deposit is a bilateral loan in
// substance: the depositor (Party A) lends principal to the
// receiving institution (Party B) at a fixed rate for a fixed tenor,
// so the confirmation states the lender, the borrower, the value
// and maturity dates, the principal, the total contracted interest,
// and the rate.
//
// Sequence A identifies the contract (:15A: :20: = the deposit ID,
// :22A: always NEWT, :22B: always CONF — this endpoint confirms new
// contracts only, AMND/CANC variants are a later round — :22C: the
// 16-character common reference, :82a: Party A the depositor,
// :87a: Party B the counterparty). Sequence B states the economics
// (:15B: :17R: always L — Party A is the lender — :30T: the trade
// date = the opening date, :30V: the value date = the opening date,
// a deposit starts earning on the day it opens, :30P: the maturity
// date, :32B: the principal, :34E: the total contracted interest,
// :37G: the rate, :14D: always ACT/365). Sequence C names the bank
// that receives Party A's principal (:15C: :57a: = the counterparty
// BIC) and sequence D the bank that receives Party B's
// principal-plus-interest (:15D: :57a: = the depositor). There is
// no :21: — there is no prior confirmation to reference — and
// :94A:/:21N:/:83a:/:77D:/:32H:/:30X:/:30F:/:38J: and sequences
// F/G/H/I are omitted: no scope, contract-number, fund-identity,
// terms-text, separate settlement-amount, next-interest-date,
// single-period, day-count-extra, tax, broker, or additional-amount
// data exists here.
//
// :22C: is a deterministic derivation, not a stored field: the
// MT300 common reference (first 4 of the depositor identifier +
// first 2 of the counterparty BIC + the last 4 digits of the
// deposit ID) + the first 4 of the counterparty BIC + the first 2
// of the depositor identifier, format 4!a2!c4!n4!a2!c — the SWIFT
// Category 3 MRG shape (e.g. CHAS335928GRIN2L). Any component that
// does not fit fails loud with ErrInvalidMT320Export.
//
// :34E: is the contracted interest total: TermDays x the daily
// interest (see termDepositDailyInterestCents — floor(principal x
// rateBps / 10000 / 365), the same math as the daily accrual), on
// an ACT/365 basis. The business-day-only accrual gate (see
// businessday.go) changes only the day-by-day booking rhythm, never
// this contracted total: a gate-on export renders the same :34E:
// as a gate-off one.
//
// Party identifiers follow the MT300 convention (see
// mt300PartyIdentifierLocked): the depositor's registered BIC when
// one exists, else the internal account ID, uppercased. A BIC
// party renders :82A:/:57A:; an account-ID party renders :82D:
// /:57D: with the account ID, which must pass mt103CheckX. The
// counterparty is caller-given on every export (the ledger holds
// no bank-identity registry) and is always :87A:/:57A:. Blocks 1/2
// take the same identifiers through mt103NormalizeLT — the sender
// LT is the depositor, the receiver LT the counterparty BIC — so a
// depositor without a registered BIC needs an LT-form account ID
// (8, 11, or 12 alphanumeric characters) to export at all;
// anything else fails loud rather than emitting a malformed header.
//
// The export is read-only: it takes the read lock only (via
// GetTermDeposit and the identifier lookup), bumps no version,
// books nothing, and appends no audit event. Active and matured
// deposits confirm; an early-withdrawn deposit reports
// ErrTermDepositNotActive. A tiered-rate deposit (LG-145) cannot
// be confirmed: no single :37G: exists to write, so it fails loud
// with ErrInvalidMT320Export rather than stating a rate the
// contract does not have. Repeat exports are byte-identical: every
// field is pinned to the stored deposit and the caller-given
// options, no wall clock enters the output.

// ErrInvalidMT320Export is returned when a term deposit cannot be
// rendered as an MT320: a :20: reference outside 16x, a :22C:
// component that does not fit 4!a2!c4!n4!a2!c, a counterparty BIC
// that is not a valid 8/11 BIC, a party identifier that is not a
// valid LT form, a :82D:/:57D: account ID outside the SWIFT `x`
// character set, a tiered-rate deposit with no single rate, or a
// non-positive rate. It is a 400-class error: the stored deposit
// cannot be expressed as a valid confirmation.
var ErrInvalidMT320Export = errors.New("ledger: invalid MT320 export")

// MT320ExportOptions tunes ExportMT320.
//
// DepositID names the term deposit to confirm. CounterpartyBIC is
// the deposit-receiving institution's BIC (required, 8 or 11
// characters): the ledger holds no bank-identity registry, so the
// caller names the counterparty on every export.
type MT320ExportOptions struct {
	DepositID       string
	CounterpartyBIC string
}

// ExportMT320 renders the term deposit as a SWIFT FIN MT320 fixed
// loan/deposit confirmation. Unknown IDs report
// ErrTermDepositNotFound; an early-withdrawn deposit reports
// ErrTermDepositNotActive. Active and matured deposits export.
func (l *Ledger) ExportMT320(opts MT320ExportOptions) (string, error) {
	// Caller-given options fail fast, before any deposit lookup: a
	// counterparty that is not a valid BIC can never be a valid
	// confirmation.
	counterparty := strings.ToUpper(strings.TrimSpace(opts.CounterpartyBIC))
	if counterparty == "" || !validBIC(counterparty) {
		return "", fmt.Errorf("%w: counterparty BIC %q: want a valid 8/11-character BIC", ErrInvalidMT320Export, opts.CounterpartyBIC)
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
	// A tiered-rate deposit has no single rate to state in :37G::
	// the confirmation cannot express a ladder, so it fails loud
	// instead of picking one band's rate.
	if len(td.RateTiers) > 0 {
		return "", fmt.Errorf("%w: term deposit %q carries a tiered rate ladder, no single :37G: exists", ErrInvalidMT320Export, opts.DepositID)
	}
	// The depositor's best-known identifier (registered BIC, else
	// the internal account ID, uppercased) — read under the lock,
	// like the MT300 identifier lookup.
	l.mu.RLock()
	depositorID, depositorBIC := l.mt300PartyIdentifierLocked(td.Account)
	l.mu.RUnlock()

	// :20: is the deposit ID, 16x.
	if err := mt320TransactionReference(td.ID); err != nil {
		return "", err
	}
	// :22C: derives deterministically from the depositor, the
	// counterparty, and the deposit ID's last 4 digits.
	commonRef, err := mt320CommonReference(depositorID, counterparty, td.ID)
	if err != nil {
		return "", err
	}
	// Blocks 1/2 LTs derive from the same identifiers: a depositor
	// without a registered BIC needs an LT-form account ID (8, 11,
	// or 12 alphanumeric characters) — anything else fails loud
	// here rather than emitting a malformed header.
	senderLT, err := mt103NormalizeLT(depositorID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT320Export, depositorID, err)
	}
	receiverLT, err := mt103NormalizeLT(counterparty)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT320Export, counterparty, err)
	}
	// A :82D:/:57D: account ID must be SWIFT `x`.
	if !depositorBIC {
		if err := mt103CheckX(depositorID); err != nil {
			return "", fmt.Errorf("%w: :82D:/:57D: account ID %q: %w", ErrInvalidMT320Export, depositorID, err)
		}
	}
	// :37G: is the flat rate, exact to 12 decimals (see
	// mt320RateBps); :34E: is the contracted interest total —
	// TermDays x one day's interest, computed in big.Int so a long
	// tenor can never overflow the int64 day figure.
	rate, err := mt320RateBps(td.RateBps)
	if err != nil {
		return "", err
	}
	daily, err := termDepositDailyInterestCents(td)
	if err != nil {
		return "", fmt.Errorf("%w: daily interest for term deposit %q: %w", ErrInvalidMT320Export, td.ID, err)
	}
	totalInterest := new(big.Int).Mul(big.NewInt(daily), big.NewInt(td.TermDays))

	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I320. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I320%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", td.ID)
	// Block 4: the field set, in canonical order. Sequence A
	// identifies the contract; sequence B states the economics
	// (the depositor lends, so :17R: is L); sequence C names the
	// bank receiving the principal from Party A (the counterparty);
	// sequence D names the bank receiving principal-plus-interest
	// from Party B (the depositor).
	b.WriteString("{4:\n")
	b.WriteString(":15A:\n")
	fmt.Fprintf(&b, ":20:%s\n", td.ID)
	b.WriteString(":22A:NEWT\n")
	b.WriteString(":22B:CONF\n")
	fmt.Fprintf(&b, ":22C:%s\n", commonRef)
	if depositorBIC {
		fmt.Fprintf(&b, ":82A:%s\n", depositorID)
	} else {
		fmt.Fprintf(&b, ":82D:%s\n", depositorID)
	}
	fmt.Fprintf(&b, ":87A:%s\n", counterparty)
	b.WriteString(":15B:\n")
	b.WriteString(":17R:L\n")
	fmt.Fprintf(&b, ":30T:%s\n", mt320Date(td.OpenedAt))
	fmt.Fprintf(&b, ":30V:%s\n", mt320Date(td.OpenedAt))
	fmt.Fprintf(&b, ":30P:%s\n", mt320Date(td.MaturesAt))
	fmt.Fprintf(&b, ":32B:%s%s\n", td.Currency, mt940Amount(td.AmountCents))
	fmt.Fprintf(&b, ":34E:%s%s\n", td.Currency, mt300CentsAmount(totalInterest))
	fmt.Fprintf(&b, ":37G:%s\n", rate)
	b.WriteString(":14D:ACT/365\n")
	b.WriteString(":15C:\n")
	fmt.Fprintf(&b, ":57A:%s\n", counterparty)
	b.WriteString(":15D:\n")
	if depositorBIC {
		fmt.Fprintf(&b, ":57A:%s\n", depositorID)
	} else {
		fmt.Fprintf(&b, ":57D:%s\n", depositorID)
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt320TransactionReference validates the :20: reference (the
// deposit ID): 1–16 SWIFT `x` characters, no braces, no line breaks
// (a single-line field must not smuggle a second line into block
// 4), no leading/trailing colon or slash, no double slash. It
// mirrors mt300TransactionReference but wraps
// ErrInvalidMT320Export — the MT300 helper's error would map to
// the wrong message type in the handler.
func mt320TransactionReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :20: want 1-%d characters, got %q", ErrInvalidMT320Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :20: line break in a single-line field: %q", ErrInvalidMT320Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :20: %w", ErrInvalidMT320Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :20: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT320Export, ref)
	}
	return nil
}

// mt320CommonReference derives the :22C: common reference: the
// MT300 common reference (first 4 of the depositor identifier, 4!a
// letters, + first 2 of the counterparty BIC, 2!c, + the last 4
// digits of the deposit ID, 4!n) + the first 4 of the counterparty
// BIC (4!a) + the first 2 of the depositor identifier (2!c) —
// 16 characters in the SWIFT Category 3 MRG shape (e.g.
// CHAS335928GRIN2L). Identifiers are uppercased before slicing;
// any component that does not fit fails loud — a confirmation must
// never carry a reference the counterparty cannot match on.
func mt320CommonReference(depositorID, counterpartyBIC, depositID string) (string, error) {
	base, err := mt300CommonReference(depositorID, counterpartyBIC, depositID)
	if err != nil {
		return "", fmt.Errorf("%w: :22C: %v", ErrInvalidMT320Export, err)
	}
	counterpartyBIC = strings.ToUpper(counterpartyBIC)
	depositorID = strings.ToUpper(depositorID)
	if len(counterpartyBIC) < 4 || len(depositorID) < 2 {
		return "", fmt.Errorf("%w: :22C: needs a counterparty BIC of at least 4 characters and a depositor identifier of at least 2 (got %q, %q)", ErrInvalidMT320Export, counterpartyBIC, depositorID)
	}
	first4 := counterpartyBIC[:4]
	for i := 0; i < 4; i++ {
		if first4[i] < 'A' || first4[i] > 'Z' {
			return "", fmt.Errorf("%w: :22C: counterparty BIC %q does not start with 4 letters", ErrInvalidMT320Export, counterpartyBIC)
		}
	}
	first2 := depositorID[:2]
	for i := 0; i < 2; i++ {
		c := first2[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", fmt.Errorf("%w: :22C: depositor identifier %q does not start with 2 alphanumeric characters", ErrInvalidMT320Export, depositorID)
		}
	}
	return base + first4 + first2, nil
}

// mt320RateBps renders RateBps/10000 as a decimal with a comma
// separator, half-up rounded at the 12th decimal place. The
// rounding is exact big.Int arithmetic, never float — the same
// math as mt300Rate, wrapped in ErrInvalidMT320Export instead of
// ErrInvalidMT300Export (see mt300Rate for the derivation).
func mt320RateBps(bps int64) (string, error) {
	if bps <= 0 {
		return "", fmt.Errorf("%w: rate %d bps must be positive", ErrInvalidMT320Export, bps)
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

// mt320Date renders an 8!n date (YYYYMMDD, UTC). MT320 dates are
// full 8-digit dates — unlike MT300's :30T:/:30V:, which (as
// documented in LG-392) render 6!n through mt940ValueDate — so
// this helper exists instead of reusing that one.
func mt320Date(t time.Time) string {
	return t.UTC().Format("20060102")
}
