// Installment-plan period-count amendment (LG-285).
//
// OpenInstallmentPlan fixes a plan's period count at creation
// (LG-134); AmendInstallmentPlanRule (LG-264) owns the billing-rule
// revision, AmendInstallmentPlanFirstDueAt (LG-265) the first-due-date
// revision, AmendInstallmentPlanCurrency (LG-266) the currency
// revision, and AmendInstallmentPlanPrincipal (LG-284) the principal
// revision. This module is the period-count path: an operator revises
// a live plan's number of periods instead of cancelling it and
// creating a new one. Amending in place matters for the same reason
// as the sibling amendments: cancelling plus re-creating is a new
// plan — a fresh ID that breaks the plan's period history and its
// "installment/<ID>/<seq>" idempotency lineage — while an amendment
// keeps the same plan, the same legs, and one audit trail from
// creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or overdue plan with zero posted periods may be
//     amended. An unknown ID fails with ErrInstallmentNotFound; a
//     completed or settled_early plan fails with
//     ErrInstallmentBadTransition. A plan that has already posted a
//     period fails with ErrInstallmentBadTransition as well: with
//     zero posted periods no period has been journaled, so changing
//     the period count restates nothing — every period amount and
//     every due date is still uncomputed. Once a period is posted,
//     its journaled row fixes the amount that period collected and
//     the due date it collected on, and moving the period count
//     would restate the plan's economics mid-flight; that is out of
//     scope for this amendment. (The backlog item names no separate
//     error for the posted case; ErrInstallmentBadTransition is the
//     existing "this transition is not allowed for this plan"
//     error, and the wrap message names the posted-period count.)
//     An overdue plan with zero posted periods is amendable —
//     overdue is a hold, not a terminal state — and the new period
//     count governs the catch-up sweep exactly as it would govern
//     the next period of an active plan.
//   - The new period count reuses the creation gate (see
//     OpenInstallmentPlan): it must be at least 1, else
//     ErrInvalidInstallmentPeriods. A failed amendment leaves the
//     original period count exactly as it was.
//   - The amendment touches NumPeriods and nothing else: the payer,
//     payee, principal, rule, first due date, currency, legs,
//     status, and posted-period count are unchanged. Period amounts
//     are recomputed from the live principal and period count at
//     sweep time (installmentPeriodAmount: floor division with the
//     final period absorbing the remainder), and due dates are
//     recomputed from the live first due date and rule
//     (installmentDueAt), so the new period count governs every
//     unposted period from the next sweep with no sweep-path
//     change — and because no period has posted yet, there is no
//     posted history to reconcile: the periods still sum to exactly
//     the principal, over exactly the new number of periods.
//   - Amending to the current period count is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_num_periods_amended, with the plan ID, the
//     previous and new period counts, and the posted-period count,
//     always zero) so the "who moved the period count, and from
//     what to what" trail is tamper-evident. Installment plans have
//     no Ctx variant and no snapshot mechanism, so this amendment
//     has neither: the period count rides the in-memory plan row
//     exactly like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial period count,
// AmendInstallmentPlanRule owns the billing rule,
// AmendInstallmentPlanFirstDueAt owns the first due date,
// AmendInstallmentPlanCurrency owns the currency,
// AmendInstallmentPlanPrincipal owns the principal revision, and
// this module owns only the period-count revision. It is orthogonal
// to the installment-plan payer/payee amendment (LG-286), which
// moves the collection legs — this moves how the principal is cut
// into periods, and the two compose (either order) because period
// amounts are always derived live from principal and period count.
package ledger

import "fmt"

// AmendInstallmentPlanNumPeriods revises a live installment plan's
// period count in place. Only active or overdue plans with zero
// posted periods may be amended; unknown IDs fail with
// ErrInstallmentNotFound, completed or settled_early plans and plans
// with posted periods fail with ErrInstallmentBadTransition, and a
// period count below 1 fails with ErrInvalidInstallmentPeriods.
// Only NumPeriods changes; the payer, payee, principal, rule, first
// due date, currency, legs, status, and posted periods are
// untouched, and no period has posted yet so nothing is restated —
// the new period count governs every period from the next sweep.
// Amending to the current period count is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as installment_num_periods_amended.
func (l *Ledger) AmendInstallmentPlanNumPeriods(id string, numPeriods int) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's period count", ErrInstallmentBadTransition, plan.Status)
	}
	if plan.PostedPeriods != 0 {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend period count of a plan with %d posted periods", ErrInstallmentBadTransition, plan.PostedPeriods)
	}
	if numPeriods < 1 {
		return InstallmentPlan{}, ErrInvalidInstallmentPeriods
	}
	if plan.NumPeriods == numPeriods {
		return plan, nil
	}
	previous := plan.NumPeriods
	plan.NumPeriods = numPeriods
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_num_periods_amended",
		Actor:         "AmendInstallmentPlanNumPeriods",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":              id,
			"previous_num_periods": previous,
			"num_periods":          numPeriods,
			"posted_periods":       plan.PostedPeriods,
		},
	})
	return plan, nil
}
