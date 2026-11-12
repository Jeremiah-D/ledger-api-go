package ledger

import (
	"context"
	"fmt"
	"time"
)

// Schedule preview horizon-cap runtime revision (LG-399).
//
// PreviewSchedule accepts a horizon up to maxSchedulePreviewHorizon,
// and the bound was a construction-time constant (8760h, one year).
// An operator who wants to preview a multi-year loan-servicing plan
// (a 2-year window needs 17520h) or to tighten the window to 90 days
// to limit dry-run cost had to change the constant and redeploy.
// This module is the orthogonal runtime path: an operator revises the
// live cap in place, instead of restarting the server.
//
// Semantics, along LG-347 (the forward grace: the same knob,
// (previous, error) audit shape), LG-344 (the check order —
// validate, compare, swap, then audit), LG-397/LG-398 (the schedule
// run-history and preview firing-cap sides of the same pattern,
// different objects):
//
//   - The cap is a live field on the Ledger, guarded by l.mu — the
//     same lock that guards the schedule registry, so no second lock
//     is introduced and no lock order is created. The only
//     enforcement read site (PreviewSchedule) reads it under the
//     read lock its caller already holds.
//   - The constant stays as the default-value source, renamed
//     defaultMaxSchedulePreviewHorizon: the live field seeds from
//     it (8760h) at construction.
//   - A horizon cap of zero or below is rejected with
//     ErrScheduleBadHorizon and the live value is left untouched:
//     zero would make every preview reject, which is switching the
//     preview surface off, not a meaningful cap (along LG-336).
//     There is no upper bound: the firing-count cap
//     (defaultMaxPreviewFirings) still truncates the nominal-time
//     list, and the memory/CPU trade-off belongs to the operator
//     (along LG-356).
//   - Revising to the current cap is a no-op with no audit event.
//     The revision is audited as schedule_preview_horizon_retuned,
//     with previous/current as duration strings
//     (previous_max_horizon / current_max_horizon, keyed after the
//     knob, along the previous_depth / current_depth shape).
//   - Tightening or loosening takes effect on the next
//     PreviewSchedule (and the next HTTP preview, which reads the
//     same live value): the preview is computed fresh each time
//     and keeps no stored state, so there is nothing to trim —
//     unlike LG-397's immediate history trim, there is no stored
//     history here at all.
//   - The cap does not round-trip through snapshots: preview is
//     read-only (schedule_preview.go), so a restart restores the
//     schedules and the 8760h default. The revision event stays in
//     the audit trail.

// SetSchedulePreviewMaxHorizon revises the live preview-horizon cap
// in place and returns the pre-revision cap (for the audit trail).
// The next PreviewSchedule already enforces the new cap. A cap of
// zero or below is rejected with ErrScheduleBadHorizon and the live
// cap is left untouched (the returned value is the live cap in that
// case too). Revising to the current cap is a no-op with no audit
// event. The revision is audited as
// schedule_preview_horizon_retuned.
func (l *Ledger) SetSchedulePreviewMaxHorizon(d time.Duration) (time.Duration, error) {
	return l.SetSchedulePreviewMaxHorizonCtx(context.Background(), d)
}

// SetSchedulePreviewMaxHorizonCtx is SetSchedulePreviewMaxHorizon
// with per-call audit actor attribution (LG-73).
func (l *Ledger) SetSchedulePreviewMaxHorizonCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	// Check order, along LG-344: validate first, then compare,
	// then swap and audit — all under l.mu, which also guards
	// the field and the schedule registry, so the decision and
	// the swap are atomic with respect to every preview reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.schedulePreviewMaxHorizon
	if d <= 0 {
		return previous, fmt.Errorf("%w: got %s", ErrScheduleBadHorizon, d)
	}
	if d == previous {
		return previous, nil
	}
	l.schedulePreviewMaxHorizon = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "schedule_preview_horizon_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_horizon": previous.String(),
			"current_max_horizon":  d.String(),
		},
	})
	return previous, nil
}

// SchedulePreviewMaxHorizon reports the live preview-horizon cap:
// the longest window PreviewSchedule accepts for one plan. The
// name is shared with the default constant, which is the
// construction-time default (8760h) and the seed — the method
// reads the live value.
func (l *Ledger) SchedulePreviewMaxHorizon() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.schedulePreviewMaxHorizon
}
