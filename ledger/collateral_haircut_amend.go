package ledger

import (
	"context"
	"strings"
)

// Collateral haircut amendment (LG-275).
//
// PledgeCollateral (LG-173) fixes a position's haircut at pledge
// time, and a repeat pledge marks the whole position to market at a
// new unit value and haircut — but only as a side effect of adding
// quantity, and only by repricing the unit value at the same time.
// There was no way to revise the haircut alone: an operator who
// wanted a riskier asset discounted harder (or a stabilized asset
// discounted less) had to release and re-pledge, a new position
// with a fresh PledgedAt that breaks the pledge history, plus a gap
// in which the account has no coverage at all. This module is the
// orthogonal in-place haircut path: an operator revises a live
// pledge's risk discount without touching its identity, its
// quantity, its unit value, or its history.
//
// Semantics, deliberately fail-closed throughout, along the
// PledgeCollateral and RevalueCollateral gates:
//
//   - Only a pledged position may be amended. An empty account fails
//     with ErrEmptyCollateralAccount; an empty asset ID with
//     ErrEmptyCollateralAsset; an (account, asset) pair with no
//     pledged position fails with ErrCollateralNotFound, the same
//     lookup miss RevalueCollateral reports.
//   - The new haircut follows the PledgeCollateral gate exactly:
//     0..10000 basis points, else ErrInvalidCollateralHaircut — a
//     haircut above 100% would make the asset count negative, a
//     configuration bug, not a clampable value. A failed amendment
//     leaves the original haircut exactly as it was.
//   - The amendment touches the haircut and nothing else: the
//     quantity, the unit value, and the PledgedAt instant are
//     unchanged. There is no clawback question: the effective
//     collateral value reads the live HaircutBps on every coverage
//     evaluation (see collateralEffectiveLocked), so the new
//     haircut takes effect from the very next coverage check with
//     no gate-path change — a raise immediately tightens coverage,
//     and a lowering immediately loosens it, while a draw that
//     already cleared under the old haircut is never revisited.
//   - Amending to the current haircut is a no-op: no state change,
//     no audit event, no margin scan — the position is already
//     exactly what the operator asked for.
//   - Like the other collateral changes, the amendment is
//     structural config: it bumps neither the ledger version nor
//     the audit chain, and no outbox event fires (RevalueCollateral
//     follows the same convention). The amendment lands one audit
//     event (collateral_haircut_amended, with the asset, the
//     previous and new haircuts) so the "who repriced the risk,
//     and from what to what" trail is tamper-evident. Collateral
//     pledges are snapshot records, so the amended haircut rides
//     the pledge record through snapshots exactly like the
//     original — no snapshot change is needed.
//   - Raising the haircut can breach coverage immediately, so the
//     amendment runs the margin scan and returns any margin calls
//     it triggers, following the RevalueCollateral /
//     SetCollateralRequirement precedent.
//
// Orthogonality: PledgeCollateral owns the initial haircut (and the
// position itself), ReleaseCollateral owns quantity, RevalueCollateral
// owns the unit value, SetCollateralRequirement owns the coverage
// ratio, and this module owns only the haircut revision.

// AmendCollateralHaircut revises a pledged position's haircut to
// newHaircutBps in place. The new haircut must lie in 0..10000 basis
// points; a failed amendment leaves the pledge exactly as it was.
// Amending to the current haircut is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as collateral_haircut_amended, and it returns any margin
// calls the new haircut triggers.
func (l *Ledger) AmendCollateralHaircut(account AccountID, assetID string, newHaircutBps int64) (CollateralPledge, []MarginCall, error) {
	return l.AmendCollateralHaircutCtx(context.Background(), account, assetID, newHaircutBps)
}

// AmendCollateralHaircutCtx is AmendCollateralHaircut with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendCollateralHaircutCtx(ctx context.Context, account AccountID, assetID string, newHaircutBps int64) (CollateralPledge, []MarginCall, error) {
	if account == "" {
		return CollateralPledge{}, nil, ErrEmptyCollateralAccount
	}
	if strings.TrimSpace(assetID) == "" {
		return CollateralPledge{}, nil, ErrEmptyCollateralAsset
	}
	if newHaircutBps < 0 || newHaircutBps > 10000 {
		return CollateralPledge{}, nil, ErrInvalidCollateralHaircut
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := collateralKey{account: account, asset: assetID}
	p, exists := l.collateralPledges[k]
	if !exists {
		return CollateralPledge{}, nil, ErrCollateralNotFound
	}
	if newHaircutBps == p.HaircutBps {
		return p, nil, nil
	}
	previous := p.HaircutBps
	p.HaircutBps = newHaircutBps
	l.collateralPledges[k] = p
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "collateral_haircut_amended",
		Actor:         "AmendCollateralHaircut",
		TraceID:       string(account) + "/" + assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"asset_id":             assetID,
			"previous_haircut_bps": previous,
			"haircut_bps":          newHaircutBps,
		},
	})
	return p, l.checkMarginCallsLocked(account), nil
}
