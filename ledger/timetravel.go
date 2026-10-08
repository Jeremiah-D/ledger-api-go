package ledger

import "errors"

// ErrVersionInFuture is returned by BalanceAt when the requested version
// is beyond the ledger's current version: the future has no balance yet,
// and inventing one would be a lie the audit trail could not support.
var ErrVersionInFuture = errors.New("ledger: version is beyond the current ledger version")

// BalanceAt returns the net balance (in cents) of the given account in the
// given currency as of ledger version `version` — the balance after exactly
// `version` successful posts. It is the historical counterpart of
// Balance/BalanceIn/Snapshot: the same version Snapshot reports can be fed
// back here to reproduce the balance that was current at that version,
// which makes it the primitive for audit replay and point-in-time
// reconciliation ("what did this account hold when the incident happened").
//
// Version 0 is the genesis: every account reports zero. A version beyond
// the current ledger version is rejected with ErrVersionInFuture — time
// travel only goes backwards. An empty currency means the default currency
// (see DefaultCurrency); unknown accounts report zero at every version.
//
// Implementation: one prefix scan over the audit chain under a single read
// lock. The chain holds exactly one link per successful Post in Post order
// (link i is the entry posted at version i+1 — the commit path bumps the
// version and appends the link together), so folding the first `version`
// links reproduces the balance exactly as the books showed it then. The
// result is a consistent point-in-time view: concurrent Posts cannot slip
// in mid-scan.
//
// Cost is O(version) per query — a deliberate trade-off: the journal is
// append-only, so a cached BalanceAt answer never goes stale. Callers that
// read the same historical version repeatedly should cache the result
// rather than rescan.
func (l *Ledger) BalanceAt(a AccountID, currency string, version uint64) (int64, error) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if version > l.version {
		return 0, ErrVersionInFuture
	}
	var balance int64
	for _, link := range l.chain[:version] {
		e := l.entries[link.entryID]
		if e.Currency != currency {
			continue
		}
		if e.DebitAccount == a {
			balance += e.AmountCents
		}
		if e.CreditAccount == a {
			balance -= e.AmountCents
		}
	}
	return balance, nil
}
