package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Overall deadline for per-transfer staged approval (LG-226).
//
// LG-223 already gives each staged stage its own lifetime; this
// module is the orthogonal overall bound: SetStagedReviewDeadline
// installs one opt-in deadline for the whole staged review, and a
// pending staged review past that deadline is auto-rejected —
// regardless of which stage it sits on, never silently approved or
// advanced. The two bounds compose without interfering: a stage
// timeout rejects on its own rung with
// transfer_review_staged_stage_timeout, the overall deadline rejects
// on any rung with transfer_review_staged_deadline_expired, and a
// review carrying neither bound behaves exactly as LG-221 alone
// describes. Stage order and voting rules are untouched.
//
// Semantics, mirroring the stage timeout:
//   - The deadline is opt-in and lives on the staged-approval
//     progress record (StagedApprovalProgress.Deadline, zero when
//     unset): it only ever applies to a review with stages
//     installed, and installing it on a review without stages fails
//     with ErrStagedApprovalNotConfigured. Re-setting replaces the
//     previous deadline — an operator may extend the bound before it
//     lapses — but the new deadline must not be earlier than the
//     install instant: a past deadline is rejected and the original
//     state is untouched (a zero deadline is past, so it fails too).
//   - Deadline evaluation is lazy by predicate (a lapsed review
//     already counts as inactive for available-balance purposes, via
//     reviewActiveLocked) plus an explicit sweep in ExpireReviews
//     and in ExpireStagedReviewsAt — the staged sweep already named
//     by LG-223 now covers both bounds, so operators keep one entry
//     point and the two sweeps never double-handle a review (a
//     review rejected by the stage sweep is no longer pending when
//     the deadline sweep runs). The lazy approve path rejects first
//     and reports ErrReviewExpired, like the stage-timeout path.
//   - A deadline rejection is a rejection, not an expiry: the review
//     status becomes rejected with ReviewedBy "system", the staged
//     progress (stages, decided approvers, timeouts, deadline) stays
//     on the record, and the audit event
//     transfer_review_staged_deadline_expired carries the stage, the
//     deadline, and the reason "staged approval deadline expired" —
//     so the "which rung was open when the clock ran out" trail is
//     tamper-evident. The sweep additionally emits one
//     review_rejected outbox envelope per lapsed review, like the
//     stage sweep. The deadline never bumps the ledger version: the
//     frozen legs move no money, they only stop reserving funds.
//   - A manual reject at a lapsed deadline still rejects (the manual
//     reviewer is recorded instead of "system"): both endings are
//     terminal rejections, so a late human decision cannot be
//     mistaken for an advance, and the deadline path never has to win
//     a race against an operator who is already deciding.

// Staged deadline errors.
var (
	// ErrInvalidStagedReviewDeadline is returned by
	// SetStagedReviewDeadline when the deadline is earlier than the
	// install instant (a past deadline, including zero).
	ErrInvalidStagedReviewDeadline = errors.New("ledger: invalid staged approval review deadline")
)

// SetStagedReviewDeadline installs one opt-in overall deadline on a
// pending staged review. The review must carry stages (otherwise
// ErrStagedApprovalNotConfigured) and must still be pending, and
// the deadline must not be earlier than the install instant — a
// past deadline fails and leaves the review untouched. Re-setting
// replaces the previous deadline. Installing is structural — it does
// not bump the ledger version, like installing the stages and the
// per-stage timeouts themselves.
func (l *Ledger) SetStagedReviewDeadline(transferID string, deadline time.Time) error {
	return l.SetStagedReviewDeadlineCtx(context.Background(), transferID, deadline)
}

// SetStagedReviewDeadlineCtx is SetStagedReviewDeadline with per-call
// audit actor attribution (LG-73). The deadline installs silently
// like stages and timeouts do — it is disclosed on the review
// record, the reconcile report, and the deadline event itself when
// one fires.
func (l *Ledger) SetStagedReviewDeadlineCtx(ctx context.Context, transferID string, deadline time.Time) error {
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
	if deadline.Before(now) {
		return ErrInvalidStagedReviewDeadline
	}
	p.Deadline = deadline
	l.reviews[transferID] = r
	return nil
}

// StagedReviewDeadline reports the overall deadline installed on a
// review, or false when the review carries no stages or no deadline
// (or does not exist). The returned value is a copy; it carries no
// mutable state, so no copy discipline is needed beyond the value
// itself.
func (l *Ledger) StagedReviewDeadline(transferID string) (time.Time, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	r, ok := l.reviews[transferID]
	if !ok || r.StagedApproval == nil || r.StagedApproval.Deadline.IsZero() {
		return time.Time{}, false
	}
	return r.StagedApproval.Deadline, true
}

// stagedDeadlineExpired reports whether the review's overall staged
// deadline has lapsed: the review carries stages with a deadline
// installed, and now is at or past it — regardless of which stage
// the review sits on. Deadline evaluation is lazy by predicate — a
// lapsed review already counts as inactive for available-balance
// purposes, even before a sweep runs. Callers must hold l.mu (either
// lock suffices).
func (l *Ledger) stagedDeadlineExpired(r *TransferReview, now time.Time) bool {
	p := r.StagedApproval
	if p == nil || p.Deadline.IsZero() {
		return false
	}
	return !now.Before(p.Deadline)
}

// expireStagedDeadlinesLocked marks every pending review whose
// overall staged deadline has lapsed as rejected, releasing its
// frozen funds, and returns how many were marked. Reviews without
// stages — or with no deadline installed — are untouched. Callers
// must hold the write lock; the batched audit event is emitted by
// the caller, and one review_rejected outbox envelope per review is
// emitted here, like the stage sweep.
func (l *Ledger) expireStagedDeadlinesLocked(now time.Time) (marked int, ids []string) {
	for _, r := range l.reviews {
		if r.Status == ReviewStatusPending && l.stagedDeadlineExpired(r, now) {
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
					"reason":      "staged approval deadline expired",
				},
			})
		}
	}
	sort.Strings(ids)
	return marked, ids
}

// emitStagedDeadlineExpiredAuditLocked emits the batched staged
// overall-deadline audit event. Callers must hold l.mu.
func (l *Ledger) emitStagedDeadlineExpiredAuditLocked(marked int, ids []string) {
	l.emitAudit(AuditEvent{
		Op:            "transfer_review_staged_deadline_expired",
		Actor:         "ExpireReviews",
		TraceID:       fmt.Sprintf("review-staged-deadline@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"deadline_expired": marked,
			"transfer_ids":     ids,
			"reason":           "staged approval deadline expired",
		},
	})
}
