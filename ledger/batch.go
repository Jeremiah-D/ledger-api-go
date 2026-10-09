package ledger

import (
	"errors"
	"math"
	"time"
)

// Atomic batch posting.
//
// PostBatch commits several double-entry journal entries in one atomic
// step: the payroll / bulk-settlement view of the ledger. Every entry is
// an ordinary double-entry posting (see validateJournalEntry); the batch
// is the unit of atomicity — either every new entry lands or none do.
//
// The check order mirrors Post and PostTransfer, lifted to the batch:
// field validation of every entry first (one bad leg rejects the whole
// batch — no partial booking), then the batch-level idempotency replay
// check, then per-entry idempotency resolution against the shared Post
// key namespace, then entry-ID conflicts, then the frozen / overdraft /
// daily-limit risk controls. The risk controls run sequentially in batch
// order against a saturating simulation of the batch's own balance
// effects, so a batch cannot smuggle an overdraft or a limit breach past
// the per-entry guards by splitting one big outflow into small legs.
//
// Idempotency is two-level. The batch-level key (Batch.IdempotencyKey)
// lives in its own namespace (batchKeys): reposting it returns the whole
// original receipt with Duplicate == true and books nothing new. Each
// entry's own IdempotencyKey shares the entry-level namespace with Post:
// a key already posted — by Post, by an earlier batch, or by a transfer
// principal — resolves to the originally posted entry, which is returned
// in the receipt without being booked again. A batch may freely mix fresh
// entries and replays; only the fresh ones consume budget in the risk
// checks and land in the journal.
//
// A rejected batch records nothing: no journal rows, no idempotency
// registrations, no chain links, no version bump. Replays — at either
// level — record nothing either. Readers never observe a half-posted
// batch.
//
// Every committed entry carries the batch's ID (JournalEntry.BatchID),
// so operators can trace a bulk settlement back to its batch, and goes
// through commitEntryLocked like any other posting: one audit-chain link
// per entry, in batch order. Reconcile's VerifyChain and
// VerifyAccountingEquation therefore cover batch entries exactly like
// ordinary postings — no separate batch bookkeeping exists to drift.

// Validation errors returned by PostBatch.
var (
	// ErrEmptyBatchID is returned when the batch carries no ID.
	ErrEmptyBatchID = errors.New("ledger: batch ID must not be empty")
	// ErrEmptyBatch is returned when the batch names no entries.
	ErrEmptyBatch = errors.New("ledger: batch must contain at least one entry")
	// ErrBatchDuplicateEntryID is returned when two entries in one batch
	// share an entry ID.
	ErrBatchDuplicateEntryID = errors.New("ledger: batch contains duplicate entry ID")
	// ErrBatchEntryIDConflict is returned when a new batch entry's ID is
	// already used as a journal entry ID. Reusing an ID would overwrite
	// the journal row while appending a new chain link, corrupting the
	// audit chain — so the whole batch is rejected instead.
	ErrBatchEntryIDConflict = errors.New("ledger: batch entry ID already used as a journal entry ID")
	// ErrBatchDuplicateIdempotencyKey is returned when two new entries in
	// one batch share an idempotency key: the key could only ever resolve
	// to one of them, so the batch is ambiguous and rejected. (Two
	// entries whose key replays an already-posted entry both resolve to
	// that original — that is not a conflict.)
	ErrBatchDuplicateIdempotencyKey = errors.New("ledger: batch contains duplicate entry idempotency key")
)

// Batch is one atomic multi-entry posting: the payroll / bulk-settlement
// request. Entries commit in slice order; a zero CreatedAt on the batch
// or on an entry is filled with the post time.
type Batch struct {
	ID             string         `json:"batch_id"`
	Entries        []JournalEntry `json:"entries"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// BatchReceipt reports what PostBatch committed.
//
// Entries holds every entry of the batch in batch order: freshly posted
// entries carry the batch's ID (JournalEntry.BatchID), and replayed
// entries are the originally posted rows. Duplicate replays return the
// originally posted entries with Duplicate == true and book nothing new.
type BatchReceipt struct {
	BatchID   string         `json:"batch_id"`
	Entries   []JournalEntry `json:"entries"`
	Duplicate bool           `json:"duplicate"`
}

// batchRecord remembers what a batch posted so idempotent replays can
// rebuild the receipt. Batch keys live in their own namespace
// (batchKeys), independent of the entry/transfer/hold/sweep/merge key
// namespaces, and expire with the TTL like every other key index (see
// pruneBatchKeysLocked).
type batchRecord struct {
	batchID   string
	entryIDs  []string
	createdAt time.Time
}

// PostBatch atomically posts every entry of b. See the package comment
// for the check order and the two-level idempotency semantics. A zero
// b.CreatedAt is filled with the current time; entries with a zero
// CreatedAt inherit the batch's post time (so the daily-limit day bucket
// is deterministic for entries that do not name their own timestamp).
func (l *Ledger) PostBatch(b Batch) (BatchReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b.ID == "" {
		return BatchReceipt{}, ErrEmptyBatchID
	}
	if len(b.Entries) == 0 {
		return BatchReceipt{}, ErrEmptyBatch
	}

	now := time.Now()
	entries := make([]JournalEntry, len(b.Entries))
	copy(entries, b.Entries)

	// 1. Field validation + currency normalization for every entry,
	// before anything is recorded: one bad leg rejects the whole batch.
	// Duplicate entry IDs inside the batch are rejected here too —
	// later entries could otherwise shadow earlier ones.
	seenIDs := make(map[string]bool, len(entries))
	for i := range entries {
		e := &entries[i]
		if err := validateJournalEntry(e); err != nil {
			return BatchReceipt{}, err
		}
		if seenIDs[e.ID] {
			return BatchReceipt{}, ErrBatchDuplicateEntryID
		}
		seenIDs[e.ID] = true
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
	}

	// 2. Batch-level idempotency: a replayed key returns the whole
	// original receipt, before any entry-level work — the replay books
	// nothing new, so it must not fail on state that changed since.
	if b.IdempotencyKey != "" {
		if rec, ok := l.batchKeys[b.IdempotencyKey]; ok {
			committed := make([]JournalEntry, 0, len(rec.entryIDs))
			for _, id := range rec.entryIDs {
				committed = append(committed, l.entries[id])
			}
			return BatchReceipt{BatchID: rec.batchID, Entries: committed, Duplicate: true}, nil
		}
	}

	// 3. Per-entry idempotency resolution against the shared Post key
	// namespace: a key already posted resolves to the original entry,
	// which is returned in the receipt without being booked again.
	replayed := make([]bool, len(entries))
	newKeys := make(map[string]bool)
	for i := range entries {
		e := &entries[i]
		if e.IdempotencyKey == "" {
			continue
		}
		if orig, ok := l.byKey[e.IdempotencyKey]; ok {
			entries[i] = orig
			replayed[i] = true
			continue
		}
		if newKeys[e.IdempotencyKey] {
			return BatchReceipt{}, ErrBatchDuplicateIdempotencyKey
		}
		newKeys[e.IdempotencyKey] = true
	}

	// 4. Entry-ID conflicts: a new entry must not reuse a journaled ID.
	// (Replays carry the original entry's ID, which is journaled by
	// definition, so they skip this check.)
	for i := range entries {
		if replayed[i] {
			continue
		}
		if _, exists := l.entries[entries[i].ID]; exists {
			return BatchReceipt{}, ErrBatchEntryIDConflict
		}
	}

	// 5. Frozen checks on every new entry's legs. Replays book nothing
	// new, so they are exempt — mirroring Post's replay-first order.
	for i := range entries {
		if replayed[i] {
			continue
		}
		e := &entries[i]
		if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
			return BatchReceipt{}, ErrAccountFrozen
		}
	}

	// 5b. Period gate: a batch entry whose final timestamp lands in a
	// closed accounting period rejects the whole batch (see period.go).
	// Replays are exempt — they book nothing new, so they must not fail
	// on a period that closed after the original posting. CreatedAt was
	// finalized in step 1.
	for i := range entries {
		if replayed[i] {
			continue
		}
		if err := l.periodRejectedLocked(entries[i].CreatedAt); err != nil {
			return BatchReceipt{}, err
		}
	}

	// 6. Overdraft simulation in batch order: each new entry's credit
	// leg is checked against the account's balance as the batch's
	// earlier entries left it. A protected payer cannot dodge the guard
	// by splitting one overdrawing outflow into small legs.
	sim := newBatchBalanceSim()
	for i := range entries {
		if replayed[i] {
			continue
		}
		e := &entries[i]
		if l.noOverdraft[e.CreditAccount] {
			k := accountCurrency{account: e.CreditAccount, currency: e.Currency}
			if sim.balance(l, k) < e.AmountCents {
				return BatchReceipt{}, ErrAccountOverdraft
			}
		}
		sim.apply(e)
	}

	// 7. Daily-limit simulation: same sequential semantics — earlier
	// entries in the batch consume the UTC day's budget for later ones.
	// The formula mirrors dailyLimitRejectedLocked exactly, with the
	// batch's own planned outflow added to the ledger's bucket.
	batchOutflow := make(map[dailyLimitKey]int64)
	for i := range entries {
		if replayed[i] {
			continue
		}
		e := &entries[i]
		limit, ok := l.dailyLimits[dailyLimitKey{account: e.CreditAccount, currency: e.Currency}]
		if ok {
			if e.AmountCents > limit {
				return BatchReceipt{}, ErrDailyLimitExceeded
			}
			bucket := dailyLimitKey{account: e.CreditAccount, currency: e.Currency, day: utcDay(e.CreatedAt)}
			total := satAddInt64(l.dailyOutflow[bucket], batchOutflow[bucket])
			if total > limit-e.AmountCents {
				return BatchReceipt{}, ErrDailyLimitExceeded
			}
			batchOutflow[bucket] = satAddInt64(batchOutflow[bucket], e.AmountCents)
		}
	}

	// 8. Atomic commit: every new entry's journal row, idempotency index
	// entries, per-account index rows, balances, totals, chain links, and
	// version bumps land together, under the one write lock, after all
	// checks passed. Any failure above returned before the first
	// mutation, so there is nothing to roll back.
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	committedIDs := make([]string, 0, len(entries))
	accounts := make([]AccountID, 0, 2*len(entries))
	seenAccounts := make(map[AccountID]bool)
	for i := range entries {
		e := &entries[i]
		committedIDs = append(committedIDs, e.ID)
		if replayed[i] {
			continue
		}
		e.BatchID = b.ID
		l.commitEntryLocked(*e)
		l.addDailyOutflowLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt)
		for _, a := range []AccountID{e.DebitAccount, e.CreditAccount} {
			if !seenAccounts[a] {
				seenAccounts[a] = true
				accounts = append(accounts, a)
			}
		}
	}
	if b.IdempotencyKey != "" {
		l.batchKeys[b.IdempotencyKey] = batchRecord{
			batchID:   b.ID,
			entryIDs:  committedIDs,
			createdAt: now,
		}
	}
	l.emitAudit(AuditEvent{
		Op:            "post_batch",
		Actor:         "PostBatch",
		TraceID:       b.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      committedIDs,
		Accounts:      accounts,
		Details: map[string]any{
			"entries":     len(committedIDs),
			"new_entries": len(committedIDs) - countTrue(replayed),
		},
	})

	return BatchReceipt{BatchID: b.ID, Entries: entries, Duplicate: false}, nil
}

// countTrue counts the set flags in a replay mask.
func countTrue(mask []bool) int {
	n := 0
	for _, v := range mask {
		if v {
			n++
		}
	}
	return n
}

// batchBalanceSim tracks the running per-(account, currency) balance
// deltas of a batch under validation, so sequential risk checks see each
// entry against the books as the batch's earlier entries left them. All
// arithmetic saturates: adversarial amounts can never wrap the simulation
// into a false pass.
type batchBalanceSim struct {
	inflow  map[accountCurrency]int64 // simulated debits per (account, currency)
	outflow map[accountCurrency]int64 // simulated credits per (account, currency)
}

func newBatchBalanceSim() *batchBalanceSim {
	return &batchBalanceSim{
		inflow:  make(map[accountCurrency]int64),
		outflow: make(map[accountCurrency]int64),
	}
}

// satAddInt64 adds with saturation: on overflow it returns the extreme in
// the direction of the overflow instead of wrapping.
func satAddInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// balance reports the account's balance after the simulated entries:
// ledger balance + simulated debits − simulated credits, saturated.
// Callers must hold l.mu.
func (s *batchBalanceSim) balance(l *Ledger, k accountCurrency) int64 {
	return satAddInt64(satAddInt64(l.balances[k], s.inflow[k]), -s.outflow[k])
}

// apply records one entry's balance effects in the simulation: the debit
// leg grows the debit account, the credit leg shrinks the credit account.
func (s *batchBalanceSim) apply(e *JournalEntry) {
	dk := accountCurrency{account: e.DebitAccount, currency: e.Currency}
	ck := accountCurrency{account: e.CreditAccount, currency: e.Currency}
	s.inflow[dk] = satAddInt64(s.inflow[dk], e.AmountCents)
	s.outflow[ck] = satAddInt64(s.outflow[ck], e.AmountCents)
}

// pruneBatchKeysLocked evicts batch idempotency keys older than the TTL,
// mirroring the entry-level key expiry: after the TTL, reposting a batch
// key books a brand-new batch. The journaled batch entries themselves are
// untouched — only the replay-detection index shrinks. A non-positive TTL
// disables pruning. Callers must hold l.mu (the write lock; the batch
// mutates the index).
func (l *Ledger) pruneBatchKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.batchKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.batchKeys, key)
			removed++
		}
	}
	return removed
}
