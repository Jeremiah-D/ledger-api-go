package ledger

import (
	"time"
)

// Balance-change notification hooks (LG-40).
//
// An opt-in callback for downstream fintech plumbing: when a committed
// operation moves money, every touched account's balance change is
// delivered to a caller-supplied hook — the building block for payment
// notifications, settlement webhooks, and cache invalidation. The hook
// is disabled by default (nil), so ledgers that never register one pay
// no cost and behave exactly as before.
//
// Dispatch semantics:
//   - One BalanceChange per touched (account, currency) with the net
//     delta of the operation: Post fires one event per leg; PostTransfer
//     fires one per touched account (the payer's delta nets amount +
//     fee); Capture, PostSweep, and PostMerge fire one per touched
//     account. Idempotent replays and rejected operations book nothing
//     and never fire.
//   - The events are fully materialized under the write lock, after the
//     atomic commit zone — the old/new balances, the ledger version, and
//     the trace ID describe the committed state, exactly like the audit
//     event of the same operation.
//   - Dispatch is asynchronous: each event is handed to its own
//     goroutine, so a slow hook can never block the commit path and the
//     committing call returns without waiting for any hook.
//   - Hook failures are isolated: a hook that panics is recovered, a
//     hook that overruns its timeout is abandoned — neither can alter or
//     roll back the committed entries. Callbacks must treat events as
//     unordered: events from concurrent operations interleave.
//   - Callbacks must not call back into the Ledger (deadlock: the write
//     lock is still held when the goroutines are spawned) and must be
//     safe for concurrent use — events from one operation are dispatched
//     in parallel.

// BalanceChange is one account's balance movement caused by a committed
// operation, delivered to a BalanceChangeHook. OldCents is the balance
// before the operation committed, NewCents the balance after, DeltaCents
// is NewCents - OldCents (negative for the paying side), Version is the
// ledger version after the commit, and Trace correlates the event with
// the operation (entry ID for Post, transfer/sweep/merge ID for those
// operations, hold ID for Capture).
type BalanceChange struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	OldCents   int64     `json:"old_balance_cents"`
	NewCents   int64     `json:"new_balance_cents"`
	DeltaCents int64     `json:"delta_cents"`
	Version    uint64    `json:"version"`
	Trace      string    `json:"trace"`
}

// BalanceChangeHook receives balance-change notifications. It is invoked
// asynchronously, at most once per (account, currency) per committed
// operation, and must be safe for concurrent use.
type BalanceChangeHook func(BalanceChange)

// defaultBalanceHookTimeout is the per-callback budget when no explicit
// timeout is configured: long enough for a local webhook fan-out, short
// enough that a wedged callback cannot hold dispatcher goroutines
// forever.
const defaultBalanceHookTimeout = 5 * time.Second

// WithBalanceChangeHook registers an opt-in balance-change notification
// hook (see BalanceChangeHook). A nil hook disables notifications: the
// default.
func WithBalanceChangeHook(hook BalanceChangeHook) Option {
	return func(l *Ledger) {
		l.balanceHook = hook
	}
}

// WithBalanceChangeHookTimeout sets the per-callback timeout for the
// balance-change hook: a callback that has not returned within d is
// abandoned (its goroutine may still finish on its own). Non-positive
// durations are ignored, keeping the current timeout. The default is
// 5 seconds.
func WithBalanceChangeHookTimeout(d time.Duration) Option {
	return func(l *Ledger) {
		if d > 0 {
			l.balanceHookTimeout = d
		}
	}
}

// SetBalanceChangeHook registers (or replaces) the balance-change hook at
// runtime, like the construction-time WithBalanceChangeHook. Passing nil
// disables notifications. Structural, like SetLowBalanceThreshold: it
// does not bump the ledger version.
func (l *Ledger) SetBalanceChangeHook(hook BalanceChangeHook) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.balanceHook = hook
}

// SetBalanceChangeHookTimeout changes the per-callback timeout at
// runtime, like WithBalanceChangeHookTimeout. Non-positive durations are
// ignored. Structural: no version bump.
func (l *Ledger) SetBalanceChangeHookTimeout(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d > 0 {
		l.balanceHookTimeout = d
	}
}

// BalanceChangeHookEnabled reports whether a balance-change hook is
// currently registered. Read-only.
func (l *Ledger) BalanceChangeHookEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balanceHook != nil
}

// balanceSnapshotLocked captures the pre-commit balances of the touched
// (account, currency) pairs so the post-commit dispatch can report
// old/new/delta. It returns nil when no hook is registered, so the commit
// path allocates nothing when the feature is off. Callers must hold the
// write lock, and must call it strictly before the first
// commitEntryLocked of the operation.
func (l *Ledger) balanceSnapshotLocked(touched []accountCurrency) map[accountCurrency]int64 {
	if l.balanceHook == nil {
		return nil
	}
	old := make(map[accountCurrency]int64, len(touched))
	for _, k := range touched {
		if _, ok := old[k]; !ok {
			old[k] = l.balances[k]
		}
	}
	return old
}

// fireBalanceHooksLocked builds one BalanceChange per touched
// (account, currency) from the pre-commit snapshot and the committed
// balances, and dispatches the registered hook asynchronously — one
// goroutine per event, each with its own timeout and panic isolation.
// The events are fully materialized before any goroutine starts, and the
// goroutines never touch the Ledger, so spawning them under the write
// lock cannot deadlock and the committing call returns without waiting
// for any callback. It is a no-op when no hook is registered or old is
// nil (the feature is off). Callers must hold the write lock, and must
// call it strictly after the last commitEntryLocked of the operation.
//
// version is read here, after the commit zone, so every event carries
// the ledger version the operation landed at.
func (l *Ledger) fireBalanceHooksLocked(touched []accountCurrency, old map[accountCurrency]int64, trace string) {
	if l.balanceHook == nil || old == nil {
		return
	}
	hook := l.balanceHook
	timeout := l.balanceHookTimeout
	if timeout <= 0 {
		timeout = defaultBalanceHookTimeout
	}
	version := l.version
	seen := make(map[accountCurrency]bool, len(old))
	events := make([]BalanceChange, 0, len(old))
	for _, k := range touched {
		if seen[k] {
			continue
		}
		seen[k] = true
		newBal := l.balances[k]
		oldBal := old[k]
		events = append(events, BalanceChange{
			Account:    k.account,
			Currency:   k.currency,
			OldCents:   oldBal,
			NewCents:   newBal,
			DeltaCents: newBal - oldBal,
			Version:    version,
			Trace:      trace,
		})
	}
	for _, ev := range events {
		go invokeBalanceHook(hook, ev, timeout)
	}
}

// invokeBalanceHook runs one hook callback with panic isolation and a
// timeout. A hook that panics is recovered here; a hook that overruns
// timeout is abandoned — either way the committed entries stand, and
// the caller (the commit path's dispatcher) never waits. A hook that
// outlives its timeout keeps running in the background; callbacks must
// therefore be idempotent and cheap to abandon.
func invokeBalanceHook(hook BalanceChangeHook, ev BalanceChange, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		defer func() {
			// Isolate hook panics: a crashing callback must not take
			// down the ledger process, and the commit it follows has
			// already landed.
			_ = recover()
			close(done)
		}()
		hook(ev)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
