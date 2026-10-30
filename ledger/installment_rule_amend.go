// Installment-plan billing-rule amendment (LG-264).
//
// OpenInstallmentPlan fixes a plan's billing rule at creation
// (LG-134); SettleInstallmentEarly owns the early-settlement
// transition. This module is the first in-place rule path for
// installment plans, alongside the transfer-schedule revisions
// (LG-253–LG-263): an operator revises a live plan's billing rule
// instead of cancelling it and creating a new one. Amending in
// place matters for the same reason as those amendments:
// cancelling plus re-creating is a new plan — a fresh ID that
// breaks the plan's period history and its
// "installment/<ID>/<seq>" idempotency lineage — while an
// amendment keeps the same plan, the same legs, and one audit
// trail from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or overdue plan may be amended. An unknown ID
//     fails with ErrInstallmentNotFound; a completed or
//     settled_early plan fails with ErrInstallmentBadTransition,
//     the installment analogue of ErrScheduleBadTransition. An
//     overdue plan is amendable because overdue is a hold, not a
//     terminal state: the rule revision simply waits with the plan
//     and governs the next sweep's unposted periods, exactly as it
//     would govern the next period of an active plan.
//   - The new rule reuses the creation gate (see
//     OpenInstallmentPlan): it must be monthly, weekly, or daily,
//     via ErrInvalidInstallmentRule. A failed amendment leaves the
//     original rule exactly as it was.
//   - The amendment touches Rule and nothing else: the principal,
//     period count, first due date, currency, legs, status, and
//     posted-period count are unchanged. There is no amount to
//     recompute — period amounts depend only on principal and
//     period count (installmentPeriodAmount: floor division with
//     the final period absorbing the remainder), never on the rule
//     — so the revision is a single-field write.
//   - InstallmentsDue reads the live rule at sweep time
//     (installmentDueAt over the stored plan, anchored at
//     FirstDueAt), so the new rule governs unposted periods from
//     the next sweep with no sweep-path change: an unposted period
//     already due under the new rule books normally at the next
//     sweep, and already-posted periods are journaled rows whose
//     due dates and amounts are never rewritten.
//   - Amending to the current rule is a no-op: the row is
//     untouched and no audit event fires, so a retried or
//     redundant revision does not pollute the audit trail with a
//     change that changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_rule_amended, with the plan ID, the previous and
//     new rules, and the posted-period count) so the "who moved the
//     billing rule, and from what to what" trail is tamper-evident.
//     Installment plans have no Ctx variant and no snapshot
//     mechanism, so this amendment has neither: the rule rides the
//     in-memory plan row exactly like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial rule,
// SettleInstallmentEarly owns early settlement, InstallmentsDue
// owns firing, and this module owns only the rule revision. It is
// orthogonal to the transfer-schedule interval amendment (LG-254),
// which moves a scheduled transfer's firing interval — this moves
// an installment plan's billing rule.
package ledger

import "fmt"

// AmendInstallmentPlanRule revises a live installment plan's billing
// rule in place. Only active or overdue plans may be amended;
// unknown IDs fail with ErrInstallmentNotFound and completed or
// settled_early plans fail with ErrInstallmentBadTransition. The new
// rule must be monthly, weekly, or daily
// (ErrInvalidInstallmentRule). Only Rule changes; the principal,
// period count, first due date, currency, legs, status, and posted
// periods are untouched, and already-posted periods are never
// rewritten — the new rule governs unposted periods from the next
// sweep. Amending to the current rule is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as installment_rule_amended.
func (l *Ledger) AmendInstallmentPlanRule(id string, rule string) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's rule", ErrInstallmentBadTransition, plan.Status)
	}
	switch rule {
	case InstallmentMonthly, InstallmentWeekly, InstallmentDaily:
	default:
		return InstallmentPlan{}, ErrInvalidInstallmentRule
	}
	if plan.Rule == rule {
		return plan, nil
	}
	previous := plan.Rule
	plan.Rule = rule
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_rule_amended",
		Actor:         "AmendInstallmentPlanRule",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":        id,
			"previous_rule":  previous,
			"rule":           rule,
			"posted_periods": plan.PostedPeriods,
		},
	})
	return plan, nil
}
