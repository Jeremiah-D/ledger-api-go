package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Manual FX rate overrides with dual control (LG-69).
//
// The rate table (see fx.go) is the market price: SetFXRate installs what
// the feed says. When the feed is wrong — a stale quote, a fat-fingered
// upstream, a market dislocation — an operator can install a manual
// override price for one directional pair, but the fintech maker-checker
// rule applies: SetRateOverride only *proposes* (the maker); the override
// takes effect only after a *second* operator approves it
// (ApproveRateOverride, the checker), and the proposer can never approve
// their own override.
//
// While an override is active, every cross-currency transfer for the pair
// converts at the override price instead of the market rate, and each
// such conversion emits an fx_rate_overridden audit event binding the
// transfer to the override's provenance (proposer, approver, reason,
// both rates, the deviation). The receipt discloses the override too
// (see FXConversion.OverrideApplied), so the payee-side settlement is
// never silently re-priced.
//
// Two guardrails bound the override's blast radius:
//
//   - Deviation cap: the override price may not deviate from the live
//     market rate by more than the configured basis-points cap (default
//     10%, see WithRateOverrideMaxDeviationBps), checked at proposal
//     and re-checked at approval — a market that moved between the two
//     blocks the approval, so the checker never rubber-stamps a stale
//     price. Deviation is exact big.Rat math; no float touches money.
//   - Auto-expiry: every override carries a TTL (per-call, defaulting
//     to WithRateOverrideTTL). An override past its ExpiresAt is never
//     applied — expiry is lazy by predicate in the conversion path,
//     exactly like rate expiry — and ExpireRateOverrides (swept on a
//     timer by the operator, like ExpireReviews) marks lapsed overrides
//     expired and audits them. There is no perpetual override.
//
// Overrides are operational state, like transfer reviews: they do not
// round-trip through ExportSnapshot/ImportSnapshot. After a restore,
// operators re-propose. They are structural in the other sense: they
// never bump the ledger version and book no journal rows.
//
// Override lifecycle: pending -> active -> expired, or pending ->
// rejected. Approving an already-active override is a no-op (idempotent);
// rejecting a rejected/expired one is likewise a no-op. An active
// override cannot be rejected — it lapses on its own TTL.

// Manual FX rate override statuses. An override prices conversions only
// while active; every other status is terminal.
const (
	RateOverrideStatusPending  = "pending"
	RateOverrideStatusActive   = "active"
	RateOverrideStatusRejected = "rejected"
	RateOverrideStatusExpired  = "expired"
)

// Defaults: a one-hour override lifetime and a 10% deviation cap. Both
// are construction-time config (see WithRateOverrideTTL and
// WithRateOverrideMaxDeviationBps).
const (
	defaultRateOverrideTTL             = time.Hour
	defaultRateOverrideMaxDeviationBps = 1000 // 10%
)

// Validation and state errors returned by the rate-override APIs.
var (
	// ErrRateOverrideNotFound is returned for an unknown (from, to)
	// pair: no override was ever proposed for it.
	ErrRateOverrideNotFound = errors.New("ledger: FX rate override not found")
	// ErrRateOverrideNotPending is returned when a decision targets an
	// override that is no longer pending. Approving an already-active
	// override is the one exception — it is a no-op returning nil,
	// idempotent like ApproveTransferReview.
	ErrRateOverrideNotPending = errors.New("ledger: FX rate override is not pending")
	// ErrRateOverrideConflict is returned by SetRateOverride when the
	// pair already has a pending or active override: one override per
	// pair at a time, so two makers cannot stack competing prices.
	ErrRateOverrideConflict = errors.New("ledger: FX rate override already pending or active for pair")
	// ErrRateOverrideSameApprover is returned by ApproveRateOverride
	// when the approver is the proposer: maker-checker requires two
	// distinct operators. Note both default to "operator" when the
	// identity is left empty, so completing the flow requires explicit
	// operator names.
	ErrRateOverrideSameApprover = errors.New("ledger: FX rate override approver must differ from the proposer")
	// ErrRateOverrideDeviationExceeded is returned when the override
	// price deviates from the live market rate by more than the
	// configured cap — at proposal, and re-checked at approval. The
	// override stays pending, so the proposer can re-propose at a fresh
	// price.
	ErrRateOverrideDeviationExceeded = errors.New("ledger: FX rate override deviates from the market rate beyond the configured cap")
	// ErrRateOverrideNoMarketRate is returned when the pair has no live
	// market rate to deviate from: an override prices *against* the
	// market, so it cannot exist for a pair the table does not quote
	// (or whose quote has expired).
	ErrRateOverrideNoMarketRate = errors.New("ledger: FX rate override requires a live market rate for the pair")
	// ErrInvalidRateOverride is returned for malformed override inputs:
	// bad currency codes, a same-currency pair, a non-positive rate, an
	// empty reason, or a negative TTL.
	ErrInvalidRateOverride = errors.New("ledger: invalid FX rate override")
)

// RateOverride is one manual FX rate override: the price an operator
// proposed, the maker-checker decision on it, and its active window.
//
// Num/Den is the override price: converting fromCurrency to toCurrency
// books floor(amount * Num / Den) while the override is active.
// MarketNum/MarketDen snapshots the market rate the deviation was
// measured against at proposal, for auditability. ExpiresAt is
// exclusive — the override is inactive once now >= ExpiresAt — and is
// always set for an active override: there is no perpetual override.
// EffectiveVersion is the ledger version at which the override took
// effect (approval time); conversions priced at it disclose this
// version, like ExchangeRate.EffectiveVersion.
type RateOverride struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Num          int64  `json:"num"`
	Den          int64  `json:"den"`
	MarketNum    int64  `json:"market_num"`
	MarketDen    int64  `json:"market_den"`
	Reason       string `json:"reason"`
	Proposer     string `json:"proposer"`
	Approver     string `json:"approver,omitempty"`
	Status       string `json:"status"`
	TTL          string `json:"ttl"`
	CreatedAt    time.Time `json:"created_at"`
	// ExpiresAt is set at approval: CreatedAt + the proposal's TTL.
	// Zero while pending.
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	ApprovedAt       time.Time `json:"approved_at,omitempty"`
	EffectiveVersion uint64    `json:"effective_version"`
}

// WithRateOverrideTTL sets the default lifetime of a manual FX rate
// override, applied when SetRateOverride is called with a non-positive
// ttl. A non-positive duration keeps the built-in default (one hour):
// the default is a control, not an accident — an override that never
// expires would be a standing re-pricing of the pair, which is what the
// rate table (with its own expiry) is for.
func WithRateOverrideTTL(d time.Duration) Option {
	return func(l *Ledger) {
		if d > 0 {
			l.rateOverrideTTL = d
		}
	}
}

// WithRateOverrideMaxDeviationBps caps |override - market| / market in
// basis points, enforced at proposal and re-checked at approval.
// Default 1000 (10%). A negative cap panics: fail-fast at construction,
// like the review threshold, so a misconfigured risk control can never
// silently allow unbounded re-pricing.
func WithRateOverrideMaxDeviationBps(bps int64) Option {
	if bps < 0 {
		panic("ledger: rate override max deviation must not be negative")
	}
	return func(l *Ledger) {
		l.rateOverrideMaxDeviationBps = bps
	}
}

// overrideDeviationExceeded reports whether |override - market| / market
// exceeds maxBps basis points. Exact big.Rat math — no float64 ever
// touches money. Callers must pass positive ratios.
func overrideDeviationExceeded(oNum, oDen, mNum, mDen int64, maxBps int64) bool {
	o := new(big.Rat).SetFrac64(oNum, oDen)
	m := new(big.Rat).SetFrac64(mNum, mDen)
	dev := new(big.Rat).Sub(o, m)
	dev.Abs(dev)
	dev.Quo(dev, m)
	return dev.Cmp(new(big.Rat).SetFrac64(maxBps, 10000)) > 0
}

// overrideDeviationBpsString renders |override - market| / market in
// basis points with two decimals, for the audit trail.
func overrideDeviationBpsString(oNum, oDen, mNum, mDen int64) string {
	o := new(big.Rat).SetFrac64(oNum, oDen)
	m := new(big.Rat).SetFrac64(mNum, mDen)
	dev := new(big.Rat).Sub(o, m)
	dev.Abs(dev)
	dev.Quo(dev, m)
	dev.Mul(dev, big.NewRat(10000, 1))
	return dev.FloatString(2)
}

// overrideActiveLocked reports whether the override currently prices
// conversions: approved and not lapsed. Expiry is lazy by predicate —
// an override past its ExpiresAt is already inactive for conversion
// purposes, even before ExpireRateOverrides sweeps it. Callers must hold
// l.mu.
func overrideActiveLocked(o *RateOverride, now time.Time) bool {
	return o.Status == RateOverrideStatusActive && now.Before(o.ExpiresAt)
}

// activeRateOverrideLocked returns the override currently pricing the
// pair, or false when none is active. The returned value is a copy;
// mutating it does not affect the ledger. Callers must hold l.mu.
func (l *Ledger) activeRateOverrideLocked(p fxPair, now time.Time) (RateOverride, bool) {
	o, ok := l.rateOverrides[p]
	if !ok || !overrideActiveLocked(o, now) {
		return RateOverride{}, false
	}
	return *o, true
}

// SetRateOverride proposes a manual FX rate override for the directional
// pair (from, to): the maker half of the dual control. The override
// takes effect only after ApproveRateOverride by a *different* operator.
//
// rate is the override price as an exact rational (see
// ParseFXRateDecimal for operator-supplied decimal strings); ttl is the
// override's lifetime once approved — non-positive selects the
// configured default (WithRateOverrideTTL). reason documents *why* the
// market price is being set aside and is required: a control without a
// documented reason is not a control. An empty proposer is recorded as
// "operator".
//
// The proposal is validated against the live market rate: the pair must
// have an unexpired market rate, and |override - market| / market must
// stay within the configured deviation cap — otherwise the proposal is
// rejected and nothing is recorded. One override per pair at a time: a
// pending or active override blocks a new proposal with
// ErrRateOverrideConflict.
//
// Proposing is structural, like SetFXRate: it takes the write lock, does
// not bump the ledger version, and does not round-trip through
// snapshots.
func (l *Ledger) SetRateOverride(from, to string, rate *big.Rat, ttl time.Duration, reason, proposer string) error {
	if rate == nil || rate.Sign() <= 0 {
		return fmt.Errorf("%w: rate must be positive", ErrInvalidRateOverride)
	}
	num, den := rate.Num(), rate.Denom()
	if !num.IsInt64() || !den.IsInt64() {
		return fmt.Errorf("%w: rate %s/%s does not fit int64", ErrInvalidRateOverride, num, den)
	}
	f, t, err := normalizeFXRate(from, to, num.Int64(), den.Int64())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRateOverride, err)
	}
	if ttl < 0 {
		return fmt.Errorf("%w: ttl must not be negative", ErrInvalidRateOverride)
	}
	if reason == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidRateOverride)
	}
	if proposer == "" {
		proposer = defaultReviewOperator
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	// The override prices against the market: without a live market
	// rate there is no deviation to bound, so the control refuses to
	// invent a price for an unquoted pair.
	market, ok := l.fxRates[fxPair{from: f, to: t}]
	if !ok || (!market.ExpiresAt.IsZero() && !now.Before(market.ExpiresAt)) {
		return ErrRateOverrideNoMarketRate
	}
	p := fxPair{from: f, to: t}
	if o, exists := l.rateOverrides[p]; exists &&
		(o.Status == RateOverrideStatusPending || o.Status == RateOverrideStatusActive) {
		return ErrRateOverrideConflict
	}
	if overrideDeviationExceeded(num.Int64(), den.Int64(), market.Num, market.Den, l.rateOverrideMaxDeviationBps) {
		return fmt.Errorf("%w: %s bps over a %d bps cap",
			ErrRateOverrideDeviationExceeded,
			overrideDeviationBpsString(num.Int64(), den.Int64(), market.Num, market.Den),
			l.rateOverrideMaxDeviationBps)
	}

	effectiveTTL := ttl
	if effectiveTTL <= 0 {
		effectiveTTL = l.rateOverrideTTL
	}
	l.rateOverrides[p] = &RateOverride{
		FromCurrency: f,
		ToCurrency:   t,
		Num:          num.Int64(),
		Den:          den.Int64(),
		MarketNum:    market.Num,
		MarketDen:    market.Den,
		Reason:       reason,
		Proposer:     proposer,
		Status:       RateOverrideStatusPending,
		TTL:          effectiveTTL.String(),
		CreatedAt:    now,
	}
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_override_proposed",
		Actor:         "SetRateOverride",
		TraceID:       fmt.Sprintf("%s->%s", f, t),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from_currency":  f,
			"to_currency":    t,
			"override_rate":  fmt.Sprintf("%d/%d", num.Int64(), den.Int64()),
			"market_rate":    fmt.Sprintf("%d/%d", market.Num, market.Den),
			"deviation_bps":  overrideDeviationBpsString(num.Int64(), den.Int64(), market.Num, market.Den),
			"deviation_cap_bps": l.rateOverrideMaxDeviationBps,
			"ttl":            effectiveTTL.String(),
			"reason":         reason,
			"proposed_by":    proposer,
		},
	})
	return nil
}

// ApproveRateOverride is the checker half of the dual control: a second
// operator approves a pending override, activating it from now until
// now+TTL. The approver must differ from the proposer
// (ErrRateOverrideSameApprover); an empty approver is recorded as
// "operator".
//
// Approval re-validates against the *current* market rate: the market
// may have moved (or lapsed) between proposal and approval, and the
// checker must never rubber-stamp a price whose deviation is no longer
// within the cap. A failed re-check refuses the approval with
// ErrRateOverrideDeviationExceeded (or ErrRateOverrideNoMarketRate) and
// leaves the override pending, so the proposer can re-propose at a
// fresh price.
//
// Approving an already-active override is a no-op returning nil —
// idempotent, like ApproveTransferReview. Approving a rejected or
// expired override fails with ErrRateOverrideNotPending.
func (l *Ledger) ApproveRateOverride(from, to string, approver string) error {
	f, err := normalizeCurrency(from)
	if err != nil || from == "" {
		return fmt.Errorf("%w: from currency %q", ErrInvalidRateOverride, from)
	}
	t, err := normalizeCurrency(to)
	if err != nil || to == "" {
		return fmt.Errorf("%w: to currency %q", ErrInvalidRateOverride, to)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	p := fxPair{from: f, to: t}
	o, ok := l.rateOverrides[p]
	if !ok {
		return ErrRateOverrideNotFound
	}
	if o.Status == RateOverrideStatusActive {
		return nil
	}
	if o.Status != RateOverrideStatusPending {
		return ErrRateOverrideNotPending
	}
	if approver == "" {
		approver = defaultReviewOperator
	}
	if approver == o.Proposer {
		return ErrRateOverrideSameApprover
	}

	now := time.Now()
	market, ok := l.fxRates[p]
	if !ok || (!market.ExpiresAt.IsZero() && !now.Before(market.ExpiresAt)) {
		return ErrRateOverrideNoMarketRate
	}
	if overrideDeviationExceeded(o.Num, o.Den, market.Num, market.Den, l.rateOverrideMaxDeviationBps) {
		return fmt.Errorf("%w: market moved to %d/%d since proposal (%s bps over a %d bps cap)",
			ErrRateOverrideDeviationExceeded, market.Num, market.Den,
			overrideDeviationBpsString(o.Num, o.Den, market.Num, market.Den),
			l.rateOverrideMaxDeviationBps)
	}

	ttl, err := time.ParseDuration(o.TTL)
	if err != nil || ttl <= 0 {
		ttl = l.rateOverrideTTL
	}
	o.Status = RateOverrideStatusActive
	o.Approver = approver
	o.ApprovedAt = now
	o.ExpiresAt = now.Add(ttl)
	o.EffectiveVersion = l.version
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_override_approved",
		Actor:         "ApproveRateOverride",
		TraceID:       fmt.Sprintf("%s->%s", f, t),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from_currency":  f,
			"to_currency":    t,
			"override_rate":  fmt.Sprintf("%d/%d", o.Num, o.Den),
			"market_rate":    fmt.Sprintf("%d/%d", market.Num, market.Den),
			"deviation_bps":  overrideDeviationBpsString(o.Num, o.Den, market.Num, market.Den),
			"expires_at":     o.ExpiresAt.UTC().Format(time.RFC3339),
			"proposed_by":    o.Proposer,
			"approved_by":    approver,
			"reason":         o.Reason,
		},
	})
	return nil
}

// RejectRateOverride drops a pending override without ever pricing a
// conversion: the market rate keeps converting the pair. Rejecting is
// idempotent: a rejected or expired override returns as-is with
// changed == false and no error. Rejecting an active override fails
// with ErrRateOverrideNotPending — an active override lapses on its own
// TTL; there is no revocation. An empty reviewer is recorded as
// "operator".
func (l *Ledger) RejectRateOverride(from, to string, reviewer, reason string) (override RateOverride, changed bool, err error) {
	f, ferr := normalizeCurrency(from)
	if ferr != nil || from == "" {
		return RateOverride{}, false, fmt.Errorf("%w: from currency %q", ErrInvalidRateOverride, from)
	}
	t, terr := normalizeCurrency(to)
	if terr != nil || to == "" {
		return RateOverride{}, false, fmt.Errorf("%w: to currency %q", ErrInvalidRateOverride, to)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	p := fxPair{from: f, to: t}
	o, ok := l.rateOverrides[p]
	if !ok {
		return RateOverride{}, false, ErrRateOverrideNotFound
	}
	if o.Status == RateOverrideStatusRejected || o.Status == RateOverrideStatusExpired {
		return *o, false, nil
	}
	if o.Status != RateOverrideStatusPending {
		return RateOverride{}, false, ErrRateOverrideNotPending
	}
	if reviewer == "" {
		reviewer = defaultReviewOperator
	}
	o.Status = RateOverrideStatusRejected
	details := map[string]any{
		"from_currency": f,
		"to_currency":   t,
		"override_rate": fmt.Sprintf("%d/%d", o.Num, o.Den),
		"proposed_by":   o.Proposer,
		"rejected_by":   reviewer,
		"reason":        o.Reason,
	}
	if reason != "" {
		details["reject_reason"] = reason
	}
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_override_rejected",
		Actor:         "RejectRateOverride",
		TraceID:       fmt.Sprintf("%s->%s", f, t),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return *o, true, nil
}

// GetRateOverride returns the override record for the directional pair,
// or false when none was ever proposed. The returned record is a copy;
// mutating it does not affect the ledger.
func (l *Ledger) GetRateOverride(from, to string) (RateOverride, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	o, ok := l.rateOverrides[fxPair{from: from, to: to}]
	if !ok {
		return RateOverride{}, false
	}
	return *o, true
}

// RateOverrides lists every override ever proposed, sorted by
// (from, to) — pending, active, rejected, and expired alike, so
// operators can see the pair's full override history. The returned
// records are copies.
func (l *Ledger) RateOverrides() []RateOverride {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]RateOverride, 0, len(l.rateOverrides))
	for _, o := range l.rateOverrides {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromCurrency != out[j].FromCurrency {
			return out[i].FromCurrency < out[j].FromCurrency
		}
		return out[i].ToCurrency < out[j].ToCurrency
	})
	return out
}

// ExpireRateOverrides marks every active override whose ExpiresAt has
// passed as expired and returns how many were marked. Lapsed overrides
// were already inactive for conversion purposes (expiry is lazy by
// predicate); the sweep exists so operators can observe which overrides
// lapsed. It never touches pending, rejected, or already-expired
// overrides, and it does not bump the ledger version. A sweep that
// expires nothing emits no audit event — zero-expire ticks would drown
// the audit trail in noise (see ExpireReviewsAt for the convention).
func (l *Ledger) ExpireRateOverrides() int {
	return l.ExpireRateOverridesAt(time.Now())
}

// ExpireRateOverridesAt is ExpireRateOverrides with a caller-supplied
// timestamp, so tests can pin expiry deterministically.
func (l *Ledger) ExpireRateOverridesAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked := 0
	var pairs []string
	for p, o := range l.rateOverrides {
		if o.Status == RateOverrideStatusActive && !now.Before(o.ExpiresAt) {
			o.Status = RateOverrideStatusExpired
			marked++
			pairs = append(pairs, fmt.Sprintf("%s->%s", p.from, p.to))
		}
	}
	if marked > 0 {
		sort.Strings(pairs)
		l.emitAudit(AuditEvent{
			Op:            "fx_rate_override_expired",
			Actor:         "ExpireRateOverrides",
			TraceID:       fmt.Sprintf("fx-override-expire@%d", l.version),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"expired": marked,
				"pairs":   pairs,
			},
		})
	}
	return marked
}

// FXRateOverrideConversionCount reports how many cross-currency
// transfers have converted at an approved manual override price since
// process start (fx_rate_overridden audit events). The HTTP layer syncs
// this into ledger_fx_rate_overridden_total on every /metrics scrape.
// In-memory only — it does not round-trip through snapshots.
func (l *Ledger) FXRateOverrideConversionCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxOverrideConversions
}

// emitRateOverriddenAuditLocked records one fx_rate_overridden event:
// the conversion priced at an approved manual override instead of the
// market rate. It binds the transfer to the override's full provenance
// — proposer, approver, reason, both rates, and the measured deviation
// — so the re-pricing is auditable per conversion. Callers must hold
// the write lock; the event is emitted from the FX write path right
// after the review gate, covering both directly-settled and
// review-frozen transfers (nothing after the gate can fail).
func (l *Ledger) emitRateOverriddenAuditLocked(t Transfer, toCurrency string, override RateOverride, market ExchangeRate, convertedCents int64) {
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_overridden",
		Actor:         "PostTransfer",
		TraceID:       t.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{t.From, t.To},
		Details: map[string]any{
			"from_currency":      t.Currency,
			"to_currency":        toCurrency,
			"override_rate":      fmt.Sprintf("%d/%d", override.Num, override.Den),
			"market_rate":        fmt.Sprintf("%d/%d", market.Num, market.Den),
			"deviation_bps":      overrideDeviationBpsString(override.Num, override.Den, market.Num, market.Den),
			"proposed_by":        override.Proposer,
			"approved_by":        override.Approver,
			"reason":             override.Reason,
			"override_expires_at": override.ExpiresAt.UTC().Format(time.RFC3339),
			"source_cents":       t.AmountCents,
			"converted_cents":    convertedCents,
		},
	})
}
