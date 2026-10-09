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

// ErrDailyLimitExceeded is returned by Post and PostTransfer when the
// posting would take the account's cumulative outflow for the UTC calendar
// day above its configured daily outflow limit (see SetDailyLimit).
var ErrDailyLimitExceeded = errors.New("ledger: posting would exceed the account's daily outflow limit")

// ErrInvalidDailyLimit is returned by SetDailyLimit when the limit is
// negative. A zero limit is legal: it stops all outflow (a risk-control
// freeze-out); clearing a limit uses ClearDailyLimit.
var ErrInvalidDailyLimit = errors.New("ledger: daily outflow limit must not be negative")

// dailyLimitKey identifies one configured limit and one accumulation
// bucket: an account's outflow in one currency on one UTC calendar day.
type dailyLimitKey struct {
	account  AccountID
	currency string
	day      string // UTC date as "2006-01-02"; "" in dailyLimits (the config map)
}

// DailyLimit is one configured daily outflow limit, used by the
// reconciliation report and the disaster-recovery snapshot.
type DailyLimit struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	LimitCents int64     `json:"limit_cents"`
}

// utcDay returns the UTC calendar day of t as "2006-01-02". ISO dates
// sort lexicographically in chronological order, which the bucket pruning
// relies on.
func utcDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// SetDailyLimit configures (or replaces) the daily outflow limit for one
// account in one currency: the sum of that account's outflow on a UTC
// calendar day may not exceed limitCents. "Outflow" is the payer side —
// for Post it is the credit account (the account funds leave, matching
// PostTransfer's payer), for PostTransfer it is the payer's total outflow
// (amount plus any fee leg). A limit of 0 stops all outflow; negative
// limits are rejected with ErrInvalidDailyLimit.
//
// Setting or clearing a limit is structural, like Freeze or SetParent: it
// does not bump the ledger version (no balances change) and it is visible
// to audit tooling — TrialBalance reports the default-currency limit and
// Reconcile lists every configured limit.
func (l *Ledger) SetDailyLimit(a AccountID, currency string, limitCents int64) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if limitCents < 0 {
		return ErrInvalidDailyLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dailyLimits[dailyLimitKey{account: a, currency: currency}] = limitCents
	return nil
}

// ClearDailyLimit removes the daily outflow limit for one account in one
// currency. Clearing a limit that was never set is a no-op. Like
// SetDailyLimit, it is structural and does not bump the ledger version.
func (l *Ledger) ClearDailyLimit(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.dailyLimits, dailyLimitKey{account: a, currency: currency})
}

// DailyLimit reports the configured daily outflow limit for one account
// in one currency, and whether a limit is configured at all.
func (l *Ledger) DailyLimit(a AccountID, currency string) (int64, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	limit, ok := l.dailyLimits[dailyLimitKey{account: a, currency: currency}]
	return limit, ok
}

// WithDailyLimit configures a daily outflow limit at construction time
// (see SetDailyLimit). Invalid currency codes and negative limits panic —
// fail-fast at construction, like the fee schedule — so a misconfigured
// limit can never silently under-enforce.
func WithDailyLimit(a AccountID, currency string, limitCents int64) Option {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid daily limit currency %q: %v", currency, err))
	}
	if limitCents < 0 {
		panic("ledger: daily outflow limit must not be negative")
	}
	return func(l *Ledger) {
		l.dailyLimits[dailyLimitKey{account: a, currency: currency}] = limitCents
	}
}

// ParseDailyLimits parses the LEDGER_DAILY_OUTFLOW_LIMITS environment
// variable into daily outflow limit configs:
//
//	"<account>:<currency>:<limitCents>[,<account>:<currency>:<limitCents>...]"
//
// e.g. "cust-123:USD:100000,cust-456:EUR:50000" caps cust-123's daily USD
// outflow at $1000.00 and cust-456's daily EUR outflow at €500.00.
// Limits are integer cents and must not be negative; currencies follow the
// usual ISO 4217 validation. Parsing is strict — a malformed segment is an
// error, so a misconfigured deployment fails fast at startup instead of
// silently running without its risk control.
func ParseDailyLimits(raw string) ([]DailyLimit, error) {
	fail := func(format string, args ...any) ([]DailyLimit, error) {
		return nil, fmt.Errorf("ledger: invalid daily outflow limits %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []DailyLimit
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 3 {
			return fail("segment %q must be \"<account>:<currency>:<limitCents>\"", seg)
		}
		account := strings.TrimSpace(parts[0])
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[1]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		limit, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil || limit < 0 {
			return fail("segment %q: limit %q must be a non-negative integer (cents)", seg, parts[2])
		}
		out = append(out, DailyLimit{Account: AccountID(account), Currency: currency, LimitCents: limit})
	}
	return out, nil
}

// dailyLimitRejectedLocked reports whether posting amount cents of outflow
// for account in currency at time at would exceed the account's configured
// daily outflow limit. Accounts without a configured limit are never
// rejected. The accumulated outflow is keyed by the posting's own UTC
// calendar day, so a backdated entry counts toward its own day —
// deterministic for tests and honest for backfills.
//
// The comparison never adds total + amount, so it cannot overflow even
// when amount is huge: amount > limit rejects immediately (a non-negative
// total can never stay within the limit then), otherwise
// total > limit - amount is equivalent and both sides are non-negative.
// Callers must hold l.mu.
func (l *Ledger) dailyLimitRejectedLocked(account AccountID, currency string, amount int64, at time.Time) bool {
	limit, ok := l.dailyLimits[dailyLimitKey{account: account, currency: currency}]
	if !ok {
		return false
	}
	if amount > limit {
		return true
	}
	total := l.dailyOutflow[dailyLimitKey{account: account, currency: currency, day: utcDay(at)}]
	return total > limit-amount
}

// addDailyOutflowLocked records amount cents of outflow for account in
// currency on the posting's UTC calendar day. Callers must hold the write
// lock and must have run dailyLimitRejectedLocked first; the add runs in
// the same critical section as the commit, so the check-then-add is
// atomic. The total saturates at MaxInt64 on overflow — the limit check
// above already rejected any posting that would have fit under a
// configured limit, so saturation only matters for unlimited accounts
// with absurd cumulative volume.
func (l *Ledger) addDailyOutflowLocked(account AccountID, currency string, amount int64, at time.Time) {
	key := dailyLimitKey{account: account, currency: currency, day: utcDay(at)}
	total := l.dailyOutflow[key]
	if total > math.MaxInt64-amount {
		l.dailyOutflow[key] = math.MaxInt64
		return
	}
	l.dailyOutflow[key] = total + amount
}

// maybePruneDailyOutflowLocked drops outflow buckets for UTC days older
// than yesterday: a new posting can only reference its own day's bucket
// (backdated or current), so anything older is dead weight. Pruning runs
// at most once per pruneInterval — amortized O(1) per Post, like the
// idempotency-key sweep. Callers must hold l.mu.
func (l *Ledger) maybePruneDailyOutflowLocked(now time.Time) {
	if now.Sub(l.lastDailyPrune) < l.pruneInterval {
		return
	}
	l.lastDailyPrune = now
	cutoff := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for k := range l.dailyOutflow {
		if k.day < cutoff {
			delete(l.dailyOutflow, k)
		}
	}
}

// dailyLimitsLocked returns every configured daily outflow limit, sorted
// by (account, currency), for the reconciliation report and the snapshot.
// Callers must hold l.mu.
func (l *Ledger) dailyLimitsLocked() []DailyLimit {
	out := make([]DailyLimit, 0, len(l.dailyLimits))
	for k, limit := range l.dailyLimits {
		out = append(out, DailyLimit{Account: k.account, Currency: k.currency, LimitCents: limit})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}
