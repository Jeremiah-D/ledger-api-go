package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Schedule preview firing-count cap runtime revision (LG-398).
//
// PreviewSchedule lists at most maxPreviewFirings nominal firings per
// plan, and the bound was a construction-time constant (10000). An
// operator who wants to see a high-frequency plan's full annual firing
// sequence (a 1-minute cadence over a year would need 525600 entries)
// or to tighten a single dry-run's cost under CPU pressure had to
// change the constant and redeploy. This module is the orthogonal
// runtime path: an operator revises the live cap in place, instead of
// restarting the server.
//
// Semantics, along LG-347 (the forward grace: the same knob,
// (previous, error) audit shape), LG-344 (the check order —
// validate, compare, swap, then audit), LG-395/LG-397 (the report
// and schedule run-history sides of the same pattern, different
// objects):
//
//   - The cap is a live field on the Ledger, guarded by l.mu — the
//     same lock that guards the schedule registry, so no second lock
//     is introduced and no lock order is created. The only
//     enforcement read site (PreviewSchedule) reads it under the
//     read lock its caller already holds and passes it to the
//     receiver-less previewFiringTimes as a plain parameter.
//   - The constant stays as the default-value source, renamed
//     defaultMaxPreviewFirings: the live field seeds from it (10000)
//     at construction.
//   - A cap below 1 is rejected with
//     ErrInvalidSchedulePreviewMaxFirings and the live value is left
//     untouched. Zero is rejected, not a "list nothing" state: a
//     zero cap would make every preview return zero firings, which
//     is switching the preview surface off, not a meaningful cap
//     (along LG-336). There is no upper bound: the horizon cap
//     (defaultMaxSchedulePreviewHorizon) still bounds the window, and the
//     memory/CPU trade-off belongs to the operator (along LG-356).
//   - Revising to the current cap is a no-op with no audit event.
//     The revision is audited as schedule_preview_firings_retuned,
//     with previous/current as integers (previous_max_firings /
//     current_max_firings, keyed after the knob, along the
//     previous_max_records / current_max_records shape).
//   - Tightening or loosening takes effect on the next
//     PreviewSchedule: the preview is computed fresh each time and
//     keeps no stored state, so there is nothing to trim —
//     unlike LG-397's immediate history trim, there is no stored
//     history here at all.
//   - The cap does not round-trip through snapshots: preview is
//     read-only (schedule_preview.go), so a restart restores the
//     schedules and the 10000 default. The revision event stays in
//     the audit trail.

var (
	// ErrInvalidSchedulePreviewMaxFirings is returned by
	// SetSchedulePreviewMaxFirings for a cap below 1: the live cap
	// is left untouched.
	ErrInvalidSchedulePreviewMaxFirings = errors.New("ledger: schedule preview max firings must be at least 1")
)

// SetSchedulePreviewMaxFirings revises the live per-preview firing
// cap in place and returns the pre-revision cap (for the audit
// trail). The next PreviewSchedule already lists at most the new
// cap. A cap below 1 is rejected with
// ErrInvalidSchedulePreviewMaxFirings and the live cap is left
// untouched (the returned value is the live cap in that case too).
// Revising to the current cap is a no-op with no audit event. The
// revision is audited as schedule_preview_firings_retuned.
func (l *Ledger) SetSchedulePreviewMaxFirings(n int) (int, error) {
	return l.SetSchedulePreviewMaxFiringsCtx(context.Background(), n)
}

// SetSchedulePreviewMaxFiringsCtx is SetSchedulePreviewMaxFirings
// with per-call audit actor attribution (LG-73).
func (l *Ledger) SetSchedulePreviewMaxFiringsCtx(ctx context.Context, n int) (int, error) {
	// Check order, along LG-344: validate first, then compare,
	// then swap and audit — all under l.mu, which also guards
	// the field and the schedule registry, so the decision and
	// the swap are atomic with respect to every preview reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.schedulePreviewMaxFirings
	if n < 1 {
		return previous, fmt.Errorf("%w: got %d", ErrInvalidSchedulePreviewMaxFirings, n)
	}
	if n == previous {
		return previous, nil
	}
	l.schedulePreviewMaxFirings = n
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "schedule_preview_firings_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_firings": previous,
			"current_max_firings":  n,
		},
	})
	return previous, nil
}

// SchedulePreviewMaxFirings reports the live per-preview firing
// cap: how many nominal firings PreviewSchedule lists at most for
// one plan. The name is shared with the default constant, which is
// the construction-time default (10000) and the seed — the method
// reads the live value.
func (l *Ledger) SchedulePreviewMaxFirings() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.schedulePreviewMaxFirings
}
