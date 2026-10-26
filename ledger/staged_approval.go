package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Per-transfer staged approval for dual-control reviews (LG-221, the
// fintech tiered-approval flow for a single high-value transfer).
//
// LG-42's review is a single-operator decision and LG-124 upgrades it
// to a parallel N-of-M quorum; LG-126 sequences that idea as a GLOBAL
// policy (one chain for every review, each stage its own approver set
// and threshold). Staged approval is the per-transfer counterpart:
// SetApprovalStages attaches an ordered role chain to ONE pending
// review, and that review — and only that review — advances through
// it. The two are orthogonal by scope: a global chain decides every
// review alike; stages decide one transfer on its own terms.
//
// Stage flow, enforced at the ledger layer:
//   - the review advances stage by stage: exactly one approver signs
//     each stage, in order; a stage approval advances the review to
//     the next stage (audit event
//     transfer_review_staged_stage_advanced); the last stage's
//     approval settles through the LG-42 settlement path, so a review
//     with unfinished stages can never settle — the flat approve
//     entry point becomes a stage vote while stages are installed;
//   - a reject at any stage closes the review at once through the
//     LG-42 rejection path (the recorded stage vote stays on the
//     review, so the dissent trail survives);
//   - the approver of a stage must differ from the previous stage's
//     approver (ErrStagedApproverRepeated): adjacent rungs may never
//     be signed by the same person, so "tiered" approval cannot
//     collapse into one operator rubber-stamping the chain. A role
//     may still recur on non-adjacent stages, and the same approver
//     may sign non-adjacent stages — only the adjacent repeat is
//     banned, because that is the self-approval hole;
//   - every stage vote and every advance enters the structured audit
//     chain (transfer_review_staged_vote, with stage number and role),
//     so the "who approved which rung" trail is tamper-evident; the
//     per-stage approvers ride the review record, the snapshot, and
//     the reconcile report, like the LG-124/LG-126 votes.
//
// Honest boundary: a stage's Role names the function that must sign
// it (e.g. "treasury", "finance"), but the ledger holds no
// identity-to-role registry — role membership is enforced by the
// calling operator system, which maps identities to roles before it
// calls these entry points. What the ledger guarantees is the part a
// ledger must guarantee: order, one signature per stage, no adjacent
// self-approval, and no settlement before the last stage.
//
// One decision scheme per review: stages install only on a fresh
// pending review — one already carrying chain progress or quorum
// votes belongs to that scheme and refuses installation. While stages
// are installed they take precedence over a global chain or quorum
// policy for this review; clearing them is not offered, because a
// half-walked chain must not be silently replaced mid-flight — the
// review terminates (settles, rejects, or expires) with its stages
// intact on the record.

// Staged vote outcomes: what one stage vote did, reported on the
// decision receipts. "advanced" and "settled" reuse the LG-126/LG-124
// outcome vocabulary; "duplicate" is reserved for the idempotent
// re-approval replay of an already-settled review.
const (
	// StagedOutcomeAdvanced means the vote signed its stage and the
	// review moved to the next stage.
	StagedOutcomeAdvanced = "advanced"
	// StagedOutcomeSettled means the vote signed the last stage and
	// the review settled through the LG-42 settlement path.
	StagedOutcomeSettled = "settled"
	// StagedOutcomeDuplicate means the call re-approved an
	// already-settled review and returned the original receipt.
	StagedOutcomeDuplicate = "duplicate"
)

// Staged approval errors.
var (
	// ErrInvalidApprovalStages is returned by SetApprovalStages for
	// stages that could never be installed: no stages, or a stage
	// with a blank role.
	ErrInvalidApprovalStages = errors.New("ledger: invalid staged approval stages")
	// ErrStagedApprovalAlreadySet is returned by SetApprovalStages
	// when the review already carries stages.
	ErrStagedApprovalAlreadySet = errors.New("ledger: staged approval already set on review")
	// ErrStagedApprovalConflict is returned by SetApprovalStages when
	// the review already belongs to another decision scheme: chain
	// progress or quorum votes are already recorded on it.
	ErrStagedApprovalConflict = errors.New("ledger: review already decided under another approval scheme")
	// ErrStagedApprovalNotConfigured is returned by the explicit
	// staged entry points for a review that carries no stages.
	ErrStagedApprovalNotConfigured = errors.New("ledger: review has no staged approval")
	// ErrStagedApproverRepeated is returned when a stage's approver
	// is the same identity that signed the previous stage.
	ErrStagedApproverRepeated = errors.New("ledger: staged approver must differ from previous stage approver")
)

// ApprovalStage is one rung of a per-transfer staged approval: the
// role that must sign it. Exactly one approver signs a stage — there
// is no per-stage threshold, unlike LG-126's chain stages.
type ApprovalStage struct {
	Role string `json:"role"`
}

// StagedApprovalProgress is a review's position in its staged
// approval: the ordered stages, the current stage index, and the
// approver of each decided stage, in stage order. It rides the review
// record through snapshots and the reconcile report. Invariant:
// len(Approvers) == Stage while the review is pending and staged.
//
// LG-223: Timeouts carries the opt-in per-stage lifetimes installed
// by SetStageTimeouts (nil when no timeouts are installed — a stage
// with no timeout never times out, and a zero timeout means the same
// for that stage). StageStartedAt is when the current stage became
// current: the timeout-install instant for the stage the review sits
// on when timeouts land, and the advance instant for every later
// stage — so each stage's clock runs from its own start, never from
// the review's creation or from a previous stage's start.
type StagedApprovalProgress struct {
	Stages    []ApprovalStage `json:"stages"`
	Stage     int             `json:"stage"`
	Approvers []string        `json:"approvers"`
	// Timeouts is the per-stage lifetime installed by
	// SetStageTimeouts, in stage order; nil when unset. It is only
	// meaningful while stages are installed — it never applies to a
	// flat, quorum, or chain decision.
	Timeouts []time.Duration `json:"timeouts,omitempty"`
	// StageStartedAt is when the current stage started, for the
	// LG-223 timeout clock. Zero when no timeouts are installed.
	StageStartedAt time.Time `json:"stage_started_at,omitempty"`
}

// StagedApprovalTally is the live staged-approval count disclosed on
// the decision receipts, like ReviewQuorumTally for the flat LG-124
// quorum and ReviewChainTally for the LG-126 chain.
type StagedApprovalTally struct {
	Stage        int      `json:"stage"`
	StageCount   int      `json:"stage_count"`
	Role         string   `json:"role"`
	Approvers    []string `json:"approvers"`
	LastApprover string   `json:"last_approver"`
	Outcome      string   `json:"outcome"`
}

// validateApprovalStages checks a stage list for installation: at
// least one stage, every role non-blank after trimming. Roles may
// repeat — only approvers are barred from adjacent repeats.
func validateApprovalStages(stages []ApprovalStage) error {
	if len(stages) == 0 {
		return ErrInvalidApprovalStages
	}
	for _, s := range stages {
		if strings.TrimSpace(s.Role) == "" {
			return ErrInvalidApprovalStages
		}
	}
	return nil
}

// copyStagedApproval deep-copies a staged-approval progress record,
// preserving nil.
func copyStagedApproval(p *StagedApprovalProgress) *StagedApprovalProgress {
	if p == nil {
		return nil
	}
	out := &StagedApprovalProgress{
		Stage:          p.Stage,
		Stages:         append([]ApprovalStage(nil), p.Stages...),
		Approvers:      append([]string(nil), p.Approvers...),
		Timeouts:       append([]time.Duration(nil), p.Timeouts...),
		StageStartedAt: p.StageStartedAt,
	}
	return out
}

// stagedApprovalEqual compares two staged-approval progress records:
// stages in order, current stage, and the decided approvers in order.
func stagedApprovalEqual(a, b *StagedApprovalProgress) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Stage != b.Stage || len(a.Stages) != len(b.Stages) || len(a.Approvers) != len(b.Approvers) ||
		len(a.Timeouts) != len(b.Timeouts) || a.StageStartedAt.UnixNano() != b.StageStartedAt.UnixNano() {
		return false
	}
	for i := range a.Stages {
		if a.Stages[i].Role != b.Stages[i].Role {
			return false
		}
	}
	for i := range a.Approvers {
		if a.Approvers[i] != b.Approvers[i] {
			return false
		}
	}
	for i := range a.Timeouts {
		if a.Timeouts[i] != b.Timeouts[i] {
			return false
		}
	}
	return true
}

// SetApprovalStages attaches an ordered role chain to one pending
// review. The review must be fresh: installing on a review that
// already carries stages, chain progress, or quorum votes fails, and
// a lapsed review is auto-rejected first (like the approve path) and
// refused with ErrReviewExpired. Installing is structural — it does
// not bump the ledger version, like the chain and quorum policies.
func (l *Ledger) SetApprovalStages(transferID string, stages []ApprovalStage) error {
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
		l.emitReviewExpireAuditCtxLocked(context.Background(), 1)
		return ErrReviewExpired
	}
	if r.StagedApproval != nil {
		return ErrStagedApprovalAlreadySet
	}
	if r.ChainProgress != nil || len(r.ApproveVotes) > 0 || len(r.RejectVotes) > 0 {
		return ErrStagedApprovalConflict
	}
	if err := validateApprovalStages(stages); err != nil {
		return err
	}
	trimmed := make([]ApprovalStage, len(stages))
	for i, s := range stages {
		trimmed[i] = ApprovalStage{Role: strings.TrimSpace(s.Role)}
	}
	r.StagedApproval = &StagedApprovalProgress{
		Stages:    trimmed,
		Stage:     0,
		Approvers: []string{},
	}
	l.reviews[transferID] = r
	return nil
}

// StagedApproval reports the staged-approval progress installed on a
// review, or false when the review carries none (or does not exist).
// The returned record is a copy; mutating it does not affect the
// ledger.
func (l *Ledger) StagedApproval(transferID string) (StagedApprovalProgress, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	r, ok := l.reviews[transferID]
	if !ok || r.StagedApproval == nil {
		return StagedApprovalProgress{}, false
	}
	return *copyStagedApproval(r.StagedApproval), true
}

// stagedTallyLocked builds the live tally disclosed on a staged
// decision receipt. Callers must hold the write lock.
func stagedTallyLocked(r *TransferReview, p *StagedApprovalProgress, lastApprover, outcome string) *StagedApprovalTally {
	return &StagedApprovalTally{
		Stage:        p.Stage,
		StageCount:   len(p.Stages),
		Role:         p.Stages[p.Stage].Role,
		Approvers:    append([]string(nil), p.Approvers...),
		LastApprover: lastApprover,
		Outcome:      outcome,
	}
}

// pendingStagedReceiptLocked builds the pending receipt for a stage
// vote that advanced the review without settling it. Callers must
// hold the write lock.
func pendingStagedReceiptLocked(r *TransferReview, p *StagedApprovalProgress, approver, outcome string) TransferReceipt {
	return TransferReceipt{
		TransferID:   r.TransferID,
		ReviewStatus: ReviewStatusPending,
		StagedTally:  stagedTallyLocked(r, p, approver, outcome),
	}
}

// emitStagedVoteAuditLocked records one staged stage vote in the
// structured audit chain. Callers must hold the write lock.
func (l *Ledger) emitStagedVoteAuditLocked(ctx context.Context, actor string, r *TransferReview, p *StagedApprovalProgress, voter, vote string) {
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_staged_vote",
		Actor:         actor,
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"stage":       p.Stage,
			"stage_count": len(p.Stages),
			"role":        p.Stages[p.Stage].Role,
			"voter":       voter,
			"vote":        vote,
		},
	})
}

// castStagedApproveVoteLocked signs the review's current stage and
// advances it — settling through the LG-42 settlement path on the
// last stage. The review lookup, the terminal-state handling, and
// the review-level expiry check already happened in the caller.
// Callers must hold the write lock.
func (l *Ledger) castStagedApproveVoteLocked(ctx context.Context, r *TransferReview, approver string, now time.Time) (TransferReceipt, error) {
	p := r.StagedApproval
	if p == nil {
		return TransferReceipt{}, ErrStagedApprovalNotConfigured
	}
	if p.Stage < 0 || p.Stage >= len(p.Stages) {
		return TransferReceipt{}, fmt.Errorf("stage %d: %w", p.Stage, ErrInvalidApprovalStages)
	}
	// LG-223: a stage that lapsed while nobody was watching rejects
	// the review first — the operator can never approve past a silent
	// approver, and the review can never advance to the next stage
	// on a timed-out rung.
	if l.stagedStageTimedOut(r, now) {
		stage := p.Stages[p.Stage]
		l.timeoutStagedStageLocked(r, now)
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "transfer_review_staged_stage_timeout",
			Actor:         "ApproveTransferReview",
			TraceID:       r.TransferID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
			Details: map[string]any{
				"stage":       p.Stage,
				"stage_count": len(p.Stages),
				"role":        stage.Role,
				"timeout":     p.Timeouts[p.Stage].String(),
				"reason":      "staged stage timeout",
			},
		})
		return TransferReceipt{}, ErrReviewExpired
	}
	if len(p.Approvers) > 0 && p.Approvers[len(p.Approvers)-1] == approver {
		return TransferReceipt{}, ErrStagedApproverRepeated
	}
	stage := p.Stages[p.Stage]
	p.Approvers = append(p.Approvers, approver)
	l.emitStagedVoteAuditLocked(ctx, "ApproveTransferReview", r, p, approver, "approve")
	if p.Stage == len(p.Stages)-1 {
		// Last stage: settle exactly like a single-operator approval,
		// with ReviewedBy naming every stage approver in stage order
		// — the "who approved" trail for the whole chain.
		return l.settleReviewLocked(ctx, r, strings.Join(p.Approvers, ","), now, nil, nil)
	}
	from := p.Stage
	p.Stage++
	// LG-223: the next stage's clock starts at the advance instant,
	// not at the review's creation — each stage is timed from its
	// own start.
	p.StageStartedAt = now
	l.reviews[r.TransferID] = r
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_staged_stage_advanced",
		Actor:         "ApproveTransferReview",
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"from_stage":  from,
			"to_stage":    p.Stage,
			"stage_count": len(p.Stages),
			"voter":       approver,
			"role":        stage.Role,
		},
	})
	return pendingStagedReceiptLocked(r, p, approver, StagedOutcomeAdvanced), nil
}

// castStagedRejectVoteLocked records a staged reject vote and closes
// the review at once through the LG-42 rejection path — a reject is
// decisive at every stage, like the LG-126 chain. The recorded vote
// audit stays on the trail even though the review is rejected. The
// review lookup and terminal-state handling already happened in the
// caller. Callers must hold the write lock.
func (l *Ledger) castStagedRejectVoteLocked(ctx context.Context, r *TransferReview, reviewer, reason string) (review *TransferReview, changed bool, err error) {
	p := r.StagedApproval
	if p == nil {
		return nil, false, ErrStagedApprovalNotConfigured
	}
	l.emitStagedVoteAuditLocked(ctx, "RejectTransferReview", r, p, reviewer, "reject")
	return l.rejectReviewLocked(ctx, r, reviewer, reason, nil)
}

// ApproveStagedReview signs the current stage of a staged review:
// the explicit entry point for the staged flow (ApproveTransferReview
// routes here while stages are installed). An empty approver is
// recorded as "operator", like the flat path.
func (l *Ledger) ApproveStagedReview(transferID, approver string) (TransferReceipt, error) {
	return l.ApproveStagedReviewCtx(context.Background(), transferID, approver)
}

// ApproveStagedReviewCtx is ApproveStagedReview with per-call audit
// actor attribution (LG-73).
func (l *Ledger) ApproveStagedReviewCtx(ctx context.Context, transferID, approver string) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	r, ok := l.reviews[transferID]
	if !ok {
		return TransferReceipt{}, ErrReviewNotFound
	}
	if r.Status == ReviewStatusApproved {
		dup := *r.ApprovedReceipt
		dup.Duplicate = true
		return dup, nil
	}
	if r.Status != ReviewStatusPending {
		return TransferReceipt{}, ErrReviewNotPending
	}
	now := time.Now()
	if reviewExpiredLocked(r, now) {
		l.expireReviewLocked(r, now)
		l.emitReviewExpireAuditCtxLocked(ctx, 1)
		return TransferReceipt{}, ErrReviewExpired
	}
	if r.StagedApproval == nil {
		return TransferReceipt{}, ErrStagedApprovalNotConfigured
	}
	if approver == "" {
		approver = defaultReviewOperator
	}
	return l.castStagedApproveVoteLocked(ctx, r, approver, now)
}

// RejectStagedReview rejects a staged review at its current stage:
// the explicit entry point for the staged flow (RejectTransferReview
// routes here while stages are installed). An empty reviewer is
// recorded as "operator", like the flat path.
func (l *Ledger) RejectStagedReview(transferID, reviewer, reason string) (review *TransferReview, changed bool, err error) {
	return l.RejectStagedReviewCtx(context.Background(), transferID, reviewer, reason)
}

// RejectStagedReviewCtx is RejectStagedReview with per-call audit
// actor attribution (LG-73).
func (l *Ledger) RejectStagedReviewCtx(ctx context.Context, transferID, reviewer, reason string) (review *TransferReview, changed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	r, ok := l.reviews[transferID]
	if !ok {
		return nil, false, ErrReviewNotFound
	}
	if r.Status == ReviewStatusRejected || r.Status == ReviewStatusExpired {
		out := *r
		return &out, false, nil
	}
	if r.Status != ReviewStatusPending {
		return nil, false, ErrReviewNotPending
	}
	if r.StagedApproval == nil {
		return nil, false, ErrStagedApprovalNotConfigured
	}
	if reviewer == "" {
		reviewer = defaultReviewOperator
	}
	return l.castStagedRejectVoteLocked(ctx, r, reviewer, reason)
}
