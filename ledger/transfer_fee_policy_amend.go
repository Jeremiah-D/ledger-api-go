package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Transfer fee policy amendment (LG-241).
//
// WithTransferFeeSchedule / WithTransferFeePolicy (LG-25) install the
// ledger-wide default fee policy at construction; this module is the
// orthogonal runtime path: an operator revises the default schedule
// (or flat rate) in place, instead of rebuilding the ledger and
// replaying its journal. Revising in place matters because a rebuild
// is a new ledger — a gap in which no policy (or the old policy) is
// live, plus a fresh identity that breaks the audit story — while an
// amendment keeps the same ledger, the same journal, and the same
// audit trail from old schedule to new.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only future transfers are repriced. PostTransfer resolves the
//     fee at posting time from the live feeTiers / feeRevenueAccount
//     fields, so a transfer posted after the amendment is charged
//     under the new schedule (and discloses the new tier), while a
//     transfer already posted keeps its journaled fee leg exactly as
//     booked — its receipt is a historical record, never rewritten.
//     A replayed idempotency key likewise keeps returning the
//     original receipt, not a repriced one.
//   - An explicit FeeCents or SkipFee still wins over the policy
//     after the amendment: resolveFeeLocked checks the explicit
//     fields before it ever reads the live schedule, so the
//     amendment changes only the default, never a caller's override.
//   - Validation is strict, like ParseFeeSchedule rather than the
//     construction options: the construction options normalize
//     (negative rates become 0, rates above 10000 bps clamp to
//     10000, an empty table or revenue account disables the
//     policy), but a runtime amendment must never silently
//     reprice or silently disable. An empty tier list, an empty
//     revenue account, a negative minimum, a negative rate, a
//     rate above 10000 bps, a first tier not starting at 0, or
//     tier minimums that are not strictly increasing all fail
//     with ErrInvalidTransferFeeSchedule, and the previous
//     schedule is left exactly as it was — the operator fixes the
//     table and retries, the ledger never runs on a half-installed
//     schedule. (A fee-free policy is a 0 bps tier, not an empty
//     table: that keeps the tier disclosure honest.)
//   - The installed table is a defensive copy: mutating the
//     caller's slice after the call cannot reprice the ledger.
//   - One disclosure caveat, inherited from the existing replay
//     design rather than introduced here: a replayed transfer's
//     tier disclosure re-derives under the live schedule
//     (rediscloseFeeTierLocked), so after an amendment a replay
//     of a pre-amendment transfer may disclose tier -1 even
//     though its journaled fee leg is byte-for-byte unchanged —
//     the same treatment replays already give FX disclosure when
//     a rate table changes. The fee leg itself (entries,
//     FeeCents, balances) is never rewritten; only the
//     re-derived disclosure can fall back.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, but it lands one transfer_fee_schedule_amended
//     audit event carrying the previous and new schedules and
//     revenue accounts. The amended schedule rides the config
//     line (fee_tiers) through snapshots exactly like a
//     construction-time schedule — no snapshot change is needed.
//
// Orthogonality: WithTransferFeeSchedule / WithTransferFeePolicy own
// the construction-time policy, ParseFeeSchedule owns the
// environment-variable form, PostTransfer owns per-transfer fee
// resolution (including the explicit-fee and SkipFee overrides),
// and this module owns only the runtime revision of the default.

// Transfer fee policy amendment errors.
var (
	// ErrInvalidTransferFeeSchedule is returned by
	// SetTransferFeeSchedule / SetTransferFeePolicy when the
	// proposed schedule cannot be installed: empty tiers, empty
	// revenue account, negative minimum or rate, rate above
	// 10000 bps, first tier not starting at 0, or tier
	// minimums not strictly increasing.
	ErrInvalidTransferFeeSchedule = errors.New("ledger: invalid transfer fee schedule")
)

// validateTransferFeeSchedule checks a proposed schedule against the
// strict runtime rules and returns a defensive copy of it, or an
// error that leaves the caller's ledger untouched.
func validateTransferFeeSchedule(tiers []FeeTier, revenueAccount AccountID) ([]FeeTier, error) {
	if len(tiers) == 0 {
		return nil, fmt.Errorf("%w: tier list must not be empty", ErrInvalidTransferFeeSchedule)
	}
	if revenueAccount == "" {
		return nil, fmt.Errorf("%w: revenue account must not be empty", ErrInvalidTransferFeeSchedule)
	}
	out := make([]FeeTier, len(tiers))
	for i, t := range tiers {
		if t.MinAmountCents < 0 {
			return nil, fmt.Errorf("%w: tier %d minimum must not be negative", ErrInvalidTransferFeeSchedule, i)
		}
		if i > 0 && t.MinAmountCents <= out[i-1].MinAmountCents {
			return nil, fmt.Errorf("%w: tier minimums must be strictly increasing", ErrInvalidTransferFeeSchedule)
		}
		if t.RateBps < 0 {
			return nil, fmt.Errorf("%w: tier %d rate must not be negative", ErrInvalidTransferFeeSchedule, i)
		}
		if t.RateBps > 10000 {
			return nil, fmt.Errorf("%w: tier %d rate must not exceed 10000 bps", ErrInvalidTransferFeeSchedule, i)
		}
		out[i] = t
	}
	if out[0].MinAmountCents != 0 {
		return nil, fmt.Errorf("%w: first tier must start at 0", ErrInvalidTransferFeeSchedule)
	}
	return out, nil
}

// SetTransferFeeSchedule replaces the ledger-wide default transfer
// fee schedule in place. Only transfers posted after the call are
// charged under the new schedule; already-posted fee legs are
// untouched. An invalid schedule fails with
// ErrInvalidTransferFeeSchedule and the previous schedule is
// unchanged. The amendment is audited as
// transfer_fee_schedule_amended.
func (l *Ledger) SetTransferFeeSchedule(tiers []FeeTier, revenueAccount AccountID) error {
	return l.SetTransferFeeScheduleCtx(context.Background(), tiers, revenueAccount)
}

// SetTransferFeeScheduleCtx is SetTransferFeeSchedule with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetTransferFeeScheduleCtx(ctx context.Context, tiers []FeeTier, revenueAccount AccountID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setTransferFeeScheduleLocked(ctx, tiers, revenueAccount)
}

// setTransferFeeScheduleLocked validates and installs a schedule.
// Callers must hold l.mu. Validation runs before any mutation, so a
// rejected schedule leaves the previous policy exactly as it was.
func (l *Ledger) setTransferFeeScheduleLocked(ctx context.Context, tiers []FeeTier, revenueAccount AccountID) error {
	validated, err := validateTransferFeeSchedule(tiers, revenueAccount)
	if err != nil {
		return err
	}
	previousTiers := append([]FeeTier(nil), l.feeTiers...)
	previousRevenue := l.feeRevenueAccount
	l.feeTiers = validated
	l.feeRevenueAccount = revenueAccount
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_fee_schedule_amended",
		Actor:         "SetTransferFeeSchedule",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{revenueAccount},
		Details: map[string]any{
			"previous_tiers":           previousTiers,
			"tiers":                    append([]FeeTier(nil), validated...),
			"previous_revenue_account": string(previousRevenue),
			"revenue_account":          string(revenueAccount),
		},
	})
	return nil
}

// SetTransferFeePolicy replaces the ledger-wide default transfer fee
// policy with a single flat rate in place: a flat rate is a schedule
// with one tier starting at 0, so it shares the schedule amendment's
// strict validation (a negative rate or a rate above 10000 bps is
// rejected, not normalized) and its future-only effect. A 0 rate is
// legal and means fee-free with the tier still disclosed.
func (l *Ledger) SetTransferFeePolicy(rateBps int64, revenueAccount AccountID) error {
	return l.SetTransferFeePolicyCtx(context.Background(), rateBps, revenueAccount)
}

// SetTransferFeePolicyCtx is SetTransferFeePolicy with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetTransferFeePolicyCtx(ctx context.Context, rateBps int64, revenueAccount AccountID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setTransferFeeScheduleLocked(ctx, []FeeTier{{MinAmountCents: 0, RateBps: rateBps}}, revenueAccount)
}
