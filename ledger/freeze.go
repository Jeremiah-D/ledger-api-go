package ledger

import (
	"errors"
	"sort"
	"time"
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

// ErrFreezeUntilNotFuture is returned by FreezeUntil when the requested
// automatic-unfreeze time is not in the future. A freeze that would
// expire immediately is almost certainly an operator mistake (a wrong
// clock or a stale timestamp), and silently installing a no-op freeze
// would hide it — the caller must pass a future time or the zero time
// for a manual freeze.
var ErrFreezeUntilNotFuture = errors.New("ledger: freeze auto-unfreeze time must be in the future")

// Freeze stops an account: subsequent Post calls that name the account as
// either leg are rejected with ErrAccountFrozen. Freezing is idempotent —
// freezing an already-frozen account changes nothing. Freezing an unknown
// account is allowed (the account may appear later) and takes effect
// immediately. Freeze does not bump the ledger version: balances are
// unchanged by a freeze, and version is the balance-change detector
// (see Snapshot).
//
// A manual Freeze clears any previously scheduled automatic-unfreeze
// deadline (see FreezeUntil): a manual freeze never auto-releases, so
// re-freezing a timed-frozen account manually converts it to a manual
// freeze.
func (l *Ledger) Freeze(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.freezeLocked(a, time.Time{})
}

// FreezeUntil freezes like Freeze, but schedules the freeze to lift
// automatically at until. A zero until is a manual freeze, identical to
// Freeze. A non-zero until must be in the future, otherwise
// ErrFreezeUntilNotFuture is returned and nothing changes.
//
// When the deadline passes, the freeze is released either lazily — every
// write path (Post, PostTransfer, PostBatch, ReverseEntry, Hold) checks
// for expired timed freezes before its frozen gate — or by the background
// freeze sweeper (see StartFreezeSweeper) or an explicit
// ExpireFrozenAccounts call. Every automatic release is recorded as a
// "frozen_auto_released" structured audit event and bumps the ledger's
// FrozenAutoReleases counter. Like Freeze, it does not bump the ledger
// version.
func (l *Ledger) FreezeUntil(a AccountID, until time.Time) error {
	if !until.IsZero() && !until.After(time.Now()) {
		return ErrFreezeUntilNotFuture
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.freezeLocked(a, until)
	return nil
}

// freezeLocked is the shared body of Freeze and FreezeUntil. A zero until
// deletes any scheduled deadline (manual freeze); a non-zero until
// installs it. Callers must hold the write lock.
func (l *Ledger) freezeLocked(a AccountID, until time.Time) {
	l.frozen[a] = true
	if until.IsZero() {
		delete(l.frozenUntil, a)
	} else {
		l.frozenUntil[a] = until
	}
	details := map[string]any(nil)
	if !until.IsZero() {
		details = map[string]any{"auto_unfreeze_at": until.UTC().Format(time.RFC3339)}
	}
	l.emitAudit(AuditEvent{
		Op:            "freeze",
		Actor:         "Freeze",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details:       details,
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxAccountFrozen,
		Account: a,
	})
	l.recordFreezeEventLocked(AccountFreezeEvent{
		Account:        a,
		Kind:           FreezeEventFrozen,
		At:             time.Now().UTC(),
		AutoUnfreezeAt: until,
	})
}

// Unfreeze lifts a freeze previously applied with Freeze. Unfreezing an
// account that was never frozen is a no-op. Like Freeze, it does not bump
// the ledger version.
func (l *Ledger) Unfreeze(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.frozen, a)
	// A manual unfreeze also drops any scheduled deadline: the account is
	// no longer frozen, so the deadline is meaningless.
	delete(l.frozenUntil, a)
	l.emitAudit(AuditEvent{
		Op:            "unfreeze",
		Actor:         "Unfreeze",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxAccountUnfrozen,
		Account: a,
	})
	l.recordFreezeEventLocked(AccountFreezeEvent{
		Account: a,
		Kind:    FreezeEventUnfrozen,
		At:      time.Now().UTC(),
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

// frozenUntilCopyLocked returns a copy of the timed-freeze deadline
// index for reports (see reconcile.go). Callers must hold l.mu.
func (l *Ledger) frozenUntilCopyLocked() map[AccountID]time.Time {
	out := make(map[AccountID]time.Time, len(l.frozenUntil))
	for a, until := range l.frozenUntil {
		out[a] = until
	}
	return out
}

// FreezeAutoUnfreezeAt reports the scheduled automatic-unfreeze time of
// an account frozen with FreezeUntil. The second return value is false
// for manually frozen accounts and for accounts that are not frozen.
func (l *Ledger) FreezeAutoUnfreezeAt(a AccountID) (time.Time, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.frozenUntil[a]
	return t, ok
}

// ExpireFrozenAccounts releases every timed freeze whose automatic-
// unfreeze deadline has passed. It is the manual/operator twin of the
// background freeze sweeper's tick (see StartFreezeSweeper): the same
// scan, the same audit events, the same counter. It returns the number
// of accounts released.
func (l *Ledger) ExpireFrozenAccounts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releaseExpiredFreezesLocked(time.Now(), "expire_frozen_accounts")
}

// releaseExpiredFreezesLocked unfreezes every account whose automatic-
// unfreeze deadline has passed as of now, and returns the number of
// accounts released. Each release is recorded as a "frozen_auto_released"
// structured audit event (flat version bracket, like freeze/unfreeze —
// unfreezing moves no money) and an account_unfrozen outbox event, and
// bumps the ledger's FrozenAutoReleases counter. The trigger names the
// caller ("write_path" for the lazy write-path check, "freeze_sweeper"
// for the background sweeper) and is recorded in the audit event's
// details. Callers must hold the write lock; the map is nil-safe.
func (l *Ledger) releaseExpiredFreezesLocked(now time.Time, trigger string) int {
	released := 0
	for a, until := range l.frozenUntil {
		if now.Before(until) {
			continue
		}
		delete(l.frozen, a)
		delete(l.frozenUntil, a)
		released++
		l.emitAudit(AuditEvent{
			Op:            "frozen_auto_released",
			Actor:         "FreezeAutoRelease",
			TraceID:       string(a),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{a},
			Details: map[string]any{
				"trigger":          trigger,
				"auto_unfreeze_at": until.UTC().Format(time.RFC3339),
			},
		})
		l.emitOutboxLocked(OutboxEvent{
			Type:    OutboxAccountUnfrozen,
			Account: a,
		})
		l.recordFreezeEventLocked(AccountFreezeEvent{
			Account: a,
			Kind:    FreezeEventAutoReleased,
			At:      now.UTC(),
			Trigger: trigger,
		})
	}
	if released > 0 {
		l.frozenAutoReleases.Add(uint64(released))
	}
	return released
}

// FrozenAutoReleases reports how many timed account freezes have been
// released automatically (see FreezeUntil): lazy write-path releases,
// background-sweeper releases, and explicit ExpireFrozenAccounts calls
// all count. Operational metric (surfaced on GET /metrics as
// ledger_freeze_auto_releases_total), not books: it is in-memory only
// and does not round-trip through snapshots.
func (l *Ledger) FrozenAutoReleases() uint64 {
	return l.frozenAutoReleases.Load()
}
