package ledger

import (
	"context"
)

// FX rate provider amendment (LG-289).
//
// WithFXRateProvider (LG-74) installs the market FX rate provider at
// construction; this module is the orthogonal runtime path: an operator
// swaps the rate source in place, instead of rebuilding the ledger and
// replaying its journal. Swapping in place matters because a rebuild
// is a new ledger — a gap in which the old source (or no source)
// governs pricing, plus a fresh identity that breaks the audit story
// — while an amendment keeps the same ledger, the same journal, and
// the same audit trail from old source to new.
//
// The swap fixes a real staleness bug in the watch (LG-68):
// StartFXRateWatch used to capture the provider at build time
// (`provider: l.fxRateProvider`) and the tick only ever read the
// captured value, so a provider installed after the watch started —
// or replaced while it ran — was silently ignored by every later
// tick. The tick now reads the live field on every tick, exactly
// like refreshFXRatesFromProvider already did, and the watch's own
// provider field retires with this revision.
//
// Semantics, deliberately minimal throughout:
//
//   - The new provider takes effect immediately for every later
//     refresh, because both refresh paths read the live field, never
//     a cached copy: the watch tick refreshes from l.fxRateProvider
//     at tick time, and a direct refreshFXRatesFromProvider call
//     reads the same live field. No watch restart, sweep, or replay
//     path needed a code change — the revision is clean exactly
//     because the direct-refresh reader was already live and the
//     tick is now fixed to match it.
//   - The already-refreshed rate table is untouched: the swap
//     re-refreshes nothing and changes no stored rate row, so a
//     rate installed by the old source keeps its price and expiry
//     until the new source's next tick moves it (or the expiry scan
//     judges it by its table row, as before).
//   - A nil install clears the provider, exactly as at construction
//     (LG-74): the watcher falls back to static-table behavior with
//     zero change — no extra audit events from the tick, no extra
//     counters — and the next tick is byte-for-byte the behavior
//     before any provider existed. Installing from nil is the
//     mirror image: the next tick starts polling the new source.
//   - Reinstalling the same instance is a no-op: no state change,
//     no audit event. The comparison is by instance identity, so
//     swapping in a different adapter over the same feed is still a
//     real revision and is audited.
//   - The amendment is structural, like SetIdempotencyTTL: it moves
//     no money, so it bumps neither the ledger version nor the
//     audit chain, but it lands one fx_rate_provider_replaced audit
//     event carrying the previous and new provider *presence* (a
//     boolean each — never an instance pointer or implementation
//     detail). The provider is a caller-held adapter, not ledger
//     state: it rides no snapshot row, so after a snapshot restore
//     the operator must re-set the provider (per the LG-287
//     snapshot guardrails) — no snapshot change is needed.
//
// Orthogonality: WithFXRateProvider owns the construction-time
// provider, refreshFXRatesFromProvider owns the per-tick fetch and
// install, SetFXRateBreaker owns the per-pair move circuit breaker,
// and this module owns only the runtime revision of the provider
// itself. There is no HTTP endpoint for the swap (per LG-241/242):
// the provider is a caller-held adapter, installed in-process.

// SetFXRateProvider replaces the market FX rate provider in place.
// The new provider governs every later watch tick and direct
// refresh; a nil provider clears the source and the watcher falls
// back to static-table behavior. Already-refreshed rates are
// untouched. Reinstalling the same instance is a no-op. The
// revision is audited as fx_rate_provider_replaced.
func (l *Ledger) SetFXRateProvider(p FXRateProvider) {
	l.SetFXRateProviderCtx(context.Background(), p)
}

// SetFXRateProviderCtx is SetFXRateProvider with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetFXRateProviderCtx(ctx context.Context, p FXRateProvider) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.fxRateProvider
	if previous == p {
		return
	}
	l.fxRateProvider = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_rate_provider_replaced",
		Actor:         "SetFXRateProvider",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_provider": previous != nil,
			"provider":          p != nil,
		},
	})
}
