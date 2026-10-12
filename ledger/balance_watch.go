package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Balance threshold watchers (LG-61).
//
// A balance watch is an opt-in, per-account, per-currency alert band: a
// low level, a high level, or both. When a committed posting takes an
// account's balance below the low level (or above the high level), the
// ledger records one structured audit event ("balance_threshold_breached",
// with the direction, the level, and the balance) and bumps the breach
// counter. The alert is advisory, never a risk-control rejection —
// postings are never blocked for crossing a level — so the evaluation
// runs strictly after the atomic commit zone (commitEntryLocked has
// returned, balances already updated) and can neither fail nor alter a
// posting.
//
// Alert semantics:
//   - Edge-triggered, one event per breach: the first posting that takes
//     the balance below the low level (or above the high level) fires;
//     further postings while the balance stays outside the band are
//     silent. When a posting brings the balance back inside the band
//     (at or above low, at or below high), the alert re-arms, so the
//     next crossing fires again.
//   - The evaluation reads balances through the same view BalanceIn
//     reports — read-only, never mutating accounting state, and never
//     inside the write atomic zone.
//   - Rejected postings (frozen, overdraft, daily-limit, validation,
//     ...) book nothing, so they never evaluate: only committed postings
//     (Post, PostTransfer, Capture, and the other commit paths) can
//     cross a level.
//   - Watches are structural config, like frozen accounts and daily
//     outflow limits: setting or clearing one does not bump the ledger
//     version, survives disaster-recovery snapshots, and is listed by
//     Reconcile. Any int64 level is legal (negative lows are meaningful
//     for accounts that can go negative).
//   - Idempotent replays book nothing and never evaluate: a replayed
//     key cannot fire a breach.
//   - Watch CRUD is itself audited: SetBalanceWatch emits
//     "balance_watch_set" and ClearBalanceWatch emits
//     "balance_watch_cleared".
//
// This engine supersedes the low-only alert of LG-37: the legacy
// SetLowBalanceThreshold family (see low_balance.go) is now a thin
// low-side-only facade over the same store, so there is exactly one
// threshold system, one breach state, and one audit event shape.

// BalanceWatchDirection names which side of a watch fired: "low" when a
// posting took the balance below the low level, "high" when it took the
// balance above the high level. It is the "direction" field of the
// balance_threshold_breached audit event.
type BalanceWatchDirection string

const (
	// BalanceWatchLow fires when the balance falls below the low level.
	BalanceWatchLow BalanceWatchDirection = "low"
	// BalanceWatchHigh fires when the balance rises above the high level.
	BalanceWatchHigh BalanceWatchDirection = "high"
)

// BalanceWatchConfig is the input form for SetBalanceWatch: a nil side
// means "leave this side unset". At least one side must be set.
type BalanceWatchConfig struct {
	LowCents  *int64
	HighCents *int64
}

// BalanceWatch is one account's configured threshold watch in one
// currency, used by the reconciliation report and the disaster-recovery
// snapshot. A nil side means that side is not watched.
type BalanceWatch struct {
	Account   AccountID `json:"account"`
	Currency  string    `json:"currency"`
	LowCents  *int64    `json:"low_cents,omitempty"`
	HighCents *int64    `json:"high_cents,omitempty"`
}

// balanceWatchKey identifies one configured watch: an account's alert
// band in one currency.
type balanceWatchKey struct {
	account  AccountID
	currency string
}

// watchBreachState tracks which directions of a watch are currently in
// breach (silenced until the balance recovers inside the band).
type watchBreachState struct {
	low  bool
	high bool
}

// Balance watch validation errors.
var (
	ErrEmptyWatchAccount = errors.New("ledger: balance watch account must not be empty")
	ErrBalanceWatchEmpty = errors.New("ledger: balance watch must set at least one of low_cents, high_cents")
	ErrInvalidBalanceWatch = errors.New("ledger: balance watch low_cents must be below high_cents")
)

// checkBalanceWatchConfig validates a watch configuration: at least one
// side set, and low strictly below high when both are set. A band with
// low >= high would fire both directions on a single posting, so it is
// rejected instead of silently mis-alerting.
func checkBalanceWatchConfig(cfg BalanceWatchConfig) error {
	if cfg.LowCents == nil && cfg.HighCents == nil {
		return ErrBalanceWatchEmpty
	}
	if cfg.LowCents != nil && cfg.HighCents != nil && *cfg.LowCents >= *cfg.HighCents {
		return ErrInvalidBalanceWatch
	}
	return nil
}

// int64ptr is a tiny helper for building watch configs in tests and
// options.
func int64ptr(v int64) *int64 { return &v }

// SetBalanceWatch configures (or replaces) the balance threshold watch
// for one account in one currency: the first committed posting that
// takes the account's balance below lowCents (when set) or above
// highCents (when set) emits one balance_threshold_breached audit event
// per direction. Setting a watch is structural, like SetDailyLimit or
// Freeze: it does not bump the ledger version (no balances change).
// Replacing a watch while the balance is already outside the new band
// does not fire: the breach state is re-derived from the live balance,
// so a config change never backfills — only a later committed posting
// that crosses a level fires. The operation is audited as
// "balance_watch_set".
func (l *Ledger) SetBalanceWatch(a AccountID, currency string, cfg BalanceWatchConfig) error {
	if a == "" {
		return ErrEmptyWatchAccount
	}
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if err := checkBalanceWatchConfig(cfg); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setBalanceWatchLocked(a, currency, cfg)
	return nil
}

// setBalanceWatchLocked is SetBalanceWatch's body. Callers must hold the
// write lock; the config must already be validated.
func (l *Ledger) setBalanceWatchLocked(a AccountID, currency string, cfg BalanceWatchConfig) {
	key := balanceWatchKey{account: a, currency: currency}
	l.balanceWatches[key] = BalanceWatch{
		Account:   a,
		Currency:  currency,
		LowCents:  cfg.LowCents,
		HighCents: cfg.HighCents,
	}
	// Never backfill: derive the breach state from the live balance, so
	// a config change alone cannot fire a stale alert.
	st := watchBreachState{}
	balance := l.balances[accountCurrency{account: a, currency: currency}]
	if cfg.LowCents != nil && balance < *cfg.LowCents {
		st.low = true
	}
	if cfg.HighCents != nil && balance > *cfg.HighCents {
		st.high = true
	}
	if st.low || st.high {
		l.balanceWatchBreached[key] = st
	} else {
		delete(l.balanceWatchBreached, key)
	}
	details := map[string]any{
		"account":  string(a),
		"currency": currency,
	}
	if cfg.LowCents != nil {
		details["low_cents"] = *cfg.LowCents
	}
	if cfg.HighCents != nil {
		details["high_cents"] = *cfg.HighCents
	}
	l.emitAudit(AuditEvent{
		Op:       "balance_watch_set",
		Actor:    "SetBalanceWatch",
		Accounts: []AccountID{a},
		Details:  details,
	})
}

// ClearBalanceWatch removes the balance threshold watch for one account
// in one currency, and clears its breach state. Clearing a watch that was
// never set is a no-op. Like SetBalanceWatch, it is structural and does
// not bump the ledger version. The operation is audited as
// "balance_watch_cleared".
func (l *Ledger) ClearBalanceWatch(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := balanceWatchKey{account: a, currency: currency}
	if _, ok := l.balanceWatches[key]; !ok {
		return
	}
	delete(l.balanceWatches, key)
	delete(l.balanceWatchBreached, key)
	l.emitAudit(AuditEvent{
		Op:       "balance_watch_cleared",
		Actor:    "ClearBalanceWatch",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":  string(a),
			"currency": currency,
		},
	})
}

// GetBalanceWatch reports the configured balance threshold watch for one
// account in one currency, and whether a watch is configured at all.
func (l *Ledger) GetBalanceWatch(a AccountID, currency string) (BalanceWatch, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	w, ok := l.balanceWatches[balanceWatchKey{account: a, currency: currency}]
	return w, ok
}

// ListBalanceWatches returns every configured balance threshold watch,
// sorted by (account, currency), for the reconciliation report and the
// disaster-recovery snapshot.
func (l *Ledger) ListBalanceWatches() []BalanceWatch {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balanceWatchesLocked()
}

// balanceWatchesLocked returns every configured watch, sorted by
// (account, currency). Callers must hold l.mu.
func (l *Ledger) balanceWatchesLocked() []BalanceWatch {
	out := make([]BalanceWatch, 0, len(l.balanceWatches))
	for _, w := range l.balanceWatches {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// balanceWatchLowCentsLocked / balanceWatchHighCentsLocked report one
// side of the watch for the trial-balance config summary. Callers must
// hold l.mu. 0 with false means the side is not watched.
func (l *Ledger) balanceWatchLowCentsLocked(a AccountID, currency string) int64 {
	if w, ok := l.balanceWatches[balanceWatchKey{account: a, currency: currency}]; ok && w.LowCents != nil {
		return *w.LowCents
	}
	return 0
}

func (l *Ledger) balanceWatchHighCentsLocked(a AccountID, currency string) int64 {
	if w, ok := l.balanceWatches[balanceWatchKey{account: a, currency: currency}]; ok && w.HighCents != nil {
		return *w.HighCents
	}
	return 0
}

// WithBalanceWatch configures a balance threshold watch at construction
// time (see SetBalanceWatch). An invalid config panics — fail-fast at
// construction, like the fee schedule and daily limits — so a
// misconfigured watch can never silently under-alert.
func WithBalanceWatch(a AccountID, currency string, cfg BalanceWatchConfig) Option {
	if a == "" {
		panic("ledger: invalid balance watch: account must not be empty")
	}
	currency, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid balance watch currency %q: %v", currency, err))
	}
	if err := checkBalanceWatchConfig(cfg); err != nil {
		panic(fmt.Sprintf("ledger: invalid balance watch: %v", err))
	}
	return func(l *Ledger) {
		l.balanceWatches[balanceWatchKey{account: a, currency: currency}] = BalanceWatch{
			Account:   a,
			Currency:  currency,
			LowCents:  cfg.LowCents,
			HighCents: cfg.HighCents,
		}
	}
}

// ParseBalanceWatches parses the LEDGER_BALANCE_WATCHES environment
// variable into balance watch configs:
//
//	"<account>:<currency>:<lowCents>:<highCents>[,<account>:<currency>:<lowCents>:<highCents>...]"
//
// Either level may be empty to leave that side unwatched, e.g.
// "cust-123:USD:50000:,ops:EUR::-10000" watches cust-123's USD balance
// against a $500.00 low and ops' EUR balance against a -€100.00 high.
// Levels are integer cents and may be negative; currencies follow the
// usual ISO 4217 validation. When both levels are set the low must be
// below the high. Parsing is strict — a malformed segment is an error,
// so a misconfigured deployment fails fast at startup instead of
// silently running without its alert band.
func ParseBalanceWatches(raw string) ([]BalanceWatch, error) {
	fail := func(format string, args ...any) ([]BalanceWatch, error) {
		return nil, fmt.Errorf("ledger: invalid balance watches %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []BalanceWatch
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 4 {
			return fail("segment %q must be \"<account>:<currency>:<lowCents>:<highCents>\" (either level may be empty)", seg)
		}
		account := strings.TrimSpace(parts[0])
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[1]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		w := BalanceWatch{Account: AccountID(account), Currency: currency}
		if s := strings.TrimSpace(parts[2]); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fail("segment %q: low_cents %q must be an integer (cents)", seg, parts[2])
			}
			w.LowCents = int64ptr(v)
		}
		if s := strings.TrimSpace(parts[3]); s != "" {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fail("segment %q: high_cents %q must be an integer (cents)", seg, parts[3])
			}
			w.HighCents = int64ptr(v)
		}
		if err := checkBalanceWatchConfig(BalanceWatchConfig{LowCents: w.LowCents, HighCents: w.HighCents}); err != nil {
			return fail("segment %q: %v", seg, err)
		}
		out = append(out, w)
	}
	return out, nil
}

// BalanceWatchBreachCount reports how many balance-threshold breach alerts
// the ledger has emitted since it started. The counter resets on restart —
// alert history lives in the structured audit log (op
// "balance_threshold_breached"), which is the durable record.
func (l *Ledger) BalanceWatchBreachCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balanceWatchBreachCount
}

// touchedAccounts collects the (account, currency) pairs a committed
// entry set touched, for post-commit balance-watch evaluation. It dedupes
// nothing; evaluateBalanceWatchesLocked dedupes before evaluating.
func touchedAccounts(entries []JournalEntry) []accountCurrency {
	out := make([]accountCurrency, 0, 2*len(entries))
	for _, e := range entries {
		out = append(out,
			accountCurrency{account: e.DebitAccount, currency: e.Currency},
			accountCurrency{account: e.CreditAccount, currency: e.Currency})
	}
	return out
}

// evaluateBalanceWatchesLocked evaluates balance threshold watches for
// the given (account, currency) pairs after a commit. Callers must hold
// l.mu (the write lock, from a committing entrypoint) — the evaluation
// runs after the atomic commit zone (commitEntryLocked has returned),
// reads balances the same way BalanceIn does (read-only, never mutating
// accounting state), and is advisory only: it can neither fail nor alter
// the posting. The audit events and the counter are the only effects.
//
// actor is the entrypoint name for the audit event ("Post",
// "PostTransfer", ...), and traceID correlates the event with the
// operation (entry ID, transfer/sweep/merge/batch ID, or hold ID).
func (l *Ledger) evaluateBalanceWatchesLocked(touched []accountCurrency, traceID, actor string) {
	seen := make(map[accountCurrency]bool, len(touched))
	for _, k := range touched {
		if seen[k] {
			continue
		}
		seen[k] = true
		key := balanceWatchKey{account: k.account, currency: k.currency}
		w, ok := l.balanceWatches[key]
		if !ok {
			continue
		}
		balance := l.balances[k]
		st := l.balanceWatchBreached[key]
		if w.LowCents != nil {
			if balance < *w.LowCents {
				// One event per breach: while the balance stays below
				// the low level, further postings are silent.
				if !st.low {
					st.low = true
					l.fireBalanceWatchLocked(k, BalanceWatchLow, *w.LowCents, balance, traceID, actor)
				}
			} else {
				// Recovered to at or above the low level: re-arm,
				// silently, so the next fall below alerts again.
				st.low = false
			}
		}
		if w.HighCents != nil {
			if balance > *w.HighCents {
				// One event per breach: while the balance stays above
				// the high level, further postings are silent.
				if !st.high {
					st.high = true
					l.fireBalanceWatchLocked(k, BalanceWatchHigh, *w.HighCents, balance, traceID, actor)
				}
			} else {
				// Recovered to at or below the high level: re-arm,
				// silently, so the next rise above alerts again.
				st.high = false
			}
		}
		if st.low || st.high {
			l.balanceWatchBreached[key] = st
		} else {
			delete(l.balanceWatchBreached, key)
		}
	}
}

// fireBalanceWatchLocked records one threshold breach: the counter bump
// and the structured audit event carrying the direction, the level, and
// the balance. Callers must hold the write lock; the breach-state update
// is the caller's job (see evaluateBalanceWatchesLocked).
func (l *Ledger) fireBalanceWatchLocked(k accountCurrency, direction BalanceWatchDirection, threshold, balance int64, traceID, actor string) {
	l.balanceWatchBreachCount++
	l.emitAudit(AuditEvent{
		Op:            "balance_threshold_breached",
		Actor:         actor,
		TraceID:       traceID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{k.account},
		Details: map[string]any{
			"account":         string(k.account),
			"currency":        k.currency,
			"direction":       string(direction),
			"threshold_cents": threshold,
			"balance_cents":   balance,
			"version":         l.version,
		},
	})
}

// recomputeBalanceWatchBreachesLocked re-derives the breach state from
// the restored balances after a snapshot import: an (account, currency)
// whose restored balance is already outside its band counts as breached,
// so the first posting after the restore does not backfire a stale
// alert. It emits no events and touches no counters — a restore is not an
// operation. Callers must hold the write lock.
func (l *Ledger) recomputeBalanceWatchBreachesLocked() {
	for key, w := range l.balanceWatches {
		st := watchBreachState{}
		balance := l.balances[accountCurrency{account: key.account, currency: key.currency}]
		if w.LowCents != nil && balance < *w.LowCents {
			st.low = true
		}
		if w.HighCents != nil && balance > *w.HighCents {
			st.high = true
		}
		if st.low || st.high {
			l.balanceWatchBreached[key] = st
		} else {
			delete(l.balanceWatchBreached, key)
		}
	}
}
