package ledger

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrBudgetExceeded is returned by Post, PostTransfer, PostBatch,
// PostSweep, and hold Capture when the posting would take the account's
// cumulative spend for the UTC calendar month above its configured
// monthly spend budget (see SetAccountBudget).
var ErrBudgetExceeded = errors.New("ledger: posting would exceed the account's monthly spend budget")

// ErrInvalidBudget is returned by SetAccountBudget when the budget is
// negative. A zero budget is legal: it stops all spend (a risk-control
// freeze-out); clearing a budget uses ClearAccountBudget.
var ErrInvalidBudget = errors.New("ledger: monthly spend budget must not be negative")

// budgetKey identifies one configured budget: an account's spend in one
// currency over one UTC calendar month.
type budgetKey struct {
	account  AccountID
	currency string
}

// monthlySpendKey identifies one accumulation bucket: an account's spend
// in one currency in one UTC calendar month.
type monthlySpendKey struct {
	account  AccountID
	currency string
	month    string // UTC month as "2006-01"
}

// AccountBudget is one configured monthly spend budget, used by the
// reconciliation report and the disaster-recovery snapshot.
type AccountBudget struct {
	Account     AccountID `json:"account"`
	Currency    string    `json:"currency"`
	BudgetCents int64     `json:"budget_cents"`
}

// AccountBudgetUsage is one configured monthly spend budget's current
// utilization at report time.
type AccountBudgetUsage struct {
	Account AccountID `json:"account"`
	Currency string   `json:"currency"`
	// Month is the UTC calendar month the usage figures describe, as
	// "2006-01".
	Month string `json:"month"`
	BudgetCents int64 `json:"budget_cents"`
	// UsedCents is the account's booked spend for the month, in cents.
	// It includes spend reserved by pending dual-control reviews (see
	// LG-42): a review reserves its transfer's total outflow when it is
	// created, so the budget reflects committed future spend, not just
	// settled entries.
	UsedCents int64 `json:"used_cents"`
	// Utilization is UsedCents / BudgetCents as a fraction. It is 0
	// when the budget is 0: a zero budget admits no spend, so used is
	// necessarily 0 and the ratio would be 0/0.
	Utilization float64 `json:"utilization"`
	// Exceeded is true when the booked spend is above the budget. Budget
	// changes that would create this state are rejected (see
	// SetAccountBudget), so true here is a canary — always worth an
	// operator's look.
	Exceeded bool `json:"exceeded"`
}

// utcMonth returns the UTC calendar month of t as "2006-01". ISO months
// sort lexicographically in chronological order, which the bucket pruning
// relies on.
func utcMonth(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// SetAccountBudget configures (or replaces) the monthly spend budget for
// one account in one currency: the sum of that account's spend in a UTC
// calendar month may not exceed budgetCents. "Spend" is the payer side —
// the same legs the daily outflow limit (LG-26) guards: for Post it is
// the credit account, for PostTransfer the payer's total outflow (amount
// plus any fee leg), for PostBatch every entry's credit leg, for
// PostSweep every source leg, and for hold Capture the hold account's
// settlement leg. A budget of 0 stops all spend; negative budgets are
// rejected with ErrInvalidBudget.
//
// The daily outflow limit (LG-26) is a rolling spend window: it guards
// how fast money leaves within a day. The monthly budget is an
// accounting-period control: it guards how much the account may spend in
// the calendar month. The two are orthogonal — a posting must clear both
// — and they accumulate independently (per UTC day vs per UTC month).
//
// Lowering a budget must not put the account in immediate violation: the
// account's already-booked spend for the current UTC month must fit under
// the new budget. A change that would breach it is rejected with
// ErrBudgetExceeded (wrapped with the figures), mirroring SetDailyLimit,
// so the operator learns the current usage instead of installing a budget
// that rejects every subsequent posting.
//
// Setting or clearing a budget is structural, like Freeze or SetParent:
// it does not bump the ledger version (no balances change). It is
// visible to audit tooling — the change lands in the structured audit
// log's hash chain (ops "budget_set" / "budget_cleared") — the trial
// balance carries the default-currency budget, and Reconcile reports
// every configured budget's utilization.
func (l *Ledger) SetAccountBudget(a AccountID, currency string, budgetCents int64) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if budgetCents < 0 {
		return ErrInvalidBudget
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	month := utcMonth(time.Now())
	used := l.monthlySpend[monthlySpendKey{account: a, currency: currency, month: month}]
	if budgetCents < used {
		return fmt.Errorf("%w: %q already spent %d cents of %s in %s, above the new budget %d",
			ErrBudgetExceeded, a, used, currency, month, budgetCents)
	}
	key := budgetKey{account: a, currency: currency}
	if old, ok := l.budgets[key]; ok && old == budgetCents {
		return nil // no change: nothing to record
	}
	l.budgets[key] = budgetCents
	l.emitAudit(AuditEvent{
		Op:            "budget_set",
		Actor:         "SetAccountBudget",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"currency":     currency,
			"budget_cents": budgetCents,
			"month":        month,
		},
	})
	return nil
}

// ClearAccountBudget removes the monthly spend budget for one account in
// one currency. Clearing a budget that was never set is a no-op. Like
// SetAccountBudget, it is structural and does not bump the ledger
// version; an actual removal lands in the audit log.
func (l *Ledger) ClearAccountBudget(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := budgetKey{account: a, currency: currency}
	if _, ok := l.budgets[key]; !ok {
		return
	}
	delete(l.budgets, key)
	l.emitAudit(AuditEvent{
		Op:            "budget_cleared",
		Actor:         "ClearAccountBudget",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details:       map[string]any{"currency": currency},
	})
}

// AccountBudget reports the configured monthly spend budget for one
// account in one currency, and whether a budget is configured at all.
func (l *Ledger) AccountBudget(a AccountID, currency string) (int64, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	budget, ok := l.budgets[budgetKey{account: a, currency: currency}]
	return budget, ok
}

// MonthlySpend reports the account's booked spend in currency for the
// given UTC month ("2006-01"), including spend reserved by pending
// dual-control reviews. Unknown (account, currency, month) triples
// report zero.
func (l *Ledger) MonthlySpend(a AccountID, currency, month string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.monthlySpend[monthlySpendKey{account: a, currency: currency, month: month}]
}

// WithAccountBudget configures a monthly spend budget at construction
// time (see SetAccountBudget). Invalid currency codes and negative
// budgets panic — fail-fast at construction, like the fee schedule — so
// a misconfigured budget can never silently under-enforce.
func WithAccountBudget(a AccountID, currency string, budgetCents int64) Option {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid monthly spend budget currency %q: %v", currency, err))
	}
	if budgetCents < 0 {
		panic("ledger: monthly spend budget must not be negative")
	}
	return func(l *Ledger) {
		l.budgets[budgetKey{account: a, currency: currency}] = budgetCents
	}
}

// ParseAccountBudgets parses the LEDGER_MONTHLY_SPEND_BUDGETS environment
// variable into monthly spend budget configs:
//
//	"<account>:<currency>:<budgetCents>[,<account>:<currency>:<budgetCents>...]"
//
// e.g. "cust-123:USD:500000,cust-456:EUR:200000" caps cust-123's monthly
// USD spend at $5000.00 and cust-456's monthly EUR spend at €2000.00.
// Budgets are integer cents and must not be negative; currencies follow
// the usual ISO 4217 validation. Parsing is strict — a malformed segment
// is an error, so a misconfigured deployment fails fast at startup
// instead of silently running without its risk control.
func ParseAccountBudgets(raw string) ([]AccountBudget, error) {
	fail := func(format string, args ...any) ([]AccountBudget, error) {
		return nil, fmt.Errorf("ledger: invalid monthly spend budgets %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []AccountBudget
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 3 {
			return fail("segment %q must be \"<account>:<currency>:<budgetCents>\"", seg)
		}
		account := strings.TrimSpace(parts[0])
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[1]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		budget, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil || budget < 0 {
			return fail("segment %q: budget %q must be a non-negative integer (cents)", seg, parts[2])
		}
		out = append(out, AccountBudget{Account: AccountID(account), Currency: currency, BudgetCents: budget})
	}
	return out, nil
}

// budgetRejectedLocked reports whether posting amount cents of spend for
// account in currency at time at would exceed the account's monthly spend
// budget. The accumulated spend is keyed by the posting's own UTC
// calendar month, so a backdated entry counts toward its own month —
// deterministic for tests and honest for backfills. Accounts without a
// configured budget are never rejected.
//
// The comparison never adds total + amount, so it cannot overflow even
// when amount is huge: amount > budget rejects immediately (a
// non-negative total can never stay within the budget then), otherwise
// total > budget - amount is equivalent and both sides are non-negative.
// Callers must hold l.mu.
func (l *Ledger) budgetRejectedLocked(account AccountID, currency string, amount int64, at time.Time) bool {
	return l.budgetViolatedLocked(account, currency, amount, at, nil)
}

// budgetViolatedLocked is budgetRejectedLocked with planned-but-
// uncommitted spend carried in extra per (account, currency, month), for
// batch/sweep simulations; nil for direct postings. The running total
// saturates like addMonthlySpendLocked. Callers must hold l.mu.
func (l *Ledger) budgetViolatedLocked(account AccountID, currency string, amount int64, at time.Time, extra map[monthlySpendKey]int64) bool {
	if len(l.budgets) == 0 {
		return false
	}
	budget, ok := l.budgets[budgetKey{account: account, currency: currency}]
	if !ok {
		return false
	}
	if amount > budget {
		return true
	}
	month := utcMonth(at)
	used := l.monthlySpend[monthlySpendKey{account: account, currency: currency, month: month}]
	if extra != nil {
		for k, v := range extra {
			if k.account == account && k.currency == currency && k.month == month {
				used = satAddInt64(used, v)
			}
		}
	}
	return used > budget-amount
}

// addMonthlySpendLocked records amount cents of spend for account in
// currency on the posting's UTC calendar month. Callers must hold the
// write lock and must have run budgetRejectedLocked first; the add runs
// in the same critical section as the commit, so the check-then-add is
// atomic. The total saturates at MaxInt64 on overflow — the budget check
// above already rejected any posting that would have fit under a
// configured budget, so saturation only matters for unbudgeted accounts
// with absurd cumulative volume.
//
// Idempotent replays never reach this add: every gated path returns the
// replay before the budget check, so a replay consumes no budget.
// Pending dual-control reviews (LG-42) reserve their transfer's total
// outflow at creation — the review freeze is atomic, covering funds and
// budget together — so the reserved spend counts in the month's used
// figure from creation, and approval settles against the already-
// reserved budget without consuming it twice.
func (l *Ledger) addMonthlySpendLocked(account AccountID, currency string, amount int64, at time.Time) {
	key := monthlySpendKey{account: account, currency: currency, month: utcMonth(at)}
	total := l.monthlySpend[key]
	if total > math.MaxInt64-amount {
		l.monthlySpend[key] = math.MaxInt64
		return
	}
	l.monthlySpend[key] = total + amount
}

// maybePruneMonthlySpendLocked drops spend buckets for UTC months older
// than last month: a new posting can only reference its own month's
// bucket (backdated or current), so anything older is dead weight.
// Pruning runs at most once per pruneInterval — amortized O(1) per
// posting, like the idempotency-key sweep. Callers must hold l.mu.
func (l *Ledger) maybePruneMonthlySpendLocked(now time.Time) {
	if now.Sub(l.lastBudgetPrune) < l.pruneInterval {
		return
	}
	l.lastBudgetPrune = now
	cutoff := now.UTC().AddDate(0, -1, 0).Format("2006-01")
	for k := range l.monthlySpend {
		if k.month < cutoff {
			delete(l.monthlySpend, k)
		}
	}
}

// budgetsLocked returns every configured monthly spend budget, sorted by
// (account, currency), for the reconciliation report and the snapshot.
// Callers must hold l.mu.
func (l *Ledger) budgetsLocked() []AccountBudget {
	out := make([]AccountBudget, 0, len(l.budgets))
	for k, budget := range l.budgets {
		out = append(out, AccountBudget{Account: k.account, Currency: k.currency, BudgetCents: budget})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// budgetUtilizationLocked builds the reconcile report's budget_utilization
// rows: every configured budget with the current UTC month's booked spend
// (including pending-review reservations) and its utilization fraction.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) budgetUtilizationLocked(now time.Time) []AccountBudgetUsage {
	month := utcMonth(now.UTC())
	out := make([]AccountBudgetUsage, 0, len(l.budgets))
	for k, budget := range l.budgets {
		used := l.monthlySpend[monthlySpendKey{account: k.account, currency: k.currency, month: month}]
		var utilization float64
		if budget > 0 {
			utilization = float64(used) / float64(budget)
		}
		out = append(out, AccountBudgetUsage{
			Account:     k.account,
			Currency:    k.currency,
			Month:       month,
			BudgetCents: budget,
			UsedCents:   used,
			Utilization: utilization,
			Exceeded:    used > budget,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// budgetImpactLocked diffs the live ledger's monthly-spend buckets
// against a dry-run twin's (see cloneForDryRunLocked): every budgeted
// (account, currency, month) whose spend the simulated posting changed
// becomes one preview row. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) budgetImpactLocked(twin *Ledger) []BudgetImpact {
	var out []BudgetImpact
	for k, after := range twin.monthlySpend {
		before := l.monthlySpend[k]
		if after == before {
			continue
		}
		budget, ok := l.budgets[budgetKey{account: k.account, currency: k.currency}]
		if !ok {
			continue
		}
		out = append(out, BudgetImpact{
			Account:         k.account,
			Currency:        k.currency,
			Month:           k.month,
			BudgetCents:     budget,
			UsedBeforeCents: before,
			UsedAfterCents:  after,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Month < out[j].Month
	})
	return out
}
