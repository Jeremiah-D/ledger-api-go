package ledger

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Future-stage role revision for per-transfer staged approval (LG-227).
//
// LG-221 installs an ordered role chain on one pending review and
// SetApprovalStages is install-only: once stages exist, a second
// install fails with ErrStagedApprovalAlreadySet. That leaves no way
// to fix a future rung — a role that no longer exists, a team that
// was renamed — without killing the review and starting over. This
// module is the post-install revision path, and it is deliberately
// narrower than a re-install:
//
//   - Only stages that have not started yet may change: stages with
//     index greater than the current Stage. The current stage and
//     every decided stage are immutable, and the approver recorded
//     for each decided stage never moves — a revision cannot rewrite
//     who already signed, only who must sign next.
//   - The caller supplies exactly the future stages, in order: the
//     list length must equal len(Stages)-Stage-1. A short or long
//     list fails, because the stage count never changes — stages
//     are neither added nor removed by a revision, and a positional
//     list cannot silently shift a role onto the wrong rung.
//   - New roles are validated with the installation rule
//     (validateApprovalStages, minus the non-empty-list check that
//     the length check already covers): a blank role is rejected
//     and the review is untouched. Roles are trimmed like at
//     install time.
//   - The per-stage timeouts (LG-223) and the overall deadline
//     (LG-226) ride the same progress record and are untouched by a
//     revision: a role change moves no clock. The current stage's
//     start instant is likewise untouched, so a stage that is
//     already running keeps the timeout it started with.
//   - A revision is structural, like the install itself: it does
//     not bump the ledger version, and it lands one audit event
//     (staged_future_roles_replaced, with the current stage, the
//     stage count, and the old and new future roles) so the "who
//     was supposed to sign, and when did that change" trail is
//     tamper-evident. The revised roles ride the review record,
//     the reconcile report, and snapshots exactly like the
//     installed roles — they are the same record, mutated in place.
//   - Orthogonality: this revises roles only. LG-221 owns stage
//     order and voting rules, LG-223 owns per-stage timeouts,
//     LG-226 owns the overall deadline, and none of those change
//     here. A review with no future stages left (sitting on its
//     last stage) has nothing to revise and fails — the caller
//     cannot use a revision to smuggle a stage in or out.
//
// A revision on a review that is no longer pending fails, and a
// review-level lapse auto-rejects first like the install path — a
// dead review's future roles are not worth revising.

// Future-stage role revision errors.
var (
	// ErrInvalidFutureStageRoles is returned by
	// ReplaceFutureStageRoles when the replacement list cannot
	// apply: no future stages left to revise, a length that does
	// not cover exactly the future stages, or a blank role.
	ErrInvalidFutureStageRoles = errors.New("ledger: invalid future staged approval roles")
)

// ReplaceFutureStageRoles replaces the roles of the not-yet-started
// stages of a staged review: roles[i] becomes the role of stage
// Stage+1+i, in order. The review must be pending and carry stages
// (otherwise ErrStagedApprovalNotConfigured), and the list must
// cover exactly the future stages with no blank role. The current
// and decided stages, their approvers, the stage count, the
// per-stage timeouts, and the overall deadline are all untouched.
// Revising is structural — it does not bump the ledger version.
func (l *Ledger) ReplaceFutureStageRoles(transferID string, roles []ApprovalStage) error {
	return l.ReplaceFutureStageRolesCtx(context.Background(), transferID, roles)
}

// ReplaceFutureStageRolesCtx is ReplaceFutureStageRoles with per-call
// audit actor attribution (LG-73).
func (l *Ledger) ReplaceFutureStageRolesCtx(ctx context.Context, transferID string, roles []ApprovalStage) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	r, ok := l.reviews[transferID]
	if !ok {
		return ErrReviewNotFound
	}
	if r.Status != ReviewStatusPending {
		return ErrReviewNotPending
	}
	now := time.Now()
	if reviewExpiredLocked(r, now) {
		l.expireReviewLocked(r, now)
		l.emitReviewExpireAuditCtxLocked(ctx, 1)
		return ErrReviewExpired
	}
	p := r.StagedApproval
	if p == nil {
		return ErrStagedApprovalNotConfigured
	}
	future := len(p.Stages) - p.Stage - 1
	if future <= 0 || len(roles) != future {
		return ErrInvalidFutureStageRoles
	}
	trimmed := make([]ApprovalStage, len(roles))
	for i, s := range roles {
		if strings.TrimSpace(s.Role) == "" {
			return ErrInvalidFutureStageRoles
		}
		trimmed[i] = ApprovalStage{Role: strings.TrimSpace(s.Role)}
	}
	previous := append([]ApprovalStage(nil), p.Stages[p.Stage+1:]...)
	for i, s := range trimmed {
		p.Stages[p.Stage+1+i] = s
	}
	l.reviews[transferID] = r
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "staged_future_roles_replaced",
		Actor:         "ReplaceFutureStageRoles",
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"stage":          p.Stage,
			"stage_count":    len(p.Stages),
			"from_stage":     p.Stage + 1,
			"previous_roles": stageRoles(previous),
			"roles":          stageRoles(trimmed),
		},
	})
	return nil
}

// stageRoles renders a stage list as its role names, for audit
// details.
func stageRoles(stages []ApprovalStage) []string {
	out := make([]string, len(stages))
	for i, s := range stages {
		out[i] = s.Role
	}
	return out
}
