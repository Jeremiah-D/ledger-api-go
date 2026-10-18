package ledger

import (
	"errors"
	"math"
	"sort"
	"time"
)

// Budget pace prediction (LG-116).
//
// LG-81's budget utilization is a snapshot: AccountBudgetUsage reports
// how much of the month's budget is already spent, but says nothing
// about where the run-rate lands the account by month end. An operator
// watching spend pressure wants the trajectory: at the current pace,
// does the month close inside the budget or blow past it? BudgetPace
// answers that by linearly extrapolating the booked month-to-date spend
// over the elapsed share of the UTC calendar month.
//
// The projection is advisory, never a posting gate: it neither blocks
// nor permits postings. The hard control stays ErrBudgetExceeded (see
// budget.go), which rejects a posting whose commit would take the
// month's cumulative spend above the budget. Pace and enforcement are
// orthogonal by design — a prediction can be wrong (spend is bursty),
// a gate must be right. The pace alert fires as a budget_pace_alert
// audit event plus the ledger_budget_pace_alerts_total counter so the
// operator's alerting pipeline sees the same signal the reconcile
// report shows.
//
// The alert is a once-per-(account, currency, month) transition, not a
// per-call event: the first BudgetPace call that projects an overrun
// fires the alert and records it in budgetPaceAlerted; later calls in
// the same month stay silent. Repeated firing would page the operator
// every time the pace job runs — the projection hasn't changed, only
// the sampling did. A new month starts with a clean slate (the alerted
// set is pruned with the monthly-spend buckets).
//
// Reconcile discloses every budget's pace (the "budget_pace" section)
// but does not fire alerts: Reconcile runs under the read lock and is
// read-only, while firing an alert mutates the alerted set. The alert
// belongs to the explicit pace check — BudgetPace — the way the hard
// budget gate belongs to the posting paths.

// ErrInvalidBudgetPaceRatio is returned by SetBudgetPaceAlertRatio when
// the ratio is not a positive finite number. A ratio of 0 or less would
// alert on every non-negative projection (or divide nothing by
// nothing); NaN/Inf are rejected outright — fail-fast at configuration,
// like every other risk-control threshold in this package.
var ErrInvalidBudgetPaceRatio = errors.New("ledger: budget pace alert ratio must be a positive finite number")

// DefaultBudgetPaceAlertRatio is the out-of-the-box pace alert
// multiplier: a pace alert fires when the projected month-end spend
// exceeds the budget itself. SetBudgetPaceAlertRatio lowers it for
// earlier warning (e.g. 0.8 pages when the trajectory would spend 80%
// of the budget).
const DefaultBudgetPaceAlertRatio = 1.0

// BudgetPace is one configured monthly spend budget's spend trajectory
// at a point in time: the booked month-to-date spend linearly
// extrapolated to month end at the current run-rate.
type BudgetPace struct {
	// Account and Currency identify the budget; Month is the UTC
	// calendar month the figures describe, as "2006-01".
	Account     AccountID `json:"account"`
	Currency    string    `json:"currency"`
	Month       string    `json:"month"`
	BudgetCents int64     `json:"budget_cents"`
	// UsedCents is the account's booked spend for the month, in cents
	// (the same figure AccountBudgetUsage reports: booked entries plus
	// spend reserved by pending dual-control reviews).
	UsedCents int64 `json:"used_cents"`
	// ElapsedFraction is the share of the UTC calendar month elapsed
	// at report time, in (0, 1]. Before the month starts it is 0 (no
	// run-rate exists yet); at or past month end it is 1 and the
	// projection is the booked spend itself.
	ElapsedFraction float64 `json:"elapsed_fraction"`
	// ProjectedCents is UsedCents / ElapsedFraction — the linear
	// month-end extrapolation, saturating at MaxInt64. At the month's
	// first instant there is no run-rate, so the projection equals the
	// booked spend; the same holds past month end, when the month is
	// closed and the "trajectory" is the final figure.
	ProjectedCents int64 `json:"projected_cents"`
	// ProjectedUtilization is ProjectedCents / BudgetCents as a
	// fraction; 0 when the budget is 0 (a zero budget admits no spend,
	// so used — and therefore the projection — is necessarily 0 and
	// the ratio would be 0/0).
	ProjectedUtilization float64 `json:"projected_utilization"`
	// PaceAlert is true when the projection exceeds the budget times
	// the configured alert ratio: the account is on track to overspend
	// its month at the current pace. Advisory only — it never blocks a
	// posting.
	PaceAlert bool `json:"pace_alert"`
}

// SetBudgetPaceAlertRatio configures the pace-alert multiplier: a pace
// alert fires when a budget's projected month-end spend exceeds
// budget * ratio. The default is 1.0 (see
// DefaultBudgetPaceAlertRatio). Ratios below 1.0 page earlier; above
// 1.0 only on severe overruns. Non-positive, NaN, or infinite ratios
// are rejected with ErrInvalidBudgetPaceRatio. Structural config, like
// the budgets themselves: it does not bump the ledger version.
func (l *Ledger) SetBudgetPaceAlertRatio(ratio float64) error {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return ErrInvalidBudgetPaceRatio
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budgetPaceAlertRatio = ratio
	return nil
}

// BudgetPaceAlertRatio reports the configured pace-alert multiplier.
func (l *Ledger) BudgetPaceAlertRatio() float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.budgetPaceAlertRatio
}

// BudgetPace computes every configured monthly spend budget's spend
// trajectory at now: the booked month-to-date spend linearly
// extrapolated to the UTC month end at the current run-rate.
//
// This is the alert entry point, not just a report: the first call in
// a month whose projection exceeds the budget times the alert ratio
// fires a budget_pace_alert audit event and bumps the
// ledger_budget_pace_alerts_total counter. The alert fires once per
// (account, currency, month) — later calls stay silent until the next
// month — because the projection, not the sampling, is the signal.
// Reconcile's "budget_pace" section discloses the same rows without
// firing alerts (Reconcile is read-only).
//
// Advisory only: it neither blocks nor permits postings. The hard
// monthly-spend gate stays ErrBudgetExceeded in the posting paths.
func (l *Ledger) BudgetPace(now time.Time) []BudgetPace {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budgetPaceLocked(now, true)
}

// budgetPaceLocked is the lock-held core of BudgetPace; fireAlerts
// controls whether newly-detected overruns emit the audit event and
// bump the counter (true from BudgetPace; false from Reconcile, which
// must stay read-only). Callers must hold l.mu; the write lock is
// required when fireAlerts is true, the read lock suffices otherwise.
func (l *Ledger) budgetPaceLocked(now time.Time, fireAlerts bool) []BudgetPace {
	now = now.UTC()
	month := utcMonth(now)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	monthLen := monthEnd.Sub(monthStart)
	var elapsed float64
	switch {
	case !now.After(monthStart):
		// At or before the month's first instant there is no
		// run-rate: the projection is the booked spend.
		elapsed = 0
	case !now.Before(monthEnd):
		// Month end or later: the trajectory is closed, the
		// projection is the final booked spend.
		elapsed = 1
	default:
		elapsed = float64(now.Sub(monthStart)) / float64(monthLen)
	}
	out := make([]BudgetPace, 0, len(l.budgets))
	for k, budget := range l.budgets {
		used := l.monthlySpend[monthlySpendKey{account: k.account, currency: k.currency, month: month}]
		projected := projectPace(used, elapsed)
		var utilization float64
		if budget > 0 {
			utilization = float64(projected) / float64(budget)
		}
		alert := budget > 0 && utilization > l.budgetPaceAlertRatio
		if alert && fireAlerts {
			mkey := monthlySpendKey{account: k.account, currency: k.currency, month: month}
			if !l.budgetPaceAlerted[mkey] {
				l.budgetPaceAlerted[mkey] = true
				l.budgetPaceAlerts.Add(1)
				l.emitAudit(AuditEvent{
					Op:            "budget_pace_alert",
					Actor:         "BudgetPace",
					TraceID:       string(k.account),
					VersionBefore: l.version,
					VersionAfter:  l.version,
					Accounts:      []AccountID{k.account},
					Details: map[string]any{
						"account":               string(k.account),
						"currency":              k.currency,
						"month":                 month,
						"budget_cents":          budget,
						"used_cents":            used,
						"projected_cents":       projected,
						"projected_utilization": utilization,
						"alert_ratio":           l.budgetPaceAlertRatio,
					},
				})
			}
		}
		out = append(out, BudgetPace{
			Account:              k.account,
			Currency:             k.currency,
			Month:                month,
			BudgetCents:          budget,
			UsedCents:            used,
			ElapsedFraction:      elapsed,
			ProjectedCents:       projected,
			ProjectedUtilization: utilization,
			PaceAlert:            alert,
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

// BudgetPaceAlertCount reports how many budget pace alerts have fired
// since process start: the ledger-side counter surfaced on GET
// /metrics as ledger_budget_pace_alerts_total. Operational metric,
// not books — it does not round-trip through snapshots.
func (l *Ledger) BudgetPaceAlertCount() uint64 {
	return l.budgetPaceAlerts.Load()
}

// projectPace extrapolates cents booked over elapsed (a month-share in
// (0,1)) to the full month, saturating at MaxInt64. Pure helper, kept
// exported-testable for the boundary arithmetic.
func projectPace(cents int64, elapsed float64) int64 {
	if elapsed <= 0 || elapsed >= 1 {
		return cents
	}
	p := float64(cents) / elapsed
	if p >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(p)
}

// budgetPaceAlertPruneLocked drops pace-alert marks for UTC months
// older than last month, alongside the monthly-spend buckets (see
// maybePruneMonthlySpendLocked): a new month starts with a clean
// alert slate. Callers must hold l.mu.
func (l *Ledger) budgetPaceAlertPruneLocked(now time.Time) {
	cutoff := now.UTC().AddDate(0, -1, 0).Format("2006-01")
	for k := range l.budgetPaceAlerted {
		if k.month < cutoff {
			delete(l.budgetPaceAlerted, k)
		}
	}
}
