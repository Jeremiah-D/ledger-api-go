package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Account suspension (LG-120): a reversible operational pause on one
// account.
//
// A suspended account rejects every write path that would book a new
// journal entry through it with ErrAccountSuspended: Post (and every
// system posting that funnels through postLockedGated — accruals,
// dividends, dormancy fees, ECL, escheat, capitalization, credit-line
// interest), PostBatch, PostTransfer (principal, fee, and split legs,
// FX transfers), reversals, holds and captures, sweeps, merges, closing
// entries, revaluation entries, term-deposit opens, allocations, and
// journal-rule derived legs. Rejected postings record nothing, bump
// nothing, and leave the ledger version untouched — a suspension is
// about refusing new activity, not rewriting history.
//
// Ordering inside every gate is deliberate: the closed check runs first
// (terminal lifecycle state), then dormant, then the frozen risk-control
// stop, then the suspended check, then overdraft. The three states are
// orthogonal and can stack: an account may be frozen *and* suspended
// (the frozen error wins on a double-set account, preserving the
// pre-suspension observable behavior), and a suspension never
// unfreezes, uncloses, or closes anything. Suspending a closed account
// is allowed but changes nothing observable — the close still rejects
// first.
//
// Suspension is reversible, unlike CloseAccount: ResumeAccount lifts it.
// It is idempotent, like CloseAccount: suspending an already-suspended
// account returns the original record with alreadySuspended == true and
// emits nothing new (no duplicate audit event, no duplicate outbox
// event, no metric bump), so operator retries are safe. Suspending an
// unknown account is allowed (the account may appear later) and takes
// effect immediately; an empty account ID is rejected with
// ErrEmptyAccountID.
//
// Reads are unaffected by design: Balance, Snapshot, TrialBalance,
// ListEntries, the audit chain, and Reconcile keep working on suspended
// accounts, so risk and reconciliation tooling can keep watching an
// account while it is paused. The suspend/resume transitions themselves
// are structural config, like freezes and closures: they never bump the
// journal version, they survive disaster-recovery snapshots (see
// snapshot.go), and each transition is recorded as a structured audit
// event ("suspend"/"resume") plus an outbox domain event
// ("account_suspended"/"account_resumed").
//
// Hold *release* is exempt from the suspended gate, exactly like the
// frozen gate: releasing frees reserved funds without booking a journal
// entry, so it is not a write path in the journal sense. The suspend
// and resume operations themselves are structural and never
// self-block: a suspended account can always be resumed.
var ErrAccountSuspended = errors.New("ledger: account is suspended")

// AccountSuspension is the suspension record of one account (see
// SuspendAccount). It is structural config, like the frozen-account
// list and the account closures: suspending never bumps the journal
// version and survives snapshots.
type AccountSuspension struct {
	Account     AccountID `json:"account"`
	SuspendedAt time.Time `json:"suspended_at"`
}

// SuspendAccount suspends an account: from this point on, every write
// path that would book a new journal entry through the account is
// rejected with ErrAccountSuspended. Suspension is reversible — see
// ResumeAccount — and idempotent: suspending an already-suspended
// account returns the original record with alreadySuspended == true
// and changes nothing (no new audit event, no new outbox event).
//
// Suspending an unknown account is allowed (the account may appear
// later; the suspension is advisory until then) and takes effect
// immediately. An empty account ID is rejected with ErrEmptyAccountID.
//
// The suspension does not bump the ledger version (balances are
// unchanged) and does not touch the audit chain: it is operational
// state, not bookkeeping. It is recorded as a "suspend" structured
// audit event and as one "account_suspended" outbox event, so
// downstream consumers can react (e.g. pause the account's payment
// rails).
func (l *Ledger) SuspendAccount(a AccountID) (suspension AccountSuspension, alreadySuspended bool, err error) {
	return l.SuspendAccountCtx(context.Background(), a)
}

// SuspendAccountCtx is SuspendAccount with per-call audit actor
// attribution (LG-73): the suspend audit event records the operator
// carried by ctx when set, and the entrypoint name otherwise.
func (l *Ledger) SuspendAccountCtx(ctx context.Context, a AccountID) (AccountSuspension, bool, error) {
	if a == "" {
		return AccountSuspension{}, false, ErrEmptyAccountID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.suspended[a]; ok {
		// Idempotent replay of the suspension: the account is already
		// paused, so the operator's retry gets the original record
		// back instead of a duplicate transition. Like every replay
		// path, it books nothing new and emits nothing new.
		return s, true, nil
	}
	now := time.Now().UTC()
	s := AccountSuspension{Account: a, SuspendedAt: now}
	l.suspended[a] = s
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "suspend",
		Actor:         "SuspendAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"suspended_at": now.Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxAccountSuspended,
		Account: a,
	})
	return s, false, nil
}

// ResumeAccount lifts a suspension previously applied with
// SuspendAccount. Resuming an account that is not suspended is a no-op
// returning false. Like SuspendAccount, it does not bump the ledger
// version; the resume is recorded as a "resume" structured audit event
// and an "account_resumed" outbox event.
func (l *Ledger) ResumeAccount(a AccountID) bool {
	return l.ResumeAccountCtx(context.Background(), a)
}

// ResumeAccountCtx is ResumeAccount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) ResumeAccountCtx(ctx context.Context, a AccountID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.suspended[a]; !ok {
		return false
	}
	delete(l.suspended, a)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "resume",
		Actor:         "ResumeAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxAccountResumed,
		Account: a,
	})
	return true
}

// IsSuspended reports whether the account is currently suspended.
// Reads and postings to other accounts are unaffected.
func (l *Ledger) IsSuspended(a AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.suspendedLocked(a)
}

// suspendedLocked reports the suspended state of an account. Callers
// must hold l.mu (either lock suffices; the write paths hold the write
// lock).
func (l *Ledger) suspendedLocked(a AccountID) bool {
	_, ok := l.suspended[a]
	return ok
}

// suspendedAccountsLocked returns the sorted list of currently
// suspended accounts. Callers must hold l.mu.
func (l *Ledger) suspendedAccountsLocked() []AccountID {
	out := make([]AccountID, 0, len(l.suspended))
	for a := range l.suspended {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// suspendedCopyLocked returns a copy of the suspension records for
// reports and snapshots (see snapshot.go). Callers must hold l.mu.
func (l *Ledger) suspendedCopyLocked() []AccountSuspension {
	out := make([]AccountSuspension, 0, len(l.suspended))
	for _, s := range l.suspended {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}
