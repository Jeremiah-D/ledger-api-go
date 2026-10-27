package ledger

import (
	"errors"
	"time"
)

// Pending-review escalation (LG-232).
//
// A pending dual-control review (see review.go, LG-42) is an operator
// queue: a large transfer waits for a human decision while the payer's
// funds sit frozen. When the queue stalls — a review nobody looked at
// for a day, a shift handover that dropped it — nobody notices unless
// something pages. Escalation is that page: an opt-in sweep that, for
// every still-pending review older than a configured threshold, emits
// one pending_review_escalated audit event and bumps the escalation
// counter, so the operator's alerting pipeline sees the stalled queue.
//
// The escalation is alert-only, never a decision:
//   - It approves nothing and rejects nothing: the review stays
//     pending, the frozen amount and the freeze hold do not move, and
//     the ledger version does not bump — the sweep books no money, so
//     there is nothing for the books or the chain to record. The audit
//     event brackets the same version twice, like the review-expiry
//     sweep's batched event.
//   - It is edge-triggered, not level-triggered: a review escalates
//     once, when the sweep first finds it over the threshold, and the
//     EscalatedAt stamp on the review record is what remembers that.
//     Later sweeps skip it — re-paging every tick for the same stalled
//     review would drown the alert channel in repeats of one fact.
//   - Resolution clears the stamp: approve, reject, and expiry all reset
//     EscalatedAt to zero, so a decided review carries no stale
//     escalation state (the audit trail keeps the history — the
//     pending_review_escalated events stay in the log).
//
// The candidate set is the actionable pending list (see
// pendingTransferReviewsLocked): reviews whose expiry already lapsed
// but which ExpireReviews has not swept yet are already inactive, so
// they are not escalated — expiry owns them. The threshold is strictly
// exceeded, not merely reached: a review exactly threshold-old is not
// yet "overdue". The policy is opt-in and defaults to disabled, like
// the review threshold itself; the counter is operational state, not
// books — it does not round-trip through snapshots (see
// BudgetPaceAlertCount for the convention), while the EscalatedAt stamp
// rides the review record and does survive a snapshot round-trip, so a
// restored ledger never re-pages an already-escalated review.

// ErrInvalidPendingReviewEscalationConfig is returned by
// SetPendingReviewEscalationConfig when an enabled policy carries no
// positive threshold: a zero or negative age would escalate every
// pending review on the first sweep (or never, for a negative one),
// which is a misconfiguration, not a policy. Fail-fast at
// configuration, like every other risk-control threshold in this
// package. A disabled policy ignores its threshold entirely.
var ErrInvalidPendingReviewEscalationConfig = errors.New("ledger: pending-review escalation threshold age must be positive when enabled")

// PendingReviewEscalationConfig is the opt-in escalation policy for
// stalled dual-control reviews: when Enabled, a review still pending
// more than ThresholdAge after its creation escalates on the next
// EscalatePendingReviews sweep. The zero value disables escalation.
type PendingReviewEscalationConfig struct {
	Enabled      bool          `json:"enabled"`
	ThresholdAge time.Duration `json:"threshold_age"`
}

// SetPendingReviewEscalationConfig installs (or replaces) the
// pending-review escalation policy. An enabled policy needs a positive
// ThresholdAge (ErrInvalidPendingReviewEscalationConfig otherwise); a
// disabled policy accepts any threshold, which it ignores. Setting a
// policy is structural config, like SetReviewThreshold: it does not
// bump the ledger version. The policy is in-memory operator config —
// it does not round-trip through snapshots, while the per-review
// escalation stamps do (they ride the review records).
func (l *Ledger) SetPendingReviewEscalationConfig(cfg PendingReviewEscalationConfig) error {
	if cfg.Enabled && cfg.ThresholdAge <= 0 {
		return ErrInvalidPendingReviewEscalationConfig
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pendingReviewEscalationCfg = cfg
	return nil
}

// PendingReviewEscalationConfig reports the installed escalation
// policy, or the disabled zero value when none was set.
func (l *Ledger) PendingReviewEscalationConfig() PendingReviewEscalationConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pendingReviewEscalationCfg
}

// PendingReviewEscalationCount reports how many pending-review
// escalations have fired since process start: the ledger-side counter
// for the pending_review_escalated audit events. Operational metric,
// not books — it does not round-trip through snapshots.
func (l *Ledger) PendingReviewEscalationCount() uint64 {
	return l.pendingReviewEscalations.Load()
}

// EscalatePendingReviews escalates every still-pending review older
// than the configured threshold at now, and returns the newly escalated
// reviews (copies, in pending-list order) — nil when escalation is
// disabled or nothing is overdue. Each newly escalated review gets its
// EscalatedAt stamp set to now, one pending_review_escalated audit
// event naming the transfer, the amount, and the pending-since time,
// and one counter increment.
//
// The sweep is edge-triggered: a review with a non-zero EscalatedAt is
// skipped, however old it is, so one stalled review pages exactly once
// until it is decided (which clears the stamp). The sweep moves no
// money: amounts, freeze holds, and the ledger version are untouched,
// and the review stays pending for its operator decision.
func (l *Ledger) EscalatePendingReviews(now time.Time) []TransferReview {
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg := l.pendingReviewEscalationCfg
	if !cfg.Enabled {
		return nil
	}
	var escalated []TransferReview
	for _, r := range l.pendingTransferReviewsLocked(now) {
		// Edge-triggered: an already-escalated review never pages
		// twice. Strictly over the threshold: a review exactly
		// threshold-old is not yet overdue.
		if !r.EscalatedAt.IsZero() || now.Sub(r.CreatedAt) <= cfg.ThresholdAge {
			continue
		}
		// Mutate the registry record itself, not the pending-list
		// copy: the stamp is what makes the next sweep skip it.
		if rec, ok := l.reviews[r.TransferID]; ok {
			rec.EscalatedAt = now
			r = *rec
		}
		l.pendingReviewEscalations.Add(1)
		l.emitAudit(AuditEvent{
			Op:            "pending_review_escalated",
			Actor:         "EscalatePendingReviews",
			TraceID:       r.TransferID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Transfer.From, r.Transfer.To},
			Details: map[string]any{
				"transfer_id":   r.TransferID,
				"amount_cents":  r.Transfer.AmountCents,
				"currency":      r.Transfer.Currency,
				"pending_since": r.CreatedAt.UTC().Format(time.RFC3339),
				"escalated_at":  now.UTC().Format(time.RFC3339),
			},
		})
		escalated = append(escalated, r)
	}
	return escalated
}
