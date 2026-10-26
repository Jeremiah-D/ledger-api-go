package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Bad-debt write-off recovery (LG-219, fintech: receivables
// lifecycle).
//
// A written-off receivable sometimes pays after all: the customer
// who was deemed uncollectible sends cash anyway. That cash is
// real income, but it is not a payment of the receivable — the
// receivable was already expensed and removed by WriteOff (see
// writeoff.go, LG-212), and resurrecting it would rewrite history.
// Recovery therefore books its own entry, in the opposite
// direction from the write-off and touching neither the original
// sale nor the write-off row:
//
//	recover -> debit cash, credit bad-debt recovery income
//
// The written-off receivable's balance never moves: recovery
// income is a separate P&L account, not the receivable, so a
// recovered account stays at its post-write-off balance and the
// original write-off entry stays byte-identical in the journal.
// The receivable lifecycle this completes is sale (debit
// receivable / credit revenue) -> write-off (debit bad-debt
// expense / credit receivable) -> recovery (debit cash / credit
// recovery income): each stage is its own auditable row, and no
// stage ever edits an earlier row.
//
// Recovery posts through the system path (postSystemLocked), the
// same path WriteOff uses: the full gate order runs (validation,
// denylist, idempotency replay, frozen, overdraft, daily-limit,
// period), journal rules never fire, and the dormant-account stop
// is skipped — like the write-off itself, a recovery cleans up a
// receivable's aftermath, including on quiet accounts.
//
// The two booking accounts are ledger-wide configured
// (SetWriteOffRecoveryConfig): a recovery with no accounts
// configured is fail-closed, because booking cash or income into
// an unconfigured account would silently misstate the books.
// Both accounts must differ from each other and from the
// receivable account being recovered against: crediting recovery
// income into the receivable account itself (or debiting cash
// out of it) would move the receivable balance by the back door
// and defeat the no-restoration rule above.
//
// The cap is the cumulative written-off balance: a single recovery
// may never exceed total written off for that (account, currency)
// minus total already recovered for it, and a recovery against an
// account with nothing written off is refused for the same reason.
// The totals are read off the journal itself — written off is the
// sum of that account's write-off rows, recovered is the sum of
// its recovery rows — so the cap needs no separate state, survives
// disaster-recovery snapshots exactly like the rows it sums, and
// can never drift from the books. A recovery that would exceed
// the remaining written-off balance fails closed and books
// nothing: partial recovery is the normal case, and the remainder
// stays recoverable by later calls.
//
// Entry IDs are deterministic —
// "writeoff-recovery/<account>/<currency>/<writeoff-id>/<seq>" —
// and are also the idempotency keys, so a repeated posting of the
// same ID replays the original entry instead of booking cash
// twice. RecoverWriteOff derives both components from the
// journal: <writeoff-id> is the trailing segment of the latest
// write-off row for that (account, currency) — the reason hash
// WriteOff embedded in its own ID — and <seq> is one past the
// count of recovery rows already journaled for that pair, so
// successive recoveries get successive sequence numbers.
// RecoverWriteOffFor takes both components from the caller
// instead, for callers that anchor a recovery to one specific
// write-off or must reproduce an ID across ledgers: the
// <writeoff-id> there is an audit anchor only, not a per-write-off
// cap bucket — the cap is always the account-level cumulative
// balance above, so a recovery anchored to one write-off may still
// draw on the account's whole written-off pool. The flip side of
// the derived form is deliberate and mirrors WriteOff: the
// sequence number, not the amount, distinguishes successive
// recoveries, so two recoveries of the same amount are two rows
// (seq 1, seq 2), while repeating an explicit ID is a replay,
// never a second booking — including when the pool has since
// been exhausted, because the replay check runs before the cap
// check, like every other posting's idempotency replay.
//
// This is orthogonal to WriteOff (LG-212, see writeoff.go): that
// expenses an uncollectible receivable out of the books, this
// books cash back in after the fact. It is orthogonal to
// ReverseEscheatment (see escheat.go): that returns swept money
// to its account, this recognizes recovered cash as income
// without touching the account it came from.

// WriteOffRecoveryConfig names the two booking accounts for
// bad-debt recovery: the cash account debited when recovered
// cash arrives, and the recovery-income account credited with
// that cash.
type WriteOffRecoveryConfig struct {
	CashAccount           AccountID `json:"cash_account"`
	RecoveryIncomeAccount AccountID `json:"recovery_income_account"`
}

// Bad-debt recovery validation and state errors.
var (
	// ErrWriteOffRecoveryAccountsNotConfigured is returned by
	// recovery when no recovery accounts have been configured
	// with SetWriteOffRecoveryConfig.
	ErrWriteOffRecoveryAccountsNotConfigured = errors.New("ledger: bad-debt recovery accounts are not configured")
	// ErrInvalidWriteOffRecoveryAccount is returned by recovery
	// when the receivable account is empty or contains '/'.
	ErrInvalidWriteOffRecoveryAccount = errors.New("ledger: write-off recovery account must be non-empty and must not contain '/'")
	// ErrInvalidWriteOffRecoveryAmount is returned by recovery
	// when the amount is not positive.
	ErrInvalidWriteOffRecoveryAmount = errors.New("ledger: write-off recovery amount must be positive")
	// ErrWriteOffRecoverySameAccount is returned by recovery
	// when the configured booking accounts are not distinct, or
	// when either of them equals the receivable account: either
	// case would net the booking or move the receivable balance.
	ErrWriteOffRecoverySameAccount = errors.New("ledger: write-off recovery booking accounts must differ from each other and from the receivable account")
	// ErrInvalidWriteOffRecoveryID is returned by
	// RecoverWriteOffFor when the write-off ID is empty or
	// contains '/'.
	ErrInvalidWriteOffRecoveryID = errors.New("ledger: write-off recovery write-off ID must be non-empty and must not contain '/'")
	// ErrInvalidWriteOffRecoverySeq is returned by
	// RecoverWriteOffFor when the sequence number is less than 1.
	ErrInvalidWriteOffRecoverySeq = errors.New("ledger: write-off recovery sequence number must be at least 1")
	// ErrWriteOffRecoveryExceedsWrittenOff is returned by
	// recovery when the amount exceeds the account's remaining
	// written-off balance in that currency: recovery can never
	// book more cash than was ever written off.
	ErrWriteOffRecoveryExceedsWrittenOff = errors.New("ledger: write-off recovery amount exceeds the remaining written-off balance")
)

// checkWriteOffRecoveryConfig validates a recovery config
// before it is stored or used.
func checkWriteOffRecoveryConfig(c WriteOffRecoveryConfig) error {
	if c.CashAccount == "" || c.RecoveryIncomeAccount == "" {
		return ErrWriteOffRecoveryAccountsNotConfigured
	}
	if c.CashAccount == c.RecoveryIncomeAccount {
		return ErrWriteOffRecoverySameAccount
	}
	return nil
}

// WithWriteOffRecoveryConfig configures the ledger-wide recovery
// booking accounts (see RecoverWriteOff). Validation happens at
// booking time, like WithBadDebtExpenseAccount.
func WithWriteOffRecoveryConfig(c WriteOffRecoveryConfig) Option {
	return func(l *Ledger) {
		l.writeOffRecoveryConfig = c
		l.writeOffRecoveryConfigSet = true
	}
}

// SetWriteOffRecoveryConfig configures (or replaces) the
// ledger-wide recovery booking accounts. It validates eagerly:
// both accounts must be non-empty and distinct. Structural
// config — it does not bump the journal version. Like the
// deferred-revenue config, it is in-memory only and does not
// round-trip through disaster-recovery snapshots: the journaled
// recovery rows do, so the money is safe, and the operator
// re-configures the accounts on restore.
func (l *Ledger) SetWriteOffRecoveryConfig(c WriteOffRecoveryConfig) error {
	if err := checkWriteOffRecoveryConfig(c); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeOffRecoveryConfig = c
	l.writeOffRecoveryConfigSet = true
	l.emitAudit(AuditEvent{
		Op:       "writeoff_recovery_config_set",
		Actor:    "ledger",
		Accounts: []AccountID{c.CashAccount, c.RecoveryIncomeAccount},
		Details: map[string]any{
			"cash_account":            string(c.CashAccount),
			"recovery_income_account": string(c.RecoveryIncomeAccount),
		},
	})
	return nil
}

// WriteOffRecoveryConfig returns the configured recovery booking
// accounts, reporting whether any were configured.
func (l *Ledger) WriteOffRecoveryConfig() (WriteOffRecoveryConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.writeOffRecoveryConfig, l.writeOffRecoveryConfigSet
}

// writeOffRecoveryBooking returns the configured booking accounts
// or a fail-closed error; callers must hold the write lock.
func (l *Ledger) writeOffRecoveryBooking() (WriteOffRecoveryConfig, error) {
	if !l.writeOffRecoveryConfigSet {
		return WriteOffRecoveryConfig{}, ErrWriteOffRecoveryAccountsNotConfigured
	}
	cfg := l.writeOffRecoveryConfig
	if err := checkWriteOffRecoveryConfig(cfg); err != nil {
		return WriteOffRecoveryConfig{}, err
	}
	return cfg, nil
}

// writeOffRecoveryEntryID builds the deterministic recovery entry
// ID and idempotency key.
func writeOffRecoveryEntryID(account AccountID, currency, writeoffID string, seq int) string {
	return "writeoff-recovery/" + string(account) + "/" + currency + "/" +
		writeoffID + "/" + fmt.Sprintf("%d", seq)
}

// writeOffTotalsLocked reads the recovery cap inputs off the
// journal; callers must hold the write lock. written is the sum
// of the account's write-off rows in the currency, recovered the
// sum of its recovery rows, count the number of those recovery
// rows, and latestWriteOffID the trailing segment of the latest
// write-off row's ID in Post order ("" when nothing was ever
// written off). The write-off rows are found through the
// account's Post-order index, so "latest" is journal order, not
// map iteration order; the recovery rows carry the anchor
// account in their IDs, so they are summed from the journal map
// (their amounts are order-independent).
func (l *Ledger) writeOffTotalsLocked(account AccountID, currency string) (written, recovered int64, count int, latestWriteOffID string) {
	writeoffPrefix := "writeoff/" + string(account) + "/" + currency + "/"
	for _, id := range l.byAccount[account] {
		if !strings.HasPrefix(id, writeoffPrefix) {
			continue
		}
		if e, ok := l.entries[id]; ok {
			written += e.AmountCents
			if i := strings.LastIndex(id, "/"); i >= 0 {
				latestWriteOffID = id[i+1:]
			}
		}
	}
	recoveryPrefix := "writeoff-recovery/" + string(account) + "/" + currency + "/"
	for id, e := range l.entries {
		if strings.HasPrefix(id, recoveryPrefix) {
			recovered += e.AmountCents
			count++
		}
	}
	return written, recovered, count, latestWriteOffID
}

// WriteOffRecoveryTotals reports, for one receivable account and
// currency, how much was ever written off, how much of it has
// been recovered, and how much remains recoverable — all read
// off the journal, so the figures always agree with the books.
func (l *Ledger) WriteOffRecoveryTotals(account AccountID, currency string) (writtenOff, recovered, remaining int64, err error) {
	if err := checkWriteOffAccount(account); err != nil {
		return 0, 0, 0, ErrInvalidWriteOffRecoveryAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return 0, 0, 0, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	writtenOff, recovered, _, _ = l.writeOffTotalsLocked(account, cur)
	return writtenOff, recovered, writtenOff - recovered, nil
}

// recoverWriteOffLocked is the shared recovery core; callers
// must hold the write lock and must already have validated the
// account, currency, and amount. The replay check runs before
// the cap check, like WriteOff: a repeated ID returns the
// original entry even after the pool is exhausted.
func (l *Ledger) recoverWriteOffLocked(account AccountID, cur, writeoffID string, seq int, amountCents int64) (JournalEntry, bool, error) {
	cfg, err := l.writeOffRecoveryBooking()
	if err != nil {
		return JournalEntry{}, false, err
	}
	if cfg.CashAccount == account || cfg.RecoveryIncomeAccount == account {
		return JournalEntry{}, false, ErrWriteOffRecoverySameAccount
	}
	id := writeOffRecoveryEntryID(account, cur, writeoffID, seq)
	if orig, ok := l.byKey[id]; ok {
		return orig, true, nil
	}
	written, recovered, _, _ := l.writeOffTotalsLocked(account, cur)
	if amountCents > written-recovered {
		return JournalEntry{}, false, ErrWriteOffRecoveryExceedsWrittenOff
	}
	e := JournalEntry{
		ID:             id,
		DebitAccount:   cfg.CashAccount,
		CreditAccount:  cfg.RecoveryIncomeAccount,
		AmountCents:    amountCents,
		Currency:       cur,
		IdempotencyKey: id,
		Memo:           fmt.Sprintf("bad-debt recovery %s seq %d", writeoffID, seq),
		CreatedAt:      time.Now(),
	}
	posted, duplicate, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if duplicate {
		// Unreachable through the byKey pre-check above, but kept
		// for the same reason the write-off path keeps its
		// duplicate branch: the post is the authority on replay,
		// not the pre-check.
		return posted, true, nil
	}
	l.emitAudit(AuditEvent{
		Op:            "writeoff_recovery",
		Actor:         "RecoverWriteOff",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{account, cfg.CashAccount, cfg.RecoveryIncomeAccount},
		Details: map[string]any{
			"currency":          cur,
			"amount_cents":      amountCents,
			"entry_id":          posted.ID,
			"writeoff_id":       writeoffID,
			"seq":               seq,
			"written_off_cents": written,
			"recovered_cents":   recovered + amountCents,
			"remaining_cents":   written - recovered - amountCents,
		},
	})
	return posted, false, nil
}

// RecoverWriteOff books cash recovered after a write-off: one
// atomic journal entry — debit the configured cash account,
// credit the configured bad-debt recovery income account — for
// amountCents in currency. The original write-off rows are never
// touched and the written-off receivable's balance never moves;
// only cash and recovery income move.
//
// The call is fail-closed unless every precondition holds: the
// account is valid, the currency is a 3-letter uppercase code
// (empty means the default currency), the amount is positive and
// no larger than the account's remaining written-off balance in
// that currency, and recovery accounts are configured, distinct,
// and different from the account. The entry ID is derived from
// the journal (latest write-off's reason hash, next sequence
// number), so successive calls book successive recoveries. A
// replay of an already-journaled ID — through
// RecoverWriteOffFor with the same write-off ID and sequence
// number — returns the original entry with duplicate true and
// books nothing new.
//
// A successful recovery is audited as "writeoff_recovery" with
// the recovered total and the remaining written-off balance in
// the details.
func (l *Ledger) RecoverWriteOff(account AccountID, amountCents int64, currency string) (JournalEntry, bool, error) {
	if err := checkWriteOffAccount(account); err != nil {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoveryAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoveryAmount
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// The shared core re-checks the cap after the config and
	// replay checks, so a call against an empty pool still fails
	// with the cap error rather than building an ID off an empty
	// write-off anchor; latestWriteOffID is non-empty whenever
	// the core reaches the posting step, because a positive
	// remaining balance implies at least one write-off row.
	_, _, count, latestWriteOffID := l.writeOffTotalsLocked(account, cur)
	return l.recoverWriteOffLocked(account, cur, latestWriteOffID, count+1, amountCents)
}

// RecoverWriteOffFor is RecoverWriteOff with a caller-chosen
// write-off ID and sequence number: the entry ID is exactly
// "writeoff-recovery/<account>/<currency>/<writeoffID>/<seq>".
// Use it to anchor a recovery to one specific write-off, or to
// reproduce an ID across ledgers. Repeating the same write-off
// ID and sequence number replays the original entry with
// duplicate true and books nothing new, even after the pool is
// exhausted — the replay check runs before the cap check. The
// cap itself is the same account-level cumulative balance as
// RecoverWriteOff: the write-off ID is an audit anchor, not a
// per-write-off bucket.
func (l *Ledger) RecoverWriteOffFor(account AccountID, amountCents int64, currency, writeoffID string, seq int) (JournalEntry, bool, error) {
	if err := checkWriteOffAccount(account); err != nil {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoveryAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoveryAmount
	}
	if writeoffID == "" || strings.Contains(writeoffID, "/") {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoveryID
	}
	if seq < 1 {
		return JournalEntry{}, false, ErrInvalidWriteOffRecoverySeq
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.recoverWriteOffLocked(account, cur, writeoffID, seq, amountCents)
}
