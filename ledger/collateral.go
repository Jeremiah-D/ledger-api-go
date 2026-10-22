package ledger

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// Collateral: pledged assets covering drawn utilization (LG-173,
// fintech).
//
// A collateral position is an account's pledge of an off-ledger asset
// (securities, inventory, receivables — anything the ledger does not
// itself book) against the account's drawn utilization. It is
// orthogonal to the credit line (see creditline.go): the line sets how
// far negative the balance may go (the draw ceiling); collateral sets
// how much of the draw must be covered by pledged value (the coverage
// floor). An account may carry either, both, or neither.
//
// Each pledged asset carries a haircut: the per-asset discount the
// bank applies to the market value before it counts as coverage
// (volatile assets take bigger haircuts). The effective collateral
// value of one position is
//
//	floor(quantity * unitValueCents * (10000 - haircutBps) / 10000)
//
// computed overflow-safe (see policyFeeCents): a pledge that would
// overflow int64 saturates at math.MaxInt64 rather than wrapping.
//
// Coverage is evaluated per (account, currency) against the account's
// drawn utilization in that currency — drawn = max(0, -balance), the
// same utilization the credit line uses. The account opts in with a
// minimum coverage ratio (e.g. 12500 = 125%): a new draw that would
// leave drawn * minCoverageBps / 10000 uncovered by effective
// collateral is rejected with ErrInsufficientCollateral, in Post's
// gate and in PostTransfer's gate, after the overdraft check — both
// are account risk controls guarding the same outflow.
//
// Collateral values are denominated in the requirement currency being
// evaluated: the operator maintains per-currency valuations through
// RevalueCollateral. For the common single-currency account this is
// exact; a multi-currency account revalues each requirement currency
// separately. The module never invents an FX conversion — a silent
// conversion would misstate coverage.
//
// A market revaluation (or a release, or a raised requirement) that
// drops coverage below the required ratio triggers a margin call: the
// call is returned to the caller (the AgingAlert / FXRateAlert
// convention — alerts are returned structs, not side channels) and
// recorded as a collateral_margin_call audit event. Pledging,
// releasing, revaluing and (re)setting the requirement are structural
// config: they never bump the journal version, and the registry
// round-trips through disaster-recovery snapshots like the
// credit-line registry.

// collateralKey identifies one pledged position: an account pledges
// each asset at most once; top-ups accumulate into the same position.
type collateralKey struct {
	account AccountID
	asset   string
}

// CollateralPledge is one pledged asset position: quantity units of
// assetID pledged by account, last valued at unitValueCents per unit
// with a haircutBps haircut.
type CollateralPledge struct {
	Account        AccountID `json:"account"`
	AssetID        string    `json:"asset_id"`
	Quantity       int64     `json:"quantity"`
	UnitValueCents int64     `json:"unit_value_cents"`
	HaircutBps     int64     `json:"haircut_bps"`
	PledgedAt      time.Time `json:"pledged_at"`
}

// collateralReqKey identifies one coverage requirement: an account
// carries at most one minimum coverage ratio per currency.
type collateralReqKey struct {
	account  AccountID
	currency string
}

// CollateralRequirement is an account's opt-in minimum collateral
// coverage ratio in one currency: drawn utilization must stay covered
// at MinCoverageBps basis points (10000 = 100%).
type CollateralRequirement struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	MinCoverageBps int64     `json:"min_coverage_bps"`
}

// MarginCall is the collateral alert: the account's coverage in
// currency fell below the required ratio. Returned by
// RevalueCollateral / ReleaseCollateral / SetCollateralRequirement
// when they breach coverage, and queryable at any time with
// CheckMarginCall. At is the evaluation time, not a persisted
// timestamp.
type MarginCall struct {
	Account         AccountID `json:"account"`
	Currency        string    `json:"currency"`
	EffectiveCents  int64     `json:"effective_collateral_cents"`
	DrawnCents      int64     `json:"drawn_cents"`
	CoverageBps     int64     `json:"coverage_bps"`
	RequiredBps     int64     `json:"required_bps"`
	At              time.Time `json:"at"`
}

// Validation and state errors returned by the collateral APIs.
var (
	ErrEmptyCollateralAccount = errors.New("ledger: collateral account must not be empty")
	ErrEmptyCollateralAsset   = errors.New("ledger: collateral asset ID must not be empty")
	ErrCollateralNonPositiveQuantity = errors.New("ledger: collateral quantity must be greater than zero")
	ErrCollateralNonPositiveValue = errors.New("ledger: collateral unit value must be greater than zero")
	// ErrInvalidCollateralHaircut is returned when haircutBps is
	// outside 0..10000: a haircut above 100% would make the asset
	// count negative — that is a configuration bug, not a clampable
	// value (same bound style as the credit-line rate).
	ErrInvalidCollateralHaircut = errors.New("ledger: collateral haircut must be between 0 and 10000 basis points")
	// ErrCollateralNotFound is returned for operations on an
	// (account, asset) pair with no pledged position.
	ErrCollateralNotFound = errors.New("ledger: no collateral pledged for this account and asset")
	// ErrCollateralReleaseExceedsPledge is returned by
	// ReleaseCollateral when the release quantity exceeds the pledged
	// quantity: collateral is never released short.
	ErrCollateralReleaseExceedsPledge = errors.New("ledger: collateral release exceeds pledged quantity")
	// ErrInvalidCoverageRatio is returned by
	// SetCollateralRequirement for a negative ratio: 0 clears the
	// requirement, any positive ratio sets it.
	ErrInvalidCoverageRatio = errors.New("ledger: collateral coverage ratio must not be negative")
	// ErrInsufficientCollateral is returned by the Post / PostTransfer
	// gates when a new draw would leave the account's utilization
	// uncovered at the required ratio: the draw is rejected, nothing
	// is recorded.
	ErrInsufficientCollateral = errors.New("ledger: draw exceeds collateral coverage")
)

// collateralEffectiveValue computes one position's effective
// collateral value: floor(quantity * unitValue * (10000 - haircut) /
// 10000), overflow-safe. The gross product saturates at
// math.MaxInt64; the haircut leg reuses the overflow-safe
// policyFeeCents basis-point math.
func collateralEffectiveValue(quantity, unitValueCents, haircutBps int64) int64 {
	var gross int64
	if quantity > 0 && unitValueCents > math.MaxInt64/quantity {
		gross = math.MaxInt64
	} else {
		gross = quantity * unitValueCents
	}
	return gross - policyFeeCents(gross, haircutBps)
}

// collateralCovers reports whether effective collateral covers drawn
// utilization at minCoverageBps: effective >= ceil(drawn *
// minCoverageBps / 10000), overflow-safe. A non-positive drawn
// amount or ratio is trivially covered — nothing is drawn, or nothing
// is required.
func collateralCovers(effective, drawn, minCoverageBps int64) bool {
	if drawn <= 0 || minCoverageBps <= 0 {
		return true
	}
	if drawn > math.MaxInt64/minCoverageBps {
		// The required coverage exceeds any int64 collateral: no
		// position, however large, can cover this draw.
		return false
	}
	required := (drawn*minCoverageBps + 9999) / 10000
	return effective >= required
}

// collateralCoverageBps renders the coverage ratio in basis points:
// floor(effective * 10000 / drawn), overflow-safe. -1 when nothing is
// drawn — the ratio is undefined and trivially satisfied, and -1
// keeps it distinguishable from a real 0% reading.
func collateralCoverageBps(effective, drawn int64) int64 {
	if drawn <= 0 {
		return -1
	}
	return effective/drawn*10000 + (effective%drawn)*10000/drawn
}

// PledgeCollateral pledges quantity units of assetID for account at
// unitValueCents per unit with a haircutBps haircut. Pledging is
// cumulative per (account, asset): a repeat pledge adds the quantity
// and marks the position to market at the new unit value and haircut,
// keeping the original PledgedAt. Pledging never bumps the journal
// version and is audited as collateral_pledged.
func (l *Ledger) PledgeCollateral(account AccountID, assetID string, quantity, unitValueCents, haircutBps int64) (CollateralPledge, error) {
	if account == "" {
		return CollateralPledge{}, ErrEmptyCollateralAccount
	}
	if strings.TrimSpace(assetID) == "" {
		return CollateralPledge{}, ErrEmptyCollateralAsset
	}
	if quantity <= 0 {
		return CollateralPledge{}, ErrCollateralNonPositiveQuantity
	}
	if unitValueCents <= 0 {
		return CollateralPledge{}, ErrCollateralNonPositiveValue
	}
	if haircutBps < 0 || haircutBps > 10000 {
		return CollateralPledge{}, ErrInvalidCollateralHaircut
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := collateralKey{account: account, asset: assetID}
	p, exists := l.collateralPledges[k]
	if exists {
		p.Quantity += quantity
		p.UnitValueCents = unitValueCents
		p.HaircutBps = haircutBps
	} else {
		p = CollateralPledge{
			Account:        account,
			AssetID:        assetID,
			Quantity:       quantity,
			UnitValueCents: unitValueCents,
			HaircutBps:     haircutBps,
			PledgedAt:      time.Now(),
		}
	}
	l.collateralPledges[k] = p
	l.emitAudit(AuditEvent{
		Op:            "collateral_pledged",
		Actor:         "PledgeCollateral",
		TraceID:       string(account) + "/" + assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"asset_id":         assetID,
			"quantity":         p.Quantity,
			"unit_value_cents": p.UnitValueCents,
			"haircut_bps":      p.HaircutBps,
		},
	})
	return p, nil
}

// ReleaseCollateral releases quantity units of assetID from the
// account's pledged position. Releasing more than pledged is refused
// fail-closed; releasing the whole position deletes the row. A
// release can drop coverage below the required ratio, so it runs the
// margin scan and returns any margin calls it triggers. The release
// never bumps the journal version and is audited as
// collateral_released.
func (l *Ledger) ReleaseCollateral(account AccountID, assetID string, quantity int64) ([]MarginCall, error) {
	if account == "" {
		return nil, ErrEmptyCollateralAccount
	}
	if strings.TrimSpace(assetID) == "" {
		return nil, ErrEmptyCollateralAsset
	}
	if quantity <= 0 {
		return nil, ErrCollateralNonPositiveQuantity
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := collateralKey{account: account, asset: assetID}
	p, exists := l.collateralPledges[k]
	if !exists {
		return nil, ErrCollateralNotFound
	}
	if quantity > p.Quantity {
		return nil, ErrCollateralReleaseExceedsPledge
	}
	p.Quantity -= quantity
	if p.Quantity == 0 {
		delete(l.collateralPledges, k)
	} else {
		l.collateralPledges[k] = p
	}
	l.emitAudit(AuditEvent{
		Op:            "collateral_released",
		Actor:         "ReleaseCollateral",
		TraceID:       string(account) + "/" + assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"asset_id":          assetID,
			"released_quantity": quantity,
			"remaining":         p.Quantity,
		},
	})
	return l.checkMarginCallsLocked(account), nil
}

// RevalueCollateral marks the account's assetID position to a new
// market value per unit. The revaluation can drop coverage below the
// required ratio, so it runs the margin scan: the returned margin
// calls are the alert, and each one is also recorded as a
// collateral_margin_call audit event. The revaluation never bumps the
// journal version and is audited as collateral_revalued.
func (l *Ledger) RevalueCollateral(account AccountID, assetID string, unitValueCents int64) ([]MarginCall, error) {
	if account == "" {
		return nil, ErrEmptyCollateralAccount
	}
	if strings.TrimSpace(assetID) == "" {
		return nil, ErrEmptyCollateralAsset
	}
	if unitValueCents <= 0 {
		return nil, ErrCollateralNonPositiveValue
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := collateralKey{account: account, asset: assetID}
	p, exists := l.collateralPledges[k]
	if !exists {
		return nil, ErrCollateralNotFound
	}
	p.UnitValueCents = unitValueCents
	l.collateralPledges[k] = p
	l.emitAudit(AuditEvent{
		Op:            "collateral_revalued",
		Actor:         "RevalueCollateral",
		TraceID:       string(account) + "/" + assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"asset_id":         assetID,
			"unit_value_cents": unitValueCents,
		},
	})
	return l.checkMarginCallsLocked(account), nil
}

// SetCollateralRequirement sets the account's minimum collateral
// coverage ratio in currency to minCoverageBps basis points (10000 =
// 100%): the account opts in to collateral-gated draws. A 0 ratio
// clears the requirement. Raising the ratio can breach coverage
// immediately, so it runs the margin scan and returns any margin
// calls it triggers. Structural config — never bumps the journal
// version — audited as collateral_requirement_set.
func (l *Ledger) SetCollateralRequirement(account AccountID, currency string, minCoverageBps int64) ([]MarginCall, error) {
	if account == "" {
		return nil, ErrEmptyCollateralAccount
	}
	if minCoverageBps < 0 {
		return nil, ErrInvalidCoverageRatio
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := collateralReqKey{account: account, currency: cur}
	if minCoverageBps == 0 {
		delete(l.collateralReqs, k)
	} else {
		l.collateralReqs[k] = CollateralRequirement{
			Account:        account,
			Currency:       cur,
			MinCoverageBps: minCoverageBps,
		}
	}
	l.emitAudit(AuditEvent{
		Op:            "collateral_requirement_set",
		Actor:         "SetCollateralRequirement",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"currency":         cur,
			"min_coverage_bps": minCoverageBps,
		},
	})
	return l.checkMarginCallsLocked(account), nil
}

// GetCollateralRequirement returns the account's coverage requirement
// for currency, or false when none is set.
func (l *Ledger) GetCollateralRequirement(account AccountID, currency string) (CollateralRequirement, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	req, ok := l.collateralReqs[collateralReqKey{account: account, currency: currency}]
	return req, ok
}

// GetCollateral returns the pledged position for (account, assetID),
// or false when none is pledged.
func (l *Ledger) GetCollateral(account AccountID, assetID string) (CollateralPledge, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.collateralPledges[collateralKey{account: account, asset: assetID}]
	return p, ok
}

// CollateralPositions lists every pledged position of the account,
// sorted by asset ID: the read side of the registry.
func (l *Ledger) CollateralPositions(account AccountID) []CollateralPledge {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]CollateralPledge, 0)
	for k, p := range l.collateralPledges {
		if k.account == account {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out
}

// collateralEffectiveLocked sums the account's pledged positions at
// their effective (post-haircut) values. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) collateralEffectiveLocked(account AccountID) int64 {
	var total int64
	for k, p := range l.collateralPledges {
		if k.account != account {
			continue
		}
		v := collateralEffectiveValue(p.Quantity, p.UnitValueCents, p.HaircutBps)
		if total > math.MaxInt64-v {
			return math.MaxInt64
		}
		total += v
	}
	return total
}

// CollateralCoverage reports the account's collateral coverage in
// currency: the effective (post-haircut) collateral value, the drawn
// utilization (the negative part of the balance, zero when the balance
// is non-negative), and the coverage ratio in basis points (-1 when
// nothing is drawn: the ratio is undefined and trivially satisfied).
// ok is false when the account carries no coverage requirement for
// the currency; an empty currency means the default currency.
func (l *Ledger) CollateralCoverage(account AccountID, currency string) (effective, drawn, coverageBps int64, ok bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, exists := l.collateralReqs[collateralReqKey{account: account, currency: currency}]; !exists {
		return 0, 0, 0, false
	}
	effective = l.collateralEffectiveLocked(account)
	if bal := l.balances[accountCurrency{account: account, currency: currency}]; bal < 0 {
		drawn = -bal
	}
	return effective, drawn, collateralCoverageBps(effective, drawn), true
}

// CheckMarginCall evaluates the account's current coverage in
// currency against its requirement and returns the margin call when
// coverage is below the required ratio, or nil when coverage holds
// (or no requirement is set). It is the query side of the margin
// scan; the mutating paths call checkMarginCallsLocked directly.
func (l *Ledger) CheckMarginCall(account AccountID, currency string) *MarginCall {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, mc := range l.checkMarginCallsLocked(account) {
		if mc.Currency == currency {
			out := mc
			return &out
		}
	}
	return nil
}

// checkMarginCallsLocked scans every coverage requirement of the
// account and returns one margin call per breaching currency, each
// also recorded as a collateral_margin_call audit event. Callers must
// hold l.mu.
func (l *Ledger) checkMarginCallsLocked(account AccountID) []MarginCall {
	now := time.Now()
	effective := l.collateralEffectiveLocked(account)
	var out []MarginCall
	for k, req := range l.collateralReqs {
		if k.account != account {
			continue
		}
		var drawn int64
		if bal := l.balances[accountCurrency{account: k.account, currency: k.currency}]; bal < 0 {
			drawn = -bal
		}
		if collateralCovers(effective, drawn, req.MinCoverageBps) {
			continue
		}
		mc := MarginCall{
			Account:        account,
			Currency:       k.currency,
			EffectiveCents: effective,
			DrawnCents:     drawn,
			CoverageBps:    collateralCoverageBps(effective, drawn),
			RequiredBps:    req.MinCoverageBps,
			At:             now,
		}
		out = append(out, mc)
		l.emitAudit(AuditEvent{
			Op:            "collateral_margin_call",
			Actor:         "collateral",
			TraceID:       string(account) + "/" + k.currency,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{account},
			Details: map[string]any{
				"currency":               k.currency,
				"effective_collateral_cents": effective,
				"drawn_cents":            drawn,
				"coverage_bps":           mc.CoverageBps,
				"required_bps":           req.MinCoverageBps,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// collateralRejectedLocked reports whether a new outflow of
// outflowCents from account in currency would breach the account's
// collateral coverage requirement: the projected utilization after
// the outflow must stay covered at the required ratio. Only the
// credit (payer) side is gated — the account funds leave there —
// matching the overdraft and daily-limit gates. Callers must hold
// l.mu; the admission path holds the write lock.
func (l *Ledger) collateralRejectedLocked(account AccountID, currency string, outflowCents int64) error {
	req, ok := l.collateralReqs[collateralReqKey{account: account, currency: currency}]
	if !ok || req.MinCoverageBps <= 0 {
		return nil
	}
	balance := l.balances[accountCurrency{account: account, currency: currency}]
	if outflowCents >= 0 && balance <= math.MinInt64+outflowCents {
		// balance - outflowCents would underflow int64 (or land
		// exactly on MinInt64, whose negation overflows): the
		// projected utilization is unbounded, and no collateral
		// position can cover it.
		return ErrInsufficientCollateral
	}
	var projected int64
	if after := balance - outflowCents; after < 0 {
		projected = -after
	}
	if projected <= 0 {
		// No draw — the outflow does not take the account
		// negative, so there is nothing for collateral to cover.
		return nil
	}
	if !collateralCovers(l.collateralEffectiveLocked(account), projected, req.MinCoverageBps) {
		return ErrInsufficientCollateral
	}
	return nil
}

// collateralPledgesLocked is the sorted pledge listing used by the
// snapshot config export. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) collateralPledgesLocked() []CollateralPledge {
	out := make([]CollateralPledge, 0, len(l.collateralPledges))
	for _, p := range l.collateralPledges {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].AssetID < out[j].AssetID
	})
	return out
}

// collateralReqsLocked is the sorted requirement listing used by the
// snapshot config export. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) collateralReqsLocked() []CollateralRequirement {
	out := make([]CollateralRequirement, 0, len(l.collateralReqs))
	for _, r := range l.collateralReqs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}
