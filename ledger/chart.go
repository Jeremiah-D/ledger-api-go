package ledger

import (
	"errors"
	"sort"
)

// Chart of accounts (opt-in).
//
// A chart of accounts classifies every account into one of the five
// classic categories — asset, liability, equity, revenue, expense — and
// enforces the normal posting direction for registered accounts: the debit
// leg of a journal entry must name an asset or an expense account (a debit
// increases them), and the credit leg must name a liability, equity, or
// revenue account (a credit increases them). A posting that books the
// reverse direction — debiting a liability, crediting an asset — is
// rejected with ErrAccountDirectionMismatch before anything is recorded.
//
// The control is opt-in per account: accounts without a registered type
// skip the check entirely, so a ledger that never registers anything
// posts exactly as before. Internal contra and clearing accounts that
// routinely book both directions (a payable credited by accruals and
// debited by their reversals, an FX clearing account, suspense) should
// stay unregistered or use dedicated contra accounts — this is the
// strict chart-of-accounts control accounting software applies to
// statement accounts, not a blanket double-entry rule.
//
// Chart registrations are structural config, like the frozen-account list
// and the overdraft guards: registering or clearing a type does not bump
// the journal version, and registrations round-trip through the
// disaster-recovery snapshot config record (see snapshot.go). The check
// runs in Post's gate order after the idempotency replay check and before
// the frozen check: replaying a key that was posted before the account
// was registered returns the original entry instead of failing, because
// the replay books nothing new.

// AccountType is the chart-of-accounts category of an account.
type AccountType string

// The five chart-of-accounts categories.
const (
	AccountTypeAsset     AccountType = "asset"
	AccountTypeLiability AccountType = "liability"
	AccountTypeEquity    AccountType = "equity"
	AccountTypeRevenue   AccountType = "revenue"
	AccountTypeExpense   AccountType = "expense"
)

// Validation errors for the chart of accounts.
var (
	// ErrInvalidAccountType is returned by RegisterAccountType when the
	// type is not one of the five chart-of-accounts categories.
	ErrInvalidAccountType = errors.New("ledger: invalid account type")
	// ErrEmptyChartAccount is returned by RegisterAccountType when the
	// account ID is empty.
	ErrEmptyChartAccount = errors.New("ledger: chart-of-accounts account must not be empty")
	// ErrAccountDirectionMismatch is returned by Post when a leg of the
	// entry books against the account's chart-of-accounts type: a debit
	// leg naming a liability/equity/revenue account, or a credit leg
	// naming an asset/expense account. The posting records nothing — no
	// journal row, no chain link, no version bump. Accounts without a
	// registered type never trigger this error.
	ErrAccountDirectionMismatch = errors.New("ledger: posting direction conflicts with the account's chart-of-accounts type")
)

// AccountTypeRegistration is one row of the chart of accounts: the type
// registered for an account. It is the unit the snapshot config record
// serializes (see snapshot.go).
type AccountTypeRegistration struct {
	Account AccountID   `json:"account"`
	Type    AccountType `json:"type"`
}

// validAccountType reports whether t is one of the five chart categories.
func validAccountType(t AccountType) bool {
	switch t {
	case AccountTypeAsset, AccountTypeLiability, AccountTypeEquity,
		AccountTypeRevenue, AccountTypeExpense:
		return true
	}
	return false
}

// RegisterAccountType records the chart-of-accounts type of an account.
// Re-registering an account replaces its type; clearing is
// ClearAccountType. Registration is structural config: it does not bump
// the journal version, and it survives disaster-recovery snapshots.
func (l *Ledger) RegisterAccountType(account AccountID, t AccountType) error {
	if account == "" {
		return ErrEmptyChartAccount
	}
	if !validAccountType(t) {
		return ErrInvalidAccountType
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chart[account] = t
	return nil
}

// ClearAccountType removes the chart-of-accounts registration of an
// account. Clearing an account that was never registered is a no-op.
func (l *Ledger) ClearAccountType(account AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.chart, account)
}

// AccountTypeOf reports the registered chart-of-accounts type of an
// account. The second return value is false when the account is
// unregistered — unregistered accounts skip the direction check in Post.
func (l *Ledger) AccountTypeOf(account AccountID) (AccountType, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.chart[account]
	return t, ok
}

// accountTypesLocked lists every chart registration, sorted by account.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) accountTypesLocked() []AccountTypeRegistration {
	out := make([]AccountTypeRegistration, 0, len(l.chart))
	for a, t := range l.chart {
		out = append(out, AccountTypeRegistration{Account: a, Type: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// chartDirectionRejectedLocked reports whether either leg of e books
// against the registered chart-of-accounts type of the account it names:
// a debit leg on a liability/equity/revenue account (a debit decreases
// them), or a credit leg on an asset/expense account (a credit decreases
// them). Unregistered accounts are skipped. Callers must hold l.mu.
func (l *Ledger) chartDirectionRejectedLocked(e JournalEntry) error {
	if t, ok := l.chart[e.DebitAccount]; ok {
		switch t {
		case AccountTypeLiability, AccountTypeEquity, AccountTypeRevenue:
			return ErrAccountDirectionMismatch
		}
	}
	if t, ok := l.chart[e.CreditAccount]; ok {
		switch t {
		case AccountTypeAsset, AccountTypeExpense:
			return ErrAccountDirectionMismatch
		}
	}
	return nil
}

// AccountTypeTotals aggregates one chart-of-accounts category's books in
// one currency: every cent ever debited to registered accounts of that
// type, every cent ever credited from them, and the resulting net.
// Unregistered accounts contribute to no category. Sorted by (type,
// currency) in Reconcile's report.
type AccountTypeTotals struct {
	Type             AccountType `json:"type"`
	Currency         string      `json:"currency"`
	TotalDebitsCents int64       `json:"total_debits_cents"`
	TotalCreditsCents int64      `json:"total_credits_cents"`
	NetBalanceCents  int64       `json:"net_balance_cents"`
}

// accountTypeTotalsLocked aggregates the chart-of-accounts categories
// from the debit/credit totals and net balances, restricted to accounts
// with a registered type. Callers must hold l.mu; the read lock suffices
// because nothing is mutated.
func (l *Ledger) accountTypeTotalsLocked() []AccountTypeTotals {
	type key struct {
		typ      AccountType
		currency string
	}
	agg := make(map[key]*AccountTypeTotals)
	touch := func(t AccountType, currency string) *AccountTypeTotals {
		k := key{typ: t, currency: currency}
		if a, ok := agg[k]; ok {
			return a
		}
		a := &AccountTypeTotals{Type: t, Currency: currency}
		agg[k] = a
		return a
	}
	// Only (account, currency) pairs carrying activity appear in the
	// totals maps, so iterating them (rather than the chart) keeps the
	// scan proportional to booked data.
	for k, d := range l.debitTotals {
		if t, ok := l.chart[k.account]; ok {
			touch(t, k.currency).TotalDebitsCents += d
		}
	}
	for k, c := range l.creditTotals {
		if t, ok := l.chart[k.account]; ok {
			touch(t, k.currency).TotalCreditsCents += c
		}
	}
	for k, b := range l.balances {
		if t, ok := l.chart[k.account]; ok {
			touch(t, k.currency).NetBalanceCents += b
		}
	}
	out := make([]AccountTypeTotals, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}
