package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// delegationSeed returns a ledger with a 10000-cent review threshold,
// the payer funded, and an audit log wired to dir so delegation audit
// events can be asserted. Tests that read the audit trail must Close
// the log first: Close flushes the async writer, so the read is
// deterministic (see TestFreezeCtxActorAttribution).
func delegationSeed(t *testing.T, dir string) (*Ledger, *AuditLog) {
	t.Helper()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithReviewThreshold(10000), WithAuditLog(al))
	seedBalance(t, l, "payer", 100000)
	return l, al
}

// delegationTransfer posts a 30000-cent transfer that freezes into
// pending review, returning the transfer ID.
func delegationTransfer(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer %q: %v", id, err)
	}
}

// grantAs grants a delegation as principal, failing the test on error.
func grantAs(t *testing.T, l *Ledger, principal, scope, reviewID, delegatee string, expiresAt time.Time) ReviewDelegation {
	t.Helper()
	ctx := ContextWithActor(context.Background(), principal)
	d, err := l.DelegateVoteCtx(ctx, scope, reviewID, delegatee, expiresAt)
	if err != nil {
		t.Fatalf("DelegateVoteCtx(%s -> %s): %v", principal, delegatee, err)
	}
	return d
}

func TestDelegateVoteValidation(t *testing.T) {
	l := reviewSeed(t, 10000)
	ctx := ContextWithActor(context.Background(), "alice")

	cases := []struct {
		name       string
		scope      string
		delegatee  string
		expiresAt  time.Time
	}{
		{"unknown scope", "mandate", "dave", time.Time{}},
		{"empty scope", "", "dave", time.Time{}},
		{"blank delegatee", DelegationScopeQuorum, "  ", time.Time{}},
		{"self delegation", DelegationScopeQuorum, "alice", time.Time{}},
		{"past expiry", DelegationScopeQuorum, "dave", time.Now().Add(-time.Hour)},
		{"now expiry", DelegationScopeQuorum, "dave", time.Now()},
	}
	for _, tc := range cases {
		if _, err := l.DelegateVoteCtx(ctx, tc.scope, "", tc.delegatee, tc.expiresAt); !errors.Is(err, ErrInvalidDelegation) {
			t.Errorf("%s: err = %v, want ErrInvalidDelegation", tc.name, err)
		}
	}
	// A rejected grant leaves nothing behind.
	if got := l.ReviewDelegations(); len(got) != 0 {
		t.Errorf("delegations after rejected grants = %d, want 0", len(got))
	}
	if got := l.ReviewDelegationsGranted(); got != 0 {
		t.Errorf("granted counter after rejected grants = %d, want 0", got)
	}

	// A valid grant records principal, scope, and expiry.
	d := grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Now().Add(time.Hour))
	if d.Principal != "alice" || d.Delegatee != "dave" || d.Scope != DelegationScopeQuorum {
		t.Errorf("delegation = %+v, want alice -> dave scope quorum", d)
	}
	if d.GrantedAt.IsZero() || d.ExpiresAt.IsZero() {
		t.Errorf("delegation times not set: %+v", d)
	}
}

func TestDelegateVoteGrantRevokeAudit(t *testing.T) {
	dir := t.TempDir()
	l, al := delegationSeed(t, dir)

	d := grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})
	if got := l.ReviewDelegationsGranted(); got != 1 {
		t.Errorf("granted counter = %d, want 1", got)
	}
	listed := l.ReviewDelegations()
	if len(listed) != 1 || listed[0].Principal != "alice" || listed[0].Delegatee != "dave" {
		t.Fatalf("ReviewDelegations = %+v, want [alice -> dave]", listed)
	}

	// Idempotent re-grant: same record, no new audit event, no new count.
	d2 := grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})
	if d2.GrantedAt.UnixNano() != d.GrantedAt.UnixNano() {
		t.Errorf("idempotent re-grant moved GrantedAt: %+v -> %+v", d, d2)
	}
	if got := l.ReviewDelegationsGranted(); got != 1 {
		t.Errorf("granted counter after idempotent re-grant = %d, want 1", got)
	}

	// Extension: new expiry, audited, but still one grant.
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Now().Add(2*time.Hour))
	if got := l.ReviewDelegationsGranted(); got != 1 {
		t.Errorf("granted counter after extension = %d, want 1", got)
	}

	// Revoke: terminal, audited; a second revoke is a silent no-op.
	ctx := ContextWithActor(context.Background(), "alice")
	revoked, err := l.RevokeDelegationCtx(ctx, DelegationScopeQuorum, "", "dave")
	if err != nil || !revoked {
		t.Fatalf("RevokeDelegation = %v, %v, want true, nil", revoked, err)
	}
	if got := l.ReviewDelegations(); len(got) != 0 {
		t.Errorf("delegations after revoke = %d, want 0", len(got))
	}
	revoked, err = l.RevokeDelegationCtx(ctx, DelegationScopeQuorum, "", "dave")
	if err != nil || revoked {
		t.Errorf("second RevokeDelegation = %v, %v, want false, nil", revoked, err)
	}
	// Revoking a delegation that was never granted is also a no-op.
	revoked, err = l.RevokeDelegationCtx(ctx, DelegationScopeQuorum, "", "mallory")
	if err != nil || revoked {
		t.Errorf("revoke unknown = %v, %v, want false, nil", revoked, err)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	grants := byOp["delegation_granted"]
	// One grant + one extension: the idempotent re-grant stayed silent.
	if len(grants) != 2 {
		t.Fatalf("delegation_granted events = %d, want 2", len(grants))
	}
	ev := grants[0]
	if ev.Actor != "alice" {
		t.Errorf("grant actor = %q, want alice", ev.Actor)
	}
	if ev.Details["scope"] != DelegationScopeQuorum || ev.Details["principal"] != "alice" || ev.Details["delegatee"] != "dave" {
		t.Errorf("grant details = %+v, want scope/principal/delegatee", ev.Details)
	}
	if grants[1].Details["extended"] != true {
		t.Errorf("extension event details = %+v, want extended=true", grants[1].Details)
	}
	revokes := byOp["delegation_revoked"]
	if len(revokes) != 1 {
		t.Fatalf("delegation_revoked events = %d, want 1", len(revokes))
	}
	if revokes[0].Details["principal"] != "alice" || revokes[0].Details["delegatee"] != "dave" {
		t.Errorf("revoke details = %+v, want principal/delegatee", revokes[0].Details)
	}
}

func TestDelegateVoteCycleRejected(t *testing.T) {
	l := reviewSeed(t, 10000)

	// Direct cycle: alice -> dave, then dave -> alice.
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})
	ctx := ContextWithActor(context.Background(), "dave")
	if _, err := l.DelegateVoteCtx(ctx, DelegationScopeQuorum, "", "alice", time.Time{}); !errors.Is(err, ErrDelegationCycle) {
		t.Errorf("dave -> alice: err = %v, want ErrDelegationCycle", err)
	}

	// Longer chain: alice -> dave -> erin, then erin -> alice.
	grantAs(t, l, "dave", DelegationScopeChain, "", "erin", time.Time{})
	ctx = ContextWithActor(context.Background(), "erin")
	if _, err := l.DelegateVoteCtx(ctx, DelegationScopeReview, "", "alice", time.Time{}); !errors.Is(err, ErrDelegationCycle) {
		t.Errorf("erin -> alice: err = %v, want ErrDelegationCycle", err)
	}

	// The rejected grants left the graph unchanged: alice still
	// delegates only to dave.
	listed := l.ReviewDelegations()
	if len(listed) != 2 {
		t.Fatalf("delegations after rejected cycles = %d, want 2", len(listed))
	}
}

func TestDelegationExpiryResolution(t *testing.T) {
	l := reviewSeed(t, 10000)
	now := time.Now()
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", now.Add(time.Hour))
	grantAs(t, l, "alice", DelegationScopeQuorum, "tx-9", "erin", time.Time{})

	// The resolver is exercised under the read lock; grants happen
	// outside it (a write lock nests fatally under a held read lock).
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.resolveDelegationLocked(DelegationScopeQuorum, "tx-1", "dave", now); !ok {
		t.Error("active delegation did not resolve")
	}
	// Wrong scope does not resolve.
	if _, ok := l.resolveDelegationLocked(DelegationScopeChain, "tx-1", "dave", now); ok {
		t.Error("quorum delegation resolved for chain scope")
	}
	// A lapsed delegation no longer resolves (lazy expiry).
	if _, ok := l.resolveDelegationLocked(DelegationScopeQuorum, "tx-1", "dave", now.Add(2*time.Hour)); ok {
		t.Error("expired delegation still resolved")
	}
	// A review-scoped delegation only resolves for its review.
	if _, ok := l.resolveDelegationLocked(DelegationScopeQuorum, "tx-9", "erin", now); !ok {
		t.Error("review-scoped delegation did not resolve for its review")
	}
	if _, ok := l.resolveDelegationLocked(DelegationScopeQuorum, "tx-1", "erin", now); ok {
		t.Error("review-scoped delegation resolved for another review")
	}
}

func TestDelegatedQuorumApproveVote(t *testing.T) {
	dir := t.TempDir()
	l, al := delegationSeed(t, dir)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 2, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	delegationTransfer(t, l, "tx-d1")

	// Without a delegation the outsider cannot vote.
	if _, err := l.ApproveTransferReview("tx-d1", "dave"); !errors.Is(err, ErrQuorumNotMember) {
		t.Fatalf("dave without delegation: err = %v, want ErrQuorumNotMember", err)
	}

	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})

	// Dave votes: recorded as alice's vote, with dual attribution.
	receipt, err := l.ApproveTransferReview("tx-d1", "dave")
	if err != nil {
		t.Fatalf("delegated approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("ReviewStatus = %q, want pending", receipt.ReviewStatus)
	}
	tally := receipt.QuorumTally
	if tally == nil {
		t.Fatal("missing quorum tally")
	}
	if len(tally.ApproveVotes) != 1 || tally.ApproveVotes[0] != "alice" {
		t.Errorf("tally approve votes = %v, want [alice]", tally.ApproveVotes)
	}
	if tally.LastVoter != "dave" {
		t.Errorf("tally LastVoter = %q, want dave (the actual caller)", tally.LastVoter)
	}

	// Dave voting again replays alice's vote idempotently.
	receipt2, err := l.ApproveTransferReview("tx-d1", "dave")
	if err != nil || !receipt2.Duplicate {
		t.Errorf("delegated replay = %+v, %v, want Duplicate receipt", receipt2, err)
	}
	// Alice voting directly now replays her exercised vote: dave
	// already cast it in her name, so the call is idempotent (the
	// one-vote-per-approver rule applies to the principal).
	aliceReplay, err := l.ApproveTransferReview("tx-d1", "alice")
	if err != nil || !aliceReplay.Duplicate {
		t.Errorf("alice after delegated vote = %+v, %v, want Duplicate replay", aliceReplay, err)
	}

	// Bob's vote reaches the threshold: the review settles as alice+bob.
	receipt, err = l.ApproveTransferReview("tx-d1", "bob")
	if err != nil {
		t.Fatalf("bob approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}
	rev, _ := l.GetTransferReview("tx-d1")
	if rev.ReviewedBy != "alice,bob" {
		t.Errorf("ReviewedBy = %q, want alice,bob", rev.ReviewedBy)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	votes := byOp["transfer_review_vote"]
	if len(votes) != 2 {
		t.Fatalf("transfer_review_vote events = %d, want 2", len(votes))
	}
	if votes[0].Details["voter"] != "alice" || votes[0].Details["delegate"] != "dave" || votes[0].Details["principal"] != "alice" {
		t.Errorf("delegated vote details = %+v, want voter/delegate/principal", votes[0].Details)
	}
	if _, ok := votes[0].Details["delegate"]; ok && votes[1].Details["delegate"] != nil {
		t.Errorf("direct vote carried delegate attribution: %+v", votes[1].Details)
	}
	delegated := byOp["vote_by_delegate"]
	if len(delegated) != 1 {
		t.Fatalf("vote_by_delegate events = %d, want 1", len(delegated))
	}
	dv := delegated[0]
	if dv.TraceID != "tx-d1" || dv.Details["scope"] != DelegationScopeQuorum ||
		dv.Details["vote"] != "approve" || dv.Details["delegate"] != "dave" || dv.Details["principal"] != "alice" {
		t.Errorf("vote_by_delegate = %+v, want dual attribution", dv)
	}
}

func TestDelegatedQuorumSettlingVote(t *testing.T) {
	dir := t.TempDir()
	l, al := delegationSeed(t, dir)
	// 1-of-2: the delegatee's vote settles the review at once.
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob"}, 1, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	delegationTransfer(t, l, "tx-d7")
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})

	receipt, err := l.ApproveTransferReview("tx-d7", "dave")
	if err != nil {
		t.Fatalf("delegated settling vote: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}
	rev, _ := l.GetTransferReview("tx-d7")
	if rev.ReviewedBy != "alice" {
		t.Errorf("ReviewedBy = %q, want alice", rev.ReviewedBy)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	// Exactly one vote_by_delegate: the settling vote is one vote,
	// not a vote plus a settlement.
	if len(byOp["vote_by_delegate"]) != 1 {
		t.Errorf("vote_by_delegate events = %d, want 1", len(byOp["vote_by_delegate"]))
	}
	approved := byOp["transfer_review_approved"]
	if len(approved) != 1 {
		t.Fatalf("transfer_review_approved events = %d, want 1", len(approved))
	}
	if approved[0].Details["delegate"] != "dave" || approved[0].Details["principal"] != "alice" {
		t.Errorf("settlement details = %+v, want dual attribution", approved[0].Details)
	}
}

func TestDelegatedQuorumRejectVote(t *testing.T) {
	dir := t.TempDir()
	l, al := delegationSeed(t, dir)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob"}, 2, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	delegationTransfer(t, l, "tx-d2")
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})

	rev, changed, err := l.RejectTransferReview("tx-d2", "dave", "looks off")
	if err != nil || !changed {
		t.Fatalf("delegated reject = %v, %v, want changed", changed, err)
	}
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "alice" {
		t.Errorf("review = %+v, want rejected by alice", rev)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	rejects := byOp["transfer_review_rejected"]
	if len(rejects) != 1 {
		t.Fatalf("transfer_review_rejected events = %d, want 1", len(rejects))
	}
	if rejects[0].Details["delegate"] != "dave" || rejects[0].Details["principal"] != "alice" {
		t.Errorf("reject details = %+v, want dual attribution", rejects[0].Details)
	}
	delegated := byOp["vote_by_delegate"]
	if len(delegated) != 1 || delegated[0].Details["vote"] != "reject" {
		t.Errorf("vote_by_delegate events = %+v, want one reject", delegated)
	}
}

func TestDelegatedChainVote(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithReviewThreshold(10000), WithAuditLog(al))
	seedBalance(t, l, "payer", 100000)
	if err := l.SetApprovalChain(ApprovalChain{Stages: []ApprovalChainStage{
		{Approvers: []string{"alice", "bob"}, ThresholdN: 1},
		{Approvers: []string{"carol"}, ThresholdN: 1},
	}}); err != nil {
		t.Fatalf("SetApprovalChain: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-d3", From: "payer", To: "payee", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	grantAs(t, l, "alice", DelegationScopeChain, "", "dave", time.Time{})

	// Dave votes on stage 0 through alice's delegation: the vote is
	// recorded as alice's and advances the review.
	receipt, err := l.ApproveTransferReview("tx-d3", "dave")
	if err != nil {
		t.Fatalf("delegated chain vote: %v", err)
	}
	tally := receipt.ChainTally
	if tally == nil || tally.Outcome != ChainOutcomeAdvanced || tally.Stage != 1 {
		t.Fatalf("chain tally = %+v, want advanced to stage 1", tally)
	}
	if len(tally.ApproveVotes) != 0 {
		t.Errorf("stage 1 votes = %v, want none yet", tally.ApproveVotes)
	}
	rev, _ := l.GetTransferReview("tx-d3")
	if len(rev.ChainProgress.Votes[0].ApproveVotes) != 1 || rev.ChainProgress.Votes[0].ApproveVotes[0] != "alice" {
		t.Errorf("stage 0 votes = %+v, want [alice]", rev.ChainProgress.Votes[0].ApproveVotes)
	}

	// Carol settles the final stage directly.
	receipt, err = l.ApproveTransferReview("tx-d3", "carol")
	if err != nil {
		t.Fatalf("carol approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	delegated := byOp["vote_by_delegate"]
	if len(delegated) != 1 {
		t.Fatalf("vote_by_delegate events = %d, want 1", len(delegated))
	}
	if delegated[0].Details["scope"] != DelegationScopeChain || delegated[0].Details["delegate"] != "dave" {
		t.Errorf("vote_by_delegate = %+v, want chain scope dual attribution", delegated[0])
	}
	chainVotes := byOp["transfer_review_chain_vote"]
	if len(chainVotes) != 2 {
		t.Fatalf("transfer_review_chain_vote events = %d, want 2", len(chainVotes))
	}
	if chainVotes[0].Details["delegate"] != "dave" || chainVotes[0].Details["principal"] != "alice" {
		t.Errorf("delegated chain vote details = %+v", chainVotes[0].Details)
	}
}

func TestDelegatedFlatReviewVote(t *testing.T) {
	dir := t.TempDir()
	l, al := delegationSeed(t, dir)
	delegationTransfer(t, l, "tx-d4")
	delegationTransfer(t, l, "tx-d5")

	// Control: without a delegation the reviewer is recorded as-is.
	receipt, err := l.ApproveTransferReview("tx-d4", "dave")
	if err != nil {
		t.Fatalf("direct approve: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("ReviewStatus = %q, want approved", receipt.ReviewStatus)
	}
	rev, _ := l.GetTransferReview("tx-d4")
	if rev.ReviewedBy != "dave" {
		t.Errorf("ReviewedBy = %q, want dave", rev.ReviewedBy)
	}

	// With a review-scope delegation, dave's approval is attributed
	// to alice, with dual attribution in the audit trail.
	grantAs(t, l, "alice", DelegationScopeReview, "", "dave", time.Time{})
	receipt, err = l.ApproveTransferReview("tx-d5", "dave")
	if err != nil {
		t.Fatalf("delegated approve: %v", err)
	}
	rev, _ = l.GetTransferReview("tx-d5")
	if rev.ReviewedBy != "alice" {
		t.Errorf("ReviewedBy = %q, want alice", rev.ReviewedBy)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("audit log Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	approved := byOp["transfer_review_approved"]
	if len(approved) != 2 {
		t.Fatalf("transfer_review_approved events = %d, want 2", len(approved))
	}
	if _, ok := approved[0].Details["delegate"]; ok {
		t.Errorf("direct approval carried delegate attribution: %+v", approved[0].Details)
	}
	if approved[1].Details["delegate"] != "dave" || approved[1].Details["principal"] != "alice" ||
		approved[1].Details["reviewed_by"] != "alice" {
		t.Errorf("delegated approval details = %+v", approved[1].Details)
	}
	delegated := byOp["vote_by_delegate"]
	if len(delegated) != 1 || delegated[0].Details["scope"] != DelegationScopeReview {
		t.Errorf("vote_by_delegate events = %+v, want one review-scope", delegated)
	}
}

func TestDelegatedFlatReviewReject(t *testing.T) {
	l, _ := delegationSeed(t, t.TempDir())
	delegationTransfer(t, l, "tx-d6")
	grantAs(t, l, "alice", DelegationScopeReview, "", "dave", time.Time{})

	rev, changed, err := l.RejectTransferReview("tx-d6", "dave", "duplicate")
	if err != nil || !changed {
		t.Fatalf("delegated reject = %v, %v, want changed", changed, err)
	}
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "alice" {
		t.Errorf("review = %+v, want rejected by alice", rev)
	}
}

func TestDelegationSnapshotRoundTrip(t *testing.T) {
	l := reviewSeed(t, 10000)
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Now().Add(time.Hour))
	grantAs(t, l, "bob", DelegationScopeChain, "tx-1", "erin", time.Time{})
	// A revoked delegation must not survive the round trip.
	grantAs(t, l, "carol", DelegationScopeReview, "", "mallory", time.Time{})
	ctx := ContextWithActor(context.Background(), "carol")
	if _, err := l.RevokeDelegationCtx(ctx, DelegationScopeReview, "", "mallory"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !delegationsEqual(l.ReviewDelegations(), l2.ReviewDelegations()) {
		t.Errorf("delegations after round trip = %+v, want %+v", l2.ReviewDelegations(), l.ReviewDelegations())
	}
	if l2.ReviewDelegationsGranted() != l.ReviewDelegationsGranted() {
		t.Errorf("granted counter after round trip = %d, want %d",
			l2.ReviewDelegationsGranted(), l.ReviewDelegationsGranted())
	}
	if len(l2.ReviewDelegations()) != 2 {
		t.Errorf("restored delegations = %d, want 2 (revoked one dropped)", len(l2.ReviewDelegations()))
	}

	// The restored delegations still back votes: install a quorum and
	// vote through the restored alice -> dave delegation.
	if err := l2.SetReviewQuorumPolicy([]string{"alice", "bob"}, 1, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	l2.mu.RLock()
	_, ok := l2.resolveDelegationLocked(DelegationScopeQuorum, "tx-1", "dave", time.Now())
	l2.mu.RUnlock()
	if !ok {
		t.Error("restored delegation does not resolve")
	}
}

func TestDelegationSnapshotRejectsCorrupt(t *testing.T) {
	l := reviewSeed(t, 10000)
	// One real grant so the delegations fields are present in the
	// exported config line (both are omitempty).
	grantAs(t, l, "alice", DelegationScopeQuorum, "", "dave", time.Time{})
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Inject a cyclic delegation pair into the snapshot JSON: the
	// exporter only ever writes acyclic data, so this is corruption.
	raw := buf.Bytes()
	cycle := `{"scope":"quorum","principal":"x","delegatee":"y","granted_at":"2026-10-19T00:00:00Z"},` +
		`{"scope":"quorum","principal":"y","delegatee":"x","granted_at":"2026-10-19T00:00:00Z"}`
	withCycle := bytes.Replace(raw, []byte(`"review_delegations_granted"`),
		[]byte(`"review_delegations":[`+cycle+`],"review_delegations_granted"`), 1)
	if _, err := ImportSnapshot(bytes.NewReader(withCycle)); err == nil {
		t.Error("ImportSnapshot accepted a cyclic delegation set, want rejection")
	}
}

func TestDelegationConcurrency(t *testing.T) {
	l := reviewSeed(t, 10000)
	if err := l.SetReviewQuorumPolicy([]string{"alice", "bob", "carol"}, 2, true); err != nil {
		t.Fatalf("SetReviewQuorumPolicy: %v", err)
	}
	const n = 8
	for i := 0; i < n; i++ {
		delegationTransfer(t, l, fmt.Sprintf("tx-race-%d", i))
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			delegatee := fmt.Sprintf("dave-%d", i)
			principal := []string{"alice", "bob", "carol"}[i%3]
			tx := fmt.Sprintf("tx-race-%d", i)
			ctx := ContextWithActor(context.Background(), principal)
			if _, err := l.DelegateVoteCtx(ctx, DelegationScopeQuorum, "", delegatee, time.Now().Add(time.Hour)); err != nil {
				t.Errorf("grant %d: %v", i, err)
				return
			}
			// The delegatee's vote counts as the principal's; a
			// second principal's direct vote settles the review.
			if _, err := l.ApproveTransferReview(tx, delegatee); err != nil {
				t.Errorf("delegated vote %d: %v", i, err)
				return
			}
			other := []string{"alice", "bob", "carol"}[(i+1)%3]
			if _, err := l.ApproveTransferReview(tx, other); err != nil {
				t.Errorf("settle vote %d: %v", i, err)
				return
			}
			_ = l.ReviewDelegations()
			_ = l.ReviewDelegationsGranted()
			rctx := ContextWithActor(context.Background(), principal)
			if _, err := l.RevokeDelegationCtx(rctx, DelegationScopeQuorum, "", delegatee); err != nil {
				t.Errorf("revoke %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := l.ReviewDelegationsGranted(); got != n {
		t.Errorf("granted counter = %d, want %d", got, n)
	}
	if got := len(l.ReviewDelegations()); got != 0 {
		t.Errorf("delegations after revokes = %d, want 0", got)
	}
	for i := 0; i < n; i++ {
		rev, ok := l.GetTransferReview(fmt.Sprintf("tx-race-%d", i))
		if !ok || rev.Status != ReviewStatusApproved {
			t.Errorf("tx-race-%d status = %+v, want approved", i, rev)
		}
	}
}
