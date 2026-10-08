package ledger

import (
	"errors"
	"sort"
)

// Validation errors for the sub-account hierarchy.
var (
	// ErrEmptyAccountID is returned by hierarchy operations that name an
	// empty account.
	ErrEmptyAccountID = errors.New("ledger: account ID must not be empty")
	// ErrAccountSelfParent is returned by SetParent when an account is
	// assigned as its own parent.
	ErrAccountSelfParent = errors.New("ledger: an account cannot be its own parent")
	// ErrParentCycle is returned by SetParent when the assignment would
	// close a cycle in the account hierarchy (the child is already an
	// ancestor of the proposed parent). The hierarchy must stay a forest
	// so balance rollups always terminate.
	ErrParentCycle = errors.New("ledger: parent assignment would create a cycle in the account hierarchy")
)

// SetParent assigns parent as the parent account of child, building the
// sub-account hierarchy that Rollup aggregates over. The classic shape is
// a merchant account with sub-merchant children, each of which may have
// its own children. Passing an empty parent clears the assignment.
//
// The assignment is structural, not bookkeeping: like Freeze, it does
// not bump the ledger version, so a hierarchy change never looks like
// money movement to version-based change detectors.
//
// Accounts need no prior registration — an account springs into existence
// on its first Post, and a parent can be named before either side has
// postings, which is exactly how a merchant onboards sub-merchants before
// their first settlement.
//
// A self-assignment is rejected with ErrAccountSelfParent, and any
// assignment that would close a cycle is rejected with ErrParentCycle.
// The check walks the ancestor chain of the proposed parent: if the child
// appears anywhere on it, linking them would create a loop.
func (l *Ledger) SetParent(child, parent AccountID) error {
	if child == "" {
		return ErrEmptyAccountID
	}
	if parent == child {
		return ErrAccountSelfParent
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if parent != "" {
		for a := parent; a != ""; a = l.parents[a] {
			if a == child {
				return ErrParentCycle
			}
		}
		l.parents[child] = parent
	} else {
		delete(l.parents, child)
	}
	return nil
}

// Parent returns the parent account of child, or false when the account
// has no parent assigned.
func (l *Ledger) Parent(child AccountID) (AccountID, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.parents[child]
	return p, ok
}

// RolledCurrencyBalance is the aggregated net balance of an account
// subtree in one currency: the account's own balance plus every
// descendant's, in that currency only. Currencies are never summed
// together — each row stands on its own, like the accounting equation.
type RolledCurrencyBalance struct {
	Currency     string `json:"currency"`
	BalanceCents int64  `json:"balance_cents"`
}

// AccountRollup aggregates an account's balances with those of every
// descendant in the sub-account hierarchy (see SetParent): the subtree
// rooted at Account. It is the settlement operator's view — one merchant
// account rolling up all its sub-merchants — computed in a single read
// pass, so the rows describe one consistent ledger version.
type AccountRollup struct {
	Account AccountID `json:"account"`
	// Parent is the account's own parent, empty when it is a hierarchy
	// root (or has no parent assigned).
	Parent AccountID `json:"parent,omitempty"`
	// DescendantCount is the number of accounts below Account in the
	// hierarchy, not counting Account itself.
	DescendantCount int `json:"descendant_count"`
	// Accounts is Account plus every descendant, sorted. An account that
	// has never posted still appears here when it is linked into the
	// hierarchy — a sub-merchant exists before its first settlement.
	Accounts []AccountID `json:"accounts"`
	// ByCurrency is the per-currency aggregated net balance of the whole
	// subtree, sorted by currency code. Accounts with no postings in a
	// currency contribute zero to that row.
	ByCurrency []RolledCurrencyBalance `json:"by_currency"`
	// Version is the ledger version the rollup was read at — the same
	// sequence Snapshot reports, so a rollup and a snapshot taken at one
	// version describe the same books.
	Version uint64 `json:"version"`
	// Frozen reports whether Account itself is currently stopped by a
	// risk-control freeze (see Freeze). Descendants have their own flags;
	// the rollup does not conflate them.
	Frozen bool `json:"frozen"`
}

// Rollup returns the balance rollup of the subtree rooted at the given
// account: the account itself plus every descendant in the sub-account
// hierarchy. An unknown account — or one with no descendants — rolls up
// to just itself with zero balances. The scan holds the read lock, so it
// runs concurrently with other reads and never observes a half-posted
// entry.
func (l *Ledger) Rollup(a AccountID) AccountRollup {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.rollupLocked(a)
}

// rollupLocked builds the rollup. Callers must hold l.mu; the read lock
// suffices because nothing is mutated. The walk is a BFS over the
// child-adjacency rebuilt from the parent map on each call: the hierarchy
// is small (accounts, not entries) and rebuilding keeps SetParent O(1)
// instead of maintaining a second index.
func (l *Ledger) rollupLocked(a AccountID) AccountRollup {
	children := make(map[AccountID][]AccountID, len(l.parents))
	for child, parent := range l.parents {
		children[parent] = append(children[parent], child)
	}

	// The parent map is cycle-free by construction (SetParent rejects
	// cycles), so this BFS always terminates. The `seen` guard is
	// belt-and-braces against a corrupted map.
	seen := map[AccountID]bool{a: true}
	queue := []AccountID{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}

	accounts := make([]AccountID, 0, len(seen))
	for acct := range seen {
		accounts = append(accounts, acct)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })

	// Currency discovery mirrors trialBalanceLocked: union the key sets of
	// the balance and totals maps on the account dimension, so a currency
	// with only debit/credit totals (a storage-layer oddity) still shows
	// up. Balances are summed per currency across the whole subtree.
	currencies := make(map[string]bool)
	for k := range l.balances {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	for k := range l.debitTotals {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	for k := range l.creditTotals {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	codes := make([]string, 0, len(currencies))
	for c := range currencies {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	byCurrency := make([]RolledCurrencyBalance, 0, len(codes))
	for _, c := range codes {
		var total int64
		for acct := range seen {
			total += l.balances[accountCurrency{account: acct, currency: c}]
		}
		byCurrency = append(byCurrency, RolledCurrencyBalance{
			Currency:     c,
			BalanceCents: total,
		})
	}

	return AccountRollup{
		Account:         a,
		Parent:          l.parents[a],
		DescendantCount: len(seen) - 1,
		Accounts:        accounts,
		ByCurrency:      byCurrency,
		Version:         l.version,
		Frozen:          l.frozen[a],
	}
}
