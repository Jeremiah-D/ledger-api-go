// Installment-plan first-due-date amendment (LG-265).
//
// OpenInstallmentPlan fixes a plan's first due date at creation
// (LG-134); AmendInstallmentPlanRule (LG-264) owns the billing-rule
// revision. This module is the first-due-date path: an operator moves
// a live plan's anchor date instead of cancelling it and creating a
// new one. Amending in place matters for the same reason as the rule
// amendment: cancelling plus re-creating is a new plan — a fresh ID
// that breaks the plan's period history and its
// "installment/<ID>/<seq>" idempotency lineage — while an amendment
// keeps the same plan, the same legs, and one audit trail from
// creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or overdue plan may be amended. An unknown ID
//     fails with ErrInstallmentNotFound; a completed or
//     settled_early plan fails with ErrInstallmentBadTransition.
//     An overdue plan is amendable because overdue is a hold, not a
//     terminal state: the date revision simply waits with the plan
//     and governs the next sweep's unposted periods, exactly as it
//     would govern the next period of an active plan.
//   - The new first due date reuses the creation gate (see
//     OpenInstallmentPlan): it must not be the zero time, via the
//     new ErrInvalidInstallmentFirstDueAt. A failed amendment leaves
//     the original first due date exactly as it was.
//   - The amendment touches FirstDueAt and nothing else: the
//     principal, period count, rule, currency, legs, status, and
//     posted-period count are unchanged. There is no amount to
//     recompute — period amounts depend only on principal and
//     period count (installmentPeriodAmount: floor division with
//     the final period absorbing the remainder), never on the first
//     due date — so the revision is a single-field write.
//   - InstallmentsDue reads the live first due date at sweep time
//     (installmentDueAt over the stored plan, anchored at the live
//     FirstDueAt), so the new date governs unposted periods from
//     the next sweep with no sweep-path change: an unposted period
//     already due under the new date books normally at the next
//     sweep, and already-posted periods are journaled rows whose
//     due dates and amounts are never rewritten. Moving the first
//     due date later can leave an unposted period's due date
//     earlier than an already-posted one — the due date only
//     decides sweep release order, it never reorders posted
//     history.
//   - Amending to the current first due date is a no-op: the row is
//     untouched and no audit event fires, so a retried or
//     redundant revision does not pollute the audit trail with a
//     change that changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_first_due_at_amended, with the plan ID, the
//     previous and new first due dates, and the posted-period
//     count) so the "who moved the anchor date, and from what to
//     what" trail is tamper-evident. Installment plans have no Ctx
//     variant and no snapshot mechanism, so this amendment has
//     neither: the first due date rides the in-memory plan row
//     exactly like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial first due
// date, AmendInstallmentPlanRule owns the billing rule, and this
// module owns only the first-due-date revision. It is orthogonal to
// the transfer-schedule next-run amendment (LG-262), which moves a
// scheduled transfer's next firing time — this moves an installment
// plan's first due date.
package ledger

import (
	"fmt"
	"time"
)

// AmendInstallmentPlanFirstDueAt revises a live installment plan's
// first due date in place. Only active or overdue plans may be
// amended; unknown IDs fail with ErrInstallmentNotFound and completed
// or settled_early plans fail with ErrInstallmentBadTransition. The
// new date must not be the zero time
// (ErrInvalidInstallmentFirstDueAt). Only FirstDueAt changes; the
// principal, period count, rule, currency, legs, status, and posted
// periods are untouched, and already-posted periods are never
// rewritten — the new date governs unposted periods from the next
// sweep. Amending to the current first due date is a no-op. Amending
// is off-journal — it does not bump the ledger version — but it is
// audited as installment_first_due_at_amended.
func (l *Ledger) AmendInstallmentPlanFirstDueAt(id string, firstDueAt time.Time) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's first due date", ErrInstallmentBadTransition, plan.Status)
	}
	if firstDueAt.IsZero() {
		return InstallmentPlan{}, ErrInvalidInstallmentFirstDueAt
	}
	if plan.FirstDueAt.Equal(firstDueAt) {
		return plan, nil
	}
	previous := plan.FirstDueAt
	plan.FirstDueAt = firstDueAt
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_first_due_at_amended",
		Actor:         "AmendInstallmentPlanFirstDueAt",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":               id,
			"previous_first_due_at": previous.UTC().Format(time.RFC3339),
			"first_due_at":          firstDueAt.UTC().Format(time.RFC3339),
			"posted_periods":        plan.PostedPeriods,
		},
	})
	return plan, nil
}
