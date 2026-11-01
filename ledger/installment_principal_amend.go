// Installment-plan principal amendment (LG-284).
//
// OpenInstallmentPlan fixes a plan's principal at creation (LG-134);
// AmendInstallmentPlanRule (LG-264) owns the billing-rule revision,
// AmendInstallmentPlanFirstDueAt (LG-265) the first-due-date revision,
// and AmendInstallmentPlanCurrency (LG-266) the currency revision.
// This module is the principal path: an operator revises a live
// plan's principal instead of cancelling it and creating a new one.
// Amending in place matters for the same reason as the sibling
// amendments: cancelling plus re-creating is a new plan — a fresh ID
// that breaks the plan's period history and its
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
//     the principal restates nothing — every period amount is still
//     uncomputed. Once a period is posted, its journaled row fixes
//     the amount that period collected, and moving the principal
//     would restate the plan's economics mid-flight; that is out of
//     scope for this amendment. (The backlog item names no separate
//     error for the posted case; ErrInstallmentBadTransition is the
//     existing "this transition is not allowed for this plan"
//     error, and the wrap message names the posted-period count.)
//     An overdue plan with zero posted periods is amendable —
//     overdue is a hold, not a terminal state — and the new
//     principal governs the catch-up sweep exactly as it would
//     govern the next period of an active plan.
//   - The new principal reuses the creation gate (see
//     OpenInstallmentPlan): it must be positive, else
//     ErrInvalidInstallmentAmount. A failed amendment leaves the
//     original principal exactly as it was.
//   - The amendment touches PrincipalCents and nothing else: the
//     payer, payee, period count, rule, first due date, currency,
//     legs, status, and posted-period count are unchanged. Period
//     amounts are recomputed from the live principal at sweep time
//     (installmentPeriodAmount: floor division with the final
//     period absorbing the remainder), so the new principal governs
//     every unposted period from the next sweep with no sweep-path
//     change — and because no period has posted yet, there is no
//     posted history to reconcile: the periods still sum to exactly
//     the new principal.
//   - Amending to the current principal is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_principal_amended, with the plan ID, the
//     previous and new principals, and the posted-period count,
//     always zero) so the "who moved the principal, and from what
//     to what" trail is tamper-evident. Installment plans have no
//     Ctx variant and no snapshot mechanism, so this amendment has
//     neither: the principal rides the in-memory plan row exactly
//     like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial principal,
// AmendInstallmentPlanRule owns the billing rule,
// AmendInstallmentPlanFirstDueAt owns the first due date,
// AmendInstallmentPlanCurrency owns the currency, and this module
// owns only the principal revision. It is orthogonal to the
// installment-plan period-count amendment (LG-285), which moves the
// number of periods — this moves the principal, and the two compose
// (either order) because period amounts are always derived live
// from principal and period count.
package ledger

import "fmt"

// AmendInstallmentPlanPrincipal revises a live installment plan's
// principal in place. Only active or overdue plans with zero posted
// periods may be amended; unknown IDs fail with
// ErrInstallmentNotFound, completed or settled_early plans and plans
// with posted periods fail with ErrInstallmentBadTransition, and a
// non-positive principal fails with ErrInvalidInstallmentAmount.
// Only PrincipalCents changes; the payer, payee, period count, rule,
// first due date, currency, legs, status, and posted periods are
// untouched, and no period has posted yet so nothing is restated —
// the new principal governs every period from the next sweep.
// Amending to the current principal is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as installment_principal_amended.
func (l *Ledger) AmendInstallmentPlanPrincipal(id string, principalCents int64) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's principal", ErrInstallmentBadTransition, plan.Status)
	}
	if plan.PostedPeriods != 0 {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend principal of a plan with %d posted periods", ErrInstallmentBadTransition, plan.PostedPeriods)
	}
	if principalCents <= 0 {
		return InstallmentPlan{}, ErrInvalidInstallmentAmount
	}
	if plan.PrincipalCents == principalCents {
		return plan, nil
	}
	previous := plan.PrincipalCents
	plan.PrincipalCents = principalCents
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_principal_amended",
		Actor:         "AmendInstallmentPlanPrincipal",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":                  id,
			"previous_principal_cents": previous,
			"principal_cents":          principalCents,
			"posted_periods":           plan.PostedPeriods,
		},
	})
	return plan, nil
}
