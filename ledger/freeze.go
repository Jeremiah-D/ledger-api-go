package ledger

import (
	"errors"
	"sort"
)

// ErrAccountFrozen is returned by Post when the debit or the credit leg of
// the entry touches a frozen account. A frozen account is a risk-control
// stop: no new money may move through it. Rejected posts record nothing,
// bump nothing, and leave the ledger version untouched — the freeze is
// about refusing new activity, not rewriting history.
//
// Ordering inside Post is deliberate: field validation runs first, then
// the idempotency-key replay check, then the frozen check. A replay of a
// key that was posted *before* the account was frozen returns the original
// entry (duplicate == true) instead of failing: the replay books nothing
// new, and breaking the idempotency contract on a frozen account would
// turn a risk control into a data-availability incident for callers that
// retry. Any post that would book a new entry through a frozen account is
// rejected.
//
// Reads are unaffected by design: Balance, Snapshot, TrialBalance,
// ListEntries, the audit chain, and Reconcile keep working on frozen
// accounts, so risk and reconciliation tooling can keep watching an
// account while it is stopped.
var ErrAccountFrozen = errors.New("ledger: account is frozen")

// Freeze stops an account: subsequent Post calls that name the account as
// either leg are rejected with ErrAccountFrozen. Freezing is idempotent —
// freezing an already-frozen account changes nothing. Freezing an unknown
// account is allowed (the account may appear later) and takes effect
// immediately. Freeze does not bump the ledger version: balances are
// unchanged by a freeze, and version is the balance-change detector
// (see Snapshot).
func (l *Ledger) Freeze(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frozen[a] = true
	l.emitAudit(AuditEvent{
		Op:            "freeze",
		Actor:         "Freeze",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
	})
}

// Unfreeze lifts a freeze previously applied with Freeze. Unfreezing an
// account that was never frozen is a no-op. Like Freeze, it does not bump
// the ledger version.
func (l *Ledger) Unfreeze(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.frozen, a)
	l.emitAudit(AuditEvent{
		Op:            "unfreeze",
		Actor:         "Unfreeze",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
	})
}

// IsFrozen reports whether the account is currently frozen. Reads and
// postings to other accounts are unaffected.
func (l *Ledger) IsFrozen(a AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.frozen[a]
}

// frozenLocked reports the frozen state of an account. Callers must hold
// l.mu (either lock suffices; Post holds the write lock).
func (l *Ledger) frozenLocked(a AccountID) bool {
	return l.frozen[a]
}

// frozenAccountsLocked returns the sorted list of currently frozen
// accounts. Callers must hold l.mu.
func (l *Ledger) frozenAccountsLocked() []AccountID {
	out := make([]AccountID, 0, len(l.frozen))
	for a := range l.frozen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
