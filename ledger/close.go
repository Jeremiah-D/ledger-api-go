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
// journal-rule derived legs. A closed account is a lifecycle stop: it
// refuses new activity until reopened with ReopenAccount (LG-362) —
// unlike the freeze, which is a risk-control stop lifted by Unfreeze,
// a close is lifted only by an explicit reopen. Rejected postings
// record nothing, bump nothing, and leave the ledger version untouched
// — the close is about refusing new activity, not rewriting history.
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

// AccountClosure is the lifecycle-close record of one account (see
// CloseAccount, reversed by ReopenAccount). It is structural config,
// like the frozen-account list: closing never bumps the ledger version
// and survives snapshots.
type AccountClosure struct {
	Account  AccountID `json:"account"`
	ClosedAt time.Time `json:"closed_at"`
}

// CloseAccount closes an account: from this point on, every write path
// that would book a new journal entry through the account is rejected
// with ErrAccountClosed. Closing is reversible via ReopenAccount
// (LG-362), and idempotent: closing an already-closed account returns
// the original closure record with alreadyClosed == true and changes
// nothing. Closing an unknown account is allowed (the account may
// appear later; the close is advisory until then) and takes effect
// immediately.
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

// ReopenAccount reopens an account previously closed with CloseAccount,
// restoring its write paths immediately: every write path reads the live
// closed map, so a reopened account accepts new postings at once. The
// reopen is the inverse of CloseAccount (LG-362): the merchant-offboarding
// reversal and the operator-error recovery path — a merchant that returns
// after going offline, or an account closed by mistake, comes back
// without rebuilding its aliases, budgets, KYC, and authorizations
// under a new account.
//
// Reopening an account that was never closed is an idempotent no-op
// returning wasClosed == false and no audit event; reopening an unknown
// account is likewise a no-op — there is nothing to restore. The
// returned record is the deleted original closure.
//
// With cascade == true, every descendant in the sub-account hierarchy
// (see SetParent) is reopened in the same call: the whole subtree comes
// back at once, and the returned record is the root account's. With
// cascade == false only the root reopens — a descendant closed
// independently stays closed.
//
// The reopen does not bump the ledger version (balances are unchanged),
// does not touch the journal, and does not resurrect postings rejected
// while the account was closed (they were never booked). It is recorded
// as an "account_reopen" structured audit event naming every reopened
// account, and as one "account_reopened" outbox event per reopened
// account, so downstream consumers can react. Freezing and dormancy are
// orthogonal: reopening a frozen account leaves it frozen, and
// reopening a dormant account leaves the dormancy mark in place.
func (l *Ledger) ReopenAccount(a AccountID, cascade bool) (closure AccountClosure, wasClosed bool, err error) {
	return l.ReopenAccountCtx(context.Background(), a, cascade)
}

// ReopenAccountCtx is ReopenAccount with per-call audit actor attribution
// (LG-73): the account_reopen audit event records the operator carried by
// ctx when set, and the entrypoint name otherwise.
func (l *Ledger) ReopenAccountCtx(ctx context.Context, a AccountID, cascade bool) (closure AccountClosure, wasClosed bool, err error) {
	if a == "" {
		return AccountClosure{}, false, ErrEmptyAccountID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	orig, ok := l.closed[a]
	if !ok {
		// Idempotent no-op: the account is not closed, so there is
		// nothing to restore — like closing an already-closed
		// account, a reopen of a live account changes nothing and
		// audits nothing.
		return AccountClosure{}, false, nil
	}
	targets := []AccountID{a}
	if cascade {
		targets = l.closeCascadeTargetsLocked(a)
	}
	reopened := make([]AccountID, 0, len(targets))
	for _, t := range targets {
		if _, ok := l.closed[t]; ok {
			delete(l.closed, t)
			reopened = append(reopened, t)
		}
	}
	now := time.Now().UTC()
	details := map[string]any{
		"closed_at":   orig.ClosedAt.Format(time.RFC3339),
		"reopened_at": now.Format(time.RFC3339),
		"cascade":     cascade,
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "account_reopen",
		Actor:         "ReopenAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      reopened,
		Details:       details,
	})
	for _, t := range reopened {
		l.emitOutboxLocked(OutboxEvent{
			Type:    OutboxAccountReopened,
			Account: t,
		})
	}
	return orig, true, nil
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
