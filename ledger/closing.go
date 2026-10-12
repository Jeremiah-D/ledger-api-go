package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Period-closing entries (LG-48).
//
// When an accounting period closes, operators can opt into booking
// closing entries: the period's profit-and-loss accounts are zeroed and
// their balances are carried into a retained-earnings account — the
// standard period-end close of double-entry bookkeeping. The ledger has
// no built-in account-type classification, so the P&L set and the
// retained-earnings account are opt-in structural config (see
// SetClosingAccounts), like frozen accounts or daily limits: no version
// bump, survives disaster-recovery snapshots, and changes are recorded in
// the structured audit chain.
//
// Closing entries are ordinary journal entries: they append audit-chain
// links, bump the version, and are covered by VerifyAccountingEquation
// and Reconcile exactly like any other posting. They differ from Post in
// three deliberate ways, all documented here so the special-casing is
// never a surprise:
//
//   - They are timestamped at the first instant of the NEXT month
//     (00:00:00 UTC of month+1), never in the period being closed —
//     the period gate would reject a posting dated inside the locked
//     month, and accounting convention books the close as the new
//     period's opening move. The next month must itself be open.
//   - They do not fire the opt-in journal rules engine (see rules.go):
//     derived legs are business postings (tax withholding, platform
//     fees); appending them to a mechanical close would misstate the
//     books. Closing entries commit through commitEntryLocked directly,
//     exactly like PostMerge's legs.
//   - They are never routed into dual-control review (see review.go):
//     they are system-generated closings, not operator transfers.
//
// Entry IDs are deterministic — "<period>/closing/<account>/<currency>"
// — so a close is naturally idempotent: re-closing an already-closed
// period is a no-op, and an ID that already exists aborts the close
// instead of double-booking.
//
// The close is atomic: the period lock lands in the same critical
// section as the last closing entry. If any gate fails (frozen retained
// account, overdraft on a credited leg, daily limit, the next month
// already closed, ...), the close fails with that gate's error and the
// period stays OPEN — never a silent half-close. Rejected closes record
// nothing: no journal rows, no chain links, no version bump, no period
// lock — exactly like rejected Posts.

// Validation errors returned by closing-account configuration and
// period closes with closing entries.
var (
	// ErrClosingNotConfigured is returned by ClosePeriodWithClosing
	// when no closing accounts were configured (see SetClosingAccounts).
	// Closing is opt-in: the operator must declare the P&L set and the
	// retained-earnings account before closing with closing=true, so a
	// close can never silently book entries against an unintended
	// mapping.
	ErrClosingNotConfigured = errors.New("ledger: closing accounts not configured")
	// ErrClosingRetainedInPLAccounts is returned by SetClosingAccounts
	// when the retained-earnings account also appears in the P&L set: a
	// close would try to zero the retained account into itself.
	ErrClosingRetainedInPLAccounts = errors.New("ledger: retained-earnings account must not be a P&L account")
	// ErrClosingEmptyAccount is returned by SetClosingAccounts when a
	// P&L account or the retained-earnings account is empty.
	ErrClosingEmptyAccount = errors.New("ledger: closing account must not be empty")
	// ErrClosingIDConflict is returned by ClosePeriodWithClosing when a
	// planned closing entry ID already exists in the journal. Closing
	// entry IDs are deterministic, so a conflict means something else —
	// a manual posting or a previous close — already booked that ID; the
	// close fails closed and the period stays open.
	ErrClosingIDConflict = errors.New("ledger: closing entry ID already exists")
)

// ClosingReceipt describes a period close performed with
// ClosePeriodWithClosing.
type ClosingReceipt struct {
	// Period is the closed "YYYY-MM" month.
	Period string
	// Closed is true when the period is locked after this call.
	Closed bool
	// AlreadyClosed is true when the period was already locked before
	// this call: the close is a no-op and Entries is empty.
	AlreadyClosed bool
	// Entries are the closing entries booked by this close, in commit
	// order (P&L account sorted, then currency sorted). Empty when the
	// close booked nothing (already closed, or no P&L account carried a
	// non-zero balance).
	Entries []JournalEntry
}

// SetClosingAccounts declares the opt-in period-closing mapping: the set
// of profit-and-loss accounts whose balances are zeroed at period close,
// and the retained-earnings account that receives them. Passing an empty
// retained account with no P&L accounts clears the configuration
// (closing disabled); a retained account with P&L accounts, or a P&L set
// without a retained account, is a misconfiguration and is rejected.
// Listing the retained-earnings account among the P&L accounts is
// rejected: the close would zero the retained account into itself.
//
// The mapping is structural config — it does not bump the ledger version
// — but unlike most structural config it is recorded in the structured
// audit chain (op "closing_accounts_set"), so a later auditor can see
// exactly which mapping produced a period's closing entries.
func (l *Ledger) SetClosingAccounts(pl []AccountID, retained AccountID) error {
	seen := make(map[AccountID]bool, len(pl))
	uniq := make([]AccountID, 0, len(pl))
	for _, a := range pl {
		if a == "" {
			return ErrClosingEmptyAccount
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}
	if retained == "" {
		if len(uniq) > 0 {
			return ErrClosingNotConfigured
		}
		// Clear: closing disabled.
		l.mu.Lock()
		l.closingPL = make(map[AccountID]bool)
		l.closingRetained = ""
		l.mu.Unlock()
		l.emitAudit(AuditEvent{
			Op:            "closing_accounts_set",
			Actor:         "SetClosingAccounts",
			VersionBefore: l.Version(),
			VersionAfter:  l.Version(),
			Details: map[string]any{
				"pl_accounts":      []string{},
				"retained_earnings": "",
			},
		})
		return nil
	}
	if seen[retained] {
		return ErrClosingRetainedInPLAccounts
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i] < uniq[j] })

	l.mu.Lock()
	l.closingPL = make(map[AccountID]bool, len(uniq))
	for _, a := range uniq {
		l.closingPL[a] = true
	}
	l.closingRetained = retained
	version := l.version
	l.mu.Unlock()

	plNames := make([]string, 0, len(uniq))
	for _, a := range uniq {
		plNames = append(plNames, string(a))
	}
	l.emitAudit(AuditEvent{
		Op:            "closing_accounts_set",
		Actor:         "SetClosingAccounts",
		VersionBefore: version,
		VersionAfter:  version,
		Accounts:      append(append([]AccountID{}, uniq...), retained),
		Details: map[string]any{
			"pl_accounts":      plNames,
			"retained_earnings": string(retained),
		},
	})
	return nil
}

// ClosingAccounts returns the configured period-closing mapping: the
// P&L accounts (sorted) and the retained-earnings account. An empty
// retained account means closing is not configured.
func (l *Ledger) ClosingAccounts() (pl []AccountID, retained AccountID) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.closingAccountsLocked()
}

// closingAccountsLocked returns the closing mapping. Callers must hold
// l.mu; the read lock suffices.
func (l *Ledger) closingAccountsLocked() ([]AccountID, AccountID) {
	pl := make([]AccountID, 0, len(l.closingPL))
	for a := range l.closingPL {
		pl = append(pl, a)
	}
	sort.Slice(pl, func(i, j int) bool { return pl[i] < pl[j] })
	return pl, l.closingRetained
}

// nextPeriodStart returns the first instant of the month after id
// ("YYYY-MM"): the timestamp every closing entry of that period's close
// carries. Callers must have validated id.
func nextPeriodStart(id string) time.Time {
	t, _ := time.Parse("2006-01", id)
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// closingEntryID is the deterministic ID of one closing entry: the
// period, the P&L account, and the currency. Deterministic IDs make the
// close idempotent and make a conflicting manual posting fail the close
// instead of double-booking.
func closingEntryID(period string, account AccountID, currency string) string {
	return fmt.Sprintf("%s/closing/%s/%s", period, account, currency)
}

// ClosePeriodWithClosing locks the accounting period id like ClosePeriod,
// and — when closing accounts are configured — first books the period's
// closing entries: every (P&L account, currency) with a non-zero balance
// is zeroed into the retained-earnings account, with the entries dated at
// the next month's first instant.
//
// A positive (debit-normal, e.g. expense) balance b posts
// {debit: retained, credit: account, amount: b}: the account's balance
// falls to zero and retained earnings absorbs it. A negative
// (credit-normal, e.g. revenue) balance posts {debit: account, credit:
// retained, amount: -b}. Zero balances book nothing.
//
// The checks run in Post's gate order — validation, then frozen, then
// overdraft, then daily-limit, then the period gate (keyed on the next
// month, which must be open) — and the commit is atomic across all
// closing entries plus the period lock. The first failing gate aborts the
// whole close: the period stays open and nothing is recorded. Closing an
// already-closed period is a no-op success.
//
// The close is recorded in the structured audit chain (op "period_close",
// with closing details); like ClosePeriod it does not bump the version
// for the lock itself — the version moves only through the closing
// entries' own commits.
func (l *Ledger) ClosePeriodWithClosing(id string) (ClosingReceipt, error) {
	if err := validatePeriodID(id); err != nil {
		return ClosingReceipt{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closedPeriods[id] {
		l.emitAudit(AuditEvent{
			Op:            "period_close",
			Actor:         "ClosePeriodWithClosing",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"closed":         true,
				"closing":        true,
				"already_closed": true,
			},
		})
		return ClosingReceipt{Period: id, Closed: true, AlreadyClosed: true}, nil
	}

	pl, retained := l.closingAccountsLocked()
	if retained == "" {
		return ClosingReceipt{}, ErrClosingNotConfigured
	}

	at := nextPeriodStart(id)
	entries := make([]JournalEntry, 0)
	for _, account := range pl {
		currencies := make([]string, 0)
		for k, b := range l.balances {
			if k.account == account && b != 0 {
				currencies = append(currencies, k.currency)
			}
		}
		sort.Strings(currencies)
		for _, currency := range currencies {
			balance := l.balances[accountCurrency{account: account, currency: currency}]
			if balance == 0 {
				continue
			}
			e := JournalEntry{
				ID:        closingEntryID(id, account, currency),
				Currency:  currency,
				CreatedAt: at,
				Memo:      "closing:" + id,
			}
			if balance > 0 {
				// Debit-normal balance (e.g. an expense account):
				// credit the P&L account to zero, debit retained.
				e.DebitAccount = retained
				e.CreditAccount = account
				e.AmountCents = balance
			} else {
				// Credit-normal balance (e.g. a revenue account):
				// debit the P&L account to zero, credit retained.
				e.DebitAccount = account
				e.CreditAccount = retained
				e.AmountCents = -balance
			}
			if err := validateJournalEntry(&e); err != nil {
				return ClosingReceipt{}, err
			}
			entries = append(entries, e)
		}
	}

	// Every entry clears Post's gate order before anything is committed:
	// an ID conflict, a frozen account, an overdraft, a daily limit, or
	// a closed next month aborts the whole close and leaves the period
	// open. The replay exemption does not apply here — closing entries
	// carry no idempotency keys and are never replays.
	for i := range entries {
		e := &entries[i]
		if _, exists := l.entries[e.ID]; exists {
			return ClosingReceipt{}, fmt.Errorf("%w: %s", ErrClosingIDConflict, e.ID)
		}
		if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
			return ClosingReceipt{}, ErrAccountFrozen
		}
		if l.overdraftRejectedLocked(*e) {
			return ClosingReceipt{}, ErrAccountOverdraft
		}
		if l.dailyLimitRejectedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt) {
			return ClosingReceipt{}, ErrDailyLimitExceeded
		}
		if err := l.periodRejectedLocked(e.CreatedAt); err != nil {
			return ClosingReceipt{}, err
		}
	}

	// Atomic commit: every closing entry's journal row, balances,
	// totals, chain links, and version bumps land together, and the
	// period lock lands in the same critical section. A reader never
	// sees closing entries without the locked period, and never sees a
	// locked period whose P&L accounts were not zeroed.
	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	hookTouched := touchedAccounts(entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for i := range entries {
		l.commitEntryLocked(entries[i])
		l.addDailyOutflowLocked(entries[i].CreditAccount, entries[i].Currency, entries[i].AmountCents, entries[i].CreatedAt)
	}
	l.closedPeriods[id] = true

	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(hookTouched, id, "ClosePeriodWithClosing")

	entryIDs := make([]string, 0, len(entries))
	accounts := []AccountID{retained}
	seenAccounts := map[AccountID]bool{retained: true}
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
		for _, a := range []AccountID{e.DebitAccount, e.CreditAccount} {
			if !seenAccounts[a] {
				seenAccounts[a] = true
				accounts = append(accounts, a)
			}
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	l.emitAudit(AuditEvent{
		Op:            "period_close",
		Actor:         "ClosePeriodWithClosing",
		TraceID:       id,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      accounts,
		Details: map[string]any{
			"closed":             true,
			"closing":            true,
			"closing_entries":    len(entries),
			"retained_earnings":  string(retained),
		},
	})
	// Domain outbox: one entry_posted per closing entry.
	l.emitOutboxEntriesLocked(entries)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, id)

	return ClosingReceipt{
		Period:  id,
		Closed:  true,
		Entries: entries,
	}, nil
}
