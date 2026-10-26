package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// stagedSeed returns a ledger with a 10000-cent review threshold and
// the payer funded with 100000 cents, plus a 30000-cent transfer
// frozen into pending review.
func stagedSeed(t *testing.T, id string) *Ledger {
	t.Helper()
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer %q: %v", id, err)
	}
	rev, ok := l.GetTransferReview(id)
	if !ok || rev.Status != ReviewStatusPending {
		t.Fatalf("transfer %q review = %+v, want pending", id, rev.Status)
	}
	return l
}

var stagedRoles = []ApprovalStage{{Role: "manager"}, {Role: "finance"}, {Role: "treasury"}}

func TestStagedApprovalValidation(t *testing.T) {
	l := stagedSeed(t, "tx-s1")
	if err := l.SetApprovalStages("tx-unknown", stagedRoles); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrReviewNotFound", err)
	}
	if err := l.SetApprovalStages("tx-s1", nil); !errors.Is(err, ErrInvalidApprovalStages) {
		t.Errorf("no stages: err = %v, want ErrInvalidApprovalStages", err)
	}
	if err := l.SetApprovalStages("tx-s1", []ApprovalStage{{Role: "  "}}); !errors.Is(err, ErrInvalidApprovalStages) {
		t.Errorf("blank role: err = %v, want ErrInvalidApprovalStages", err)
	}
	if err := l.SetApprovalStages("tx-s1", stagedRoles); err != nil {
		t.Fatalf("valid install: %v", err)
	}
	if err := l.SetApprovalStages("tx-s1", stagedRoles); !errors.Is(err, ErrStagedApprovalAlreadySet) {
		t.Errorf("second install: err = %v, want ErrStagedApprovalAlreadySet", err)
	}
	if _, ok := l.StagedApproval("tx-s1"); !ok {
		t.Error("StagedApproval accessor: not found, want installed")
	}
	// A review that is no longer pending refuses installation.
	l2 := stagedSeed(t, "tx-s2")
	if _, err := l2.ApproveTransferReview("tx-s2", "alice"); err != nil {
		t.Fatalf("flat approve: %v", err)
	}
	if err := l2.SetApprovalStages("tx-s2", stagedRoles); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("settled review: err = %v, want ErrReviewNotPending", err)
	}
}

func TestStagedApprovalExplicitEntryNeedsStages(t *testing.T) {
	l := stagedSeed(t, "tx-s3")
	if _, err := l.ApproveStagedReview("tx-s3", "alice"); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged approve: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
	if _, _, err := l.RejectStagedReview("tx-s3", "alice", ""); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged reject: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
}

func TestStagedApprovalFlow(t *testing.T) {
	l := stagedSeed(t, "tx-s4")
	if err := l.SetApprovalStages("tx-s4", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	// Stage 0 through the explicit entry point: advances, never settles.
	receipt, err := l.ApproveStagedReview("tx-s4", "alice")
	if err != nil {
		t.Fatalf("stage 0: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending || receipt.StagedTally == nil {
		t.Fatalf("stage 0 receipt = %+v, want pending with staged tally", receipt)
	}
	if receipt.StagedTally.Stage != 1 || receipt.StagedTally.Role != "finance" || receipt.StagedTally.Outcome != StagedOutcomeAdvanced {
		t.Errorf("stage 0 tally = %+v, want stage 1 finance advanced", receipt.StagedTally)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("after stage 0: payee balance = %d, want 0 (unsettled)", got)
	}
	rev, _ := l.GetTransferReview("tx-s4")
	if rev.Status != ReviewStatusPending {
		t.Errorf("after stage 0: review status = %q, want pending", rev.Status)
	}
	// The same approver may not sign the adjacent stage.
	if _, err := l.ApproveTransferReview("tx-s4", "alice"); !errors.Is(err, ErrStagedApproverRepeated) {
		t.Errorf("adjacent repeat: err = %v, want ErrStagedApproverRepeated", err)
	}
	// Stage 1 through the flat entry point: it routes into the stage vote.
	if _, err := l.ApproveTransferReview("tx-s4", "bob"); err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("after stage 1: payee balance = %d, want 0 (still unsettled)", got)
	}
	// Last stage settles exactly like a single-operator approval.
	receipt, err = l.ApproveTransferReview("tx-s4", "carol")
	if err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("stage 2 receipt status = %q, want approved", receipt.ReviewStatus)
	}
	if got := l.Balance("payer"); got != 70000 {
		t.Errorf("settled payer balance = %d, want 70000", got)
	}
	if got := l.Balance("payee"); got != 30000 {
		t.Errorf("settled payee balance = %d, want 30000", got)
	}
	rev, _ = l.GetTransferReview("tx-s4")
	if rev.Status != ReviewStatusApproved || rev.ReviewedBy != "alice,bob,carol" {
		t.Errorf("settled review = status %q reviewed_by %q, want approved alice,bob,carol", rev.Status, rev.ReviewedBy)
	}
	if rev.StagedApproval == nil || len(rev.StagedApproval.Approvers) != 3 {
		t.Errorf("settled review staged progress = %+v, want 3 approvers", rev.StagedApproval)
	}
	// Re-approving returns the original receipt idempotently.
	dup, err := l.ApproveTransferReview("tx-s4", "carol")
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if !dup.Duplicate || dup.TransferID != "tx-s4" {
		t.Errorf("re-approve receipt = %+v, want duplicate", dup)
	}
}

func TestStagedApprovalRejectEndsAtAnyStage(t *testing.T) {
	l := stagedSeed(t, "tx-s5")
	if err := l.SetApprovalStages("tx-s5", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-s5", "alice"); err != nil {
		t.Fatalf("stage 0: %v", err)
	}
	// A reject at stage 1 closes the review at once, through the
	// flat entry point.
	review, changed, err := l.RejectTransferReview("tx-s5", "bob", "amount disputed")
	if err != nil || !changed {
		t.Fatalf("reject: review=%v changed=%v err=%v", review, changed, err)
	}
	if review.Status != ReviewStatusRejected {
		t.Errorf("rejected review status = %q, want rejected", review.Status)
	}
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("after reject: payer available = %d, want 100000 (freeze released)", got)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("after reject: payee balance = %d, want 0", got)
	}
	if _, err := l.ApproveTransferReview("tx-s5", "carol"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("approve after reject: err = %v, want ErrReviewNotPending", err)
	}
}

func TestStagedApprovalConflictsWithOtherSchemes(t *testing.T) {
	// A review that already carries quorum votes belongs to the quorum.
	l := quorumSeed(t, false)
	quorumTransfer(t, l, "tx-s6")
	if _, err := l.ApproveTransferReview("tx-s6", "alice"); err != nil {
		t.Fatalf("quorum vote: %v", err)
	}
	if err := l.SetApprovalStages("tx-s6", stagedRoles); !errors.Is(err, ErrStagedApprovalConflict) {
		t.Errorf("quorum-voted review: err = %v, want ErrStagedApprovalConflict", err)
	}
	// A review born under an active chain carries chain progress.
	l2 := reviewSeed(t, 10000)
	if err := l2.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{{Approvers: []string{"alice"}, ThresholdN: 1}}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	if _, err := l2.PostTransfer(Transfer{ID: "tx-s7", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l2.SetApprovalStages("tx-s7", stagedRoles); !errors.Is(err, ErrStagedApprovalConflict) {
		t.Errorf("chain-born review: err = %v, want ErrStagedApprovalConflict", err)
	}
}

func TestStagedApprovalAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-s8", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-s8", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.ApproveStagedReviewCtx(ctx, "tx-s8", "alice"); err != nil {
		t.Fatalf("stage 0: %v", err)
	}
	if _, err := l.ApproveTransferReviewCtx(ctx, "tx-s8", "bob"); err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	votes := byOp["transfer_review_staged_vote"]
	if len(votes) != 2 {
		t.Fatalf("staged vote events = %d, want 2", len(votes))
	}
	if votes[0].Details["voter"] != "alice" || votes[0].Details["role"] != "manager" || votes[0].Details["vote"] != "approve" {
		t.Errorf("vote event 1 = %+v", votes[0].Details)
	}
	if votes[0].Actor != "ops-desk" {
		t.Errorf("vote event actor = %q, want ops-desk", votes[0].Actor)
	}
	if advanced := byOp["transfer_review_staged_stage_advanced"]; len(advanced) != 1 {
		t.Errorf("stage-advanced events = %d, want 1", len(advanced))
	} else if advanced[0].Details["from_stage"] != float64(0) || advanced[0].Details["to_stage"] != float64(1) {
		t.Errorf("stage-advanced event = %+v", advanced[0].Details)
	}
	if got := byOp["transfer_review_approved"]; len(got) != 1 {
		t.Errorf("approved events = %d, want 1", len(got))
	}
}

func TestStagedApprovalRecordSurvivesJSON(t *testing.T) {
	l := stagedSeed(t, "tx-s9")
	if err := l.SetApprovalStages("tx-s9", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-s9", "alice"); err != nil {
		t.Fatalf("stage 0: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-s9")
	raw, err := json.Marshal(rev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back TransferReview
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !stagedApprovalEqual(rev.StagedApproval, back.StagedApproval) {
		t.Errorf("round-trip staged progress = %+v, want %+v", back.StagedApproval, rev.StagedApproval)
	}
	// The accessor returns a copy: mutating it must not affect the ledger.
	mut, _ := l.StagedApproval("tx-s9")
	mut.Approvers[0] = "mallory"
	again, _ := l.StagedApproval("tx-s9")
	if again.Approvers[0] != "alice" {
		t.Errorf("accessor copy leaked mutation: %v", again.Approvers)
	}
}
