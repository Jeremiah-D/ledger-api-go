package ledger

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Low-balance alert watermark (LG-37).
//
// A low-balance threshold is an opt-in, per-account, per-currency alert
// level: when a committed posting takes an account's balance below its
// threshold, the ledger records one structured audit event
// ("low_balance_breach") and bumps the breach counter. The alert is
// advisory, never a risk-control rejection — postings are never blocked
// for crossing a threshold — so the evaluation runs strictly after the
// atomic commit zone (commitEntryLocked has returned, balances already
// updated) and can never fail or alter a posting.
//
// Alert semantics:
//   - One event per breach: the first posting that takes the balance
//     below the threshold fires; further postings while the balance
//     stays below are silent. When a posting brings the balance back to
//     at or above the threshold, the alert re-arms, so the next fall
//     below fires again.
//   - The evaluation reads balances through the same view BalanceIn
//     reports — read-only, never mutating accounting state, and never
//     inside the write atomic zone.
//   - Thresholds are structural config, like frozen accounts and daily
//     outflow limits: setting or clearing one does not bump the ledger
//     version, survives disaster-recovery snapshots, and is listed by
//     Reconcile. Any int64 threshold is legal (negative thresholds are
//     meaningful for accounts that can go negative).
//   - Idempotent replays book nothing and never evaluate: a replayed
//     key cannot fire a breach.

// lowBalanceKey identifies one configured threshold: an account's alert
// level in one currency.
type lowBalanceKey struct {
	account  AccountID
	currency string
}

// LowBalanceThreshold is one configured low-balance alert level, used by
// the reconciliation report and the disaster-recovery snapshot.
type LowBalanceThreshold struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	ThresholdCents int64     `json:"threshold_cents"`
}

// SetLowBalanceThreshold configures (or replaces) the low-balance alert
// level for one account in one currency: the first committed posting
// that takes the account's balance below thresholdCents emits one
// low_balance_breach audit event. Setting a threshold is structural, like
// SetDailyLimit or Freeze: it does not bump the ledger version (no
// balances change). Replacing a threshold while the balance is already
// below the new level does not fire: the alert only fires on a committed
// posting that crosses the level, so a config change never backfills.
func (l *Ledger) SetLowBalanceThreshold(a AccountID, currency string, thresholdCents int64) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lowBalanceThresholds[lowBalanceKey{account: a, currency: currency}] = thresholdCents
	return nil
}

// ClearLowBalanceThreshold removes the low-balance alert level for one
// account in one currency, and clears its breach state. Clearing a
// threshold that was never set is a no-op. Like SetLowBalanceThreshold,
// it is structural and does not bump the ledger version.
func (l *Ledger) ClearLowBalanceThreshold(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := lowBalanceKey{account: a, currency: currency}
	delete(l.lowBalanceThresholds, key)
	delete(l.lowBalanceBreached, key)
}

// LowBalanceThreshold reports the configured low-balance alert level for
// one account in one currency, and whether a threshold is configured at
// all.
func (l *Ledger) LowBalanceThreshold(a AccountID, currency string) (int64, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	threshold, ok := l.lowBalanceThresholds[lowBalanceKey{account: a, currency: currency}]
	return threshold, ok
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
		l.lowBalanceThresholds[lowBalanceKey{account: a, currency: currency}] = thresholdCents
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

// LowBalanceBreachCount reports how many low-balance breach alerts the
// ledger has emitted since it started. The counter resets on restart —
// alert history lives in the structured audit log (op
// "low_balance_breach"), which is the durable record.
func (l *Ledger) LowBalanceBreachCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lowBalanceBreaches
}

// touchedAccounts collects the (account, currency) pairs a committed
// entry set touched, for post-commit low-balance evaluation. It dedupes
// nothing; evaluateLowBalanceLocked dedupes before evaluating.
func touchedAccounts(entries []JournalEntry) []accountCurrency {
	out := make([]accountCurrency, 0, 2*len(entries))
	for _, e := range entries {
		out = append(out,
			accountCurrency{account: e.DebitAccount, currency: e.Currency},
			accountCurrency{account: e.CreditAccount, currency: e.Currency})
	}
	return out
}

// evaluateLowBalanceLocked evaluates low-balance thresholds for the given
// (account, currency) pairs after a commit. Callers must hold l.mu (the
// write lock, from a committing entrypoint) — the evaluation runs after
// the atomic commit zone (commitEntryLocked has returned), reads balances
// the same way BalanceIn does (read-only, never mutating accounting
// state), and is advisory only: it can neither fail nor alter the
// posting. The audit event and counter are the only effects.
//
// actor is the entrypoint name for the audit event ("Post",
// "PostTransfer", ...), and traceID correlates the event with the
// operation (entry ID, transfer/sweep/merge/batch ID, or hold ID).
func (l *Ledger) evaluateLowBalanceLocked(touched []accountCurrency, traceID, actor string) {
	seen := make(map[accountCurrency]bool, len(touched))
	for _, k := range touched {
		if seen[k] {
			continue
		}
		seen[k] = true
		threshold, ok := l.lowBalanceThresholds[lowBalanceKey{account: k.account, currency: k.currency}]
		if !ok {
			continue
		}
		balance := l.balances[k]
		breachKey := lowBalanceKey{account: k.account, currency: k.currency}
		if balance < threshold {
			// One event per breach: while the balance stays below the
			// threshold, further postings are silent.
			if l.lowBalanceBreached[breachKey] {
				continue
			}
			l.lowBalanceBreached[breachKey] = true
			l.lowBalanceBreaches++
			l.emitAudit(AuditEvent{
				Op:            "low_balance_breach",
				Actor:         actor,
				TraceID:       traceID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{k.account},
				Details: map[string]any{
					"account":         string(k.account),
					"currency":        k.currency,
					"threshold_cents": threshold,
					"balance_cents":   balance,
					"version":         l.version,
				},
			})
			continue
		}
		// Recovered to at or above the threshold: re-arm, silently, so
		// the next fall below alerts again.
		delete(l.lowBalanceBreached, breachKey)
	}
}

// lowBalanceThresholdsLocked returns every configured low-balance alert
// level, sorted by (account, currency), for the reconciliation report and
// the snapshot. Callers must hold l.mu.
func (l *Ledger) lowBalanceThresholdsLocked() []LowBalanceThreshold {
	out := make([]LowBalanceThreshold, 0, len(l.lowBalanceThresholds))
	for k, threshold := range l.lowBalanceThresholds {
		out = append(out, LowBalanceThreshold{Account: k.account, Currency: k.currency, ThresholdCents: threshold})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// recomputeLowBalanceBreachesLocked re-derives the breach state from the
// restored balances after a snapshot import: an (account, currency) whose
// restored balance is already below its threshold counts as breached, so
// the first posting after the restore does not backfire a stale alert.
// It emits no events and touches no counters — a restore is not an
// operation. Callers must hold the write lock.
func (l *Ledger) recomputeLowBalanceBreachesLocked() {
	for k, threshold := range l.lowBalanceThresholds {
		breachKey := lowBalanceKey{account: k.account, currency: k.currency}
		if l.balances[accountCurrency{account: k.account, currency: k.currency}] < threshold {
			l.lowBalanceBreached[breachKey] = true
		} else {
			delete(l.lowBalanceBreached, breachKey)
		}
	}
}
