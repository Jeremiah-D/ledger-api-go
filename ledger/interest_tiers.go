package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Tiered deposit rates (LG-114).
//
// AccrueInterest (see accrual.go) prices a whole principal at one flat
// annual rate. Real deposit products price balance *bands* differently —
// the first million cents at 5.00%/yr, the next at 3.00%/yr, and so on.
// SetInterestTiers installs that rate ladder: AccrueInterestTiered then
// books one day's interest as the sum over bands of
// floor(bandPrincipal * rateBps / 10000 / 365) cents, the same
// overflow-safe basis-point math the transfer fee policy uses
// (see policyFeeCents in transfer.go), divided by the days in a year.
//
// A tier table is a non-empty slice of InterestTier sorted by MinCents:
// band i covers [MinCents_i, MinCents_{i+1}) and the last band is
// open-ended. The first band must start at zero — a principal below the
// first minimum would otherwise earn nothing on its lowest cents, which
// is a configuration bug, not a rate design.
//
// The table is structural config: SetInterestTiers does not bump the
// journal version (no balances change) and every change is recorded in
// the structured audit log (see audit.go) as "interest_tiers_set" or
// "interest_tiers_cleared". Setting an empty table clears the ladder and
// returns tiered accrual to "not configured". The table round-trips
// through the disaster-recovery snapshot config record, like the
// transfer fee schedule.
//
// Tiered accruals are their own accrual kind ("interest-tier"): the entry
// ID is "accrual/interest-tier/<expense>/<payable>/<YYYY-MM-DD>" and the
// memo is "accrual:interest-tier", so tiered rows are queryable with
// AccruedTotal(..., "interest-tier") and reversible with ReverseAccrual.
// They share the single-rate cap semantics (capCents caps the cumulative
// net tiered accrual for the triple) and the same gate order — full Post
// gates, no journal rules, no dual-control review, business-day gate —
// because they ride the shared accrue path (see accrual.go). They are
// not capitalizable: CapitalizeInterest only nets the plain "interest"
// kind (see accrue.go's accruedTotalLocked), so tiered rows stay
// outstanding until reversed.
//
// Negative principals are not eligible for the ladder: a negative
// balance is overdraft territory, priced by AccrueOverdraftInterest (see
// overdraft_interest.go), which is orthogonal to deposit bands. Passing
// a non-positive principal to AccrueInterestTiered is rejected with
// ErrNonPositiveAmount.

// AccrualKindInterestTiered is the accrual kind tiered deposit rates
// book under: the memo rides as "accrual:interest-tier" and the entry ID
// as "accrual/interest-tier/<expense>/<payable>/<date>".
const AccrualKindInterestTiered = "interest-tier"

// InterestTier is one band of the tiered deposit-rate ladder: balances
// from MinCents (inclusive) up to the next tier's MinCents (exclusive)
// earn RateBps per year. MinCents must be strictly ascending across the
// table and the first band must start at zero; RateBps is the annual
// rate in basis points (500 = 5%/yr), same units as AccrueInterest.
type InterestTier struct {
	MinCents int64 `json:"min_cents"`
	RateBps  int64 `json:"rate_bps"`
}

// Validation errors for the tier table.
var (
	// ErrInvalidInterestTier is returned by SetInterestTiers when the
	// table is malformed: first band not at zero, minima not strictly
	// ascending, a negative minimum, or a rate outside 1..10000 bps.
	ErrInvalidInterestTier = errors.New("ledger: interest tier table must start at 0 with strictly ascending minima and rates between 1 and 10000 basis points")
	// ErrInterestTiersNotConfigured is returned by AccrueInterestTiered
	// when no tier table is installed: call SetInterestTiers first, or
	// use AccrueInterest for a flat rate.
	ErrInterestTiersNotConfigured = errors.New("ledger: no interest tier table configured")
)

// validateInterestTiers checks the ladder shape without touching the
// ledger: first band at zero, strictly ascending minima, non-negative
// minima, rates in 1..10000. An empty table is valid — it means "clear".
func validateInterestTiers(tiers []InterestTier) error {
	for i, t := range tiers {
		if t.MinCents < 0 {
			return fmt.Errorf("%w: band %d has negative minimum %d", ErrInvalidInterestTier, i, t.MinCents)
		}
		if t.RateBps < 1 || t.RateBps > 10000 {
			return fmt.Errorf("%w: band %d rate %d bps out of range", ErrInvalidInterestTier, i, t.RateBps)
		}
		if i == 0 {
			if t.MinCents != 0 {
				return fmt.Errorf("%w: first band must start at 0, got %d", ErrInvalidInterestTier, t.MinCents)
			}
			continue
		}
		if t.MinCents <= tiers[i-1].MinCents {
			return fmt.Errorf("%w: band %d minimum %d not above band %d minimum %d",
				ErrInvalidInterestTier, i, t.MinCents, i-1, tiers[i-1].MinCents)
		}
	}
	return nil
}

// SetInterestTiers installs (or replaces) the tiered deposit-rate
// ladder used by AccrueInterestTiered. Passing an empty (or nil) table
// clears the ladder and returns tiered accrual to "not configured".
//
// Setting the table is structural: it does not bump the journal version
// and it is visible to audit tooling — the change is recorded as
// "interest_tiers_set" with the new bands, and clearing as
// "interest_tiers_cleared". Re-installing the identical table is a
// silent no-op, so idempotent retries stay silent in the compliance
// trail.
func (l *Ledger) SetInterestTiers(tiers []InterestTier) error {
	if err := validateInterestTiers(tiers); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if tiersEqual(l.interestTiers, tiers) {
		// Identical table: nothing changed, no audit event — the
		// trail records changes, not retries.
		return nil
	}
	cp := append([]InterestTier(nil), tiers...)
	l.interestTiers = cp

	op := "interest_tiers_set"
	details := map[string]any{}
	if len(cp) > 0 {
		bands := make([]map[string]any, 0, len(cp))
		for _, t := range cp {
			bands = append(bands, map[string]any{
				"min_cents": t.MinCents,
				"rate_bps":  t.RateBps,
			})
		}
		details["tiers"] = bands
	} else {
		op = "interest_tiers_cleared"
	}
	l.emitAudit(AuditEvent{
		Op:            op,
		Actor:         "SetInterestTiers",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return nil
}

// tiersEqual reports whether two tier tables are identical band by band.
func tiersEqual(a, b []InterestTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// InterestTiers returns the installed tier table, or nil when tiered
// accrual is off. The returned slice is a copy.
func (l *Ledger) InterestTiers() []InterestTier {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]InterestTier(nil), l.interestTiers...)
}

// tieredInterestCents computes one day's interest on principalCents
// under the given ladder: the sum over bands of
// floor(bandPrincipal * rateBps / 10000 / 365). Every band's arithmetic
// is overflow-safe — policyFeeCents never overflows int64 (see
// transfer.go) and the band totals accumulate through addCents, so a
// ladder whose bands sum past int64 reports an overflow error instead of
// wrapping. Callers must hold l.mu or pass a table they own.
func tieredInterestCents(principalCents int64, tiers []InterestTier) (int64, error) {
	var total int64
	for i, t := range tiers {
		if principalCents <= t.MinCents {
			break
		}
		ceiling := principalCents
		if i+1 < len(tiers) {
			ceiling = tiers[i+1].MinCents
		}
		band := ceiling - t.MinCents
		if band > principalCents-t.MinCents {
			band = principalCents - t.MinCents
		}
		// Overflow-safe floor(band * rateBps / 10000 / 365): the same
		// basis-point helper the transfer fee policy uses, divided by
		// the days in a year — flooring the annual figure then
		// dividing by 365 equals flooring the daily figure, because
		// the second divisor is an integer.
		amount := policyFeeCents(band, t.RateBps) / 365
		var ok bool
		total, ok = addCents(total, amount)
		if !ok {
			return 0, errors.New("ledger: tiered interest total overflows int64")
		}
	}
	return total, nil
}

// AccrueInterestTiered books one day's interest under the installed
// tier ladder: debit the expense account and credit the payable account
// for the per-band sum tieredInterestCents computes. asof sets both the
// entry's timestamp and its calendar day (UTC); a zero asof means now.
//
// capCents caps the cumulative net tiered accrual for the triple, like
// AccrueInterest's cap but tracked under the "interest-tier" kind: zero
// disables capping; negative is rejected with ErrInvalidAccrualCap.
//
// The call requires a tier table: with none installed it fails with
// ErrInterestTiersNotConfigured. A non-positive principal is rejected
// with ErrNonPositiveAmount — negative balances price through
// overdraft interest, not deposit bands (see the package doc). A dust
// accrual (zero cents after flooring every band) books nothing and
// returns ErrNonPositiveAmount, like AccrueInterest.
//
// The call is idempotent per calendar day and runs the full Post gate
// order; it never fires journal rules and never enters dual-control
// review — it rides the shared accrue path under the "interest-tier"
// kind (see accrual.go).
func (l *Ledger) AccrueInterestTiered(expense, payable AccountID, principalCents int64, asof time.Time, capCents int64) (JournalEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.interestTiers) == 0 {
		return JournalEntry{}, false, ErrInterestTiersNotConfigured
	}
	if principalCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	amount, err := tieredInterestCents(principalCents, l.interestTiers)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if amount <= 0 {
		// A dust accrual books nothing: a tiny principal under low
		// band rates rounds to zero cents for the day, like
		// AccrueInterest's dust path.
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	return l.accrueLocked(AccrualKindInterestTiered, expense, payable, amount, capCents, asof, time.Time{}, ValueDateConventionNone)
}
