// Installment-plan currency amendment (LG-266).
//
// OpenInstallmentPlan fixes a plan's settlement currency at creation
// (LG-134); AmendInstallmentPlanRule (LG-264) owns the billing-rule
// revision and AmendInstallmentPlanFirstDueAt (LG-265) the first-due-
// date revision. This module is the currency path: an operator revises
// a live plan's settlement currency instead of cancelling it and
// creating a new one. Amending in place matters for the same reason as
// the rule and date amendments: cancelling plus re-creating is a new
// plan — a fresh ID that breaks the plan's period history and its
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
//     terminal state: the currency revision simply waits with the
//     plan and governs the next sweep's unposted periods, exactly as
//     it would govern the next period of an active plan.
//   - The new currency reuses the creation gate (see
//     OpenInstallmentPlan and normalizeCurrency): an empty code
//     normalizes to the default currency (USD), and anything that is
//     not a three-letter uppercase ISO 4217 code fails with
//     ErrInvalidCurrency. A failed amendment leaves the original
//     currency exactly as it was.
//   - The amendment touches Currency and nothing else: the payer,
//     payee, principal, period count, rule, first due date, legs,
//     status, and posted-period count are unchanged. There is no
//     amount to recompute — period amounts depend only on principal
//     and period count (installmentPeriodAmount: floor division with
//     the final period absorbing the remainder), never on the
//     currency — so the revision is a single-field write.
//   - InstallmentsDue reads the live currency at sweep time (the
//     journal row for each unposted period is booked with
//     plan.Currency), so the new currency governs unposted periods
//     from the next sweep with no sweep-path change: an unposted
//     period already due under the new currency books normally at
//     the next sweep, and already-posted periods are journaled rows
//     whose currency and amounts are never rewritten. The ledger
//     books per (account, currency) — see accountCurrency — so a
//     plan that changes currency mid-flight simply accrues its
//     remaining periods in the new currency; the old currency's
//     posted rows stay exactly where they were.
//   - Amending to the current currency (after normalization, so an
//     empty code on a USD plan is a no-op) is a no-op: the row is
//     untouched and no audit event fires, so a retried or
//     redundant revision does not pollute the audit trail with a
//     change that changed nothing.
//   - Like the other installment transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (installment_currency_amended, with the plan ID, the previous
//     and new currencies, and the posted-period count) so the "who
//     moved the settlement currency, and from what to what" trail is
//     tamper-evident. Installment plans have no Ctx variant and no
//     snapshot mechanism, so this amendment has neither: the currency
//     rides the in-memory plan row exactly like the original.
//
// Orthogonality: OpenInstallmentPlan owns the initial currency,
// AmendInstallmentPlanRule owns the billing rule,
// AmendInstallmentPlanFirstDueAt owns the first due date, and this
// module owns only the currency revision. It is orthogonal to the
// transfer-schedule currency amendment (LG-263), which moves a
// scheduled transfer's base currency — this moves an installment
// plan's settlement currency.
package ledger

import "fmt"

// AmendInstallmentPlanCurrency revises a live installment plan's
// settlement currency in place. Only active or overdue plans may be
// amended; unknown IDs fail with ErrInstallmentNotFound and completed
// or settled_early plans fail with ErrInstallmentBadTransition. The
// new currency reuses the creation gate (normalizeCurrency): an empty
// code normalizes to the default currency, and anything that is not
// a three-letter uppercase ISO 4217 code fails with
// ErrInvalidCurrency. Only Currency changes; the payer, payee,
// principal, period count, rule, first due date, legs, status, and
// posted periods are untouched, and already-posted periods are never
// rewritten — the new currency governs unposted periods from the next
// sweep. Amending to the current currency (after normalization) is a
// no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as installment_currency_amended.
func (l *Ledger) AmendInstallmentPlanCurrency(id string, currency string) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status != InstallmentActive && plan.Status != InstallmentOverdue {
		return InstallmentPlan{}, fmt.Errorf("%w: cannot amend a %s plan's currency", ErrInstallmentBadTransition, plan.Status)
	}
	normalized, err := normalizeCurrency(currency)
	if err != nil {
		return InstallmentPlan{}, err
	}
	if plan.Currency == normalized {
		return plan, nil
	}
	previous := plan.Currency
	plan.Currency = normalized
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_currency_amended",
		Actor:         "AmendInstallmentPlanCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"plan_id":           id,
			"previous_currency": previous,
			"currency":          normalized,
			"posted_periods":    plan.PostedPeriods,
		},
	})
	return plan, nil
}
