package ledger

import (
	"context"
	"errors"
	"fmt"
)

// SAR structuring-detection tuning amendment (LG-292).
//
// WithSARStructuring (sar.go) installs the structuring detector tuning
// at construction — the fraction of a large-tx threshold at/above which
// a sub-threshold amount counts as a candidate, and how many candidates
// in one UTC day raise STRUCTURING; this module is the orthogonal
// runtime path: an operator revises the tuning in place, instead of
// rebuilding the ledger and replaying its journal. Revising in place
// matters because a rebuild is a new ledger — a gap in which the old
// tuning (or the defaults) governs detection, plus a fresh identity
// that breaks the audit story — while an amendment keeps the same
// ledger, the same journal, and the same audit trail from old tuning
// to new.
//
// Semantics, deliberately fail-closed throughout (along LG-241):
//
//   - The revision governs only subsequent DraftSAR calls: DraftSAR
//     reads the live l.sarCfg.structuringRatio /
//     structuringMinCount under l.mu at draft time, so a draft
//     assembled after the amendment clusters under the new floor
//     and count gate, while a draft assembled before it keeps the
//     sightings it was built with. No draft path needed a code
//     change — the revision is clean exactly because the readers
//     were already live.
//   - Already-drafted and already-filed reports are immutable:
//     filing seals a report with a SHA-256 content hash (LG-188)
//     and there is no update path, so retuning the detector never
//     rewrites a filed report's reason codes, sightings, or hash.
//     A report that needs the new tuning is re-drafted, not
//     amended.
//   - Validation is strict, like LG-241 rather than the
//     construction option: the construction option panics on an
//     out-of-range ratio or a minCount below 2, but a runtime
//     amendment must never panic and must never silently install
//     a tuning that under-detects. A ratio outside (0, 1) or a
//     minCount below 2 fails with ErrInvalidSARStructuring, and
//     the previous tuning is left exactly as it was — the
//     operator fixes the values and retries, the ledger never
//     runs on a half-installed detector.
//   - Revising to the values already in force is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, but it lands one sar_structuring_amended audit event
//     carrying the previous and new ratio and min-count. The
//     tuning does NOT ride snapshots — snapshot.go carries no
//     sarCfg line — so a restored ledger comes back on the
//     0.75 / 3 defaults and operators must re-set any amended
//     tuning after a restore.
//
// Orthogonality: WithSARStructuring owns the construction-time
// tuning, DraftSAR owns detection (reading the live tuning),
// FileSAR owns the immutable filing, and this module owns only the
// runtime revision of the tuning itself. There is no HTTP endpoint
// for the revision (per LG-241/242).

// SAR structuring-detection tuning amendment errors.
var (
	// ErrInvalidSARStructuring is returned by SetSARStructuring
	// when the proposed tuning cannot be installed: a ratio
	// outside (0, 1), or a minCount below 2. The previous tuning
	// is left exactly as it was.
	ErrInvalidSARStructuring = errors.New("ledger: invalid SAR structuring tuning")
)

// SetSARStructuring replaces the structuring detector tuning in
// place. Only DraftSAR calls made after the call cluster under the
// new floor (ratio * threshold) and count gate; already-drafted and
// already-filed reports are untouched. A ratio outside (0, 1) or a
// minCount below 2 fails with ErrInvalidSARStructuring and leaves
// the previous tuning unchanged. Revising to the current values is
// a no-op. The amendment is audited as sar_structuring_amended.
func (l *Ledger) SetSARStructuring(ratio float64, minCount int) error {
	return l.SetSARStructuringCtx(context.Background(), ratio, minCount)
}

// SetSARStructuringCtx is SetSARStructuring with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetSARStructuringCtx(ctx context.Context, ratio float64, minCount int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !(ratio > 0 && ratio < 1) {
		return fmt.Errorf("%w: ratio must be in (0, 1), got %v", ErrInvalidSARStructuring, ratio)
	}
	if minCount < 2 {
		return fmt.Errorf("%w: minCount must be >= 2, got %d", ErrInvalidSARStructuring, minCount)
	}
	previousRatio := l.sarCfg.structuringRatio
	previousMinCount := l.sarCfg.structuringMinCount
	if previousRatio == ratio && previousMinCount == minCount {
		return nil
	}
	l.sarCfg.structuringRatio = ratio
	l.sarCfg.structuringMinCount = minCount
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sar_structuring_amended",
		Actor:         "SetSARStructuring",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ratio":     previousRatio,
			"ratio":              ratio,
			"previous_min_count": previousMinCount,
			"min_count":          minCount,
		},
	})
	return nil
}
