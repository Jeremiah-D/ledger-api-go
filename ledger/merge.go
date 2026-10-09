package ledger

import (
	"errors"
	"sort"
	"time"
)

// Validation errors returned by PostMerge.
var (
	// ErrEmptyMergeID is returned when the merge carries no ID.
	ErrEmptyMergeID = errors.New("ledger: merge ID must not be empty")
	// ErrEmptyMergeSource is returned when the merge names no source
	// account.
	ErrEmptyMergeSource = errors.New("ledger: merge source account must not be empty")
	// ErrEmptyMergeTarget is returned when the merge names no target
	// account.
	ErrEmptyMergeTarget = errors.New("ledger: merge target account must not be empty")
	// ErrMergeSameAccount is returned when source and target are the
	// same account: a merge consolidates one account into another.
	ErrMergeSameAccount = errors.New("ledger: merge source and target must differ")
	// ErrMergeIDConflict is returned when the merge ID — or one of the
	// leg entry IDs derived from it — is already used as a journal entry
	// ID. Merge legs book under "<merge ID>/<currency>", so the merge ID
	// lives in the same ID namespace as journal entries.
	ErrMergeIDConflict = errors.New("ledger: merge ID already used as a journal entry ID")
)

// Merge describes one atomic account merge: every currency balance of the
// source account moves to the target account, one journal entry per
// currency, and the source account is frozen afterwards.
//
// It is the account-lifecycle view of the payment domain: when a merchant
// entity changes (acquisition,主体变更, account consolidation), its old
// account is drained into the successor account and decommissioned. The
// freeze is permanent by convention — nothing in the ledger unfreezes a
// merged source automatically; an operator who must resurrect it calls
// Unfreeze explicitly, and the audit log shows both the merge and the
// unfreeze.
//
// Leg semantics: for each currency with a non-zero source balance, one
// double-entry leg books under "<merge ID>/<currency>" (the merge-ID
// prefix is what marks journal entries as merge legs). A positive source
// balance moves as debit target / credit source — the target grows, the
// source shrinks to exactly zero. A negative source balance (the account
// owes money) moves as debit source / credit target: the source still
// lands at exactly zero, and the target absorbs the debt (its balance
// decreases). Debt absorption is flagged per leg (DebtAbsorbed) so
// readers never mistake it for an ordinary credit.
//
// The merge commits atomically: field validation, the idempotency replay
// check, the ID-conflict check, the frozen check on both accounts, and
// the overdraft check on the target all run before the first journal row
// lands, so a merge that fails on any check records nothing: no journal
// rows, no chain links, no version bump, and the source is not frozen.
// Readers never observe a half-merged pair.
//
// Risk-control interplay, in check order:
//   - Frozen: both the source and the target must be unfrozen. The check
//     runs after the idempotency replay check, so retrying a merge whose
//     key was posted before the freeze still returns the original
//     receipt instead of failing.
//   - Overdraft: when the target is overdraft-protected, a merge that
//     would take the target below zero in any currency — only possible
//     via debt absorption, since positive balances only ever grow the
//     target — is rejected with ErrAccountOverdraft.
//
// Holds (see hold.go) are advisory reservations, not journaled money: a
// merge moves the full journal balance, including any held amounts. It is
// the operator's job to settle or release holds before merging; the books
// stay balanced either way because every leg is an ordinary double-entry
// posting.
//
// Daily outflow limits (see daily_limit.go) do not apply: a merge is an
// administrative consolidation, not a payment outflow.
type Merge struct {
	ID string `json:"merge_id"`
	// From is the source account: drained to zero in every currency,
	// then frozen.
	From AccountID `json:"from_account"`
	// To is the target account: receives every leg.
	To             AccountID `json:"to_account"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// MergeLeg is one committed movement inside a merge: the full balance of
// From in Currency, moved to the merge's target.
type MergeLeg struct {
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	// DebtAbsorbed is true when the source balance in this currency was
	// negative: the leg zeroes the source by debiting it and crediting
	// the target, so the target absorbs the debt (its balance decreases
	// by AmountCents). False for ordinary positive-balance legs, where
	// the target grows.
	DebtAbsorbed bool `json:"debt_absorbed"`
	// EntryID is the journal entry that booked this leg:
	// "<merge ID>/<currency>". The merge-ID prefix is what marks journal
	// entries as merge legs.
	EntryID string `json:"entry_id"`
}

// MergeReceipt reports what PostMerge committed.
//
// Legs and Entries are in commit order (currencies sorted), so the
// receipt is deterministic. A merge whose source held no balances commits
// no legs — Legs and Entries are empty — but still freezes the source and
// registers its idempotency key, so a replay returns the same receipt
// instead of failing on the now-frozen source. Duplicate replays return
// the originally posted receipt with Duplicate == true and book nothing
// new.
type MergeReceipt struct {
	MergeID      string         `json:"merge_id"`
	From         AccountID      `json:"from_account"`
	To           AccountID      `json:"to_account"`
	Legs         []MergeLeg     `json:"legs"`
	Entries      []JournalEntry `json:"entries"`
	SourceFrozen bool           `json:"source_frozen"`
	Duplicate    bool           `json:"duplicate"`
}

// mergeRecord is the authoritative registry entry for one committed
// merge, kept in Ledger.merges keyed by merge ID. It feeds idempotent
// replays (via Ledger.mergeKeys, which maps idempotency keys to merge
// IDs), the Reconcile merge history, and snapshot export. Merge keys live
// in their own namespace, independent of the entry/transfer/hold/sweep
// key namespaces, and expire with the TTL like every other key index
// (see pruneMergeKeysLocked).
type mergeRecord struct {
	mergeID   string
	from      AccountID
	to        AccountID
	legs      []MergeLeg
	entryIDs  []string
	createdAt time.Time
}

// PostMerge atomically merges the source account into the target account
// and freezes the source.
//
// The check order is validation, then the idempotency replay check (a
// replayed key returns the original receipt even though the source is now
// frozen, because the replay books nothing new), then the merge-ID
// conflict check, then the frozen check on both accounts, then the
// overdraft check on the target. Rejected merges record nothing: no
// journal rows, no chain links, no version bump, no freeze. A zero
// CreatedAt is filled with the current time.
func (l *Ledger) PostMerge(m Merge) (MergeReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if m.ID == "" {
		return MergeReceipt{}, ErrEmptyMergeID
	}
	if m.From == "" {
		return MergeReceipt{}, ErrEmptyMergeSource
	}
	if m.To == "" {
		return MergeReceipt{}, ErrEmptyMergeTarget
	}
	if m.From == m.To {
		return MergeReceipt{}, ErrMergeSameAccount
	}

	// The leg plan is computed before any check that depends on ledger
	// state, but after field validation: balances are read under the
	// write lock, so the plan describes exactly what the commit will
	// book — no concurrent Post can slip in between.
	legs := l.mergeLegsLocked(m)

	if m.IdempotencyKey != "" {
		if mergeID, ok := l.mergeKeys[m.IdempotencyKey]; ok {
			rec := l.merges[mergeID]
			entries := make([]JournalEntry, 0, len(rec.entryIDs))
			for _, id := range rec.entryIDs {
				entries = append(entries, l.entries[id])
			}
			return MergeReceipt{
				MergeID:      rec.mergeID,
				From:         rec.from,
				To:           rec.to,
				Legs:         rec.legs,
				Entries:      entries,
				SourceFrozen: true,
				Duplicate:    true,
			}, nil
		}
	}

	if _, exists := l.entries[m.ID]; exists {
		return MergeReceipt{}, ErrMergeIDConflict
	}
	for _, leg := range legs {
		if _, exists := l.entries[leg.EntryID]; exists {
			return MergeReceipt{}, ErrMergeIDConflict
		}
	}

	if l.frozenLocked(m.From) || l.frozenLocked(m.To) {
		return MergeReceipt{}, ErrAccountFrozen
	}

	if l.mergeOverdraftRejectedLocked(m.From, m.To) {
		return MergeReceipt{}, ErrAccountOverdraft
	}

	// The period gate is keyed on the merge's effective post time (see
	// period.go): a backdated merge landing in a closed accounting period
	// is rejected with ErrPeriodClosed. It runs after the idempotency
	// replay check above, so replaying a key merged before the period
	// closed returns the original receipt.
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if err := l.periodRejectedLocked(createdAt); err != nil {
		return MergeReceipt{}, err
	}

	// Atomic commit: every leg's journal row, per-account index entries,
	// balances, totals, chain links, and version bumps land together,
	// under the one write lock, after all checks passed. The merge
	// registry entry and the source freeze land in the same critical
	// section: a reader never sees merged legs without the frozen source,
	// and never sees a frozen source without its legs. Any failure above
	// returned before the first mutation, so there is nothing to roll
	// back.
	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	versionBefore := l.version
	entries := make([]JournalEntry, 0, len(legs))
	for _, leg := range legs {
		e := JournalEntry{
			ID:          leg.EntryID,
			AmountCents: leg.AmountCents,
			Currency:    leg.Currency,
			CreatedAt:   createdAt,
		}
		if leg.DebtAbsorbed {
			e.DebitAccount, e.CreditAccount = m.From, m.To
		} else {
			e.DebitAccount, e.CreditAccount = m.To, m.From
		}
		l.commitEntryLocked(e)
		entries = append(entries, e)
	}
	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.merges[m.ID] = mergeRecord{
		mergeID:   m.ID,
		from:      m.From,
		to:        m.To,
		legs:      legs,
		entryIDs:  entryIDs,
		createdAt: createdAt,
	}
	if m.IdempotencyKey != "" {
		l.mergeKeys[m.IdempotencyKey] = m.ID
	}
	// The source is decommissioned: frozen in the same critical section
	// as the legs. Freeze does not bump the version (see freeze.go) —
	// balances are what version tracks, and the freeze changes none.
	l.frozen[m.From] = true

	l.emitAudit(AuditEvent{
		Op:            "merge",
		Actor:         "PostMerge",
		TraceID:       m.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{m.From, m.To},
		Details: map[string]any{
			"legs": len(legs),
		},
	})

	return MergeReceipt{
		MergeID:      m.ID,
		From:         m.From,
		To:           m.To,
		Legs:         legs,
		Entries:      entries,
		SourceFrozen: true,
		Duplicate:    false,
	}, nil
}

// mergeLegsLocked plans the legs of a merge: one leg per currency with a
// non-zero source balance, currencies sorted for determinism. Positive
// balances move as debit target / credit source; negative balances are
// absorbed as debt (debit source / credit target) so the source still
// lands at exactly zero. Callers must hold l.mu.
func (l *Ledger) mergeLegsLocked(m Merge) []MergeLeg {
	var currencies []string
	for k, bal := range l.balances {
		if k.account == m.From && bal != 0 {
			currencies = append(currencies, k.currency)
		}
	}
	sort.Strings(currencies)
	legs := make([]MergeLeg, 0, len(currencies))
	for _, currency := range currencies {
		bal := l.balances[accountCurrency{account: m.From, currency: currency}]
		leg := MergeLeg{
			From:     m.From,
			To:       m.To,
			Currency: currency,
			EntryID:  m.ID + "/" + currency,
		}
		if bal < 0 {
			leg.AmountCents = -bal
			leg.DebtAbsorbed = true
		} else {
			leg.AmountCents = bal
		}
		legs = append(legs, leg)
	}
	return legs
}

// mergeOverdraftRejectedLocked reports whether merging from into target
// would overdraw an overdraft-protected target. The target's per-currency
// delta is the source's balance in that currency — positive balances flow
// in, negative balances are absorbed as debt — so only a negative delta
// can overdraw. The comparison never subtracts, so it cannot overflow:
// T + d < 0 with d < 0 is equivalent to T < -d, and -d is positive. (A
// source balance of math.MinInt64 would make -d overflow, but such a
// balance would require posting more cents than int64 can count, which
// commitEntryLocked's own additions would have wrapped long before.)
// Callers must hold l.mu.
func (l *Ledger) mergeOverdraftRejectedLocked(from, target AccountID) bool {
	if !l.noOverdraft[target] {
		return false
	}
	for k, bal := range l.balances {
		if k.account != from || bal >= 0 {
			continue
		}
		if l.balances[accountCurrency{account: target, currency: k.currency}] < -bal {
			return true
		}
	}
	return false
}

// pruneMergeKeysLocked evicts merge idempotency keys older than the TTL,
// mirroring the entry-level key expiry: after the TTL, reposting a merge
// key attempts a brand-new merge (which will fail on the frozen source
// with ErrAccountFrozen — the merge already happened). The merge registry
// itself (Ledger.merges) is untouched: it is the audit history Reconcile
// reports, not a replay-detection index. A non-positive TTL disables
// pruning. Callers must hold l.mu (the write lock; the prune mutates the
// index).
func (l *Ledger) pruneMergeKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, mergeID := range l.mergeKeys {
		if rec, ok := l.merges[mergeID]; ok && rec.createdAt.Before(cutoff) {
			delete(l.mergeKeys, key)
			removed++
		}
	}
	return removed
}
