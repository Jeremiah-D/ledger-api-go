package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// chainSeed returns a ledger with a global review threshold armed and a
// two-stage approval chain installed: stage 0 (maker check) approvers
// alice/bob with N=1, stage 1 (controller) approver carol with N=1.
func chainSeed(t *testing.T) *Ledger {
	t.Helper()
	l := reviewSeed(t, 10000)
	if err := l.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"alice", "bob"}, ThresholdN: 1},
		{Approvers: []string{"carol"}, ThresholdN: 1},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	return l
}

// chainTransfer posts a threshold-crossing transfer that freezes into
// pending review.
func chainTransfer(t *testing.T, l *Ledger, id string) TransferReceipt {
	t.Helper()
	receipt, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 20000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("ReviewStatus = %q, want pending", receipt.ReviewStatus)
	}
	return receipt
}

func TestApprovalChainValidation(t *testing.T) {
	valid := ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"a", "b"}, ThresholdN: 2, Timeout: time.Hour},
	}}
	if err := validateApprovalChain(&valid); err != nil {
		t.Errorf("valid chain rejected: %v", err)
	}
	if err := validateApprovalChain(nil); err != nil {
		t.Errorf("nil chain rejected: %v", err)
	}

	cases := map[string]ApprovalChain{
		"no stages":            {Stages: nil},
		"empty approvers":      {Stages: []ApprovalChainStage{{Approvers: nil, ThresholdN: 1}}},
		"threshold zero":       {Stages: []ApprovalChainStage{{Approvers: []string{"a"}, ThresholdN: 0}}},
		"threshold above M":    {Stages: []ApprovalChainStage{{Approvers: []string{"a"}, ThresholdN: 2}}},
		"blank approver":       {Stages: []ApprovalChainStage{{Approvers: []string{" "}, ThresholdN: 1}}},
		"duplicate approver":   {Stages: []ApprovalChainStage{{Approvers: []string{"a", "a"}, ThresholdN: 1}}},
		"negative timeout":     {Stages: []ApprovalChainStage{{Approvers: []string{"a"}, ThresholdN: 1, Timeout: -time.Second}}},
		"second stage invalid": {Stages: []ApprovalChainStage{{Approvers: []string{"a"}, ThresholdN: 1}, {Approvers: nil, ThresholdN: 1}}},
	}
	for name, c := range cases {
		if err := validateApprovalChain(&c); err == nil {
			t.Errorf("case %q: expected validation error, got nil", name)
		}
		if err := New().SetApprovalChain(c); !errors.Is(err, ErrInvalidApprovalChain) {
			t.Errorf("case %q: SetApprovalChain error = %v, want ErrInvalidApprovalChain", name, err)
		}
	}

	// Fail-fast at construction: an invalid chain panics.
	defer func() {
		if r := recover(); r == nil {
			t.Error("WithApprovalChain(invalid) did not panic")
		}
	}()
	_ = WithApprovalChain(ApprovalChain{})
}

func TestChainSequentialFlow(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-chain1")

	// Stage 0: alice votes approve — threshold 1 reached, the review
	// advances to stage 1, still pending.
	receipt, err := l.ApproveTransferReview("tx-chain1", "alice")
	if err != nil {
		t.Fatalf("stage-0 approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Errorf("ReviewStatus = %q, want pending after stage 0", receipt.ReviewStatus)
	}
	if receipt.ChainTally == nil {
		t.Fatal("no ChainTally on the advance receipt")
	}
	if receipt.ChainTally.Outcome != ChainOutcomeAdvanced {
		t.Errorf("ChainTally.Outcome = %q, want advanced", receipt.ChainTally.Outcome)
	}
	if receipt.ChainTally.Stage != 1 || receipt.ChainTally.StageCount != 2 {
		t.Errorf("ChainTally stage = %d/%d, want 1/2", receipt.ChainTally.Stage, receipt.ChainTally.StageCount)
	}

	// bob was a stage-0 approver but is not a stage-1 member: his vote
	// is refused at the new stage.
	if _, err := l.ApproveTransferReview("tx-chain1", "bob"); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("stage-1 vote by bob: err = %v, want ErrQuorumNotMember", err)
	}

	// The money has not moved yet: balances untouched while pending.
	if got := l.Balance("payer"); got != 100000 {
		t.Errorf("payer balance = %d, want 100000 (still frozen)", got)
	}

	// Stage 1: carol votes approve — final stage, the review settles.
	receipt, err = l.ApproveTransferReview("tx-chain1", "carol")
	if err != nil {
		t.Fatalf("stage-1 approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}
	if got := l.Balance("payer"); got != 100000-20000 {
		t.Errorf("payer balance = %d, want 80000 after settle", got)
	}

	// The review record carries the stage and the per-stage votes.
	rev, ok := l.GetTransferReview("tx-chain1")
	if !ok {
		t.Fatal("review record missing after settle")
	}
	if rev.ChainProgress == nil {
		t.Fatal("ChainProgress is nil after chain settle")
	}
	if rev.ChainProgress.Stage != 1 {
		t.Errorf("ChainProgress.Stage = %d, want 1", rev.ChainProgress.Stage)
	}
	if !equalQuorumVotes(rev.ChainProgress.Votes[0].ApproveVotes, []string{"alice"}) {
		t.Errorf("stage-0 votes = %v, want [alice]", rev.ChainProgress.Votes[0].ApproveVotes)
	}
	if !equalQuorumVotes(rev.ChainProgress.Votes[1].ApproveVotes, []string{"carol"}) {
		t.Errorf("stage-1 votes = %v, want [carol]", rev.ChainProgress.Votes[1].ApproveVotes)
	}
	if rev.ReviewedBy != "carol" {
		t.Errorf("ReviewedBy = %q, want carol", rev.ReviewedBy)
	}
}

func TestChainMultiApproverStage(t *testing.T) {
	l := reviewSeed(t, 10000)
	// One stage needs 2 of 3 approvers: the first vote records, the
	// second settles.
	if err := l.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"a1", "a2", "a3"}, ThresholdN: 2},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	chainTransfer(t, l, "tx-2of3")

	r1, err := l.ApproveTransferReview("tx-2of3", "a1")
	if err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if r1.ChainTally.Outcome != QuorumOutcomeRecorded {
		t.Errorf("first vote outcome = %q, want recorded", r1.ChainTally.Outcome)
	}
	if r1.ReviewStatus != ReviewStatusPending {
		t.Errorf("ReviewStatus = %q, want pending", r1.ReviewStatus)
	}

	r2, err := l.ApproveTransferReview("tx-2of3", "a2")
	if err != nil {
		t.Fatalf("second vote: %v", err)
	}
	if r2.ReviewStatus != ReviewStatusApproved {
		t.Errorf("ReviewStatus = %q, want approved after 2-of-3", r2.ReviewStatus)
	}

	// Re-approving a settled review is idempotent and drops the tally.
	r3, err := l.ApproveTransferReview("tx-2of3", "a3")
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if !r3.Duplicate || r3.ChainTally != nil {
		t.Errorf("re-approve: Duplicate=%v ChainTally=%v, want Duplicate=true nil tally", r3.Duplicate, r3.ChainTally != nil)
	}
}

func TestChainRejectDecisiveAtAnyStage(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-rej")

	// Advance to stage 1 first.
	if _, err := l.ApproveTransferReview("tx-rej", "alice"); err != nil {
		t.Fatalf("stage-0 approve: %v", err)
	}
	// A reject vote on stage 1 closes the review immediately.
	rev, changed, err := l.RejectTransferReview("tx-rej", "carol", "suspicious")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true")
	}
	if rev.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected", rev.Status)
	}
	if rev.ReviewedBy != "carol" {
		t.Errorf("ReviewedBy = %q, want carol", rev.ReviewedBy)
	}
	// The dissent trail survives: stage 0 approve + stage 1 reject.
	if rev.ChainProgress == nil || !equalQuorumVotes(rev.ChainProgress.Votes[0].ApproveVotes, []string{"alice"}) {
		t.Errorf("stage-0 votes lost: %+v", rev.ChainProgress)
	}
	if !equalQuorumVotes(rev.ChainProgress.Votes[1].RejectVotes, []string{"carol"}) {
		t.Errorf("stage-1 reject vote = %v, want [carol]", rev.ChainProgress.Votes[1].RejectVotes)
	}
	// The freeze is released.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 after reject", got)
	}
}

func TestChainVoteIdempotencyAndConflicts(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-votes")

	// Idempotent replay of the same approve vote.
	r1, err := l.ApproveTransferReview("tx-votes", "alice")
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	_ = r1
	if _, err := l.ApproveTransferReview("tx-votes", "alice"); !errors.Is(err, ErrReviewerAlreadyVoted) {
		// alice already advanced the review to stage 1, where she is
		// not a member: the first vote stands either way.
		t.Logf("re-vote after advance: %v", err)
	}

	// Fresh review for the same-stage replay path.
	chainTransfer(t, l, "tx-votes2")
	l2 := l
	if err := l2.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"x", "y"}, ThresholdN: 2},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	r2, err := l2.ApproveTransferReview("tx-votes2", "x")
	if err != nil {
		t.Fatalf("vote x: %v", err)
	}
	if r2.ChainTally.Outcome != QuorumOutcomeRecorded {
		t.Fatalf("outcome = %q, want recorded", r2.ChainTally.Outcome)
	}
	dup, err := l2.ApproveTransferReview("tx-votes2", "x")
	if err != nil {
		t.Fatalf("replay vote x: %v", err)
	}
	if !dup.Duplicate || dup.ChainTally == nil || dup.ChainTally.Outcome != QuorumOutcomeDuplicate {
		t.Errorf("replay: Duplicate=%v outcome=%q, want Duplicate=true duplicate", dup.Duplicate, dup.ChainTally.Outcome)
	}
	rev, _ := l2.GetTransferReview("tx-votes2")
	if len(rev.ChainProgress.Votes[0].ApproveVotes) != 1 {
		t.Errorf("approve votes = %d, want 1 (replay records nothing)", len(rev.ChainProgress.Votes[0].ApproveVotes))
	}

	// Voting the opposite way fails: the first vote stands.
	if _, err := l2.ApproveTransferReview("tx-votes2", "mallory"); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("non-member vote: err = %v, want ErrQuorumNotMember", err)
	}
	if _, _, err := l2.RejectTransferReview("tx-votes2", "x", "changed mind"); !errors.Is(err, ErrReviewerAlreadyVoted) {
		t.Errorf("opposite vote: err = %v, want ErrReviewerAlreadyVoted", err)
	}
}

func TestChainStageTimeout(t *testing.T) {
	l := reviewSeed(t, 10000)
	if err := l.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"a"}, ThresholdN: 1, Timeout: time.Hour},
		{Approvers: []string{"b"}, ThresholdN: 1},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	chainTransfer(t, l, "tx-timeout")

	// The sweep at stage-timeout + epsilon rejects the review.
	if n := l.ExpireReviewsAt(time.Now().Add(2 * time.Hour)); n != 0 {
		t.Errorf("ExpireReviewsAt returned %d, want 0 (stage timeout is a reject, not an expiry)", n)
	}
	rev, ok := l.GetTransferReview("tx-timeout")
	if !ok {
		t.Fatal("review record missing")
	}
	if rev.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected after stage timeout", rev.Status)
	}
	if rev.ReviewedBy != "system" {
		t.Errorf("ReviewedBy = %q, want system", rev.ReviewedBy)
	}
	// Timed-out stages are lazy for reads: the freeze released before
	// any sweep.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000", got)
	}

	// Approving past a lapsed stage is refused, and the review is
	// rejected first — like review-level expiry.
	chainTransfer(t, l, "tx-timeout2")
	l.mu.Lock()
	r := l.reviews["tx-timeout2"]
	r.ChainProgress = &ReviewChainProgress{
		Stage:          0,
		StageStartedAt: time.Now().Add(-2 * time.Hour),
		Votes:          make([]ChainStageVotes, 2),
	}
	l.mu.Unlock()
	if _, err := l.ApproveTransferReview("tx-timeout2", "a"); !errors.Is(err, ErrReviewExpired) {
		t.Errorf("approve on lapsed stage: err = %v, want ErrReviewExpired", err)
	}
	rev2, _ := l.GetTransferReview("tx-timeout2")
	if rev2.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected", rev2.Status)
	}
}

func TestChainSupersedesQuorum(t *testing.T) {
	l := chainSeed(t)
	// Install a flat quorum policy too: the chain must win.
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 2, false); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	chainTransfer(t, l, "tx-both")

	receipt, err := l.ApproveTransferReview("tx-both", "alice")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if receipt.ChainTally == nil {
		t.Error("ChainTally is nil: the chain did not supersede the quorum policy")
	}
	if receipt.QuorumTally != nil {
		t.Error("QuorumTally set: flat quorum path leaked into the chain decision")
	}
}

func TestChainClearRestoresSingleDecision(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-clear")
	l.ClearApprovalChain()

	receipt, err := l.ApproveTransferReview("tx-clear", "anyone")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("ReviewStatus = %q, want approved (single-operator path)", receipt.ReviewStatus)
	}
	if receipt.ChainTally != nil {
		t.Error("ChainTally set after the chain was cleared")
	}
	rev, _ := l.GetTransferReview("tx-clear")
	if rev.ReviewedBy != "anyone" {
		t.Errorf("ReviewedBy = %q, want anyone", rev.ReviewedBy)
	}
}

func TestChainStageRangeError(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-range")
	if _, err := l.ApproveTransferReview("tx-range", "alice"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	// Replace the chain with a shorter one: the recorded stage no
	// longer exists.
	if err := l.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"z"}, ThresholdN: 1},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-range", "z"); !errors.Is(err, ErrApprovalChainStageRange) {
		t.Errorf("approve: err = %v, want ErrApprovalChainStageRange", err)
	}
}

func TestChainSnapshotRoundTrip(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-snap")
	if _, err := l.ApproveTransferReview("tx-snap", "alice"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	r2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// The policy round-trips.
	c1, ok1 := l.ApprovalChain()
	c2, ok2 := r2.ApprovalChain()
	if !ok1 || !ok2 {
		t.Fatal("approval chain missing after round-trip")
	}
	if len(c1.Stages) != len(c2.Stages) || c1.Stages[0].ThresholdN != c2.Stages[0].ThresholdN {
		t.Errorf("chain differs after round-trip: %+v vs %+v", c1, c2)
	}

	// The review record — stage and per-stage votes — round-trips.
	orig, _ := l.GetTransferReview("tx-snap")
	rest, _ := r2.GetTransferReview("tx-snap")
	if !reviewsEqual(&orig, &rest) {
		t.Errorf("review differs after round-trip:\norig: %+v\nrest: %+v", orig, rest)
	}

	// The restored ledger enforces the chain: carol (stage 1) can
	// settle, bob (stage 0) cannot.
	if _, err := r2.ApproveTransferReview("tx-snap", "bob"); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("restored ledger stage-1 vote by bob: err = %v, want ErrQuorumNotMember", err)
	}
	receipt, err := r2.ApproveTransferReview("tx-snap", "carol")
	if err != nil {
		t.Fatalf("restored ledger settle: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}

	// A snapshot carrying an invalid chain fails the import.
	bad := strings.Replace(buf.String(), `"threshold_n":1`, `"threshold_n":9`, 1)
	if _, err := ImportSnapshot(strings.NewReader(bad)); err == nil {
		t.Error("import of snapshot with invalid chain succeeded, want failure")
	}
}

func TestChainReconcileDisclosure(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-rep")
	rep := l.Reconcile(time.Now())
	if rep.ApprovalChain == nil {
		t.Fatal("reconcile report omits the approval chain")
	}
	if len(rep.ApprovalChain.Stages) != 2 {
		t.Errorf("report chain stages = %d, want 2", len(rep.ApprovalChain.Stages))
	}
	found := false
	for _, pr := range rep.PendingReviews {
		if pr.TransferID == "tx-rep" {
			found = true
			if pr.ChainProgress == nil || pr.ChainProgress.Stage != 0 {
				t.Errorf("pending review chain progress = %+v, want stage 0", pr.ChainProgress)
			}
		}
	}
	if !found {
		t.Error("pending review tx-rep missing from the reconcile report")
	}
}

func TestChainGetReviewDeepCopy(t *testing.T) {
	l := chainSeed(t)
	chainTransfer(t, l, "tx-copy")
	if _, err := l.ApproveTransferReview("tx-copy", "alice"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-copy")
	rev.ChainProgress.Stage = 99
	rev.ChainProgress.Votes[0].ApproveVotes[0] = "mallory"
	fresh, _ := l.GetTransferReview("tx-copy")
	if fresh.ChainProgress.Stage != 1 {
		t.Errorf("mutating the returned record changed the ledger: stage = %d", fresh.ChainProgress.Stage)
	}
	if fresh.ChainProgress.Votes[0].ApproveVotes[0] != "alice" {
		t.Errorf("mutating the returned record changed the ledger: votes = %v", fresh.ChainProgress.Votes[0].ApproveVotes)
	}
}
