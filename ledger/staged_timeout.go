package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Per-stage timeouts for per-transfer staged approval (LG-223).
//
// LG-126 already gives the GLOBAL approval chain an opt-in per-stage
// timeout; this module is the per-transfer counterpart for LG-221's
// staged approval, and the two are orthogonal by scope exactly like
// the schemes themselves: a global chain timeout decides every review
// alike, a staged timeout decides one transfer on its own terms. A
// review decided under a chain or quorum scheme never consults staged
// timeouts — they live on the staged-approval progress record and are
// only meaningful while stages are installed.
//
// Semantics, mirroring the chain timeout:
//   - SetStageTimeouts installs one lifetime per stage, in stage
//     order; the list must cover every stage, and a negative lifetime
//     is rejected. Zero means that stage never times out on its own.
//     Installation is opt-in and one-shot: timeouts install once, and
//     a second install fails — a half-walked chain's clocks must not
//     be silently rewritten mid-flight.
//   - Each stage's clock runs from its own start: the install instant
//     for the stage the review sits on when timeouts land, and the
//     advance instant for every later stage (see
//     castStagedApproveVoteLocked). A stage that waits longer than
//     its lifetime without a decision auto-rejects the review —
//     fail-closed, never an advance to the next stage and never a
//     settlement past a silent approver.
//   - Timeout evaluation is lazy by predicate (a timed-out stage
//     already counts as inactive for available-balance purposes, via
//     reviewActiveLocked) plus an explicit sweep in ExpireReviews
//     (ExpireStagedReviewsAt), like review-level expiry and the
//     chain timeout. The lazy approve path rejects first and reports
//     ErrReviewExpired, like the chain path.
//   - A timeout rejection is a rejection, not an expiry: the review
//     status becomes rejected with ReviewedBy "system", the staged
//     progress (stages, decided approvers, timeouts) stays on the
//     record, and the audit event
//     transfer_review_staged_stage_timeout carries the stage, the
//     role, the configured timeout, and the reason "staged stage
//     timeout" — so the "who was silent, and which rung lapsed" trail
//     is tamper-evident. The sweep additionally emits one
//     review_rejected outbox envelope per timed-out review, like the
//     chain sweep. The timeout never bumps the ledger version: the
//     frozen legs move no money, they only stop reserving funds.
//   - A manual reject at a timed-out stage still rejects (the manual
//     reviewer is recorded instead of "system"): both endings are
//     terminal rejections, so a late human decision cannot be
//     mistaken for an advance, and the timeout path never has to win
//     a race against an operator who is already deciding.

// Staged timeout errors.
var (
	// ErrInvalidStageTimeouts is returned by SetStageTimeouts when
	// the timeout list does not cover every installed stage, or a
	// lifetime is negative. Zero is legal (that stage never times
	// out).
	ErrInvalidStageTimeouts = errors.New("ledger: invalid staged approval stage timeouts")
	// ErrStagedTimeoutsAlreadySet is returned by SetStageTimeouts
	// when the review already carries staged timeouts.
	ErrStagedTimeoutsAlreadySet = errors.New("ledger: staged approval stage timeouts already set")
)

// SetStageTimeouts installs one opt-in lifetime per installed stage
// on a pending staged review, in stage order. The review must carry
// stages (otherwise ErrStagedApprovalNotConfigured), must still be
// pending, and must not already carry timeouts. Installing is
// structural — it does not bump the ledger version, like installing
// the stages themselves. The current stage's clock starts at the
// install instant; later stages' clocks start when the review
// advances into them.
func (l *Ledger) SetStageTimeouts(transferID string, timeouts []time.Duration) error {
	return l.SetStageTimeoutsCtx(context.Background(), transferID, timeouts)
}

// SetStageTimeoutsCtx is SetStageTimeouts with per-call audit actor
// attribution (LG-73). Timeouts install silently like stages do —
// the timeout decision is disclosed on the review record, the
// reconcile report, and the timeout event itself when one fires.
func (l *Ledger) SetStageTimeoutsCtx(ctx context.Context, transferID string, timeouts []time.Duration) error {
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
	if p.Timeouts != nil {
		return ErrStagedTimeoutsAlreadySet
	}
	if len(timeouts) != len(p.Stages) {
		return ErrInvalidStageTimeouts
	}
	for _, d := range timeouts {
		if d < 0 {
			return ErrInvalidStageTimeouts
		}
	}
	p.Timeouts = append([]time.Duration(nil), timeouts...)
	p.StageStartedAt = now
	l.reviews[transferID] = r
	return nil
}

// StagedTimeouts reports the per-stage lifetimes installed on a
// review, or false when the review carries no stages or no timeouts
// (or does not exist). The returned slice is a copy; mutating it
// does not affect the ledger.
func (l *Ledger) StagedTimeouts(transferID string) ([]time.Duration, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	r, ok := l.reviews[transferID]
	if !ok || r.StagedApproval == nil || r.StagedApproval.Timeouts == nil {
		return nil, false
	}
	return append([]time.Duration(nil), r.StagedApproval.Timeouts...), true
}

// stagedStageTimedOut reports whether the review's current staged
// stage has lapsed: the review carries stages with timeouts, the
// current stage has a positive timeout, the stage clock has started,
// and now is at or past the stage start plus the timeout. Timeout
// evaluation is lazy by predicate — a timed-out stage already counts
// as inactive for available-balance purposes, even before
// ExpireReviews sweeps it. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) stagedStageTimedOut(r *TransferReview, now time.Time) bool {
	p := r.StagedApproval
	if p == nil || p.Timeouts == nil {
		return false
	}
	if p.Stage < 0 || p.Stage >= len(p.Stages) || p.Stage >= len(p.Timeouts) {
		return false
	}
	timeout := p.Timeouts[p.Stage]
	if timeout <= 0 || p.StageStartedAt.IsZero() {
		return false
	}
	return !now.Before(p.StageStartedAt.Add(timeout))
}

// timeoutStagedStageLocked marks one review rejected for a lapsed
// staged stage, releasing its frozen funds. The decided approvers
// and the timeouts stay on the review — the trail shows who voted
// before the stage went silent. Callers must hold the write lock;
// the audit event is emitted by the caller.
func (l *Ledger) timeoutStagedStageLocked(r *TransferReview, now time.Time) {
	r.Status = ReviewStatusRejected
	r.ReviewedBy = "system"
	r.ReviewedAt = now
	l.reviews[r.TransferID] = r
}

// expireStagedStagesLocked marks every pending review whose current
// staged stage has timed out as rejected, releasing its frozen
// funds, and returns how many were marked. Reviews without stages —
// or with no timeouts installed — are untouched. Callers must hold
// the write lock; the batched audit event is emitted by the caller,
// and one review_rejected outbox envelope per review is emitted
// here, like the chain sweep.
func (l *Ledger) expireStagedStagesLocked(now time.Time) (marked int, ids []string) {
	for _, r := range l.reviews {
		if r.Status == ReviewStatusPending && l.stagedStageTimedOut(r, now) {
			l.timeoutStagedStageLocked(r, now)
			marked++
			ids = append(ids, r.TransferID)
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxReviewRejected,
				Account:     r.Transfer.From,
				AmountCents: r.Transfer.AmountCents,
				Currency:    r.Transfer.Currency,
				Details: map[string]any{
					"transfer_id": r.TransferID,
					"reviewed_by": "system",
					"reason":      "staged stage timeout",
				},
			})
		}
	}
	sort.Strings(ids)
	return marked, ids
}

// emitStagedStageTimeoutAuditLocked emits the batched staged-stage
// timeout audit event. Callers must hold l.mu.
func (l *Ledger) emitStagedStageTimeoutAuditLocked(marked int, ids []string) {
	l.emitAudit(AuditEvent{
		Op:            "transfer_review_staged_stage_timeout",
		Actor:         "ExpireReviews",
		TraceID:       fmt.Sprintf("review-staged-timeout@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"timed_out":    marked,
			"transfer_ids": ids,
			"reason":       "staged stage timeout",
		},
	})
}

// ExpireStagedReviewsAt is the explicit staged sweep with a
// caller-supplied timestamp, so tests and operators can pin timeout
// deterministically: it marks every pending staged review whose
// current stage has lapsed as rejected, and — since LG-226 — every
// pending staged review whose overall deadline has lapsed, and
// returns how many were marked in total. A review rejected by the
// stage sweep is no longer pending for the deadline sweep, so the
// two bounds never double-handle a review; each bound emits its own
// batched audit event. It is the staged counterpart of the
// chain-timeout half of ExpireReviewsAt; ExpireReviewsAt itself runs
// both staged sweeps too, so operators normally need only the one
// entry point.
func (l *Ledger) ExpireStagedReviewsAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked, ids := l.expireStagedStagesLocked(now)
	if marked > 0 {
		l.emitStagedStageTimeoutAuditLocked(marked, ids)
	}
	if expired, expiredIDs := l.expireStagedDeadlinesLocked(now); expired > 0 {
		l.emitStagedDeadlineExpiredAuditLocked(expired, expiredIDs)
		marked += expired
	}
	return marked
}
