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

// ErrCreditLimitExceeded is returned by Post when the credit account
// (the account funds leave) carries a credit line (see creditline.go) and
// the posting would take its balance below the line's floor (-limit). It
// is a 422-class semantic rejection, like ErrAccountOverdraft: the request
// is well-formed, the credit just is not there. Unlike the overdraft
// error, it names the line so callers can distinguish "protected at zero"
// from "protected at -limit".
var ErrCreditLimitExceeded = errors.New("ledger: posting would exceed the account's credit line")

// overdraftRejectedLocked reports whether posting e as the credit (payer)
// leg would overdraw a protected account, returning the rejection error
// or nil. A protected account with a credit line for the entry's currency
// (see GrantCreditLine) may go negative down to -limit: postings that
// stay within the line clear the gate, postings that would breach the
// floor are rejected with ErrCreditLimitExceeded, and a protected account
// without a line keeps the zero floor with ErrAccountOverdraft. The
// frozen gate always runs first (see the Post gate order), so a credit
// line never un-freezes an account — frozen still fully blocks.
//
// The check runs against the account's balance in the entry's currency —
// a EUR balance cannot cover a USD posting. Callers must hold l.mu. The
// comparison never subtracts, so it cannot overflow even when the account
// balance is already negative and the amount is huge: bal < amount + floor
// is equivalent to bal - amount < floor for every int64 triple, and the
// sum amount + floor cannot overflow because floor <= 0 and amount > 0
// bound it within int64 (floor >= -limit >= -MaxInt64).
func (l *Ledger) overdraftRejectedLocked(e JournalEntry) error {
	if !l.noOverdraft[e.CreditAccount] {
		return nil
	}
	var floor int64
	if cl, ok := l.creditLines[creditLineKey{account: e.CreditAccount, currency: e.Currency}]; ok {
		floor = -cl.LimitCents
	}
	if l.balances[accountCurrency{account: e.CreditAccount, currency: e.Currency}] < e.AmountCents+floor {
		if floor < 0 {
			return ErrCreditLimitExceeded
		}
		return ErrAccountOverdraft
	}
	return nil
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
