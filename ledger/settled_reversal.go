package ledger

import (
	"errors"
)

// Settled-transfer reversal (LG-231).
//
// An operator-driven correction for a transfer that already settled:
// ReverseSettledTransfer books a partial reversal of the transfer's
// principal journal entry for a caller-chosen slice of the settled
// amount, returning the settled funds to the payer. It is the
// transfer-level twin of ReverseEntryPartial (see reversal.go): the
// transfer ID names the principal, the amount names the slice, and the
// reason names the business cause — a wrong payee discovered after
// settlement, a duplicate payment confirmed by operations, a recalled
// payout.
//
// Only a posted, same-currency transfer can be reversed this way:
//   - The transfer must resolve through the pain.001 registry (see
//     pain001.go), which records same-currency transfer principals at
//     commit. An unknown ID, a raw journal post (never a transfer),
//     and a cross-currency transfer (which settles as two legs through
//     the FX clearing account, not one debtor-to-creditor principal)
//     all fail with ErrSettledTransferNotFound — the same registry
//     semantics disputes use (see RaiseDispute). An FX transfer must be
//     unwound leg by leg, like any other multi-leg posting.
//   - A pending-review principal fails with ErrReversalOfPendingReview:
//     the money has not moved to the payee yet, so there is nothing to
//     send back — the review workflow is the correct way to cancel it.
//     Rejected and expired reviews stay pending-marked, so they fail
//     here too; an approved review's principal is effective and
//     reversible.
//   - The fee leg, when the transfer carried one, is not reversed:
//     reversing a transfer reverses one journal entry at a time (see
//     reversal.go), so the "<transfer ID>/fee" leg needs its own
//     ReverseEntryPartial call, like any other leg.
//
// The remaining reversible amount is the principal minus everything
// already reversed against it (see netReversedLocked): prior
// independent reversals — full or partial, direct or through
// ReverseEntryPartial — and upheld dispute refunds, which are partial
// reversals of the same principal (see ResolveDispute). A request over
// the remainder fails with ErrAlreadyReversed, so the settled amount
// can never be corrected twice over.
//
// The risk gates are ReverseEntryPartial's, reused by delegation, not
// re-implemented: frozen, closed, suspended, overdraft on the swapped
// credit leg, the daily outflow limit, rolling velocity, and the period
// gate on the reversal's own timestamp. A rejection records nothing.
// The idempotency key rides the ledger-wide key namespace, so a
// repeated key replays the entry it created (pass a fresh key per
// reversal intent — reusing the transfer's own posting key replays the
// principal entry, per that namespace's contract).
//
// The business reason is recorded on the settled_transfer_reversed
// audit event, not on the journal entry: the entry keeps the
// deterministic partial-reversal memo so it stays searchable as a
// reversal, while the operator's cause rides the audit trail next to
// the transfer ID, the amount, and the remaining reversible amount
// after this correction. The event fires only for a newly booked
// reversal; an idempotent replay books nothing new and stays silent.
// The reversal itself is a journal entry, so it round-trips through
// snapshots like any other posting, and the remaining cap still holds
// on the restored ledger.

// ErrSettledTransferNotFound is returned when the transfer ID names no
// posted same-currency transfer: an unknown ID, a raw journal post
// that was never a transfer, or a cross-currency transfer (which has
// no single-currency principal in the registry). It is a 404-class
// error: the caller named a transfer the ledger never settled as one.
var ErrSettledTransferNotFound = errors.New("ledger: settled transfer not found")

// ReverseSettledTransfer appends a partial reversal of the settled
// transfer's principal journal entry for amountCents, returning the
// slice to the payer, and records reason as the business cause on the
// settled_transfer_reversed audit event.
//
// The check order mirrors RaiseDispute: the amount is field validation
// (ErrNonPositiveAmount); the transfer resolves through the
// same-currency registry (ErrSettledTransferNotFound); the principal
// entry must be effective, not pending review
// (ErrReversalOfPendingReview); then the call delegates to
// ReverseEntryPartial, which owns the idempotency replay, the
// remaining-amount cap (ErrAlreadyReversed), and the full risk-gate
// order. A rejection at any step records nothing: no journal row, no
// chain link, no version bump, no audit event.
//
// The commit is atomic and append-only, exactly like ReverseEntryPartial:
// the reversal's journal row, idempotency index entry, per-account index
// rows, balance and total updates, audit-chain link, and version bump
// land together under the one write lock, and the settled_transfer_reversed
// audit event brackets the same commit. Journal rules do not fire and
// the reversal never enters dual-control review, like every other
// reversal.
func (l *Ledger) ReverseSettledTransfer(transferID string, amountCents int64, reason string, idempotencyKey string) (reversal JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reverseSettledTransferLocked(transferID, amountCents, reason, idempotencyKey)
}

// reverseSettledTransferLocked is the shared implementation of
// ReverseSettledTransfer. Callers must hold l.mu (the write lock).
func (l *Ledger) reverseSettledTransferLocked(transferID string, amountCents int64, reason string, idempotencyKey string) (reversal JournalEntry, duplicate bool, err error) {
	// The amount is field validation, like in ReverseEntryPartial: a
	// non-positive slice is rejected before the transfer lookup, so a
	// malformed request fails fast regardless of the ledger's state.
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}

	// The transfer resolves through the same-currency registry: only
	// a posted same-currency transfer has a principal to reverse.
	// Unknown IDs, raw journal posts, and cross-currency transfers
	// are not in the registry and land here.
	if _, ok := l.painTransfers[transferID]; !ok {
		return JournalEntry{}, false, ErrSettledTransferNotFound
	}
	// The principal entry must still exist, and it must be effective:
	// a pending-review transfer has not moved money to the payee, so
	// there is nothing to send back. The registry implies the entry,
	// but the journal is the authority — a missing row is the same
	// "no such settled transfer" outcome, not a partial state.
	orig, ok := l.entries[transferID]
	if !ok {
		return JournalEntry{}, false, ErrSettledTransferNotFound
	}
	if orig.PendingReview {
		return JournalEntry{}, false, ErrReversalOfPendingReview
	}

	versionBefore := l.version
	reversal, duplicate, err = l.reverseEntryPartialLocked(transferID, amountCents, idempotencyKey)
	if err != nil {
		return JournalEntry{}, false, err
	}
	// An idempotent replay books nothing new, so it stays silent: no
	// second audit event for a reversal that already happened.
	if duplicate {
		return reversal, true, nil
	}
	// The remaining reversible amount after this correction — the
	// principal minus everything now reversed against it, dispute
	// refunds included — rides the audit event so operations can see
	// at a glance how much of the transfer is still correctable.
	remaining := orig.AmountCents - l.netReversedLocked(transferID)
	l.emitAudit(AuditEvent{
		Op:            "settled_transfer_reversed",
		Actor:         "ReverseSettledTransfer",
		TraceID:       transferID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{reversal.ID},
		Accounts:      []AccountID{reversal.DebitAccount, reversal.CreditAccount},
		Details: map[string]any{
			"transfer_id":                transferID,
			"amount_cents":               amountCents,
			"currency":                   reversal.Currency,
			"reason":                     reason,
			"reversal_entry_id":          reversal.ID,
			"remaining_reversible_cents": remaining,
		},
	})

	return reversal, false, nil
}
