package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
//   - link:   a reversal_of entry link (see links.go, LG-123) from the
//             reversal entry to the original — the tamper-evident twin of
//             the memo above, declared atomically with the reversal so the
//             correction and its association can never diverge.
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
//
// Partial reversals (see ReverseEntryPartial) correct an entry in
// several smaller steps: each books a deterministic
// "<entry ID>/reversal/<n>" entry for a caller-chosen slice of the
// original amount. The net outstanding correction — genuine reversals
// minus their own reversal-of-reversal corrections, so a corrected
// partial frees its slice again — can never exceed the original
// amount; anything over the cap is rejected with ErrAlreadyReversed. A
// full reversal is admitted only when nothing of the entry is currently
// reversed (it would over-correct otherwise): correct the outstanding
// partials individually first. Partial reversals run the same full
// Post risk-gate order as full reversals and are counted separately on
// GET /metrics (ledger_reversals_total{kind="partial"}).

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
// ("<entry ID>/reversal"), so the operation is idempotent. The reversal
// carries no metadata; see ReverseEntryWithMetadata for caller-supplied
// structured metadata on the reversal entry.
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
	return l.ReverseEntryWithMetadata(entryID, idempotencyKey, nil)
}

// ReverseEntryWithMetadata is ReverseEntry with caller-supplied
// structured metadata (see JournalEntry.Metadata): the metadata is
// stamped on the reversal journal entry, like the inherited external
// reference. The original entry's metadata is not inherited — only the
// metadata given here is journaled. Invalid metadata rejects the
// reversal with ErrInvalidMetadata before anything is recorded.
func (l *Ledger) ReverseEntryWithMetadata(entryID string, idempotencyKey string, metadata map[string]string) (reversal JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reverseEntryLocked(entryID, idempotencyKey, metadata)
}

// reverseEntryLocked is the shared implementation of ReverseEntry and
// ReverseEntryWithMetadata. Callers must hold l.mu (the write lock).
func (l *Ledger) reverseEntryLocked(entryID string, idempotencyKey string, metadata map[string]string) (reversal JournalEntry, duplicate bool, err error) {
	// Metadata is field validation, like the legs and the amount: an
	// invalid map is rejected before the entry lookup or the replay
	// checks, so a malformed request fails fast regardless of the
	// ledger's reversal state.
	if err := checkMetadata(metadata); err != nil {
		return JournalEntry{}, false, err
	}

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

	// A full reversal nets the entry's whole amount, so it is admitted
	// only when nothing of the entry is currently reversed: a genuine
	// partial reversal already corrects part of this entry, and a full
	// reversal on top would over-correct. Reversal-of-reversal
	// corrections net back (see netReversedLocked), so correcting the
	// outstanding partials individually re-opens the full reversal
	// path. Anything outstanding is rejected with ErrAlreadyReversed.
	if l.netReversedLocked(entryID) != 0 {
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
		Attachments:    append([]string(nil), orig.Attachments...),
		Metadata:       metadata,
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
	if l.closedLocked(reversal.DebitAccount) || l.closedLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountClosed
	}
	if l.frozenLocked(reversal.DebitAccount) || l.frozenLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountFrozen
	}
	if l.suspendedLocked(reversal.DebitAccount) || l.suspendedLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountSuspended
	}
	if err := l.overdraftRejectedLocked(reversal); err != nil {
		return JournalEntry{}, false, err
	}
	if l.dailyLimitRejectedLocked(reversal.CreditAccount, reversal.Currency, reversal.AmountCents, reversal.CreatedAt) {
		return JournalEntry{}, false, ErrDailyLimitExceeded
	}
	// Rolling velocity limits (see velocity.go): the reversal is a new
	// journal row, so it counts toward both legs' windows like any
	// other posting.
	if err := l.velocityRejectedLocked(reversal.DebitAccount, reversal.Currency, VelocityInflow, reversal.AmountCents, reversal.CreatedAt, "ReverseEntry"); err != nil {
		return JournalEntry{}, false, err
	}
	if err := l.velocityRejectedLocked(reversal.CreditAccount, reversal.Currency, VelocityOutflow, reversal.AmountCents, reversal.CreatedAt, "ReverseEntry"); err != nil {
		return JournalEntry{}, false, err
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
	// The reversal declares its formal entry link (see links.go, LG-123):
	// reversal_of is the tamper-evident twin of the "reversal_of:<id>"
	// memo — the memo stays searchable, the link stays verifiable. It
	// lands inside the same write lock as the reversal's commit, so the
	// entry and its link are atomic.
	if _, _, err := l.declareEntryLinkLocked(context.Background(), reversalID, entryID, LinkTypeReversalOf); err != nil {
		return JournalEntry{}, false, err
	}
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

// partialReversalIDFor returns the deterministic journal entry ID of the
// n-th partial reversal of entryID: "<entry ID>/reversal/<n>". The index
// makes every partial reversal idempotent on its own, independent of the
// full reversal's "<entry ID>/reversal" ID.
func partialReversalIDFor(entryID string, n int) string {
	return entryID + "/reversal/" + strconv.Itoa(n)
}

// partialReversalMemoFor returns the memo stamped on the n-th partial
// reversal of entryID. It keeps the "reversal_of:" prefix (so partials
// stay searchable through GET /entries?memo=reversal_of:) and encodes
// the partial index, which is what distinguishes it from a full
// reversal's memo.
func partialReversalMemoFor(entryID string, n int) string {
	return "reversal_of:" + entryID + "/partial/" + strconv.Itoa(n)
}

// isGenuinePartialReversalOf reports whether e is the genuine n-th
// partial reversal of entryID: the deterministic partial ID *and* the
// partial memo. Both must match — the ID alone could have been taken by
// a hand-posted entry (see ErrReversalIDConflict).
func isGenuinePartialReversalOf(e JournalEntry, entryID string, n int) bool {
	return e.ID == partialReversalIDFor(entryID, n) && e.Memo == partialReversalMemoFor(entryID, n)
}

// partialReversalExternalRefFor derives the external reference of the
// n-th partial reversal of an entry: the "reversal_of:" prefix plus the
// original entry's reference plus the partial index, so the reversal
// stays correlatable with the corrected posting and partials never
// collide with each other (or with a full reversal) in the reference
// index. An entry without a reference yields no inherited reference.
// Like reversalExternalRefFor, an overlong inherited reference rejects
// the reversal with ErrExternalRefTooLong instead of truncating.
func partialReversalExternalRefFor(origRef string, n int) (string, error) {
	if origRef == "" {
		return "", nil
	}
	inherited := reversalRefPrefix + origRef + "/partial/" + strconv.Itoa(n)
	if err := checkExternalRefLength(inherited); err != nil {
		return "", err
	}
	return inherited, nil
}

// hasGenuinePartialReversalLocked reports whether entryID has at least
// one genuine partial reversal. Partial indexes are dense from 1 (the
// journal is append-only, so no index is ever removed), so checking
// index 1 suffices. Callers must hold l.mu.
func (l *Ledger) hasGenuinePartialReversalLocked(entryID string) bool {
	e, taken := l.entries[partialReversalIDFor(entryID, 1)]
	return taken && isGenuinePartialReversalOf(e, entryID, 1)
}

// netReversedLocked returns the net amount of entryID currently
// reversed: the amounts of its genuine reversals (full and partial)
// minus whatever of those reversals was itself reversed — a
// reversal-of-reversal nets the correction back, so only the
// *outstanding* correction counts against the entry. A squatter in the
// partial namespace contributes nothing (it is not a genuine reversal;
// it blocks new partials via ErrReversalIDConflict elsewhere) and
// terminates the scan, because no genuine partial can exist beyond a
// squatter. The journal is append-only and reversal IDs strictly
// lengthen, so the recursion always terminates. Callers must hold
// l.mu.
func (l *Ledger) netReversedLocked(entryID string) int64 {
	var net int64
	if full, taken := l.entries[reversalIDFor(entryID)]; taken && isGenuineReversalOf(full, entryID) {
		net += full.AmountCents - l.netReversedLocked(full.ID)
	}
	for n := 1; ; n++ {
		id := partialReversalIDFor(entryID, n)
		e, taken := l.entries[id]
		if !taken {
			break
		}
		if !isGenuinePartialReversalOf(e, entryID, n) {
			break
		}
		net += e.AmountCents - l.netReversedLocked(e.ID)
	}
	return net
}

// ReverseEntryPartial appends a partial reversing journal entry for the
// journal entry with the given ID: the caller-chosen amountCents (at
// most the entry's remaining unreversed amount), same currency, debit
// and credit legs swapped, memo "reversal_of:<entry ID>/partial/<n>".
// The partial reversal ID is deterministic ("<entry ID>/reversal/<n>",
// n the 1-based index of this entry's n-th partial reversal), so the
// operation is idempotent per index.
//
// The cumulative corrected amount — the genuine partial reversals'
// amounts plus a genuine full reversal's whole amount, which also
// counts — can never exceed the original entry's amount: anything over
// the cap is rejected with ErrAlreadyReversed (a 409-class error, like
// a second full reversal). The original entry is never modified: the
// audit chain is append-only, so a partial reversal only appends a new
// link.
//
// The check order mirrors ReverseEntry: the amount must be positive
// (ErrNonPositiveAmount); the original entry must exist
// (ErrEntryNotFound); a pending-review entry cannot be reversed
// (ErrReversalOfPendingReview); then the idempotency replay check — a
// repeated idempotency key returns the originally created entry with
// duplicate == true. Keyless calls always book the next partial index:
// the deterministic ID is per-index, so a keyless call cannot identify
// which partial to replay — pass an idempotency key for retry safety.
// A hand-posted entry squatting in the deterministic partial
// namespace is rejected with ErrReversalIDConflict. After that come
// the risk gates in Post order: closed, frozen, suspended,
// overdraft on the swapped credit leg, the daily outflow limit on the
// same leg, and the period gate on the reversal's own timestamp. A
// rejection records nothing: no journal row, no chain link, no version
// bump.
//
// The commit is atomic and append-only, exactly like ReverseEntry:
// the partial reversal's journal row, idempotency index entry,
// per-account index rows, balance and total updates, audit-chain link,
// and version bump land together under the one write lock. Journal
// rules do not fire and the reversal never enters dual-control review.
// Like full reversals it fires the balance-change hook and the
// low-balance evaluation after commit, and emits the entry_posted
// domain event plus the entry_reversed envelope (with partial details).
func (l *Ledger) ReverseEntryPartial(entryID string, amountCents int64, idempotencyKey string) (reversal JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reverseEntryPartialLocked(entryID, amountCents, idempotencyKey)
}

// reverseEntryPartialLocked is the shared implementation of
// ReverseEntryPartial. Callers must hold l.mu (the write lock).
func (l *Ledger) reverseEntryPartialLocked(entryID string, amountCents int64, idempotencyKey string) (reversal JournalEntry, duplicate bool, err error) {
	// The amount is field validation, like the legs: a non-positive
	// partial reversal is rejected before the entry lookup or the
	// replay checks, so a malformed request fails fast regardless of
	// the ledger's reversal state.
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}

	orig, ok := l.entries[entryID]
	if !ok {
		return JournalEntry{}, false, ErrEntryNotFound
	}
	if orig.PendingReview {
		return JournalEntry{}, false, ErrReversalOfPendingReview
	}

	// Idempotency replay first, like Post and ReverseEntry: a repeated
	// key returns the entry the key created, whatever it was, without
	// booking anything.
	if idempotencyKey != "" {
		if prev, ok := l.byKey[idempotencyKey]; ok {
			return prev, true, nil
		}
	}

	// Walk the deterministic partial namespace to find the next free
	// index. The journal is append-only, so the genuine indexes are
	// dense from 1 and the scan terminates at the first untaken index.
	// A hand-posted entry squatting in the namespace is rejected with
	// ErrReversalIDConflict, like the full reversal's namespace
	// conflict: booking the partial would silently overwrite that row.
	n := 1
	for {
		id := partialReversalIDFor(entryID, n)
		e, taken := l.entries[id]
		if !taken {
			break
		}
		if !isGenuinePartialReversalOf(e, entryID, n) {
			return JournalEntry{}, false, ErrReversalIDConflict
		}
		n++
	}
	// The cumulative corrected amount — the net outstanding reversal
	// (genuine reversals minus their own reversal-of-reversal
	// corrections, so a corrected partial frees its slice again) plus
	// this partial — can never exceed the original entry's amount:
	// anything over the cap is rejected with ErrAlreadyReversed. The
	// cap check is written as a subtraction so a huge amountCents
	// cannot overflow the addition: the net never exceeds
	// orig.AmountCents (every admitted reversal held the same cap), so
	// orig.AmountCents - net is non-negative.
	net := l.netReversedLocked(entryID)
	if amountCents > orig.AmountCents-net {
		return JournalEntry{}, false, ErrAlreadyReversed
	}

	now := time.Now()
	inheritedRef, err := partialReversalExternalRefFor(orig.ExternalRef, n)
	if err != nil {
		return JournalEntry{}, false, err
	}
	reversalID := partialReversalIDFor(entryID, n)
	reversal = JournalEntry{
		ID:             reversalID,
		DebitAccount:   orig.CreditAccount,
		CreditAccount:  orig.DebitAccount,
		AmountCents:    amountCents,
		Currency:       orig.Currency,
		IdempotencyKey: idempotencyKey,
		Memo:           partialReversalMemoFor(entryID, n),
		ExternalRef:    inheritedRef,
		Attachments:    append([]string(nil), orig.Attachments...),
		CreatedAt:      now,
	}
	if err := validateJournalEntry(&reversal); err != nil {
		return JournalEntry{}, false, err
	}
	// External-reference uniqueness, like in ReverseEntry: the
	// inherited reference is deterministic, so a collision can only
	// come from a hand-posted row — but the journal stays append-only,
	// so it is still rejected rather than overwritten.
	if inheritedRef != "" {
		if owner, taken := l.byRef[inheritedRef]; taken {
			return JournalEntry{}, false, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, inheritedRef, owner)
		}
	}

	// The risk gates run in Post order on the swapped legs, exactly
	// like ReverseEntry: the partial reversal moves real money back, so
	// it earns the same protections as the original posting.
	l.releaseExpiredFreezesLocked(time.Now(), "write_path")
	if l.closedLocked(reversal.DebitAccount) || l.closedLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountClosed
	}
	if l.frozenLocked(reversal.DebitAccount) || l.frozenLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountFrozen
	}
	if l.suspendedLocked(reversal.DebitAccount) || l.suspendedLocked(reversal.CreditAccount) {
		return JournalEntry{}, false, ErrAccountSuspended
	}
	if err := l.overdraftRejectedLocked(reversal); err != nil {
		return JournalEntry{}, false, err
	}
	if l.dailyLimitRejectedLocked(reversal.CreditAccount, reversal.Currency, reversal.AmountCents, reversal.CreatedAt) {
		return JournalEntry{}, false, ErrDailyLimitExceeded
	}
	// Rolling velocity limits (see velocity.go), like the full
	// reversal above: a partial reversal is a new journal row and
	// counts toward both legs' windows.
	if err := l.velocityRejectedLocked(reversal.DebitAccount, reversal.Currency, VelocityInflow, reversal.AmountCents, reversal.CreatedAt, "ReverseEntryPartial"); err != nil {
		return JournalEntry{}, false, err
	}
	if err := l.velocityRejectedLocked(reversal.CreditAccount, reversal.Currency, VelocityOutflow, reversal.AmountCents, reversal.CreatedAt, "ReverseEntryPartial"); err != nil {
		return JournalEntry{}, false, err
	}
	// The period gate is keyed on the reversal's own timestamp, like
	// ReverseEntry: the books are corrected as of now.
	if err := l.periodRejectedLocked(reversal.CreatedAt); err != nil {
		return JournalEntry{}, false, err
	}

	// Atomic append-only commit, exactly like ReverseEntry.
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	hookTouched := touchedAccounts([]JournalEntry{reversal})
	hookOld := l.balanceSnapshotLocked(hookTouched)
	l.commitEntryLocked(reversal)
	// The partial reversal declares its formal entry link (see links.go,
	// LG-123), like a full reversal: reversal_of is the tamper-evident
	// twin of the "reversal_of:<id>/partial/<n>" memo. It lands inside
	// the same write lock as the partial's commit, so the entry and its
	// link are atomic.
	if _, _, err := l.declareEntryLinkLocked(context.Background(), reversalID, entryID, LinkTypeReversalOf); err != nil {
		return JournalEntry{}, false, err
	}
	l.addDailyOutflowLocked(reversal.CreditAccount, reversal.Currency, reversal.AmountCents, reversal.CreatedAt)
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(hookTouched, reversalID, "ReverseEntryPartial")
	l.emitAudit(AuditEvent{
		Op:            "reverse",
		Actor:         "ReverseEntryPartial",
		TraceID:       reversalID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{reversalID},
		Accounts:      []AccountID{reversal.DebitAccount, reversal.CreditAccount},
		Details: map[string]any{
			"reversed_entry_id": entryID,
			"amount_cents":      reversal.AmountCents,
			"currency":          reversal.Currency,
			"partial":           true,
			"reversal_index":    n,
		},
	})
	// Domain outbox: the partial reversal is itself a journal row
	// (entry_posted), plus the entry_reversed envelope naming the
	// corrected entry and the partial index.
	l.emitOutboxEntriesLocked([]JournalEntry{reversal})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxEntryReversed,
		Account:     reversal.DebitAccount,
		EntryID:     reversalID,
		AmountCents: reversal.AmountCents,
		Currency:    reversal.Currency,
		Details: map[string]any{
			"reversed_entry_id": entryID,
			"partial":           true,
			"reversal_index":    n,
		},
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, reversalID)

	return reversal, false, nil
}
