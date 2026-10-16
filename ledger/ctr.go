package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Large-transaction reporting (LG-94, compliance / fintech).
//
// Currency Transaction Reports: the ledger aggregates every account's
// UTC-day inflow and outflow per currency, and when one side reaches the
// configured per-currency threshold a report entry is generated into the
// structured audit chain (op "large_tx_report", hash-sealed like every
// other audit event, see audit.go). The /metrics counter
// ledger_large_tx_reports_total and the reconciliation report's
// large_tx_reports section disclose what was generated.
//
// Thresholds are per currency in that currency's own cents — the CTR
// convention (a USD 10,000 line and a EUR 9,000 line are independent),
// not one converted number. Every report still discloses the converted
// amount in the ledger's reporting currency (default USD, see
// SetLargeTxReportCurrency), converted at the FX rate table's mid price
// effective at the end of the report day. A flow currency with no live
// rate is reported anyway with ConversionOK=false — a missing rate must
// never silently drop a reportable flow.
//
// Generation is read-only: it scans the journal under the read lock,
// books nothing, bumps nothing, and never blocks postings. Audit emission
// uses the same async non-blocking enqueue the reconcile job uses (see
// Reconcile). Regenerating a day replays the stored reports (marked
// Duplicate) instead of double-reporting: the audit chain receives each
// report exactly once.
//
// Inflow is the debit side, outflow the credit side — the same leg
// convention the daily outflow limit uses (see daily_limit.go). Rows
// pending dual-control review (see review.go) are excluded: they booked
// no money movement.

// Validation errors for large-transaction reporting.
var (
	// ErrInvalidLargeTxThreshold is returned when a reporting threshold
	// is not positive: a zero threshold would report every flow, which
	// is a configuration bug, not a reporting policy.
	ErrInvalidLargeTxThreshold = errors.New("ledger: large-transaction threshold must be positive")
)

// LargeTxDirection is the flow side a report covers.
type LargeTxDirection string

const (
	// LargeTxInflow reports money into the account (debit legs).
	LargeTxInflow LargeTxDirection = "inflow"
	// LargeTxOutflow reports money out of the account (credit legs).
	LargeTxOutflow LargeTxDirection = "outflow"
)

// LargeTxThreshold is one configured reporting threshold: an account's
// UTC-day inflow or outflow in Currency reaching ThresholdCents
// generates a report.
type LargeTxThreshold struct {
	Currency       string `json:"currency"`
	ThresholdCents int64  `json:"threshold_cents"`
}

// LargeTxReport is one generated large-transaction report entry.
type LargeTxReport struct {
	// Account is the reported account; Day is the UTC "2006-01-02" the
	// flow was booked on.
	Account  AccountID        `json:"account"`
	Day      string           `json:"day"`
	Currency string           `json:"currency"`
	// Direction is inflow (debit legs) or outflow (credit legs).
	Direction LargeTxDirection `json:"direction"`
	// AmountCents is the day's total flow on that side, in Currency.
	AmountCents int64 `json:"amount_cents"`
	// ThresholdCents is the threshold that fired, in Currency.
	ThresholdCents int64 `json:"threshold_cents"`
	// ReportCurrency is the currency the converted amount is disclosed
	// in; ConvertedCents is AmountCents converted at the FX table's mid
	// price effective at the end of the report day. When ConversionOK is
	// false no live rate existed and ConvertedCents is zero — the flow
	// is reported anyway, never dropped.
	ReportCurrency string `json:"report_currency"`
	ConvertedCents int64  `json:"converted_cents"`
	ConversionOK   bool   `json:"conversion_ok"`
	// RateVersion is the FX rate's EffectiveVersion when ConversionOK is
	// true (zero when Currency == ReportCurrency: no conversion needed).
	RateVersion uint64 `json:"rate_version,omitempty"`
	// GeneratedAt is when the report was first generated.
	GeneratedAt time.Time `json:"generated_at"`
	// Duplicate is true when this call replayed an already-generated
	// report instead of generating it.
	Duplicate bool `json:"duplicate,omitempty"`
}

// largeTxReportKey identifies one stored report: an (account, currency,
// day, direction) tuple reports at most once into the audit chain.
type largeTxReportKey struct {
	account   AccountID
	currency  string
	day       string
	direction LargeTxDirection
}

// WithLargeTxThresholds configures large-transaction reporting thresholds
// at construction (see SetLargeTxThreshold). Invalid currency codes and
// non-positive thresholds panic — fail-fast at construction, like the
// daily-limit option — so a misconfigured threshold can never silently
// under-report.
func WithLargeTxThresholds(m map[string]int64) Option {
	validated := make(map[string]int64, len(m))
	for currency, threshold := range m {
		cur, err := normalizeCurrency(currency)
		if err != nil {
			panic(fmt.Sprintf("ledger: invalid large-transaction threshold currency %q: %v", currency, err))
		}
		if threshold <= 0 {
			panic(fmt.Sprintf("ledger: large-transaction threshold for %s must be positive, got %d", cur, threshold))
		}
		validated[cur] = threshold
	}
	return func(l *Ledger) {
		for cur, threshold := range validated {
			l.largeTxThresholds[cur] = threshold
		}
	}
}

// WithLargeTxReportCurrency sets the reporting currency at construction
// (see SetLargeTxReportCurrency). Invalid codes panic — fail-fast, like
// every other currency option.
func WithLargeTxReportCurrency(currency string) Option {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid large-transaction report currency %q: %v", currency, err))
	}
	return func(l *Ledger) {
		l.largeTxReportCurrency = cur
	}
}

// SetLargeTxThreshold configures (or replaces) the reporting threshold
// for one currency: an account's UTC-day inflow or outflow in that
// currency reaching thresholdCents generates a report on the next
// GenerateLargeTxReports call. Thresholds are positive integer cents;
// zero or negative values are rejected with ErrInvalidLargeTxThreshold.
//
// Setting or clearing a threshold is structural, like SetDailyLimit: it
// does not bump the journal version. The change itself is audited
// (op "large_tx_threshold_set") — compliance config changes belong in
// the audit trail.
func (l *Ledger) SetLargeTxThreshold(currency string, thresholdCents int64) error {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if thresholdCents <= 0 {
		return ErrInvalidLargeTxThreshold
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.largeTxThresholds[cur] = thresholdCents
	l.emitAudit(AuditEvent{
		Op:            "large_tx_threshold_set",
		Actor:         "SetLargeTxThreshold",
		TraceID:       "ctr-threshold/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"currency":        cur,
			"threshold_cents": thresholdCents,
		},
	})
	return nil
}

// ClearLargeTxThreshold removes the reporting threshold for one currency.
// Clearing a threshold that was never set is a no-op. Like
// SetLargeTxThreshold, it is structural and audited.
func (l *Ledger) ClearLargeTxThreshold(currency string) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.largeTxThresholds[cur]; !ok {
		return
	}
	delete(l.largeTxThresholds, cur)
	l.emitAudit(AuditEvent{
		Op:            "large_tx_threshold_cleared",
		Actor:         "ClearLargeTxThreshold",
		TraceID:       "ctr-threshold/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"currency": cur},
	})
}

// LargeTxThreshold reports the configured reporting threshold for one
// currency, and whether a threshold is configured at all.
func (l *Ledger) LargeTxThreshold(currency string) (int64, bool) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return 0, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	threshold, ok := l.largeTxThresholds[cur]
	return threshold, ok
}

// SetLargeTxReportCurrency sets the currency converted report amounts are
// disclosed in (default USD). Invalid codes are rejected with the currency
// error. Structural and audited, like SetLargeTxThreshold.
func (l *Ledger) SetLargeTxReportCurrency(currency string) error {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.largeTxReportCurrency = cur
	l.emitAudit(AuditEvent{
		Op:            "large_tx_report_currency_set",
		Actor:         "SetLargeTxReportCurrency",
		TraceID:       "ctr-report-currency",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"report_currency": cur},
	})
	return nil
}

// LargeTxReportCurrency reports the currency converted report amounts
// are disclosed in.
func (l *Ledger) LargeTxReportCurrency() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.largeTxReportCurrency
}

// ParseLargeTxThresholds parses the LEDGER_LARGE_TX_THRESHOLDS
// environment variable into reporting threshold configs:
//
//	"<currency>:<thresholdCents>[,<currency>:<thresholdCents>...]"
//
// e.g. "USD:1000000,EUR:900000" reports USD flows at/above $10,000 and
// EUR flows at/above €9,000 per account-day side. Thresholds are integer
// cents and must be positive; currencies follow the usual ISO 4217
// validation. Parsing is strict — a malformed segment is an error, so a
// misconfigured deployment fails fast at startup instead of silently
// running without its compliance reporting.
func ParseLargeTxThresholds(raw string) ([]LargeTxThreshold, error) {
	fail := func(format string, args ...any) ([]LargeTxThreshold, error) {
		return nil, fmt.Errorf("ledger: invalid large-transaction thresholds %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []LargeTxThreshold
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 2 {
			return fail("segment %q must be \"<currency>:<thresholdCents>\"", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[0]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		threshold, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || threshold <= 0 {
			return fail("segment %q: threshold %q must be a positive integer (cents)", seg, parts[1])
		}
		out = append(out, LargeTxThreshold{Currency: currency, ThresholdCents: threshold})
	}
	return out, nil
}

// largeTxFlow accumulates one (account, currency) side's totals.
type largeTxFlow struct {
	inflow  int64
	outflow int64
}

// largeTxFlowKey keys the per-day aggregation.
type largeTxFlowKey struct {
	account  AccountID
	currency string
}

// GenerateLargeTxReports scans the journal for one UTC calendar day and
// generates a large-transaction (CTR) report for every (account,
// currency) whose inflow or outflow that day reaches the configured
// threshold for the currency. Reports sort by (account, currency,
// direction).
//
// The scan is read-only: it books nothing, bumps nothing, and the
// journal scan runs under the read lock — generating reports never
// blocks postings beyond a brief store-and-audit critical section. Each
// generated report is recorded in the structured audit chain
// (op "large_tx_report", hash-sealed like every other audit event).
// Regenerating a day replays the stored reports (Duplicate marks them)
// instead of double-reporting: the audit chain receives each report
// exactly once.
//
// day is "2006-01-02" UTC; empty means today. A malformed day is an
// error.
//
// The first generation for an (account, currency, day, direction) tuple
// wins; later generations the same day replay the stored row. Run
// generation as an end-of-day batch for complete daily totals.
func (l *Ledger) GenerateLargeTxReports(day string) ([]LargeTxReport, error) {
	if day == "" {
		day = utcDay(time.Now())
	}
	dayStart, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, fmt.Errorf("ledger: invalid report day %q: want YYYY-MM-DD", day)
	}
	// The conversion rate is the mid price effective at the end of the
	// report day: the report describes that day's flows at that day's
	// closing price, not at scan time.
	rateAt := dayStart.Add(24*time.Hour - time.Second)

	l.mu.RLock()
	flows := make(map[largeTxFlowKey]*largeTxFlow)
	for _, e := range l.entries {
		if e.PendingReview {
			// Pending dual-control review: booked but not effective —
			// no money moved, so nothing to report.
			continue
		}
		if utcDay(e.CreatedAt) != day {
			continue
		}
		cur := e.Currency
		if cur == "" {
			cur = DefaultCurrency
		}
		inKey := largeTxFlowKey{account: e.DebitAccount, currency: cur}
		f := flows[inKey]
		if f == nil {
			f = &largeTxFlow{}
			flows[inKey] = f
		}
		f.inflow = satAddInt64(f.inflow, e.AmountCents)
		outKey := largeTxFlowKey{account: e.CreditAccount, currency: cur}
		g := flows[outKey]
		if g == nil {
			g = &largeTxFlow{}
			flows[outKey] = g
		}
		g.outflow = satAddInt64(g.outflow, e.AmountCents)
	}

	// Build candidate reports under the read lock: the conversion reads
	// the FX rate table (pure read, see effectiveFXRateLocked). The scan
	// is a point-in-time snapshot — a posting that commits between the
	// scan and the store below is simply picked up by the next
	// generation.
	type candidate struct {
		key       largeTxReportKey
		amount    int64
		threshold int64
		rep       LargeTxReport
	}
	reportCurrency := l.largeTxReportCurrency
	var candidates []candidate
	now := time.Now().UTC()
	for k, f := range flows {
		threshold, ok := l.largeTxThresholds[k.currency]
		if !ok {
			continue
		}
		sides := []struct {
			direction LargeTxDirection
			amount    int64
		}{
			{LargeTxInflow, f.inflow},
			{LargeTxOutflow, f.outflow},
		}
		for _, side := range sides {
			if side.amount < threshold {
				continue
			}
			rep := LargeTxReport{
				Account:        k.account,
				Day:            day,
				Currency:       k.currency,
				Direction:      side.direction,
				AmountCents:    side.amount,
				ThresholdCents: threshold,
				ReportCurrency: reportCurrency,
				GeneratedAt:    now,
			}
			if k.currency == reportCurrency {
				rep.ConvertedCents = side.amount
				rep.ConversionOK = true
			} else if eff, err := l.effectiveFXRateLocked(fxPair{from: k.currency, to: reportCurrency}, "", rateAt); err == nil {
				// Reports convert at the mid price: the dealing side is
				// irrelevant to a compliance report, and the mid is the
				// table's reference price (see effectiveFXRateLocked).
				if converted, ok := fxConvertCents(side.amount, eff.MidNum, eff.MidDen); ok {
					rep.ConvertedCents = converted
					rep.ConversionOK = true
					rep.RateVersion = eff.EffectiveVersion
				}
			}
			// A missing/expired rate or an overflowing conversion leaves
			// ConversionOK false with ConvertedCents zero: the flow is
			// reported anyway, never silently dropped.
			candidates = append(candidates, candidate{
				key:       largeTxReportKey{account: k.account, currency: k.currency, day: day, direction: side.direction},
				amount:    side.amount,
				threshold: threshold,
				rep:       rep,
			})
		}
	}
	l.mu.RUnlock()

	// Phase two, under the write lock: dedupe against the stored reports,
	// store the new ones, and record them in the audit chain. The audit
	// enqueue never blocks (see emitAudit), so this never stalls
	// postings beyond the lock handoff.
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybePruneLargeTxReportsLocked(time.Now())
	out := make([]LargeTxReport, 0, len(candidates))
	for _, c := range candidates {
		if existing, ok := l.largeTxReports[c.key]; ok {
			existing.Duplicate = true
			out = append(out, existing)
			continue
		}
		l.largeTxReports[c.key] = c.rep
		l.emitAudit(AuditEvent{
			Op:            "large_tx_report",
			Actor:         "GenerateLargeTxReports",
			TraceID:       fmt.Sprintf("ctr/%s/%s/%s/%s", day, c.key.account, c.key.currency, c.key.direction),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{c.key.account},
			Details: map[string]any{
				"day":             day,
				"currency":        c.key.currency,
				"direction":       string(c.key.direction),
				"amount_cents":    c.amount,
				"threshold_cents": c.threshold,
				"report_currency": c.rep.ReportCurrency,
				"converted_cents": c.rep.ConvertedCents,
				"conversion_ok":   c.rep.ConversionOK,
				"rate_version":    c.rep.RateVersion,
			},
		})
		out = append(out, c.rep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Direction < out[j].Direction
	})
	return out, nil
}

// largeTxReportsForDayLocked returns the stored reports for one UTC day,
// sorted by (account, currency, direction), for the reconciliation
// report. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) largeTxReportsForDayLocked(day string) []LargeTxReport {
	var out []LargeTxReport
	for key, rep := range l.largeTxReports {
		if key.day == day {
			out = append(out, rep)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Direction < out[j].Direction
	})
	return out
}

// largeTxThresholdsLocked returns every configured reporting threshold,
// sorted by currency, for the disaster-recovery snapshot. Callers must
// hold l.mu; the read lock suffices.
func (l *Ledger) largeTxThresholdsLocked() []LargeTxThreshold {
	out := make([]LargeTxThreshold, 0, len(l.largeTxThresholds))
	for cur, threshold := range l.largeTxThresholds {
		out = append(out, LargeTxThreshold{Currency: cur, ThresholdCents: threshold})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// maybePruneLargeTxReportsLocked drops stored reports for UTC days older
// than yesterday: a new generation can only reference its own day's rows,
// and the audit chain already carries every generated report, so older
// rows are dead weight. Pruning runs at most once per pruneInterval —
// amortized O(1) per generation, like the idempotency-key sweep.
// Callers must hold l.mu.
func (l *Ledger) maybePruneLargeTxReportsLocked(now time.Time) {
	if now.Sub(l.lastLargeTxPrune) < l.pruneInterval {
		return
	}
	l.lastLargeTxPrune = now
	cutoff := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for k := range l.largeTxReports {
		if k.day < cutoff {
			delete(l.largeTxReports, k)
		}
	}
}
