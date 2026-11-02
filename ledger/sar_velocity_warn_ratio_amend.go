package ledger

import (
	"context"
	"errors"
	"fmt"
)

// SAR velocity warning-band amendment (LG-293).
//
// WithSARVelocityWarnRatio (sar.go) installs the velocity warning-band
// fraction at construction — the fraction of a velocity limit at/above
// which rolling-window usage raises VELOCITY_ANOMALY; this module is
// the orthogonal runtime path: an operator revises the fraction in
// place, instead of rebuilding the ledger and replaying its journal.
// Revising in place matters because a rebuild is a new ledger — a gap
// in which the old warning band (or the default) governs detection,
// plus a fresh identity that breaks the audit story — while an
// amendment keeps the same ledger, the same journal, and the same audit
// trail from old fraction to new.
//
// Semantics, deliberately fail-closed throughout (along LG-241):
//
//   - The revision governs only subsequent DraftSAR calls: DraftSAR
//     reads the live l.sarCfg.velocityWarnRatio under l.mu at draft
//     time (via sarVelocitySightingsLocked), so a draft assembled
//     after the amendment warns under the new band, while a draft
//     assembled before it keeps the sightings it was built with. No
//     draft path needed a code change — the revision is clean exactly
//     because the readers were already live.
//   - Already-drafted and already-filed reports are immutable:
//     filing seals a report with a SHA-256 content hash (LG-188)
//     and there is no update path, so retuning the warning band never
//     rewrites a filed report's reason codes, sightings, or hash.
//     A report that needs the new band is re-drafted, not amended.
//   - Validation is strict, like LG-241 rather than the
//     construction option: the construction option panics on an
//     out-of-range ratio, but a runtime amendment must never panic
//     and must never silently install a band that under-detects. A
//     ratio outside (0, 1) fails with
//     ErrInvalidSARVelocityWarnRatio, and the previous fraction is
//     left exactly as it was — the operator fixes the value and
//     retries, the ledger never runs on a half-installed detector.
//   - Revising to the value already in force is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, but it lands one sar_velocity_warn_ratio_amended audit
//     event carrying the previous and new ratio. The tuning does NOT
//     ride snapshots — snapshot.go carries no sarCfg line — so a
//     restored ledger comes back on the 0.8 default and operators
//     must re-set any amended tuning after a restore. (The velocity
//     *usage* deque is live-only too, so a restored ledger also
//     rebuilds its window usage from fresh postings before the band
//     can flag again; the limit configuration itself does ride
//     snapshots.)
//
// Orthogonality: WithSARVelocityWarnRatio owns the construction-time
// tuning, DraftSAR owns detection (reading the live tuning),
// FileSAR owns the immutable filing, and this module owns only the
// runtime revision of the tuning itself. There is no HTTP endpoint
// for the revision (per LG-241/242).

// SAR velocity warning-band amendment errors.
var (
	// ErrInvalidSARVelocityWarnRatio is returned by
	// SetSARVelocityWarnRatio when the proposed fraction cannot be
	// installed: a ratio outside (0, 1). The previous fraction is
	// left exactly as it was.
	ErrInvalidSARVelocityWarnRatio = errors.New("ledger: invalid SAR velocity warn ratio")
)

// SetSARVelocityWarnRatio replaces the velocity warning-band fraction
// in place. Only DraftSAR calls made after the call warn under the
// new band (usage at/above ratio * limit); already-drafted and
// already-filed reports are untouched. A ratio outside (0, 1) fails
// with ErrInvalidSARVelocityWarnRatio and leaves the previous
// fraction unchanged. Revising to the current value is a no-op. The
// amendment is audited as sar_velocity_warn_ratio_amended.
func (l *Ledger) SetSARVelocityWarnRatio(ratio float64) error {
	return l.SetSARVelocityWarnRatioCtx(context.Background(), ratio)
}

// SetSARVelocityWarnRatioCtx is SetSARVelocityWarnRatio with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetSARVelocityWarnRatioCtx(ctx context.Context, ratio float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !(ratio > 0 && ratio < 1) {
		return fmt.Errorf("%w: ratio must be in (0, 1), got %v", ErrInvalidSARVelocityWarnRatio, ratio)
	}
	previous := l.sarCfg.velocityWarnRatio
	if previous == ratio {
		return nil
	}
	l.sarCfg.velocityWarnRatio = ratio
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sar_velocity_warn_ratio_amended",
		Actor:         "SetSARVelocityWarnRatio",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ratio": previous,
			"ratio":          ratio,
		},
	})
	return nil
}
