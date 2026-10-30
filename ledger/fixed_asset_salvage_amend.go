// Fixed-asset salvage-value amendment (LG-269).
//
// RegisterFixedAsset (LG-141) fixes an asset's salvage value at
// registration; Depreciate books one month at a time against the
// depreciable base (cost minus salvage). This module is the revision
// path: an operator revises a live asset's salvage value instead of
// retiring the asset and registering a replacement. Amending in
// place matters because a replacement is a new asset — a fresh ID
// that breaks the "depr/<assetID>/<YYYY-MM>" idempotency lineage,
// so already-booked months could never be re-run or netted against
// the same asset — while an amendment keeps the same asset, the same
// booked months, and one audit trail from registration to full
// depreciation.
//
// Semantics, deliberately fail-closed throughout:
//
//   - An unknown ID fails with ErrFixedAssetNotFound. The new
//     salvage reuses the registration gate (see validateFixedAsset):
//     it must satisfy 0 <= salvage < cost, otherwise
//     ErrInvalidSalvage and the original salvage is untouched.
//   - An amendment that leaves no future depreciation fails with
//     ErrFixedAssetFullyDepreciated: the remaining depreciable base
//     is evaluated against the NEW salvage (cost - new salvage -
//     accumulated), so a revision that shrinks the base to or below
//     what is already booked is rejected. The gate runs before the
//     no-op check, following the installment precedent (a terminal
//     plan rejects even a same-value amendment) — the fail-closed
//     state gate wins over the redundant call. The corollary is
//     deliberate: lowering the salvage can REOPEN a fully
//     depreciated asset — an asset fully depreciated under a high
//     salvage regains future months when the salvage drops, because
//     the gate is evaluated against the post-amendment base, not the
//     pre-amendment one.
//   - The amendment touches SalvageCents and nothing else: the
//     cost, useful life, method, currency, and already-booked
//     months are unchanged. Booked months are deterministic journal
//     rows ("depr/<assetID>/<YYYY-MM>") and are never recomputed —
//     changing the salvage does not restate history, it only
//     changes what future months charge.
//   - The new salvage governs from the next Depreciate call:
//     monthlyDepreciation reads the live SalvageCents every month,
//     so no sweep change is needed. Straight-line charges
//     (cost - new salvage) / life per month; double-declining
//     charges bookValue*2/life, still floored at the new salvage
//     by the remaining-base cap. Both stay under the remaining-base
//     cap with the dust true-up, so the lifetime total converges
//     exactly to the new salvage: a salvage raised after months
//     are booked shortens the remaining base, and the final months
//     true up to it instead of over-depreciating.
//   - Amending to the current salvage is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like registration, the amendment is structural: it bumps
//     neither the ledger version nor the audit chain, and no outbox
//     event fires — the asset was already reported when registered.
//     The amendment lands one audit event
//     (fixed_asset_salvage_amended, with the asset ID and the
//     previous and new salvage values) so the "who moved the
//     salvage, and from what to what" trail is tamper-evident. The
//     asset register is in-memory structural config (see
//     depreciation.go), so an amendment, like the registration
//     itself, does not round-trip a snapshot restore — re-register
//     after restore. This module has no Ctx variant: the
//     fixed-asset module has none (see the LG-264 precedent for
//     modules without one).
//
// Orthogonality: RegisterFixedAsset owns the initial salvage,
// Depreciate owns the monthly charge, and this module owns only
// the salvage revision. It is orthogonal to the useful-life
// amendment (LG-268, which moves the horizon the base is spread
// over) and the method amendment (LG-270, which moves how the base
// is spread) — this moves only the depreciable base itself.
package ledger

import "fmt"

// AmendFixedAssetSalvageValue revises a registered fixed asset's
// salvage value in place. An unknown ID fails with
// ErrFixedAssetNotFound; a salvage outside 0 <= salvage < cost
// fails with ErrInvalidSalvage; an amendment that leaves no future
// depreciation (cost - new salvage - accumulated <= 0) fails with
// ErrFixedAssetFullyDepreciated. Only SalvageCents changes; the
// cost, useful life, method, currency, and already-booked months
// are untouched, and already-booked months are never recomputed —
// the new salvage governs from the next Depreciate call. Amending
// to the current salvage is a no-op. Amending is off-journal — it
// does not bump the ledger version — but it is audited as
// fixed_asset_salvage_amended.
func (l *Ledger) AmendFixedAssetSalvageValue(assetID string, salvageCents int64) (FixedAsset, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return FixedAsset{}, ErrFixedAssetNotFound
	}
	if salvageCents < 0 || salvageCents >= asset.CostCents {
		return FixedAsset{}, ErrInvalidSalvage
	}
	booked, err := l.accumulatedDepreciationLocked(assetID)
	if err != nil {
		return FixedAsset{}, err
	}
	// The remaining base is evaluated against the NEW salvage:
	// cost - new salvage - booked. A revision that leaves nothing
	// to depreciate is rejected; a revision that reopens base
	// (lowering the salvage on a fully depreciated asset) is
	// allowed, because the gate answers "is there future
	// depreciation under the revised base", not "was there any
	// under the old one".
	base, ok := addCents(asset.CostCents, -salvageCents)
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
	if asset.SalvageCents == salvageCents {
		return asset, nil
	}
	previous := asset.SalvageCents
	asset.SalvageCents = salvageCents
	l.fixedAssets[assetID] = asset
	l.emitAudit(AuditEvent{
		Op:            "fixed_asset_salvage_amended",
		Actor:         "AmendFixedAssetSalvageValue",
		TraceID:       assetID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{asset.Account},
		Details: map[string]any{
			"asset_id":               assetID,
			"previous_salvage_cents": previous,
			"salvage_cents":          salvageCents,
		},
	})
	return asset, nil
}
