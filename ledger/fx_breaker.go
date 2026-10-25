package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// FX rate-move circuit breaker (LG-214, fintech: FX risk control).
//
// The rate table can move violently — a bad feed, a fat-fingered
// upstream quote, a market dislocation — and every cross-currency
// transfer that settles afterwards converts at the new price. The
// expiry watch (LG-68) only warns about stale rates, the triangular
// consistency scan (LG-76) only advises about self-inconsistent
// tables, and the manual-override deviation cap (LG-69) only bounds
// operator-proposed prices: none of them *stops* a spot settlement
// when the market itself jumps. The breaker is the stop.
//
// Per directional pair, opt-in: SetFXRateBreaker installs a maximum
// deviation in basis points and anchors the pair to the market rate
// in effect at that moment (the "anchor": the table's mid price and
// its EffectiveVersion). No breaker configured means no behavior
// change — every existing transfer settles exactly as before.
//
// At spot-settlement time, before anything books, the live market
// mid for the pair is compared with the anchor (exact big.Rat math,
// the same fxSlippageBps helper the LG-86 slippage gate uses, so
// deviation is floor(|live − anchor| / anchor * 10000) bps). A live
// mid more than MaxDeviationBps from the anchor trips the breaker:
//
//   - the settlement is rejected with ErrFXRateBreakerTripped
//     (422-class, like ErrFXRateMissing: well-formed, but the
//     ledger refuses to convert at this price);
//   - one fx_rate_breaker_tripped audit event records the pair, the
//     anchor, the live rate, the deviation, and the threshold;
//   - the breaker stays tripped: every later spot settlement for
//     the pair is rejected with the same error until an operator
//     resets it — the trip is latching, not per-attempt, so a rate
//     that briefly spikes and falls back does not silently re-open
//     trading. A rejection while already tripped emits no second
//     audit event: the trip itself is the record.
//   - nothing books on a tripped settlement: no journal rows, no
//     chain links, no version bump — the rejection happens before
//     the first mutation, like every other FX commit-time rejection.
//
// The breaker measures the live mid, not the dealing price and not
// the LG-86 admitted price. Measuring the mid keeps the dealing
// spread out of the trip decision (a wide spread is not a market
// jump), and measuring the live price — rather than the price an
// admitted transfer would settle at — is what makes it a circuit
// breaker rather than a second slippage cap: when the market has
// moved past the breaker, even admitted-price settlements halt
// until an operator reviews and resets. ResetFXRateBreaker is that
// operator action: it re-anchors the pair to the current market mid
// and clears the trip, so trading resumes at the new price level.
//
// Forward settlements are exempt, deliberately: a forward's price
// is contractually locked at booking (LG-110), so its settlement
// never faces the live price — the LG-86 slippage precedent
// (forward slippage is zero by construction, because the customer
// never faced the live price) applies here too. A live-market
// breaker cannot re-price or block a hedge; an operator who wants
// to stop a forward cancels it. Spot split transfers are checked
// per chunk, like any other spot transfer.
//
// Breakers are operational state, like rate overrides (LG-69):
// they do not round-trip through ExportSnapshot/ImportSnapshot.
// After a restore, operators re-set them. They never bump the
// ledger version and book no journal rows.

// FX rate breaker errors.
var (
	// ErrInvalidFXRateBreaker is returned by SetFXRateBreaker for
	// malformed inputs: bad currency codes, a same-currency pair,
	// or a non-positive deviation threshold.
	ErrInvalidFXRateBreaker = errors.New("ledger: invalid FX rate breaker")
	// ErrFXRateBreakerNoRate is returned by SetFXRateBreaker and
	// ResetFXRateBreaker when the pair has no live market rate to
	// anchor to: a breaker anchors to a price, so it cannot exist
	// for a pair the table does not quote (or whose quote has
	// expired).
	ErrFXRateBreakerNoRate = errors.New("ledger: FX rate breaker requires a live market rate for the pair")
	// ErrFXRateBreakerNotConfigured is returned by
	// ResetFXRateBreaker when no breaker was ever set for the pair.
	ErrFXRateBreakerNotConfigured = errors.New("ledger: FX rate breaker not configured for currency pair")
	// ErrFXRateBreakerTripped is returned by a spot FX settlement
	// whose live market mid deviates from the pair's anchor by more
	// than the configured threshold, or by any spot settlement for
	// a pair whose breaker is already tripped. It is a 422-class
	// semantic rejection: the request is well-formed, but the
	// ledger refuses to convert at a price past the breaker.
	ErrFXRateBreakerTripped = errors.New("ledger: FX rate breaker tripped for currency pair")
)

// FXRateBreaker is one directional pair's rate-move circuit breaker:
// the deviation budget, the anchored market mid it is measured
// against, and — once tripped — the live rate that tripped it.
type FXRateBreaker struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	// MaxDeviationBps is the opt-in trip threshold, in basis
	// points: a live mid deviating by more than this from the
	// anchor trips the breaker. Exactly at the threshold does not
	// trip (see overrideDeviationExceeded's strict-greater rule).
	MaxDeviationBps int64 `json:"max_deviation_bps"`
	// AnchorNum/AnchorDen is the market mid at set/reset time, and
	// AnchorVersion its EffectiveVersion.
	AnchorNum     int64  `json:"anchor_num"`
	AnchorDen     int64  `json:"anchor_den"`
	AnchorVersion uint64 `json:"anchor_version"`
	// Tripped reports whether the breaker has tripped and not yet
	// been reset. TrippedAt, TrippedRateNum/TrippedRateDen, and
	// TrippedDeviationBps describe the trip (the live mid that
	// tripped it, and its floored deviation in bps); all are zero
	// while the breaker is armed but untripped.
	Tripped             bool      `json:"tripped"`
	TrippedAt           time.Time `json:"tripped_at,omitempty"`
	TrippedRateNum      int64     `json:"tripped_rate_num,omitempty"`
	TrippedRateDen      int64     `json:"tripped_rate_den,omitempty"`
	TrippedDeviationBps int64     `json:"tripped_deviation_bps,omitempty"`
}

// SetFXRateBreaker installs (or replaces) the rate-move circuit
// breaker for the directional pair (from, to): a spot settlement
// whose live market mid deviates from the anchor by more than
// maxBps basis points trips the breaker and is rejected. The
// anchor is the pair's current market mid (its table rate and
// EffectiveVersion). Replacing a breaker re-anchors it and clears
// any trip — setting is an operator action, like resetting.
//
// Like SetFXRate, this is a structural change: it takes the write
// lock, does not bump the ledger version, and emits an
// fx_rate_breaker_set audit event. It does not round-trip through
// snapshots (see the package comment).
func (l *Ledger) SetFXRateBreaker(from, to string, maxBps int64) (FXRateBreaker, error) {
	f, t, err := normalizeFXRate(from, to, 1, 1)
	if err != nil {
		return FXRateBreaker{}, fmt.Errorf("%w: %v", ErrInvalidFXRateBreaker, err)
	}
	if maxBps <= 0 {
		return FXRateBreaker{}, fmt.Errorf("%w: max deviation bps must be positive, got %d", ErrInvalidFXRateBreaker, maxBps)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rate, ok := l.fxRates[fxPair{from: f, to: t}]
	now := time.Now()
	if !ok || (!rate.ExpiresAt.IsZero() && !now.Before(rate.ExpiresAt)) {
		return FXRateBreaker{}, ErrFXRateBreakerNoRate
	}
	if l.fxBreakers == nil {
		l.fxBreakers = make(map[fxPair]FXRateBreaker)
	}
	b := FXRateBreaker{
		FromCurrency:    f,
		ToCurrency:      t,
		MaxDeviationBps: maxBps,
		AnchorNum:       rate.Num,
		AnchorDen:       rate.Den,
		AnchorVersion:   rate.EffectiveVersion,
	}
	l.fxBreakers[fxPair{from: f, to: t}] = b
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_breaker_set",
		Actor:         "SetFXRateBreaker",
		TraceID:       fmt.Sprintf("%s->%s", f, t),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from_currency":     f,
			"to_currency":       t,
			"max_deviation_bps": maxBps,
			"anchor":            fmt.Sprintf("%d/%d", rate.Num, rate.Den),
			"anchor_version":    rate.EffectiveVersion,
		},
	})
	return b, nil
}

// FXRateBreaker returns the configured breaker for (from, to), or
// false when none is set.
func (l *Ledger) FXRateBreaker(from, to string) (FXRateBreaker, bool) {
	f, err := normalizeCurrency(from)
	if err != nil {
		return FXRateBreaker{}, false
	}
	t, err := normalizeCurrency(to)
	if err != nil {
		return FXRateBreaker{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	b, ok := l.fxBreakers[fxPair{from: f, to: t}]
	return b, ok
}

// FXRateBreakers lists every configured breaker, sorted by
// (from, to), for operator inspection.
func (l *Ledger) FXRateBreakers() []FXRateBreaker {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]FXRateBreaker, 0, len(l.fxBreakers))
	for _, b := range l.fxBreakers {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromCurrency != out[j].FromCurrency {
			return out[i].FromCurrency < out[j].FromCurrency
		}
		return out[i].ToCurrency < out[j].ToCurrency
	})
	return out
}

// ResetFXRateBreaker re-anchors the pair's breaker to the current
// market mid and clears any trip, so spot settlements resume at the
// new price level. The threshold is unchanged. It fails when no
// breaker is configured for the pair, or when the pair has no live
// market rate to re-anchor to. Like SetFXRateBreaker, it does not
// bump the ledger version.
func (l *Ledger) ResetFXRateBreaker(from, to string) (FXRateBreaker, error) {
	f, err := normalizeCurrency(from)
	if err != nil {
		return FXRateBreaker{}, fmt.Errorf("%w: %v", ErrInvalidFXRateBreaker, err)
	}
	t, err := normalizeCurrency(to)
	if err != nil {
		return FXRateBreaker{}, fmt.Errorf("%w: %v", ErrInvalidFXRateBreaker, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.fxBreakers[fxPair{from: f, to: t}]
	if !ok {
		return FXRateBreaker{}, ErrFXRateBreakerNotConfigured
	}
	rate, ok := l.fxRates[fxPair{from: f, to: t}]
	now := time.Now()
	if !ok || (!rate.ExpiresAt.IsZero() && !now.Before(rate.ExpiresAt)) {
		return FXRateBreaker{}, ErrFXRateBreakerNoRate
	}
	b.AnchorNum, b.AnchorDen, b.AnchorVersion = rate.Num, rate.Den, rate.EffectiveVersion
	b.Tripped = false
	b.TrippedAt = time.Time{}
	b.TrippedRateNum, b.TrippedRateDen, b.TrippedDeviationBps = 0, 0, 0
	l.fxBreakers[fxPair{from: f, to: t}] = b
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_breaker_reset",
		Actor:         "ResetFXRateBreaker",
		TraceID:       fmt.Sprintf("%s->%s", f, t),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from_currency":     f,
			"to_currency":       t,
			"max_deviation_bps": b.MaxDeviationBps,
			"anchor":            fmt.Sprintf("%d/%d", rate.Num, rate.Den),
			"anchor_version":    rate.EffectiveVersion,
		},
	})
	return b, nil
}

// checkFXRateBreakerLocked enforces the pair's breaker against the
// live market mid, if one is configured. Callers must hold the
// write lock; the check runs before the first mutation, so a
// tripped settlement records nothing except — on the trip itself
// — the fx_rate_breaker_tripped audit event. A pair with no
// breaker, and a forward-locked settlement (see below), always
// pass.
//
// The live mid is passed in, not the dealing price: the spread is
// not a market jump. Forward-locked settlements are not checked
// here at all — the caller only invokes this helper on the live
// path, never for a forwardLocked transfer — because a forward
// settles at its contractually locked price, never at the live
// price (LG-110 / LG-86 precedent).
func (l *Ledger) checkFXRateBreakerLocked(pair fxPair, liveMidNum, liveMidDen int64, now time.Time) error {
	b, ok := l.fxBreakers[pair]
	if !ok {
		return nil
	}
	if b.Tripped {
		return ErrFXRateBreakerTripped
	}
	deviation := fxSlippageBps(liveMidNum, liveMidDen, b.AnchorNum, b.AnchorDen)
	if deviation <= b.MaxDeviationBps {
		return nil
	}
	b.Tripped = true
	b.TrippedAt = now
	b.TrippedRateNum, b.TrippedRateDen = liveMidNum, liveMidDen
	b.TrippedDeviationBps = deviation
	if l.fxBreakers == nil {
		l.fxBreakers = make(map[fxPair]FXRateBreaker)
	}
	l.fxBreakers[pair] = b
	l.emitAudit(AuditEvent{
		Op:            "fx_rate_breaker_tripped",
		Actor:         "PostTransfer",
		TraceID:       fmt.Sprintf("%s->%s", pair.from, pair.to),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from_currency":     pair.from,
			"to_currency":       pair.to,
			"max_deviation_bps": b.MaxDeviationBps,
			"anchor":            fmt.Sprintf("%d/%d", b.AnchorNum, b.AnchorDen),
			"anchor_version":    b.AnchorVersion,
			"live_rate":         fmt.Sprintf("%d/%d", liveMidNum, liveMidDen),
			"deviation_bps":     deviation,
			"tripped_at":        now.UTC().Format(time.RFC3339),
		},
	})
	return ErrFXRateBreakerTripped
}
