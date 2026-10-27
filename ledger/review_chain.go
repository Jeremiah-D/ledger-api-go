package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Sequential approval chain for dual-control reviews (LG-126, the
// fintech tiered-approval flow: maker -> checker -> controller).
//
// LG-42's review is a single-approver decision and LG-124 upgrades it
// to a parallel N-of-M quorum. An approval chain sequences that idea:
// the review advances through ordered stages, each stage its own
// approver set (M) and threshold (N). The review settles only when the
// last stage reaches its threshold; a reject vote at any stage closes
// the review immediately — a reject is decisive everywhere, no
// configuration knob.
//
// The chain is opt-in and structural (no version bump, like the quorum
// policy and the review thresholds): while no chain is configured,
// ApproveTransferReview and RejectTransferReview behave exactly as
// LG-42/LG-124 define. Once a chain is installed, the same entry points
// become per-stage vote entry points. The chain supersedes the flat
// quorum policy while configured: a review cannot be decided by two
// parallel voting schemes at once.
//
// Stage flow, enforced at the ledger layer:
//   - the review advances stage by stage: votes on the current stage
//     only; a vote that reaches the stage threshold advances the
//     review to the next stage (audit event
//     transfer_review_chain_stage_advanced); the last stage's
//     threshold settles through the LG-42 settlement path;
//   - every vote is named and enters the structured audit chain
//     (transfer_review_chain_vote) with its stage number, so the
//     "who approved which rung" trail is tamper-evident; the per-stage
//     votes ride the review record, the snapshot, and the reconcile
//     report, like the LG-124 votes;
//   - only current-stage members may vote (ErrQuorumNotMember); one
//     vote per approver per stage: re-voting the same way is idempotent
//     (returns the current tally, settles nothing, audits nothing);
//     voting the opposite way fails with ErrReviewerAlreadyVoted;
//   - a reject vote at any stage rejects the review at once through
//     the LG-42 rejection path (the recorded vote stays on the review,
//     so the dissent trail survives);
//
// Stage timeouts are opt-in per stage (Timeout): a stage that waits
// longer than its timeout auto-rejects the review — funds can never
// strand on an approver who went silent. Timeout evaluation is lazy by
// predicate (a timed-out stage already counts as inactive for
// available-balance purposes) plus an explicit sweep in ExpireReviews,
// like review-level expiry. Zero means the stage never times out.
//
// Policy changes are structural and take effect at decision time: votes
// already recorded stay on the record (history is never rewritten);
// clearing the chain restores the LG-42/LG-124 decision path for later
// decisions, and reviews mid-chain keep their recorded votes.

// Chain vote outcomes: what one vote did, reported on the decision
// receipts. "recorded" and "settled" and "duplicate" reuse the LG-124
// outcome vocabulary; "advanced" is new: the vote reached the stage
// threshold and the review moved to the next stage.
const (
	// ChainOutcomeAdvanced means the vote reached the stage threshold
	// and the review advanced to the next stage.
	ChainOutcomeAdvanced = "advanced"
)

// Chain vote errors. Member and double-vote semantics reuse the LG-124
// sentinels; the range error is chain-specific.
var (
	// ErrInvalidApprovalChain is returned by SetApprovalChain (and the
	// snapshot import) for a chain that could never be installed: no
	// stages, a stage with no approvers, a threshold below 1 or above
	// the stage's approver count, a blank or duplicated approver, or a
	// negative stage timeout.
	ErrInvalidApprovalChain = errors.New("ledger: invalid review approval chain")
	// ErrApprovalChainStageRange is returned when a vote targets a
	// review whose recorded stage no longer exists in the active chain
	// (the chain was replaced with fewer stages mid-flight). The votes
	// already recorded stay; the operator must re-install a compatible
	// chain to continue.
	ErrApprovalChainStageRange = errors.New("ledger: review chain stage out of range")
)

// ApprovalChainStage is one rung of a sequential approval chain: M
// named approvers of which ThresholdN distinct approve votes advance
// the review (or settle it, on the last stage). Timeout is the opt-in
// stage lifetime: a stage that waits longer than Timeout auto-rejects
// the review. Zero means the stage never times out on its own.
type ApprovalChainStage struct {
	Approvers  []string      `json:"approvers"`
	ThresholdN int           `json:"threshold_n"`
	Timeout    time.Duration `json:"timeout,omitempty"`
}

// ApprovalChain is the sequential approval-chain policy: the ordered
// stages a dual-control review advances through. The review settles
// only when the last stage reaches its threshold.
type ApprovalChain struct {
	Stages []ApprovalChainStage `json:"stages"`
}

// ChainStageVotes is the named-vote record of one chain stage:
// approver identities in vote order. Order matters — it is the
// decision sequence.
type ChainStageVotes struct {
	ApproveVotes []string `json:"approve_votes"`
	RejectVotes  []string `json:"reject_votes"`
}

// ReviewChainProgress is the review's position in its approval chain:
// the current stage index, when the stage started (for the opt-in
// stage timeout), and the per-stage named votes. It rides the review
// record through snapshots and the reconcile report.
type ReviewChainProgress struct {
	Stage          int               `json:"stage"`
	StageStartedAt time.Time         `json:"stage_started_at"`
	Votes          []ChainStageVotes `json:"votes"`
}

// ReviewChainTally is the live per-stage vote count disclosed on the
// decision receipts, like ReviewQuorumTally for the flat LG-124 quorum.
// LastVoter and Outcome describe the vote call the tally is attached to
// (see the QuorumOutcome constants and ChainOutcomeAdvanced).
type ReviewChainTally struct {
	Stage        int      `json:"stage"`
	StageCount   int      `json:"stage_count"`
	ApproveVotes []string `json:"approve_votes"`
	RejectVotes  []string `json:"reject_votes"`
	ThresholdN   int      `json:"threshold_n"`
	MemberCount  int      `json:"member_count"`
	LastVoter    string   `json:"last_voter"`
	Outcome      string   `json:"outcome"`
}

// validateApprovalChain checks a chain structurally: at least one
// stage; each stage at least one approver, 1 <= ThresholdN <=
// len(Approvers), non-blank unique approver identities, non-negative
// timeout. A nil chain is valid (chain disabled).
func validateApprovalChain(c *ApprovalChain) error {
	if c == nil {
		return nil
	}
	if len(c.Stages) == 0 {
		return fmt.Errorf("needs at least one stage")
	}
	for i := range c.Stages {
		s := &c.Stages[i]
		if len(s.Approvers) == 0 {
			return fmt.Errorf("stage %d: needs at least one approver", i)
		}
		if s.ThresholdN < 1 {
			return fmt.Errorf("stage %d: threshold_n must be >= 1, got %d", i, s.ThresholdN)
		}
		if s.ThresholdN > len(s.Approvers) {
			return fmt.Errorf("stage %d: threshold_n %d exceeds approver count %d", i, s.ThresholdN, len(s.Approvers))
		}
		if s.Timeout < 0 {
			return fmt.Errorf("stage %d: timeout must not be negative", i)
		}
		seen := make(map[string]bool, len(s.Approvers))
		for _, a := range s.Approvers {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("stage %d: approver identity must not be blank", i)
			}
			if seen[a] {
				return fmt.Errorf("stage %d: duplicate approver %q", i, a)
			}
			seen[a] = true
		}
	}
	return nil
}

// approvalChainError wraps a structural validation failure in
// ErrInvalidApprovalChain so callers can match it with errors.Is.
func approvalChainError(err error) error {
	return fmt.Errorf("%w: %s", ErrInvalidApprovalChain, err)
}

// WithApprovalChain installs a sequential approval-chain policy at
// construction time (see SetApprovalChain). An invalid chain panics —
// fail-fast at construction, like the quorum policy, so a misconfigured
// approval control can never silently run weakened.
func WithApprovalChain(c ApprovalChain) Option {
	if err := validateApprovalChain(&c); err != nil {
		panic(approvalChainError(err))
	}
	return func(l *Ledger) {
		l.approvalChain = copyApprovalChain(&c)
	}
}

// SetApprovalChain installs (or replaces) the sequential
// approval-chain policy for dual-control reviews. Installing is
// structural, like SetReviewQuorumPolicy: it does not bump the ledger
// version, and it round-trips through snapshots.
//
// While the chain is active, ApproveTransferReview records a named
// approve vote on the review's current stage (advancing the review when
// the stage threshold is reached, settling on the last stage) and
// RejectTransferReview records a named reject vote that closes the
// review immediately. Clearing with ClearApprovalChain restores the
// LG-42/LG-124 decision path; votes already recorded stay on their
// reviews.
func (l *Ledger) SetApprovalChain(c ApprovalChain) error {
	if err := validateApprovalChain(&c); err != nil {
		return approvalChainError(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.approvalChain = copyApprovalChain(&c)
	return nil
}

// ClearApprovalChain removes the approval-chain policy: later review
// decisions go back to the LG-42 single-operator (or LG-124 quorum)
// path. Reviews that already carry chain votes keep them — history is
// never rewritten.
func (l *Ledger) ClearApprovalChain() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.approvalChain = nil
}

// ApprovalChain reports the active approval-chain policy. The returned
// policy is a copy; mutating it does not affect the ledger. The second
// return value is false when no chain is configured.
func (l *Ledger) ApprovalChain() (ApprovalChain, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.approvalChain == nil {
		return ApprovalChain{}, false
	}
	return *copyApprovalChain(l.approvalChain), true
}

// copyApprovalChain deep-copies a chain, preserving nil.
func copyApprovalChain(c *ApprovalChain) *ApprovalChain {
	if c == nil {
		return nil
	}
	out := &ApprovalChain{Stages: make([]ApprovalChainStage, len(c.Stages))}
	for i, s := range c.Stages {
		out.Stages[i] = ApprovalChainStage{
			Approvers:  append([]string(nil), s.Approvers...),
			ThresholdN: s.ThresholdN,
			Timeout:    s.Timeout,
		}
	}
	return out
}

// approvalChainCopyLocked returns a copy of the active chain, or nil
// when none is configured. Callers must hold l.mu.
func (l *Ledger) approvalChainCopyLocked() *ApprovalChain {
	return copyApprovalChain(l.approvalChain)
}

// copyChainProgress deep-copies a chain progress record, preserving
// nil.
func copyChainProgress(p *ReviewChainProgress) *ReviewChainProgress {
	if p == nil {
		return nil
	}
	out := &ReviewChainProgress{
		Stage:          p.Stage,
		StageStartedAt: p.StageStartedAt,
		Votes:          make([]ChainStageVotes, len(p.Votes)),
	}
	for i, v := range p.Votes {
		out.Votes[i] = ChainStageVotes{
			ApproveVotes: append([]string(nil), v.ApproveVotes...),
			RejectVotes:  append([]string(nil), v.RejectVotes...),
		}
	}
	return out
}

// chainProgressEqual compares two chain progress records: stage,
// stage start, and the per-stage named votes in order.
func chainProgressEqual(a, b *ReviewChainProgress) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Stage != b.Stage || a.StageStartedAt.UnixNano() != b.StageStartedAt.UnixNano() ||
		len(a.Votes) != len(b.Votes) {
		return false
	}
	for i := range a.Votes {
		if !equalQuorumVotes(a.Votes[i].ApproveVotes, b.Votes[i].ApproveVotes) ||
			!equalQuorumVotes(a.Votes[i].RejectVotes, b.Votes[i].RejectVotes) {
			return false
		}
	}
	return true
}

// chainStageTimedOut reports whether the review's current chain stage
// has lapsed: the review carries chain progress, the active chain
// still has that stage, the stage has a positive timeout, and now is
// at or past the stage start plus the timeout. Timeout evaluation is
// lazy by predicate — a timed-out stage already counts as inactive for
// available-balance purposes, even before ExpireReviews sweeps it.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) chainStageTimedOut(r *TransferReview, now time.Time) bool {
	p := r.ChainProgress
	c := l.approvalChain
	if p == nil || c == nil {
		return false
	}
	if p.Stage < 0 || p.Stage >= len(c.Stages) {
		return false
	}
	timeout := c.Stages[p.Stage].Timeout
	if timeout <= 0 {
		return false
	}
	return !now.Before(p.StageStartedAt.Add(timeout))
}

// bindChainProgressLocked ensures the review carries a progress record
// bound to the active chain: a fresh review starts at stage 0, and a
// review whose votes were recorded under a different-length chain is
// re-bound preserving the votes of the overlapping stages. Callers
// must hold the write lock.
func (l *Ledger) bindChainProgressLocked(r *TransferReview, c *ApprovalChain, now time.Time) *ReviewChainProgress {
	p := r.ChainProgress
	if p == nil {
		p = &ReviewChainProgress{
			Stage:          0,
			StageStartedAt: now,
			Votes:          make([]ChainStageVotes, len(c.Stages)),
		}
		r.ChainProgress = p
		return p
	}
	if len(p.Votes) != len(c.Stages) {
		nv := make([]ChainStageVotes, len(c.Stages))
		copy(nv, p.Votes)
		p.Votes = nv
	}
	return p
}

// pendingChainReceiptLocked builds the receipt returned for a chain
// vote that did not settle the review: the review is still pending,
// and the tally discloses the current stage's vote count. The
// Duplicate flag is set by the caller for idempotent replays. Callers
// must hold l.mu.
func pendingChainReceiptLocked(r *TransferReview, c *ApprovalChain, p *ReviewChainProgress, voter, outcome string) TransferReceipt {
	stage := c.Stages[p.Stage]
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
		ChainTally: &ReviewChainTally{
			Stage:        p.Stage,
			StageCount:   len(c.Stages),
			ApproveVotes: append([]string(nil), p.Votes[p.Stage].ApproveVotes...),
			RejectVotes:  append([]string(nil), p.Votes[p.Stage].RejectVotes...),
			ThresholdN:   stage.ThresholdN,
			MemberCount:  len(stage.Approvers),
			LastVoter:    voter,
			Outcome:      outcome,
		},
	}
}

// chainSettledReviewerLocked derives the review's ReviewedBy identity
// from the approve votes that settled the final stage: the approver
// identities, sorted for determinism, joined with commas — like
// quorumReviewedByLocked for the flat quorum. Callers must hold l.mu.
func chainSettledReviewerLocked(r *TransferReview, p *ReviewChainProgress) string {
	voters := append([]string(nil), p.Votes[p.Stage].ApproveVotes...)
	sort.Strings(voters)
	return strings.Join(voters, ",")
}

// emitChainVoteAuditLocked records one named chain vote in the
// structured audit chain: who voted, which way, on which stage, and
// the resulting tally against the stage parameters. Votes move no
// money, so the version is unchanged on both sides. actor is the
// decision entrypoint ("ApproveTransferReview" or
// "RejectTransferReview"). deleg carries the delegation the vote was
// cast through (LG-138), or nil for a direct vote: a delegated vote
// is recorded as the principal's and carries delegate+principal in
// the details. Callers must hold l.mu.
func (l *Ledger) emitChainVoteAuditLocked(ctx context.Context, actor string, r *TransferReview, p *ReviewChainProgress, voter, vote string, c *ApprovalChain, deleg *voteDelegation) {
	details := map[string]any{
		"stage":        p.Stage,
		"stage_count":  len(c.Stages),
		"voter":        voter,
		"vote":         vote,
		"approve_votes": len(p.Votes[p.Stage].ApproveVotes),
		"reject_votes":  len(p.Votes[p.Stage].RejectVotes),
		"threshold_n":   c.Stages[p.Stage].ThresholdN,
		"member_count":  len(c.Stages[p.Stage].Approvers),
	}
	if deleg != nil {
		details["delegate"] = deleg.delegate
		details["principal"] = deleg.principal
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_chain_vote",
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

// timeoutChainStageLocked marks one review rejected for a lapsed chain
// stage, releasing its frozen funds. The votes stay on the review —
// the trail shows who voted before the stage went silent. Callers must
// hold the write lock; the audit event is emitted by the caller.
func (l *Ledger) timeoutChainStageLocked(r *TransferReview, now time.Time) {
	r.Status = ReviewStatusRejected
	r.ReviewedBy = "system"
	r.ReviewedAt = now
	// LG-232: a timed-out stage decides the review — clear the stamp.
	r.EscalatedAt = time.Time{}
	l.reviews[r.TransferID] = r
}

// castChainApproveVoteLocked records one named approve vote on the
// review's current chain stage and advances the review when the stage
// threshold is reached — settling through the LG-42 settlement path
// on the last stage. The review lookup, the terminal-state handling,
// and the review-level expiry check already happened in
// ApproveTransferReviewCtx.
//
// LG-138: a non-member holding an active chain-scope delegation votes
// through it — the vote is recorded as the principal's (the
// one-vote-per-approver-per-stage rule applies to the principal),
// with delegate+principal in the audit trail. The principal must
// itself be a current-stage member; otherwise the vote still fails
// with ErrQuorumNotMember. Callers must hold the write lock.
func (l *Ledger) castChainApproveVoteLocked(ctx context.Context, r *TransferReview, approver string, c *ApprovalChain, now time.Time) (TransferReceipt, error) {
	p := l.bindChainProgressLocked(r, c, now)
	if p.Stage < 0 || p.Stage >= len(c.Stages) {
		return TransferReceipt{}, fmt.Errorf("stage %d: %w", p.Stage, ErrApprovalChainStageRange)
	}
	stage := c.Stages[p.Stage]
	// A stage that lapsed while nobody was watching rejects the review
	// first — the operator can never approve past a silent approver.
	if l.chainStageTimedOut(r, now) {
		l.timeoutChainStageLocked(r, now)
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "transfer_review_chain_stage_timeout",
			Actor:         "ApproveTransferReview",
			TraceID:       r.TransferID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
			Details: map[string]any{
				"stage":       p.Stage,
				"stage_count": len(c.Stages),
				"timeout":     stage.Timeout.String(),
			},
		})
		return TransferReceipt{}, ErrReviewExpired
	}
	// voter is the identity the vote is recorded as; deleg is non-nil
	// when the vote was cast through a delegation. The tally's
	// LastVoter keeps the actual caller (the delegate), while the
	// per-stage vote lists carry the principal.
	voter := approver
	var deleg *voteDelegation
	if !quorumStringContains(stage.Approvers, approver) {
		res, ok := l.resolveDelegationLocked(DelegationScopeChain, r.TransferID, approver, now)
		if !ok || !quorumStringContains(stage.Approvers, res.principal) {
			return TransferReceipt{}, fmt.Errorf("stage %d: %w", p.Stage, ErrQuorumNotMember)
		}
		voter, deleg = res.principal, res
	}
	if quorumStringContains(p.Votes[p.Stage].RejectVotes, voter) {
		// The voter already voted reject on this stage: the first
		// vote stands.
		return TransferReceipt{}, ErrReviewerAlreadyVoted
	}
	if quorumStringContains(p.Votes[p.Stage].ApproveVotes, voter) {
		// Idempotent replay of the recorded approve vote: the current
		// tally, marked as a replay. It settles nothing and stays
		// silent in the audit trail, like every idempotent replay.
		receipt := pendingChainReceiptLocked(r, c, p, approver, QuorumOutcomeDuplicate)
		receipt.Duplicate = true
		return receipt, nil
	}
	p.Votes[p.Stage].ApproveVotes = append(p.Votes[p.Stage].ApproveVotes, voter)
	l.reviews[r.TransferID] = r
	l.emitChainVoteAuditLocked(ctx, "ApproveTransferReview", r, p, voter, "approve", c, deleg)
	if len(p.Votes[p.Stage].ApproveVotes) >= stage.ThresholdN {
		if p.Stage == len(c.Stages)-1 {
			// Final stage reached: settle through the LG-42
			// settlement path — the frozen legs flip to effective
			// exactly like a single-operator approval.
			return l.settleReviewLocked(ctx, r, chainSettledReviewerLocked(r, p), now, nil, deleg)
		}
		// Stage threshold reached: advance to the next stage. The
		// votes of the settled stage stay on the review.
		from := p.Stage
		p.Stage++
		p.StageStartedAt = now
		l.reviews[r.TransferID] = r
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "transfer_review_chain_stage_advanced",
			Actor:         "ApproveTransferReview",
			TraceID:       r.TransferID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
			Details: map[string]any{
				"from_stage":  from,
				"to_stage":    p.Stage,
				"stage_count": len(c.Stages),
				"voter":       approver,
			},
		})
		return pendingChainReceiptLocked(r, c, p, approver, ChainOutcomeAdvanced), nil
	}
	return pendingChainReceiptLocked(r, c, p, approver, QuorumOutcomeRecorded), nil
}

// castChainRejectVoteLocked records one named reject vote on the
// review's current chain stage and closes the review immediately — a
// reject is decisive at every stage. The review lookup and
// terminal-state handling already happened in
// RejectTransferReviewCtx.
//
// LG-138: a non-member holding an active chain-scope delegation
// rejects through it — the vote is recorded as the principal's, with
// delegate+principal in the audit trail. Callers must hold the write
// lock.
func (l *Ledger) castChainRejectVoteLocked(ctx context.Context, r *TransferReview, reviewer, reason string, c *ApprovalChain, now time.Time) (review *TransferReview, changed bool, err error) {
	p := l.bindChainProgressLocked(r, c, now)
	if p.Stage < 0 || p.Stage >= len(c.Stages) {
		return nil, false, fmt.Errorf("stage %d: %w", p.Stage, ErrApprovalChainStageRange)
	}
	stage := c.Stages[p.Stage]
	// voter is the identity the vote is recorded as; deleg is non-nil
	// when the vote was cast through a delegation.
	voter := reviewer
	var deleg *voteDelegation
	if !quorumStringContains(stage.Approvers, reviewer) {
		res, ok := l.resolveDelegationLocked(DelegationScopeChain, r.TransferID, reviewer, now)
		if !ok || !quorumStringContains(stage.Approvers, res.principal) {
			return nil, false, fmt.Errorf("stage %d: %w", p.Stage, ErrQuorumNotMember)
		}
		voter, deleg = res.principal, res
	}
	if quorumStringContains(p.Votes[p.Stage].ApproveVotes, voter) {
		// The voter already voted approve on this stage: the first
		// vote stands.
		return nil, false, ErrReviewerAlreadyVoted
	}
	if quorumStringContains(p.Votes[p.Stage].RejectVotes, voter) {
		// Idempotent replay of the recorded reject vote: the record
		// as-is, unchanged, and silent — like the terminal-state
		// replay in RejectTransferReviewCtx.
		out := *r
		return &out, false, nil
	}
	p.Votes[p.Stage].RejectVotes = append(p.Votes[p.Stage].RejectVotes, voter)
	l.reviews[r.TransferID] = r
	l.emitChainVoteAuditLocked(ctx, "RejectTransferReview", r, p, voter, "reject", c, deleg)
	// One reject vote is decisive at every stage: reject through the
	// standard path, which records the decisive reviewer, releases the
	// freeze, and emits the rejection audit event and outbox envelope.
	// The recorded vote stays on the review.
	return l.rejectReviewLocked(ctx, r, voter, reason, deleg)
}

// expireChainStagesLocked marks every pending review whose current
// chain stage has timed out as rejected, releasing its frozen funds,
// and returns how many were marked. Reviews without chain progress —
// or with the chain since cleared — are untouched. Callers must hold
// the write lock; the batched audit event is emitted by the caller.
func (l *Ledger) expireChainStagesLocked(now time.Time) (marked int, ids []string) {
	c := l.approvalChain
	if c == nil {
		return 0, nil
	}
	for _, r := range l.reviews {
		if r.Status == ReviewStatusPending && l.chainStageTimedOut(r, now) {
			l.timeoutChainStageLocked(r, now)
			marked++
			ids = append(ids, r.TransferID)
			// Domain outbox: one review_rejected per review — the
			// timeout releases the reserved funds back to available,
			// a domain fact, exactly like a decisive reject vote.
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxReviewRejected,
				Account:     r.Transfer.From,
				AmountCents: r.Transfer.AmountCents,
				Currency:    r.Transfer.Currency,
				Details: map[string]any{
					"transfer_id": r.TransferID,
					"reviewed_by": "system",
					"reason":      "chain stage timeout",
				},
			})
		}
	}
	return marked, ids
}

// emitChainStageTimeoutAuditLocked emits the batched stage-timeout
// audit event. Callers must hold l.mu.
func (l *Ledger) emitChainStageTimeoutAuditLocked(marked int, ids []string) {
	l.emitAudit(AuditEvent{
		Op:            "transfer_review_chain_stage_timeout",
		Actor:         "ExpireReviews",
		TraceID:       fmt.Sprintf("review-chain-timeout@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"timed_out":    marked,
			"transfer_ids": ids,
		},
	})
}
