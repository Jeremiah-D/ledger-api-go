// Installment late fees: opt-in per-plan overdue interest/penalties on
// unpaid installment periods (LG-150).
//
// A分期 plan (see installment.go, LG-134) that misses a period's due
// date goes overdue — InstallmentsDue marks the status and a later
// sweep posts the late period honestly backdated. Money that stays
// overdue, however, is free money for the payer: this module lets an
// operator opt a plan into late-fee accrual with
// SetInstallmentLateFee, and AccrueInstallmentLateFees books one
// day's fee per overdue period on every sweep.
//
// Each fee row is an ordinary journal entry — debit the rule's
// FeeAccount, credit the plan's Payer — following the fee-leg
// direction the ledger uses everywhere (debit the recipient, credit
// the payer, see buildFeeLegEntries): the payer owes the fee on top
// of the missed period. The entry ID is deterministic,
// "installment-latefee/<planID>/<seq>/<YYYY-MM-DD>" (asof, UTC), so a
// sweep is idempotent per day: replaying the same day returns the
// same rows with Duplicate set, and a late sweep never double-books.
//
// Fees are booked through postSystemLocked (see accrual.go): the full
// Post gate order — field validation, idempotency replay, chart
// direction, frozen, overdraft, daily-limit, period — with posting
// rules and dual-control review off, exactly like the LG-55 system
// accruals. A frozen payer, an overdrafted payer, or a closed period
// therefore blocks the fee like it blocks any other booking; a failed
// fee is reported on the result and the sweep continues with the next
// period. Late-fee rules are a structural configuration (no version
// bump), like the other operator knobs in this package.
//
// Late fees are bookkeeping, not collections: the LG-118 dunning
// escalations (reminder, suspension threat, legal hold) are the
// operational counterpart and stay untouched.
//
// The per-period cap (MaxTotalCents) measures gross late fees booked
// for the period, net of any "<entry>/reversal" mirror rows, so a
// plan can never owe more than the agreed cap on one missed period.
// Reversals of late-fee rows are ordinary ReverseEntryPartial calls;
// they do not disable the rule.
package ledger

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// InstallmentLateFeeRule is the opt-in late-fee configuration for one
// installment plan (see SetInstallmentLateFee). Fees accrue per
// overdue period, per day: floor(outstanding * DailyRateBps / 10000 /
// 365) + FixedCentsPerDay, with the whole period amount as the base.
type InstallmentLateFeeRule struct {
	// FeeAccount receives the fee: the late-fee income account (or any
	// designated receivable). Debited; the plan's payer is credited.
	FeeAccount AccountID `json:"fee_account"`
	// DailyRateBps is the annual late-fee rate in basis points, applied
	// pro-rata per 365-day year on the outstanding period amount, like
	// AccrueInterest. 0 disables the rate component.
	DailyRateBps int64 `json:"daily_rate_bps,omitempty"`
	// FixedCentsPerDay is a flat per-day penalty in the plan's
	// currency cents, added on top of the rate component. 0 disables
	// it.
	FixedCentsPerDay int64 `json:"fixed_cents_per_day,omitempty"`
	// MaxTotalCents caps the total late fees bookable for one period.
	// 0 means uncapped. Measured net of reversals (see the package
	// doc).
	MaxTotalCents int64 `json:"max_total_cents,omitempty"`
}

// InstallmentLateFeeResult is one overdue period's outcome in an
// AccrueInstallmentLateFees sweep.
type InstallmentLateFeeResult struct {
	PlanID           string    `json:"plan_id"`
	Period           int       `json:"period"`
	DueAt            time.Time `json:"due_at"`
	OutstandingCents int64     `json:"outstanding_cents"`
	AmountCents      int64     `json:"amount_cents"`
	// EntryID is the deterministic journal row:
	// "installment-latefee/<planID>/<seq>/<YYYY-MM-DD>".
	EntryID string `json:"entry_id"`
	// Posted is true when the sweep booked (or replayed) the row.
	Posted bool `json:"posted"`
	// Duplicate is true when the row already existed: the sweep is
	// idempotent per day.
	Duplicate bool `json:"duplicate,omitempty"`
	// Capped is true when MaxTotalCents swallowed part or all of the
	// day's fee: AmountCents is what could still be booked (0 when
	// the cap was already exhausted).
	Capped bool `json:"capped,omitempty"`
}

// validateInstallmentLateFeeRule checks a late-fee rule before it is
// stored: the fee account must be a real account name (never a
// deterministic-ID fragment — '/' is reserved for the entry ID
// scheme), the rate must be a real basis-point value, and the rule
// must actually book something (a zero rule would sweep silently
// forever).
func validateInstallmentLateFeeRule(rule InstallmentLateFeeRule) error {
	if rule.FeeAccount == "" || strings.Contains(string(rule.FeeAccount), "/") {
		return fmt.Errorf("%w: fee account must be a plain account name, got %q", ErrInvalidInstallmentLateFeeRule, rule.FeeAccount)
	}
	if rule.DailyRateBps < 0 || rule.DailyRateBps > 10000 {
		return fmt.Errorf("%w: daily rate %d bps out of range [0,10000]", ErrInvalidInstallmentLateFeeRule, rule.DailyRateBps)
	}
	if rule.FixedCentsPerDay < 0 {
		return fmt.Errorf("%w: fixed daily penalty must be >= 0, got %d", ErrInvalidInstallmentLateFeeRule, rule.FixedCentsPerDay)
	}
	if rule.MaxTotalCents < 0 {
		return fmt.Errorf("%w: max total must be >= 0, got %d", ErrInvalidInstallmentLateFeeRule, rule.MaxTotalCents)
	}
	if rule.DailyRateBps == 0 && rule.FixedCentsPerDay == 0 {
		return fmt.Errorf("%w: a rule must charge something: set a daily rate or a fixed penalty", ErrInvalidInstallmentLateFeeRule)
	}
	return nil
}

// SetInstallmentLateFee opts one installment plan into late-fee
// accrual (see the package doc). The plan must exist
// (ErrInstallmentNotFound); the rule is validated (see
// validateInstallmentLateFeeRule). Installing a rule on a plan that
// already has one replaces it. Structural configuration: the version
// does not move.
func (l *Ledger) SetInstallmentLateFee(planID string, rule InstallmentLateFeeRule) error {
	if err := validateInstallmentLateFeeRule(rule); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[planID]
	if !ok {
		return ErrInstallmentNotFound
	}
	l.installmentLateFees[planID] = rule
	l.emitAudit(AuditEvent{
		Op:            "installment_late_fee_set",
		Actor:         "SetInstallmentLateFee",
		TraceID:       planID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, rule.FeeAccount},
		Details: map[string]any{
			"plan_id":            planID,
			"fee_account":        string(rule.FeeAccount),
			"daily_rate_bps":     rule.DailyRateBps,
			"fixed_cents_per_day": rule.FixedCentsPerDay,
			"max_total_cents":    rule.MaxTotalCents,
		},
	})
	return nil
}

// ClearInstallmentLateFee removes a plan's late-fee rule: future
// AccrueInstallmentLateFees sweeps skip the plan, and already-booked
// fee rows are untouched (they are real history). The plan must exist;
// clearing a plan without a rule is a no-op.
func (l *Ledger) ClearInstallmentLateFee(planID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.installments[planID]; !ok {
		return ErrInstallmentNotFound
	}
	if _, ok := l.installmentLateFees[planID]; !ok {
		return nil
	}
	delete(l.installmentLateFees, planID)
	l.emitAudit(AuditEvent{
		Op:            "installment_late_fee_cleared",
		Actor:         "ClearInstallmentLateFee",
		TraceID:       planID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"plan_id": planID},
	})
	return nil
}

// InstallmentLateFee reports the late-fee rule on a plan, if any.
func (l *Ledger) InstallmentLateFee(planID string) (InstallmentLateFeeRule, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rule, ok := l.installmentLateFees[planID]
	return rule, ok
}

// installmentLateFeeEntryID is the deterministic journal row ID for
// one period's late fee on one accrual day (asof, UTC): replaying the
// same sweep replays the same rows.
func installmentLateFeeEntryID(planID string, seq int, asof time.Time) string {
	return fmt.Sprintf("installment-latefee/%s/%d/%s", planID, seq, asof.UTC().Format("2006-01-02"))
}

// lateFeeTotalLocked measures what an overdue period has already been
// charged, net of any reversed fee rows: the sum of journal rows with
// the period's late-fee prefix, minus their "<id>/reversal" mirrors.
// It mirrors accruedTotalLocked's netting (see accrual.go). Callers
// must hold l.mu.
func (l *Ledger) lateFeeTotalLocked(planID string, seq int) (int64, error) {
	prefix := fmt.Sprintf("installment-latefee/%s/%d/", planID, seq)
	var total int64
	for id, e := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		rest := strings.TrimPrefix(id, prefix)
		var ok bool
		if strings.Contains(rest, "/reversal/") {
			// A reversal nets its fee out of the total.
			total, ok = addCents(total, -e.AmountCents)
		} else {
			total, ok = addCents(total, e.AmountCents)
		}
		if !ok {
			return 0, fmt.Errorf("ledger: late-fee total overflows int64")
		}
	}
	return total, nil
}

// AccrueInstallmentLateFees books one day's late fee (asof) for every
// overdue period of every plan with a late-fee rule: periods that are
// due (due <= asof) and not yet posted, on non-terminal plans. See the
// package doc for the entry legs, the deterministic ID, and the gate
// behavior. A failed period is reported and the sweep continues — the
// first error is returned alongside the results, exactly like
// InstallmentsDue.
//
// Plans without a rule, periods not yet due, and terminal plans are
// skipped silently; periods whose computed fee is 0 (rate dust) are
// skipped too. A zero asof means now.
func (l *Ledger) AccrueInstallmentLateFees(asof time.Time) ([]InstallmentLateFeeResult, error) {
	if asof.IsZero() {
		asof = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	ids := make([]string, 0, len(l.installments))
	for id, plan := range l.installments {
		if plan.Status == InstallmentActive || plan.Status == InstallmentOverdue {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var out []InstallmentLateFeeResult
	var firstErr error
	for _, id := range ids {
		rule, ok := l.installmentLateFees[id]
		if !ok {
			continue
		}
		plan := l.installments[id]
		for seq := plan.PostedPeriods + 1; seq <= plan.NumPeriods; seq++ {
			due := installmentDueAt(plan, seq)
			if due.After(asof) {
				break
			}
			outstanding := installmentPeriodAmount(plan, seq)
			daily := policyFeeCents(outstanding, rule.DailyRateBps)/365 + rule.FixedCentsPerDay
			if daily <= 0 {
				// Rate dust on a tiny outstanding amount: books
				// nothing, and is not a failure.
				continue
			}
			entryID := installmentLateFeeEntryID(plan.ID, seq, asof)
			fee := daily
			capped := false
			if rule.MaxTotalCents > 0 {
				total, err := l.lateFeeTotalLocked(plan.ID, seq)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				remaining := rule.MaxTotalCents - total
				if remaining <= 0 {
					// The cap is already exhausted: record the
					// outcome so the operator can see it.
					out = append(out, InstallmentLateFeeResult{
						PlanID: plan.ID, Period: seq, DueAt: due,
						OutstandingCents: outstanding, EntryID: entryID,
						Capped: true,
					})
					continue
				}
				if fee > remaining {
					fee, capped = remaining, true
				}
			}
			e := JournalEntry{
				ID:             entryID,
				DebitAccount:   rule.FeeAccount,
				CreditAccount:  plan.Payer,
				AmountCents:    fee,
				Currency:       plan.Currency,
				IdempotencyKey: entryID,
				Memo:           fmt.Sprintf("installment %s period %d late fee", plan.ID, seq),
				CreatedAt:      asof,
			}
			_, dup, err := l.postSystemLocked(e, false)
			if err != nil {
				// A failed fee (frozen payer, overdraft,
				// closed period) is reported, not fatal: the
				// next period may still accrue.
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			out = append(out, InstallmentLateFeeResult{
				PlanID: plan.ID, Period: seq, DueAt: due,
				OutstandingCents: outstanding, AmountCents: fee,
				EntryID: entryID, Posted: true, Duplicate: dup,
				Capped: capped,
			})
		}
	}
	return out, firstErr
}
