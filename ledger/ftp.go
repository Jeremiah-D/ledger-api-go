package ledger

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Funds transfer pricing (FTP): charging business units for the
// funding they consume and paying them for the funding they provide.
//
// A bank's treasury is the internal market-maker: business units with
// a funding shortfall (a gap) borrow from the pool and pay the FTP
// rate; units with surplus funding lend to the pool and earn it. This
// module prices those internal transfers off a per-currency rate curve
// and books them as ordinary journal entries, so the FTP P&L is
// auditable exactly like customer money.
//
// The curve: one annualized rate curve per currency, a list of
// (tenorMonths, rateBps) buckets. Rates between buckets are linearly
// interpolated; tenors outside the curve clamp to the nearest endpoint
// (flat extrapolation — the operator extends the curve instead of
// trusting extrapolation). Every SetFtpCurve call bumps the currency's
// curve version, stamps SetAt, and emits an "ftp_curve_set" audit
// event: the version on each charge's audit details names the exact
// curve that priced it, so historical charges stay reproducible.
//
// A charge: ChargeFtp books one entry per (account, currency, tenor,
// calendar day) — debit the BU FTP expense and credit the treasury
// pool for a gap (the unit pays for funding), debit the pool and
// credit the BU FTP revenue for a surplus (the pool pays the unit).
// The entry ID is deterministic —
// "ftp/<currency>/<account>/<tenorMonths>/<YYYY-MM-DD>" (UTC date) —
// so re-running the day's charge returns the original entry with
// duplicate == true. FtpPreview computes the same price with zero
// side effects: no entry, no version bump, no audit event.
//
// Charges are system adjusting entries: they run the full Post gate
// order (field validation, idempotency replay, chart direction,
// frozen, overdraft, daily-limit, period) through the system path
// (postSystemLocked) — they never fire the journal rules engine and
// never enter dual-control review, like accruals (see accrual.go).
// The charge amount is floor(|gap| * rateBps / 10000 * tenorMonths /
// 12) cents, computed with the overflow-safe basis-point helper the
// transfer fee policy uses (see policyFeeCents); a dust charge that
// rounds to zero cents books nothing (ErrNonPositiveAmount).
//
// The internal counterparties (pool, expense, revenue accounts) are
// structural config via WithFtpAccounts and round-trip through
// snapshots, like the FX accounts. Curves round-trip too, with their
// versions. Reconcile discloses per-currency FTP totals derived from
// the journal's "ftp:charge" rows — the same legs the audit chain and
// trial balances carry, so the section can never disagree with the
// books (the LG-93 precedent).

// FtpTenorPoint is one bucket of an FTP curve: the annualized rate in
// basis points applying at exactly TenorMonths months.
type FtpTenorPoint struct {
	TenorMonths int64 `json:"tenor_months"`
	RateBps     int64 `json:"rate_bps"`
}

// FtpCurve is the versioned FTP rate curve for one currency. Points is
// sorted ascending by TenorMonths. Version counts SetFtpCurve calls for
// the currency, starting at 1; SetAt is the last set time.
type FtpCurve struct {
	Currency string          `json:"currency"`
	Points   []FtpTenorPoint `json:"points"`
	Version  uint64          `json:"version"`
	SetAt    time.Time       `json:"set_at"`
}

// FtpCurveSnapshot is the snapshot form of one currency's curve.
type FtpCurveSnapshot struct {
	Currency string          `json:"currency"`
	Points   []FtpTenorPoint `json:"points"`
	Version  uint64          `json:"version"`
	SetAt    time.Time       `json:"set_at"`
}

// FtpAccounts names the internal counterparties of every FTP charge:
// the treasury pool on one side, and the business-unit FTP expense
// (gap charges) and revenue (surplus payouts) accounts on the other.
type FtpAccounts struct {
	PoolAccount    AccountID `json:"pool_account"`
	ExpenseAccount AccountID `json:"expense_account"`
	RevenueAccount AccountID `json:"revenue_account"`
}

// FtpDirection names which way the internal transfer flows.
type FtpDirection string

const (
	// FtpDirectionCost: the business unit has a funding gap and pays
	// the pool — debit FTP expense, credit pool.
	FtpDirectionCost FtpDirection = "cost"
	// FtpDirectionRevenue: the business unit has surplus funding and
	// the pool pays it — debit pool, credit FTP revenue.
	FtpDirectionRevenue FtpDirection = "revenue"
)

// FtpPreview is the read-only price of one FTP charge: the same
// numbers ChargeFtp would book, computed with zero side effects.
type FtpPreview struct {
	Currency     string       `json:"currency"`
	Account      AccountID    `json:"account"`
	TenorMonths  int64        `json:"tenor_months"`
	GapCents     int64        `json:"gap_cents"`
	RateBps      int64        `json:"rate_bps"`
	CurveVersion uint64       `json:"curve_version"`
	Direction    FtpDirection `json:"direction"`
	ChargeCents  int64        `json:"charge_cents"`
}

// FtpTotals is the per-currency rollup of booked FTP charges, derived
// from the journal (see ftpTotalsLocked), disclosed by Reconcile.
type FtpTotals struct {
	Currency string `json:"currency"`
	// CostCents is the total the business units paid the pool for
	// funding gaps (debit FTP expense / credit pool legs).
	CostCents int64 `json:"cost_cents"`
	// RevenueCents is the total the pool paid the business units for
	// surplus funding (debit pool / credit FTP revenue legs).
	RevenueCents int64 `json:"revenue_cents"`
	// Charges is the number of booked FTP charge entries.
	Charges int64 `json:"charges"`
}

// FTP validation errors.
var (
	// ErrInvalidFtpCurve is returned by SetFtpCurve when the curve is
	// empty, has a non-positive or out-of-range tenor, a negative or
	// absurd rate, duplicate tenors, or a bad currency code.
	ErrInvalidFtpCurve = errors.New("ledger: invalid FTP curve")
	// ErrFtpCurveNotFound is returned when no curve is configured for
	// the requested currency.
	ErrFtpCurveNotFound = errors.New("ledger: no FTP curve for currency")
	// ErrFtpAccountsNotConfigured is returned when an FTP charge or
	// preview is attempted before WithFtpAccounts named all three
	// internal counterparties.
	ErrFtpAccountsNotConfigured = errors.New("ledger: FTP accounts not configured")
	// ErrInvalidFtpAccounts is returned by WithFtpAccounts when an
	// account is empty, duplicated across roles, or contains '/'.
	ErrInvalidFtpAccounts = errors.New("ledger: invalid FTP accounts")
	// ErrFtpZeroGap is returned when the funding gap/surplus is zero:
	// there is nothing to price, so no charge is booked.
	ErrFtpZeroGap = errors.New("ledger: FTP funding gap is zero")
	// ErrFtpTenorOutOfRange is returned when tenorMonths is outside
	// 1..1200 months: FTP tenors are bounded (100 years is the sanity
	// cap) so the charge math stays overflow-safe.
	ErrFtpTenorOutOfRange = errors.New("ledger: FTP tenor must be between 1 and 1200 months")
	// ErrFtpChargeOverflow is returned when the priced charge does not
	// fit int64 cents — a data error, not a wrappable number.
	ErrFtpChargeOverflow = errors.New("ledger: FTP charge overflows int64")
)

// maxFtpRateBps caps a curve rate at 1000%/yr: above that is a
// configuration bug, not a price.
const maxFtpRateBps = 100000

// maxFtpTenorMonths caps a pricing tenor at 100 years.
const maxFtpTenorMonths = 1200

// WithFtpAccounts names the internal counterparties of every FTP
// charge. All three accounts must be non-empty, distinct, and
// slash-free; anything else panics — structural misconfiguration is a
// programmer error, caught at construction like the other With*
// options. The accounts round-trip through snapshots.
func WithFtpAccounts(a FtpAccounts) Option {
	if err := checkFtpAccounts(a); err != nil {
		panic(err)
	}
	return func(l *Ledger) {
		l.ftpAccounts = a
	}
}

// checkFtpAccounts validates the three internal counterparty accounts.
func checkFtpAccounts(a FtpAccounts) error {
	for _, acct := range []AccountID{a.PoolAccount, a.ExpenseAccount, a.RevenueAccount} {
		if acct == "" || strings.Contains(string(acct), "/") {
			return ErrInvalidFtpAccounts
		}
	}
	if a.PoolAccount == a.ExpenseAccount || a.PoolAccount == a.RevenueAccount ||
		a.ExpenseAccount == a.RevenueAccount {
		return ErrInvalidFtpAccounts
	}
	return nil
}

// ftpAccountsConfiguredLocked reports whether the three internal
// counterparties are all named. Callers must hold l.mu.
func (l *Ledger) ftpAccountsConfiguredLocked() bool {
	return l.ftpAccounts.PoolAccount != "" &&
		l.ftpAccounts.ExpenseAccount != "" &&
		l.ftpAccounts.RevenueAccount != ""
}

// SetFtpCurve installs (or replaces) the FTP rate curve for one
// currency. Points are stored sorted ascending by tenor; the currency's
// version bumps by one and SetAt is stamped. Validation: at least one
// point, tenors in 1..1200 strictly increasing, rates in
// 0..100000 bps, currency a valid ISO 4217 code.
//
// Setting a curve is structural config: it does not bump the ledger
// version and it survives ExportSnapshot/ImportSnapshot with its
// version. Every set emits an "ftp_curve_set" audit event carrying the
// new version and points, so the audit trail shows exactly which curve
// priced every historical charge.
func (l *Ledger) SetFtpCurve(currency string, points []FtpTenorPoint) (FtpCurve, error) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return FtpCurve{}, ErrInvalidFtpCurve
	}
	sorted, err := normalizeFtpPoints(points)
	if err != nil {
		return FtpCurve{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := l.ftpCurves[cur]
	version := uint64(1)
	if prev != nil {
		version = prev.Version + 1
	}
	c := &FtpCurve{
		Currency: cur,
		Points:   sorted,
		Version:  version,
		SetAt:    time.Now().UTC(),
	}
	if l.ftpCurves == nil {
		l.ftpCurves = make(map[string]*FtpCurve)
	}
	l.ftpCurves[cur] = c
	pts := make([]map[string]any, 0, len(sorted))
	for _, p := range sorted {
		pts = append(pts, map[string]any{"tenor_months": p.TenorMonths, "rate_bps": p.RateBps})
	}
	l.emitAudit(AuditEvent{
		Op:            "ftp_curve_set",
		Actor:         "SetFtpCurve",
		TraceID:       "ftp-curve/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"currency": cur,
			"version":  version,
			"points":   pts,
		},
	})
	return *c, nil
}

// normalizeFtpPoints validates and sorts a curve's points.
func normalizeFtpPoints(points []FtpTenorPoint) ([]FtpTenorPoint, error) {
	if len(points) == 0 {
		return nil, ErrInvalidFtpCurve
	}
	sorted := make([]FtpTenorPoint, len(points))
	copy(sorted, points)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TenorMonths < sorted[j].TenorMonths })
	for i, p := range sorted {
		if p.TenorMonths < 1 || p.TenorMonths > maxFtpTenorMonths {
			return nil, ErrInvalidFtpCurve
		}
		if p.RateBps < 0 || p.RateBps > maxFtpRateBps {
			return nil, ErrInvalidFtpCurve
		}
		if i > 0 && sorted[i].TenorMonths == sorted[i-1].TenorMonths {
			return nil, ErrInvalidFtpCurve
		}
	}
	return sorted, nil
}

// FtpCurveFor returns the current curve for a currency, or false when
// none is configured.
func (l *Ledger) FtpCurveFor(currency string) (FtpCurve, bool) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return FtpCurve{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.ftpCurves[cur]
	if !ok {
		return FtpCurve{}, false
	}
	out := *c
	out.Points = append([]FtpTenorPoint(nil), c.Points...)
	return out, true
}

// FtpRateBps returns the annualized FTP rate in basis points for
// (currency, tenorMonths): the curve's rate at the tenor, linearly
// interpolated between bracketing buckets, clamped to the nearest
// endpoint outside the curve. It also returns the curve version that
// priced the rate.
func (l *Ledger) FtpRateBps(currency string, tenorMonths int64) (rateBps int64, version uint64, err error) {
	if tenorMonths < 1 || tenorMonths > maxFtpTenorMonths {
		return 0, 0, ErrFtpTenorOutOfRange
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return 0, 0, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.ftpCurves[cur]
	if !ok {
		return 0, 0, ErrFtpCurveNotFound
	}
	return interpolateFtpRate(c.Points, tenorMonths), c.Version, nil
}

// interpolateFtpRate prices one tenor off a sorted curve: exact
// buckets return their rate, in-between tenors interpolate linearly,
// and out-of-range tenors clamp to the nearest endpoint (flat
// extrapolation — the operator extends the curve instead of trusting
// extrapolation). Integer math truncates toward zero on negative
// slopes; bps granularity makes the bias sub-cent.
func interpolateFtpRate(points []FtpTenorPoint, tenorMonths int64) int64 {
	if tenorMonths <= points[0].TenorMonths {
		return points[0].RateBps
	}
	last := points[len(points)-1]
	if tenorMonths >= last.TenorMonths {
		return last.RateBps
	}
	for i := 0; i+1 < len(points); i++ {
		a, b := points[i], points[i+1]
		if tenorMonths >= a.TenorMonths && tenorMonths <= b.TenorMonths {
			// rate = ra + (rb-ra)*(t-ta)/(tb-ta). (rb-ra)*(t-ta) is
			// bounded by 100000 bps * 1199 months: no overflow.
			return a.RateBps + (b.RateBps-a.RateBps)*(tenorMonths-a.TenorMonths)/(b.TenorMonths-a.TenorMonths)
		}
	}
	return last.RateBps
}

// ftpDirectionOf maps the signed funding gap to the charge direction:
// positive is a shortfall (the unit pays), negative is a surplus (the
// pool pays).
func ftpDirectionOf(gapCents int64) FtpDirection {
	if gapCents < 0 {
		return FtpDirectionRevenue
	}
	return FtpDirectionCost
}

// ftpChargeCents prices one charge: floor(|gap| * rateBps / 10000 *
// tenorMonths / 12) cents. The basis-point leg reuses policyFeeCents
// (overflow-safe, <= |gap|); the tenor leg is overflow-checked and
// reports ErrFtpChargeOverflow instead of wrapping.
func ftpChargeCents(gapCents, rateBps, tenorMonths int64) (int64, error) {
	g := gapCents
	if g == math.MinInt64 {
		return 0, ErrFtpChargeOverflow
	}
	if g < 0 {
		g = -g
	}
	annual := policyFeeCents(g, rateBps)
	if tenorMonths > 0 && annual > math.MaxInt64/tenorMonths {
		return 0, ErrFtpChargeOverflow
	}
	return annual * tenorMonths / 12, nil
}

// ftpChargeEntryID builds the deterministic charge entry ID: one
// charge per (account, currency, tenor, calendar day) is idempotent,
// like the daily accruals.
func ftpChargeEntryID(account AccountID, currency string, tenorMonths int64, asof time.Time) string {
	return "ftp/" + currency + "/" + string(account) + "/" +
		strconv.FormatInt(tenorMonths, 10) + "/" + asof.UTC().Format("2006-01-02")
}

// FtpPreview prices one FTP charge with zero side effects: no entry,
// no version bump, no audit event, no metrics. It runs the same
// validation and math as ChargeFtp, so a preview and the charge it
// previews can never disagree.
func (l *Ledger) FtpPreview(account AccountID, currency string, tenorMonths int64, gapCents int64) (FtpPreview, error) {
	return l.ftpPrice(account, currency, tenorMonths, gapCents)
}

// ftpPrice is the shared pricing path for FtpPreview and ChargeFtp:
// the same validation and math, so a preview and the charge it
// previews can never disagree. Callers hold no lock.
func (l *Ledger) ftpPrice(account AccountID, currency string, tenorMonths int64, gapCents int64) (FtpPreview, error) {
	if account == "" || strings.Contains(string(account), "/") {
		return FtpPreview{}, errors.New("ledger: FTP account must be non-empty and must not contain '/'")
	}
	if tenorMonths < 1 || tenorMonths > maxFtpTenorMonths {
		return FtpPreview{}, ErrFtpTenorOutOfRange
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return FtpPreview{}, err
	}
	if gapCents == 0 {
		return FtpPreview{}, ErrFtpZeroGap
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.ftpAccountsConfiguredLocked() {
		return FtpPreview{}, ErrFtpAccountsNotConfigured
	}
	c, ok := l.ftpCurves[cur]
	if !ok {
		return FtpPreview{}, ErrFtpCurveNotFound
	}
	rate := interpolateFtpRate(c.Points, tenorMonths)
	charge, err := ftpChargeCents(gapCents, rate, tenorMonths)
	if err != nil {
		return FtpPreview{}, err
	}
	if charge <= 0 {
		// A dust charge books nothing: a tiny gap under a low rate
		// rounds to zero cents, mirroring the accrual dust rule (see
		// AccrueInterest).
		return FtpPreview{}, ErrNonPositiveAmount
	}
	return FtpPreview{
		Currency:     cur,
		Account:      account,
		TenorMonths:  tenorMonths,
		GapCents:     gapCents,
		RateBps:      rate,
		CurveVersion: c.Version,
		Direction:    ftpDirectionOf(gapCents),
		ChargeCents:  charge,
	}, nil
}

// ChargeFtp books one internal funds-transfer-pricing charge: the
// business unit's funding gap (positive gapCents) or surplus (negative)
// priced off the currency's FTP curve at tenorMonths. A gap books
// debit FTP expense / credit treasury pool (the unit pays for
// funding); a surplus books debit pool / credit FTP revenue (the pool
// pays the unit).
//
// The call is idempotent per (account, currency, tenor, calendar day):
// re-running the same day returns the original entry with
// duplicate == true. asof sets both the entry's timestamp and its
// calendar day (UTC); a zero asof means now. The entry is a system
// adjusting entry — full Post gates, no rules cascade, no
// dual-control, like accruals (see the ftp.go package doc comment).
func (l *Ledger) ChargeFtp(account AccountID, currency string, tenorMonths int64, gapCents int64, asof time.Time) (JournalEntry, bool, error) {
	price, err := l.ftpPrice(account, currency, tenorMonths, gapCents)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	id := ftpChargeEntryID(account, price.Currency, tenorMonths, asof)

	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-check under the write lock: the curve or the accounts could
	// have changed between the preview read and now. Pricing off a
	// stale curve would book a charge the audit trail cannot
	// reproduce, so re-price here and book exactly this. (Curves are
	// never removed, so the entry still exists; the guard is
	// defensive.)
	curve, ok := l.ftpCurves[price.Currency]
	if !ok {
		return JournalEntry{}, false, ErrFtpCurveNotFound
	}
	if !l.ftpAccountsConfiguredLocked() {
		return JournalEntry{}, false, ErrFtpAccountsNotConfigured
	}
	rate := interpolateFtpRate(curve.Points, tenorMonths)
	version := curve.Version
	charge, err := ftpChargeCents(gapCents, rate, tenorMonths)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if charge <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	var debit, credit AccountID
	if price.Direction == FtpDirectionCost {
		debit, credit = l.ftpAccounts.ExpenseAccount, l.ftpAccounts.PoolAccount
	} else {
		debit, credit = l.ftpAccounts.PoolAccount, l.ftpAccounts.RevenueAccount
	}
	e := JournalEntry{
		ID:             id,
		DebitAccount:   debit,
		CreditAccount:  credit,
		AmountCents:    charge,
		Currency:       price.Currency,
		IdempotencyKey: id,
		Memo:           "ftp:charge",
		CreatedAt:      asof,
	}
	versionBefore := l.version
	posted, dup, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if !dup {
		l.emitAudit(AuditEvent{
			Op:            "ftp_charged",
			Actor:         "ChargeFtp",
			TraceID:       id,
			VersionBefore: versionBefore,
			VersionAfter:  l.version,
			Accounts:      []AccountID{account, debit, credit},
			Details: map[string]any{
				"direction":     string(price.Direction),
				"gap_cents":     gapCents,
				"tenor_months":  tenorMonths,
				"rate_bps":      rate,
				"curve_version": version,
				"charge_cents":  charge,
				"currency":      price.Currency,
			},
		})
	}
	return posted, dup, nil
}

// ftpCurvesSnapshotLocked returns the snapshot form of every
// configured curve, sorted by currency. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) ftpCurvesSnapshotLocked() []FtpCurveSnapshot {
	out := make([]FtpCurveSnapshot, 0, len(l.ftpCurves))
	for _, c := range l.ftpCurves {
		out = append(out, FtpCurveSnapshot{
			Currency: c.Currency,
			Points:   append([]FtpTenorPoint(nil), c.Points...),
			Version:  c.Version,
			SetAt:    c.SetAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// ftpTotalsLocked derives the per-currency FTP totals from the
// journal's "ftp:charge" rows, sorted by currency. Cost rows are the
// debit-expense legs, revenue rows the credit-revenue legs — the same
// legs the audit chain and trial balances carry, so the section can
// never disagree with the books. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) ftpTotalsLocked() []FtpTotals {
	byCur := make(map[string]*FtpTotals)
	for _, e := range l.entries {
		if e.Memo != "ftp:charge" {
			continue
		}
		t := byCur[e.Currency]
		if t == nil {
			t = &FtpTotals{Currency: e.Currency}
			byCur[e.Currency] = t
		}
		t.Charges++
		switch {
		case e.DebitAccount == l.ftpAccounts.ExpenseAccount:
			t.CostCents += e.AmountCents
		case e.CreditAccount == l.ftpAccounts.RevenueAccount:
			t.RevenueCents += e.AmountCents
		}
	}
	out := make([]FtpTotals, 0, len(byCur))
	for _, t := range byCur {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// ftpCurveVersionsLocked reports each configured currency's curve
// version, sorted by currency, so Reconcile discloses exactly which
// curve generation priced the booked charges. Callers must hold l.mu;
// the read lock suffices.
func (l *Ledger) ftpCurveVersionsLocked() []FtpCurveSnapshot {
	out := make([]FtpCurveSnapshot, 0, len(l.ftpCurves))
	for _, c := range l.ftpCurves {
		out = append(out, FtpCurveSnapshot{
			Currency: c.Currency,
			Points:   append([]FtpTenorPoint(nil), c.Points...),
			Version:  c.Version,
			SetAt:    c.SetAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
