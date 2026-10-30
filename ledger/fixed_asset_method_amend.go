// Fixed-asset depreciation-method amendment (LG-270).
//
// RegisterFixedAsset (LG-141) fixes an asset's depreciation method
// at registration; Depreciate books one month at a time under that
// method. This module is the revision path: an operator revises a
// live asset's method instead of retiring the asset and registering
// a replacement. Amending in place matters because a replacement is
// a new asset — a fresh ID that breaks the "depr/<assetID>/<YYYY-MM>"
// idempotency lineage, so already-booked months could never be
// re-run or netted against the same asset — while an amendment keeps
// the same asset, the same booked months, and one audit trail from
// registration to full depreciation.
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrFixedAssetNotFound. The new method
//     reuses the registration gate (see validDepreciationMethod): it
//     must be "straight-line" or "double-declining", otherwise
//     ErrInvalidDepreciationMethod and the original method is
//     untouched.
//   - An asset whose remaining depreciable base (cost - salvage -
//     accumulated) is zero or negative fails with
//     ErrFixedAssetFullyDepreciated: a fully depreciated asset has
//     no future depreciation for the revision to govern. The gate
//     runs before the no-op check, following the installment
//     precedent (a terminal plan rejects even a same-value
//     amendment) — the fail-closed state gate wins over the
//     redundant call.
//   - The amendment touches Method and nothing else: the cost,
//     salvage, useful life, currency, and already-booked months
//     are unchanged. Booked months are deterministic journal rows
//     ("depr/<assetID>/<YYYY-MM>") and are never recomputed —
//     changing the method does not restate history, it only changes
//     how future months spread the remaining base. Months already
//     taken keep their old-method amounts.
//   - The new method governs from the next Depreciate call:
//     monthlyDepreciation reads the live Method every month, so no
//     sweep change is needed. Straight-line charges base/life per
//     month; double-declining charges bookValue*2/life. Both stay
//     under the remaining-base cap with the dust true-up, so the
//     schedule still converges exactly to salvage without
//     over-depreciating — in either direction: switching to
//     double-declining after straight-line months front-loads the
//     remainder, and switching back to straight-line re-evens it.
//   - Amending to the current method is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like registration, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the asset was already reported when registered.
//     The amendment lands one audit event
//     (fixed_asset_method_amended, with the asset ID and the
//     previous and new methods) so the "who moved the method, and
//     from what to what" trail is tamper-evident. The asset
//     register is in-memory structural config (see depreciation.go),
//     so an amendment, like the registration itself, does not
//     round-trip a snapshot restore — re-register after restore.
//     This module has no Ctx variant: the fixed-asset module has
//     none (see the LG-264 precedent for modules without one).
//
// Orthogonality: RegisterFixedAsset owns the initial method,
// Depreciate owns the monthly charge, and this module owns only
// the method revision. It is orthogonal to the useful-life
// amendment (LG-268, which moves the horizon the base is spread
// over) and the salvage amendment (LG-269, which moves the
// depreciable base) — this moves only how the base is spread.
package ledger

import "fmt"

// AmendFixedAssetDepreciationMethod revises a registered fixed
// asset's depreciation method in place. An unknown ID fails with
// ErrFixedAssetNotFound; a method other than "straight-line" or
// "double-declining" fails with ErrInvalidDepreciationMethod; a
// fully depreciated asset (remaining depreciable base zero or
// negative) fails with ErrFixedAssetFullyDepreciated. Only Method
// changes; the cost, salvage, useful life, currency, and
// already-booked months are untouched, and already-booked months
// are never recomputed — the new method governs from the next
// Depreciate call. Amending to the current method is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as fixed_asset_method_amended.
func (l *Ledger) AmendFixedAssetDepreciationMethod(assetID string, method string) (FixedAsset, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return FixedAsset{}, ErrFixedAssetNotFound
	}
	if !validDepreciationMethod(method) {
		return FixedAsset{}, ErrInvalidDepreciationMethod
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
	if asset.Method == method {
		return asset, nil
	}
	previous := asset.Method
	asset.Method = method
	l.fixedAssets[assetID] = asset
	l.emitAudit(AuditEvent{
		Op:            "fixed_asset_method_amended",
		Actor:         "AmendFixedAssetDepreciationMethod",
		TraceID:       assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{asset.Account},
		Details: map[string]any{
			"asset_id":        assetID,
			"previous_method": previous,
			"method":          method,
		},
	})
	return asset, nil
}
