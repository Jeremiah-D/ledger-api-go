package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Installment plans: scheduled receivables collected in equal periods
// (LG-134, fintech).
//
// An installment plan splits a principal into NumPeriods equal
// collections from a payer to a payee on a fixed billing rule (monthly,
// weekly, or daily). It models the BNPL /分期付款 pattern: the merchant
// (payee) is owed the principal, and the buyer's account (payer) is
// debited once per period. InstallmentsDue is the sweeper: it books
// every period whose due date has arrived, through the existing atomic
// Post path, so each period's journal row clears the full gate order
// (field validation, idempotency replay, chart direction, frozen,
// overdraft, daily-limit, period) exactly like a hand-posted payment.
//
// Period economics: every period collects floor(principal/NumPeriods)
// cents; the final period absorbs the division remainder, so the
// periods always sum to exactly the principal — no dust is created or
// lost. The collection is a customer payment, so it follows the
// transfer convention: the payee is debited (their balance grows) and
// the payer is credited. Period rows are journaled as of their due
// date, not the sweep time: the obligation existed at the due date, so
// a late sweep backdates honestly rather than pretending the money
// moved today.
//
// Overdue state machine: active -> overdue when a due period's posting
// is rejected (insufficient funds, frozen account, ...). The rejection
// is not retried inside the same sweep — later periods wait, because
// collecting period 3 while period 2 failed would skip the queue. A
// later sweep that posts every outstanding period recovers the plan to
// active. Early settlement closes the plan in one row for the remaining
// balance; completing the final period normally closes it as
// completed. Every transition is audited.
//
// Idempotency: period rows carry the deterministic entry ID and
// idempotency key "installment/<planID>/<seq>", and the early-settle
// row "installment/<planID>/early-settle". Re-running a sweep replays
// the original rows instead of double-booking. Plan opens carry their
// own key namespace (installmentKeys), independent of Post's byKey and
// every other operation's namespace, expiring with the ledger's
// idempotency TTL like the others. In-memory only: unlike term
// deposits, plans do not round-trip through disaster-recovery
// snapshots — the journaled period rows do, so the money is safe; only
// the plan metadata is lost. Re-opening a plan after a restore replays
// each period's deterministic key against the shared idempotency
// index and returns the originally posted rows (see the netting
// precedent in netting.go).

// Installment billing rules: the cadence of period due dates,
// anchored at FirstDueAt. Monthly adds calendar months; weekly and
// daily add 7*i / i calendar days.
const (
	InstallmentMonthly = "monthly"
	InstallmentWeekly  = "weekly"
	InstallmentDaily   = "daily"
)

// Installment plan statuses. Only active and overdue plans accrue new
// due periods; the other two are terminal.
const (
	InstallmentActive       = "active"
	InstallmentOverdue      = "overdue"
	InstallmentSettledEarly = "settled_early"
	InstallmentCompleted    = "completed"
)

// Validation and state errors returned by the installment APIs.
var (
	ErrEmptyInstallmentID            = errors.New("ledger: installment plan ID must not be empty")
	ErrInstallmentIDTaken            = errors.New("ledger: installment plan ID already in use")
	ErrEmptyInstallmentPayer         = errors.New("ledger: installment payer must not be empty")
	ErrEmptyInstallmentPayee         = errors.New("ledger: installment payee must not be empty")
	ErrInstallmentSameAccount        = errors.New("ledger: installment payer and payee must differ")
	ErrInvalidInstallmentAmount      = errors.New("ledger: installment principal must be positive")
	ErrInvalidInstallmentPeriods     = errors.New("ledger: installment periods must be at least 1")
	ErrInvalidInstallmentRule        = errors.New("ledger: installment rule must be monthly, weekly, or daily")
	ErrEmptyInstallmentDueDate       = errors.New("ledger: installment first due date must not be zero")
	ErrInvalidInstallmentFirstDueAt  = errors.New("ledger: installment first due date must not be zero")
	ErrInstallmentNotFound           = errors.New("ledger: installment plan not found")
	ErrInstallmentTerminal           = errors.New("ledger: installment plan is terminal")
	ErrInstallmentBadTransition      = errors.New("ledger: installment plan status transition not allowed")
	ErrInvalidInstallmentLateFeeRule = errors.New("ledger: installment late-fee rule invalid")
)

// maxInstallmentIDLen caps plan IDs. Deterministic period entry IDs
// embed the plan ID ("installment/<id>/<seq>"), so a bounded plan ID
// keeps entry IDs bounded too.
const maxInstallmentIDLen = 64

// InstallmentPlan is one scheduled receivable. PostedPeriods counts the
// periods already journaled; the next collection is period
// PostedPeriods+1 (1-based in entry IDs and results).
type InstallmentPlan struct {
	ID             string    `json:"id"`
	Payer          AccountID `json:"payer"`
	Payee          AccountID `json:"payee"`
	PrincipalCents int64     `json:"principal_cents"`
	NumPeriods     int       `json:"num_periods"`
	Rule           string    `json:"rule"`
	FirstDueAt     time.Time `json:"first_due_at"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	PostedPeriods  int       `json:"posted_periods"`
	CreatedAt      time.Time `json:"created_at"`
}

// OpenInstallmentPlanInput opens one installment plan.
type OpenInstallmentPlanInput struct {
	// ID is the caller-chosen plan ID, unique across plans.
	ID string
	// Payer is debited... precisely: the payer's balance is credited
	// each period (they pay); the payee is debited (they receive).
	Payer          AccountID
	Payee          AccountID
	PrincipalCents int64
	NumPeriods     int
	// Rule is InstallmentMonthly, InstallmentWeekly, or
	// InstallmentDaily.
	Rule string
	// FirstDueAt anchors period 1; period i is due
	// FirstDueAt + (i-1) rule steps.
	FirstDueAt time.Time
	// Currency is the ISO 4217 code; empty means the default.
	Currency string
	// IdempotencyKey makes the open idempotent in its own namespace.
	IdempotencyKey string
}

// installmentPeriodAmount returns the collection for the 1-based
// period seq: floor(principal/NumPeriods), except the final period,
// which absorbs the remainder so all periods sum to the principal.
func installmentPeriodAmount(plan InstallmentPlan, seq int) int64 {
	base := plan.PrincipalCents / int64(plan.NumPeriods)
	if seq == plan.NumPeriods {
		return plan.PrincipalCents - base*int64(plan.NumPeriods-1)
	}
	return base
}

// installmentDueAt returns the due date of the 1-based period seq.
func installmentDueAt(plan InstallmentPlan, seq int) time.Time {
	switch plan.Rule {
	case InstallmentWeekly:
		return plan.FirstDueAt.AddDate(0, 0, 7*(seq-1))
	case InstallmentDaily:
		return plan.FirstDueAt.AddDate(0, 0, seq-1)
	default: // InstallmentMonthly
		return plan.FirstDueAt.AddDate(0, seq-1, 0)
	}
}

// installmentEntryID derives the deterministic journal entry ID (and
// idempotency key) for one period collection.
func installmentEntryID(planID string, seq int) string {
	return fmt.Sprintf("installment/%s/%d", planID, seq)
}

// installmentEarlySettleID derives the deterministic journal entry ID
// (and idempotency key) for the early-settlement row.
func installmentEarlySettleID(planID string) string {
	return fmt.Sprintf("installment/%s/early-settle", planID)
}

// installmentPostedSum returns the total already collected across the
// journaled periods, computed deterministically from the plan terms —
// the journal rows are the money, this is the arithmetic.
func installmentPostedSum(plan InstallmentPlan) int64 {
	var sum int64
	for seq := 1; seq <= plan.PostedPeriods; seq++ {
		sum += installmentPeriodAmount(plan, seq)
	}
	return sum
}

// OpenInstallmentPlan validates and stores one installment plan. The
// plan opens in the active state with zero periods posted.
//
// Opening with a replayed idempotency key returns the original plan.
// Opening under a taken plan ID is rejected with
// ErrInstallmentIDTaken, even when the key is fresh — IDs are the
// identity, keys are the replay shortcut. A zero CreatedAt is filled
// with the open time. The transition is audited.
func (l *Ledger) OpenInstallmentPlan(in OpenInstallmentPlanInput) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if in.IdempotencyKey != "" {
		if rec, ok := l.installmentKeys[in.IdempotencyKey]; ok {
			if plan, ok := l.installments[rec.planID]; ok {
				return plan, nil
			}
		}
	}
	if in.ID == "" || len(in.ID) > maxInstallmentIDLen {
		return InstallmentPlan{}, ErrEmptyInstallmentID
	}
	if _, taken := l.installments[in.ID]; taken {
		return InstallmentPlan{}, ErrInstallmentIDTaken
	}
	if in.Payer == "" {
		return InstallmentPlan{}, ErrEmptyInstallmentPayer
	}
	if in.Payee == "" {
		return InstallmentPlan{}, ErrEmptyInstallmentPayee
	}
	if in.Payer == in.Payee {
		return InstallmentPlan{}, ErrInstallmentSameAccount
	}
	if in.PrincipalCents <= 0 {
		return InstallmentPlan{}, ErrInvalidInstallmentAmount
	}
	if in.NumPeriods < 1 {
		return InstallmentPlan{}, ErrInvalidInstallmentPeriods
	}
	switch in.Rule {
	case InstallmentMonthly, InstallmentWeekly, InstallmentDaily:
	default:
		return InstallmentPlan{}, ErrInvalidInstallmentRule
	}
	if in.FirstDueAt.IsZero() {
		return InstallmentPlan{}, ErrEmptyInstallmentDueDate
	}
	currency, err := normalizeCurrency(in.Currency)
	if err != nil {
		return InstallmentPlan{}, err
	}

	now := time.Now()
	plan := InstallmentPlan{
		ID:             in.ID,
		Payer:          in.Payer,
		Payee:          in.Payee,
		PrincipalCents: in.PrincipalCents,
		NumPeriods:     in.NumPeriods,
		Rule:           in.Rule,
		FirstDueAt:     in.FirstDueAt,
		Currency:       currency,
		Status:         InstallmentActive,
		CreatedAt:      now,
	}
	l.installments[plan.ID] = plan
	if in.IdempotencyKey != "" {
		l.installmentKeys[in.IdempotencyKey] = installmentKeyRecord{planID: plan.ID, createdAt: now}
	}
	l.emitAudit(AuditEvent{
		Op:            "installment_plan_open",
		Actor:         "OpenInstallmentPlan",
		TraceID:       plan.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"principal_cents": plan.PrincipalCents,
			"num_periods":     plan.NumPeriods,
			"rule":            plan.Rule,
			"currency":        plan.Currency,
		},
	})
	return plan, nil
}

// InstallmentPlan returns one plan by ID.
func (l *Ledger) InstallmentPlan(id string) (InstallmentPlan, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	plan, ok := l.installments[id]
	return plan, ok
}

// InstallmentPlans lists every plan, sorted by ID.
func (l *Ledger) InstallmentPlans() []InstallmentPlan {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]InstallmentPlan, 0, len(l.installments))
	for _, plan := range l.installments {
		out = append(out, plan)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// InstallmentPeriodResult is one period's sweep outcome.
type InstallmentPeriodResult struct {
	PlanID      string    `json:"plan_id"`
	Period      int       `json:"period"`
	DueAt       time.Time `json:"due_at"`
	AmountCents int64     `json:"amount_cents"`
	EntryID     string    `json:"entry_id"`
	// Posted is true when the period's row is journaled after this
	// sweep (freshly posted or replayed). False means the posting was
	// rejected and the plan went overdue.
	Posted bool `json:"posted"`
	// Duplicate is true when the period's row was already journaled
	// and this sweep replayed it.
	Duplicate bool `json:"duplicate"`
}

// InstallmentsDue sweeps every non-terminal plan and books each period
// whose due date has arrived (due <= now), in period order, through
// the atomic Post path.
//
// Each period posts exactly one journal row with the deterministic ID
// "installment/<planID>/<seq>": a re-run replays the original row, so
// the sweeper can run blindly on a schedule without double-booking. A
// period whose posting is rejected stops its plan's sweep — the plan
// transitions to overdue and is audited — while other plans continue.
// The returned error is the first rejection encountered; the results
// slice still describes everything the sweep attempted.
//
// A plan that posts all of its periods transitions to completed. An
// overdue plan that catches up on every outstanding period recovers to
// active. Both transitions are audited.
func (l *Ledger) InstallmentsDue(now time.Time) ([]InstallmentPeriodResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	ids := make([]string, 0, len(l.installments))
	for id, plan := range l.installments {
		if plan.Status == InstallmentActive || plan.Status == InstallmentOverdue {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var results []InstallmentPeriodResult
	var firstErr error
	for _, id := range ids {
		plan := l.installments[id]
		wasOverdue := plan.Status == InstallmentOverdue
		postedAny := false
		failed := false
		for seq := plan.PostedPeriods + 1; seq <= plan.NumPeriods; seq++ {
			due := installmentDueAt(plan, seq)
			if due.After(now) {
				break
			}
			amount := installmentPeriodAmount(plan, seq)
			entryID := installmentEntryID(plan.ID, seq)
			// The payee receives: debit the payee, credit the payer,
			// per the transfer convention (see PostTransfer). The row
			// is journaled as of its due date.
			posted, dup, err := l.postLocked(JournalEntry{
				ID:             entryID,
				DebitAccount:   plan.Payee,
				CreditAccount:  plan.Payer,
				AmountCents:    amount,
				Currency:       plan.Currency,
				IdempotencyKey: entryID,
				Memo:           fmt.Sprintf("installment %s period %d/%d", plan.ID, seq, plan.NumPeriods),
				CreatedAt:      due,
			}, true)
			_ = posted
			res := InstallmentPeriodResult{
				PlanID:      plan.ID,
				Period:      seq,
				DueAt:       due,
				AmountCents: amount,
				EntryID:     entryID,
			}
			if err != nil {
				plan.Status = InstallmentOverdue
				failed = true
				l.installments[id] = plan
				l.emitAudit(AuditEvent{
					Op:            "installment_overdue",
					Actor:         "InstallmentsDue",
					TraceID:       plan.ID,
					VersionBefore: l.version,
					VersionAfter:  l.version,
					Accounts:      []AccountID{plan.Payer, plan.Payee},
					Details: map[string]any{
						"period":       seq,
						"due_at":       due.UTC().Format(time.RFC3339),
						"amount_cents": amount,
						"error":        err.Error(),
					},
				})
				results = append(results, res)
				if firstErr == nil {
					firstErr = fmt.Errorf("ledger: installment %s period %d: %w", plan.ID, seq, err)
				}
				break
			}
			plan.PostedPeriods = seq
			postedAny = true
			res.Posted = true
			res.Duplicate = dup
			results = append(results, res)
		}
		// Terminal and recovery transitions, audited. Recovery fires
		// only for a plan that entered the sweep overdue, posted at
		// least one period, and did not fail again: the same sweep
		// that marks a plan overdue (or fails it again) must not
		// recover it. Both can fire in one sweep: an overdue plan
		// that posts its final period recovers and completes.
		if wasOverdue && postedAny && !failed {
			plan.Status = InstallmentActive
			l.installments[id] = plan
			l.emitAudit(AuditEvent{
				Op:            "installment_recovered",
				Actor:         "InstallmentsDue",
				TraceID:       plan.ID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{plan.Payer, plan.Payee},
				Details: map[string]any{
					"posted_periods": plan.PostedPeriods,
				},
			})
		}
		if plan.PostedPeriods == plan.NumPeriods && plan.Status != InstallmentCompleted {
			plan.Status = InstallmentCompleted
			l.installments[id] = plan
			l.emitAudit(AuditEvent{
				Op:            "installment_completed",
				Actor:         "InstallmentsDue",
				TraceID:       plan.ID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{plan.Payer, plan.Payee},
				Details: map[string]any{
					"principal_cents": plan.PrincipalCents,
					"num_periods":     plan.NumPeriods,
				},
			})
		} else {
			l.installments[id] = plan
		}
	}
	return results, firstErr
}

// SettleInstallmentEarly closes a non-terminal plan in one row for the
// remaining principal: principal minus the periods already journaled.
// The row carries the deterministic ID
// "installment/<planID>/early-settle", so re-running it replays the
// original instead of double-booking. The plan transitions to
// settled_early and the transition is audited.
//
// Re-running on an already terminal plan returns it unchanged.
func (l *Ledger) SettleInstallmentEarly(id string, now time.Time) (InstallmentPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	plan, ok := l.installments[id]
	if !ok {
		return InstallmentPlan{}, ErrInstallmentNotFound
	}
	if plan.Status == InstallmentSettledEarly || plan.Status == InstallmentCompleted {
		return plan, nil
	}
	remaining := plan.PrincipalCents - installmentPostedSum(plan)
	if remaining > 0 {
		entryID := installmentEarlySettleID(plan.ID)
		if _, _, err := l.postLocked(JournalEntry{
			ID:             entryID,
			DebitAccount:   plan.Payee,
			CreditAccount:  plan.Payer,
			AmountCents:    remaining,
			Currency:       plan.Currency,
			IdempotencyKey: entryID,
			Memo:           fmt.Sprintf("installment %s early settlement", plan.ID),
			CreatedAt:      now,
		}, true); err != nil {
			return InstallmentPlan{}, err
		}
	}
	plan.Status = InstallmentSettledEarly
	plan.PostedPeriods = plan.NumPeriods
	l.installments[id] = plan
	l.emitAudit(AuditEvent{
		Op:            "installment_settled_early",
		Actor:         "SettleInstallmentEarly",
		TraceID:       plan.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{plan.Payer, plan.Payee},
		Details: map[string]any{
			"remaining_cents": remaining,
			"posted_periods":  plan.NumPeriods,
		},
	})
	return plan, nil
}

// installmentKeyRecord remembers a plan open's idempotency key so
// replays rebuild the original plan.
type installmentKeyRecord struct {
	planID    string
	createdAt time.Time
}

// pruneInstallmentKeysLocked evicts plan-open idempotency keys older
// than the ledger's idempotency TTL. Callers must hold the write lock.
func (l *Ledger) pruneInstallmentKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.installmentKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.installmentKeys, key)
			removed++
		}
	}
	return removed
}
