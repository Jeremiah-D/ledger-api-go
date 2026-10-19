package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// N-of-M multi-approver quorum for dual-control reviews (the fintech
// four-eyes flow, hardened).
//
// LG-42's review is a single-approver decision: one operator identity
// settles or releases the frozen transfer. A quorum policy upgrades
// that to an opt-in approver set (M named approvers) plus an approval
// threshold N: every decision is a named vote recorded in the review,
// and the transfer settles only once N distinct approvers voted
// approve. A single stolen operator credential can no longer move
// money on its own — the attacker would need N of the M approver
// identities.
//
// The policy is opt-in and structural (no version bump, like the
// review thresholds): while no policy is configured, ApproveTransferReview
// and RejectTransferReview behave exactly as LG-42 defines. Once a
// policy is installed, the same entry points become vote entry points:
// an approve call records one named approve vote (settling when the
// tally reaches N), a reject call records one named reject vote.
//
// Reject votes are decisive by configuration (RejectDecisive): when
// true, one reject vote rejects the review immediately, releasing the
// freeze — the single-approver veto of classic four-eyes. When false,
// the reject vote is recorded (visible in the audit trail and the
// reconcile report) but the review stays pending: it can still reach
// the approval threshold, and otherwise lapses through expiry. The
// dissent is never silent either way.
//
// Voting rules, enforced at the ledger layer:
//   - only policy members may vote (ErrQuorumNotMember);
//   - one vote per approver per review: re-voting the same way is
//     idempotent (returns the current tally, settles nothing new,
//     stays silent in the audit trail); voting the opposite way fails
//     with ErrReviewerAlreadyVoted;
//   - an empty approver identity defaults to "operator" first, then
//     the membership check applies — quorum mode requires named
//     approvers, so "operator" votes only when it is a member.
//
// Combination with LG-42, unchanged unless noted:
//   - threshold-triggered review creation is untouched;
//   - ExpireReviews / the ReviewSweeper auto-reject lapsed reviews with
//     their votes intact — the votes are part of the review record, so
//     the terminal trail survives;
//   - approving an already-approved review returns the original
//     approval receipt idempotently (the idempotent replay returns the
//     receipt without the vote tally: the tally described the settling
//     call, not the replay);
//   - policy changes are structural and take effect at decision time:
//     votes already recorded stay on the record (history is never
//     rewritten); new votes are checked against the current policy.
//
// The vote events enter the structured audit chain (LG-32) like every
// other review decision, and /metrics exposes
// ledger_review_quorum_votes_total{approver,vote,outcome}.

// Quorum vote outcomes: what one vote did, reported on the decision
// receipts and on the vote metric.
const (
	// QuorumOutcomeRecorded means the vote was recorded and the review
	// is still pending.
	QuorumOutcomeRecorded = "recorded"
	// QuorumOutcomeSettled means the vote reached the approval
	// threshold and the review settled.
	QuorumOutcomeSettled = "settled"
	// QuorumOutcomeDecisive means a reject vote closed the review
	// immediately (RejectDecisive policy).
	QuorumOutcomeDecisive = "decisive"
	// QuorumOutcomeDuplicate means the call replayed an already-recorded
	// vote: idempotent, settles nothing, audits nothing.
	QuorumOutcomeDuplicate = "duplicate"
)

// Validation and voting errors returned by the quorum APIs.
var (
	// ErrInvalidQuorumPolicy is returned by SetReviewQuorumPolicy (and
	// the snapshot import) for a policy that could never be installed:
	// no approvers, a threshold below 1 or above the approver count, a
	// blank approver identity, or a duplicated approver.
	ErrInvalidQuorumPolicy = errors.New("ledger: invalid review quorum policy")
	// ErrQuorumNotMember is returned when a vote names an approver who
	// is not in the active quorum policy.
	ErrQuorumNotMember = errors.New("ledger: approver is not a review quorum member")
	// ErrReviewerAlreadyVoted is returned when an approver who already
	// cast a vote on a review tries to vote the opposite way. The first
	// vote stands; re-voting the same way is idempotent instead.
	ErrReviewerAlreadyVoted = errors.New("ledger: approver already voted on this review")
)

// QuorumPolicy is the N-of-M approval quorum for dual-control reviews:
// M named approvers, of which ThresholdN distinct approve votes settle
// a review. RejectDecisive selects the reject semantics: true rejects
// the review on the first reject vote; false records reject votes as
// dissent while the review stays pending (approval or expiry still
// terminates it).
type QuorumPolicy struct {
	Approvers      []string `json:"approvers"`
	ThresholdN     int      `json:"threshold_n"`
	RejectDecisive bool     `json:"reject_decisive"`
}

// ReviewQuorumTally is the live vote count of a review under a quorum
// policy: the named votes in vote order plus the policy parameters they
// are counted against. LastVoter and Outcome describe the vote call the
// tally is attached to (see the QuorumOutcome constants).
type ReviewQuorumTally struct {
	ApproveVotes []string `json:"approve_votes"`
	RejectVotes  []string `json:"reject_votes"`
	ThresholdN   int      `json:"threshold_n"`
	MemberCount  int      `json:"member_count"`
	LastVoter    string   `json:"last_voter"`
	Outcome      string   `json:"outcome"`
}

// validateQuorumPolicy checks a policy structurally: at least one
// approver, 1 <= ThresholdN <= len(Approvers), non-blank unique
// approver identities. A nil policy is valid (quorum disabled).
func validateQuorumPolicy(p *QuorumPolicy) error {
	if p == nil {
		return nil
	}
	if len(p.Approvers) == 0 {
		return fmt.Errorf("needs at least one approver")
	}
	if p.ThresholdN < 1 {
		return fmt.Errorf("threshold_n must be >= 1, got %d", p.ThresholdN)
	}
	if p.ThresholdN > len(p.Approvers) {
		return fmt.Errorf("threshold_n %d exceeds approver count %d", p.ThresholdN, len(p.Approvers))
	}
	seen := make(map[string]bool, len(p.Approvers))
	for _, a := range p.Approvers {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("approver identity must not be blank")
		}
		if seen[a] {
			return fmt.Errorf("duplicate approver %q", a)
		}
		seen[a] = true
	}
	return nil
}

// quorumPolicyError wraps a structural validation failure in
// ErrInvalidQuorumPolicy so callers can match it with errors.Is.
func quorumPolicyError(err error) error {
	return fmt.Errorf("%w: %s", ErrInvalidQuorumPolicy, err)
}

// WithReviewQuorumPolicy installs an N-of-M quorum policy at
// construction time (see SetReviewQuorumPolicy). An invalid policy
// panics — fail-fast at construction, like the review threshold, so a
// misconfigured four-eyes control can never silently run weakened.
func WithReviewQuorumPolicy(p QuorumPolicy) Option {
	if err := validateQuorumPolicy(&p); err != nil {
		panic(quorumPolicyError(err))
	}
	return func(l *Ledger) {
		l.reviewQuorum = &QuorumPolicy{
			Approvers:      append([]string(nil), p.Approvers...),
			ThresholdN:     p.ThresholdN,
			RejectDecisive: p.RejectDecisive,
		}
	}
}

// SetReviewQuorumPolicy installs (or replaces) the N-of-M quorum policy
// for dual-control reviews: approvers is the M named approver
// identities, thresholdN the number of distinct approve votes that
// settle a review, rejectDecisive the reject semantics (see
// QuorumPolicy). Installing the policy is structural, like
// SetReviewThreshold: it does not bump the ledger version.
//
// While the policy is active, ApproveTransferReview records a named
// approve vote per call (settling at N votes) and
// RejectTransferReview records a named reject vote per call (rejecting
// immediately when rejectDecisive is true). Clearing the policy with
// ClearReviewQuorumPolicy restores the LG-42 single-decision behavior
// for later decisions; votes already recorded stay on their reviews.
func (l *Ledger) SetReviewQuorumPolicy(approvers []string, thresholdN int, rejectDecisive bool) error {
	p := &QuorumPolicy{
		Approvers:      append([]string(nil), approvers...),
		ThresholdN:     thresholdN,
		RejectDecisive: rejectDecisive,
	}
	if err := validateQuorumPolicy(p); err != nil {
		return quorumPolicyError(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reviewQuorum = p
	return nil
}

// ClearReviewQuorumPolicy removes the quorum policy: later review
// decisions go back to the LG-42 single-operator path. Reviews that
// already carry votes keep them — history is never rewritten.
func (l *Ledger) ClearReviewQuorumPolicy() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reviewQuorum = nil
}

// ReviewQuorumPolicy reports the active quorum policy. The returned
// policy is a copy; mutating it does not affect the ledger. The second
// return value is false when no policy is configured.
func (l *Ledger) ReviewQuorumPolicy() (QuorumPolicy, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.reviewQuorum == nil {
		return QuorumPolicy{}, false
	}
	out := *l.reviewQuorum
	out.Approvers = append([]string(nil), l.reviewQuorum.Approvers...)
	return out, true
}

// copyQuorumPolicy deep-copies a policy, preserving nil.
func copyQuorumPolicy(p *QuorumPolicy) *QuorumPolicy {
	if p == nil {
		return nil
	}
	out := *p
	out.Approvers = append([]string(nil), p.Approvers...)
	return &out
}

// reviewQuorumCopyLocked returns a copy of the active policy, or nil
// when none is configured. Callers must hold l.mu.
func (l *Ledger) reviewQuorumCopyLocked() *QuorumPolicy {
	return copyQuorumPolicy(l.reviewQuorum)
}

// equalQuorumVotes compares two named-vote lists in order: vote order
// is meaningful (it is the decision sequence), so order matters.
func equalQuorumVotes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// quorumHasApprover reports whether approver is a member of the policy.
// Callers must hold l.mu.
func quorumHasApprover(p *QuorumPolicy, approver string) bool {
	for _, a := range p.Approvers {
		if a == approver {
			return true
		}
	}
	return false
}

// quorumStringContains reports slice membership for the vote lists.
// Callers must hold l.mu.
func quorumStringContains(votes []string, approver string) bool {
	for _, v := range votes {
		if v == approver {
			return true
		}
	}
	return false
}

// quorumReviewedByLocked derives the review's ReviewedBy identity from
// the approve votes that settled it: the N approver identities, sorted
// for determinism, joined with commas. Callers must hold l.mu.
func quorumReviewedByLocked(r *TransferReview) string {
	voters := append([]string(nil), r.ApproveVotes...)
	sort.Strings(voters)
	return strings.Join(voters, ",")
}

// pendingQuorumReceiptLocked builds the receipt returned for an approve
// vote that did not settle the review (or replayed one): the review is
// still pending, and the tally discloses the current vote count. The
// Duplicate flag is set by the caller for idempotent replays. Callers
// must hold l.mu.
func pendingQuorumReceiptLocked(r *TransferReview, qp *QuorumPolicy, voter, outcome string) TransferReceipt {
	return TransferReceipt{
		TransferID:   r.TransferID,
		Entries:      append([]JournalEntry(nil), r.Entries...),
		FeeCents:     r.FeeCents,
		FeeTierIndex: r.FeeTierIndex,
		FeeRateBps:   r.FeeRateBps,
		FX:           r.FX,
		ReviewStatus: ReviewStatusPending,
		// The fee-split breakdown rebuilds from the frozen legs'
		// memos, like an idempotent replay (see fee_split.go).
		FeeSplits: feeSplitSharesFromLegs(r.TransferID, r.Entries),
		QuorumTally: &ReviewQuorumTally{
			ApproveVotes: append([]string(nil), r.ApproveVotes...),
			RejectVotes:  append([]string(nil), r.RejectVotes...),
			ThresholdN:   qp.ThresholdN,
			MemberCount:  len(qp.Approvers),
			LastVoter:    voter,
			Outcome:      outcome,
		},
	}
}

// settledQuorumTallyLocked builds the tally attached to the settlement
// receipt when a vote reached the threshold. Callers must hold l.mu.
func settledQuorumTallyLocked(r *TransferReview, qp *QuorumPolicy, voter string) *ReviewQuorumTally {
	return &ReviewQuorumTally{
		ApproveVotes: append([]string(nil), r.ApproveVotes...),
		RejectVotes:  append([]string(nil), r.RejectVotes...),
		ThresholdN:   qp.ThresholdN,
		MemberCount:  len(qp.Approvers),
		LastVoter:    voter,
		Outcome:      QuorumOutcomeSettled,
	}
}

// emitQuorumVoteAuditLocked records one named vote in the structured
// audit chain: who voted, which way, and the resulting tally against
// the policy parameters. Votes move no money, so the version is
// unchanged on both sides. actor is the decision entrypoint
// ("ApproveTransferReview" or "RejectTransferReview"). deleg carries
// the delegation the vote was cast through (LG-138), or nil for a
// direct vote: a delegated vote is recorded as the principal's and
// carries delegate+principal in the details. Callers must hold l.mu.
func (l *Ledger) emitQuorumVoteAuditLocked(ctx context.Context, actor string, r *TransferReview, voter, vote string, qp *QuorumPolicy, deleg *voteDelegation) {
	details := map[string]any{
		"voter":         voter,
		"vote":          vote,
		"approve_votes": len(r.ApproveVotes),
		"reject_votes":  len(r.RejectVotes),
		"threshold_n":   qp.ThresholdN,
		"member_count":  len(qp.Approvers),
	}
	if deleg != nil {
		details["delegate"] = deleg.delegate
		details["principal"] = deleg.principal
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_vote",
		Actor:         actor,
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details:       details,
	})
	if deleg != nil {
		l.emitVoteByDelegateLocked(ctx, actor, vote, r, deleg)
	}
}

// castQuorumApproveVoteLocked records one named approve vote on a
// pending review and settles it once the tally reaches the policy
// threshold. The review lookup, the terminal-state handling, and the
// expiry check already happened in ApproveTransferReviewCtx.
//
// LG-138: a non-member holding an active quorum-scope delegation votes
// through it — the vote is recorded as the principal's (the
// one-vote-per-approver rule applies to the principal), with
// delegate+principal in the audit trail. The principal must itself be
// a policy member; otherwise the vote still fails with
// ErrQuorumNotMember. Callers must hold the write lock.
func (l *Ledger) castQuorumApproveVoteLocked(ctx context.Context, r *TransferReview, approver string, qp *QuorumPolicy, now time.Time) (TransferReceipt, error) {
	// voter is the identity the vote is recorded as; deleg is non-nil
	// when the vote was cast through a delegation. The tally's
	// LastVoter keeps the actual caller (the delegate), while the
	// named vote lists carry the principal.
	voter := approver
	var deleg *voteDelegation
	if !quorumHasApprover(qp, approver) {
		res, ok := l.resolveDelegationLocked(DelegationScopeQuorum, r.TransferID, approver, now)
		if !ok || !quorumHasApprover(qp, res.principal) {
			return TransferReceipt{}, ErrQuorumNotMember
		}
		voter, deleg = res.principal, res
	}
	if quorumStringContains(r.RejectVotes, voter) {
		// The voter already voted reject: the first vote stands.
		return TransferReceipt{}, ErrReviewerAlreadyVoted
	}
	if quorumStringContains(r.ApproveVotes, voter) {
		// Idempotent replay of the recorded approve vote: the current
		// tally, marked as a replay. It settles nothing and stays
		// silent in the audit trail, like every idempotent replay.
		receipt := pendingQuorumReceiptLocked(r, qp, approver, QuorumOutcomeDuplicate)
		receipt.Duplicate = true
		return receipt, nil
	}
	r.ApproveVotes = append(r.ApproveVotes, voter)
	l.reviews[r.TransferID] = r
	l.emitQuorumVoteAuditLocked(ctx, "ApproveTransferReview", r, voter, "approve", qp, deleg)
	if len(r.ApproveVotes) >= qp.ThresholdN {
		// Quorum reached: settle through the LG-42 settlement path —
		// the frozen legs flip to effective exactly like a
		// single-operator approval — with the final tally disclosed on
		// the receipt.
		return l.settleReviewLocked(ctx, r, quorumReviewedByLocked(r), now, settledQuorumTallyLocked(r, qp, approver), deleg)
	}
	return pendingQuorumReceiptLocked(r, qp, approver, QuorumOutcomeRecorded), nil
}

// castQuorumRejectVoteLocked records one named reject vote on a pending
// review. With RejectDecisive the vote closes the review immediately
// through the LG-42 rejection path (the vote is recorded first, so the
// dissent trail survives); without it the vote is recorded as dissent
// and the review stays pending — approval or expiry still terminates
// it. The review lookup and terminal-state handling already happened in
// RejectTransferReviewCtx.
//
// LG-138: a non-member holding an active quorum-scope delegation
// rejects through it — the vote is recorded as the principal's, with
// delegate+principal in the audit trail. Callers must hold the write
// lock.
func (l *Ledger) castQuorumRejectVoteLocked(ctx context.Context, r *TransferReview, reviewer, reason string, qp *QuorumPolicy) (review *TransferReview, changed bool, err error) {
	// voter is the identity the vote is recorded as; deleg is non-nil
	// when the vote was cast through a delegation.
	voter := reviewer
	var deleg *voteDelegation
	if !quorumHasApprover(qp, reviewer) {
		res, ok := l.resolveDelegationLocked(DelegationScopeQuorum, r.TransferID, reviewer, time.Now())
		if !ok || !quorumHasApprover(qp, res.principal) {
			return nil, false, ErrQuorumNotMember
		}
		voter, deleg = res.principal, res
	}
	if quorumStringContains(r.ApproveVotes, voter) {
		// The voter already voted approve: the first vote stands.
		return nil, false, ErrReviewerAlreadyVoted
	}
	if quorumStringContains(r.RejectVotes, voter) {
		// Idempotent replay of the recorded reject vote: the record
		// as-is, unchanged, and silent — like the terminal-state
		// replay in RejectTransferReviewCtx.
		out := *r
		return &out, false, nil
	}
	r.RejectVotes = append(r.RejectVotes, voter)
	l.reviews[r.TransferID] = r
	l.emitQuorumVoteAuditLocked(ctx, "RejectTransferReview", r, voter, "reject", qp, deleg)
	if qp.RejectDecisive {
		// One reject vote is decisive: reject through the standard
		// path, which records the decisive reviewer, releases the
		// freeze, and emits the rejection audit event and outbox
		// envelope. The recorded vote stays on the review.
		return l.rejectReviewLocked(ctx, r, voter, reason, deleg)
	}
	// Recorded but not decisive: the review stays pending and the
	// freeze stays in place. The recorded vote is a state change
	// (changed == true) even though the status is untouched — the
	// caller distinguishes "vote recorded" from "review rejected" by
	// the returned status.
	out := *r
	return &out, true, nil
}
