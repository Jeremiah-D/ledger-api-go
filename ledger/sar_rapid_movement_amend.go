package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SAR rapid-movement detection tuning amendment (LG-294).
//
// WithSARRapidMovement (sar.go) installs the rapid-movement detector
// tuning at construction — the in-then-out window and the fraction of
// arrived funds that must leave inside it to raise RAPID_MOVEMENT;
// this module is the orthogonal runtime path: an operator revises the
// tuning in place, instead of rebuilding the ledger and replaying its
// journal. Revising in place matters because a rebuild is a new
// ledger — a gap in which the old tuning (or the defaults) governs
// detection, plus a fresh identity that breaks the audit story —
// while an amendment keeps the same ledger, the same journal, and the
// same audit trail from old tuning to new.
//
// Semantics, deliberately fail-closed throughout (along LG-241):
//
//   - The revision governs only subsequent DraftSAR calls: DraftSAR
//     reads the live l.sarCfg.rapidMoveWindow / rapidMoveRatio
//     under l.mu at draft time (via sarRapidMovementLocked), so a
//     draft assembled after the amendment cites under the new
//     window and ratio, while a draft assembled before it keeps
//     the sightings it was built with. No draft path needed a code
//     change — the revision is clean exactly because the readers
//     were already live.
//   - Already-drafted and already-filed reports are immutable:
//     filing seals a report with a SHA-256 content hash (LG-188)
//     and there is no update path, so retuning the detector never
//     rewrites a filed report's reason codes, sightings, or hash.
//     A report that needs the new tuning is re-drafted, not
//     amended.
//   - Validation is strict, like LG-241 rather than the
//     construction option: the construction option panics on a
//     non-positive window or a ratio outside (0, 1], but a runtime
//     amendment must never panic and must never silently install a
//     tuning that under-detects. A window <= 0 or a ratio outside
//     (0, 1] fails with ErrInvalidSARRapidMovement, and the
//     previous tuning is left exactly as it was — the operator
//     fixes the values and retries, the ledger never runs on a
//     half-installed detector.
//   - Revising to the values already in force is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, but it lands one sar_rapid_movement_amended audit
//     event carrying the previous and new window and ratio. The
//     tuning does NOT ride snapshots — snapshot.go carries no
//     sarCfg line — so a restored ledger comes back on the 4h /
//     0.8 defaults and operators must re-set any amended tuning
//     after a restore. (The rapid-movement detector walks the
//     journal entries themselves, which do round-trip, so a
//     restored ledger re-flags the same legs under the defaults
//     without fresh postings.)
//
// Orthogonality: WithSARRapidMovement owns the construction-time
// tuning, DraftSAR owns detection (reading the live tuning),
// FileSAR owns the immutable filing, and this module owns only the
// runtime revision of the tuning itself. There is no HTTP endpoint
// for the revision (per LG-241/242).

// SAR rapid-movement detection tuning amendment errors.
var (
	// ErrInvalidSARRapidMovement is returned by
	// SetSARRapidMovement when the proposed tuning cannot be
	// installed: a window <= 0, or a ratio outside (0, 1]. The
	// previous tuning is left exactly as it was.
	ErrInvalidSARRapidMovement = errors.New("ledger: invalid SAR rapid-movement tuning")
)

// SetSARRapidMovement replaces the rapid-movement detector tuning in
// place. Only DraftSAR calls made after the call cite under the new
// window and ratio (outflow at/above ratio of the window's inflow,
// within window of the first inflow); already-drafted and
// already-filed reports are untouched. A window <= 0 or a ratio
// outside (0, 1] fails with ErrInvalidSARRapidMovement and leaves
// the previous tuning unchanged. Revising to the current values is
// a no-op. The amendment is audited as sar_rapid_movement_amended.
func (l *Ledger) SetSARRapidMovement(window time.Duration, ratio float64) error {
	return l.SetSARRapidMovementCtx(context.Background(), window, ratio)
}

// SetSARRapidMovementCtx is SetSARRapidMovement with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetSARRapidMovementCtx(ctx context.Context, window time.Duration, ratio float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if window <= 0 {
		return fmt.Errorf("%w: window must be > 0, got %v", ErrInvalidSARRapidMovement, window)
	}
	if !(ratio > 0 && ratio <= 1) {
		return fmt.Errorf("%w: ratio must be in (0, 1], got %v", ErrInvalidSARRapidMovement, ratio)
	}
	previousWindow := l.sarCfg.rapidMoveWindow
	previousRatio := l.sarCfg.rapidMoveRatio
	if previousWindow == window && previousRatio == ratio {
		return nil
	}
	l.sarCfg.rapidMoveWindow = window
	l.sarCfg.rapidMoveRatio = ratio
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sar_rapid_movement_amended",
		Actor:         "SetSARRapidMovement",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_window": previousWindow.String(),
			"window":          window.String(),
			"previous_ratio":  previousRatio,
			"ratio":           ratio,
		},
	})
	return nil
}
