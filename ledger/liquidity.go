package ledger

import (
	"errors"
	"sort"
	"time"
)

// Intraday liquidity positions (LG-98).
//
// Per (account, currency), the intraday liquidity report forecasts one UTC
// day's net cash position from four independent sources:
//
//  1. Booked flows: journaled entries whose CreatedAt falls on the day.
//     The debit leg is an inflow, the credit leg an outflow — the package
//     convention (see the ledger.go doc: the debit account is increased,
//     the credit account decreased).
//  2. Holds: authorization holds are off-journal reservations, so they
//     never appear in the booked flows. A hold still outstanding at the
//     end of the day (status active, ExpiresAt past the day's end)
//     counts as reserved outflow. Holds that expire during the day release
//     their funds back and are excluded — expiry is lazy by predicate,
//     exactly like heldLocked.
//  3. Schedule firings: active transfer schedules whose nominal runs fall
//     on the day. A run whose NextRunAt is already past counts once (the
//     catch-up the next SweepDue would fire — sweeps fire one run per
//     schedule and skip the missed gap); later runs follow the schedule's
//     Interval until the day ends or EndsAt passes. Each run books the
//     nominal AmountCents as outflow on From and inflow on To, in the
//     schedule's currency. FX-settled schedules (ToCurrency set) attribute
//     the nominal principal in the schedule currency — the conversion
//     detail is excluded from the forecast by design.
//  4. Term deposits: active deposits whose MaturesAt falls on the day
//     release principal plus the interest accrued so far as inflow.
//
// The report is strictly read-only: it takes the read lock, writes no
// journal rows, bumps no version, emits no audit events, and mutates no
// balances, holds, schedules, or deposits. It is a pure function of the
// ledger state and the requested date, so the same date always yields the
// same positions until the underlying state changes.
//
// Low-liquidity alerts ride the balance-watch alert channel (see
// balance_watch.go): SetLiquidityAlertThreshold installs an opt-in,
// per-account, per-currency floor for the day's net position, and
// EvaluateLiquidityAlerts emits one structured "low_liquidity_alert" audit
// event per breach — advisory, never a rejection, edge-triggered with
// re-arm on recovery, exactly like the balance watches. Thresholds are
// structural config (no version bump, snapshot-persistent, disclosed by
// Reconcile), but the breach state is runtime-only like its balance-watch
// sibling.

// LiquidityAlertThreshold is one account's configured intraday-liquidity
// low threshold in one currency: when the day's net position (see
// LiquidityPosition) falls below ThresholdCents,
// EvaluateLiquidityAlerts fires one low_liquidity_alert audit event.
type LiquidityAlertThreshold struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	ThresholdCents int64     `json:"threshold_cents"`
}

// liquidityThresholdKey identifies one configured threshold: an account's
// net-position floor in one currency.
type liquidityThresholdKey struct {
	account  AccountID
	currency string
}

// LiquidityPosition is one (account, currency)'s intraday liquidity
// position on one UTC day: the forecasted inflow/outflow breakdown and
// the net position.
type LiquidityPosition struct {
	Account  AccountID `json:"account"`
	Currency string    `json:"currency"`
	// BookedInflowCents is the day's journaled inflow: the debit legs of
	// entries created that day.
	BookedInflowCents int64 `json:"booked_inflow_cents"`
	// BookedOutflowCents is the day's journaled outflow: the credit legs
	// of entries created that day.
	BookedOutflowCents int64 `json:"booked_outflow_cents"`
	// HoldReservedCents is the outflow reserved by authorization holds
	// still outstanding at the end of the day. Off-journal, so it never
	// overlaps the booked flows.
	HoldReservedCents int64 `json:"hold_reserved_cents"`
	// ScheduledInflowCents / ScheduledOutflowCents are the nominal
	// principal of active transfer-schedule runs firing that day (inflow
	// on To, outflow on From).
	ScheduledInflowCents  int64 `json:"scheduled_inflow_cents"`
	ScheduledOutflowCents int64 `json:"scheduled_outflow_cents"`
	// TermDepositInflowCents is the principal plus accrued interest of
	// active term deposits maturing that day.
	TermDepositInflowCents int64 `json:"term_deposit_inflow_cents"`
	// NetPositionCents is the forecasted net: total inflow minus total
	// outflow, saturated (see satAddInt64).
	NetPositionCents int64 `json:"net_position_cents"`
	// ThresholdCents is the configured low-liquidity alert floor, when
	// one is set for this (account, currency).
	ThresholdCents *int64 `json:"threshold_cents,omitempty"`
	// BelowThreshold is true when a threshold is configured and the net
	// position is below it.
	BelowThreshold bool `json:"below_threshold"`
}

// IntradayLiquidityReport is the read-only intraday liquidity forecast
// for one UTC day, sorted by (account, currency).
type IntradayLiquidityReport struct {
	Date        string              `json:"date"`
	GeneratedAt time.Time           `json:"generated_at"`
	Positions   []LiquidityPosition `json:"positions"`
}

// LiquidityAlert is one low_liquidity_alert emission: the (account,
// currency) whose day's net position fell below its configured floor.
type LiquidityAlert struct {
	Account          AccountID `json:"account"`
	Currency         string    `json:"currency"`
	Date             string    `json:"date"`
	ThresholdCents   int64     `json:"threshold_cents"`
	NetPositionCents int64     `json:"net_position_cents"`
}

// Liquidity validation errors.
var (
	ErrEmptyLiquidityAccount = errors.New("ledger: liquidity alert account must not be empty")
)

// liquidityFlowAcc accumulates one (account, currency)'s intraday flows.
type liquidityFlowAcc struct {
	bookedIn  int64
	bookedOut int64
	held      int64
	schedIn   int64
	schedOut  int64
	tdIn      int64
}

// IntradayLiquidity returns the read-only intraday liquidity forecast for
// the UTC day containing date. See the package doc for the four forecast
// sources and the read-only contract.
func (l *Ledger) IntradayLiquidity(date time.Time) IntradayLiquidityReport {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.intradayLiquidityLocked(date)
}

// intradayLiquidityLocked is IntradayLiquidity's body. Callers must hold
// l.mu (the read lock suffices — nothing is mutated).
func (l *Ledger) intradayLiquidityLocked(date time.Time) IntradayLiquidityReport {
	day := utcDay(date)
	dayStart := time.Date(date.UTC().Year(), date.UTC().Month(), date.UTC().Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	acc := make(map[liquidityThresholdKey]*liquidityFlowAcc)
	get := func(a AccountID, currency string) *liquidityFlowAcc {
		k := liquidityThresholdKey{account: a, currency: currency}
		if f, ok := acc[k]; ok {
			return f
		}
		f := &liquidityFlowAcc{}
		acc[k] = f
		return f
	}

	// 1. Booked flows: journaled entries created that day. Debit leg is
	// an inflow, credit leg an outflow (the package convention).
	for _, e := range l.entries {
		if utcDay(e.CreatedAt) != day {
			continue
		}
		in := get(e.DebitAccount, e.Currency)
		in.bookedIn = satAddInt64(in.bookedIn, e.AmountCents)
		out := get(e.CreditAccount, e.Currency)
		out.bookedOut = satAddInt64(out.bookedOut, e.AmountCents)
	}

	// 2. Holds: reservations still outstanding at the end of the day
	// count as reserved outflow. Expired holds (even before the
	// ExpireHolds sweep) and non-active statuses are excluded.
	for _, h := range l.holds {
		if h.Status != HoldStatusActive || !dayEnd.Before(h.ExpiresAt) {
			continue
		}
		f := get(h.Account, h.Currency)
		f.held = satAddInt64(f.held, h.AmountCents)
	}

	// 3. Schedule firings: nominal runs of active schedules falling on
	// the day. An overdue schedule contributes its one catch-up run (the
	// run the next SweepDue would fire); later runs follow the Interval
	// until the day ends or EndsAt passes.
	for _, sched := range l.schedules {
		if sched.Status != ScheduleActive {
			continue
		}
		for range scheduleFiringsOnDay(sched, dayStart, dayEnd) {
			from := get(sched.From, sched.Currency)
			from.schedOut = satAddInt64(from.schedOut, sched.AmountCents)
			to := get(sched.To, sched.Currency)
			to.schedIn = satAddInt64(to.schedIn, sched.AmountCents)
		}
	}

	// 4. Term deposits: active deposits maturing that day release
	// principal plus accrued interest as inflow.
	for _, td := range l.termDeposits {
		if td.Status != TermDepositActive || utcDay(td.MaturesAt) != day {
			continue
		}
		f := get(td.Account, td.Currency)
		release := satAddInt64(td.AmountCents, l.termDepositInterestAccruedLocked(td.ID))
		f.tdIn = satAddInt64(f.tdIn, release)
	}

	// Threshold-configured pairs always appear, even with zero flows, so
	// a floor on a quiet account still evaluates.
	for k := range l.liquidityThresholds {
		get(k.account, k.currency)
	}

	positions := make([]LiquidityPosition, 0, len(acc))
	for k, f := range acc {
		inflow := satAddInt64(satAddInt64(f.bookedIn, f.schedIn), f.tdIn)
		outflow := satAddInt64(satAddInt64(f.bookedOut, f.held), f.schedOut)
		net := satAddInt64(inflow, -outflow)
		p := LiquidityPosition{
			Account:                k.account,
			Currency:               k.currency,
			BookedInflowCents:      f.bookedIn,
			BookedOutflowCents:     f.bookedOut,
			HoldReservedCents:      f.held,
			ScheduledInflowCents:   f.schedIn,
			ScheduledOutflowCents:  f.schedOut,
			TermDepositInflowCents: f.tdIn,
			NetPositionCents:       net,
		}
		if th, ok := l.liquidityThresholds[k]; ok {
			p.ThresholdCents = int64ptr(th.ThresholdCents)
			p.BelowThreshold = net < th.ThresholdCents
		}
		positions = append(positions, p)
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].Account != positions[j].Account {
			return positions[i].Account < positions[j].Account
		}
		return positions[i].Currency < positions[j].Currency
	})

	return IntradayLiquidityReport{
		Date:        day,
		GeneratedAt: date,
		Positions:   positions,
	}
}

// scheduleFiringsOnDay returns the nominal firing times of one schedule
// that fall inside [dayStart, dayEnd). The schedule must be active: an
// overdue NextRunAt yields its one catch-up run (the run the next
// SweepDue would fire — sweeps never backfill the missed gap); otherwise
// the cadence advances from NextRunAt by whole intervals until the day
// ends or EndsAt passes.
func scheduleFiringsOnDay(sched *TransferSchedule, dayStart, dayEnd time.Time) []time.Time {
	if sched.Interval <= 0 {
		return nil
	}
	var out []time.Time
	due := sched.NextRunAt
	if due.Before(dayStart) {
		// Overdue: the next sweep fires exactly one run.
		if !sched.EndsAt.IsZero() && due.After(sched.EndsAt) {
			return nil
		}
		return []time.Time{due}
	}
	for due.Before(dayStart) {
		due = due.Add(sched.Interval)
	}
	for due.Before(dayEnd) {
		if !sched.EndsAt.IsZero() && due.After(sched.EndsAt) {
			break
		}
		out = append(out, due)
		due = due.Add(sched.Interval)
	}
	return out
}

// SetLiquidityAlertThreshold configures (or replaces) the low-liquidity
// alert floor for one account in one currency: the first
// EvaluateLiquidityAlerts run that sees the day's net position below
// thresholdCents emits one low_liquidity_alert audit event. Setting a
// threshold is structural, like SetBalanceWatch: it does not bump the
// ledger version (no balances change), survives disaster-recovery
// snapshots, and is listed by Reconcile. The operation is audited as
// "liquidity_alert_threshold_set".
func (l *Ledger) SetLiquidityAlertThreshold(a AccountID, currency string, thresholdCents int64) error {
	if a == "" {
		return ErrEmptyLiquidityAccount
	}
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.liquidityThresholds[liquidityThresholdKey{account: a, currency: currency}] = LiquidityAlertThreshold{
		Account:        a,
		Currency:       currency,
		ThresholdCents: thresholdCents,
	}
	l.emitAudit(AuditEvent{
		Op:       "liquidity_alert_threshold_set",
		Actor:    "SetLiquidityAlertThreshold",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":         string(a),
			"currency":        currency,
			"threshold_cents": thresholdCents,
		},
	})
	return nil
}

// ClearLiquidityAlertThreshold removes the low-liquidity alert floor for
// one account in one currency, and clears its breach state. Clearing a
// threshold that was never set is a no-op. Like SetLiquidityAlertThreshold,
// it is structural and does not bump the ledger version. The operation is
// audited as "liquidity_alert_threshold_cleared".
func (l *Ledger) ClearLiquidityAlertThreshold(a AccountID, currency string) {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := liquidityThresholdKey{account: a, currency: currency}
	if _, ok := l.liquidityThresholds[key]; !ok {
		return
	}
	delete(l.liquidityThresholds, key)
	delete(l.liquidityBreached, key)
	l.emitAudit(AuditEvent{
		Op:       "liquidity_alert_threshold_cleared",
		Actor:    "ClearLiquidityAlertThreshold",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":  string(a),
			"currency": currency,
		},
	})
}

// GetLiquidityAlertThreshold reports the configured low-liquidity alert
// floor for one account in one currency, and whether one is configured.
func (l *Ledger) GetLiquidityAlertThreshold(a AccountID, currency string) (int64, bool) {
	if currency == "" {
		currency = DefaultCurrency
	} else {
		var err error
		if currency, err = normalizeCurrency(currency); err != nil {
			return 0, false
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	th, ok := l.liquidityThresholds[liquidityThresholdKey{account: a, currency: currency}]
	if !ok {
		return 0, false
	}
	return th.ThresholdCents, true
}

// ListLiquidityAlertThresholds returns every configured low-liquidity
// alert floor, sorted by (account, currency), for the reconciliation
// report and the disaster-recovery snapshot.
func (l *Ledger) ListLiquidityAlertThresholds() []LiquidityAlertThreshold {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.liquidityThresholdsLocked()
}

// liquidityThresholdsLocked returns every configured threshold, sorted by
// (account, currency). Callers must hold l.mu.
func (l *Ledger) liquidityThresholdsLocked() []LiquidityAlertThreshold {
	out := make([]LiquidityAlertThreshold, 0, len(l.liquidityThresholds))
	for _, th := range l.liquidityThresholds {
		out = append(out, th)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// EvaluateLiquidityAlerts evaluates every configured low-liquidity
// threshold against the net positions of the UTC day containing date
// and fires one structured "low_liquidity_alert" audit event per breach,
// through the same alert channel as the balance watches (advisory only —
// nothing is rejected, nothing is booked). Edge-triggered: the first
// evaluation that sees the net position below the floor fires; further
// evaluations while the position stays below are silent; an evaluation
// that sees the position back at or above the floor re-arms the alert
// silently. It returns the alerts fired by this call.
func (l *Ledger) EvaluateLiquidityAlerts(date time.Time) []LiquidityAlert {
	l.mu.Lock()
	defer l.mu.Unlock()
	report := l.intradayLiquidityLocked(date)
	out := make([]LiquidityAlert, 0)
	for _, p := range report.Positions {
		key := liquidityThresholdKey{account: p.Account, currency: p.Currency}
		th, ok := l.liquidityThresholds[key]
		if !ok {
			continue
		}
		if p.NetPositionCents < th.ThresholdCents {
			// Below the floor: fire once, then silence until recovery.
			if !l.liquidityBreached[key] {
				l.liquidityBreached[key] = true
				l.liquidityAlertCount++
				l.emitAudit(AuditEvent{
					Op:       "low_liquidity_alert",
					Actor:    "EvaluateLiquidityAlerts",
					Accounts: []AccountID{p.Account},
					Details: map[string]any{
						"account":            string(p.Account),
						"currency":           p.Currency,
						"date":               report.Date,
						"threshold_cents":    th.ThresholdCents,
						"net_position_cents": p.NetPositionCents,
					},
				})
				out = append(out, LiquidityAlert{
					Account:          p.Account,
					Currency:         p.Currency,
					Date:             report.Date,
					ThresholdCents:   th.ThresholdCents,
					NetPositionCents: p.NetPositionCents,
				})
			}
		} else {
			// Recovered to at or above the floor: re-arm, silently.
			delete(l.liquidityBreached, key)
		}
	}
	return out
}

// LiquidityAlertCount reports how many low-liquidity alert events the
// ledger has emitted since it started. The counter resets on restart —
// alert history lives in the structured audit log (op
// "low_liquidity_alert"), which is the durable record.
func (l *Ledger) LiquidityAlertCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.liquidityAlertCount
}

// recomputeLiquidityBreachesLocked re-derives the breach state from the
// restored ledger after a snapshot import: an (account, currency) whose
// restored net position for today is already below its floor counts as
// breached, so the first evaluation after the restore does not backfire a
// stale alert. It emits no events and touches no counters — a restore is
// not an alert.
func (l *Ledger) recomputeLiquidityBreachesLocked() {
	l.liquidityBreached = make(map[liquidityThresholdKey]bool)
	report := l.intradayLiquidityLocked(time.Now())
	for _, p := range report.Positions {
		if p.BelowThreshold {
			l.liquidityBreached[liquidityThresholdKey{account: p.Account, currency: p.Currency}] = true
		}
	}
}
