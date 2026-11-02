package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FX rate override guardrail amendment (LG-287).
//
// WithRateOverrideTTL / WithRateOverrideMaxDeviationBps (LG-69)
// install the two override guardrails at construction; this module
// is the orthogonal runtime path: an operator revises the default
// override lifetime and the deviation cap in place, instead of
// rebuilding the ledger and replaying its journal. Revising in place
// matters because a rebuild is a new ledger — a gap in which the
// old guardrails (or none) govern override proposals, plus a fresh
// identity that breaks the audit story — while an amendment keeps
// the same ledger, the same journal, and the same audit trail from
// old guardrail to new.
//
// Semantics, deliberately fail-closed throughout (along LG-241):
//
//   - The TTL revision governs only later proposals that pass a
//     non-positive ttl: SetRateOverride stamps ttl <= 0 with the
//     live l.rateOverrideTTL at proposal time, and
//     ApproveRateOverride falls back to the live default only when
//     a stored TTL fails to parse — so an already-pending or
//     already-active override keeps its stored TTL and ExpiresAt
//     exactly as approved. Lengthening the default never extends
//     a live override; shortening it never cuts one short.
//   - The cap revision governs new proposals immediately and
//     pending approvals immediately: both read the live
//     l.rateOverrideMaxDeviationBps at call time. Tightening the
//     cap after a proposal refuses that pending override's
//     approval with ErrRateOverrideDeviationExceeded — the
//     override stays pending, so the proposer can re-propose at
//     a fresh price — while an already-active override is never
//     repriced and runs out on its stored ExpiresAt.
//   - Validation is strict, like LG-241 rather than the
//     construction options: the construction options normalize
//     (a non-positive TTL keeps the one-hour default, a negative
//     cap panics), but a runtime amendment must never silently
//     re-install the default or silently allow unbounded
//     re-pricing. A TTL of zero or less fails with
//     ErrInvalidRateOverrideTTL and a negative cap fails with
//     ErrInvalidRateOverrideMaxDeviationBps, and the previous
//     guardrail is left exactly as it was. The cap has no upper
//     bound: construction rejects only negatives, and the runtime
//     path invents no 10000 bps ceiling.
//   - Revising to the value already in force is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetTransferFeeSchedule:
//     it moves no money, so it bumps neither the ledger version
//     nor the audit chain, but it lands one
//     fx_rate_override_ttl_amended or
//     fx_rate_override_max_deviation_bps_amended audit event
//     carrying the previous and new values. The guardrails do
//     NOT ride snapshots — snapshot.go carries no rateOverride
//     line, and the overrides themselves do not round-trip
//     (LG-69) — so a restored ledger comes back on the
//     one-hour / 1000 bps defaults and operators must re-set
//     any amended guardrail after a restore.
//
// Orthogonality: WithRateOverrideTTL /
// WithRateOverrideMaxDeviationBps own the construction-time
// guardrails, SetRateOverride / ApproveRateOverride own the
// per-override lifecycle, and this module owns only the runtime
// revision of the two guardrails themselves.

// FX rate override guardrail amendment errors.
var (
	// ErrInvalidRateOverrideTTL is returned by
	// SetRateOverrideTTL when the proposed default override
	// lifetime is not positive: a zero or negative TTL would
	// silently re-install the one-hour default, so the
	// revision is refused and the previous TTL is unchanged.
	ErrInvalidRateOverrideTTL = errors.New("ledger: invalid FX rate override TTL")
	// ErrInvalidRateOverrideMaxDeviationBps is returned by
	// SetRateOverrideMaxDeviationBps when the proposed cap is
	// negative: a negative cap would silently allow unbounded
	// re-pricing, so the revision is refused and the previous
	// cap is unchanged. There is no upper bound.
	ErrInvalidRateOverrideMaxDeviationBps = errors.New("ledger: invalid FX rate override max deviation")
)

// SetRateOverrideTTL replaces the default lifetime applied to a
// manual FX rate override proposed with a non-positive ttl, in
// place. Only proposals made after the call stamp the new TTL;
// already-pending and already-active overrides keep their stored
// TTL and ExpiresAt. A non-positive TTL fails with
// ErrInvalidRateOverrideTTL and leaves the previous TTL
// unchanged. Revising to the current value is a no-op. The
// amendment is audited as fx_rate_override_ttl_amended.
func (l *Ledger) SetRateOverrideTTL(ttl time.Duration) error {
	return l.SetRateOverrideTTLCtx(context.Background(), ttl)
}

// SetRateOverrideTTLCtx is SetRateOverrideTTL with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetRateOverrideTTLCtx(ctx context.Context, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ttl <= 0 {
		return fmt.Errorf("%w: ttl must be positive, got %s", ErrInvalidRateOverrideTTL, ttl)
	}
	previous := l.rateOverrideTTL
	if previous == ttl {
		return nil
	}
	l.rateOverrideTTL = ttl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_rate_override_ttl_amended",
		Actor:         "SetRateOverrideTTL",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ttl": previous.String(),
			"ttl":          ttl.String(),
		},
	})
	return nil
}

// SetRateOverrideMaxDeviationBps replaces the cap on
// |override - market| / market in basis points, in place. New
// proposals are checked against the new cap immediately, and
// pending overrides are re-checked against it at approval — a
// tightened cap refuses a pending approval with
// ErrRateOverrideDeviationExceeded while the override stays
// pending; already-active overrides are never repriced. A
// negative cap fails with
// ErrInvalidRateOverrideMaxDeviationBps and leaves the previous
// cap unchanged. There is no upper bound. Revising to the
// current value is a no-op. The amendment is audited as
// fx_rate_override_max_deviation_bps_amended.
func (l *Ledger) SetRateOverrideMaxDeviationBps(bps int64) error {
	return l.SetRateOverrideMaxDeviationBpsCtx(context.Background(), bps)
}

// SetRateOverrideMaxDeviationBpsCtx is
// SetRateOverrideMaxDeviationBps with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetRateOverrideMaxDeviationBpsCtx(ctx context.Context, bps int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bps < 0 {
		return fmt.Errorf("%w: max deviation must not be negative, got %d bps", ErrInvalidRateOverrideMaxDeviationBps, bps)
	}
	previous := l.rateOverrideMaxDeviationBps
	if previous == bps {
		return nil
	}
	l.rateOverrideMaxDeviationBps = bps
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_rate_override_max_deviation_bps_amended",
		Actor:         "SetRateOverrideMaxDeviationBps",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_deviation_bps": previous,
			"max_deviation_bps":          bps,
		},
	})
	return nil
}
