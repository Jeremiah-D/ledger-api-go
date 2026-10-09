package ledger

import (
	"errors"
	"sort"
	"time"
)

// Validation errors returned by period operations.
var (
	// ErrPeriodClosed is returned by every journal-writing operation when
	// the entry's timestamp falls in a closed accounting period. A closed
	// period is locked: no new entry may be booked with a timestamp in
	// that month, so a closed month's books cannot change after the fact.
	// Rejected postings record nothing — no journal row, no chain link,
	// no version bump — exactly like the other risk controls.
	ErrPeriodClosed = errors.New("ledger: accounting period is closed")
	// ErrInvalidPeriodID is returned by ClosePeriod/ReopenPeriod when the
	// period ID is not a valid "YYYY-MM" UTC month.
	ErrInvalidPeriodID = errors.New("ledger: period ID must be \"YYYY-MM\"")
)

// periodIDOf returns the accounting period a journal entry with timestamp
// t lands in: the UTC calendar month, "2006-01". Months are the compliance
// boundary — a period close locks every posting whose timestamp falls in
// that month, including backdated entries. UTC keeps the boundary
// deterministic regardless of the process timezone.
func periodIDOf(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// validatePeriodID rejects anything that is not a "YYYY-MM" UTC month.
func validatePeriodID(id string) error {
	if _, err := time.Parse("2006-01", id); err != nil {
		return ErrInvalidPeriodID
	}
	return nil
}

// ClosePeriod locks the accounting period id: journal entries whose
// timestamp falls in that month are rejected with ErrPeriodClosed by every
// journal-writing operation (Post, PostTransfer, PostBatch, PostSweep,
// PostMerge, hold Capture). Closing is idempotent — closing an
// already-closed period changes nothing. Closing a period with no entries
// yet is allowed: the lock is about the time boundary, not about existing
// activity, so operators can lock a month before any backdated posting
// arrives. The current and future months can be closed too — an operator
// who closes the current month opts into rejecting all new postings dated
// now, which is the documented behavior of the lock, not an accident.
//
// ClosePeriod does not bump the ledger version: it records no money
// movement, and version is the balance-change detector (see Snapshot).
// The close is recorded in the structured audit log's hash chain (op
// "period_close") so the lock action itself is tamper-evident; it is not
// part of the journal entry chain (chain.go), which records journaled
// entries only.
func (l *Ledger) ClosePeriod(id string) error {
	if err := validatePeriodID(id); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closedPeriods[id] = true
	l.emitAudit(AuditEvent{
		Op:            "period_close",
		Actor:         "ClosePeriod",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"closed": true,
		},
	})
	return nil
}

// ReopenPeriod unlocks a period previously closed with ClosePeriod, so
// postings dated in that month are accepted again. Reopening a period that
// was never closed is a no-op. Like ClosePeriod it does not bump the
// version, and the reopen is recorded in the audit log's hash chain (op
// "period_reopen").
func (l *Ledger) ReopenPeriod(id string) error {
	if err := validatePeriodID(id); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.closedPeriods, id)
	l.emitAudit(AuditEvent{
		Op:            "period_reopen",
		Actor:         "ReopenPeriod",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"closed": false,
		},
	})
	return nil
}

// IsPeriodClosed reports whether the accounting period id is currently
// locked.
func (l *Ledger) IsPeriodClosed(id string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.closedPeriods[id]
}

// ClosedPeriods returns the sorted list of currently locked accounting
// periods.
func (l *Ledger) ClosedPeriods() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.closedPeriodsLocked()
}

// closedPeriodsLocked returns the sorted list of currently locked periods.
// Callers must hold l.mu; the read lock suffices. The reconciliation scan
// and the snapshot export call this directly so they see the same state
// under the same lock.
func (l *Ledger) closedPeriodsLocked() []string {
	out := make([]string, 0, len(l.closedPeriods))
	for id := range l.closedPeriods {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// periodRejectedLocked returns ErrPeriodClosed when a journal entry with
// timestamp at would land in a closed accounting period. Callers must run
// their idempotency replay check first: a replay books nothing new, so it
// must not fail on a period that closed after the original posting — the
// same replay-first ordering Post uses for frozen accounts. Callers must
// hold l.mu.
func (l *Ledger) periodRejectedLocked(at time.Time) error {
	if l.closedPeriods[periodIDOf(at)] {
		return ErrPeriodClosed
	}
	return nil
}
