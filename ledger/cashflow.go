package ledger

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// Cash flow statement (LG-144).
//
// The statement answers the treasury question a payments operator asks at
// period end: how much cash moved in and out, in which activity, and does
// the period's net cash movement reconcile with the cash accounts'
// opening and closing balances?
//
// Cash-class accounts: an account is cash-class when it is marked with
// MarkCashAccount — opt-in, independent of the chart-of-accounts
// registrations (a cash account that pays out typically stays
// unregistered from the chart, because chart.go's direction control
// rejects crediting an asset-class account; only the counterparty's
// chart type drives the activity classification, so the mark works the
// same either way). The mark is structural config: marking or unmarking
// does not bump the journal version, changes are audited
// (cash_account_marked / cash_account_unmarked), and the marks survive
// disaster-recovery snapshots. A mark may be scoped to one currency (a
// multi-currency settlement account that is cash-class only in its
// settlement currency) or cover every currency (empty currency).
//
// Aggregation: for each effective journal entry (PendingReview rows are
// excluded — booked but not effective, the LG-43 settlement convention)
// in the report currency whose CreatedAt falls in [from, to] (UTC calendar
// days, inclusive), every leg on a cash-class account is a cash movement:
// a debit leg (the cash account receives money) is an inflow, a credit leg
// (the cash account pays money) is an outflow. The activity classification
// comes from the counterparty — the other leg's account — and its
// chart-of-accounts type, via cashActivityFor below.
//
// Counterparty chart type -> activity mapping (approximations documented,
// because the ledger cannot know the business substance of a leg):
//
//	revenue  -> operating   (sales, fees, interest income)
//	expense  -> operating   (supplier payments, wages, operating costs)
//	equity   -> financing   (owner contributions, dividend payouts)
//	liability-> financing   (borrowings and repayments; approximation —
//	            working-capital moves like trade payables are arguably
//	            operating, so keep payables unregistered to land them in
//	            operating, or accept the financing label)
//	asset    -> investing   (capex, asset disposals; approximation —
//	            intercompany asset book moves also land here)
//	unregistered counterparty -> operating (fallback: chart registration
//	            is opt-in, so an unmapped counterparty lands in operating,
//	            the least surprising default for a payments ledger)
//
// A transfer between two cash accounts nets to zero at the pool level:
// with both accounts unregistered from the chart (the usual case —
// chart.go's direction control rejects crediting an asset-class
// account, so cash accounts that pay out typically stay unregistered),
// the legs classify as operating by the unregistered-counterparty
// fallback below. The gross flows are honest, and every total still
// reconciles.
//
// One honest consequence of the strict chart direction control: with
// counterparties registered, investing inflows via asset-class
// counterparties do not occur (crediting an asset is rejected), and
// financing outflows via liability/equity counterparties do not occur
// (debiting them is rejected). The investing leg of the statement is
// therefore typically capex-style outflows; the financing leg is
// borrowings and owner contributions. The mapping stays truthful
// because it classifies what the ledger actually allows to be posted.
//
// Reconciliation: the report independently accumulates each cash account's
// opening balance (effective legs before the period), its period net flow,
// and its closing balance (effective legs before the period end). A cash
// account whose closing != opening + net flow lands in Discrepancies —
// the same self-consistency role TrialBalanceDiscrepancy plays in
// Reconcile: on a healthy report the list is empty, and the shape exists
// so tooling can distinguish "ran clean" from "did not run".
//
// The report is read-only: building it bumps no version, emits no audit
// event, and posts no journal rows.

// Cash flow statement errors.
var (
	// ErrEmptyCashAccount is returned by MarkCashAccount when the
	// account ID is empty.
	ErrEmptyCashAccount = errors.New("ledger: cash account must not be empty")
	// ErrInvalidCashFlowRange is returned by CashFlow when from/to is
	// not "YYYY-MM-DD" or when from is after to.
	ErrInvalidCashFlowRange = errors.New("ledger: cash flow range must be from/to YYYY-MM-DD with from <= to")
	// ErrEmptyCashFlowCurrency is returned by CashFlow when the
	// currency is empty.
	ErrEmptyCashFlowCurrency = errors.New("ledger: cash flow currency must not be empty")
	// ErrCashFlowOverflow is returned by CashFlow when the aggregation
	// would overflow int64 cents.
	ErrCashFlowOverflow = errors.New("ledger: cash flow aggregation overflowed")
)

// CashActivity is one of the three cash-flow statement activities.
type CashActivity string

// The three cash-flow statement activities.
const (
	CashActivityOperating CashActivity = "operating"
	CashActivityInvesting CashActivity = "investing"
	CashActivityFinancing CashActivity = "financing"
)

// cashActivityOrder is the deterministic activity order used in reports
// and CSV exports.
var cashActivityOrder = []CashActivity{
	CashActivityOperating,
	CashActivityInvesting,
	CashActivityFinancing,
}

// cashActivityFor maps the counterparty's chart-of-accounts type to the
// cash-flow activity. The mapping's approximations are documented on the
// package comment above; an unregistered counterparty (chart registration
// is opt-in) falls back to operating.
func cashActivityFor(t AccountType, registered bool) CashActivity {
	if !registered {
		return CashActivityOperating
	}
	switch t {
	case AccountTypeRevenue, AccountTypeExpense:
		return CashActivityOperating
	case AccountTypeEquity, AccountTypeLiability:
		return CashActivityFinancing
	case AccountTypeAsset:
		return CashActivityInvesting
	}
	return CashActivityOperating // unreachable: validAccountType has five members
}

// CashAccountMark is one opt-in cash-class designation of an account: the
// account's movements in the statement currency are cash inflows/outflows.
// An empty Currency marks the account in every currency. It is the unit
// the snapshot config record serializes (see snapshot.go).
type CashAccountMark struct {
	Account  AccountID `json:"account"`
	Currency string    `json:"currency,omitempty"`
}

// normalizeCashCurrency validates the currency of a cash-class mark: an
// empty code means "every currency", so it must NOT pass through
// normalizeCurrency (which maps empty to the default currency); a
// non-empty code is canonicalized like any other.
func normalizeCashCurrency(code string) (string, error) {
	if code == "" {
		return "", nil
	}
	return normalizeCurrency(code)
}

// MarkCashAccount designates an account as cash-class for the cash-flow
// statement. Currency scopes the mark to one currency; an empty currency
// marks the account in every currency. Re-marking an account is
// idempotent. Structural config: no version bump, audited as
// "cash_account_marked", survives disaster-recovery snapshots.
func (l *Ledger) MarkCashAccount(account AccountID, currency string) error {
	if account == "" {
		return ErrEmptyCashAccount
	}
	cur, err := normalizeCashCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur == "" {
		l.cashAccounts[account] = true
	} else {
		l.cashAccountCurrencies[accountCurrency{account: account, currency: cur}] = true
	}
	details := map[string]any{"account": string(account)}
	if cur != "" {
		details["currency"] = cur
	} else {
		details["all_currencies"] = true
	}
	l.emitAudit(AuditEvent{
		Op:       "cash_account_marked",
		Actor:    "MarkCashAccount",
		Accounts: []AccountID{account},
		Details:  details,
	})
	return nil
}

// UnmarkCashAccount removes the cash-class designation of an account in
// the given currency scope (empty currency removes the every-currency
// mark). Removing a mark that was never set is a no-op. Structural
// config: no version bump, audited as "cash_account_unmarked".
func (l *Ledger) UnmarkCashAccount(account AccountID, currency string) error {
	if account == "" {
		return ErrEmptyCashAccount
	}
	cur, err := normalizeCashCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur == "" {
		delete(l.cashAccounts, account)
	} else {
		delete(l.cashAccountCurrencies, accountCurrency{account: account, currency: cur})
	}
	l.emitAudit(AuditEvent{
		Op:       "cash_account_unmarked",
		Actor:    "UnmarkCashAccount",
		Accounts: []AccountID{account},
		Details: map[string]any{
			"account":       string(account),
			"currency":      cur,
			"marks_cleared": true,
		},
	})
	return nil
}

// IsCashAccount reports whether account is cash-class in the given
// currency: either marked in every currency, or marked for that currency.
// The currency should be a canonical code (entries store canonical codes).
func (l *Ledger) IsCashAccount(account AccountID, currency string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.isCashAccountLocked(account, currency)
}

// isCashAccountLocked is IsCashAccount's body. Callers must hold l.mu;
// the read lock suffices.
func (l *Ledger) isCashAccountLocked(account AccountID, currency string) bool {
	if l.cashAccounts[account] {
		return true
	}
	return l.cashAccountCurrencies[accountCurrency{account: account, currency: currency}]
}

// cashAccountsLocked lists the cash-class designations, sorted by
// (account, currency). Callers must hold l.mu.
func (l *Ledger) cashAccountsLocked() []CashAccountMark {
	out := make([]CashAccountMark, 0, len(l.cashAccounts)+len(l.cashAccountCurrencies))
	for a := range l.cashAccounts {
		out = append(out, CashAccountMark{Account: a})
	}
	for k := range l.cashAccountCurrencies {
		out = append(out, CashAccountMark{Account: k.account, Currency: k.currency})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// CashAccounts lists the cash-class designations, sorted by (account,
// currency). The returned slice is a copy.
func (l *Ledger) CashAccounts() []CashAccountMark {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cashAccountsLocked()
}

// WithCashAccount designates an account as cash-class at construction
// time (see MarkCashAccount). An empty account or an invalid currency
// panics — fail-fast at construction, like the settlement channels.
func WithCashAccount(account AccountID, currency string) Option {
	if account == "" {
		panic("ledger: cash account must not be empty")
	}
	cur, err := normalizeCashCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid cash account currency %q", currency))
	}
	return func(l *Ledger) {
		if cur == "" {
			l.cashAccounts[account] = true
			return
		}
		l.cashAccountCurrencies[accountCurrency{account: account, currency: cur}] = true
	}
}

// CashFlowOptions drives one cash-flow statement.
type CashFlowOptions struct {
	// From is the inclusive period start, "YYYY-MM-DD" (UTC).
	From string
	// To is the inclusive period end, "YYYY-MM-DD" (UTC).
	To string
	// Currency is the 3-letter ISO 4217 code the statement is drawn
	// in. Required — cents never mix across currencies.
	Currency string
}

// CashFlowActivity is one activity's aggregation: gross inflows, gross
// outflows, and the net movement. InflowCents - OutflowCents == NetCents.
type CashFlowActivity struct {
	Activity     CashActivity `json:"activity"`
	InflowCents  int64        `json:"inflow_cents"`
	OutflowCents int64        `json:"outflow_cents"`
	NetCents     int64        `json:"net_cents"`
}

// CashFlowAccountRow is one cash account's period view: its gross flows,
// the per-activity breakdown, and the opening/closing balances the
// reconciliation identity ties together (ClosingCents == OpeningCents +
// NetCents on a clean report).
type CashFlowAccountRow struct {
	Account      AccountID `json:"account"`
	InflowCents  int64     `json:"inflow_cents"`
	OutflowCents int64     `json:"outflow_cents"`
	NetCents     int64     `json:"net_cents"`
	OpeningCents int64     `json:"opening_cents"`
	ClosingCents int64     `json:"closing_cents"`
	// Activities is the per-activity breakdown of this account's flows,
	// in the fixed operating/investing/financing order.
	Activities []CashFlowActivity `json:"activities"`
}

// CashFlowDiscrepancy is one cash account whose independently accumulated
// closing balance does not equal its opening balance plus its period net
// flow. On a healthy report the list is empty; the shape exists so
// reconciliation tooling can distinguish "ran clean" from "did not run".
type CashFlowDiscrepancy struct {
	Account              AccountID `json:"account"`
	OpeningCents         int64     `json:"opening_cents"`
	NetFlowCents         int64     `json:"net_flow_cents"`
	ExpectedClosingCents int64     `json:"expected_closing_cents"`
	ActualClosingCents   int64     `json:"actual_closing_cents"`
	DifferenceCents      int64     `json:"difference_cents"`
}

// CashFlowReport is the cash-flow statement for one period in one
// currency. Read-only: building it changes no accounting state — no
// version bump, no audit event, no journal rows.
type CashFlowReport struct {
	From         string            `json:"from"`
	To           string            `json:"to"`
	Currency     string            `json:"currency"`
	GeneratedAt  time.Time         `json:"generated_at"`
	Version      uint64            `json:"version"`
	CashAccounts []CashAccountMark `json:"cash_accounts"`
	// Activities is the three-activity breakdown, always in the order
	// operating, investing, financing.
	Activities    []CashFlowActivity    `json:"activities"`
	Accounts      []CashFlowAccountRow  `json:"accounts"`
	Discrepancies []CashFlowDiscrepancy `json:"discrepancies"`
	OpeningCents  int64                 `json:"opening_cents"`
	InflowCents   int64                 `json:"inflow_cents"`
	OutflowCents  int64                 `json:"outflow_cents"`
	NetFlowCents  int64                 `json:"net_flow_cents"`
	ClosingCents  int64                 `json:"closing_cents"`
}

// cashFlowAccum is one cash account's running aggregation during the
// report scan.
type cashFlowAccum struct {
	opening, inflow, outflow, closing int64
	actIn, actOut                     map[CashActivity]int64
}

// CashFlow builds the cash-flow statement for the inclusive period
// [from, to] (UTC calendar days) in one currency: per-activity inflow/
// outflow/net from the cash-class accounts' effective legs, each account's
// opening and closing balances, and the reconciliation check that
// closing == opening + net flow.
//
// Entries marked PendingReview are excluded — booked but not effective,
// the LG-43 settlement convention. A ledger with no cash-class marks
// yields an empty-but-valid report (zero totals, no discrepancies), not
// an error: "nothing designated cash" is a configuration state, not a
// failure. The scan holds the read lock for its whole duration, so the
// report describes one consistent point in time. Aggregation uses the
// checked addCents arithmetic: a pathological overflow fails the report
// with ErrCashFlowOverflow instead of wrapping.
func (l *Ledger) CashFlow(now time.Time, opts CashFlowOptions) (CashFlowReport, error) {
	fromDay, err := parseSettlementDay(opts.From)
	if err != nil {
		return CashFlowReport{}, fmt.Errorf("%w: from %q", ErrInvalidCashFlowRange, opts.From)
	}
	toDay, err := parseSettlementDay(opts.To)
	if err != nil {
		return CashFlowReport{}, fmt.Errorf("%w: to %q", ErrInvalidCashFlowRange, opts.To)
	}
	if toDay.Before(fromDay) {
		return CashFlowReport{}, fmt.Errorf("%w: from %q after to %q", ErrInvalidCashFlowRange, opts.From, opts.To)
	}
	cur := opts.Currency
	if cur == "" {
		return CashFlowReport{}, ErrEmptyCashFlowCurrency
	}
	if cur, err = normalizeCurrency(cur); err != nil {
		return CashFlowReport{}, err
	}
	dayStart := fromDay
	dayEnd := toDay.Add(24 * time.Hour) // exclusive

	l.mu.RLock()
	defer l.mu.RUnlock()

	byAccount := make(map[AccountID]*cashFlowAccum)
	activityIn := map[CashActivity]int64{}
	activityOut := map[CashActivity]int64{}

	touch := func(a AccountID) *cashFlowAccum {
		ac := byAccount[a]
		if ac == nil {
			ac = &cashFlowAccum{actIn: map[CashActivity]int64{}, actOut: map[CashActivity]int64{}}
			byAccount[a] = ac
		}
		return ac
	}
	// addChecked accumulates with the ledger's checked cents arithmetic
	// (transfer.go): a pathological total fails the report instead of
	// wrapping around zero.
	addChecked := func(dst *int64, delta int64) error {
		sum, ok := addCents(*dst, delta)
		if !ok {
			return ErrCashFlowOverflow
		}
		*dst = sum
		return nil
	}

	for _, e := range l.entries {
		// Pending-review rows are booked but not effective: balances
		// and totals never see them, so neither does the cash-flow
		// statement.
		if e.PendingReview {
			continue
		}
		if e.Currency != cur {
			continue
		}
		debitCash := l.isCashAccountLocked(e.DebitAccount, cur)
		creditCash := l.isCashAccountLocked(e.CreditAccount, cur)
		if !debitCash && !creditCash {
			continue
		}
		// The closing bucket is accumulated separately from
		// opening+flow, so the reconciliation check below compares two
		// independent aggregations of the same journal rather than an
		// identity with itself.
		if e.CreatedAt.Before(dayEnd) {
			if debitCash {
				if err := addChecked(&touch(e.DebitAccount).closing, e.AmountCents); err != nil {
					return CashFlowReport{}, err
				}
			}
			if creditCash {
				if err := addChecked(&touch(e.CreditAccount).closing, -e.AmountCents); err != nil {
					return CashFlowReport{}, err
				}
			}
		}
		if e.CreatedAt.Before(dayStart) {
			if debitCash {
				if err := addChecked(&touch(e.DebitAccount).opening, e.AmountCents); err != nil {
					return CashFlowReport{}, err
				}
			}
			if creditCash {
				if err := addChecked(&touch(e.CreditAccount).opening, -e.AmountCents); err != nil {
					return CashFlowReport{}, err
				}
			}
			continue
		}
		if !e.CreatedAt.Before(dayEnd) {
			continue
		}
		// In-period legs: the cash account's side decides the flow
		// direction, the counterparty's chart type decides the
		// activity (see cashActivityFor). A debit leg on the cash
		// account is money in (inflow); a credit leg is money out
		// (outflow).
		if debitCash {
			ct, registered := l.chart[e.CreditAccount]
			act := cashActivityFor(ct, registered)
			ac := touch(e.DebitAccount)
			if err := addChecked(&ac.inflow, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			v := ac.actIn[act]
			if err := addChecked(&v, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			ac.actIn[act] = v
			v = activityIn[act]
			if err := addChecked(&v, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			activityIn[act] = v
		}
		if creditCash {
			ct, registered := l.chart[e.DebitAccount]
			act := cashActivityFor(ct, registered)
			ac := touch(e.CreditAccount)
			if err := addChecked(&ac.outflow, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			v := ac.actOut[act]
			if err := addChecked(&v, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			ac.actOut[act] = v
			v = activityOut[act]
			if err := addChecked(&v, e.AmountCents); err != nil {
				return CashFlowReport{}, err
			}
			activityOut[act] = v
		}
	}

	report := CashFlowReport{
		From:         opts.From,
		To:           opts.To,
		Currency:     cur,
		GeneratedAt:  now,
		Version:      l.version,
		CashAccounts: l.cashAccountsLocked(),
		Activities:   make([]CashFlowActivity, 0, len(cashActivityOrder)),
		Accounts:     make([]CashFlowAccountRow, 0, len(byAccount)),
		// Non-nil so the JSON export renders [] instead of null — an
		// empty discrepancy list means "ran clean", not "did not run".
		Discrepancies: make([]CashFlowDiscrepancy, 0),
	}
	ordered := make([]AccountID, 0, len(byAccount))
	for a := range byAccount {
		ordered = append(ordered, a)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, a := range ordered {
		ac := byAccount[a]
		net, ok := addCents(ac.inflow, -ac.outflow)
		if !ok {
			return CashFlowReport{}, ErrCashFlowOverflow
		}
		expected, ok := addCents(ac.opening, net)
		if !ok {
			return CashFlowReport{}, ErrCashFlowOverflow
		}
		row := CashFlowAccountRow{
			Account:      a,
			InflowCents:  ac.inflow,
			OutflowCents: ac.outflow,
			NetCents:     net,
			OpeningCents: ac.opening,
			ClosingCents: ac.closing,
			Activities:   make([]CashFlowActivity, 0, len(cashActivityOrder)),
		}
		for _, act := range cashActivityOrder {
			in, out := ac.actIn[act], ac.actOut[act]
			anet, ok := addCents(in, -out)
			if !ok {
				return CashFlowReport{}, ErrCashFlowOverflow
			}
			row.Activities = append(row.Activities, CashFlowActivity{
				Activity:     act,
				InflowCents:  in,
				OutflowCents: out,
				NetCents:     anet,
			})
		}
		report.Accounts = append(report.Accounts, row)
		if diff, ok := addCents(ac.closing, -expected); !ok || diff != 0 {
			if !ok {
				return CashFlowReport{}, ErrCashFlowOverflow
			}
			report.Discrepancies = append(report.Discrepancies, CashFlowDiscrepancy{
				Account:              a,
				OpeningCents:         ac.opening,
				NetFlowCents:         net,
				ExpectedClosingCents: expected,
				ActualClosingCents:   ac.closing,
				DifferenceCents:      diff,
			})
		}
		for _, pair := range [][2]*int64{
			{&report.OpeningCents, &ac.opening},
			{&report.InflowCents, &ac.inflow},
			{&report.OutflowCents, &ac.outflow},
			{&report.ClosingCents, &ac.closing},
		} {
			if err := addChecked(pair[0], *pair[1]); err != nil {
				return CashFlowReport{}, err
			}
		}
	}
	netFlow, ok := addCents(report.InflowCents, -report.OutflowCents)
	if !ok {
		return CashFlowReport{}, ErrCashFlowOverflow
	}
	report.NetFlowCents = netFlow
	for _, act := range cashActivityOrder {
		in, out := activityIn[act], activityOut[act]
		anet, ok := addCents(in, -out)
		if !ok {
			return CashFlowReport{}, ErrCashFlowOverflow
		}
		report.Activities = append(report.Activities, CashFlowActivity{
			Activity:     act,
			InflowCents:  in,
			OutflowCents: out,
			NetCents:     anet,
		})
	}
	sort.Slice(report.Discrepancies, func(i, j int) bool {
		return report.Discrepancies[i].Account < report.Discrepancies[j].Account
	})
	return report, nil
}

// WriteJSON encodes the report as indented JSON to w. The indented form
// is meant for humans: it is what the operator archives from
// GET /cash-flow.
func (r CashFlowReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// cashFlowCSVHeader is the column layout of the activity export: one row
// per (account, activity) with the gross flows behind it, plus the
// account's opening/closing balances so the reconciliation identity
// (closing == opening + inflow - outflow) is checkable per row.
// Deterministic: accounts are sorted, activities follow the fixed
// operating/investing/financing order.
var cashFlowCSVHeader = []string{
	"from", "to", "currency", "account", "activity",
	"inflow_cents", "outflow_cents", "net_cents",
	"opening_cents", "closing_cents",
}

// WriteCSV exports the per-(account, activity) cash-flow breakdown as
// CSV via encoding/csv.
func (r CashFlowReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(cashFlowCSVHeader); err != nil {
		return fmt.Errorf("ledger: cash flow csv: %w", err)
	}
	for _, row := range r.Accounts {
		for _, act := range row.Activities {
			line := []string{
				r.From,
				r.To,
				r.Currency,
				string(row.Account),
				string(act.Activity),
				strconv.FormatInt(act.InflowCents, 10),
				strconv.FormatInt(act.OutflowCents, 10),
				strconv.FormatInt(act.NetCents, 10),
				strconv.FormatInt(row.OpeningCents, 10),
				strconv.FormatInt(row.ClosingCents, 10),
			}
			if err := cw.Write(line); err != nil {
				return fmt.Errorf("ledger: cash flow csv: %w", err)
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
