package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Journal entry reversal (冲正).
//
// A reversal is the accounting correction for a posted journal entry that
// should not have happened (a fat-fingered amount, a duplicate posting
// discovered after the fact, a payment sent to the wrong account). Instead
// of deleting or editing the original — which would break the tamper-
// evident audit chain — a reversal appends a new journal entry with the
// debit and credit legs swapped, netting the original's balance effects to
// zero while leaving both entries visible in the journal forever.
//
// The reversal entry is fully determined by the original:
//   - ID:     "<original ID>/reversal" (deterministic: repeating the
//             reversal returns the existing entry instead of booking again)
//   - legs:   debit and credit swapped
//   - amount: the original amount, in the original currency
//   - memo:   "reversal_of:<original ID>" (searchable via GET /entries?memo=)
//   - time:   the reversal's own post time (now), not the original's — a
//             reversal corrects the books as of today, so the period gate
//             applies to the reversal's timestamp, and a reversal can never
//             reach back into a closed period.
//
// The original entry is never modified: the audit chain is append-only, so
// a reversal only appends a new link. Reversing a transfer reverses one
// journal entry at a time — a transfer with a fee leg needs its principal
// entry and its "<transfer ID>/fee" leg reversed individually, each by its
// own journal entry ID (POST /entries/{id}/reverse addresses journal
// entries, not transfers).
//
// A reversal runs the full Post risk gate order — idempotency replay, then
// frozen (403), overdraft (422), daily-limit (422), period (422) — because
// it books real money movement: the swapped legs move the amount back, and
// the same protections that guard a fresh posting guard the way back. It
// does not trigger journal rules (LG-47) and does not enter dual-control
// review (LG-42): a reversal is a deterministic correction of one known
// entry, not a new business posting, and the gates above already cover it.
// Like every other posting it fires the balance-change hook and the
// low-balance evaluation after commit.
//
// Reversing a reversal entry itself (POST /entries/<id>/reversal/reverse)
// is allowed: it re-books the original movement as its own audited entry,
// which is the transparent way to correct an erroneous reversal. What is
// rejected with ErrAlreadyReversed is a second reversal *of the same
// entry* carrying a *new* idempotency key — the deterministic ID means a
// retry (no key, or the same key) returns the existing reversal with
// duplicate == true instead of failing.

// Reversal errors.
var (
	// ErrEntryNotFound is returned when the journal entry to reverse does
	// not exist. It is a 404-class error: the caller named an entry ID the
	// ledger never journaled.
	ErrEntryNotFound = errors.New("ledger: journal entry not found")
	// ErrAlreadyReversed is returned when the entry already has a genuine
	// reversal ("<entry ID>/reversal" with memo "reversal_of:<entry ID>")
	// and the request carries a *new* idempotency key — i.e. the caller
	// signals a distinct reversal intent, not a retry. A retry (no key, or
	// the same key the reversal was created with) returns the existing
	// reversal with duplicate == true instead. It is a 409-class error:
	// the request conflicts with the entry's current state.
	ErrAlreadyReversed = errors.New("ledger: journal entry already reversed")
	// ErrReversalIDConflict is returned when "<entry ID>/reversal" is
	// already taken by a journal entry that is not a genuine reversal of
	// this entry (its memo is not "reversal_of:<entry ID>"): somebody
	// posted into the reversal ID namespace by hand. Booking the reversal
	// would silently overwrite that row, so the reversal is refused with
	// a 400-class error instead.
	ErrReversalIDConflict = errors.New("ledger: reversal ID already used by an unrelated journal entry")
	// ErrReversalOfPendingReview is returned when the entry to reverse is
	// marked PendingReview (see review.go): the entry is booked but not
	// effective, so a reversal — which *is* effective — would move money
	// the original never moved. Approve or reject the review first; the
	// review workflow is the correct way to cancel a pending transfer.
	// It is a 409-class error: the request conflicts with the entry's
	// current state.
	ErrReversalOfPendingReview = errors.New("ledger: cannot reverse a pending-review entry; approve or reject the review first")
)

// reversalMemoFor returns the memo stamped on every reversal entry. The
// "reversal_of:" prefix makes reversals searchable through
// GET /entries?memo=reversal_of: and lets ReverseEntry distinguish a
// genuine reversal from a hand-posted ID collision.
func reversalMemoFor(entryID string) string {
	return "reversal_of:" + entryID
}

// reversalRefPrefix is the external-reference prefix stamped on every
// reversal entry that inherits one (see reversalExternalRefFor).
const reversalRefPrefix = "reversal_of:"

// reversalExternalRefFor derives the external reference of the reversal
// of an entry: the "reversal_of:" prefix plus the original entry's
// reference, so the reversal stays correlatable with the corrected
// posting. An entry without a reference yields no inherited reference.
// The concatenated value must still fit maxExternalRefRunes UTF-8
// characters: an overlong inherited reference rejects the reversal with
// ErrExternalRefTooLong (the design truncates nothing — silently
// shortening a correlation identifier would corrupt downstream
// reconciliation).
func reversalExternalRefFor(origRef string) (string, error) {
	if origRef == "" {
		return "", nil
	}
	inherited := reversalRefPrefix + origRef
	if err := checkExternalRefLength(inherited); err != nil {
		return "", err
	}
	return inherited, nil
}

// reversalIDFor returns the deterministic journal entry ID of the
// reversal of entryID.
func reversalIDFor(entryID string) string {
	return entryID + "/reversal"
}

// isGenuineReversalOf reports whether e is the genuine reversal of
// entryID: the deterministic reversal ID *and* the reversal memo. Both
// must match — the ID alone could have been taken by a hand-posted entry
// (see ErrReversalIDConflict).
func isGenuineReversalOf(e JournalEntry, entryID string) bool {
	return e.ID == reversalIDFor(entryID) && e.Memo == reversalMemoFor(entryID)
}

// ReverseEntry appends a reversing journal entry for the journal entry
// with the given ID: same amount and currency, debit and credit legs
// swapped, memo "reversal_of:<entry ID>". The reversal ID is deterministic
// ("<entry ID>/reversal"), so the operation is idempotent.
//
// The check order mirrors Post: the original entry must exist
// (ErrEntryNotFound); a pending-review entry cannot be reversed
// (ErrReversalOfPendingReview); then the idempotency replay check — a
// repeated idempotency key returns the originally created entry with
// duplicate == true. When the genuine reversal already exists, a retry
// (empty key, or the same key the reversal was created with) returns it
// with duplicate == true, while a *new* idempotency key signals a
// distinct reversal intent and is rejected with ErrAlreadyReversed.
// After that come the risk gates in Post order: frozen (ErrAccountFrozen),
// overdraft on the swapped credit leg (ErrAccountOverdraft), the daily
// outflow limit on the same leg (ErrDailyLimitExceeded), and the period
// gate on the reversal's own timestamp (ErrPeriodClosed). A rejection
// records nothing: no journal row, no chain link, no version bump.
//
// The commit is atomic and append-only: the reversal's journal row,
// idempotency index entry, per-account index rows, balance and total
// updates, audit-chain link, and version bump land together under the one
// write lock; the original entry is never touched. Journal rules do not
// fire and the reversal never enters dual-control review (see the package
// comment). A zero CreatedAt is not a parameter — the reversal is always
// timestamped at post time.
func (l *Ledger) ReverseEntry(entryID string, idempotencyKey string) (reversal JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	orig, ok := l.entries[entryID]
	if !ok {
		return JournalEntry{}, false, ErrEntryNotFound
	}
	if orig.PendingReview {
		return JournalEntry{}, false, ErrReversalOfPendingReview
	}

	// Idempotency replay first, like Post: a repeated key returns the
	// entry the key created, whatever it was, without booking anything.
	if idempotencyKey != "" {
		if prev, ok := l.byKey[idempotencyKey]; ok {
			return prev, true, nil
		}
	}

	reversalID := reversalIDFor(entryID)
	if existing, taken := l.entries[reversalID]; taken {
		if !isGenuineReversalOf(existing, entryID) {
			return JournalEntry{}, false, ErrReversalIDConflict
		}
		// The genuine reversal already exists. A retry — no key, or the
		// same key the reversal was created with (which the byKey check
		// above already returned) — gets the existing entry back. A new
		// key is a distinct reversal intent for an already-reversed
		// entry, which the deterministic ID cannot express twice.
		if idempotencyKey == "" {
			return existing, true, nil
		}
		return JournalEntry{}, false, ErrAlreadyReversed
	}

	now := time.Now()
	inheritedRef, err := reversalExternalRefFor(orig.ExternalRef)
	if err != nil {
		return JournalEntry{}, false, err
	}
	reversal = JournalEntry{
		ID:             reversalID,
		DebitAccount:   orig.CreditAccount,
		CreditAccount:  orig.DebitAccount,
		AmountCents:    orig.AmountCents,
		Currency:       orig.Currency,
		IdempotencyKey: idempotencyKey,
		Memo:           reversalMemoFor(entryID),
		ExternalRef:    inheritedRef,
		CreatedAt:      now,
	}
	if err := validateJournalEntry(&reversal); err != nil {
		return JournalEntry{}, false, err
	}
	// External-reference uniqueness, like in Post: the inherited
	// reference is deterministic ("reversal_of:<orig ref>"), so a
	// collision can only come from a hand-posted row — but the journal
	// stays append-only, so it is still rejected rather than
	// overwritten.
	if inheritedRef != "" {
		if owner, taken := l.byRef[inheritedRef]; taken {
			return JournalEntry{}, false, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, inheritedRef, owner)
		}
	}

	// The risk gates run in Post order on the swapped legs: the reversal
	// moves real money back, so it earns the same protections as the
	// original posting. The frozen check runs after the idempotency
	// replay above, like Post — a replay books nothing new. Expired timed
	// freezes release lazily before the frozen gate, like in Post (see
	// FreezeUntil).
	l.releaseExpiredFreezesLocked(time.Now(), "write_path")
	if l.frozenLocked(reversal.DebitAccount) || l.frozenLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountFrozen
	}
	if l.overdraftRejectedLocked(reversal) {
		return JournalEntry{}, false, ErrAccountOverdraft
	}
	if l.dailyLimitRejectedLocked(reversal.CreditAccount, reversal.Currency, reversal.AmountCents, reversal.CreatedAt) {
		return JournalEntry{}, false, ErrDailyLimitExceeded
	}
	// The period gate is keyed on the reversal's own timestamp: the books
	// are corrected as of now, so a reversal whose post time falls in a
	// closed period is rejected — it can never rewrite a locked month.
	if err := l.periodRejectedLocked(reversal.CreatedAt); err != nil {
		return JournalEntry{}, false, err
	}

	// Atomic append-only commit: the reversal lands as one more journal
	// row, chain link, and version bump; the original entry is untouched.
	// Journal rules deliberately do not fire (see the package comment).
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	hookTouched := touchedAccounts([]JournalEntry{reversal})
	hookOld := l.balanceSnapshotLocked(hookTouched)
	l.commitEntryLocked(reversal)
	l.addDailyOutflowLocked(reversal.CreditAccount, reversal.Currency, reversal.AmountCents, reversal.CreatedAt)
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(hookTouched, reversalID, "ReverseEntry")
	l.emitAudit(AuditEvent{
		Op:            "reverse",
		Actor:         "ReverseEntry",
		TraceID:       reversalID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{reversalID},
		Accounts:      []AccountID{reversal.DebitAccount, reversal.CreditAccount},
		Details: map[string]any{
			"reversed_entry_id": entryID,
			"amount_cents":      reversal.AmountCents,
			"currency":          reversal.Currency,
		},
	})
	// Domain outbox: the reversal is itself a journal row (entry_posted),
	// plus the entry_reversed envelope naming the corrected entry.
	l.emitOutboxEntriesLocked([]JournalEntry{reversal})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxEntryReversed,
		Account:     reversal.DebitAccount,
		EntryID:     reversalID,
		AmountCents: reversal.AmountCents,
		Currency:    reversal.Currency,
		Details:     map[string]any{"reversed_entry_id": entryID},
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, reversalID)

	return reversal, false, nil
}
