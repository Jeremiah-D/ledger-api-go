package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dual-control review for large transfers (the fintech maker-checker
// flow).
//
// A transfer whose amount reaches the configured review threshold does
// not settle immediately: PostTransfer books its legs as journal rows
// marked pending review (see JournalEntry.PendingReview) and freezes the
// payer's funds atomically instead. The frozen funds leave the payer's
// *available* balance (see Available — pending reviews deduct like
// authorization holds) but touch neither net balances, debit/credit
// totals, the ledger version, nor the audit chain: as far as the books
// are concerned, the money has not moved yet.
//
// An operator then approves (the transfer settles: the marker flips to
// effective, balances apply, the version bumps, and chain links land) or
// rejects (the freeze is released, the rows stay as historical
// pending-marked journal rows). The reviewer's identity is recorded on
// the review and in the structured audit log's hash chain (LG-32), so the
// "who approved what" trail is tamper-evident. Approvals are idempotent:
// re-approving returns the original approval receipt. Reviews can carry
// an opt-in expiry: ExpireReviews (or the background ReviewSweeper)
// auto-rejects lapsed reviews so frozen funds can never strand forever.
//
// Review lifecycle: pending_review -> approved | rejected | expired.
// Expiry is lazy by predicate for reads (an expired review already
// counts as inactive for available-balance purposes), like holds.
//
// Thresholds are opt-in, global and/or per account (the payer, From):
// WithReviewThreshold sets the ledger-wide amount in cents, and
// SetReviewThreshold sets a per-account amount that wins over the
// global one for that account. A transfer enters review when its
// principal amount is at or above the effective threshold. Zero means
// disabled (global) or "review everything from this account"
// (per-account, once explicitly set); negative is rejected.
//
// Risk-control interplay (mirrors hold.go):
//   - The review gate runs last among the PostTransfer risk controls,
//     after the period gate: a transfer that fails validation, the
//     frozen/overdraft/daily-limit checks, or the closed-period gate
//     never enters review.
//   - The daily outflow budget is reserved at review creation: the
//     payer's total outflow (amount + fee) was limit-checked when the
//     review was created, and the budget is consumed then — the freeze
//     is atomic, covering funds and limit alike. The monthly spend
//     budget (LG-81) is reserved the same way: the transfer was
//     budget-checked to reach this gate, so its outflow counts in the
//     month's used figure from creation, and approval settles against
//     the already-reserved budget.
//   - For overdraft-protected payers the freeze additionally requires
//     available coverage (balance minus active holds and pending
//     reviews); otherwise ErrInsufficientAvailableFunds. Unprotected
//     accounts keep the ledger's sign convention — the reservation is
//     advisory for them, exactly like holds.
//   - Approve re-runs the frozen, overdraft, and period checks against
//     current state: a payer frozen after review creation fails the
//     approval with ErrAccountFrozen, and a period closed after review
//     creation fails it with ErrPeriodClosed (the review stays pending,
//     so the operator can retry after reopening).

// Transfer review statuses. A review is actionable only while pending;
// every other status is terminal.
const (
	ReviewStatusPending  = "pending_review"
	ReviewStatusApproved = "approved"
	ReviewStatusRejected = "rejected"
	ReviewStatusExpired  = "expired"
)

// Validation and state errors returned by the review APIs.
var (
	// ErrReviewNotFound is returned by the review decision endpoints
	// for an unknown transfer ID.
	ErrReviewNotFound = errors.New("ledger: transfer review not found")
	// ErrReviewNotPending is returned when a review decision targets a
	// review that is no longer pending: approving or rejecting an
	// approved, rejected, or expired review. Re-approving an approved
	// review is the one exception — it returns the original approval
	// receipt idempotently instead of failing.
	ErrReviewNotPending = errors.New("ledger: transfer review is not pending")
	// ErrReviewExpired is returned by ApproveTransferReview when the
	// review's expiry had passed: the review is auto-rejected first
	// (see ExpireReviews) and the approval is refused, so an operator
	// can never approve a lapsed freeze.
	ErrReviewExpired = errors.New("ledger: transfer review has expired")
	// ErrInvalidReviewThreshold is returned by SetReviewThreshold for a
	// negative threshold. Zero is legal (review every transfer from the
	// account); clearing uses ClearReviewThreshold.
	ErrInvalidReviewThreshold = errors.New("ledger: review threshold must not be negative")
)

// defaultReviewOperator is recorded as the reviewer when the operator
// endpoint is called without a reviewer identity.
const defaultReviewOperator = "operator"

// TransferReview is one large-transfer dual-control review: the transfer
// as requested, its frozen legs, and the operator decision (if any).
//
// Entries holds the transfer's legs as committed: marked pending review
// while the review is pending (or was rejected/expired), flipped to
// effective on approval. For an approved review the entries are ordinary
// journal rows — approval settles them exactly like a direct PostTransfer
// — while the review record itself keeps the provenance (who approved,
// when, under which threshold).
type TransferReview struct {
	TransferID string `json:"transfer_id"`
	// Transfer is the transfer request as normalized by PostTransfer
	// (canonical currency codes, filled timestamps).
	Transfer Transfer `json:"transfer"`
	// Entries are the review's journal legs, in commit order (principal,
	// then the fee leg when one was booked).
	Entries  []JournalEntry `json:"entries"`
	FeeCents int64          `json:"fee_cents"`
	// FeeTierIndex and FeeRateBps disclose the fee schedule tier that
	// applied, exactly as on TransferReceipt (-1 and 0 when no policy
	// tier applied).
	FeeTierIndex int           `json:"fee_tier_index"`
	FeeRateBps   int64         `json:"fee_rate_bps"`
	FX           *FXConversion `json:"fx,omitempty"`
	// TotalOutflowCents is the frozen sum: the transfer amount plus the
	// fee leg, in the transfer's (source) currency. It is what the
	// review deducts from the payer's available balance and what the
	// daily outflow budget reserved at creation.
	TotalOutflowCents int64 `json:"total_outflow_cents"`
	// ThresholdCents is the effective review threshold that routed this
	// transfer into review, for auditability.
	ThresholdCents int64     `json:"threshold_cents"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	// ExpiresAt is exclusive — the review is expired once now >=
	// ExpiresAt. Zero means the review never expires on its own.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// ReviewedBy is the operator identity from the approve/reject call
	// (or "system" for auto-expiry); empty while pending. Under an
	// N-of-M quorum policy (see review_quorum.go) it carries the
	// comma-joined approver identities whose votes settled the review.
	ReviewedBy string    `json:"reviewed_by,omitempty"`
	ReviewedAt time.Time `json:"reviewed_at,omitempty"`
	// ApproveVotes and RejectVotes record the named quorum votes cast
	// on the review (see review_quorum.go): approver identities in vote
	// order. Empty unless a quorum policy was active when the votes
	// were cast. The votes ride the snapshot record and the reconcile
	// report, so the "who voted what" trail survives disaster recovery.
	ApproveVotes []string `json:"approve_votes,omitempty"`
	RejectVotes  []string `json:"reject_votes,omitempty"`
	// ApprovedReceipt is the settlement receipt stored at approval, so
	// re-approving returns the identical receipt idempotently. Nil
	// unless Status is approved.
	ApprovedReceipt *TransferReceipt `json:"approved_receipt,omitempty"`
}

// WithReviewThreshold sets the ledger-wide large-transfer review
// threshold in cents: a transfer whose principal amount is at or above
// the threshold enters pending review instead of settling. Zero (the
// default) disables the gate — no transfer is ever reviewed. A negative
// threshold panics: fail-fast at construction, like the fee schedule, so
// a misconfigured risk control can never silently misroute.
func WithReviewThreshold(thresholdCents int64) Option {
	if thresholdCents < 0 {
		panic("ledger: review threshold must not be negative")
	}
	return func(l *Ledger) {
		l.reviewThreshold = thresholdCents
	}
}

// WithReviewExpiry sets how long a pending review may wait for an
// operator decision before it lapses: ExpireReviews (and the background
// ReviewSweeper) auto-reject reviews older than this. A non-positive
// duration disables auto-expiry — reviews wait indefinitely. The expiry
// is opt-in, like the threshold itself.
func WithReviewExpiry(d time.Duration) Option {
	return func(l *Ledger) {
		if d > 0 {
			l.reviewExpiry = d
		}
	}
}

// ReviewThreshold is one per-account review threshold config row, used
// by the reconciliation report and the disaster-recovery snapshot.
type ReviewThreshold struct {
	Account        AccountID `json:"account"`
	ThresholdCents int64     `json:"threshold_cents"`
}

// WithReviewThresholds configures per-account review thresholds at
// construction time (see SetReviewThreshold). A per-account threshold
// wins over the global WithReviewThreshold for that account. Negative
// thresholds panic — fail-fast at construction, like the fee schedule.
func WithReviewThresholds(thresholds []ReviewThreshold) Option {
	for _, rt := range thresholds {
		if rt.Account == "" {
			panic("ledger: review threshold account must not be empty")
		}
		if rt.ThresholdCents < 0 {
			panic("ledger: review threshold must not be negative")
		}
	}
	return func(l *Ledger) {
		for _, rt := range thresholds {
			l.reviewThresholds[rt.Account] = rt.ThresholdCents
		}
	}
}

// SetReviewThreshold configures (or replaces) the review threshold for
// one account: transfers from this account (the payer) whose principal
// amount is at or above thresholdCents enter pending review. A threshold
// of 0 routes every transfer from the account into review; negative
// thresholds are rejected with ErrInvalidReviewThreshold; clearing uses
// ClearReviewThreshold. Setting a threshold is structural, like Freeze:
// it does not bump the ledger version.
func (l *Ledger) SetReviewThreshold(a AccountID, thresholdCents int64) error {
	if thresholdCents < 0 {
		return ErrInvalidReviewThreshold
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reviewThresholds[a] = thresholdCents
	return nil
}

// ClearReviewThreshold removes the per-account review threshold: the
// account falls back to the global threshold (or no review when none is
// configured). Clearing a threshold that was never set is a no-op.
func (l *Ledger) ClearReviewThreshold(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reviewThresholds, a)
}

// ReviewThreshold reports the effective review threshold for the account
// in cents: the per-account threshold when one is configured, otherwise
// the global threshold. The second return value is false when no
// threshold applies to the account at all — review is then disabled for
// it.
func (l *Ledger) ReviewThreshold(a AccountID) (int64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.effectiveReviewThresholdLocked(a)
}

// effectiveReviewThresholdLocked resolves the threshold for the payer:
// per-account wins over global. Callers must hold l.mu.
func (l *Ledger) effectiveReviewThresholdLocked(a AccountID) (int64, bool) {
	if t, ok := l.reviewThresholds[a]; ok {
		return t, true
	}
	if l.reviewThreshold > 0 {
		return l.reviewThreshold, true
	}
	return 0, false
}

// reviewThresholdsLocked returns every configured per-account review
// threshold, sorted by account, for the reconciliation report and the
// snapshot. Callers must hold l.mu.
func (l *Ledger) reviewThresholdsLocked() []ReviewThreshold {
	out := make([]ReviewThreshold, 0, len(l.reviewThresholds))
	for a, t := range l.reviewThresholds {
		out = append(out, ReviewThreshold{Account: a, ThresholdCents: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// ParseReviewThresholds parses the LEDGER_REVIEW_THRESHOLDS environment
// variable into per-account review threshold configs:
//
//	"<account>:<thresholdCents>[,<account>:<thresholdCents>...]"
//
// e.g. "treasury:1000000,ops:500000" routes treasury transfers at or
// above $10,000.00 and ops transfers at or above $5,000.00 into review.
// Thresholds are integer cents and must not be negative. Parsing is
// strict — a malformed segment is an error, so a misconfigured
// deployment fails fast at startup instead of silently running without
// its risk control (see ParseDailyLimits for the convention).
func ParseReviewThresholds(raw string) ([]ReviewThreshold, error) {
	fail := func(format string, args ...any) ([]ReviewThreshold, error) {
		return nil, fmt.Errorf("ledger: invalid review thresholds %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []ReviewThreshold
	seen := make(map[AccountID]bool)
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 2 {
			return fail("segment %q must be \"<account>:<thresholdCents>\"", seg)
		}
		account := AccountID(strings.TrimSpace(parts[0]))
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		if seen[account] {
			return fail("segment %q duplicates account %q", seg, account)
		}
		seen[account] = true
		threshold, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || threshold < 0 {
			return fail("segment %q: threshold %q must be a non-negative integer (cents)", seg, parts[1])
		}
		out = append(out, ReviewThreshold{Account: account, ThresholdCents: threshold})
	}
	return out, nil
}

// reviewExpiredLocked reports whether the review has lapsed: its ExpiresAt
// is set and now is at or past it. Expiry is lazy by predicate — an
// expired review already counts as inactive for available-balance
// purposes, even before ExpireReviews sweeps it. Callers must hold l.mu.
func reviewExpiredLocked(r *TransferReview, now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// reviewActiveLocked reports whether the review currently freezes funds:
// pending and not lapsed. Callers must hold l.mu.
func reviewActiveLocked(r *TransferReview, now time.Time) bool {
	return r.Status == ReviewStatusPending && !reviewExpiredLocked(r, now)
}

// reviewHeldLocked sums the frozen funds of active reviews on
// (account, currency) at now: the payer-side total outflow each pending
// review reserved. It is the review analogue of heldLocked — Available
// deducts both. Callers must hold l.mu.
func (l *Ledger) reviewHeldLocked(a AccountID, currency string, now time.Time) int64 {
	var total int64
	for _, r := range l.reviews {
		if r.Transfer.From == a && r.Transfer.Currency == currency && reviewActiveLocked(r, now) {
			total += r.TotalOutflowCents
		}
	}
	return total
}

// commitReviewEntryLocked records one review-frozen journal row: the row
// itself, its idempotency index entry, and the per-account index. Unlike
// commitEntryLocked it touches neither balances, debit/credit totals,
// the ledger version, nor the audit chain — a pending review moves no
// money, so there is nothing for the books or the chain to record yet.
// The PendingReview marker (covered by the chain hash once the review is
// approved and linked) keeps the frozen rows distinguishable in journal
// exports. Callers must hold the write lock.
func (l *Ledger) commitReviewEntryLocked(e JournalEntry) {
	e.PendingReview = true
	// Like commitEntryLocked, the frozen row must carry a private copy
	// of the attachments slice (see isolateAttachments).
	e = isolateAttachments(e)
	l.entries[e.ID] = e
	if e.IdempotencyKey != "" {
		l.byKey[e.IdempotencyKey] = e
	}
	// The external-reference index follows the same rule as the
	// idempotency index: the reference is claimed when the transfer
	// freezes (PostTransfer checks it before the review gate), so a
	// second transfer with the same reference cannot sneak in while
	// the review is pending. Approval settles the already-indexed
	// rows without re-registering.
	if e.ExternalRef != "" {
		l.byRef[e.ExternalRef] = e.ID
	}
	l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
	l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
}

// maybeReviewTransferLocked is the dual-control gate at the end of the
// PostTransfer risk-check chain (see transfer.go and fx.go): after
// validation, the idempotency replay check, the ID-conflict check, the
// frozen/overdraft/daily-limit checks, and the period gate all passed.
//
// When the gate is not armed (no threshold for the payer) or the
// transfer's principal amount is below the effective threshold, it
// returns reviewed == false and the caller commits normally. Otherwise
// it freezes the transfer atomically — journal rows marked pending
// review, the payer's total outflow deducted from available, the daily
// outflow budget consumed — and returns the review receipt with
// reviewed == true.
//
// A rejection here records nothing, like every other risk control. The
// returned receipt carries ReviewStatus "pending_review".
//
// Callers must hold the write lock and must have run every check above;
// entries must be fully built (IDs, legs, amounts, timestamps final).
func (l *Ledger) maybeReviewTransferLocked(t Transfer, entries []JournalEntry, feeCents int64, tierIndex int, rateBps int64, fx *FXConversion, totalOutflow int64, now time.Time) (receipt TransferReceipt, reviewed bool, err error) {
	threshold, ok := l.effectiveReviewThresholdLocked(t.From)
	if !ok || t.AmountCents < threshold {
		return TransferReceipt{}, false, nil
	}

	// For overdraft-protected payers the freeze requires available
	// coverage: balance minus active holds and active review freezes
	// must cover the total outflow. The comparison never subtracts, so
	// it cannot overflow (see overdraft.go); totalOutflow is known to
	// fit. Unprotected accounts keep the sign convention — the
	// reservation is advisory for them, exactly like holds.
	if l.noOverdraft[t.From] {
		key := accountCurrency{account: t.From, currency: t.Currency}
		reserved := l.heldLocked(t.From, t.Currency, now) + l.reviewHeldLocked(t.From, t.Currency, now)
		if !coversCents(l.balances[key], reserved, totalOutflow) {
			return TransferReceipt{}, false, ErrInsufficientAvailableFunds
		}
	}

	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	for _, e := range entries {
		l.commitReviewEntryLocked(e)
	}
	// The daily outflow budget is reserved at creation: the transfer
	// passed the limit check to reach this gate, and the freeze is
	// atomic — funds and limit budget lock together. Approval later
	// settles against the already-reserved budget.
	l.addDailyOutflowLocked(t.From, t.Currency, totalOutflow, entries[0].CreatedAt)
	// The monthly spend budget is reserved at creation too, like the
	// daily outflow budget above: the transfer passed the budget check
	// to reach this gate, and the freeze is atomic — funds, daily
	// limit, and monthly budget lock together. The reserved spend counts
	// in the month's used figure from creation; approval later settles
	// against the already-reserved budget without consuming it twice.
	// Rejected or expired reviews keep their reservation, exactly like
	// the daily outflow budget.
	l.addMonthlySpendLocked(t.From, t.Currency, totalOutflow, entries[0].CreatedAt)
	if t.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.transferKeys[t.IdempotencyKey] = ids
	}

	review := &TransferReview{
		TransferID:        t.ID,
		Transfer:          t,
		Entries:           append([]JournalEntry(nil), entries...),
		FeeCents:          feeCents,
		FeeTierIndex:      tierIndex,
		FeeRateBps:        rateBps,
		FX:                fx,
		TotalOutflowCents: totalOutflow,
		ThresholdCents:    threshold,
		Status:            ReviewStatusPending,
		CreatedAt:         now,
	}
	for i := range review.Entries {
		review.Entries[i].PendingReview = true
	}
	if l.reviewExpiry > 0 {
		review.ExpiresAt = now.Add(l.reviewExpiry)
	}
	l.reviews[t.ID] = review

	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	reviewDetails := map[string]any{
		"amount_cents":    t.AmountCents,
		"currency":        t.Currency,
		"fee_cents":       feeCents,
		"threshold_cents": threshold,
		"review_status":   ReviewStatusPending,
	}
	if !review.ExpiresAt.IsZero() {
		reviewDetails["expires_at"] = review.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if t.Memo != "" {
		reviewDetails["memo"] = t.Memo
	}
	l.emitAudit(AuditEvent{
		Op:            "transfer_review_created",
		Actor:         "PostTransfer",
		TraceID:       t.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{t.From, t.To},
		Details:       reviewDetails,
	})

	return TransferReceipt{
		TransferID:   t.ID,
		Entries:      review.Entries,
		FeeCents:     feeCents,
		FeeTierIndex: tierIndex,
		FeeRateBps:   rateBps,
		Duplicate:    false,
		FX:           fx,
		ReviewStatus: ReviewStatusPending,
		// The fee-split breakdown rebuilds from the journaled legs'
		// memos, like an idempotent replay (see fee_split.go).
		FeeSplits: feeSplitSharesFromLegs(t.ID, entries),
	}, true, nil
}

// reviewStatusLocked reports the review status of the transfer whose
// principal entry is entryID, or "" when the transfer was never
// reviewed. Callers must hold l.mu.
func (l *Ledger) reviewStatusLocked(entryID string) string {
	if r, ok := l.reviews[entryID]; ok {
		return r.Status
	}
	return ""
}

// GetTransferReview returns the review record for the transfer ID, or
// false when the transfer was never routed into review. The returned
// record is a copy; mutating it does not affect the ledger.
func (l *Ledger) GetTransferReview(transferID string) (TransferReview, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.reviews[transferID]
	if !ok {
		return TransferReview{}, false
	}
	return *r, true
}

// PendingTransferReviews lists every review currently awaiting an
// operator decision, sorted by (CreatedAt, TransferID). Reviews whose
// expiry has passed but which ExpireReviews has not swept yet are
// excluded — expiry is lazy, so they are already inactive.
func (l *Ledger) PendingTransferReviews() []TransferReview {
	return l.PendingTransferReviewsAt(time.Now())
}

// pendingTransferReviewsLocked lists the actionable reviews, sorted by
// (CreatedAt, TransferID). Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing. The reconciliation scan
// calls this directly so the pending list is built under the same read
// lock as the rest of the report.
func (l *Ledger) pendingTransferReviewsLocked(now time.Time) []TransferReview {
	var out []TransferReview
	for _, r := range l.reviews {
		if reviewActiveLocked(r, now) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].TransferID < out[j].TransferID
	})
	return out
}

// PendingTransferReviewsAt is PendingTransferReviews with a
// caller-supplied timestamp, so tests can pin expiry deterministically.
func (l *Ledger) PendingTransferReviewsAt(now time.Time) []TransferReview {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pendingTransferReviewsLocked(now)
}

// ApproveTransferReview settles a pending review: the frozen legs flip to
// effective and commit exactly like a direct PostTransfer — balances and
// totals apply, the ledger version bumps once per leg, one audit-chain
// link lands per leg (the chain hash covers the flipped marker, so the
// effective form is tamper-evident), and the structured audit log
// records the reviewer's identity in its hash chain.
//
// The risk checks re-run against current state before anything is
// recorded: a leg through a now-frozen account fails with
// ErrAccountFrozen, an overdraft-protected payer that can no longer
// cover the outflow fails with ErrAccountOverdraft, and legs dated in a
// now-closed period fail with ErrPeriodClosed — in all three cases the
// review stays pending, so the operator can retry once the underlying
// condition clears. A review whose expiry passed is auto-rejected first
// and the approval refused with ErrReviewExpired.
//
// Approval is idempotent: re-approving an approved review returns the
// original approval receipt with Duplicate == true and settles nothing
// new. Approving a rejected or expired review fails with
// ErrReviewNotPending. An empty reviewer is recorded as "operator".
//
// While an N-of-M quorum policy is active (see SetReviewQuorumPolicy,
// review_quorum.go), the call is a named approve vote instead of an
// immediate settlement: the reviewer must be a policy member, the vote
// is recorded in the review and in the structured audit chain, and the
// call returns a pending receipt carrying the live vote tally until N
// distinct approvers have voted — the Nth vote settles through the path
// above. With no policy configured the behavior is exactly the LG-42
// single-decision path.
func (l *Ledger) ApproveTransferReview(transferID, reviewer string) (TransferReceipt, error) {
	return l.ApproveTransferReviewCtx(context.Background(), transferID, reviewer)
}

// ApproveTransferReviewCtx is ApproveTransferReview with per-call audit
// actor attribution (LG-73): the approval audit event records the
// operator carried by ctx when set. The reviewer parameter remains the
// business record of who approved (dual-control evidence); the actor is
// the audit attribution of who invoked the call.
func (l *Ledger) ApproveTransferReviewCtx(ctx context.Context, transferID, reviewer string) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	r, ok := l.reviews[transferID]
	if !ok {
		return TransferReceipt{}, ErrReviewNotFound
	}
	if r.Status == ReviewStatusApproved {
		dup := *r.ApprovedReceipt
		dup.Duplicate = true
		// The idempotent replay of a quorum-settled review returns the
		// original receipt without the vote tally: the tally described
		// the settling call, not this replay. The review record (and
		// the reconcile report) still carries the full vote history.
		dup.QuorumTally = nil
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
	if reviewer == "" {
		reviewer = defaultReviewOperator
	}

	// LG-124: while a quorum policy is active, the approval is a named
	// vote (see review_quorum.go) instead of an immediate settlement.
	if qp := l.reviewQuorum; qp != nil {
		return l.castQuorumApproveVoteLocked(ctx, r, reviewer, qp, now)
	}
	return l.settleReviewLocked(ctx, r, reviewer, now, nil)
}

// settleReviewLocked runs the LG-42 approval settlement for one pending
// review: the risk checks re-run against current state, then the frozen
// legs flip to effective and commit exactly like a direct PostTransfer —
// balances and totals apply, the ledger version bumps once per leg, one
// audit-chain link lands per leg, and the structured audit log records
// the reviewer's identity in its hash chain.
//
// A leg through a now-frozen account fails with ErrAccountFrozen, an
// overdraft-protected payer that can no longer cover the outflow fails
// with ErrAccountOverdraft, and legs dated in a now-closed period fail
// with ErrPeriodClosed — in all three cases the review stays pending, so
// the operator can retry once the underlying condition clears. tally is
// the quorum vote tally to disclose on the settlement receipt, or nil
// for a direct single-operator approval. Callers must hold the write
// lock.
func (l *Ledger) settleReviewLocked(ctx context.Context, r *TransferReview, reviewer string, now time.Time, tally *ReviewQuorumTally) (TransferReceipt, error) {
	// Re-verify the risk controls against current state: the freeze was
	// advisory, so anything that changed between review creation and
	// approval is re-checked now, exactly like Capture re-checks the
	// hold's legs. The idempotency replay check does not apply here —
	// approval is its own idempotent operation, handled above.
	for _, e := range r.Entries {
		if l.closedLocked(e.DebitAccount) || l.closedLocked(e.CreditAccount) {
			return TransferReceipt{}, ErrAccountClosed
		}
		if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
			return TransferReceipt{}, ErrAccountFrozen
		}
		if l.suspendedLocked(e.DebitAccount) || l.suspendedLocked(e.CreditAccount) {
			return TransferReceipt{}, ErrAccountSuspended
		}
	}
	// Overdraft protection guards the payer's total outflow, exactly
	// like PostTransfer: a protected payer whose balance dropped below
	// the frozen outflow since review creation cannot settle. The
	// comparison never subtracts, so it cannot overflow.
	if l.noOverdraft[r.Transfer.From] &&
		l.balances[accountCurrency{account: r.Transfer.From, currency: r.Transfer.Currency}] < r.TotalOutflowCents {
		return TransferReceipt{}, ErrAccountOverdraft
	}
	// The period gate is keyed on the legs' timestamps: a period closed
	// after review creation blocks settlement with ErrPeriodClosed, and
	// the review stays pending (see the doc comment above).
	if err := l.periodRejectedLocked(r.Entries[0].CreatedAt); err != nil {
		return TransferReceipt{}, err
	}

	// Atomic settle: flip the marker to effective, then commit every
	// leg exactly like PostTransfer would — balances, totals, version
	// bumps, and chain links land together under the one write lock.
	l.maybePruneIdempotencyKeys(now)
	versionBefore := l.version
	hookTouched := touchedAccounts(r.Entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for i := range r.Entries {
		r.Entries[i].PendingReview = false
		// The settled row must carry a private copy of the attachments
		// slice, like commitEntryLocked (see isolateAttachments): the
		// review record's entries still alias the submitter's array.
		e := isolateAttachments(r.Entries[i])
		r.Entries[i] = e
		l.entries[e.ID] = e
		if e.IdempotencyKey != "" {
			// Refresh the idempotency index copy: it still carries the
			// pending marker from review creation.
			l.byKey[e.IdempotencyKey] = e
		}
		// The per-account index was already appended at review
		// creation — re-appending would duplicate it. Balances,
		// totals, the version bump, and the chain link land here, for
		// the first time: this is the settlement commit.
		l.balances[accountCurrency{account: e.DebitAccount, currency: e.Currency}] += e.AmountCents
		l.balances[accountCurrency{account: e.CreditAccount, currency: e.Currency}] -= e.AmountCents
		l.debitTotals[accountCurrency{account: e.DebitAccount, currency: e.Currency}] += e.AmountCents
		l.creditTotals[accountCurrency{account: e.CreditAccount, currency: e.Currency}] += e.AmountCents
		l.version++
		l.appendChainLink(e)
	}
	r.Status = ReviewStatusApproved
	r.ReviewedBy = reviewer
	r.ReviewedAt = now

	entryIDs := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		entryIDs = append(entryIDs, e.ID)
	}
	// Settlement cutoff evaluation (see settlement_cutoff.go): the money
	// becomes effective at approval time, so the cutoff is evaluated
	// against the approval instant — the booking is never blocked, only
	// the settlement window is decided.
	approvalSettlementStatus, approvalSettlementWindow := l.deferSettlementIfPastCutoffLocked(
		r.Transfer.Currency, now, r.TransferID, entryIDs, []AccountID{r.Transfer.From, r.Transfer.To})
	receipt := TransferReceipt{
		TransferID:   r.TransferID,
		Entries:      append([]JournalEntry(nil), r.Entries...),
		FeeCents:     r.FeeCents,
		FeeTierIndex: r.FeeTierIndex,
		FeeRateBps:   r.FeeRateBps,
		Duplicate:    false,
		FX:           r.FX,
		ReviewStatus: ReviewStatusApproved,
		// The fee-split breakdown rebuilds from the settled legs'
		// memos, like an idempotent replay (see fee_split.go).
		FeeSplits:        feeSplitSharesFromLegs(r.TransferID, r.Entries),
		SettlementStatus: approvalSettlementStatus,
		SettlementWindow: approvalSettlementWindow,
		// LG-124: the final quorum tally, when the settlement came
		// from an N-of-M vote; nil for a direct single-operator
		// approval.
		QuorumTally: tally,
	}
	r.ApprovedReceipt = &receipt
	l.reviews[r.TransferID] = r

	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(r.Entries), r.TransferID, "ApproveTransferReview")
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_approved",
		Actor:         "ApproveTransferReview",
		TraceID:       r.TransferID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details: map[string]any{
			"amount_cents": r.Transfer.AmountCents,
			"currency":     r.Transfer.Currency,
			"fee_cents":    r.FeeCents,
			"reviewed_by":  reviewer,
		},
	})
	// Domain outbox: the approval settles the legs — one entry_posted
	// per effectuated entry, then the review_approved envelope. The
	// original transfer_posted was never emitted (the transfer froze
	// into review instead of settling), so the envelope carries the
	// transfer's identity for downstream correlation.
	l.emitOutboxEntriesLocked(r.Entries)
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxReviewApproved,
		Account:     r.Transfer.From,
		EntryID:     r.Entries[0].ID,
		AmountCents: r.Transfer.AmountCents,
		Currency:    r.Transfer.Currency,
		Details: map[string]any{
			"transfer_id": r.TransferID,
			"to":          string(r.Transfer.To),
			"fee_cents":   r.FeeCents,
			"reviewed_by": reviewer,
		},
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, r.TransferID)

	return receipt, nil
}

// RejectTransferReview drops a pending review without settling anything:
// the frozen funds are released back to the payer's available balance
// (the pending review simply stops reserving them) and the legs stay in
// the journal as historical pending-marked rows. Rejecting is
// idempotent: a review that is already rejected or expired returns
// as-is with changed == false and no error. Rejecting an approved
// review fails with ErrReviewNotPending — a settled transfer cannot be
// un-settled through review. An empty reviewer is recorded as
// "operator".
//
// While an N-of-M quorum policy is active (see SetReviewQuorumPolicy,
// review_quorum.go), the call is a named reject vote instead of an
// immediate rejection: the reviewer must be a policy member. When the
// policy is RejectDecisive the vote rejects the review at once; when it
// is not, the vote is recorded as dissent and the review stays pending
// (approval or expiry still terminates it). With no policy configured
// the behavior is exactly the LG-42 single-decision path.
func (l *Ledger) RejectTransferReview(transferID, reviewer, reason string) (review *TransferReview, changed bool, err error) {
	return l.RejectTransferReviewCtx(context.Background(), transferID, reviewer, reason)
}

// RejectTransferReviewCtx is RejectTransferReview with per-call audit
// actor attribution (LG-73).
func (l *Ledger) RejectTransferReviewCtx(ctx context.Context, transferID, reviewer, reason string) (review *TransferReview, changed bool, err error) {
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
	if reviewer == "" {
		reviewer = defaultReviewOperator
	}
	// LG-124: while a quorum policy is active, the rejection is a named
	// vote (see review_quorum.go) instead of an immediate rejection.
	if qp := l.reviewQuorum; qp != nil {
		return l.castQuorumRejectVoteLocked(ctx, r, reviewer, reason, qp)
	}
	return l.rejectReviewLocked(ctx, r, reviewer, reason)
}

// rejectReviewLocked drops a pending review without settling anything:
// the frozen funds are released back to the payer's available balance
// (the pending review simply stops reserving them) and the legs stay in
// the journal as historical pending-marked rows. Only an actual state
// transition is audited: idempotent replays settle nothing and stay
// silent. Callers must hold the write lock.
func (l *Ledger) rejectReviewLocked(ctx context.Context, r *TransferReview, reviewer, reason string) (review *TransferReview, changed bool, err error) {
	now := time.Now()
	r.Status = ReviewStatusRejected
	r.ReviewedBy = reviewer
	r.ReviewedAt = now
	l.reviews[r.TransferID] = r
	// Only an actual state transition is audited: idempotent replays
	// settle nothing and stay silent.
	details := map[string]any{
		"amount_cents": r.Transfer.AmountCents,
		"currency":     r.Transfer.Currency,
		"reviewed_by":  reviewer,
	}
	if reason != "" {
		details["reason"] = reason
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_rejected",
		Actor:         "RejectTransferReview",
		TraceID:       r.TransferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
		Details:       details,
	})
	// Domain outbox: the rejection settles nothing — no entry_posted —
	// but the review's outcome is itself a domain fact.
	rejectDetails := map[string]any{
		"transfer_id": r.TransferID,
		"to":          string(r.Transfer.To),
		"reviewed_by": reviewer,
	}
	if reason != "" {
		rejectDetails["reason"] = reason
	}
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxReviewRejected,
		Account:     r.Transfer.From,
		AmountCents: r.Transfer.AmountCents,
		Currency:    r.Transfer.Currency,
		Details:     rejectDetails,
	})
	out := *r
	return &out, true, nil
}

// expireReviewLocked marks one lapsed pending review as expired,
// releasing its frozen funds. Callers must hold the write lock; the
// audit event is emitted by the caller (ExpireReviewsAt batches one
// event per sweep, ApproveTransferReview emits its own).
func (l *Ledger) expireReviewLocked(r *TransferReview, now time.Time) {
	r.Status = ReviewStatusExpired
	r.ReviewedBy = "system"
	r.ReviewedAt = now
	l.reviews[r.TransferID] = r
}

// emitReviewExpireAuditLocked emits the batched auto-expiry audit event.
// Callers must hold l.mu.
func (l *Ledger) emitReviewExpireAuditLocked(expired int) {
	l.emitReviewExpireAuditCtxLocked(context.Background(), expired)
}

// emitReviewExpireAuditCtxLocked is emitReviewExpireAuditLocked with
// per-call audit actor attribution (LG-73). Callers must hold l.mu.
func (l *Ledger) emitReviewExpireAuditCtxLocked(ctx context.Context, expired int) {
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_review_expire",
		Actor:         "ExpireReviews",
		TraceID:       fmt.Sprintf("review-expire@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"expired": expired,
		},
	})
}

// ExpireReviews marks every pending review whose ExpiresAt has passed as
// expired, releasing its frozen funds, and returns how many were
// marked. Expired reviews were already inactive for available-balance
// purposes (expiry is lazy by predicate); the sweep exists so operators
// and the reconcile report can observe which reviews lapsed. It never
// touches approved, rejected, or already-expired reviews, and it does
// not bump the ledger version.
func (l *Ledger) ExpireReviews() int {
	return l.ExpireReviewsCtx(context.Background())
}

// ExpireReviewsCtx is ExpireReviews with per-call audit actor attribution
// (LG-73): the expiry audit event records the operator carried by ctx
// when set. The background review sweeper calls the plain variant, so
// its events keep the entrypoint actor.
func (l *Ledger) ExpireReviewsCtx(ctx context.Context) int {
	return l.ExpireReviewsAtCtx(ctx, time.Now())
}

// ExpireReviewsAt is ExpireReviews with a caller-supplied timestamp, so
// tests can pin expiry deterministically.
func (l *Ledger) ExpireReviewsAt(now time.Time) int {
	return l.ExpireReviewsAtCtx(context.Background(), now)
}

// ExpireReviewsAtCtx is ExpireReviewsAt with per-call audit actor
// attribution (LG-73).
func (l *Ledger) ExpireReviewsAtCtx(ctx context.Context, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked := 0
	var expired []*TransferReview
	for _, r := range l.reviews {
		if r.Status == ReviewStatusPending && reviewExpiredLocked(r, now) {
			l.expireReviewLocked(r, now)
			marked++
			expired = append(expired, r)
		}
	}
	// A sweep that expires nothing emits no event: the background
	// sweeper ticks on a timer, and zero-expire ticks would drown the
	// audit trail in noise (see ExpireHoldsAt for the convention).
	if marked > 0 {
		l.emitReviewExpireAuditCtxLocked(ctx, marked)
		// Domain outbox: one review_expired per review — the expiry
		// releases the reserved funds back to available, a domain fact.
		for _, r := range expired {
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxReviewExpired,
				Account:     r.Transfer.From,
				AmountCents: r.Transfer.AmountCents,
				Currency:    r.Transfer.Currency,
				Details: map[string]any{
					"transfer_id": r.TransferID,
					"to":          string(r.Transfer.To),
				},
			})
		}
	}
	return marked
}

// pendingReviewByKeyLocked reports whether the transfer idempotency key
// belongs to a still-pending review. The idempotency TTL pruning skips
// such keys: dropping the replay index of a pending review would turn a
// later replay into a transfer-ID conflict instead of the review
// receipt. Callers must hold l.mu.
func (l *Ledger) pendingReviewByKeyLocked(key string) bool {
	ids, ok := l.transferKeys[key]
	if !ok || len(ids) == 0 {
		return false
	}
	r, ok := l.reviews[ids[0]]
	return ok && r.Status == ReviewStatusPending
}
