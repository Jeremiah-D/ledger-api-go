package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SWIFT FIN MT300 FX forward confirmation export (LG-388, fintech:
// SWIFT 外汇确认报文).
//
// A booked FX forward (see forward.go, LG-110) is a bilateral rate
// agreement; the MT300 is the industry-standard confirmation the
// counterparties exchange so each side can match the trade
// (:30T:/:30V:/:36:/:32B:/:33B: must agree exactly). This module
// renders one stored contract as a FIN MT300 message: sequence A
// identifies the trade (:15A: :20: :22A: :22C: :82A:/:82D:
// :87A:/:87D:), sequence B states the economics (:15B: :30T:
// :30V: :36: :32B: :33B: :57A:/:57D:). There is no :21: — there is
// no prior confirmation to reference — and :22A: is always NEWT:
// this endpoint confirms new trades only (AMND/CANC variants are a
// later round).
//
// Framing follows the MT199 convention (see mt199.go): blocks 1/2/3/4,
// block 3 `{108:MUR}` carrying the :20: reference, no block 5 (CHK
// is computed by the SWIFT network on receipt), lines joined with
// "\n", no trailing newline. Block 1 carries the From party's LT,
// block 2 `{2:I300<receiverLT>U3003}` the To party's.
//
// Party identifiers follow the camt.056 best-known-identifier
// convention (see ExportCancellationRequest): the account's
// registered BIC from accountIBANs when one exists and is valid (8
// or 11 characters, alphanumeric, first 4 letters), else the
// internal account ID, uppercased. A BIC party renders :82A:/
// :87A:/:57A:; an account-ID party renders :82D:/:87D:/:57D: with
// the account ID, which must pass mt103CheckX. Blocks 1/2 take the
// same identifier through mt103NormalizeLT — so an account without
// a registered BIC needs an LT-form account ID (8, 11, or 12
// alphanumeric characters) to export at all; anything else fails
// loud with ErrInvalidMT300Export rather than emitting a malformed
// header.
//
// :22C:, the common reference, is a deterministic derivation, not
// a stored field: first 4 characters of the From identifier +
// first 2 of the To identifier + the last 4 digits of the forward
// ID, format 4!a2!c4!n. Identifiers are uppercased before slicing;
// any component that does not fit (fewer than 4/2 characters, a
// non-letter in the first 4, a non-alphanumeric in the next 2, a
// non-digit in the forward ID's last 4) fails loud with
// ErrInvalidMT300Export.
//
// :36: renders the locked RateNum/RateDen as a decimal with a
// comma separator, half-up rounded at the 12th decimal place (see
// mt300Rate: the rounding is exact big.Int arithmetic, never
// float). :32B: is the bought amount — ToCurrency plus
// floor(NotionalCents*RateNum/RateDen) cents, rendered like
// mt940Amount — and :33B: is the sold amount — FromCurrency plus
// NotionalCents. :57A:/:57D: names the To account: the bought
// currency lands there.
//
// The export is read-only: the locked core takes the write lock
// only because the lazy-expiry check (see below) may persist a
// lapse, exactly like the Forward getter. The export itself bumps
// no version, books nothing, and appends no audit event. A booked
// contract past its settlement grace period lapses first (the
// forward_settle_at_amend.go lazy-expiry pattern), so it reports
// expired rather than exporting a dead agreement. Repeat exports
// are byte-identical: every field is pinned to the stored
// contract, no wall clock enters the output.

// ErrInvalidMT300Export is returned when a forward cannot be
// rendered as an MT300: a :20: reference outside 16x, a :22C:
// component that does not fit 4!a2!c4!n, a party identifier that
// is not a valid LT form, or a :82D:/:87D:/:57D: account ID outside
// the SWIFT `x` character set. It is a 400-class error: the stored
// contract cannot be expressed as a valid confirmation.
var ErrInvalidMT300Export = errors.New("ledger: invalid MT300 export")

// ExportMT300 renders the forward contract as a SWIFT FIN MT300
// confirmation message. Unknown IDs report ErrForwardNotFound; a
// manually cancelled contract reports ErrForwardNotBooked; a lapsed
// contract reports ErrForwardExpired. Booked and settled contracts
// export.
func (l *Ledger) ExportMT300(forwardID string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exportMT300Locked(forwardID, time.Now())
}

// exportMT300Locked is the lock-held core of ExportMT300; now is
// the export time (time.Now() from ExportMT300, pinned in tests).
// Callers must hold the write lock: the lazy-expiry check may
// persist a lapse (see the package comment).
func (l *Ledger) exportMT300Locked(id string, now time.Time) (string, error) {
	// Lazy expiry first, the forward_settle_at_amend.go pattern: a
	// booked contract past SettleAt + the live grace period lapses
	// to cancelled (reason "expired") before anything renders, so a
	// dead agreement is never confirmed.
	l.expireForwardsLocked(now)
	c, ok := l.forwards[id]
	if !ok {
		return "", ErrForwardNotFound
	}
	switch c.Status {
	case ForwardBooked, ForwardSettled:
		// Exportable.
	case ForwardCancelled:
		// A contract the lazy sweep just lapsed (or one a previous
		// sweep lapsed) reports expired, not merely "not booked".
		if c.CancelReason == ForwardCancelExpired {
			return "", fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, id, c.SettleAt.UTC().Format(time.RFC3339))
		}
		return "", fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, id, c.Status)
	default:
		return "", fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, id, c.Status)
	}
	// Snapshot copies under the lock: the render below cannot mutate
	// ledger state even by accident.
	fwd := c.copy()
	fromID, fromBIC := l.mt300PartyIdentifierLocked(fwd.FromAccount)
	toID, toBIC := l.mt300PartyIdentifierLocked(fwd.ToAccount)

	// :20: is the forward ID, 16x.
	if err := mt300TransactionReference(fwd.ID); err != nil {
		return "", err
	}
	// :22C: derives deterministically from the two identifiers and
	// the forward ID's last 4 digits.
	commonRef, err := mt300CommonReference(fromID, toID, fwd.ID)
	if err != nil {
		return "", err
	}
	// Blocks 1/2 LTs derive from the same identifiers: a party
	// without a registered BIC needs an LT-form account ID (8, 11,
	// or 12 alphanumeric characters) — anything else fails loud
	// here rather than emitting a malformed header.
	senderLT, err := mt103NormalizeLT(fromID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT300Export, fromID, err)
	}
	receiverLT, err := mt103NormalizeLT(toID)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT300Export, toID, err)
	}
	// A :82D:/:87D:/:57D: account ID must be SWIFT `x`.
	if !fromBIC {
		if err := mt103CheckX(fromID); err != nil {
			return "", fmt.Errorf("%w: :82D: account ID %q: %w", ErrInvalidMT300Export, fromID, err)
		}
	}
	if !toBIC {
		if err := mt103CheckX(toID); err != nil {
			return "", fmt.Errorf("%w: :87D: account ID %q: %w", ErrInvalidMT300Export, toID, err)
		}
	}
	// :36: is the locked rate, exact to 12 decimals (see
	// mt300Rate); :32B: is the bought amount at that rate.
	rate, err := mt300Rate(fwd.RateNum, fwd.RateDen)
	if err != nil {
		return "", err
	}
	boughtCents := mt300ConvertedCents(fwd.NotionalCents, fwd.RateNum, fwd.RateDen)

	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I300. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I300%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", fwd.ID)
	// Block 4: the field set, in canonical order.
	b.WriteString("{4:\n")
	b.WriteString(":15A:\n")
	fmt.Fprintf(&b, ":20:%s\n", fwd.ID)
	b.WriteString(":22A:NEWT\n")
	fmt.Fprintf(&b, ":22C:%s\n", commonRef)
	if fromBIC {
		fmt.Fprintf(&b, ":82A:%s\n", fromID)
	} else {
		fmt.Fprintf(&b, ":82D:%s\n", fromID)
	}
	if toBIC {
		fmt.Fprintf(&b, ":87A:%s\n", toID)
	} else {
		fmt.Fprintf(&b, ":87D:%s\n", toID)
	}
	b.WriteString(":15B:\n")
	fmt.Fprintf(&b, ":30T:%s\n", mt940ValueDate(fwd.BookedAt))
	fmt.Fprintf(&b, ":30V:%s\n", mt940ValueDate(fwd.SettleAt))
	fmt.Fprintf(&b, ":36:%s\n", rate)
	fmt.Fprintf(&b, ":32B:%s%s\n", fwd.ToCurrency, mt300CentsAmount(boughtCents))
	fmt.Fprintf(&b, ":33B:%s%s\n", fwd.FromCurrency, mt940Amount(fwd.NotionalCents))
	if toBIC {
		fmt.Fprintf(&b, ":57A:%s\n", toID)
	} else {
		fmt.Fprintf(&b, ":57D:%s\n", toID)
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt300PartyIdentifierLocked returns a party's best-known identifier
// (the camt.056 convention): the account's registered BIC from
// accountIBANs when one exists and is valid, else the internal
// account ID, uppercased. hasBIC reports which form it is, so the
// caller renders :82A:/:87A:/:57A: for a BIC and :82D:/:87D:/:57D:
// for an account ID. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) mt300PartyIdentifierLocked(acct AccountID) (identifier string, hasBIC bool) {
	if reg, ok := l.accountIBANs[acct]; ok && reg.BIC != "" && validBIC(reg.BIC) {
		return strings.ToUpper(reg.BIC), true
	}
	return strings.ToUpper(string(acct)), false
}

// mt300TransactionReference validates the :20: reference (the
// forward ID): 1–16 SWIFT `x` characters, no braces, no line breaks
// (a single-line field must not smuggle a second line into block
// 4), no leading/trailing colon or slash, no double slash.
func mt300TransactionReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :20: want 1-%d characters, got %q", ErrInvalidMT300Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :20: line break in a single-line field: %q", ErrInvalidMT300Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :20: %w", ErrInvalidMT300Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :20: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT300Export, ref)
	}
	return nil
}

// mt300CommonReference derives the :22C: common reference:
// first 4 characters of the From identifier (4!a, letters) +
// first 2 of the To identifier (2!c, alphanumeric) + the last 4
// digits of the forward ID (4!n). Identifiers are uppercased before
// slicing; any component that does not fit fails loud — a
// confirmation must never carry a reference the counterparty
// cannot match on.
func mt300CommonReference(fromID, toID, forwardID string) (string, error) {
	fromID = strings.ToUpper(fromID)
	toID = strings.ToUpper(toID)
	if len(fromID) < 4 || len(toID) < 2 || len(forwardID) < 4 {
		return "", fmt.Errorf("%w: :22C: needs a payer identifier of at least 4 characters, a payee identifier of at least 2, and a forward ID ending in 4 digits (got %q, %q, %q)", ErrInvalidMT300Export, fromID, toID, forwardID)
	}
	first4 := fromID[:4]
	for i := 0; i < 4; i++ {
		if first4[i] < 'A' || first4[i] > 'Z' {
			return "", fmt.Errorf("%w: :22C: payer identifier %q does not start with 4 letters", ErrInvalidMT300Export, fromID)
		}
	}
	first2 := toID[:2]
	for i := 0; i < 2; i++ {
		c := first2[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", fmt.Errorf("%w: :22C: payee identifier %q does not start with 2 alphanumeric characters", ErrInvalidMT300Export, toID)
		}
	}
	last4 := forwardID[len(forwardID)-4:]
	for i := 0; i < 4; i++ {
		if last4[i] < '0' || last4[i] > '9' {
			return "", fmt.Errorf("%w: :22C: forward ID %q does not end in 4 digits", ErrInvalidMT300Export, forwardID)
		}
	}
	return first4 + first2 + last4, nil
}

// mt300Rate renders RateNum/RateDen as a decimal with a comma
// separator, half-up rounded at the 12th decimal place. The rounding
// is exact big.Int arithmetic, never float: scaled =
// round(num/den * 10^12) = (2*num*10^12 + den) / (2*den) for
// positive rationals (adding half a unit in the last place, then
// truncating — the +den in the numerator is that half unit, scaled
// by 2*den). Trailing zeros are trimmed but at least one fractional
// digit is kept, so 11/10 renders "1,1" and 1/3 renders
// "0,333333333333".
func mt300Rate(num, den int64) (string, error) {
	if num <= 0 || den <= 0 {
		return "", fmt.Errorf("%w: rate %d/%d must be positive", ErrInvalidMT300Export, num, den)
	}
	const scale = 1000000000000 // 10^12
	n := big.NewInt(num)
	d := big.NewInt(den)
	// scaled = (2*num*10^12 + den) / (2*den), truncated — the
	// half-up rounding at the 12th decimal, computed exactly.
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

// mt300ConvertedCents computes floor(notional * num / den) in exact
// big.Int arithmetic: the bought amount in :32B:. All three inputs
// are positive (booking guarantees it), so big.Int Quo — truncation
// toward zero — is the floor.
func mt300ConvertedCents(notional, num, den int64) *big.Int {
	converted := new(big.Int).Mul(big.NewInt(notional), big.NewInt(num))
	return converted.Quo(converted, big.NewInt(den))
}

// mt300CentsAmount renders a non-negative big.Int cent amount the
// mt940Amount way ("11000" -> "110,00"): the absolute value with a
// comma decimal separator. The sign lives nowhere — a bought amount
// is never negative — and the big.Int form keeps :32B: exact even
// when floor(notional*num/den) would overflow int64.
func mt300CentsAmount(cents *big.Int) string {
	whole, frac := new(big.Int).QuoRem(cents, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("%s,%02d", whole.String(), frac.Int64())
}
