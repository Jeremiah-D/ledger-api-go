package ledger

import (
	"errors"
	"math"
	"strings"
	"time"
)

// Overdraft interest accrual (opt-in): charge interest on negative
// balances, one journal entry per (account, currency, calendar day).
//
// A negative balance is money the ledger has extended: the account owes
// the house. AccrueOverdraftInterest prices that extension — debit the
// interest expense account, credit the interest payable account — for
// floor(principal * rateBps / 10000 / 365) cents, where principal is the
// day's overdrawn amount. The charged account itself is not a journal
// leg: like every accrual (see accrual.go) the entry moves money between
// the configured expense/payable pair, and the account's negative
// balance is only read to size the charge.
//
// The entry ID is deterministic —
// "accrual/overdraft-interest/<account>/<currency>/<YYYY-MM-DD>" (UTC
// date) — so the idempotency key is (account, currency, day): re-running
// the same day returns the original entry with duplicate == true and
// books nothing new. The entry is an ordinary journal row: it carries
// its own audit-chain link, bumps the ledger version, and flows through
// VerifyAccountingEquation and Reconcile. It runs the full Post gate
// order (frozen, overdraft, daily-limit, closed period) but never fires
// journal rules and never enters dual-control review (accrual
// convention, see accrual.go). The dormant-account stop is skipped —
// system adjusting entries keep posting through dormant accounts.
//
// Overdraft protection (see overdraft.go) does not block the charge:
// protection guards an account's own postings from taking it lower, but
// the interest entry never touches the charged account, so a protected
// account with a negative balance is still charged. An account whose
// balance is non-negative in the currency has nothing to charge: the
// call is rejected with ErrAccountNotOverdrawn.
//
// Compounding is opt-in per call: with compound == true the day's
// principal is the overdrawn balance plus the previously accrued (and
// not yet reversed) overdraft interest for the (account, currency) pair
// — end-of-day compounding without a capitalization entry. With
// compound == false only the ledger balance is charged. A reversed
// accrual nets out of the outstanding total, so compounding never
// charges interest on interest that was reversed away.
//
// The entry rides the memo "accrual:overdraft-interest", so charged rows
// are discoverable through the ordinary journal listing and the ?memo=
// filter. A charged entry is reversible through ReverseAccrual (the
// "accrual/" ID prefix admits it); the reversal books the mirror entry
// under "<entry ID>/reversal".

// ErrAccountNotOverdrawn is returned by AccrueOverdraftInterest when
// the account's balance is not negative in the requested currency:
// there is no overdraft to charge interest on.
var ErrAccountNotOverdrawn = errors.New("ledger: account balance is not negative in the currency")

// overdraftInterestEntryID builds the deterministic overdraft-interest
// entry ID. The UTC calendar date makes one day's charge idempotent
// while a new day gets a fresh ID. The account and currency are part of
// the ID (not the expense/payable pair): the charge is per overdrawn
// account, and the (account, currency, day) triple is the idempotency
// key. Account IDs cannot contain '/' (rejected at validation), and the
// currency is the normalized 3-letter code, so the ID segments are
// unambiguous.
func overdraftInterestEntryID(account AccountID, currency string, asof time.Time) string {
	return "accrual/overdraft-interest/" + string(account) + "/" + currency + "/" + asof.UTC().Format("2006-01-02")
}

// overdraftInterestEntryPrefix is the ID prefix shared by every
// overdraft-interest charge of one (account, currency) pair, used by
// overdraftInterestAccruedLocked to find them.
func overdraftInterestEntryPrefix(account AccountID, currency string) string {
	return "accrual/overdraft-interest/" + string(account) + "/" + currency + "/"
}

// AccrueOverdraftInterest books one day's interest on the account's
// negative balance in the given currency: debit the interest expense
// account and credit the interest payable account for
// floor(principal * rateBps / 10000 / 365) cents. rateBps is the annual
// rate in basis points (500 = 5%/yr). asof sets both the entry's
// timestamp and its calendar day (UTC); a zero asof means now.
//
// principal is the absolute overdrawn balance, or — with compound ==
// true — the overdrawn balance plus the previously accrued (and not yet
// reversed) overdraft interest for the (account, currency) pair.
//
// The call is idempotent per (account, currency, calendar day):
// re-running the same day returns the original entry with duplicate ==
// true. It runs the full Post gate order, never fires journal rules,
// never enters dual-control review, and skips the dormant-account stop
// (accrual convention). Overdraft protection on the charged account
// does not block the charge (see the package doc comment).
func (l *Ledger) AccrueOverdraftInterest(account, expense, payable AccountID, currency string, rateBps int64, compound bool, asof time.Time) (JournalEntry, bool, error) {
	if rateBps < 1 || rateBps > 10000 {
		return JournalEntry{}, false, ErrInvalidAccrualRate
	}
	if account == "" {
		return JournalEntry{}, false, errors.New("ledger: overdraft interest account must not be empty")
	}
	if strings.Contains(string(account), "/") {
		return JournalEntry{}, false, ErrAccrualAccountSeparator
	}
	if err := checkAccrualAccounts(expense, payable); err != nil {
		return JournalEntry{}, false, err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if asof.IsZero() {
		asof = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// LG-95: with business-day-only accruals, a non-business-day asof
	// books nothing — interest accrues per business day, like the
	// plain accruals.
	if err := l.accrualBusinessDayGateLocked(asof); err != nil {
		return JournalEntry{}, false, err
	}
	balance := l.balances[accountCurrency{account: account, currency: cur}]
	if balance >= 0 {
		return JournalEntry{}, false, ErrAccountNotOverdrawn
	}
	if balance == math.MinInt64 {
		return JournalEntry{}, false, errors.New("ledger: overdrawn balance overflows int64")
	}
	principal := -balance
	if compound {
		// End-of-day compounding: previously accrued interest joins
		// the principal. Only days strictly before asof's date count
		// — today's charge is the one being computed.
		dayStart := asof.UTC().Truncate(24 * time.Hour)
		outstanding, err := l.overdraftInterestAccruedLocked(account, cur, time.Time{}, dayStart)
		if err != nil {
			return JournalEntry{}, false, err
		}
		var ok bool
		principal, ok = addCents(principal, outstanding)
		if !ok {
			return JournalEntry{}, false, errors.New("ledger: compounded overdraft principal overflows int64")
		}
	}
	// Overflow-safe floor(principal * rateBps / 10000 / 365): the same
	// basis-point helper the transfer fee policy uses, divided by the
	// days in a year.
	amount := policyFeeCents(principal, rateBps) / 365
	if amount <= 0 {
		// A dust charge books nothing: a tiny overdraft under a low
		// rate rounds to zero cents for the day, mirroring the fee
		// policy's dust tiers (see transfer.go).
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	id := overdraftInterestEntryID(account, cur, asof)
	e := JournalEntry{
		ID:             id,
		DebitAccount:   expense,
		CreditAccount:  payable,
		AmountCents:    amount,
		Currency:       cur,
		IdempotencyKey: id,
		Memo:           "accrual:overdraft-interest",
		CreatedAt:      asof,
	}
	return l.postSystemLocked(e, false)
}

// overdraftInterestAccruedLocked returns the net overdraft interest
// accrued for one (account, currency) pair over [since, until): the sum
// of every overdraft-interest entry in the window, minus the reversals
// of those entries. Callers must hold l.mu; the read lock suffices
// because nothing is mutated.
func (l *Ledger) overdraftInterestAccruedLocked(account AccountID, currency string, since, until time.Time) (int64, error) {
	prefix := overdraftInterestEntryPrefix(account, currency)
	var total int64
	for id, e := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		// The remainder after the prefix is the UTC date ("2006-01-02",
		// 10 chars); anything else is not an overdraft-interest row of
		// this pair — defensive, since the ID scheme is the only
		// writer.
		if len(id) != len(prefix)+10 {
			continue
		}
		if e.CreatedAt.Before(since) || !e.CreatedAt.Before(until) {
			continue
		}
		var ok bool
		total, ok = addCents(total, e.AmountCents)
		if !ok {
			return 0, errors.New("ledger: overdraft interest total overflows int64")
		}
		// A reversal of this charge nets it out of the total,
		// mirroring the accrual convention in accrual.go.
		if rev, ok := l.entries[id+"/reversal"]; ok {
			total, ok = addCents(total, -rev.AmountCents)
			if !ok {
				return 0, errors.New("ledger: overdraft interest total overflows int64")
			}
		}
	}
	return total, nil
}
