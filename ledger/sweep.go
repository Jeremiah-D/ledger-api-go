package ledger

import (
	"errors"
	"sort"
	"time"
)

// Validation errors returned by PostSweep.
var (
	// ErrEmptySweepID is returned when the sweep carries no ID.
	ErrEmptySweepID = errors.New("ledger: sweep ID must not be empty")
	// ErrEmptySweepSources is returned when the sweep names no source
	// account.
	ErrEmptySweepSources = errors.New("ledger: sweep must name at least one source account")
	// ErrEmptySweepSource is returned when one of the source accounts is
	// empty.
	ErrEmptySweepSource = errors.New("ledger: sweep source account must not be empty")
	// ErrEmptySweepTarget is returned when the sweep names no target
	// account.
	ErrEmptySweepTarget = errors.New("ledger: sweep target account must not be empty")
	// ErrSweepTargetIsSource is returned when the target account also
	// appears among the sources: a sweep moves money from the sources to
	// the target, so they must be disjoint.
	ErrSweepTargetIsSource = errors.New("ledger: sweep target must not be one of the source accounts")
	// ErrDuplicateSweepSource is returned when a source account is named
	// twice: each source is swept exactly once per sweep, in From order.
	ErrDuplicateSweepSource = errors.New("ledger: sweep source accounts must be distinct")
	// ErrSweepIDConflict is returned when the sweep ID — or one of the
	// leg entry IDs derived from it — is already used as a journal entry
	// ID. Sweep legs book under "<sweep ID>/<source>/<currency>", so the
	// sweep ID lives in the same ID namespace as journal entries.
	ErrSweepIDConflict = errors.New("ledger: sweep ID already used as a journal entry ID")
)

// Sweep describes one atomic balance sweep: the positive per-currency
// balances of the source accounts are moved to the target account, one
// journal entry per (source, currency).
//
// It is the treasury view of the payment domain: a merchant sweeping its
// sub-merchants' settlement balances (see Rollup) into the master
// account at the end of the day. Internally each leg reuses transfer
// semantics — debit the target (its balance grows), credit the source
// (its balance shrinks) — but a sweep never charges a fee: sweeps are
// internal treasury movements, so the ledger-wide transfer fee policy
// (see WithTransferFeePolicy) does not apply, and an explicit fee cannot
// be attached.
//
// Only positive balances move. A zero balance is skipped ("empty balance
// skipped"), and a negative balance is skipped too: a negative balance
// means the account owes money, and sweeping it would move debt into the
// treasury account — the caller settles negatives explicitly. Skipped
// sources simply contribute no leg.
//
// The sweep commits atomically: validation, the idempotency replay check,
// the ID-conflict check, and the frozen checks on every touched account
// all run before the first journal row lands, so a sweep that fails on any
// account records nothing: no journal rows, no chain links, no version
// bump. Readers never observe a half-swept batch.
//
// Overdraft protection needs no check: each leg moves its source's full
// positive balance, so every source lands at exactly zero and can never
// go negative. The target only ever receives.
//
// Holds (see hold.go) are advisory reservations, not journaled money: a
// sweep moves the full journal balance, including any held amounts. It is
// the operator's job to settle or release holds before sweeping; the sweep
// documents what it did in the receipt, and the books stay balanced either
// way because every leg is an ordinary double-entry posting.
type Sweep struct {
	ID string `json:"sweep_id"`
	// From lists the source accounts in sweep order. Entries commit in
	// this order, with currencies sorted within each source, so the
	// receipt is deterministic.
	From []AccountID `json:"from_accounts"`
	// To is the target account that receives every leg.
	To             AccountID `json:"to_account"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// SweepLeg is one committed movement inside a sweep: the full positive
// balance of From in Currency, moved to the sweep's target.
type SweepLeg struct {
	From        AccountID `json:"from_account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	// EntryID is the journal entry that booked this leg:
	// "<sweep ID>/<source>/<currency>". The sweep-ID prefix is what marks
	// journal entries as sweep legs.
	EntryID string `json:"entry_id"`
}

// SweepReceipt reports what PostSweep committed.
//
// Legs and Entries are in commit order: source order, currencies sorted
// within each source. A sweep whose sources all had non-positive balances
// commits nothing — Legs and Entries are empty — but still registers its
// idempotency key, so a replay returns the same empty receipt instead of
// sweeping balances that arrived later. Duplicate replays return the
// originally posted legs with Duplicate == true and book nothing new.
type SweepReceipt struct {
	SweepID   string         `json:"sweep_id"`
	Legs      []SweepLeg     `json:"legs"`
	Entries   []JournalEntry `json:"entries"`
	Duplicate bool           `json:"duplicate"`
}

// sweepRecord remembers what a sweep posted so idempotent replays can
// rebuild the receipt. Sweep keys live in their own namespace (sweepKeys),
// independent of the entry/transfer/hold key namespaces, and expire with
// the TTL like every other key index (see pruneSweepKeysLocked).
type sweepRecord struct {
	sweepID   string
	entryIDs  []string
	createdAt time.Time
}

// PostSweep atomically sweeps the positive per-currency balances of the
// source accounts into the target account, one journal entry per
// (source, currency).
//
// The check order mirrors PostTransfer: field validation first, then the
// idempotency replay check (a replayed key returns the original receipt
// even if an account was frozen after the original sweep, because the
// replay books nothing new), then the sweep-ID conflict check, then the
// frozen check on every account the sweep touches (all sources and the
// target). Rejected sweeps record nothing: no journal rows, no chain
// links, no version bump. A zero CreatedAt is filled with the current
// time.
func (l *Ledger) PostSweep(s Sweep) (SweepReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if s.ID == "" {
		return SweepReceipt{}, ErrEmptySweepID
	}
	if len(s.From) == 0 {
		return SweepReceipt{}, ErrEmptySweepSources
	}
	if s.To == "" {
		return SweepReceipt{}, ErrEmptySweepTarget
	}
	seen := make(map[AccountID]bool, len(s.From))
	for _, from := range s.From {
		if from == "" {
			return SweepReceipt{}, ErrEmptySweepSource
		}
		if from == s.To {
			return SweepReceipt{}, ErrSweepTargetIsSource
		}
		if seen[from] {
			return SweepReceipt{}, ErrDuplicateSweepSource
		}
		seen[from] = true
	}

	// The leg plan is computed before any check that depends on ledger
	// state, but after field validation: balances are read under the
	// write lock, so the plan describes exactly what the commit will
	// book — no concurrent Post can slip in between.
	legs := l.sweepLegsLocked(s)

	if s.IdempotencyKey != "" {
		if rec, ok := l.sweepKeys[s.IdempotencyKey]; ok {
			entries := make([]JournalEntry, 0, len(rec.entryIDs))
			for _, id := range rec.entryIDs {
				entries = append(entries, l.entries[id])
			}
			return SweepReceipt{
				SweepID:   rec.sweepID,
				Legs:      sweepLegsOf(entries),
				Entries:   entries,
				Duplicate: true,
			}, nil
		}
	}

	if _, exists := l.entries[s.ID]; exists {
		return SweepReceipt{}, ErrSweepIDConflict
	}
	for _, leg := range legs {
		if _, exists := l.entries[leg.EntryID]; exists {
			return SweepReceipt{}, ErrSweepIDConflict
		}
	}

	if l.frozenLocked(s.To) {
		return SweepReceipt{}, ErrAccountFrozen
	}
	for _, from := range s.From {
		if l.frozenLocked(from) {
			return SweepReceipt{}, ErrAccountFrozen
		}
	}

	// Atomic commit: every leg's journal row, per-account index entries,
	// balances, totals, chain links, and version bumps land together,
	// under the one write lock, after all checks passed. Any failure
	// above returned before the first mutation, so there is nothing to
	// roll back.
	now := time.Now()
	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	l.maybePruneIdempotencyKeys(now)
	entries := make([]JournalEntry, 0, len(legs))
	for _, leg := range legs {
		e := JournalEntry{
			ID:            leg.EntryID,
			DebitAccount:  s.To,
			CreditAccount: leg.From,
			AmountCents:   leg.AmountCents,
			Currency:      leg.Currency,
			CreatedAt:     createdAt,
		}
		l.commitEntryLocked(e)
		entries = append(entries, e)
	}
	if s.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.sweepKeys[s.IdempotencyKey] = sweepRecord{
			sweepID:   s.ID,
			entryIDs:  ids,
			createdAt: createdAt,
		}
	}

	return SweepReceipt{
		SweepID:   s.ID,
		Legs:      legs,
		Entries:   entries,
		Duplicate: false,
	}, nil
}

// sweepLegsLocked plans the legs of a sweep: for each source in From
// order, one leg per currency with a positive balance, currencies sorted
// for determinism. Zero and negative balances contribute no leg. Callers
// must hold l.mu.
func (l *Ledger) sweepLegsLocked(s Sweep) []SweepLeg {
	var legs []SweepLeg
	for _, from := range s.From {
		var currencies []string
		for k, bal := range l.balances {
			if k.account == from && bal > 0 {
				currencies = append(currencies, k.currency)
			}
		}
		sort.Strings(currencies)
		for _, currency := range currencies {
			legs = append(legs, SweepLeg{
				From:        from,
				Currency:    currency,
				AmountCents: l.balances[accountCurrency{account: from, currency: currency}],
				EntryID:     s.ID + "/" + string(from) + "/" + currency,
			})
		}
	}
	return legs
}

// sweepLegsOf rebuilds the leg descriptors of a replayed sweep from its
// committed entries. The entry ID convention "<sweep ID>/<source>/<currency>"
// carries everything the leg needs; the source account is the credit leg.
func sweepLegsOf(entries []JournalEntry) []SweepLeg {
	legs := make([]SweepLeg, 0, len(entries))
	for _, e := range entries {
		legs = append(legs, SweepLeg{
			From:        e.CreditAccount,
			Currency:    e.Currency,
			AmountCents: e.AmountCents,
			EntryID:     e.ID,
		})
	}
	return legs
}

// pruneSweepKeysLocked evicts sweep idempotency keys older than the TTL,
// mirroring the entry-level key expiry: after the TTL, reposting a sweep
// key books a brand-new sweep. The journaled sweep legs themselves are
// untouched — only the replay-detection index shrinks. A non-positive TTL
// disables pruning. Callers must hold l.mu (the write lock; the sweep
// mutates the index).
func (l *Ledger) pruneSweepKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.sweepKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.sweepKeys, key)
			removed++
		}
	}
	return removed
}
