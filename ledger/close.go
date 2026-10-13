package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrAccountClosed is returned by every write path that would book a new
// entry through a closed account: Post, PostBatch, PostTransfer (both
// principal and fee legs), FX transfers, reversals, sweeps, merges,
// holds and hold captures, closing entries, revaluation entries, and
// journal-rule derived legs. A closed account is a terminal lifecycle
// state, unlike the reversible risk-control freeze: closing is
// irreversible, and rejected postings record nothing, bump nothing, and
// leave the ledger version untouched — the close is about refusing new
// activity, not rewriting history.
//
// Ordering inside Post is deliberate: the closed check runs right after
// the idempotency replay check (a replay of a key posted before the
// closure returns the original entry instead of failing, because the
// replay books nothing new) and before the frozen check. Reads are
// unaffected by design: Balance, Snapshot, TrialBalance, ListEntries,
// the audit chain, and Reconcile keep working on closed accounts, so
// risk and reconciliation tooling can keep watching a closed account
// while it is stopped.
var ErrAccountClosed = errors.New("ledger: account is closed")

// AccountClosure is the terminal-state record of one account's lifecycle
// close (see CloseAccount). It is structural config, like the
// frozen-account list: closing never bumps the ledger version and
// survives snapshots.
type AccountClosure struct {
	Account  AccountID `json:"account"`
	ClosedAt time.Time `json:"closed_at"`
}

// CloseAccount closes an account: from this point on, every write path
// that would book a new journal entry through the account is rejected
// with ErrAccountClosed. Closing is irreversible — there is no unclose —
// and idempotent: closing an already-closed account returns the original
// closure record with alreadyClosed == true and changes nothing. Closing
// an unknown account is allowed (the account may appear later; the close
// is advisory until then) and takes effect immediately.
//
// With cascade == true, every descendant in the sub-account hierarchy
// (see SetParent) is closed in the same call: the whole subtree becomes
// terminal at once, which is the merchant-offboarding path. Each closed
// account gets its own AccountClosure record with the same ClosedAt; the
// returned record is the root account's. A cascade that finds no children
// closes just the root.
//
// The closure does not bump the ledger version (balances are unchanged)
// and does not touch the audit chain: it is lifecycle state, not
// bookkeeping. It is recorded as an "account_close" structured audit
// event naming every closed account, and as one "account_closed" outbox
// event per closed account, so downstream consumers can react (e.g.
// retire the account from onboarding UIs).
func (l *Ledger) CloseAccount(a AccountID, cascade bool) (closure AccountClosure, alreadyClosed bool, err error) {
	return l.CloseAccountCtx(context.Background(), a, cascade)
}

// CloseAccountCtx is CloseAccount with per-call audit actor attribution
// (LG-73): the account_close audit event records the operator carried by
// ctx when set, and the entrypoint name otherwise.
func (l *Ledger) CloseAccountCtx(ctx context.Context, a AccountID, cascade bool) (closure AccountClosure, alreadyClosed bool, err error) {
	if a == "" {
		return AccountClosure{}, false, ErrEmptyAccountID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.closed[a]; ok {
		// Idempotent replay of the close: the account is already
		// terminal, so the operator's retry gets the original record
		// back instead of an error. Like every replay path, it books
		// nothing new.
		return c, true, nil
	}
	now := time.Now().UTC()
	targets := []AccountID{a}
	if cascade {
		targets = l.closeCascadeTargetsLocked(a)
	}
	for _, t := range targets {
		if _, ok := l.closed[t]; !ok {
			l.closed[t] = AccountClosure{Account: t, ClosedAt: now}
		}
	}
	details := map[string]any{
		"closed_at": now.Format(time.RFC3339),
		"cascade":   cascade,
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "account_close",
		Actor:         "CloseAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      targets,
		Details:       details,
	})
	for _, t := range targets {
		l.emitOutboxLocked(OutboxEvent{
			Type:    OutboxAccountClosed,
			Account: t,
		})
	}
	return l.closed[a], false, nil
}

// closeCascadeTargetsLocked returns the root plus every descendant of a
// in the sub-account hierarchy (see rollupLocked), sorted: the cascade
// closes the whole subtree. Callers must hold the write lock.
func (l *Ledger) closeCascadeTargetsLocked(a AccountID) []AccountID {
	children := make(map[AccountID][]AccountID, len(l.parents))
	for child, parent := range l.parents {
		children[parent] = append(children[parent], child)
	}
	// The parent map is cycle-free by construction (SetParent rejects
	// cycles); the seen guard is belt-and-braces against a corrupted map.
	seen := map[AccountID]bool{a: true}
	queue := []AccountID{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	targets := make([]AccountID, 0, len(seen))
	for t := range seen {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	return targets
}

// IsClosed reports whether the account is closed. Reads are unaffected.
func (l *Ledger) IsClosed(a AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.closedLocked(a)
}

// closedLocked reports the closure state of an account. Callers must hold
// l.mu (either lock suffices; write paths hold the write lock).
func (l *Ledger) closedLocked(a AccountID) bool {
	_, ok := l.closed[a]
	return ok
}

// closedAccountsLocked returns the sorted list of currently closed
// accounts. Callers must hold l.mu.
func (l *Ledger) closedAccountsLocked() []AccountID {
	out := make([]AccountID, 0, len(l.closed))
	for a := range l.closed {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// closedCopyLocked returns a copy of the closure records for snapshots
// (see ExportSnapshot). Callers must hold l.mu.
func (l *Ledger) closedCopyLocked() []AccountClosure {
	out := make([]AccountClosure, 0, len(l.closed))
	for _, c := range l.closed {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}
