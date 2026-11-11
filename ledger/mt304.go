package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT304 third-party FX deal instruction export (LG-390,
// fintech: SWIFT 外汇第三方交易指示报文).
//
// The MT300 (LG-388) is a bilateral confirmation: the sender is one
// of the two counterparties. A fund manager who dealt for a fund with
// an executing broker is neither counterparty to the custodian —
// the MT304 is the advice/instruction the manager sends the
// custodian bank so the custodian settles the deal on the fund's
// behalf. Same contract, two message faces: MT300 confirms the
// trade between its parties, MT304 instructs a third party to
// settle it.
//
// Sequence A identifies the instruction (:15A: :20: :22A: :94A:
// :83a: :82a: :87a:); sequence B states the economics (:15B: :30T:
// :30V: :36: :32B: :53a: :33B: :57a:). The field set follows the
// Oracle 14.7 MT304 table: :22A: is always NEWT (this endpoint
// instructs new deals only; AMND/CANC variants are a later round),
// :94A: is the caller-given scope — ADVI (advice) or INST
// (instruction), the two modes of the message's purpose line
// ("Advises of or instructs settlement of a third party foreign
// exchange deal") — and there is no :22C: (the current MT304 table
// carries no such field; the MS12-era :22C: is not invented here).
// There is no :21: — there is no prior instruction to reference —
// and sequences C/D/E are omitted whole: the ledger holds no UTI,
// execution-venue, or accounting data.
//
// Party roles, and the ledger's honest limitation: the fund
// (:83a:) and the fund manager (:82a:) are both the From account —
// the ledger has no separate fund/manager identity fields, so the
// fund account is the manager's operating account and both corners
// take the same identifier; no manager BIC is invented. The
// executing broker (:87a:) is the To account, the dealing
// counterparty. The delivery bank (:53a:, B1) and the receiving
// bank (:57a:, B2) are likewise the To account: it delivers the
// bought currency and receives the sold currency. The custodian
// itself is caller-given (CustodianBIC): the ledger holds no
// custodian registry, so the receiving custodian bank is a
// per-call option, never stored.
//
// Party identifiers follow the MT300 convention (see
// mt300PartyIdentifierLocked): the account's registered BIC when
// one exists, else the internal account ID, uppercased. A BIC
// party renders :83A:/:82A:/:87A:/:53A:/:57A:; an account-ID party
// renders the :83D:/:82D:/:87D:/:53D:/:57D: form with the account
// ID, which must pass mt103CheckX. Blocks 1/2 take the same
// identifiers through mt103NormalizeLT — the sender LT is the From
// party, the receiver LT is the custodian BIC.
//
// :36: is the locked rate (see mt300Rate, 12 decimals half-up),
// :32B: the bought amount floor(NotionalCents*RateNum/RateDen)
// (see mt300ConvertedCents), :33B: the sold notional. The ledger
// settles gross, so :17N: is absent — its absence means
// non-netted — and :17O:/:17F:, :81a:/:89a:/:77H:/:14C:/:32E:
// :30U:/:14S:/:21A:/:14E: are omitted: no CCP, clearing broker,
// agreement version, settlement currency, or valuation-date data
// exists here.
//
// The export is read-only: the locked core takes the write lock
// only because the lazy-expiry check may persist a lapse, exactly
// like ExportMT300. The export itself bumps no version, books
// nothing, and appends no audit event. A booked contract past its
// settlement grace period lapses first, so it reports expired
// rather than instructing settlement of a dead agreement. Repeat
// exports are byte-identical: every field is pinned to the stored
// contract and the caller-given options, no wall clock enters the
// output.

// ErrInvalidMT304Export is returned when a forward cannot be
// rendered as an MT304: a :20: reference outside 16x, a scope
// outside ADVI/INST, a custodian BIC that is not a valid 8/11
// BIC, a party identifier that is not a valid LT form, or a
// :83D:/:82D:/:87D:/:53D:/:57D: account ID outside the SWIFT `x`
// character set. It is a 400-class error: the stored contract
// cannot be expressed as a valid instruction.
var ErrInvalidMT304Export = errors.New("ledger: invalid MT304 export")

// MT304ExportOptions tunes ExportMT304.
//
// ForwardID names the booked or settled forward contract to
// instruct. CustodianBIC is the receiving custodian bank's BIC
// (required, 8 or 11 characters): the ledger holds no custodian
// registry, so the caller names the custodian on every export.
// Scope is the message purpose — ADVI (advice) or INST
// (instruction); empty defaults to ADVI.
type MT304ExportOptions struct {
	ForwardID    string
	CustodianBIC string
	Scope        string
}

// ExportMT304 renders the forward contract as a SWIFT FIN MT304
// third-party FX deal instruction to the caller-given custodian.
// Unknown IDs report ErrForwardNotFound; a manually cancelled
// contract reports ErrForwardNotBooked; a lapsed contract reports
// ErrForwardExpired. Booked and settled contracts export.
func (l *Ledger) ExportMT304(opts MT304ExportOptions) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exportMT304Locked(opts, time.Now())
}

// exportMT304Locked is the lock-held core of ExportMT304; now is
// the export time (time.Now() from ExportMT304, pinned in tests).
// Callers must hold the write lock: the lazy-expiry check may
// persist a lapse (see the package comment).
func (l *Ledger) exportMT304Locked(opts MT304ExportOptions, now time.Time) (string, error) {
	// Caller-given options fail fast, before any contract lookup:
	// a scope outside ADVI/INST or a custodian that is not a valid
	// BIC can never be a valid instruction.
	scope := strings.ToUpper(strings.TrimSpace(opts.Scope))
	if scope == "" {
		scope = "ADVI"
	}
	if scope != "ADVI" && scope != "INST" {
		return "", fmt.Errorf("%w: scope %q: want ADVI or INST", ErrInvalidMT304Export, opts.Scope)
	}
	custodian := strings.ToUpper(strings.TrimSpace(opts.CustodianBIC))
	if custodian == "" || !validBIC(custodian) {
		return "", fmt.Errorf("%w: custodian BIC %q: want a valid 8/11-character BIC", ErrInvalidMT304Export, opts.CustodianBIC)
	}
	// Lazy expiry first, the forward_settle_at_amend.go pattern: a
	// booked contract past SettleAt + the live grace period lapses
	// to cancelled (reason "expired") before anything renders, so a
	// dead agreement is never instructed for settlement.
	l.expireForwardsLocked(now)
	c, ok := l.forwards[opts.ForwardID]
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
			return "", fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, opts.ForwardID, c.SettleAt.UTC().Format(time.RFC3339))
		}
		return "", fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, opts.ForwardID, c.Status)
	default:
		return "", fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, opts.ForwardID, c.Status)
	}
	// Snapshot copies under the lock: the render below cannot mutate
	// ledger state even by accident.
	fwd := c.copy()
	// The fund and the fund manager are both the From account (see
	// the package comment); the broker, the delivery bank, and the
	// receiving bank are all the To account.
	fromID, fromBIC := l.mt300PartyIdentifierLocked(fwd.FromAccount)
	toID, toBIC := l.mt300PartyIdentifierLocked(fwd.ToAccount)

	// :20: is the forward ID, 16x.
	if err := mt304TransactionReference(fwd.ID); err != nil {
		return "", err
	}
	// Blocks 1/2 LTs derive from the From identifier and the
	// custodian BIC: a party without a registered BIC needs an
	// LT-form account ID (8, 11, or 12 alphanumeric characters) —
	// anything else fails loud here rather than emitting a
	// malformed header.
	senderLT, err := mt103NormalizeLT(fromID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT304Export, fromID, err)
	}
	custodianLT, err := mt103NormalizeLT(custodian)
	if err != nil {
		return "", fmt.Errorf("%w: custodian LT from %q: %w", ErrInvalidMT304Export, custodian, err)
	}
	// A :83D:/:82D:/:87D:/:53D:/:57D: account ID must be SWIFT `x`.
	if !fromBIC {
		if err := mt103CheckX(fromID); err != nil {
			return "", fmt.Errorf("%w: :83D:/:82D: account ID %q: %w", ErrInvalidMT304Export, fromID, err)
		}
	}
	if !toBIC {
		if err := mt103CheckX(toID); err != nil {
			return "", fmt.Errorf("%w: :87D:/:53D:/:57D: account ID %q: %w", ErrInvalidMT304Export, toID, err)
		}
	}
	// :36: is the locked rate, exact to 12 decimals (see
	// mt300Rate); :32B: is the bought amount at that rate. The
	// booking guarantees a positive rate; a non-positive ratio
	// reaching here is a stored-contract defect, reported under
	// this export's sentinel rather than MT300's.
	if fwd.RateNum <= 0 || fwd.RateDen <= 0 {
		return "", fmt.Errorf("%w: rate %d/%d must be positive", ErrInvalidMT304Export, fwd.RateNum, fwd.RateDen)
	}
	rate, err := mt300Rate(fwd.RateNum, fwd.RateDen)
	if err != nil {
		return "", err
	}
	boughtCents := mt300ConvertedCents(fwd.NotionalCents, fwd.RateNum, fwd.RateDen)

	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I304. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I304%sU3003}\n", custodianLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", fwd.ID)
	// Block 4: the field set, in canonical order. Sequence A: the
	// fund and the fund manager share the From identifier; the
	// broker is the To identifier. Sequence B: B1 buys at the
	// locked rate (delivery bank = To), B2 sells the notional
	// (receiving bank = To).
	b.WriteString("{4:\n")
	b.WriteString(":15A:\n")
	fmt.Fprintf(&b, ":20:%s\n", fwd.ID)
	b.WriteString(":22A:NEWT\n")
	fmt.Fprintf(&b, ":94A:%s\n", scope)
	if fromBIC {
		fmt.Fprintf(&b, ":83A:%s\n", fromID)
	} else {
		fmt.Fprintf(&b, ":83D:%s\n", fromID)
	}
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
	if toBIC {
		fmt.Fprintf(&b, ":53A:%s\n", toID)
	} else {
		fmt.Fprintf(&b, ":53D:%s\n", toID)
	}
	fmt.Fprintf(&b, ":33B:%s%s\n", fwd.FromCurrency, mt940Amount(fwd.NotionalCents))
	if toBIC {
		fmt.Fprintf(&b, ":57A:%s\n", toID)
	} else {
		fmt.Fprintf(&b, ":57D:%s\n", toID)
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt304TransactionReference validates the :20: reference (the
// forward ID): 1–16 SWIFT `x` characters, no braces, no line breaks
// (a single-line field must not smuggle a second line into block
// 4), no leading/trailing colon or slash, no double slash. The
// MT300 shape (see mt300TransactionReference) under this export's
// sentinel.
func mt304TransactionReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :20: want 1-%d characters, got %q", ErrInvalidMT304Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :20: line break in a single-line field: %q", ErrInvalidMT304Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :20: %w", ErrInvalidMT304Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :20: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT304Export, ref)
	}
	return nil
}
