package ledger

import (
	"errors"
	"time"
)

// Validation errors returned by PostTransfer.
var (
	ErrEmptyTransferID = errors.New("ledger: transfer ID must not be empty")
	ErrEmptyFromAccount = errors.New("ledger: transfer from-account must not be empty")
	ErrEmptyToAccount   = errors.New("ledger: transfer to-account must not be empty")
	ErrTransferSameAccount = errors.New("ledger: transfer from- and to-accounts must differ")
	ErrTransferNonPositiveAmount = errors.New("ledger: transfer amount must be greater than zero")
	// ErrTransferIDConflict is returned when the transfer ID is already
	// used as a journal entry ID (transfers book their principal entry
	// under the transfer ID, so the two namespaces must not collide).
	ErrTransferIDConflict = errors.New("ledger: transfer ID already used as a journal entry ID")
)

// Transfer describes one atomic money movement from one account to another.
//
// It is the payment-domain view of a double-entry posting: callers name the
// payer (From) and the payee (To) instead of debit/credit legs. PostTransfer
// books a single journal entry — debit To (its balance grows), credit From
// (its balance shrinks) — following the ledger's sign convention. The
// journal entry carries the transfer's ID, so transfers are directly
// visible in journal exports and the audit chain.
//
// The transfer commits atomically: field validation, the idempotency replay
// check, and the frozen/overdraft risk checks all run before anything is
// recorded, so a transfer that fails on either leg records nothing — no
// journal row, no chain link, no version bump. Readers never observe a
// half-posted transfer.
type Transfer struct {
	ID             string    `json:"transfer_id"`
	From           AccountID `json:"from_account"`
	To             AccountID `json:"to_account"`
	AmountCents    int64     `json:"amount_cents"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// TransferReceipt reports what PostTransfer committed.
//
// Entries holds the journal entries the transfer posted, in commit order: a
// plain transfer posts exactly one entry. Duplicate replays return the
// originally posted entries with Duplicate == true and book nothing new.
type TransferReceipt struct {
	TransferID string         `json:"transfer_id"`
	Entries    []JournalEntry `json:"entries"`
	Duplicate  bool           `json:"duplicate"`
}

// PostTransfer records an atomic transfer from one account to another.
//
// The risk checks mirror Post and run in the same order: the idempotency
// replay check first (replaying a key that was posted before an account was
// frozen or protected returns the original entry instead of failing,
// because the replay books nothing new), then the frozen check on both
// legs, then the overdraft check on the payer (From). Rejected transfers —
// validation failures, ID conflicts, frozen rejections, and overdraft
// rejections alike — record nothing: no journal row, no chain link, no
// version bump.
//
// Idempotency shares the ledger-wide key namespace with Post: a key already
// used by either API replays the original entry (Duplicate == true) instead
// of booking again. A zero CreatedAt is filled with the current time.
func (l *Ledger) PostTransfer(t Transfer) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if t.ID == "" {
		return TransferReceipt{}, ErrEmptyTransferID
	}
	if t.From == "" {
		return TransferReceipt{}, ErrEmptyFromAccount
	}
	if t.To == "" {
		return TransferReceipt{}, ErrEmptyToAccount
	}
	if t.From == t.To {
		return TransferReceipt{}, ErrTransferSameAccount
	}
	if t.AmountCents <= 0 {
		return TransferReceipt{}, ErrTransferNonPositiveAmount
	}

	if t.IdempotencyKey != "" {
		if orig, ok := l.byKey[t.IdempotencyKey]; ok {
			return TransferReceipt{
				TransferID: t.ID,
				Entries:    []JournalEntry{orig},
				Duplicate:  true,
			}, nil
		}
	}

	if _, exists := l.entries[t.ID]; exists {
		return TransferReceipt{}, ErrTransferIDConflict
	}

	if l.frozenLocked(t.From) || l.frozenLocked(t.To) {
		return TransferReceipt{}, ErrAccountFrozen
	}

	e := JournalEntry{
		ID:             t.ID,
		DebitAccount:   t.To,
		CreditAccount:  t.From,
		AmountCents:    t.AmountCents,
		IdempotencyKey: t.IdempotencyKey,
		CreatedAt:      t.CreatedAt,
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	// Overdraft protection is a risk control, not bookkeeping validation:
	// it runs after the frozen check and after the idempotency replay
	// check, for the same reasons as in Post.
	if l.overdraftRejectedLocked(e) {
		return TransferReceipt{}, ErrAccountOverdraft
	}

	l.maybePruneIdempotencyKeys(time.Now())
	l.commitEntryLocked(e)

	return TransferReceipt{
		TransferID: t.ID,
		Entries:    []JournalEntry{e},
		Duplicate:  false,
	}, nil
}

// commitEntryLocked applies every effect of one validated journal entry —
// the journal row, the idempotency index, the per-account index, both net
// balances, both debit/credit totals, the audit-chain link, and the version
// bump. Callers must hold the write lock and must have run all validation
// and risk checks first; after this call returns the entry is fully
// committed — there is no partial state to roll back.
func (l *Ledger) commitEntryLocked(e JournalEntry) {
	l.entries[e.ID] = e
	if e.IdempotencyKey != "" {
		l.byKey[e.IdempotencyKey] = e
	}
	l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
	l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
	l.balances[e.DebitAccount] += e.AmountCents
	l.balances[e.CreditAccount] -= e.AmountCents
	l.debitTotals[e.DebitAccount] += e.AmountCents
	l.creditTotals[e.CreditAccount] += e.AmountCents
	l.version++
	l.appendChainLink(e)
}
