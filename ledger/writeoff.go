package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Bad-debt write-off (LG-212, fintech: receivables).
//
// A receivable that will never be collected must leave the books, not
// linger as an asset the balance sheet pretends is real. Write-off is
// the accounting end of that lifecycle: the uncollectible amount is
// expensed (debit the bad-debt expense account) and removed from the
// receivable (credit the receivable account), in one atomic journal
// entry. This is not a payment — no cash moves — and it is not a
// reversal: the original sale entry stays journaled, and the write-off
// entry is the auditable record that the receivable was abandoned.
//
// WriteOff posts through the system path (postSystemLocked): the full
// gate order runs (validation, denylist, idempotency replay, frozen,
// overdraft, daily-limit, period), journal rules never fire, and the
// dormant-account stop is skipped — the write-off exists to clean up
// receivables, including ones on quiet accounts. Like the ECL provision
// (see ecl.go), the expense account is ledger-wide configured
// (SetBadDebtExpenseAccount): a write-off with no expense account
// configured is fail-closed, because expensing into an unconfigured
// account would silently misstate the P&L.
//
// The entry is atomic and single: one journal row, debit expense,
// credit receivable, for exactly the written-off amount. The
// accounting identity holds by construction — one debit equals one
// credit in the same currency — and the row is a full journal row
// (audit-chain link, version bump), so VerifyChain and Reconcile
// cover write-offs exactly like ordinary postings.
//
// The idempotency key is deterministic —
// "writeoff/<account>/<currency>/<amount>/<reason-hash>" — so a
// retried write-off replays the original entry instead of expensing
// twice. The reason hash is the first 16 hex characters of the
// SHA-256 of the reason: the key stays short while distinct reasons
// stay distinct. The flip side is deliberate: identical inputs
// (same account, currency, amount, and reason) are the same
// write-off, so a genuinely second write-off of the same amount
// must carry a distinguishing reason — in practice the reason names
// the invoice or period being written off, so it already does.
//
// A write-off can never exceed the receivable's current balance in
// that currency: expensing more than is owed would drive the
// receivable negative and invent a liability the books never had.
// A partial write-off is the normal case — write off what is
// uncollectible, leave the collectible remainder — and a second
// write-off for the remainder is a new entry with its own reason.
// A write-off of a zero-balance or negative-balance receivable is
// refused for the same reason: there is nothing to write off.
//
// The reason is mandatory and is journaled as the entry's memo: a
// write-off without a stated reason is unauditable — an auditor
// reading the journal must see why the receivable was abandoned,
// not just that it was. The reason is capped at the memo limit
// (255 UTF-8 characters) by the journal validation itself.
//
// This is orthogonal to escheatment (LG-109, see escheat.go):
// escheatment sweeps dormant accounts' positive balances to the
// state's unclaimed-property account and freezes the account;
// write-off expenses an uncollectible receivable and leaves the
// account open. It is orthogonal to dormancy fees (LG-88, see
// dormancy.go): dormancy fees charge for inactivity, write-off
// removes principal that will never arrive.

var (
	// ErrBadDebtExpenseNotConfigured is returned by WriteOff when
	// no bad-debt expense account has been configured with
	// SetBadDebtExpenseAccount.
	ErrBadDebtExpenseNotConfigured = errors.New("ledger: bad-debt expense account is not configured")
	// ErrInvalidWriteOffAccount is returned by WriteOff when the
	// receivable account is empty or contains '/'.
	ErrInvalidWriteOffAccount = errors.New("ledger: write-off receivable account must be non-empty and must not contain '/'")
	// ErrInvalidWriteOffAmount is returned by WriteOff when the
	// amount is not positive.
	ErrInvalidWriteOffAmount = errors.New("ledger: write-off amount must be positive")
	// ErrInvalidWriteOffReason is returned by WriteOff when the
	// reason is empty or longer than the memo limit.
	ErrInvalidWriteOffReason = errors.New("ledger: write-off reason must be non-empty and at most 255 UTF-8 characters")
	// ErrWriteOffSameAccount is returned by WriteOff when the
	// receivable account is the configured bad-debt expense
	// account: expensing into the account being written off would
	// be a no-op that still bumps the journal version.
	ErrWriteOffSameAccount = errors.New("ledger: write-off receivable account must differ from the bad-debt expense account")
	// ErrWriteOffExceedsReceivable is returned by WriteOff when
	// the amount exceeds the receivable's current balance in that
	// currency: a write-off can never drive a receivable negative.
	ErrWriteOffExceedsReceivable = errors.New("ledger: write-off amount exceeds the receivable balance")
)

// writeOffEntryID builds the deterministic write-off entry ID and
// idempotency key. The reason hash keeps the key short while keeping
// distinct reasons distinct; identical inputs replay, by design.
func writeOffEntryID(account AccountID, currency string, amountCents int64, reason string) string {
	sum := sha256.Sum256([]byte(reason))
	return "writeoff/" + string(account) + "/" + currency + "/" +
		fmt.Sprintf("%d", amountCents) + "/" + hex.EncodeToString(sum[:])[:16]
}

// checkWriteOffAccount validates a write-off receivable account: the
// escheat account rule — non-empty, no '/' (the separator in the
// deterministic entry ID scheme).
func checkWriteOffAccount(account AccountID) error {
	if account == "" || strings.Contains(string(account), "/") {
		return ErrInvalidWriteOffAccount
	}
	return nil
}

// WithBadDebtExpenseAccount configures the ledger-wide bad-debt
// expense account that write-offs debit (see WriteOff).
func WithBadDebtExpenseAccount(a AccountID) Option {
	return func(l *Ledger) {
		l.badDebtExpenseAccount = a
	}
}

// SetBadDebtExpenseAccount configures (or clears, with the empty
// account) the ledger-wide bad-debt expense account that write-offs
// debit. Empty means unconfigured: WriteOff is fail-closed until
// the operator configures it. Structural config, like the ECL
// expense account — it does not bump the journal version and it
// survives disaster-recovery snapshots.
func (l *Ledger) SetBadDebtExpenseAccount(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.badDebtExpenseAccount = a
	l.emitAudit(AuditEvent{
		Op:      "bad_debt_expense_set",
		Actor:   "SetBadDebtExpenseAccount",
		TraceID: string(a),
		Details: map[string]any{"bad_debt_expense_account": string(a)},
	})
}

// BadDebtExpenseAccount reports the configured bad-debt expense
// account, or "" when none is configured.
func (l *Ledger) BadDebtExpenseAccount() AccountID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.badDebtExpenseAccount
}

// WriteOff expenses an uncollectible receivable: it books one atomic
// journal entry — debit the configured bad-debt expense account,
// credit the receivable account — for amountCents in currency, with
// reason journaled as the entry's memo.
//
// The call is fail-closed unless every precondition holds: the
// receivable account is valid, the currency is a 3-letter uppercase
// code (empty means the default currency), the amount is positive
// and no larger than the receivable's current balance in that
// currency, the reason is non-empty, and a bad-debt expense account
// is configured and differs from the receivable account. A replay
// of identical inputs returns the original entry with duplicate
// true and books nothing new — including when the receivable's
// balance has since fallen below the amount, because the replay
// check runs before the balance check, like every other posting's
// idempotency replay.
//
// A successful write-off is audited as "writeoff" with the
// post-write-off receivable balance in the details, so the
// operator can see the remainder without re-reading the books.
func (l *Ledger) WriteOff(receivableAccount AccountID, amountCents int64, currency, reason string) (JournalEntry, bool, error) {
	if err := checkWriteOffAccount(receivableAccount); err != nil {
		return JournalEntry{}, false, err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrInvalidWriteOffAmount
	}
	if reason == "" || utf8.RuneCountInString(reason) > 255 {
		return JournalEntry{}, false, ErrInvalidWriteOffReason
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.badDebtExpenseAccount == "" {
		return JournalEntry{}, false, ErrBadDebtExpenseNotConfigured
	}
	if receivableAccount == l.badDebtExpenseAccount {
		return JournalEntry{}, false, ErrWriteOffSameAccount
	}
	// Check the idempotency key before the balance check: the
	// replay inside postSystemLocked would return the original
	// entry, but this balance check runs before the post, so
	// without the pre-check a replay would fail on a balance that
	// moved after the original posting instead of returning the
	// original entry.
	id := writeOffEntryID(receivableAccount, cur, amountCents, reason)
	if orig, ok := l.byKey[id]; ok {
		return orig, true, nil
	}
	if bal := l.balances[accountCurrency{account: receivableAccount, currency: cur}]; amountCents > bal {
		return JournalEntry{}, false, ErrWriteOffExceedsReceivable
	}
	e := JournalEntry{
		ID:             id,
		DebitAccount:   l.badDebtExpenseAccount,
		CreditAccount:  receivableAccount,
		AmountCents:    amountCents,
		Currency:       cur,
		IdempotencyKey: id,
		Memo:           reason,
		CreatedAt:      time.Now(),
	}
	posted, duplicate, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if duplicate {
		// Unreachable through the byKey pre-check above, but kept
		// for the same reason the escheat path keeps its duplicate
		// branch: the post is the authority on replay, not the
		// pre-check.
		return posted, true, nil
	}
	l.emitAudit(AuditEvent{
		Op:            "writeoff",
		Actor:         "WriteOff",
		TraceID:       string(receivableAccount),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{receivableAccount, l.badDebtExpenseAccount},
		Details: map[string]any{
			"currency":                 cur,
			"amount_cents":             amountCents,
			"entry_id":                 posted.ID,
			"reason":                   reason,
			"receivable_balance_after": l.balances[accountCurrency{account: receivableAccount, currency: cur}],
		},
	})
	return posted, false, nil
}
