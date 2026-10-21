package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rolling-window velocity limits (LG-156).
//
// Where the daily outflow limit (daily_limit.go) caps an account's
// outflow per UTC calendar day and the API-key rate limiter (apikey.go)
// caps request rates per credential, velocity limits watch money
// movement itself: per account and currency, a sliding window (e.g.
// 10m, 24h) bounds the number of postings (count dimension) and the
// total cents moved (amount dimension), separately for inflow and
// outflow. Exceeding either dimension rejects the posting with
// ErrVelocityLimitExceeded (HTTP 422), before anything is booked.
//
// The three controls are deliberately orthogonal:
//
//   - daily_limit.go: UTC-day outflow only, calendar buckets.
//   - apikey.go:      per-API-key request rate, no money semantics.
//   - velocity.go:    sliding window, count + amount, inflow + outflow.
//
// A posting must clear all configured controls.

// ErrVelocityLimitExceeded is returned by Post, PostTransfer, and
// ReverseEntry when the posting would take the account's rolling
// window usage above its configured velocity limit.
var ErrVelocityLimitExceeded = errors.New("ledger: posting would exceed the account's velocity limit")

// ErrInvalidVelocityLimit is returned by SetVelocityLimit (and the
// env parser) when the configuration is unusable.
var ErrInvalidVelocityLimit = errors.New("ledger: invalid velocity limit")

// MaxVelocityWindow caps the sliding window at 30 days: longer windows
// pin unbounded event history in memory, and velocity is a
// short-horizon AML/risk control — month-scale caps belong to the
// budget/period machinery.
const MaxVelocityWindow = 30 * 24 * time.Hour

// VelocityDirection is the money-movement direction a velocity
// measurement covers. Inflow is the debit side of a journal entry
// (funds arrive); outflow is the credit side (funds leave) — the
// same payer convention the daily outflow limit uses.
type VelocityDirection string

const (
	// VelocityInflow measures funds arriving at the account.
	VelocityInflow VelocityDirection = "inflow"
	// VelocityOutflow measures funds leaving the account.
	VelocityOutflow VelocityDirection = "outflow"
)

// VelocityLimit is one configured rolling velocity limit, used by the
// reconciliation report and the disaster-recovery snapshot.
type VelocityLimit struct {
	Account        AccountID     `json:"account"`
	Currency       string        `json:"currency"`
	Window         time.Duration `json:"window"`
	MaxCount       int64         `json:"max_count"`        // <= 0 = unlimited
	MaxAmountCents int64         `json:"max_amount_cents"` // <= 0 = unlimited
}

// velocityLimitKey identifies one configured limit and one event
// deque: an account's activity in one currency.
type velocityLimitKey struct {
	account  AccountID
	currency string
}

// velocityEvent is one journal posting's contribution to a velocity
// window: the account's side of the entry. Every committed journal
// row records two events (debit side inflow, credit side outflow),
// so "count" means journal rows touching the account in that
// direction — a transfer with a fee leg counts two outflow rows for
// the payer, honestly reflecting the booked movement.
type velocityEvent struct {
	at     time.Time
	inflow bool
	cents  int64
}

// velocityClock returns the ledger's injected clock for
// velocity-window evaluation, or time.Now when none is configured.
func (l *Ledger) velocityClock() time.Time {
	if l.velocityNow != nil {
		return l.velocityNow()
	}
	return time.Now()
}

// WithVelocityClock injects the clock used for velocity-window
// evaluation (window boundaries in usage queries, pruning, and the
// zero-time fallback when recording). Tests use it to freeze time;
// production leaves it nil (time.Now).
func WithVelocityClock(now func() time.Time) Option {
	return func(l *Ledger) {
		l.velocityNow = now
	}
}

// checkVelocityLimit validates a limit configuration without
// touching the ledger.
func checkVelocityLimit(a AccountID, currency string, window time.Duration, maxCount, maxAmountCents int64) error {
	if a == "" {
		return fmt.Errorf("%w: account must not be empty", ErrInvalidVelocityLimit)
	}
	if _, err := normalizeCurrency(currency); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidVelocityLimit, err)
	}
	if window <= 0 {
		return fmt.Errorf("%w: window must be positive, got %v", ErrInvalidVelocityLimit, window)
	}
	if window > MaxVelocityWindow {
		return fmt.Errorf("%w: window %v exceeds the %v cap", ErrInvalidVelocityLimit, window, MaxVelocityWindow)
	}
	if maxCount <= 0 && maxAmountCents <= 0 {
		return fmt.Errorf("%w: at least one of max_count / max_amount_cents must be positive", ErrInvalidVelocityLimit)
	}
	return nil
}

// SetVelocityLimit configures (or replaces) the rolling velocity
// limit for one account in one currency: within any sliding window of
// length Window ending at the posting's timestamp, the account's
// inflow and outflow are each bounded by MaxCount postings and
// MaxAmountCents cents. A non-positive MaxCount / MaxAmountCents
// disables that dimension. Setting a limit is structural, like
// SetDailyLimit: it does not bump the ledger version (no balances
// change) and the configuration — not the rolling windows — survives
// disaster-recovery snapshots (windows restart at the restore
// point; see snapshot.go).
func (l *Ledger) SetVelocityLimit(a AccountID, currency string, window time.Duration, maxCount, maxAmountCents int64) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidVelocityLimit, err)
	}
	if err := checkVelocityLimit(a, currency, window, maxCount, maxAmountCents); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.velocityLimits[velocityLimitKey{account: a, currency: currency}] = VelocityLimit{
		Account: a, Currency: currency, Window: window,
		MaxCount: maxCount, MaxAmountCents: maxAmountCents,
	}
	l.emitAudit(AuditEvent{
		Op:       "velocity_limit_set",
		Actor:    "SetVelocityLimit",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":          string(a),
			"currency":         currency,
			"window":           window.String(),
			"max_count":        maxCount,
			"max_amount_cents": maxAmountCents,
		},
	})
	return nil
}

// ClearVelocityLimit removes the velocity limit for one account in
// one currency, along with its accumulated window events. Clearing a
// limit that was never set is a no-op. Structural, like
// SetVelocityLimit: no version bump.
func (l *Ledger) ClearVelocityLimit(a AccountID, currency string) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := velocityLimitKey{account: a, currency: currency}
	if _, ok := l.velocityLimits[key]; !ok {
		return
	}
	delete(l.velocityLimits, key)
	delete(l.velocityEvents, key)
	l.emitAudit(AuditEvent{
		Op:       "velocity_limit_cleared",
		Actor:    "ClearVelocityLimit",
		Accounts: []AccountID{a},
		Details: map[string]any{
			"account":  string(a),
			"currency": currency,
		},
	})
}

// VelocityLimit reports the configured velocity limit for one
// account in one currency, and whether a limit is configured at all.
func (l *Ledger) VelocityLimit(a AccountID, currency string) (VelocityLimit, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	lim, ok := l.velocityLimits[velocityLimitKey{account: a, currency: currency}]
	return lim, ok
}

// WithVelocityLimit configures a velocity limit at construction time
// (see SetVelocityLimit). Invalid configuration panics — fail-fast at
// construction, like the daily-limit and budget options — so a
// misconfigured limit can never silently under-enforce.
func WithVelocityLimit(a AccountID, currency string, window time.Duration, maxCount, maxAmountCents int64) Option {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		panic(fmt.Sprintf("ledger: invalid velocity limit currency %q: %v", currency, err))
	}
	if err := checkVelocityLimit(a, currency, window, maxCount, maxAmountCents); err != nil {
		panic(fmt.Sprintf("ledger: %v", err))
	}
	return func(l *Ledger) {
		l.velocityLimits[velocityLimitKey{account: a, currency: currency}] = VelocityLimit{
			Account: a, Currency: currency, Window: window,
			MaxCount: maxCount, MaxAmountCents: maxAmountCents,
		}
	}
}

// ParseVelocityLimits parses the LEDGER_VELOCITY_LIMITS environment
// variable into velocity limit configs:
//
//	"<account>:<currency>:<window>:<maxCount>:<maxAmountCents>[,...]"
//
// e.g. "cust-123:USD:10m:50:100000,cust-456:EUR:24h:500:5000000" caps
// cust-123's USD movement in any rolling 10 minutes at 50 postings /
// $1000 per direction, and cust-456's EUR movement in any rolling 24
// hours at 500 postings / €50000 per direction. Windows use
// time.ParseDuration syntax ("10m", "24h"). A maxCount /
// maxAmountCents of 0 or less disables that dimension. Parsing is
// strict — a malformed segment is an error, so a misconfigured
// deployment fails fast at startup instead of silently running
// without its risk control.
func ParseVelocityLimits(raw string) ([]VelocityLimit, error) {
	fail := func(format string, args ...any) ([]VelocityLimit, error) {
		return nil, fmt.Errorf("ledger: invalid velocity limits %q: "+format, append([]any{raw}, args...)...)
	}
	if raw == "" {
		return nil, nil
	}
	var out []VelocityLimit
	for _, seg := range strings.Split(raw, ",") {
		parts := strings.Split(seg, ":")
		if len(parts) != 5 {
			return fail("segment %q must be \"<account>:<currency>:<window>:<maxCount>:<maxAmountCents>\"", seg)
		}
		account := strings.TrimSpace(parts[0])
		if account == "" {
			return fail("segment %q has an empty account", seg)
		}
		currency, err := normalizeCurrency(strings.TrimSpace(parts[1]))
		if err != nil {
			return fail("segment %q: %v", seg, err)
		}
		window, err := time.ParseDuration(strings.TrimSpace(parts[2]))
		if err != nil {
			return fail("segment %q: window %q must be a Go duration (e.g. 10m, 24h)", seg, parts[2])
		}
		maxCount, err := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		if err != nil {
			return fail("segment %q: maxCount %q must be an integer", seg, parts[3])
		}
		maxAmount, err := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		if err != nil {
			return fail("segment %q: maxAmountCents %q must be an integer", seg, parts[4])
		}
		if err := checkVelocityLimit(AccountID(account), currency, window, maxCount, maxAmount); err != nil {
			return fail("segment %q: %v", seg, err)
		}
		out = append(out, VelocityLimit{
			Account: AccountID(account), Currency: currency, Window: window,
			MaxCount: maxCount, MaxAmountCents: maxAmount,
		})
	}
	return out, nil
}

// velocityUsageLocked folds the event deque for key down to the
// sliding window (at-window, at] and returns the posting count and
// total cents in the requested direction. The deque is pruned in
// place to the window, so repeated checks stay O(window). Callers
// must hold l.mu.
func (l *Ledger) velocityUsageLocked(key velocityLimitKey, dir VelocityDirection, window time.Duration, at time.Time) (count int64, cents int64) {
	events := l.velocityEvents[key]
	cutoff := at.Add(-window)
	kept := events[:0]
	wantInflow := dir == VelocityInflow
	for _, ev := range events {
		if ev.at.After(cutoff) {
			kept = append(kept, ev)
			if ev.inflow == wantInflow {
				count++
				cents = satAddInt64(cents, ev.cents)
			}
		}
	}
	// Nil out the dropped tail so pruned events don't pin memory.
	for i := len(kept); i < len(events); i++ {
		events[i] = velocityEvent{}
	}
	l.velocityEvents[key] = kept
	return count, cents
}

// velocityRejectedLocked reports whether posting amount cents in the
// given direction for account/currency at time at would exceed the
// configured velocity limit. The comparison never adds used + amount,
// so it cannot overflow: amount > limit rejects immediately,
// otherwise used > limit-amount is equivalent with non-negative
// sides. On rejection it emits a velocity_limit_breach audit event
// with the figures. Callers must hold l.mu (either lock suffices for
// the audit enqueue, which never blocks).
func (l *Ledger) velocityRejectedLocked(a AccountID, currency string, dir VelocityDirection, amount int64, at time.Time, op string) error {
	// Currency is normalized by the callers' validation paths; the
	// lookup tolerates the raw form via the same normalization.
	if currency == "" {
		currency = DefaultCurrency
	}
	key := velocityLimitKey{account: a, currency: currency}
	lim, ok := l.velocityLimits[key]
	if !ok {
		return nil
	}
	count, cents := l.velocityUsageLocked(key, dir, lim.Window, at)
	if lim.MaxCount > 0 && count+1 > lim.MaxCount {
		l.emitAudit(AuditEvent{
			Op:       "velocity_limit_breach",
			Actor:    op,
			Accounts: []AccountID{a},
			Details: map[string]any{
				"account":          string(a),
				"currency":         currency,
				"direction":        string(dir),
				"window":           lim.Window.String(),
				"dimension":        "count",
				"max_count":        lim.MaxCount,
				"window_count":     count,
				"attempted":        true,
			},
		})
		return fmt.Errorf("%w: %s %s %s window %v already has %d postings (max %d)",
			ErrVelocityLimitExceeded, a, currency, dir, lim.Window, count, lim.MaxCount)
	}
	if lim.MaxAmountCents > 0 {
		if amount > lim.MaxAmountCents || cents > lim.MaxAmountCents-amount {
			l.emitAudit(AuditEvent{
				Op:       "velocity_limit_breach",
				Actor:    op,
				Accounts: []AccountID{a},
				Details: map[string]any{
					"account":           string(a),
					"currency":          currency,
					"direction":         string(dir),
					"window":            lim.Window.String(),
					"dimension":         "amount",
					"max_amount_cents":  lim.MaxAmountCents,
					"window_cents":      cents,
					"attempted_cents":   amount,
				},
			})
			return fmt.Errorf("%w: %s %s %s window %v already moved %d cents (max %d)",
				ErrVelocityLimitExceeded, a, currency, dir, lim.Window, cents, lim.MaxAmountCents)
		}
	}
	return nil
}

// recordVelocityLocked appends one journal posting's contribution to
// the velocity windows of the accounts with a configured limit: the
// debit account records an inflow event, the credit account an
// outflow event. It is called from commitEntryLocked — the single
// funnel every journal write passes through — so batch, sweep,
// reversal, hold-capture, FX, and system postings all feed the
// windows, while idempotent replays (which skip the commit) never
// do. Events for accounts without a configured limit are not
// recorded, keeping memory bounded by the configured windows.
// Callers must hold the write lock.
func (l *Ledger) recordVelocityLocked(debit, credit AccountID, currency string, amount int64, at time.Time) {
	if len(l.velocityLimits) == 0 {
		return
	}
	if at.IsZero() {
		at = l.velocityClock()
	}
	if _, ok := l.velocityLimits[velocityLimitKey{account: debit, currency: currency}]; ok {
		l.appendVelocityEventLocked(velocityLimitKey{account: debit, currency: currency}, velocityEvent{at: at, inflow: true, cents: amount})
	}
	if _, ok := l.velocityLimits[velocityLimitKey{account: credit, currency: currency}]; ok {
		l.appendVelocityEventLocked(velocityLimitKey{account: credit, currency: currency}, velocityEvent{at: at, inflow: false, cents: amount})
	}
}

// appendVelocityEventLocked appends one event and prunes the deque to
// the configured window, so the deque never grows past the window's
// activity. Callers must hold l.mu.
func (l *Ledger) appendVelocityEventLocked(key velocityLimitKey, ev velocityEvent) {
	lim := l.velocityLimits[key]
	cutoff := ev.at.Add(-lim.Window)
	events := append(l.velocityEvents[key], ev)
	kept := events[:0]
	for _, e := range events {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	for i := len(kept); i < len(events); i++ {
		events[i] = velocityEvent{}
	}
	l.velocityEvents[key] = kept
}

// VelocityUsage is the current rolling-window usage for one account,
// currency, and direction, for operator observability
// (GET /velocity-limits/usage).
type VelocityUsage struct {
	Account        AccountID         `json:"account"`
	Currency       string            `json:"currency"`
	Direction      VelocityDirection `json:"direction"`
	Window         time.Duration     `json:"window"`
	WindowStart    time.Time         `json:"window_start"`
	WindowEnd      time.Time         `json:"window_end"`
	Count          int64             `json:"count"`
	AmountCents    int64             `json:"amount_cents"`
	MaxCount       int64             `json:"max_count"`
	MaxAmountCents int64             `json:"max_amount_cents"`
}

// VelocityUsageOf folds the current window for one account, currency,
// and direction as of the injected clock (or time.Now). It returns
// false when no limit is configured.
func (l *Ledger) VelocityUsageOf(a AccountID, currency string, dir VelocityDirection) (VelocityUsage, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := velocityLimitKey{account: a, currency: currency}
	lim, ok := l.velocityLimits[key]
	if !ok {
		return VelocityUsage{}, false
	}
	now := l.velocityClock()
	count, cents := l.velocityUsageLocked(key, dir, lim.Window, now)
	return VelocityUsage{
		Account: a, Currency: currency, Direction: dir,
		Window: lim.Window, WindowStart: now.Add(-lim.Window), WindowEnd: now,
		Count: count, AmountCents: cents,
		MaxCount: lim.MaxCount, MaxAmountCents: lim.MaxAmountCents,
	}, true
}

// velocityLimitsLocked returns every configured velocity limit,
// sorted by (account, currency), for the reconciliation report and
// the snapshot. Callers must hold l.mu.
func (l *Ledger) velocityLimitsLocked() []VelocityLimit {
	out := make([]VelocityLimit, 0, len(l.velocityLimits))
	for _, lim := range l.velocityLimits {
		out = append(out, lim)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// VelocityLimits returns every configured velocity limit, sorted by
// (account, currency).
func (l *Ledger) VelocityLimits() []VelocityLimit {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.velocityLimitsLocked()
}
