package ledger

import (
	"context"
	"testing"
)

// batchSeed funds payer and arms a global review threshold, returning a
// ledger plus two pending reviews created by threshold-crossing
// transfers.
func batchSeed(t *testing.T) (*Ledger, []string) {
	t.Helper()
	l := reviewSeed(t, 10000)
	ids := []string{"tx-b1", "tx-b2"}
	for _, id := range ids {
		if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
			t.Fatalf("PostTransfer %q: %v", id, err)
		}
	}
	return l, ids
}

func TestApproveReviewsSettlesAll(t *testing.T) {
	l, ids := batchSeed(t)

	report := l.ApproveReviews(ids, "op-alice")
	if report.ReviewedBy != "op-alice" {
		t.Errorf("ReviewedBy = %q, want op-alice", report.ReviewedBy)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(report.Results))
	}
	for i, res := range report.Results {
		if res.TransferID != ids[i] {
			t.Errorf("results[%d].TransferID = %q, want %q", i, res.TransferID, ids[i])
		}
		if res.Outcome != ReviewBatchOutcomeSettled {
			t.Errorf("results[%d].Outcome = %q, want %q", i, res.Outcome, ReviewBatchOutcomeSettled)
		}
		if res.Receipt == nil {
			t.Errorf("results[%d].Receipt is nil", i)
			continue
		}
		if res.Receipt.ReviewStatus != ReviewStatusApproved {
			t.Errorf("results[%d] receipt status = %q, want approved", i, res.Receipt.ReviewStatus)
		}
		if res.Receipt.Duplicate {
			t.Errorf("results[%d] receipt has Duplicate = true on first approval", i)
		}
	}
	if report.Settled != 2 || report.Duplicates != 0 || report.VotesRecorded != 0 || report.Failed != 0 {
		t.Errorf("counts = %+v, want settled=2 rest 0", report)
	}

	// Both settled: balances moved twice.
	if got := l.Balance("payer"); got != 100000-60000 {
		t.Errorf("payer balance = %d, want %d", got, 100000-60000)
	}
	for _, id := range ids {
		if rev, ok := l.GetTransferReview(id); !ok || rev.Status != ReviewStatusApproved {
			t.Errorf("review %q = %+v, want approved", id, rev)
		}
	}
}

func TestApproveReviewsFailureIsolation(t *testing.T) {
	l, ids := batchSeed(t)

	// Unknown ID first, rejected-but-pending review, then a good one:
	// the failures must not stop the remaining items.
	if _, _, err := l.RejectTransferReview(ids[1], "op-bob", "suspicious"); err != nil {
		t.Fatalf("RejectTransferReview: %v", err)
	}
	report := l.ApproveReviews([]string{"nope", ids[1], ids[0]}, "op-alice")
	if len(report.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(report.Results))
	}
	if report.Results[0].Outcome != ReviewBatchOutcomeFailed {
		t.Errorf("unknown ID outcome = %q, want failed", report.Results[0].Outcome)
	}
	if report.Results[0].Error == "" {
		t.Error("unknown ID result has no error text")
	}
	if report.Results[0].Receipt != nil {
		t.Error("failed result carries a receipt")
	}
	if report.Results[1].Outcome != ReviewBatchOutcomeFailed {
		t.Errorf("rejected review outcome = %q, want failed", report.Results[1].Outcome)
	}
	if report.Results[2].Outcome != ReviewBatchOutcomeSettled {
		t.Errorf("good item outcome = %q, want settled", report.Results[2].Outcome)
	}
	if report.Failed != 2 || report.Settled != 1 {
		t.Errorf("counts = %+v, want failed=2 settled=1", report)
	}
	if got := l.Balance("payer"); got != 100000-30000 {
		t.Errorf("payer balance = %d, want %d (only the good item settled)", got, 100000-30000)
	}
}

func TestApproveReviewsIdempotentReplay(t *testing.T) {
	l, ids := batchSeed(t)

	first := l.ApproveReviews(ids, "op-alice")
	if first.Settled != 2 {
		t.Fatalf("first batch settled = %d, want 2", first.Settled)
	}
	second := l.ApproveReviews(ids, "op-bob")
	if second.Duplicates != 2 {
		t.Fatalf("second batch duplicates = %d, want 2", second.Duplicates)
	}
	for i, res := range second.Results {
		if res.Outcome != ReviewBatchOutcomeDuplicate {
			t.Errorf("results[%d].Outcome = %q, want duplicate", i, res.Outcome)
		}
		if res.Receipt == nil || !res.Receipt.Duplicate {
			t.Errorf("results[%d] receipt is not the duplicate original", i)
		}
	}
	// No double-settlement: the batch re-approval booked nothing new.
	if got := l.Balance("payer"); got != 100000-60000 {
		t.Errorf("payer balance after replay = %d, want %d", got, 100000-60000)
	}
}

func TestApproveReviewsQuorumVotes(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-qb")

	// Threshold 2 of 3: the first batch vote is recorded, not settled.
	one := l.ApproveReviews([]string{"tx-qb"}, "alice")
	if one.VotesRecorded != 1 {
		t.Fatalf("first batch votes_recorded = %d, want 1", one.VotesRecorded)
	}
	res := one.Results[0]
	if res.Outcome != ReviewBatchOutcomeVoteRecorded {
		t.Fatalf("outcome = %q, want vote_recorded", res.Outcome)
	}
	if res.Receipt == nil || res.Receipt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("vote receipt = %+v, want pending", res.Receipt)
	}

	// The second vote reaches quorum and settles through the batch.
	two := l.ApproveReviews([]string{"tx-qb"}, "bob")
	if two.Settled != 1 {
		t.Fatalf("second batch settled = %d, want 1", two.Settled)
	}
	if two.Results[0].Receipt.QuorumTally == nil {
		t.Error("settling receipt carries no quorum tally")
	}
	if got := l.Balance("payer"); got != 100000-30000 {
		t.Errorf("payer balance = %d, want %d", got, 100000-30000)
	}
}

func TestApproveReviewsChainStageVote(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-cb")

	// Stage 1 (alice/bob, threshold 1): one vote records, the review
	// stays pending for stage 2 (carol).
	one := l.ApproveReviews([]string{"tx-cb"}, "alice")
	if one.VotesRecorded != 1 || one.Results[0].Outcome != ReviewBatchOutcomeVoteRecorded {
		t.Fatalf("first batch = %+v, want one vote_recorded", one.Results)
	}
	if rev, ok := l.GetTransferReview("tx-cb"); !ok || rev.Status != ReviewStatusPending {
		t.Fatalf("review = %+v, want pending", rev)
	}

	// Stage 2 (carol) settles.
	two := l.ApproveReviews([]string{"tx-cb"}, "carol")
	if two.Settled != 1 {
		t.Fatalf("second batch settled = %d, want 1", two.Settled)
	}
}

func TestApproveReviewsEmptyIsNoop(t *testing.T) {
	l := reviewSeed(t, 10000)
	versionBefore := l.Version()

	report := l.ApproveReviews(nil, "op-alice")
	if report.ReviewedBy != "op-alice" {
		t.Errorf("ReviewedBy = %q, want op-alice", report.ReviewedBy)
	}
	if len(report.Results) != 0 {
		t.Errorf("empty batch results = %d, want 0", len(report.Results))
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved on empty batch: %d -> %d", versionBefore, l.Version())
	}
}

func TestApproveReviewsDefaultReviewer(t *testing.T) {
	l, ids := batchSeed(t)
	report := l.ApproveReviews(ids, "")
	if report.ReviewedBy != "operator" {
		t.Errorf("ReviewedBy = %q, want operator", report.ReviewedBy)
	}
	if report.Settled != 2 {
		t.Errorf("settled = %d, want 2", report.Settled)
	}
}

func TestApproveReviewsEnvelopeAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	for _, id := range []string{"tx-e1", "tx-e2"} {
		if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
			t.Fatalf("PostTransfer %q: %v", id, err)
		}
	}
	ctx := ContextWithActor(context.Background(), "ops")
	report := l.ApproveReviewsCtx(ctx, []string{"tx-e1", "nope"}, "op-alice")
	if report.Settled != 1 || report.Failed != 1 {
		t.Fatalf("report = %+v, want settled=1 failed=1", report)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	byOp := auditEventsByOp(t, dir)
	env := byOp["transfer_reviews_batch_approved"]
	if len(env) != 1 {
		t.Fatalf("envelope events = %d, want 1", len(env))
	}
	if env[0].Actor != "ops" {
		t.Errorf("envelope actor = %q, want ops (ctx attribution)", env[0].Actor)
	}
	d := env[0].Details
	if d["reviewed_by"] != "op-alice" {
		t.Errorf("details reviewed_by = %v, want op-alice", d["reviewed_by"])
	}
	if d["settled"] != float64(1) || d["failed"] != float64(1) {
		t.Errorf("details counts = %v, want settled=1 failed=1", d)
	}
	items, ok := d["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("details items = %v, want 2 items", d["items"])
	}
	if len(byOp["transfer_review_approved"]) != 1 {
		t.Errorf("per-item approved events = %d, want 1", len(byOp["transfer_review_approved"]))
	}
}

func TestApproveReviewsEnvelopeEvenOnTotalFailure(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	report := l.ApproveReviews([]string{"ghost-1", "ghost-2"}, "op-alice")
	if report.Failed != 2 {
		t.Fatalf("report = %+v, want failed=2", report)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if len(byOp["transfer_reviews_batch_approved"]) != 1 {
		t.Fatalf("envelope events = %d, want 1 (rejected batches are recorded)", len(byOp["transfer_reviews_batch_approved"]))
	}
}
