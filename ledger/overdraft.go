package ledger

import (
	"errors"
	"sort"
)

// ErrAccountOverdraft is returned by Post when the credit account (the
// account funds leave) is under overdraft protection and the posting would
// drive its balance below zero.
var ErrAccountOverdraft = errors.New("ledger: posting would overdraw a protected account")

// EnableOverdraftProtection marks an account as protected from overdrafts:
// any Post that names it as the credit account and would take its balance
// below zero is rejected with ErrAccountOverdraft. Protection is opt-in per
// account — internal accounts (suspense, revenue, settlement) routinely
// carry negative balances in this ledger's sign convention, so a blanket
// rule would break ordinary bookkeeping. Enabling protection for an
// already-negative account does not change its balance; it only stops
// further Postings that would take it lower.
func (l *Ledger) EnableOverdraftProtection(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.noOverdraft[a] = true
}

// DisableOverdraftProtection lifts the overdraft guard from an account.
// Disabling protection for an account that was never protected is a no-op.
func (l *Ledger) DisableOverdraftProtection(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.noOverdraft, a)
}

// OverdraftProtected reports whether the account is currently guarded
// against overdrafts.
func (l *Ledger) OverdraftProtected(a AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.noOverdraft[a]
}

// overdraftRejectedLocked reports whether posting e as the credit (payer)
// leg would overdraw a protected account. The check runs against the
// account's balance in the entry's currency — a EUR balance cannot cover
// a USD posting. Callers must hold l.mu. The comparison never subtracts,
// so it cannot overflow even when the account balance is already negative
// and the amount is huge: b < a is equivalent to b - a < 0 for every int64
// pair.
func (l *Ledger) overdraftRejectedLocked(e JournalEntry) bool {
	return l.noOverdraft[e.CreditAccount] &&
		l.balances[accountCurrency{account: e.CreditAccount, currency: e.Currency}] < e.AmountCents
}

// overdraftProtectedAccountsLocked lists every currently protected
// account, sorted. Callers must hold l.mu.
func (l *Ledger) overdraftProtectedAccountsLocked() []AccountID {
	out := make([]AccountID, 0, len(l.noOverdraft))
	for a := range l.noOverdraft {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
