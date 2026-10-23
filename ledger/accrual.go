package ledger

import (
	"errors"
	"strings"
	"time"
)

// Accruals: systematic adjusting entries for interest and fees (opt-in).
//
// An accrual books one journal entry per (kind, expense account, payable
// account, calendar day): debit the expense account, credit the payable
// (liability) account. The entry ID is deterministic —
// "accrual/<kind>/<expense>/<payable>/<YYYY-MM-DD>" (UTC date) — so
// re-running the day's accrual is idempotent: the second call returns the
// original entry with duplicate == true and books nothing new.
//
// An accrual is an ordinary journal entry: it carries its own audit-chain
// link (the hash covers every journaled field), it bumps the ledger
// version, and it flows through VerifyAccountingEquation and Reconcile
// exactly like a hand-posted entry. It runs the full Post gate order —
// field validation, idempotency replay, chart direction, frozen,
// overdraft, daily-limit, period — with one deliberate exception: it
// never fires the journal rules engine (see rules.go). Accruals are
// system-generated adjusting entries; letting operator-configured rules
// cascade derived legs off them would double-count the adjustment. They
// also never enter dual-control review (see review.go): review routing
// lives on PostTransfer's path only, and accruals post through Post's.
// The accrual kind rides in the memo ("accrual:interest",
// "accrual:fee"), so accrual rows are discoverable through the ordinary
// journal listing and the ?memo= filter.
//
// The cumulative accrual for a (kind, expense, payable) pair is queryable
// (AccruedTotal) and can be capped (capCents): a run whose total would
// exceed the cap is rejected with ErrAccrualCapExceeded. An accrual can
// be reversed with ReverseAccrual, which books the mirror entry
// (debit payable, credit expense) under the deterministic ID
// "<accrual ID>/reversal"; one accrual admits exactly one reversal.
// AccruedTotal is net of reversals.
//
// Amounts are int64 cents and the ledger's currency is the default
// currency (see DefaultCurrency): accruals are single-currency adjusting
// entries by design, like every other journal row. Interest accrues daily
// at floor(principal * rateBps / 10000 / 365) cents, the same
// overflow-safe basis-point math the transfer fee policy uses
// (see policyFeeCents).

// Accrual kinds.
const (
	AccrualKindInterest = "interest"
	AccrualKindFee      = "fee"
)

// Validation errors for accruals.
var (
	// ErrInvalidAccrualKind is returned when the accrual kind is not
	// "interest" or "fee".
	ErrInvalidAccrualKind = errors.New("ledger: accrual kind must be \"interest\" or \"fee\"")
	// ErrAccrualAccountSeparator is returned when an accrual account ID
	// contains '/': the deterministic accrual entry ID joins its
	// components with '/', so a '/' inside an account would make two
	// different (expense, payable) pairs share one entry ID.
	ErrAccrualAccountSeparator = errors.New("ledger: accrual account IDs must not contain '/'")
	// ErrInvalidAccrualRate is returned by AccrueInterest when rateBps
	// is outside 1..10000 (a rate above 100% of principal per year is a
	// configuration bug, not a clampable rate).
	ErrInvalidAccrualRate = errors.New("ledger: accrual rate must be between 1 and 10000 basis points")
	// ErrInvalidAccrualCap is returned when capCents is negative: a cap
	// of zero disables capping.
	ErrInvalidAccrualCap = errors.New("ledger: accrual cap must not be negative")
	// ErrAccrualCapExceeded is returned when posting the accrual would
	// take the cumulative net accrual for the (kind, expense, payable)
	// pair above capCents.
	ErrAccrualCapExceeded = errors.New("ledger: accrual would exceed the cumulative accrual cap")
	// ErrAccrualNotFound is returned by ReverseAccrual when the named
	// accrual entry does not exist or is not an accrual.
	ErrAccrualNotFound = errors.New("ledger: accrual entry not found")
	// ErrAccrualAlreadyReversed is returned by ReverseAccrual when the
	// accrual was already reversed.
	ErrAccrualAlreadyReversed = errors.New("ledger: accrual was already reversed")
)

// accrualEntryID builds the deterministic accrual entry ID. The UTC
// calendar date makes one day's accrual idempotent while a new day gets
// a fresh ID.
func accrualEntryID(kind string, expense, payable AccountID, asof time.Time) string {
	return "accrual/" + kind + "/" + string(expense) + "/" + string(payable) +
		"/" + asof.UTC().Format("2006-01-02")
}

// accrualEntryPrefix is the ID prefix shared by every accrual of one
// (kind, expense, payable) triple, used by AccruedTotal to find them.
func accrualEntryPrefix(kind string, expense, payable AccountID) string {
	return "accrual/" + kind + "/" + string(expense) + "/" + string(payable) + "/"
}

// validAccrualKind reports whether kind is a known accrual kind.
func validAccrualKind(kind string) bool {
	return kind == AccrualKindInterest || kind == AccrualKindFee ||
		kind == AccrualKindInterestTiered
}

// checkAccrualAccounts rejects empty or indistinguishable accrual legs
// and account IDs that would break the deterministic ID scheme. It
// mirrors the field-validation half of validateJournalEntry so the
// caller gets the error before the cap is evaluated.
func checkAccrualAccounts(expense, payable AccountID) error {
	if expense == "" || payable == "" {
		return errors.New("ledger: accrual expense and payable accounts must not be empty")
	}
	if expense == payable {
		return ErrSameAccount
	}
	if strings.Contains(string(expense), "/") || strings.Contains(string(payable), "/") {
		return ErrAccrualAccountSeparator
	}
	return nil
}

// AccrueInterest books one day's interest accrual: debit the expense
// account and credit the payable account for
// floor(principalCents * rateBps / 10000 / 365) cents. rateBps is the
// annual rate in basis points (500 = 5%/yr). asof sets both the entry's
// timestamp and its calendar day (UTC); a zero asof means now.
//
// capCents caps the cumulative net accrual for the (interest, expense,
// payable) triple: when the already-accrued total plus this run would
// exceed it, nothing is booked and ErrAccrualCapExceeded is returned.
// The total is net of reversals and capitalizations (see
// CapitalizeInterest): capitalizing accrued interest into principal
// frees cap room for new accruals. Zero disables capping; negative is
// rejected with ErrInvalidAccrualCap.
//
// The call is idempotent per calendar day: re-running the same day
// returns the original entry with duplicate == true. The entry runs the
// full Post gate order (frozen, overdraft, daily-limit, closed period)
// but never fires journal rules and never enters dual-control review;
// see the package doc comment above for why. The dormant-account stop
// (see dormancy.go) is the one deliberate exception: accruals post
// through the system path, so interest keeps accruing on dormant
// accounts.
func (l *Ledger) AccrueInterest(expense, payable AccountID, principalCents, rateBps int64, asof time.Time, capCents int64) (JournalEntry, bool, error) {
	if rateBps < 1 || rateBps > 10000 {
		return JournalEntry{}, false, ErrInvalidAccrualRate
	}
	if principalCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	// Overflow-safe floor(principal * rateBps / 10000 / 365): the same
	// basis-point helper the transfer fee policy uses, divided by the
	// days in a year. The division is exact in ordering — flooring the
	// annual figure then dividing by 365 equals flooring the daily
	// figure, because the second divisor is an integer.
	amount := policyFeeCents(principalCents, rateBps) / 365
	if amount <= 0 {
		// A dust accrual books nothing: a tiny principal under a low
		// rate rounds to zero cents for the day, mirroring the fee
		// policy's dust tiers (see transfer.go).
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	return l.accrue(AccrualKindInterest, expense, payable, amount, capCents, asof, time.Time{}, ValueDateConventionNone)
}

// AccrueInterestWithValueDate books one day's interest accrual like
// AccrueInterest, but the accrual entry carries an explicit value date
// (起息日, LG-181): the day-count and AccruedTotal windows anchor on
// the value date, not the booking date, while the deterministic entry ID
// still anchors on the booking day (asof) so per-day idempotency is
// unchanged. The value date is validated against the ledger's
// value-date window and adjusted per the business-day convention before
// anything is journaled — fail-closed, like Post.
func (l *Ledger) AccrueInterestWithValueDate(expense, payable AccountID, principalCents, rateBps int64, asof time.Time, valueDate time.Time, convention ValueDateConvention, capCents int64) (JournalEntry, bool, error) {
	if rateBps < 1 || rateBps > 10000 {
		return JournalEntry{}, false, ErrInvalidAccrualRate
	}
	if principalCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	// Same overflow-safe daily math as AccrueInterest: flooring the
	// annual figure then dividing by 365 equals flooring the daily
	// figure, because the second divisor is an integer.
	amount := policyFeeCents(principalCents, rateBps) / 365
	if amount <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	return l.accrue(AccrualKindInterest, expense, payable, amount, capCents, asof, valueDate, convention)
}

// AccrueFee books a fee accrual of exactly amountCents: debit the
// expense account and credit the payable account. asof sets both the
// entry's timestamp and its calendar day (UTC); a zero asof means now.
//
// capCents caps the cumulative net accrual for the (fee, expense,
// payable) triple like AccrueInterest's cap: zero disables capping.
//
// The call is idempotent per calendar day, runs the full Post gate
// order, and never fires journal rules nor enters dual-control review.
// The dormant-account stop is the one deliberate exception (see
// dormancy.go): fee accruals post through the system path.
func (l *Ledger) AccrueFee(expense, payable AccountID, amountCents int64, asof time.Time, capCents int64) (JournalEntry, bool, error) {
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	return l.accrue(AccrualKindFee, expense, payable, amountCents, capCents, asof, time.Time{}, ValueDateConventionNone)
}

// AccrueFeeWithValueDate books a fee accrual like AccrueFee, but the
// accrual entry carries an explicit value date (起息日, LG-181): the
// AccruedTotal windows anchor on the value date, not the booking date,
// while the deterministic entry ID still anchors on the booking day
// (asof) so per-day idempotency is unchanged. The value date is
// validated against the ledger's value-date window and adjusted per the
// business-day convention before anything is journaled — fail-closed,
// like Post.
func (l *Ledger) AccrueFeeWithValueDate(expense, payable AccountID, amountCents int64, asof time.Time, valueDate time.Time, convention ValueDateConvention, capCents int64) (JournalEntry, bool, error) {
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	return l.accrue(AccrualKindFee, expense, payable, amountCents, capCents, asof, valueDate, convention)
}

// accrue is the shared accrual path. Callers hold no lock; it takes the
// write lock for the cap evaluation and the post. The cap and the post
// are atomic with respect to each other: two concurrent accruals for the
// same triple cannot both clear the cap check and then both book past
// it, because both run under the same write lock. A zero valueDate
// (with ValueDateConventionNone) anchors the accrual on its booking
// date, like the historical behavior.
func (l *Ledger) accrue(kind string, expense, payable AccountID, amountCents, capCents int64, asof time.Time, valueDate time.Time, convention ValueDateConvention) (JournalEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accrueLocked(kind, expense, payable, amountCents, capCents, asof, valueDate, convention)
}

// accrueLocked is accrue's body. Callers must hold l.mu (the write
// lock): it evaluates the cap and posts atomically. AccrueInterestTiered
// (see interest_tiers.go) enters here holding the lock because it needs
// the tier table read and the post under one critical section.
func (l *Ledger) accrueLocked(kind string, expense, payable AccountID, amountCents, capCents int64, asof time.Time, valueDate time.Time, convention ValueDateConvention) (JournalEntry, bool, error) {
	if !validAccrualKind(kind) {
		return JournalEntry{}, false, ErrInvalidAccrualKind
	}
	if err := checkAccrualAccounts(expense, payable); err != nil {
		return JournalEntry{}, false, err
	}
	if capCents < 0 {
		return JournalEntry{}, false, ErrInvalidAccrualCap
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	id := accrualEntryID(kind, expense, payable, asof)
	e := JournalEntry{
		ID:                  id,
		DebitAccount:        expense,
		CreditAccount:       payable,
		AmountCents:         amountCents,
		IdempotencyKey:      id,
		Memo:                "accrual:" + kind,
		ValueDate:           valueDate,
		ValueDateConvention: convention,
		CreatedAt:           asof,
	}

	// accrueLocked holds l.mu on entry (see accrue above).
	// LG-95: with business-day-only accruals, a non-business-day asof
	// books nothing — interest accrues per business day, not per
	// calendar day.
	if err := l.accrualBusinessDayGateLocked(asof); err != nil {
		return JournalEntry{}, false, err
	}
	if capCents > 0 {
		total, err := l.accruedTotalLocked(expense, payable, kind, time.Time{}, asof)
		if err != nil {
			return JournalEntry{}, false, err
		}
		// Overflow-safe: a sum that does not fit int64 necessarily
		// exceeds any int64 cap, so treat the overflow as a cap breach
		// rather than wrapping around to a small number.
		sum, ok := addCents(total, amountCents)
		if !ok || sum > capCents {
			return JournalEntry{}, false, ErrAccrualCapExceeded
		}
	}
	// fireRules=false: accruals are system adjusting entries and must
	// not cascade through operator-configured journal rules (see the
	// package doc comment). They still run the full gate order —
	// validation, idempotency replay, chart direction, frozen,
	// overdraft, daily-limit, period — commit a full journal row, and
	// bump the version. Dual-control review is never consulted: review
	// routing lives on PostTransfer's path only.
	return l.postSystemLocked(e, false)
}

// AccruedTotal returns the cumulative net accrual for one (kind,
// expense, payable) triple over [since, until): the sum of every
// accrual entry in the window, minus the reversals of those accruals,
// minus the amounts capitalized into principal (see
// CapitalizeInterest in capitalize.go — after a capitalization the
// outstanding total for the triple is zero). A zero since includes all
// history. Amounts are int64 cents in the default currency; the sum is
// overflow-checked — a total that does not fit int64 is a data error,
// not a wrappable number.
func (l *Ledger) AccruedTotal(expense, payable AccountID, kind string, since, until time.Time) (int64, error) {
	if !validAccrualKind(kind) {
		return 0, ErrInvalidAccrualKind
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.accruedTotalLocked(expense, payable, kind, since, until)
}

// accruedTotalLocked is AccruedTotal's body. Callers must hold l.mu; the
// read lock suffices because nothing is mutated.
func (l *Ledger) accruedTotalLocked(expense, payable AccountID, kind string, since, until time.Time) (int64, error) {
	prefix := accrualEntryPrefix(kind, expense, payable)
	var total int64
	for id, e := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		// The remainder after the prefix is the UTC date ("2006-01-02",
		// 10 chars); anything else is not an accrual row of this
		// triple — defensive, since the ID scheme is the only writer.
		if len(id) != len(prefix)+10 {
			continue
		}
		// LG-181: accrual rows anchor their period on the value date,
		// not the booking date — interest accrues from the value date
		// (资金起息). Rows without an explicit value date anchor on
		// their booking date, so pre-value-date history totals exactly
		// as before.
		if vd := e.EffectiveValueDate(); vd.Before(since) || !vd.Before(until) {
			continue
		}
		var ok bool
		total, ok = addCents(total, e.AmountCents)
		if !ok {
			return 0, errors.New("ledger: accrued total overflows int64")
		}
		// A reversal of this accrual nets it out of the total. The
		// reversal carries the mirror legs, so subtracting here keeps
		// AccruedTotal equal to the books' net effect.
		if rev, ok := l.entries[id+"/reversal"]; ok {
			total, ok = addCents(total, -rev.AmountCents)
			if !ok {
				return 0, errors.New("ledger: accrued total overflows int64")
			}
		}
	}
	// A capitalization of this triple (see CapitalizeInterest in
	// capitalize.go) moves the accrued amount into principal: it nets
	// out of the outstanding total exactly like a reversal. Only the
	// interest kind can be capitalized, so this loop is a no-op for
	// fees. A reversed capitalization restores the payable balance —
	// the mirror entry cancels the capitalization, so a capitalization
	// with a reversal row is skipped here. Like the accrual rows above,
	// the window anchors on the value date (LG-181); capitalization
	// rows carry none, so this is identical to the booking date.
	if kind == AccrualKindInterest {
		for id, e := range l.entries {
			exp, pay, _, ok := parseCapitalizationID(id)
			if !ok || exp != expense || pay != payable {
				continue
			}
			if vd := e.EffectiveValueDate(); vd.Before(since) || !vd.Before(until) {
				continue
			}
			if _, reversed := l.entries[id+"/reversal"]; reversed {
				continue
			}
			total, ok = addCents(total, -e.AmountCents)
			if !ok {
				return 0, errors.New("ledger: accrued total overflows int64")
			}
		}
	}
	return total, nil
}

// ReverseAccrual reverses one accrual: it books the mirror entry —
// debit the payable account, credit the expense account — for the
// original amount, under the deterministic ID "<accrual ID>/reversal".
// One accrual admits exactly one reversal: reversing twice returns the
// existing reversal with duplicate == true. asof sets the reversal's
// timestamp; a zero asof means now.
//
// It also reverses an interest capitalization (see CapitalizeInterest
// in capitalize.go — capitalization IDs share the "accrual" prefix
// family): the mirror entry debits the principal and credits the
// payable, restoring the outstanding accrued total.
//
// The reversal runs the full Post gate order and, like every accrual,
// never fires journal rules and never enters dual-control review. Note
// the chart-of-accounts interaction: the reversal debits the payable
// (liability) account, so under a registered chart (see chart.go) it is
// rejected with ErrAccountDirectionMismatch — reversing an accrual
// against a strictly-typed chart must route through a dedicated contra
// account instead.
func (l *Ledger) ReverseAccrual(accrualID string, asof time.Time) (JournalEntry, bool, error) {
	if !strings.HasPrefix(accrualID, "accrual/") && !strings.HasPrefix(accrualID, "accrual-capitalization/") {
		return JournalEntry{}, false, ErrAccrualNotFound
	}
	if strings.HasSuffix(accrualID, "/reversal") {
		// Reversing a reversal is not an operation: one accrual admits
		// exactly one reversal, and a reversal is not an accrual.
		return JournalEntry{}, false, ErrAccrualNotFound
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	revID := accrualID + "/reversal"

	l.mu.Lock()
	defer l.mu.Unlock()
	orig, ok := l.entries[accrualID]
	if !ok {
		return JournalEntry{}, false, ErrAccrualNotFound
	}
	if rev, ok := l.entries[revID]; ok {
		return rev, true, nil
	}
	e := JournalEntry{
		ID:             revID,
		DebitAccount:   orig.CreditAccount,
		CreditAccount:  orig.DebitAccount,
		AmountCents:    orig.AmountCents,
		Currency:       orig.Currency,
		IdempotencyKey: revID,
		Memo:           "accrual:reversal",
		CreatedAt:      asof,
	}
	return l.postSystemLocked(e, false)
}
