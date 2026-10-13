package ledger

import (
	"context"
	"fmt"
	"math/big"
	"time"
)

// Market FX rate provider adapter (LG-74).
//
// The rate table (see fx.go) is the market price: by default it is a
// static table the operator maintains by hand (SetFXRate /
// SetFXRateRat / LEDGER_FX_RATES). This file adds the opt-in live leg:
// an FXRateProvider the *caller* implements (a market-data feed client,
// a treasury pricing service, an in-process stub in tests) that the
// FX-rate watch (see fx_watch.go, LG-68) polls on its interval and
// installs through the SetFXRateRat write path.
//
// The convention mirrors realtime-event-bus's EB-51 BridgeTransport:
// the interface is defined by the library, implemented by the caller,
// and nil disables the feature completely — with a nil provider the
// watcher's tick is byte-for-byte the static-table behavior it was
// before, no extra audit events, no extra counters.
//
// Refresh semantics, per tick:
//
//   - The provider is asked once: FetchRates(ctx). A returned error is
//     never silent — it emits one fx_rate_provider_failed audit event
//     and bumps the failure counter — and it never kills the worker:
//     the next tick runs as scheduled.
//   - Every returned quote is validated like SetFXRateRat (positive
//     exact rate, well-formed directional pair, non-negative TTL).
//     Quotes that fail validation are ignored; the provider contract
//     says a quote is a well-formed (from, to, rate) triple.
//   - A pair under an active manual override (see fx_override.go,
//     LG-69) is never re-priced by the feed: the human price wins.
//   - Static quotes never churn: a quote whose price equals the table
//     and that carries no expiry (and the table has none) skips the
//     write entirely — no snapshot bump, no fx_rate_change noise.
//   - A quote that changes the price is written and audited as
//     fx_rate_moved (from/to, old and new rate, provider, TTL).
//     A quote that only re-stamps expiry is written without the moved
//     event: the price did not move.

// MarketRate is one market quote returned by an FXRateProvider:
// converting From to To multiplies by Rate, an exact *big.Rat (no
// float64 ever touches money — see SetFXRateRat). TTL is the quote's
// lifetime: a positive TTL stamps the installed rate with ExpiresAt =
// now+TTL (transfers at or after expiry are rejected with
// ErrFXRateExpired, exactly like operator-installed TTL rates); a
// non-positive TTL means the rate never expires.
type MarketRate struct {
	From string
	To   string
	Rate *big.Rat
	TTL  time.Duration
}

// FXRateProvider is the caller-implemented adapter between the ledger
// and an external market-data source. Implementations must be safe for
// concurrent use and must respect ctx cancellation: the watch calls
// FetchRates synchronously on its tick, so a provider that ignores a
// cancelled context stalls the expiry scan. A returned error is audited
// as fx_rate_provider_failed and the worker continues on the next tick;
// quotes that fail validation are ignored.
type FXRateProvider interface {
	FetchRates(ctx context.Context) ([]MarketRate, error)
}

// WithFXRateProvider installs the opt-in market FX rate provider the
// FX-rate watch polls on its interval (see StartFXRateWatch). A nil
// provider — the default — disables provider refreshes entirely: the
// watcher keeps its static-table behavior with zero change.
func WithFXRateProvider(p FXRateProvider) Option {
	return func(l *Ledger) {
		l.fxRateProvider = p
	}
}

// refreshFXRatesFromProvider polls the provider once and installs the
// returned quotes through the SetFXRateRat write path. A provider error
// is audited (fx_rate_provider_failed) and counted; the worker is never
// stopped by it. Called from FXRateWatch.tick; a no-op when the ledger
// has no provider.
func (l *Ledger) refreshFXRatesFromProvider(ctx context.Context, now time.Time) {
	p := l.fxRateProvider
	if p == nil {
		return
	}
	rates, err := p.FetchRates(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.fxRateProviderFails++
		l.emitAudit(AuditEvent{
			Op:            "fx_rate_provider_failed",
			Actor:         "FXRateWatch",
			TraceID:       "fx-rate-provider",
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"provider": fmt.Sprintf("%T", p),
				"error":    err.Error(),
			},
		})
		return
	}
	l.applyMarketRatesLocked(now, fmt.Sprintf("%T", p), rates)
}

// applyMarketRatesLocked installs one tick of market quotes. Every
// write goes through setFXRateRatLocked — the same locked body
// SetFXRateRat uses — so provider-installed rates snapshot, audit
// (fx_rate_change), and expire exactly like operator-installed ones.
// Callers must hold the write lock.
func (l *Ledger) applyMarketRatesLocked(now time.Time, provider string, rates []MarketRate) {
	for _, mr := range rates {
		if mr.Rate == nil || mr.Rate.Sign() <= 0 {
			continue
		}
		num, den := mr.Rate.Num(), mr.Rate.Denom()
		if !num.IsInt64() || !den.IsInt64() {
			continue
		}
		if mr.TTL < 0 {
			continue
		}
		f, t, err := normalizeFXRate(mr.From, mr.To, num.Int64(), den.Int64())
		if err != nil {
			continue
		}
		p := fxPair{from: f, to: t}
		// Maker-checker override wins over the feed: a pair under an
		// active manual override keeps the human price; the provider
		// never writes it. (The override itself is audited at
		// proposal/approval; the skip needs no per-tick event.)
		if _, overridden := l.activeRateOverrideLocked(p, now); overridden {
			continue
		}
		old, existed := l.fxRates[p]
		// No churn on static quotes: the same price with no expiry on
		// either side means the table already holds exactly this
		// state — skip the write so snapshots and the audit log stay
		// quiet. Anything else (a moved price, a new pair, or an
		// expiry re-stamp) is a real table change and goes through.
		if existed && old.Num == num.Int64() && old.Den == den.Int64() &&
			old.ExpiresAt.IsZero() && mr.TTL <= 0 {
			continue
		}
		l.setFXRateRatLocked(f, t, num.Int64(), den.Int64(), mr.TTL, now)
		l.fxRateRefreshes++
		if !existed || old.Num != num.Int64() || old.Den != den.Int64() {
			oldRate := "none"
			if existed {
				oldRate = fmt.Sprintf("%d/%d", old.Num, old.Den)
			}
			l.fxRateMoves++
			l.emitAudit(AuditEvent{
				Op:            "fx_rate_moved",
				Actor:         "FXRateWatch",
				TraceID:       fmt.Sprintf("%s->%s", f, t),
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Details: map[string]any{
					"from_currency": f,
					"to_currency":   t,
					"old_rate":      oldRate,
					"new_rate":      fmt.Sprintf("%d/%d", num.Int64(), den.Int64()),
					"provider":      provider,
					"ttl":           mr.TTL.String(),
				},
			})
		}
	}
}

// FXRateRefreshCount reports how many provider quotes have been
// installed since process start. The HTTP layer syncs this into
// ledger_fx_rate_refreshes_total on every /metrics scrape. In-memory
// only — it does not round-trip through snapshots.
func (l *Ledger) FXRateRefreshCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateRefreshes
}

// FXRateMovedCount reports how many provider refreshes changed a pair's
// price (fx_rate_moved audit events) since process start. The HTTP
// layer syncs this into ledger_fx_rate_moved_total on every /metrics
// scrape. In-memory only — it does not round-trip through snapshots.
func (l *Ledger) FXRateMovedCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateMoves
}

// FXRateProviderFailCount reports how many provider fetches have failed
// (fx_rate_provider_failed audit events) since process start. The HTTP
// layer syncs this into ledger_fx_rate_provider_failed_total on every
// /metrics scrape. In-memory only — it does not round-trip through
// snapshots.
func (l *Ledger) FXRateProviderFailCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateProviderFails
}
