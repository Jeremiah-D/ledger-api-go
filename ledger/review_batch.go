// Review batch approvals: one operator call to approve many pending
// transfer reviews (LG-149).
//
// ApproveReviews(reviewIDs, reviewer) settles (or votes on) each listed
// review through the same per-review path the single-review endpoint
// uses (see ApproveTransferReviewCtx): the LG-42 flat path settles
// directly, an active N-of-M quorum policy (LG-124) records a named
// vote, and an active sequential approval chain (LG-126) records a
// stage vote. Every item resolves independently — the batch never
// aborts early: an unknown transfer ID, a review that is no longer
// pending, or a risk-control refusal (frozen account, overdraft,
// closed period) fails only its own item, and the remaining reviews
// are still attempted. Each item carries the same idempotency as the
// single-review endpoint: re-running the batch re-approves nothing
// new, and every settled item keeps its original approval receipt.
//
// A batch is one operator action, so besides the per-item audit
// events it emits one envelope event,
// transfer_reviews_batch_approved, that lists every item's outcome —
// the compliance trail can read the batch as a unit without replaying
// the individual approvals. The envelope is emitted even when every
// item failed: a rejected batch attempt is itself a compliance fact.
// An empty batch is a no-op: no audit event, no version change.
package ledger

import (
	"context"
	"fmt"
)

// Review batch item outcomes.
const (
	// ReviewBatchOutcomeSettled marks an item the batch settled (or a
	// re-approval of an already-settled review, which keeps the
	// original receipt with Duplicate set).
	ReviewBatchOutcomeSettled = "settled"
	// ReviewBatchOutcomeDuplicate marks an idempotent replay of a
	// settled item: nothing new was booked, the original receipt is
	// returned with Duplicate set.
	ReviewBatchOutcomeDuplicate = "duplicate"
	// ReviewBatchOutcomeVoteRecorded marks a named quorum or approval-
	// chain vote that did not settle yet — the receipt stays pending.
	ReviewBatchOutcomeVoteRecorded = "vote_recorded"
	// ReviewBatchOutcomeFailed marks an item the batch could not move:
	// unknown transfer ID, non-pending review, unauthorized voter, or
	// a risk-control refusal. Error carries the reason.
	ReviewBatchOutcomeFailed = "failed"
)

// ReviewBatchItemResult is one item's outcome in an ApproveReviews
// batch: either a receipt (settled, duplicate replay, or recorded
// quorum/chain vote) or a failure describing why the item did not move.
type ReviewBatchItemResult struct {
	TransferID string `json:"transfer_id"`
	// Outcome is settled | duplicate | vote_recorded | failed.
	Outcome string `json:"outcome"`
	// Receipt is the per-item receipt when Outcome is not failed.
	Receipt *TransferReceipt `json:"receipt,omitempty"`
	// Error carries the failure reason when Outcome is failed.
	Error string `json:"error,omitempty"`
}

// ReviewBatchReport is the full outcome of one ApproveReviews call:
// per-item results in the input review-ID order, plus outcome counts
// for the operator.
type ReviewBatchReport struct {
	ReviewedBy   string                  `json:"reviewed_by"`
	Results      []ReviewBatchItemResult `json:"results"`
	Settled      int                     `json:"settled"`
	Duplicates   int                     `json:"duplicates"`
	VotesRecorded int                    `json:"votes_recorded"`
	Failed       int                     `json:"failed"`
}

// ApproveReviews approves every listed transfer review through the
// single-review approve path, returning per-item results in input
// order. See the package doc above for failure isolation, per-item
// idempotency, and the batch envelope audit event. An empty reviewer
// is recorded as "operator".
func (l *Ledger) ApproveReviews(reviewIDs []string, reviewer string) ReviewBatchReport {
	return l.ApproveReviewsCtx(context.Background(), reviewIDs, reviewer)
}

// ApproveReviewsCtx is ApproveReviews with per-call audit actor
// attribution (LG-73): the batch envelope event records the operator
// carried by ctx when set.
func (l *Ledger) ApproveReviewsCtx(ctx context.Context, reviewIDs []string, reviewer string) ReviewBatchReport {
	if reviewer == "" {
		reviewer = defaultReviewOperator
	}
	report := ReviewBatchReport{ReviewedBy: reviewer}
	if len(reviewIDs) == 0 {
		return report
	}
	for _, id := range reviewIDs {
		receipt, err := l.ApproveTransferReviewCtx(ctx, id, reviewer)
		res := ReviewBatchItemResult{TransferID: id}
		switch {
		case err != nil:
			res.Outcome = ReviewBatchOutcomeFailed
			res.Error = err.Error()
			report.Failed++
		case receipt.Duplicate && receipt.ReviewStatus == ReviewStatusApproved:
			// Idempotent replay of a settled review: the single-review
			// path returns the original receipt with Duplicate set.
			res.Outcome = ReviewBatchOutcomeDuplicate
			res.Receipt = &receipt
			report.Duplicates++
		case receipt.ReviewStatus == ReviewStatusApproved:
			res.Outcome = ReviewBatchOutcomeSettled
			res.Receipt = &receipt
			report.Settled++
		default:
			// A named quorum or approval-chain vote that did not
			// settle yet: the receipt stays pending (an idempotent
			// replay of an already-recorded vote lands here too).
			res.Outcome = ReviewBatchOutcomeVoteRecorded
			res.Receipt = &receipt
			report.VotesRecorded++
		}
		report.Results = append(report.Results, res)
	}
	// The envelope audit event: one batch-level record in the audit
	// trail, on top of the per-item approve events. It is a structural
	// observation, not a balance change, so the version does not move
	// (the per-item events already bumped it). Requires the lock, as
	// emitAuditCtx does.
	l.mu.Lock()
	items := make([]map[string]any, 0, len(report.Results))
	for _, r := range report.Results {
		item := map[string]any{"transfer_id": r.TransferID, "outcome": r.Outcome}
		if r.Error != "" {
			item["error"] = r.Error
		}
		items = append(items, item)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_reviews_batch_approved",
		Actor:         "ApproveReviews",
		TraceID:       fmt.Sprintf("review-batch-approve@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"reviewed_by":    reviewer,
			"settled":        report.Settled,
			"duplicates":     report.Duplicates,
			"votes_recorded": report.VotesRecorded,
			"failed":         report.Failed,
			"items":          items,
		},
	})
	l.mu.Unlock()
	return report
}
