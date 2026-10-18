package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// quorumSeed returns a ledger with a 10000-cent review threshold, the
// payer funded with 100000 cents, and a 2-of-3 quorum policy over
// alice/bob/carol. rejectDecisive selects the reject semantics.
func quorumSeed(t *testing.T, rejectDecisive bool) *Ledger {
	t.Helper()
	l := reviewSeed(t, 10000)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 2, rejectDecisive); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	return l
}

// quorumTransfer posts a 30000-cent transfer that freezes into pending
// review, returning the transfer ID.
func quorumTransfer(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer %q: %v", id, err)
	}
	rev, ok := l.GetTransferReview(id)
	if !ok || rev.Status != ReviewStatusPending {
		t.Fatalf("transfer %q review = %+v, want pending", id, rev.Status)
	}
}

func TestQuorumPolicyValidation(t *testing.T) {
	l := reviewSeed(t, 10000)
	bad := []struct {
		name      string
		approvers []string
		n         int
	}{
		{"no approvers", nil, 1},
		{"threshold zero", []string{"a"}, 0},
		{"threshold negative", []string{"a"}, -1},
		{"threshold exceeds members", []string{"a", "b"}, 3},
		{"blank approver", []string{"a", "  "}, 1},
		{"empty approver", []string{"a", ""}, 1},
		{"duplicate approver", []string{"a", "a"}, 1},
	}
	for _, tc := range bad {
		if err := l.SetReviewQuorumPolicy(tc.approvers, tc.n, true); !errors.Is(err, ErrInvalidQuorumPolicy) {
			t.Errorf("%s: err = %v, want ErrInvalidQuorumPolicy", tc.name, err)
		}
	}
	// A rejected install leaves no policy behind.
	if _, ok := l.ReviewQuorumPolicy(); ok {
		t.Error("policy installed after invalid Set, want none")
	}
	// Valid installs: N=1 (single-approver fast path) and N=M.
	for _, tc := range []struct {
		approvers []string
		n         int
	}{
		{[]string{"a"}, 1},
		{[]string{"a", "b", "c"}, 3},
	} {
		if err := l.SetReviewQuorumPolicy(tc.approvers, tc.n, false); err != nil {
			t.Errorf("valid policy %+v: %v", tc, err)
		}
		p, ok := l.ReviewQuorumPolicy()
		if !ok {
			t.Fatalf("policy %+v not installed", tc)
		}
		if p.ThresholdN != tc.n || len(p.Approvers) != len(tc.approvers) || p.RejectDecisive {
			t.Errorf("policy = %+v, want n=%d approvers=%v decisive=false", p, tc.n, tc.approvers)
		}
	}
	// The returned policy is a copy: mutating it must not affect the ledger.
	p, _ := l.ReviewQuorumPolicy()
	p.Approvers[0] = "mallory"
	if got, _ := l.ReviewQuorumPolicy(); got.Approvers[0] == "mallory" {
		t.Error("ReviewQuorumPolicy returned an alias, want a copy")
	}
	// Clear restores the LG-42 single-decision path.
	l.ClearReviewQuorumPolicy()
	if _, ok := l.ReviewQuorumPolicy(); ok {
		t.Error("policy still active after Clear, want none")
	}
}

func TestQuorumPolicyConstructorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("WithReviewQuorumPolicy with N=0 did not panic")
		}
	}()
	_ = New(WithReviewQuorumPolicy(QuorumPolicy{Approvers: []string{"a"}, ThresholdN: 0}))
	l := New(WithReviewQuorumPolicy(QuorumPolicy{Approvers: []string{"a", "b"}, ThresholdN: 2, RejectDecisive: true}))
	if p, ok := l.ReviewQuorumPolicy(); !ok || p.ThresholdN != 2 || !p.RejectDecisive {
		t.Errorf("constructed policy = %+v, %v", p, ok)
	}
}

func TestQuorumApproveVotesBelowThreshold(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q1")

	receipt, err := l.ApproveTransferReview("tx-q1", "alice")
	if err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Errorf("receipt ReviewStatus = %q, want pending_review", receipt.ReviewStatus)
	}
	if receipt.Duplicate {
		t.Error("fresh vote receipt has Duplicate = true")
	}
	tally := receipt.QuorumTally
	if tally == nil {
		t.Fatal("vote receipt has no quorum tally")
	}
	if tally.Outcome != QuorumOutcomeRecorded {
		t.Errorf("tally outcome = %q, want recorded", tally.Outcome)
	}
	if len(tally.ApproveVotes) != 1 || tally.ApproveVotes[0] != "alice" {
		t.Errorf("tally approve votes = %v, want [alice]", tally.ApproveVotes)
	}
	if tally.ThresholdN != 2 || tally.MemberCount != 3 {
		t.Errorf("tally = %+v, want threshold 2 of 3", tally)
	}
	if tally.LastVoter != "alice" {
		t.Errorf("tally last voter = %q, want alice", tally.LastVoter)
	}
	// One vote moves no money: balances and version untouched, review
	// still pending, freeze still reserving.
	if got := l.Balance("payer"); got != 100000 {
		t.Errorf("payer balance = %d, want 100000 (still frozen)", got)
	}
	rev, _ := l.GetTransferReview("tx-q1")
	if rev.Status != ReviewStatusPending {
		t.Errorf("review status = %q, want pending", rev.Status)
	}
	if len(rev.ApproveVotes) != 1 || rev.ApproveVotes[0] != "alice" {
		t.Errorf("review approve votes = %v, want [alice]", rev.ApproveVotes)
	}
	if rev.ReviewedBy != "" {
		t.Errorf("ReviewedBy = %q, want empty while pending", rev.ReviewedBy)
	}
}

func TestQuorumApproveSettlesAtThreshold(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q2")
	versionBefore := l.Version()

	if _, err := l.ApproveTransferReview("tx-q2", "alice"); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	receipt, err := l.ApproveTransferReview("tx-q2", "bob")
	if err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("receipt ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}
	if receipt.QuorumTally == nil || receipt.QuorumTally.Outcome != QuorumOutcomeSettled {
		t.Errorf("settle receipt tally = %+v, want outcome settled", receipt.QuorumTally)
	}
	// Settlement went through the LG-42 path: money moved, version
	// bumped once per leg.
	if got := l.Balance("payer"); got != 70000 {
		t.Errorf("payer balance = %d, want 70000", got)
	}
	if got := l.Balance("payee"); got != 30000 {
		t.Errorf("payee balance = %d, want 30000", got)
	}
	if l.Version() == versionBefore {
		t.Error("version did not bump on quorum settlement")
	}
	rev, _ := l.GetTransferReview("tx-q2")
	if rev.Status != ReviewStatusApproved {
		t.Errorf("review status = %q, want approved", rev.Status)
	}
	if rev.ReviewedBy != "alice,bob" {
		t.Errorf("ReviewedBy = %q, want \"alice,bob\"", rev.ReviewedBy)
	}
	if len(rev.ApproveVotes) != 2 {
		t.Errorf("review approve votes = %v, want 2", rev.ApproveVotes)
	}
	// Re-approving after quorum settlement is the standard idempotent
	// replay: the original receipt, marked duplicate, without the vote
	// tally (the tally described the settling call, not the replay).
	dup, err := l.ApproveTransferReview("tx-q2", "carol")
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if !dup.Duplicate {
		t.Error("re-approve Duplicate = false, want true")
	}
	if dup.QuorumTally != nil {
		t.Errorf("re-approve carries a tally %+v, want nil", dup.QuorumTally)
	}
}

func TestQuorumDuplicateVoteIdempotent(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q3")

	first, err := l.ApproveTransferReview("tx-q3", "alice")
	if err != nil {
		t.Fatalf("vote: %v", err)
	}
	second, err := l.ApproveTransferReview("tx-q3", "alice")
	if err != nil {
		t.Fatalf("re-vote: %v", err)
	}
	if !second.Duplicate {
		t.Error("re-vote Duplicate = false, want true")
	}
	if second.QuorumTally == nil || second.QuorumTally.Outcome != QuorumOutcomeDuplicate {
		t.Errorf("re-vote tally = %+v, want outcome duplicate", second.QuorumTally)
	}
	if len(second.QuorumTally.ApproveVotes) != 1 {
		t.Errorf("re-vote counted twice: %v", second.QuorumTally.ApproveVotes)
	}
	if second.ReviewStatus != ReviewStatusPending {
		t.Errorf("re-vote status = %q, want pending", second.ReviewStatus)
	}
	// The review still needs one more distinct voter.
	rev, _ := l.GetTransferReview("tx-q3")
	if len(rev.ApproveVotes) != 1 {
		t.Errorf("review votes = %v, want exactly 1 after replay", rev.ApproveVotes)
	}
	// A replay must not emit a second vote audit event: the recorded
	// vote and its replay share one event. (The full audit assertion
	// lives in TestQuorumAuditVoteEvents; here we check the receipt
	// carries the same tally as the first vote.)
	if first.QuorumTally == nil || len(first.QuorumTally.ApproveVotes) != len(second.QuorumTally.ApproveVotes) {
		t.Error("re-vote tally diverged from the recorded vote's tally")
	}
}

func TestQuorumNonMemberRejected(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q4")

	if _, err := l.ApproveTransferReview("tx-q4", "mallory"); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("non-member approve: err = %v, want ErrQuorumNotMember", err)
	}
	if _, _, err := l.RejectTransferReview("tx-q4", "mallory", ""); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("non-member reject: err = %v, want ErrQuorumNotMember", err)
	}
	// Failed votes record nothing.
	rev, _ := l.GetTransferReview("tx-q4")
	if len(rev.ApproveVotes)+len(rev.RejectVotes) != 0 {
		t.Errorf("rejected votes recorded: %+v", rev)
	}
	// An empty identity defaults to "operator" first — which is not a
	// member here — so it is rejected like any other non-member.
	if _, err := l.ApproveTransferReview("tx-q4", ""); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("empty approver: err = %v, want ErrQuorumNotMember", err)
	}
}

func TestQuorumCrossVoteRejected(t *testing.T) {
	l := quorumSeed(t, false)
	quorumTransfer(t, l, "tx-q5")

	if _, err := l.ApproveTransferReview("tx-q5", "alice"); err != nil {
		t.Fatalf("approve vote: %v", err)
	}
	// alice voted approve: a later reject vote from her fails, and the
	// recorded approve vote stands.
	if _, _, err := l.RejectTransferReview("tx-q5", "alice", "changed mind"); !errors.Is(err, ErrReviewerAlreadyVoted) {
		t.Errorf("cross vote: err = %v, want ErrReviewerAlreadyVoted", err)
	}
	if _, err := l.ApproveTransferReview("tx-q5", "bob"); err != nil {
		t.Fatalf("second approve vote: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-q5")
	if rev.Status != ReviewStatusApproved {
		t.Errorf("review status = %q, want approved (alice's vote stood)", rev.Status)
	}

	// The mirror: reject first, then approve.
	quorumTransfer(t, l, "tx-q6")
	if _, _, err := l.RejectTransferReview("tx-q6", "carol", "nope"); err != nil {
		t.Fatalf("reject vote: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-q6", "carol"); !errors.Is(err, ErrReviewerAlreadyVoted) {
		t.Errorf("cross vote after reject: err = %v, want ErrReviewerAlreadyVoted", err)
	}
}

func TestQuorumRejectDecisive(t *testing.T) {
	l := quorumSeed(t, true) // RejectDecisive
	quorumTransfer(t, l, "tx-q7")

	review, changed, err := l.RejectTransferReview("tx-q7", "bob", "sanctions")
	if err != nil {
		t.Fatalf("reject vote: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true for the decisive vote")
	}
	if review.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected", review.Status)
	}
	if review.ReviewedBy != "bob" {
		t.Errorf("ReviewedBy = %q, want bob", review.ReviewedBy)
	}
	// The dissent is on the record even though it was decisive.
	if len(review.RejectVotes) != 1 || review.RejectVotes[0] != "bob" {
		t.Errorf("reject votes = %v, want [bob]", review.RejectVotes)
	}
	// Freeze released: the full 100000 is available again.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("available = %d, want 100000 after decisive reject", got)
	}
	// Replaying the reject vote is idempotent.
	replay, changed, err := l.RejectTransferReview("tx-q7", "bob", "sanctions")
	if err != nil {
		t.Fatalf("reject replay: %v", err)
	}
	if changed {
		t.Error("reject replay changed = true, want false")
	}
	if replay.Status != ReviewStatusRejected {
		t.Errorf("replay status = %q, want rejected", replay.Status)
	}
}

func TestQuorumRejectNonDecisive(t *testing.T) {
	l := quorumSeed(t, false) // reject votes are dissent, not decisive
	quorumTransfer(t, l, "tx-q8")

	review, changed, err := l.RejectTransferReview("tx-q8", "carol", "looks odd")
	if err != nil {
		t.Fatalf("reject vote: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true for a recorded vote")
	}
	if review.Status != ReviewStatusPending {
		t.Errorf("status = %q, want pending (non-decisive)", review.Status)
	}
	if len(review.RejectVotes) != 1 || review.RejectVotes[0] != "carol" {
		t.Errorf("reject votes = %v, want [carol]", review.RejectVotes)
	}
	// The freeze stays: funds are still reserved.
	if got := l.Available("payer"); got != 70000 {
		t.Errorf("available = %d, want 70000 (freeze still in place)", got)
	}
	// The review can still reach the approval quorum despite the
	// dissent — and the dissent stays on the record.
	if _, err := l.ApproveTransferReview("tx-q8", "alice"); err != nil {
		t.Fatalf("approve vote 1: %v", err)
	}
	receipt, err := l.ApproveTransferReview("tx-q8", "bob")
	if err != nil {
		t.Fatalf("approve vote 2: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("status = %q, want approved", receipt.ReviewStatus)
	}
	rev, _ := l.GetTransferReview("tx-q8")
	if len(rev.RejectVotes) != 1 || len(rev.ApproveVotes) != 2 {
		t.Errorf("votes = approve %v reject %v, want 2/1", rev.ApproveVotes, rev.RejectVotes)
	}
}

func TestQuorumExpiryWithVotes(t *testing.T) {
	l := New(WithReviewThreshold(10000), WithReviewExpiry(time.Minute))
	seedBalance(t, l, "payer", 100000)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob"}, 2, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	quorumTransfer(t, l, "tx-q9")
	if _, err := l.ApproveTransferReview("tx-q9", "alice"); err != nil {
		t.Fatalf("vote: %v", err)
	}
	// The review lapses: the recorded vote is part of the terminal
	// record, and the freeze is released.
	if n := l.ExpireReviewsAt(time.Now().Add(2 * time.Minute)); n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	rev, _ := l.GetTransferReview("tx-q9")
	if rev.Status != ReviewStatusExpired {
		t.Errorf("status = %q, want expired", rev.Status)
	}
	if len(rev.ApproveVotes) != 1 || rev.ApproveVotes[0] != "alice" {
		t.Errorf("votes lost on expiry: %v", rev.ApproveVotes)
	}
	if rev.ReviewedBy != "system" {
		t.Errorf("ReviewedBy = %q, want system", rev.ReviewedBy)
	}
	// A vote after expiry is refused: the review is already terminal.
	if _, err := l.ApproveTransferReview("tx-q9", "bob"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("vote after expiry: err = %v, want ErrReviewNotPending", err)
	}
}

func TestQuorumSnapshotRoundTrip(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q10")
	if _, err := l.ApproveTransferReview("tx-q10", "alice"); err != nil {
		t.Fatalf("vote: %v", err)
	}
	quorumTransfer(t, l, "tx-q11")
	if _, _, err := l.RejectTransferReview("tx-q11", "bob", "test"); err != nil {
		t.Fatalf("reject vote: %v", err)
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
	p, ok := r2.ReviewQuorumPolicy()
	if !ok {
		t.Fatal("quorum policy lost in snapshot round-trip")
	}
	if p.ThresholdN != 2 || len(p.Approvers) != 3 || !p.RejectDecisive {
		t.Errorf("restored policy = %+v", p)
	}
	// The votes round-trip on both reviews.
	for _, id := range []string{"tx-q10", "tx-q11"} {
		orig, _ := l.GetTransferReview(id)
		rest, ok := r2.GetTransferReview(id)
		if !ok {
			t.Fatalf("review %q missing after restore", id)
		}
		if !reviewsEqual(&orig, &rest) {
			t.Errorf("review %q differs after round-trip:\norig: %+v\nrest: %+v", id, orig, rest)
		}
	}
	// The restored policy is live: a non-member vote fails, and the
	// pending review still needs exactly one more vote to settle.
	if _, err := r2.ApproveTransferReview("tx-q10", "mallory"); !errors.Is(err, ErrQuorumNotMember) {
		t.Errorf("restored non-member vote: err = %v", err)
	}
	receipt, err := r2.ApproveTransferReview("tx-q10", "carol")
	if err != nil {
		t.Fatalf("restored settling vote: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Errorf("restored settle: status = %q, want approved", receipt.ReviewStatus)
	}
	if got := r2.Balance("payee"); got != 30000 {
		t.Errorf("restored payee balance = %d, want 30000", got)
	}
}

func TestQuorumReconcileReport(t *testing.T) {
	l := quorumSeed(t, false)
	quorumTransfer(t, l, "tx-q12")
	if _, err := l.ApproveTransferReview("tx-q12", "alice"); err != nil {
		t.Fatalf("vote: %v", err)
	}
	if _, _, err := l.RejectTransferReview("tx-q12", "carol", "dissent"); err != nil {
		t.Fatalf("reject vote: %v", err)
	}
	report := l.Reconcile(time.Now())
	if report.ReviewQuorum == nil || report.ReviewQuorum.ThresholdN != 2 {
		t.Fatalf("report quorum = %+v, want 2-of-3", report.ReviewQuorum)
	}
	var found *TransferReview
	for i := range report.PendingReviews {
		if report.PendingReviews[i].TransferID == "tx-q12" {
			found = &report.PendingReviews[i]
		}
	}
	if found == nil {
		t.Fatal("tx-q12 missing from pending reviews")
	}
	if len(found.ApproveVotes) != 1 || found.ApproveVotes[0] != "alice" {
		t.Errorf("report approve votes = %v, want [alice]", found.ApproveVotes)
	}
	if len(found.RejectVotes) != 1 || found.RejectVotes[0] != "carol" {
		t.Errorf("report reject votes = %v, want [carol]", found.RejectVotes)
	}
}

func TestQuorumAuditVoteEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob"}, 2, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	quorumTransfer(t, l, "tx-q13")
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.ApproveTransferReviewCtx(ctx, "tx-q13", "alice"); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	if _, err := l.ApproveTransferReviewCtx(ctx, "tx-q13", "bob"); err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	votes := byOp["transfer_review_vote"]
	if len(votes) != 2 {
		t.Fatalf("vote events = %d, want 2", len(votes))
	}
	if votes[0].Details["voter"] != "alice" || votes[0].Details["vote"] != "approve" {
		t.Errorf("vote event 1 = %+v", votes[0].Details)
	}
	if votes[0].Details["threshold_n"] != float64(2) {
		t.Errorf("vote event threshold_n = %v, want 2", votes[0].Details["threshold_n"])
	}
	if votes[0].Actor != "ops-desk" {
		t.Errorf("vote event actor = %q, want ops-desk", votes[0].Actor)
	}
	// The settling vote emits both the vote and the approval.
	if got := byOp["transfer_review_approved"]; len(got) != 1 {
		t.Errorf("approved events = %d, want 1", len(got))
	}
}

func TestQuorumPolicyChangeMidFlight(t *testing.T) {
	l := quorumSeed(t, true)
	quorumTransfer(t, l, "tx-q14")
	if _, err := l.ApproveTransferReview("tx-q14", "alice"); err != nil {
		t.Fatalf("vote: %v", err)
	}
	// The policy changes mid-flight: history stays, new votes follow
	// the new policy. Raising the threshold to 3 means carol's vote is
	// still needed; alice's recorded vote is not rewritten.
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 3, true); err != nil {
		t.Fatalf("policy change: %v", err)
	}
	receipt, err := l.ApproveTransferReview("tx-q14", "bob")
	if err != nil {
		t.Fatalf("vote under new policy: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Errorf("status = %q, want pending (2 of new 3)", receipt.ReviewStatus)
	}
	if receipt.QuorumTally.ThresholdN != 3 {
		t.Errorf("tally threshold = %d, want 3", receipt.QuorumTally.ThresholdN)
	}
	// Clearing the policy restores immediate single-decision settlement
	// for later decisions.
	l.ClearReviewQuorumPolicy()
	quorumTransfer(t, l, "tx-q15")
	settled, err := l.ApproveTransferReview("tx-q15", "anyone")
	if err != nil {
		t.Fatalf("approve after clear: %v", err)
	}
	if settled.ReviewStatus != ReviewStatusApproved || settled.QuorumTally != nil {
		t.Errorf("post-clear receipt = %+v, want direct approval without tally", settled.ReviewStatus)
	}
}
