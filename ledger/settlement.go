package ledger

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Merchant settlement view (LG-43).
//
// A settlement view answers the end-of-day question a payments operator
// asks: for each merchant account, how much money moved today, in which
// currency, and does the day's net match what the settlement file from
// the payment provider says it should be?
//
// Merchant dimension: account. Journal entries always touch exactly two
// accounts, so the (account, UTC calendar day, currency) aggregation is
// the honest settlement primitive: total debits (money in to the
// merchant) and total credits (money out) booked that day, and the net
// movement. There is no tag/metadata dimension on journal entries, so
// the channel dimension (e.g. "alipay", "wechat-pay", "card") is stable
// structural config: SetSettlementChannel maps an account to its
// channel, exactly like frozen accounts or daily limits. Unmapped
// accounts carry an empty channel.
//
// Reconciliation alerts: the operator passes the day's expected nets
// (taken from the provider's settlement file) and opt-in tolerances per
// call. Any (account, currency) whose actual day-net differs from the
// expected net beyond its tolerance lands in the report's Mismatches as
// an alert item. Providing an expectation with no tolerance means
// zero-tolerance: any nonzero difference alerts. Thresholds are per-call
// inputs, not stored config, because settlement expectations come from an
// external file that changes every day.
//
// Entries marked PendingReview (see review.go) are excluded from the
// aggregation: they are booked but not effective — balances, totals, and
// the trial balance never see them — so a settlement view built on them
// would lie about the money that actually moved.
//
// Exports: CSV via encoding/csv, and a SQL text dump (CREATE TABLE +
// INSERT statements) that a real sqlite3 imports — the Go standard
// library has no SQLite driver, so there is deliberately no .db binary.

// Settlement errors.
var (
	ErrInvalidSettlementDay       = errors.New("ledger: settlement day must be YYYY-MM-DD")
	ErrEmptySettlementAccount     = errors.New("ledger: settlement channel account must not be empty")
	ErrInvalidSettlementTolerance = errors.New("ledger: settlement tolerance must have abs_cents >= 0 and 0 <= bps <= 10000")
)

// maxSettlementChannelLen caps a channel label. Long enough for
// "wechat-pay-cross-border", short enough to keep snapshot config and
// export rows bounded.
const maxSettlementChannelLen = 64

// SettlementChannel maps one account to its settlement channel (the
// payment rail the merchant settles through). Structural config: setting
// or clearing a mapping does not bump the ledger version, survives
// disaster-recovery snapshots, and is listed on the settlement report.
type SettlementChannel struct {
	Account AccountID `json:"account"`
	Channel string    `json:"channel"`
}

// SettlementExpectationKey names one expected day-net: an account's
// settlement in one currency.
type SettlementExpectationKey struct {
	Account  AccountID `json:"account"`
	Currency string    `json:"currency"`
}

// SettlementTolerance is the opt-in alert threshold for one expectation:
// alert when |actual - expected| exceeds AbsCents, or when the relative
// difference exceeds Bps basis points of |expected|. Zero values mean
// zero tolerance.
type SettlementTolerance struct {
	AbsCents int64 `json:"abs_cents"`
	Bps      int64 `json:"bps"`
}

// SettlementOptions drives one settlement report.
type SettlementOptions struct {
	// Day is the UTC calendar day to aggregate, "YYYY-MM-DD". Empty
	// means today (UTC) as of now.
	Day string
	// ExpectedNets maps (account, currency) to the day-net the payment
	// provider's settlement file says it should be, in cents. Every
	// expectation is checked; missing ledger activity counts as an
	// actual net of zero.
	ExpectedNets map[SettlementExpectationKey]int64
	// Tolerances maps (account, currency) to the opt-in alert
	// threshold. Expectations without a tolerance row alert on any
	// nonzero difference.
	Tolerances map[SettlementExpectationKey]SettlementTolerance
}

// SettlementCell is one (account, UTC day, currency) aggregation row:
// the day's total debits, total credits, and net movement, plus the
// journal entries the row was built from so an operator can drill down.
type SettlementCell struct {
	Day         string    `json:"day"`
	Account     AccountID `json:"account"`
	Channel     string    `json:"channel,omitempty"`
	Currency    string    `json:"currency"`
	DebitCents  int64     `json:"debit_cents"`
	CreditCents int64     `json:"credit_cents"`
	// NetCents is DebitCents - CreditCents: the account's net balance
	// movement for the day in this currency.
	NetCents int64 `json:"net_cents"`
	// EntryCount is the number of effective journal entries that
	// touched the account that day; EntryIDs names them, sorted, for
	// drill-down into the journal.
	EntryCount int      `json:"entry_count"`
	EntryIDs   []string `json:"entry_ids"`
}

// SettlementMismatch is one settlement alert: the day's actual net did
// not match the expected net within the configured tolerance.
type SettlementMismatch struct {
	Day         string    `json:"day"`
	Account     AccountID `json:"account"`
	Channel     string    `json:"channel,omitempty"`
	Currency    string    `json:"currency"`
	ExpectedNet int64     `json:"expected_net_cents"`
	ActualNet   int64     `json:"actual_net_cents"`
	// DifferenceCents is ActualNet - ExpectedNet, signed: positive
	// means the ledger moved more than the settlement file says.
	DifferenceCents int64 `json:"difference_cents"`
	// ToleranceAbsCents and ToleranceBps echo the tolerance that was
	// exceeded, so the report is self-describing.
	ToleranceAbsCents int64 `json:"tolerance_abs_cents"`
	ToleranceBps      int64 `json:"tolerance_bps"`
}

// SettlementReport is the day-end settlement view: the per-merchant
// aggregation plus the alert list. Read-only: building it changes no
// accounting state.
type SettlementReport struct {
	Day         string               `json:"day"`
	GeneratedAt time.Time            `json:"generated_at"`
	Version     uint64               `json:"version"`
	Cells       []SettlementCell     `json:"cells"`
	Mismatches  []SettlementMismatch `json:"mismatches"`
	// Channels lists the configured account->channel mappings at
	// report time, sorted by account, so the report is self-describing
	// about which dimension each row was rolled into.
	Channels []SettlementChannel `json:"channels"`
}

// parseSettlementDay validates a "YYYY-MM-DD" UTC calendar day. A
// round-trip format check rejects values like "2026-1-5" that
// time.Parse would otherwise accept for the "01" layout verb.
func parseSettlementDay(day string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil || t.Format("2006-01-02") != day {
		return time.Time{}, fmt.Errorf("%w: %q", ErrInvalidSettlementDay, day)
	}
	return t, nil
}

// SetSettlementChannel maps an account to its settlement channel (the
// payment rail the merchant settles through). An empty channel clears
// the mapping. Structural config like frozen accounts: no version bump,
// survives snapshots. Channel labels are capped at 64 characters.
func (l *Ledger) SetSettlementChannel(a AccountID, channel string) error {
	if a == "" {
		return ErrEmptySettlementAccount
	}
	if len(channel) > maxSettlementChannelLen {
		return fmt.Errorf("ledger: settlement channel label must be at most %d characters", maxSettlementChannelLen)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if channel == "" {
		delete(l.settlementChannels, a)
		return nil
	}
	l.settlementChannels[a] = channel
	return nil
}

// SettlementChannel reports the configured settlement channel for an
// account and whether one is configured.
func (l *Ledger) SettlementChannel(a AccountID) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ch, ok := l.settlementChannels[a]
	return ch, ok
}

// settlementChannelsLocked lists the configured mappings, sorted by
// account. Callers must hold l.mu.
func (l *Ledger) settlementChannelsLocked() []SettlementChannel {
	out := make([]SettlementChannel, 0, len(l.settlementChannels))
	for a, ch := range l.settlementChannels {
		out = append(out, SettlementChannel{Account: a, Channel: ch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// WithSettlementChannel maps an account to its settlement channel at
// construction time (see SetSettlementChannel). An empty account or an
// over-long label panics — fail-fast at construction, like the fee
// schedule and daily limits.
func WithSettlementChannel(a AccountID, channel string) Option {
	if a == "" {
		panic("ledger: settlement channel account must not be empty")
	}
	if len(channel) > maxSettlementChannelLen {
		panic(fmt.Sprintf("ledger: settlement channel label must be at most %d characters", maxSettlementChannelLen))
	}
	return func(l *Ledger) {
		if channel == "" {
			delete(l.settlementChannels, a)
			return
		}
		l.settlementChannels[a] = channel
	}
}

// normalizeSettlementOptions validates the report inputs: the day, the
// expectation currencies, and the tolerance bounds. It returns the
// normalized expectations and tolerances keyed by (account, currency)
// with canonical currency codes.
func normalizeSettlementOptions(opts SettlementOptions) (day time.Time, expected map[settlementKey]int64, tolerances map[settlementKey]SettlementTolerance, err error) {
	d := opts.Day
	if d == "" {
		d = utcDay(time.Now().UTC())
	}
	if day, err = parseSettlementDay(d); err != nil {
		return time.Time{}, nil, nil, err
	}
	expected = make(map[settlementKey]int64, len(opts.ExpectedNets))
	for k, v := range opts.ExpectedNets {
		cur, cerr := normalizeCurrency(k.Currency)
		if cerr != nil {
			return time.Time{}, nil, nil, fmt.Errorf("%w: settlement expectation currency %q", ErrInvalidCurrency, k.Currency)
		}
		expected[settlementKey{account: k.Account, currency: cur}] = v
	}
	tolerances = make(map[settlementKey]SettlementTolerance, len(opts.Tolerances))
	for k, tol := range opts.Tolerances {
		cur, cerr := normalizeCurrency(k.Currency)
		if cerr != nil {
			return time.Time{}, nil, nil, fmt.Errorf("%w: settlement tolerance currency %q", ErrInvalidCurrency, k.Currency)
		}
		if tol.AbsCents < 0 || tol.Bps < 0 || tol.Bps > 10000 {
			return time.Time{}, nil, nil, fmt.Errorf("%w: account %q (%s)", ErrInvalidSettlementTolerance, k.Account, cur)
		}
		tolerances[settlementKey{account: k.Account, currency: cur}] = tol
	}
	return day, expected, tolerances, nil
}

// settlementKey is the unexported aggregation key: (account, currency).
type settlementKey struct {
	account  AccountID
	currency string
}

// Settlement builds the end-of-day settlement view for one UTC calendar
// day: per-(account, currency) debit/credit/net totals from effective
// journal entries whose CreatedAt falls in [day 00:00:00, next day
// 00:00:00) UTC, plus the mismatch alerts for the supplied expectations.
//
// The scan holds the read lock for its whole duration, so the report
// describes one consistent point in time: a concurrent Post can never
// slip between the aggregation and the alert evaluation. Like
// Reconcile, building the report is read-only, but it records one
// "settlement_report" audit event — the durable record that the day-end
// view ran, against which ledger state, and how many alerts it raised.
func (l *Ledger) Settlement(now time.Time, opts SettlementOptions) (SettlementReport, error) {
	dayStart, expected, tolerances, err := normalizeSettlementOptions(opts)
	if err != nil {
		return SettlementReport{}, err
	}
	dayEnd := dayStart.Add(24 * time.Hour)
	dayLabel := dayStart.Format("2006-01-02")

	l.mu.RLock()
	defer l.mu.RUnlock()

	type accum struct {
		debit, credit int64
		ids           []string
	}
	byKey := make(map[settlementKey]*accum)
	for _, e := range l.entries {
		// Pending-review rows are booked but not effective: balances
		// and totals never see them, so neither does the settlement
		// view. A day-net built on them would lie about moved money.
		if e.PendingReview {
			continue
		}
		if e.CreatedAt.Before(dayStart) || !e.CreatedAt.Before(dayEnd) {
			continue
		}
		dk := settlementKey{account: e.DebitAccount, currency: e.Currency}
		a := byKey[dk]
		if a == nil {
			a = &accum{}
			byKey[dk] = a
		}
		a.debit += e.AmountCents
		a.ids = append(a.ids, e.ID)
		ck := settlementKey{account: e.CreditAccount, currency: e.Currency}
		a = byKey[ck]
		if a == nil {
			a = &accum{}
			byKey[ck] = a
		}
		a.credit += e.AmountCents
		a.ids = append(a.ids, e.ID)
	}

	report := SettlementReport{
		Day:         dayLabel,
		GeneratedAt: now,
		Version:     l.version,
		Cells:       make([]SettlementCell, 0, len(byKey)),
		Mismatches:  make([]SettlementMismatch, 0),
		Channels:    l.settlementChannelsLocked(),
	}
	nets := make(map[settlementKey]int64, len(byKey))
	for k, a := range byKey {
		sort.Strings(a.ids)
		net := a.debit - a.credit
		nets[k] = net
		report.Cells = append(report.Cells, SettlementCell{
			Day:         dayLabel,
			Account:     k.account,
			Channel:     l.settlementChannels[k.account],
			Currency:    k.currency,
			DebitCents:  a.debit,
			CreditCents: a.credit,
			NetCents:    net,
			EntryCount:  len(a.ids),
			EntryIDs:    a.ids,
		})
	}
	sort.Slice(report.Cells, func(i, j int) bool {
		if report.Cells[i].Account != report.Cells[j].Account {
			return report.Cells[i].Account < report.Cells[j].Account
		}
		return report.Cells[i].Currency < report.Cells[j].Currency
	})

	// Alert evaluation: every expectation is checked, including accounts
	// with no activity that day (actual net zero). An expectation with no
	// tolerance row alerts on any nonzero difference — zero tolerance is
	// the default because a silent default tolerance would hide real
	// settlement breaks.
	for k, want := range expected {
		actual := nets[k] // zero when the account had no activity
		diff := actual - want
		if diff == 0 {
			continue
		}
		tol := tolerances[k] // zero value when no row: strict
		if absInt64(diff) <= tol.AbsCents {
			continue
		}
		if tol.Bps > 0 && want != 0 && absInt64(diff)*10000 <= tol.Bps*absInt64(want) {
			continue
		}
		report.Mismatches = append(report.Mismatches, SettlementMismatch{
			Day:               dayLabel,
			Account:           k.account,
			Channel:           l.settlementChannels[k.account],
			Currency:          k.currency,
			ExpectedNet:       want,
			ActualNet:         actual,
			DifferenceCents:   diff,
			ToleranceAbsCents: tol.AbsCents,
			ToleranceBps:      tol.Bps,
		})
	}
	sort.Slice(report.Mismatches, func(i, j int) bool {
		if report.Mismatches[i].Account != report.Mismatches[j].Account {
			return report.Mismatches[i].Account < report.Mismatches[j].Account
		}
		return report.Mismatches[i].Currency < report.Mismatches[j].Currency
	})

	// Read-only, so the version brackets are identical — the event is the
	// durable record that the settlement view ran, like reconcile's.
	// Emitted under the read lock; the enqueue never blocks.
	l.emitAudit(AuditEvent{
		Op:            "settlement_report",
		Actor:         "Settlement",
		TraceID:       fmt.Sprintf("settlement@%d", report.Version),
		VersionBefore: report.Version,
		VersionAfter:  report.Version,
		Details: map[string]any{
			"day":        dayLabel,
			"cells":      len(report.Cells),
			"mismatches": len(report.Mismatches),
		},
	})
	return report, nil
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// WriteJSON encodes the report as indented JSON to w. The indented form is
// meant for humans: it is what the operator archives from GET /settlement.
func (r SettlementReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// settlementCSVHeader is the column layout of the cells export: one row
// per (account, day, currency) with the journal entries behind it.
var settlementCSVHeader = []string{
	"day", "account", "channel", "currency",
	"debit_cents", "credit_cents", "net_cents",
	"entry_count", "entry_ids",
}

// WriteCSV exports the settlement cells — the per-merchant day view —
// as CSV via encoding/csv. Entry IDs are semicolon-joined in one field
// so the row stays one line and stays parseable. Deterministic: cells
// are already sorted by (account, currency).
func (r SettlementReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(settlementCSVHeader); err != nil {
		return fmt.Errorf("ledger: settlement csv: %w", err)
	}
	for _, c := range r.Cells {
		row := []string{
			c.Day,
			string(c.Account),
			c.Channel,
			c.Currency,
			strconv.FormatInt(c.DebitCents, 10),
			strconv.FormatInt(c.CreditCents, 10),
			strconv.FormatInt(c.NetCents, 10),
			strconv.Itoa(c.EntryCount),
			strings.Join(c.EntryIDs, ";"),
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("ledger: settlement csv: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// settlementMismatchCSVHeader is the column layout of the alert export.
var settlementMismatchCSVHeader = []string{
	"day", "account", "channel", "currency",
	"expected_net_cents", "actual_net_cents", "difference_cents",
	"tolerance_abs_cents", "tolerance_bps",
}

// WriteMismatchesCSV exports the settlement alert list as CSV: the
// rows an operator works through at day end. Deterministic: mismatches
// are already sorted by (account, currency).
func (r SettlementReport) WriteMismatchesCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(settlementMismatchCSVHeader); err != nil {
		return fmt.Errorf("ledger: settlement mismatches csv: %w", err)
	}
	for _, m := range r.Mismatches {
		row := []string{
			m.Day,
			string(m.Account),
			m.Channel,
			m.Currency,
			strconv.FormatInt(m.ExpectedNet, 10),
			strconv.FormatInt(m.ActualNet, 10),
			strconv.FormatInt(m.DifferenceCents, 10),
			strconv.FormatInt(m.ToleranceAbsCents, 10),
			strconv.FormatInt(m.ToleranceBps, 10),
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("ledger: settlement mismatches csv: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// sqlQuote renders a SQL string literal: single quotes are doubled, the
// only escaping a SQL text dump needs.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// WriteSQL exports the report as a SQL text dump: CREATE TABLE +
// INSERT statements for the cells and the mismatch alerts, wrapped in a
// transaction.
//
// This is deliberately a .sql text file, not a SQLite .db binary: the
// Go standard library has no SQLite driver, and shipping a hand-rolled
// binary page format would be fake. Import it for real with:
//
//	sqlite3 settlement.db < settlement.sql
func (r SettlementReport) WriteSQL(w io.Writer) error {
	var sb strings.Builder
	sb.WriteString("-- ledger-api-go settlement export\n")
	sb.WriteString("-- Day: " + r.Day + "  Generated: " + r.GeneratedAt.UTC().Format(time.RFC3339) + "\n")
	sb.WriteString("-- This is a SQL text dump (CREATE TABLE + INSERT), NOT a SQLite .db binary.\n")
	sb.WriteString("-- Import with: sqlite3 settlement.db < settlement.sql\n")
	sb.WriteString("BEGIN TRANSACTION;\n")
	sb.WriteString("CREATE TABLE IF NOT EXISTS settlement_cells (\n")
	sb.WriteString("  day TEXT NOT NULL,\n")
	sb.WriteString("  account TEXT NOT NULL,\n")
	sb.WriteString("  channel TEXT NOT NULL,\n")
	sb.WriteString("  currency TEXT NOT NULL,\n")
	sb.WriteString("  debit_cents INTEGER NOT NULL,\n")
	sb.WriteString("  credit_cents INTEGER NOT NULL,\n")
	sb.WriteString("  net_cents INTEGER NOT NULL,\n")
	sb.WriteString("  entry_count INTEGER NOT NULL,\n")
	sb.WriteString("  entry_ids TEXT NOT NULL\n")
	sb.WriteString(");\n")
	sb.WriteString("CREATE TABLE IF NOT EXISTS settlement_mismatches (\n")
	sb.WriteString("  day TEXT NOT NULL,\n")
	sb.WriteString("  account TEXT NOT NULL,\n")
	sb.WriteString("  channel TEXT NOT NULL,\n")
	sb.WriteString("  currency TEXT NOT NULL,\n")
	sb.WriteString("  expected_net_cents INTEGER NOT NULL,\n")
	sb.WriteString("  actual_net_cents INTEGER NOT NULL,\n")
	sb.WriteString("  difference_cents INTEGER NOT NULL,\n")
	sb.WriteString("  tolerance_abs_cents INTEGER NOT NULL,\n")
	sb.WriteString("  tolerance_bps INTEGER NOT NULL\n")
	sb.WriteString(");\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&sb, "INSERT INTO settlement_cells VALUES (%s,%s,%s,%s,%d,%d,%d,%d,%s);\n",
			sqlQuote(c.Day), sqlQuote(string(c.Account)), sqlQuote(c.Channel), sqlQuote(c.Currency),
			c.DebitCents, c.CreditCents, c.NetCents, c.EntryCount,
			sqlQuote(strings.Join(c.EntryIDs, ";")))
	}
	for _, m := range r.Mismatches {
		fmt.Fprintf(&sb, "INSERT INTO settlement_mismatches VALUES (%s,%s,%s,%s,%d,%d,%d,%d,%d);\n",
			sqlQuote(m.Day), sqlQuote(string(m.Account)), sqlQuote(m.Channel), sqlQuote(m.Currency),
			m.ExpectedNet, m.ActualNet, m.DifferenceCents,
			m.ToleranceAbsCents, m.ToleranceBps)
	}
	sb.WriteString("COMMIT;\n")
	_, err := io.WriteString(w, sb.String())
	if err != nil {
		return fmt.Errorf("ledger: settlement sql: %w", err)
	}
	return nil
}
