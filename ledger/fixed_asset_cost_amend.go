// Fixed-asset cost amendment (LG-341).
//
// RegisterFixedAsset (LG-141) fixes an asset's capitalized cost at
// registration; Depreciate books one month at a time against the
// depreciable base (cost minus salvage), reading the live CostCents
// every month. This module is the revision path: an operator revises
// a live asset's cost instead of retiring the asset and registering
// a replacement. Amending in place matters because a replacement is
// a new asset — a fresh ID that breaks the "depr/<assetID>/<YYYY-MM>"
// idempotency lineage, so already-booked months could never be
// re-run or netted against the same asset — while an amendment keeps
// the same asset, the same booked months, and one audit trail from
// registration to full depreciation.
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrFixedAssetNotFound. The new cost
//     reuses the registration gates (see validateFixedAsset): it
//     must be positive, otherwise ErrInvalidAssetCost, and the
//     salvage must stay below it, otherwise ErrInvalidSalvage —
//     and the original cost is untouched on either rejection.
//   - A cost revision is only possible before the first
//     depreciation month is posted. Once any month has been booked,
//     the amendment fails with ErrFixedAssetDepreciationPosted:
//     the booked months were computed on the old cost, so changing
//     the cost would rewrite the economics of history — the
//     remaining-base cap would converge to the new salvage, but the
//     already-booked months would forever sit on a base the asset
//     no longer claims. This follows the installment precedent
//     (AmendInstallmentPrincipal: PostedPeriods must be 0): the
//     gate counts posted months, not the net accumulated total, so
//     a month that was booked and later reversed still blocks the
//     revision — the month was posted, and re-running it after a
//     cost change would book the same month at a different base.
//     The gate runs before the no-op check, following the
//     installment precedent (a terminal plan rejects even a
//     same-value amendment) — the fail-closed state gate wins over
//     the redundant call.
//   - The amendment touches CostCents and nothing else: the
//     salvage, useful life, method, currency, and already-booked
//     months are unchanged. Booked months are deterministic journal
//     rows ("depr/<assetID>/<YYYY-MM>") and are never recomputed —
//     changing the cost does not restate history, it only changes
//     what future months charge. (In practice the posted gate means
//     there are no booked months yet when a revision lands; the
//     rule still holds for the reversed-month case, where the
//     reversal rows stay exactly as posted.)
//   - The new cost governs from the next Depreciate call:
//     monthlyDepreciation reads the live CostCents every month, so
//     no sweep change is needed. Straight-line charges
//     (new cost - salvage) / life per month; double-declining
//     charges bookValue*2/life against the new book value. Both
//     stay under the remaining-base cap with the dust true-up, so
//     the schedule converges exactly to salvage.
//   - Amending to the current cost is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Like registration, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the asset was already reported when registered.
//     The amendment lands one audit event
//     (fixed_asset_cost_amended, with the asset ID and the previous
//     and new costs) so the "who moved the cost, and from what to
//     what" trail is tamper-evident. The asset register is
//     in-memory structural config (see depreciation.go), so an
//     amendment, like the registration itself, does not round-trip
//     a snapshot restore — re-register after restore. This module
//     has no Ctx variant: the fixed-asset module has none (see the
//     LG-264 precedent for modules without one). The backlog item
//     for LG-341 mentions a Ctx variant in passing; it is not
//     added, following the documented precedent of the three
//     sibling amendments (LG-268/269/270), which all decline one
//     for the same reason.
//
// Orthogonality: RegisterFixedAsset owns the initial cost,
// Depreciate owns the monthly charge, and this module owns only
// the cost revision. It is orthogonal to the useful-life amendment
// (LG-268, which moves the horizon the base is spread over), the
// salvage amendment (LG-269, which moves the depreciable base from
// the other side), and the method amendment (LG-270, which moves
// how the base is spread) — this moves only the capitalized cost
// itself, the top of the base.
package ledger

import (
	"fmt"
	"strings"
)

// AmendFixedAssetCost revises a registered fixed asset's capitalized
// cost in place. An unknown ID fails with ErrFixedAssetNotFound; a
// non-positive new cost fails with ErrInvalidAssetCost; a new cost
// at or below the salvage fails with ErrInvalidSalvage; an asset
// with any depreciation month posted fails with
// ErrFixedAssetDepreciationPosted. Only CostCents changes; the
// salvage, useful life, method, currency, and already-booked months
// are untouched, and already-booked months are never recomputed —
// the new cost governs from the next Depreciate call. Amending to
// the current cost is a no-op. Amending is off-journal — it does
// not bump the ledger version — but it is audited as
// fixed_asset_cost_amended.
func (l *Ledger) AmendFixedAssetCost(assetID string, newCostCents int64) (FixedAsset, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return FixedAsset{}, ErrFixedAssetNotFound
	}
	if newCostCents <= 0 {
		return FixedAsset{}, ErrInvalidAssetCost
	}
	if asset.SalvageCents >= newCostCents {
		return FixedAsset{}, ErrInvalidSalvage
	}
	if l.hasPostedDepreciationLocked(assetID) {
		return FixedAsset{}, fmt.Errorf("%w: asset %q already has depreciation posted", ErrFixedAssetDepreciationPosted, assetID)
	}
	if asset.CostCents == newCostCents {
		return asset, nil
	}
	previous := asset.CostCents
	asset.CostCents = newCostCents
	l.fixedAssets[assetID] = asset
	l.emitAudit(AuditEvent{
		Op:            "fixed_asset_cost_amended",
		Actor:         "AmendFixedAssetCost",
		TraceID:       assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{asset.Account},
		Details: map[string]any{
			"asset_id":            assetID,
			"previous_cost_cents": previous,
			"cost_cents":          newCostCents,
		},
	})
	return asset, nil
}

// hasPostedDepreciationLocked reports whether any depreciation month
// has ever been posted for the asset: any "depr/<assetID>/<YYYY-MM>"
// entry, including one later reversed (a reversal row implies its
// month was posted). Callers must hold l.mu: it scans the journal
// for this asset's depreciation namespace, classifying entries the
// same way accumulatedDepreciationLocked does. Foreign entries
// sharing the namespace prefix are ignored, exactly as there.
func (l *Ledger) hasPostedDepreciationLocked(assetID string) bool {
	prefix := depreciationEntryPrefix(assetID)
	for id := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		rest := strings.TrimPrefix(id, prefix)
		switch {
		case rest == "":
			continue
		case !strings.Contains(rest, "/"):
			// "YYYY-MM": one booked depreciation month.
			return true
		case strings.HasSuffix(rest, "/reversal") &&
			!strings.Contains(strings.TrimSuffix(rest, "/reversal"), "/"):
			// "YYYY-MM/reversal": its month was posted.
			return true
		default:
			// Foreign entry sharing the namespace prefix (e.g. a
			// hand-posted ID) — not one of ours, skip it.
			continue
		}
	}
	return false
}
