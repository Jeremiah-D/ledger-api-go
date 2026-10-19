package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Approver vote delegation for dual-control reviews (the fintech
// "vote by proxy" flow: an approver on leave delegates their vote to a
// colleague instead of the review stalling).
//
// LG-42's review is a single-operator decision, LG-124 upgrades it to
// an N-of-M quorum of named approvers, and LG-126 sequences it into a
// tiered approval chain. Delegation adds one standing permission on top
// of all three: a principal grants a delegatee the right to cast votes
// in the principal's place. A delegated vote counts exactly as the
// principal's own vote — the one-vote-per-approver rule applies to the
// principal, not the delegatee — but the audit trail attributes both
// hands: the delegate who cast it and the principal whose vote it was.
//
// Scopes: a delegation is granted for one voting path — "review" (the
// LG-42 flat single-decision path), "quorum" (LG-124 named votes), or
// "chain" (LG-126 per-stage votes) — and optionally for one review
// (ReviewID); an empty ReviewID delegates the principal's vote on
// every review in that scope. While a quorum policy or chain is
// active, a non-member caller holding an active delegation votes
// through it instead of failing with ErrQuorumNotMember; on the flat
// path the delegated decision is attributed to the principal.
//
// Delegation chains are allowed (A delegates to B, B delegates to C)
// but cycles are rejected at grant time: granting B->A while A->B is
// active fails with ErrDelegationCycle, because the trust graph must
// stay acyclic for the "whose vote was it" question to have an answer.
// The cycle check runs over active (non-revoked, non-expired)
// delegations and is scope-agnostic: trust is trust, whatever the
// voting path.
//
// Delegations are structural config (no version bump, like the quorum
// policy): they survive disaster-recovery snapshots, and every grant,
// revocation, and delegated vote lands in the structured audit chain
// (delegation_granted / delegation_revoked / vote_by_delegate).
// /metrics exposes ledger_review_delegations_total, synced from the
// ledger's ever-granted counter on every scrape.

// Delegation scopes: which voting path a delegation applies to.
const (
	// DelegationScopeReview delegates votes on the LG-42 flat
	// single-decision path.
	DelegationScopeReview = "review"
	// DelegationScopeQuorum delegates votes on the LG-124 N-of-M
	// quorum path.
	DelegationScopeQuorum = "quorum"
	// DelegationScopeChain delegates votes on the LG-126 sequential
	// chain path.
	DelegationScopeChain = "chain"
)

// Delegation errors.
var (
	// ErrInvalidDelegation is returned by DelegateVote (and the
	// snapshot import) for a delegation that could never be granted:
	// an unknown scope, a blank delegatee, a self-delegation, or an
	// expiry that is not in the future.
	ErrInvalidDelegation = errors.New("ledger: invalid review vote delegation")
	// ErrDelegationCycle is returned by DelegateVote when the grant
	// would close a cycle in the delegation trust graph (A->B->A):
	// the delegatee already reaches the principal through active
	// delegations.
	ErrDelegationCycle = errors.New("ledger: vote delegation would create a delegation cycle")
)

// ReviewDelegation is one standing vote delegation: principal grants
// delegatee the right to cast votes in the principal's place, in Scope
// (and on ReviewID when set — empty means every review in the scope).
// ExpiresAt is exclusive — the delegation is expired once now >=
// ExpiresAt. Zero means it never expires on its own. A revoked
// delegation is terminal: it never becomes active again (re-granting
// after a revoke starts a fresh delegation).
type ReviewDelegation struct {
	Scope       string    `json:"scope"`
	ReviewID    string    `json:"review_id,omitempty"`
	Principal   string    `json:"principal"`
	Delegatee   string    `json:"delegatee"`
	GrantedAt   time.Time `json:"granted_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Revoked     bool      `json:"revoked,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
}

// delegationKey identifies one delegation: a principal grants one
// delegatee at most one active delegation per (scope, review).
type delegationKey struct {
	scope     string
	reviewID  string
	principal string
	delegatee string
}

// voteDelegation is the resolved delegation behind one delegated vote:
// delegate cast the vote, exercising principal's voting power.
type voteDelegation struct {
	delegate  string
	principal string
	scope     string
	grantedAt time.Time
}

// validDelegationScope reports whether scope names a voting path.
func validDelegationScope(scope string) bool {
	switch scope {
	case DelegationScopeReview, DelegationScopeQuorum, DelegationScopeChain:
		return true
	}
	return false
}

// delegationError wraps a structural validation failure in
// ErrInvalidDelegation so callers can match it with errors.Is.
func delegationError(err error) error {
	return fmt.Errorf("%w: %s", ErrInvalidDelegation, err)
}

// validateDelegation checks a delegation structurally: a known scope
// and non-blank, distinct principal and delegatee. Expiry is not
// checked here — a restored snapshot may carry lapsed delegations,
// and the lazy expiry predicate keeps them inactive.
func validateDelegation(d *ReviewDelegation) error {
	if d == nil {
		return fmt.Errorf("nil delegation")
	}
	if !validDelegationScope(d.Scope) {
		return fmt.Errorf("unknown scope %q", d.Scope)
	}
	if strings.TrimSpace(d.Principal) == "" {
		return fmt.Errorf("principal must not be blank")
	}
	if strings.TrimSpace(d.Delegatee) == "" {
		return fmt.Errorf("delegatee must not be blank")
	}
	if d.Principal == d.Delegatee {
		return fmt.Errorf("principal and delegatee must differ")
	}
	return nil
}

// delegationExpiredLocked reports whether the delegation has lapsed:
// its ExpiresAt is set and now is at or past it. Expiry is lazy by
// predicate — an expired delegation is already unusable for voting
// even though it stays in the registry until revoked. Callers must
// hold l.mu (either lock suffices).
func delegationExpiredLocked(d *ReviewDelegation, now time.Time) bool {
	return !d.ExpiresAt.IsZero() && !now.Before(d.ExpiresAt)
}

// delegationActiveLocked reports whether the delegation can back a
// vote right now: granted, not revoked, not lapsed. Callers must hold
// l.mu (either lock suffices).
func delegationActiveLocked(d *ReviewDelegation, now time.Time) bool {
	return !d.Revoked && !delegationExpiredLocked(d, now)
}

// delegationReachesLocked reports whether from reaches to through
// active delegation edges (any scope — the trust graph is
// scope-agnostic: a cycle in trust is a cycle whatever the voting
// path). Callers must hold l.mu.
func (l *Ledger) delegationReachesLocked(from, to string, now time.Time) bool {
	visited := map[string]bool{from: true}
	stack := []string{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range l.reviewDelegations {
			if d.Principal != n || !delegationActiveLocked(d, now) {
				continue
			}
			if d.Delegatee == to {
				return true
			}
			if !visited[d.Delegatee] {
				visited[d.Delegatee] = true
				stack = append(stack, d.Delegatee)
			}
		}
	}
	return false
}

// resolveDelegationLocked finds the active delegation that lets voter
// cast a vote in scope on reviewID at now: the voter exercises the
// returned principal's voting power, and the vote is recorded as the
// principal's (the one-vote-per-approver rule applies to the
// principal). Only direct delegations resolve — a delegatee votes
// their immediate delegator's vote; longer chains exist for the cycle
// check, not for transitive voting. When several delegators apply,
// the earliest-granted wins, so resolution is deterministic. Callers
// must hold l.mu.
func (l *Ledger) resolveDelegationLocked(scope, reviewID, voter string, now time.Time) (*voteDelegation, bool) {
	var best *ReviewDelegation
	for _, d := range l.reviewDelegations {
		if d.Scope != scope || d.Delegatee != voter || !delegationActiveLocked(d, now) {
			continue
		}
		if d.ReviewID != "" && d.ReviewID != reviewID {
			continue
		}
		if best == nil || d.GrantedAt.Before(best.GrantedAt) ||
			(d.GrantedAt.Equal(best.GrantedAt) && d.Principal < best.Principal) {
			best = d
		}
	}
	if best == nil {
		return nil, false
	}
	return &voteDelegation{
		delegate:  voter,
		principal: best.Principal,
		scope:     scope,
		grantedAt: best.GrantedAt,
	}, true
}

// DelegateVote grants delegatee the right to cast votes in the
// caller's place: scope selects the voting path ("review", "quorum",
// or "chain"), reviewID optionally narrows the grant to one review
// (empty delegates the caller's vote on every review in the scope),
// and expiresAt bounds the grant (zero means it never expires on its
// own — it stays active until revoked).
//
// The caller is the principal: DelegateVoteCtx derives it from the
// context actor (see ContextWithActor, LG-73); the plain variant
// attributes the grant to "operator".
//
// The grant is structural, like SetReviewQuorumPolicy: it does not
// bump the ledger version. A grant that would close a delegation cycle
// (the delegatee already reaches the principal through active
// delegations — A->B->A) fails with ErrDelegationCycle; a grant that
// could never be valid (unknown scope, blank delegatee,
// self-delegation, a non-future expiry) fails with
// ErrInvalidDelegation.
//
// Re-granting an identical active delegation is idempotent: it returns
// the existing delegation, audits nothing, and does not move the
// ledger_review_delegations_total counter. Re-granting with a
// different expiry extends the delegation (audited, not counted
// again); re-granting after a revoke starts a fresh delegation.
func (l *Ledger) DelegateVote(scope, reviewID, delegatee string, expiresAt time.Time) (ReviewDelegation, error) {
	return l.DelegateVoteCtx(context.Background(), scope, reviewID, delegatee, expiresAt)
}

// DelegateVoteCtx is DelegateVote with per-call principal attribution
// (LG-73): the principal is the operator carried by ctx, defaulting to
// "operator" when unset.
func (l *Ledger) DelegateVoteCtx(ctx context.Context, scope, reviewID, delegatee string, expiresAt time.Time) (ReviewDelegation, error) {
	principal := strings.TrimSpace(ActorFromContext(ctx))
	if principal == "" {
		principal = defaultReviewOperator
	}
	delegatee = strings.TrimSpace(delegatee)

	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	if !validDelegationScope(scope) {
		return ReviewDelegation{}, delegationError(fmt.Errorf("unknown scope %q", scope))
	}
	if delegatee == "" {
		return ReviewDelegation{}, delegationError(fmt.Errorf("delegatee must not be blank"))
	}
	if delegatee == principal {
		return ReviewDelegation{}, delegationError(fmt.Errorf("cannot delegate to self"))
	}
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return ReviewDelegation{}, delegationError(fmt.Errorf("expires_at must be in the future"))
	}

	key := delegationKey{scope: scope, reviewID: reviewID, principal: principal, delegatee: delegatee}
	if existing, ok := l.reviewDelegations[key]; ok && !existing.Revoked {
		out := *existing
		if existing.ExpiresAt.Equal(expiresAt) {
			// Idempotent re-grant: the same delegation is already
			// active — return it, audit nothing, count nothing, like
			// every idempotent replay in the review paths.
			return out, nil
		}
		// Extension: the same standing delegation with a new
		// expiry. Audited, but not counted again — it is not a new
		// grant.
		existing.ExpiresAt = expiresAt
		existing.GrantedAt = now
		l.reviewDelegations[key] = existing
		l.emitDelegationGrantedAuditLocked(ctx, existing, true)
		out = *existing
		return out, nil
	}
	// A grant that would close a trust cycle is rejected: the
	// delegatee already reaching the principal through active
	// delegations means adding principal->delegatee loops the graph.
	if l.delegationReachesLocked(delegatee, principal, now) {
		return ReviewDelegation{}, fmt.Errorf("delegatee %q reaches principal %q: %w", delegatee, principal, ErrDelegationCycle)
	}
	d := &ReviewDelegation{
		Scope:     scope,
		ReviewID:  reviewID,
		Principal: principal,
		Delegatee: delegatee,
		GrantedAt: now,
		ExpiresAt: expiresAt,
	}
	l.reviewDelegations[key] = d
	l.reviewDelegationsGranted++
	l.emitDelegationGrantedAuditLocked(ctx, d, false)
	out := *d
	return out, nil
}

// emitDelegationGrantedAuditLocked records a delegation grant in the
// structured audit chain: who granted whom which voting power, for
// which scope and review, until when. Grants move no money, so the
// version is unchanged on both sides. Callers must hold l.mu.
func (l *Ledger) emitDelegationGrantedAuditLocked(ctx context.Context, d *ReviewDelegation, extended bool) {
	details := map[string]any{
		"scope":     d.Scope,
		"principal": d.Principal,
		"delegatee": d.Delegatee,
		"extended":  extended,
	}
	if d.ReviewID != "" {
		details["review_id"] = d.ReviewID
	}
	if !d.ExpiresAt.IsZero() {
		details["expires_at"] = d.ExpiresAt.UTC().Format(time.RFC3339)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "delegation_granted",
		Actor:         "DelegateVote",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
}

// RevokeDelegation ends the caller's active delegation to delegatee in
// scope (and on reviewID when the grant was review-scoped): the
// delegatee can no longer vote in the caller's place. The caller is
// the principal — RevokeDelegationCtx derives it from the context
// actor (LG-73), defaulting to "operator".
//
// Revoking is idempotent: revoking a delegation that is not active
// (never granted, already revoked, or already lapsed) returns
// revoked == false and no error, auditing nothing — like the review
// rejection replay convention. A successful revocation is terminal:
// the delegation never becomes active again (re-granting starts a
// fresh one), and the revocation is audited. Revoking is structural,
// like granting: no version bump.
func (l *Ledger) RevokeDelegation(scope, reviewID, delegatee string) (bool, error) {
	return l.RevokeDelegationCtx(context.Background(), scope, reviewID, delegatee)
}

// RevokeDelegationCtx is RevokeDelegation with per-call principal
// attribution (LG-73).
func (l *Ledger) RevokeDelegationCtx(ctx context.Context, scope, reviewID, delegatee string) (bool, error) {
	principal := strings.TrimSpace(ActorFromContext(ctx))
	if principal == "" {
		principal = defaultReviewOperator
	}
	delegatee = strings.TrimSpace(delegatee)

	l.mu.Lock()
	defer l.mu.Unlock()
	key := delegationKey{scope: scope, reviewID: reviewID, principal: principal, delegatee: delegatee}
	d, ok := l.reviewDelegations[key]
	if !ok || d.Revoked {
		// Idempotent: nothing active to revoke — return unchanged
		// and silent, like the terminal-state replay in
		// RejectTransferReviewCtx.
		return false, nil
	}
	now := time.Now()
	d.Revoked = true
	d.RevokedAt = now
	l.reviewDelegations[key] = d
	details := map[string]any{
		"scope":      d.Scope,
		"principal":  d.Principal,
		"delegatee":  d.Delegatee,
		"revoked_at": now.UTC().Format(time.RFC3339),
	}
	if d.ReviewID != "" {
		details["review_id"] = d.ReviewID
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "delegation_revoked",
		Actor:         "RevokeDelegation",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return true, nil
}

// emitVoteByDelegateLocked records one delegated vote in the
// structured audit chain: the dual attribution of a vote cast through
// a delegation — who actually cast it (delegate) and whose voting
// power it exercised (principal) — plus the scope, the vote direction,
// and when the delegation was granted. The vote itself is also
// recorded by the standard decision event (transfer_review_vote,
// transfer_review_chain_vote, transfer_review_approved, or
// transfer_review_rejected) carrying the same delegate/principal
// details; this event is the delegation-exercise record. Votes move no
// money by themselves, so the version is unchanged on both sides.
// actor is the decision entrypoint ("ApproveTransferReview" or
// "RejectTransferReview"). Callers must hold l.mu.
func (l *Ledger) emitVoteByDelegateLocked(ctx context.Context, actor, vote string, r *TransferReview, deleg *voteDelegation) {
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "vote_by_delegate",
		Actor:         actor,
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"scope":                deleg.scope,
			"vote":                 vote,
			"delegate":             deleg.delegate,
			"principal":            deleg.principal,
			"delegation_granted_at": deleg.grantedAt.UTC().Format(time.RFC3339),
		},
	})
}

// ReviewDelegations lists the active (non-revoked) vote delegations,
// sorted by (scope, review, principal, delegatee). Expiry is lazy by
// predicate — a lapsed delegation is listed until revoked but can no
// longer back a vote. The returned slice is a copy; mutating it does
// not affect the ledger.
func (l *Ledger) ReviewDelegations() []ReviewDelegation {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.reviewDelegationsSnapshotLocked()
}

// reviewDelegationsSnapshotLocked returns the non-revoked delegations
// sorted by (scope, review, principal, delegatee), for the public
// listing and the disaster-recovery snapshot. Callers must hold l.mu
// (either lock suffices; export holds the read lock).
func (l *Ledger) reviewDelegationsSnapshotLocked() []ReviewDelegation {
	out := make([]ReviewDelegation, 0, len(l.reviewDelegations))
	for _, d := range l.reviewDelegations {
		if d.Revoked {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].ReviewID != out[j].ReviewID {
			return out[i].ReviewID < out[j].ReviewID
		}
		if out[i].Principal != out[j].Principal {
			return out[i].Principal < out[j].Principal
		}
		return out[i].Delegatee < out[j].Delegatee
	})
	return out
}

// delegationsEqual compares two delegation lists as written by the
// snapshot: order, fields, and instants. Revoked delegations never
// reach the snapshot, so revocation state is not compared.
func delegationsEqual(a, b []ReviewDelegation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Scope != b[i].Scope || a[i].ReviewID != b[i].ReviewID ||
			a[i].Principal != b[i].Principal || a[i].Delegatee != b[i].Delegatee ||
			a[i].GrantedAt.UnixNano() != b[i].GrantedAt.UnixNano() ||
			a[i].ExpiresAt.UnixNano() != b[i].ExpiresAt.UnixNano() {
			return false
		}
	}
	return true
}

// ReviewDelegationsGranted reports how many vote delegations were ever
// granted: the ledger-side source of the
// ledger_review_delegations_total metric (synced to the HTTP metrics
// on every scrape, like the other ledger-owned counters). The count is
// monotonic — extensions and revocations do not move it — and it
// survives disaster-recovery snapshots.
func (l *Ledger) ReviewDelegationsGranted() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.reviewDelegationsGranted
}
