package ledger

import (
	"context"
	"errors"
	"time"
)

// Future-stage timeout revision for per-transfer staged approval (LG-239).
//
// SetStageTimeouts (LG-223) installs one lifetime per stage, one-shot:
// a second install fails with ErrStagedTimeoutsAlreadySet, so a
// half-walked chain's clocks could not be corrected without killing
// the review and starting over. This module is the post-install
// revision path, and it mirrors ReplaceFutureStageRoles (LG-227)
// rung for rung, with timeouts in place of roles:
//
//   - Only reviews that already carry installed stage timeouts may
//     be revised: a review with no stages fails with
//     ErrStagedApprovalNotConfigured, and a staged review whose
//     timeouts were never installed fails with
//     ErrStagedTimeoutsNotSet. Revising must never invent a
//     timeout configuration — the operator installs first with
//     SetStageTimeouts, then revises.
//   - Only stages that have not started yet may change: stages
//     with index greater than the current Stage. The current
//     stage's timeout is immutable, and so is every decided
//     stage's: a stage that is already running keeps the timeout
//     it started with, and its clock — StageStartedAt — is
//     untouched, so the current stage continues on its original
//     deadline even while its successors are re-timed. The
//     stage count never changes, and the caller supplies exactly
//     the future stages, in order: the list length must equal
//     len(Stages)-Stage-1. A short or long list fails, because a
//     positional list cannot silently shift a timeout onto the
//     wrong rung.
//   - Each new lifetime is validated with the installation rule
//     (a negative lifetime is rejected and the installed
//     timeouts are untouched). Zero is legal, with the LG-223
//     meaning: that future stage never times out on its own.
//     A review sitting on its last stage has no future stage
//     to revise and fails — the caller cannot use a revision
//     to smuggle a stage in or out.
//   - The overall deadline (LG-226) rides the same progress
//     record and is untouched by a revision: a timeout change
//     moves no overall bound, just as a role change moves no
//     clock. The decided approvers likewise never move.
//   - A revision is structural, like the install itself: it
//     does not bump the ledger version, and it lands one audit
//     event (staged_future_timeouts_amended, with the current
//     stage, the stage count, and the old and new future
//     timeouts) so the "which rung was re-timed, and from what
//     to what" trail is tamper-evident. The revised timeouts
//     ride the review record, the reconcile report, and
//     snapshots exactly like the installed timeouts — they are
//     the same record, mutated in place.
//   - Orthogonality: this revises timeouts only. LG-221 owns
//     stage order and voting rules, LG-223 owns the timeout
//     installation itself, LG-226 owns the overall deadline,
//     and LG-227 owns future-stage roles — none of those change
//     here.
//
// A revision on a review that is no longer pending fails, and a
// review-level lapse auto-expires first like the install path — a
// dead review's future timeouts are not worth revising.

// Future-stage timeout revision errors.
var (
	// ErrStagedTimeoutsNotSet is returned by
	// AmendFutureStageTimeouts when the review carries stages but
	// no timeouts were ever installed: revision never invents a
	// timeout configuration, the operator installs first.
	ErrStagedTimeoutsNotSet = errors.New("ledger: staged approval stage timeouts not set")
	// ErrInvalidFutureStageTimeouts is returned by
	// AmendFutureStageTimeouts when the replacement list cannot
	// apply: no future stages left to revise, a length that does
	// not cover exactly the future stages, or a negative
	// lifetime. Zero is legal (that stage never times out).
	ErrInvalidFutureStageTimeouts = errors.New("ledger: invalid future staged approval stage timeouts")
)

// AmendFutureStageTimeouts replaces the lifetimes of the
// not-yet-started stages of a staged review: timeouts[i] becomes
// the lifetime of stage Stage+1+i, in order. The review must be
// pending and carry installed stage timeouts (otherwise
// ErrStagedApprovalNotConfigured / ErrStagedTimeoutsNotSet), and
// the list must cover exactly the future stages with no negative
// lifetime. The current and decided stages' timeouts, the current
// stage's start instant, the stage count, the decided approvers,
// and the overall deadline are all untouched. Revising is
// structural — it does not bump the ledger version.
func (l *Ledger) AmendFutureStageTimeouts(transferID string, timeouts []time.Duration) error {
	return l.AmendFutureStageTimeoutsCtx(context.Background(), transferID, timeouts)
}

// AmendFutureStageTimeoutsCtx is AmendFutureStageTimeouts with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendFutureStageTimeoutsCtx(ctx context.Context, transferID string, timeouts []time.Duration) error {
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
	if p.Timeouts == nil {
		return ErrStagedTimeoutsNotSet
	}
	future := len(p.Stages) - p.Stage - 1
	if future <= 0 || len(timeouts) != future {
		return ErrInvalidFutureStageTimeouts
	}
	for _, d := range timeouts {
		if d < 0 {
			return ErrInvalidFutureStageTimeouts
		}
	}
	previous := append([]time.Duration(nil), p.Timeouts[p.Stage+1:]...)
	for i, d := range timeouts {
		p.Timeouts[p.Stage+1+i] = d
	}
	l.reviews[transferID] = r
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "staged_future_timeouts_amended",
		Actor:         "AmendFutureStageTimeouts",
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"stage":             p.Stage,
			"stage_count":       len(p.Stages),
			"from_stage":        p.Stage + 1,
			"previous_timeouts": durationStrings(previous),
			"timeouts":          durationStrings(timeouts),
		},
	})
	return nil
}

// durationStrings renders lifetimes in their String form, for audit
// details — the same rendering the staged-stage timeout event uses
// for a single timeout.
func durationStrings(ds []time.Duration) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}
	return out
}
