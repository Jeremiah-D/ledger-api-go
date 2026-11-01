// Installment-plan payer/payee amendment (LG-286).
//
// OpenInstallmentPlan fixes a plan's payer and payee at creation
// (LG-134); AmendInstallmentPlanRule (LG-264) owns the billing-rule
// revision, AmendInstallmentPlanFirstDueAt (LG-265) the first-due-date
// revision, AmendInstallmentPlanCurrency (LG-266) the currency
// revision, AmendInstallmentPlanPrincipal (LG-284) the principal, and
// AmendInstallmentPlanNumPeriods (LG-285) the period count. This
// module is the parties path: an operator revises a live plan's payer
// or payee in place — a buyer whose bank account changed, a merchant
// whose settlement account moved — instead of cancelling the plan and
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
//     the parties restates nothing — no journaled row references the
//     old accounts. Once a period is posted, its journaled row fixes
//     the debit/credit pair that period collected, and moving the
//     payer or payee would restate the plan's economics mid-flight;
//     that is out of scope for this amendment. (The backlog item
//     names no separate error for the posted case;
//     ErrInstallmentBadTransition is the existing "this transition
//     is not allowed for this plan" error, and the wrap message
//     names the posted-period count.) An overdue plan with zero
//     posted periods is amendable — overdue is a hold, not a
//     terminal state — and the new parties govern the catch-up
//     sweep exactly as they would govern the next period of an
//     active plan.
//   - The new party reuses the creation gate (see
//     OpenInstallmentPlan): an empty payer fails with
//     ErrEmptyInstallmentPayer, an empty payee with
//     ErrEmptyInstallmentPayee, and a payer equal to the live payee
//     (or a payee equal to the live payer) fails with
//     ErrInstallmentSameAccount. The same-account check reads the
//     live other party, so amending the payer to the current payee
//     — or the payee to the current payer — is refused exactly as
//     creation would refuse it. A failed amendment leaves the
//     original parties exactly as they were.
//   - Each method touches exactly one field and nothing else: the
//     payer amendment moves Payer only, the payee amendment moves
//     Payee only; principal, period count, rule, first due date,
//     currency, legs, status, and posted periods are unchanged. The
//     sweep reads the live parties at post time
//     (DebitAccount: plan.Payee, CreditAccount: plan.Payer), so the
//     new parties govern every unposted period from the next sweep
//     with no sweep-path change — and because no period has posted
//     yet, there is no posted history to reconcile: the periods
//     still sum to exactly the principal, just against the new
//     accounts.
//   - Amending to the current party is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_payer_amended or installment_payee_amended, with
//     the plan ID, the previous and new party, and the posted-period
//     count, always zero) so the "who moved the payer/payee, and
//     from what to what" trail is tamper-evident. Installment plans
//     have no Ctx variant and no snapshot mechanism, so this
//     amendment has neither: the parties ride the in-memory plan
//     row exactly like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial parties,
// AmendInstallmentPlanPrincipal (LG-284) owns the principal revision
// and AmendInstallmentPlanNumPeriods (LG-285) owns the period-count
// revision; this module owns only the payer/payee revision, and the
// two methods are independent of each other — each moves one party
// and leaves the other alone — so the three compose (any order)
// because the sweep always reads the live plan row.
package ledger

import "fmt"

// amendInstallmentPlanParty is the shared gate for the payer and
// payee amendments: not-found, status, and posted-period checks in
// the sibling-amendment order, then the creation gate for the new
// party against the live other party. It returns the plan and the
// validated new party, or an error with the plan untouched. The
// caller performs the single-field mutation and its audit event.
func (l *Ledger) amendInstallmentPlanParty(id string, newParty AccountID, isPayer bool) (InstallmentPlan, error) {
	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	what := "payee"
	if isPayer {
		what = "payer"
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's %s", ErrInstallmentBadTransition, plan.Status, what)
	}
	if plan.PostedPeriods != 0 {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend %s of a plan with %d posted periods", ErrInstallmentBadTransition, what, plan.PostedPeriods)
	}
	if newParty == "" {
		if isPayer {
			return InstallmentPlan{}, ErrEmptyInstallmentPayer
		}
		return InstallmentPlan{}, ErrEmptyInstallmentPayee
	}
	other := plan.Payer
	if isPayer {
		other = plan.Payee
	}
	if newParty == other {
		return InstallmentPlan{}, ErrInstallmentSameAccount
	}
	return plan, nil
}

// AmendInstallmentPlanPayer revises a live installment plan's payer
// in place. Only active or overdue plans with zero posted periods
// may be amended; unknown IDs fail with ErrInstallmentNotFound,
// completed or settled_early plans and plans with posted periods
// fail with ErrInstallmentBadTransition, and an empty payer or a
// payer equal to the live payee fails with ErrEmptyInstallmentPayer
// or ErrInstallmentSameAccount. Only Payer changes; the payee,
// principal, period count, rule, first due date, currency, legs,
// status, and posted periods are untouched, and no period has posted
// yet so nothing is restated — the new payer governs every period
// from the next sweep. Amending to the current payer is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as installment_payer_amended.
func (l *Ledger) AmendInstallmentPlanPayer(id string, newPayer AccountID) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, err := l.amendInstallmentPlanParty(id, newPayer, true)
	if err != nil {
		return InstallmentPlan{}, err
	}
	if plan.Payer == newPayer {
		return plan, nil
	}
	previous := plan.Payer
	plan.Payer = newPayer
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_payer_amended",
		Actor:         "AmendInstallmentPlanPayer",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":        id,
			"previous_payer": previous,
			"payer":          newPayer,
			"posted_periods": plan.PostedPeriods,
		},
	})
	return plan, nil
}

// AmendInstallmentPlanPayee revises a live installment plan's payee
// in place. Only active or overdue plans with zero posted periods
// may be amended; unknown IDs fail with ErrInstallmentNotFound,
// completed or settled_early plans and plans with posted periods
// fail with ErrInstallmentBadTransition, and an empty payee or a
// payee equal to the live payer fails with ErrEmptyInstallmentPayee
// or ErrInstallmentSameAccount. Only Payee changes; the payer,
// principal, period count, rule, first due date, currency, legs,
// status, and posted periods are untouched, and no period has posted
// yet so nothing is restated — the new payee governs every period
// from the next sweep. Amending to the current payee is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as installment_payee_amended.
func (l *Ledger) AmendInstallmentPlanPayee(id string, newPayee AccountID) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, err := l.amendInstallmentPlanParty(id, newPayee, false)
	if err != nil {
		return InstallmentPlan{}, err
	}
	if plan.Payee == newPayee {
		return plan, nil
	}
	previous := plan.Payee
	plan.Payee = newPayee
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_payee_amended",
		Actor:         "AmendInstallmentPlanPayee",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":        id,
			"previous_payee": previous,
			"payee":          newPayee,
			"posted_periods": plan.PostedPeriods,
		},
	})
	return plan, nil
}
