package ledger

import (
	"fmt"
	"strconv"
	"strings"
)

// Low-balance alert watermark (LG-37), now a facade over the unified
// balance-watch engine (LG-61, see balance_watch.go).
//
// The original LG-37 API only knew a low threshold; it is preserved here
// so existing callers keep working, but it reads and writes the same
// watch store SetBalanceWatch uses — there is exactly one threshold
// system. A low threshold set through this facade is the low side of the
// account's watch; any high side configured through SetBalanceWatch is
// left untouched. Breach alerts surface as "balance_threshold_breached"
// audit events with direction "low".

// LowBalanceThreshold is one configured low-balance alert level, used by
// the legacy threshold parser and by disaster-recovery snapshots written
// before LG-61 (imports map it to the low side of a balance watch).
type LowBalanceThreshold struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	ThresholdCents int64     `json:"threshold_cents"`
}

// SetLowBalanceThreshold configures (or replaces) the low side of the
// balance threshold watch for one account in one currency: the first
// committed posting that takes the account's balance below
// thresholdCents emits one balance_threshold_breached audit event
// (direction "low"). A high side already configured on the watch is
// preserved. Setting a threshold is structural, like SetDailyLimit or
// Freeze: it does not bump the ledger version (no balances change).
// Replacing the low level while the balance is already below it does not
// fire: the alert only fires on a committed posting that crosses the
// level, so a config change never backfills.
func (l *Ledger) SetLowBalanceThreshold(a AccountID, currency string, thresholdCents int64) error {
	if a == "" {
		return ErrEmptyWatchAccount
	}
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := balanceWatchKey{account: a, currency: currency}
	existing := l.balanceWatches[key]
	cfg := BalanceWatchConfig{LowCents: int64ptr(thresholdCents), HighCents: existing.HighCents}
	if err := checkBalanceWatchConfig(cfg); err != nil {
		return err
	}
	l.setBalanceWatchLocked(a, currency, cfg)
	return nil
}

// ClearLowBalanceThreshold removes the low side of the balance threshold
// watch for one account in one currency, and clears its low breach
// state. A configured high side survives; when the low side was the only
// side, the watch is removed entirely. Clearing a threshold that was
// never set is a no-op. Like SetLowBalanceThreshold, it is structural
// and does not bump the ledger version.
func (l *Ledger) ClearLowBalanceThreshold(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := balanceWatchKey{account: a, currency: currency}
	w, ok := l.balanceWatches[key]
	if !ok || w.LowCents == nil {
		return
	}
	if w.HighCents == nil {
		// The low side was the only side: remove the watch entirely.
		// (Inlined rather than calling ClearBalanceWatch: the write
		// lock is already held.)
		delete(l.balanceWatches, key)
		delete(l.balanceWatchBreached, key)
		l.emitAudit(AuditEvent{
			Op:       "balance_watch_cleared",
			Actor:    "ClearLowBalanceThreshold",
			Accounts: []AccountID{a},
			Details: map[string]any{
				"account":  string(a),
				"currency": currency,
			},
		})
		return
	}
	w.LowCents = nil
	l.balanceWatches[key] = w
	if st, breached := l.balanceWatchBreached[key]; breached {
		st.low = false
		if st.high {
			l.balanceWatchBreached[key] = st
		} else {
			delete(l.balanceWatchBreached, key)
		}
	}
	l.emitAudit(AuditEvent{
		Op:       "balance_watch_set",
		Actor:    "ClearLowBalanceThreshold",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":    string(a),
			"currency":   currency,
			"high_cents": *w.HighCents,
		},
	})
}

// LowBalanceThreshold reports the configured low side of the balance
// threshold watch for one account in one currency, and whether a low
// level is configured at all.
func (l *Ledger) LowBalanceThreshold(a AccountID, currency string) (int64, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	w, ok := l.balanceWatches[balanceWatchKey{account: a, currency: currency}]
	if !ok || w.LowCents == nil {
		return 0, false
	}
	return *w.LowCents, true
}

// WithLowBalanceThreshold configures a low-balance alert level at
// construction time (see SetLowBalanceThreshold). An invalid currency
// code panics — fail-fast at construction, like the fee schedule and
// daily limits — so a misconfigured threshold can never silently
// under-alert.
func WithLowBalanceThreshold(a AccountID, currency string, thresholdCents int64) Option {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid low-balance threshold currency %q: %v", currency, err))
	}
	return func(l *Ledger) {
		l.balanceWatches[balanceWatchKey{account: a, currency: currency}] = BalanceWatch{
			Account:   a,
			Currency:  currency,
			LowCents:  int64ptr(thresholdCents),
			HighCents: l.balanceWatches[balanceWatchKey{account: a, currency: currency}].HighCents,
		}
	}
}

// ParseLowBalanceThresholds parses the LEDGER_LOW_BALANCE_THRESHOLDS
// environment variable into low-balance threshold configs:
//
//	"<account>:<currency>:<thresholdCents>[,<account>:<currency>:<thresholdCents>...]"
//
// e.g. "cust-123:USD:50000,cust-456:EUR:-10000" alerts when cust-123's USD
// balance falls below $500.00 and when cust-456's EUR balance falls below
// -€100.00. Thresholds are integer cents and may be negative (a balance
// level, not an outflow); currencies follow the usual ISO 4217 validation.
// Parsing is strict — a malformed segment is an error, so a misconfigured
// deployment fails fast at startup instead of silently running without
// its alert level.
func ParseLowBalanceThresholds(raw string) ([]LowBalanceThreshold, error) {
	fail := func(format string, args ...any) ([]LowBalanceThreshold, error) {
		return nil, fmt.Errorf("ledger: invalid low-balance thresholds %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []LowBalanceThreshold
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 3 {
			return fail("segment %q must be \"<account>:<currency>:<thresholdCents>\"", seg)
		}
		account := strings.TrimSpace(parts[0])
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[1]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		threshold, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil {
			return fail("segment %q: threshold %q must be an integer (cents)", seg, parts[2])
		}
		out = append(out, LowBalanceThreshold{Account: AccountID(account), Currency: currency, ThresholdCents: threshold})
	}
	return out, nil
}

// LowBalanceBreachCount is the LG-37 name for the breach counter; it is
// kept as an alias of BalanceWatchBreachCount so existing callers keep
// working. New code should prefer BalanceWatchBreachCount.
func (l *Ledger) LowBalanceBreachCount() uint64 {
	return l.BalanceWatchBreachCount()
}
