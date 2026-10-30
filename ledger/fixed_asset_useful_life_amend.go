// Fixed-asset useful-life amendment (LG-268).
//
// RegisterFixedAsset (LG-141) fixes an asset's useful life at
// registration; Depreciate books one month at a time against that
// life. This module is the revision path: an operator revises a live
// asset's useful life instead of retiring the asset and registering a
// replacement. Amending in place matters because a replacement is a
// new asset — a fresh ID that breaks the "depr/<assetID>/<YYYY-MM>"
// idempotency lineage, so already-booked months could never be
// re-run or netted against the same asset — while an amendment keeps
// the same asset, the same booked months, and one audit trail from
// registration to full depreciation.
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrFixedAssetNotFound. The new life
//     reuses the registration gate (see validateFixedAsset): it must
//     be a positive number of months, otherwise ErrInvalidUsefulLife
//     and the original life is untouched.
//   - An asset whose remaining depreciable base (cost - salvage -
//     accumulated) is zero or negative fails with
//     ErrFixedAssetFullyDepreciated: a fully depreciated asset has
//     no future depreciation for the revision to govern. The gate
//     runs before the no-op check, following the installment
//     precedent (a terminal plan rejects even a same-value
//     amendment) — the fail-closed state gate wins over the
//     redundant call.
//   - The amendment touches UsefulLifeMonths and nothing else: the
//     cost, salvage, method, currency, and already-booked months
//     are unchanged. Booked months are deterministic journal rows
//     ("depr/<assetID>/<YYYY-MM>") and are never recomputed —
//     changing the life does not restate history, it only changes
//     what future months charge.
//   - The new life governs from the next Depreciate call:
//     monthlyDepreciation reads the live UsefulLifeMonths every
//     month, so no sweep change is needed. Straight-line charges
//     base/new-life per month; double-declining charges
//     bookValue*2/new-life. Both stay under the remaining-base cap
//     with the dust true-up, so a life shortened below the months
//     already booked still converges exactly to salvage: the next
//     month books at most the remaining base, and the month after
//     that trues up whatever is left.
//   - Amending to the current life is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Like registration, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the asset was already reported when registered.
//     The amendment lands one audit event
//     (fixed_asset_useful_life_amended, with the asset ID and the
//     previous and new lives) so the "who moved the useful life,
//     and from what to what" trail is tamper-evident. The asset
//     register is in-memory structural config (see depreciation.go),
//     so an amendment, like the registration itself, does not
//     round-trip a snapshot restore — re-register after restore.
//     This module has no Ctx variant: the fixed-asset module has
//     none (see the LG-264 precedent for modules without one).
//
// Orthogonality: RegisterFixedAsset owns the initial life,
// Depreciate owns the monthly charge, and this module owns only
// the life revision. It is orthogonal to the salvage amendment
// (LG-269, which moves the depreciable base) and the method
// amendment (LG-270, which moves how the base is spread) —
// this moves only the horizon the base is spread over.
package ledger

import "fmt"

// AmendFixedAssetUsefulLife revises a registered fixed asset's
// useful life in place. An unknown ID fails with
// ErrFixedAssetNotFound; a non-positive new life fails with
// ErrInvalidUsefulLife; a fully depreciated asset (remaining
// depreciable base zero or negative) fails with
// ErrFixedAssetFullyDepreciated. Only UsefulLifeMonths changes;
// the cost, salvage, method, currency, and already-booked months
// are untouched, and already-booked months are never recomputed —
// the new life governs from the next Depreciate call. Amending to
// the current life is a no-op. Amending is off-journal — it does
// not bump the ledger version — but it is audited as
// fixed_asset_useful_life_amended.
func (l *Ledger) AmendFixedAssetUsefulLife(assetID string, months int64) (FixedAsset, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return FixedAsset{}, ErrFixedAssetNotFound
	}
	if months <= 0 {
		return FixedAsset{}, ErrInvalidUsefulLife
	}
	booked, err := l.accumulatedDepreciationLocked(assetID)
	if err != nil {
		return FixedAsset{}, err
	}
	base, ok := addCents(asset.CostCents, -asset.SalvageCents)
	if !ok {
		return FixedAsset{}, errDepreciationOverflow()
	}
	remaining, ok := addCents(base, -booked)
	if !ok {
		return FixedAsset{}, errDepreciationOverflow()
	}
	if remaining <= 0 {
		return FixedAsset{}, fmt.Errorf("%w: asset %q", ErrFixedAssetFullyDepreciated, assetID)
	}
	if asset.UsefulLifeMonths == months {
		return asset, nil
	}
	previous := asset.UsefulLifeMonths
	asset.UsefulLifeMonths = months
	l.fixedAssets[assetID] = asset
	l.emitAudit(AuditEvent{
		Op:            "fixed_asset_useful_life_amended",
		Actor:         "AmendFixedAssetUsefulLife",
		TraceID:       assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{asset.Account},
		Details: map[string]any{
			"asset_id":                    assetID,
			"previous_useful_life_months": previous,
			"useful_life_months":          months,
		},
	})
	return asset, nil
}
