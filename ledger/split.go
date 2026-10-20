package ledger

import (
	"errors"
	"sort"
	"time"
)

// Account splits: carving a sub-account out of an existing one (LG-146,
// fintech).
//
// A split is the inverse of an account merge (see merge.go): where a
// merge drains a whole account into another and decommissions the
// source, a split moves specified amounts out of a source account into a
// target account — a merchant spinning a product line into its own
// sub-account, a business unit carved out of a parent ledger — while the
// source keeps operating on the remainder. The source is NOT frozen or
// closed: splitting is a reorganization, not a decommission.
//
// Leg semantics: one journal entry per leg under "<split ID>/<currency>"
// (the split-ID prefix is what marks journal entries as split legs).
// Every leg books debit target / credit source — the target grows, the
// source shrinks by exactly the leg amount, and the source retains the
// remainder. Only positive balances are splittable: a leg names either a
// fixed AmountCents or a RatioBps share (1..10000 basis points, floor)
// of the source's balance in that currency at split time, and the
// effective amount must be positive and must not exceed the balance. A
// dust ratio that floors to zero, or a fixed amount above the balance,
// is rejected — never silently clamped.
//
// The split commits atomically: field validation, the idempotency replay
// check, the ID-conflict check, the closed/frozen/suspended checks on
// both accounts, the overdraft check on the target, and the accounting-
// period gate all run before the first journal row lands, so a split
// that fails on any check records nothing: no journal rows, no chain
// links, no version bump.
//
// Risk-control interplay, in check order:
//   - Frozen/closed/suspended: both the source and the target must be
//     in service. The checks run after the idempotency replay check, so
//     retrying a split whose key was posted before the freeze still
//     returns the original receipt instead of failing.
//   - Overdraft: split legs move positive money into the target, which
//     can never take it below zero — the gate is a documented no-op
//     kept so the risk-control sequence stays explicit (see
//     SplitAccount).
//   - Period: a backdated split landing in a closed accounting period
//     is rejected with ErrPeriodClosed (see period.go), after the
//     replay check like every other writer.
//
// Holds (see hold.go) are advisory reservations, not journaled money: a
// split moves journal balances, and a split can take money the
// available-funds view considers locked — splitting is an operator
// action, not a customer payment, so the operator owns the reservation
// accounting. Daily outflow limits (see daily_limit.go) do not apply,
// for the same reason: a split is an administrative reorganization, not
// a payment outflow.
//
// Validation errors returned by SplitAccount.
var (
	// ErrEmptySplitID is returned when the split carries no ID.
	ErrEmptySplitID = errors.New("ledger: split ID must not be empty")
	// ErrEmptySplitSource is returned when the split names no source
	// account.
	ErrEmptySplitSource = errors.New("ledger: split source account must not be empty")
	// ErrEmptySplitTarget is returned when the split names no target
	// account.
	ErrEmptySplitTarget = errors.New("ledger: split target account must not be empty")
	// ErrSplitSameAccount is returned when source and target are the
	// same account: a split carves money out of one account into
	// another.
	ErrSplitSameAccount = errors.New("ledger: split source and target must differ")
	// ErrSplitNoLegs is returned when the split carries no legs: a
	// split with nothing to move is a caller bug, not a no-op.
	ErrSplitNoLegs = errors.New("ledger: split must carry at least one leg")
	// ErrSplitLegAmountAndRatio is returned when a leg carries both a
	// fixed amount and a ratio, or neither: exactly one sizing mode per
	// leg.
	ErrSplitLegAmountAndRatio = errors.New("ledger: split leg must carry exactly one of amount_cents or ratio_bps")
	// ErrSplitNonPositiveAmount is returned when a leg's effective
	// amount is not positive: a zero fixed amount, or a ratio that
	// floors to zero on a dust balance.
	ErrSplitNonPositiveAmount = errors.New("ledger: split leg amount must be positive")
	// ErrSplitInvalidRatio is returned when a leg's ratio is outside
	// 1..10000 basis points.
	ErrSplitInvalidRatio = errors.New("ledger: split leg ratio must be between 1 and 10000 basis points")
	// ErrSplitDuplicateCurrency is returned when two legs name the same
	// currency: one leg per currency, because legs book under
	// "<split ID>/<currency>".
	ErrSplitDuplicateCurrency = errors.New("ledger: split legs must have distinct currencies")
	// ErrSplitInsufficientBalance is returned when a leg's effective
	// amount exceeds the source's balance in its currency, or the
	// source's balance is not positive: only positive balances are
	// splittable.
	ErrSplitInsufficientBalance = errors.New("ledger: split leg exceeds the source balance in its currency")
	// ErrSplitIDConflict is returned when the split ID — or one of the
	// leg entry IDs derived from it — is already used as a journal entry
	// ID. Split legs book under "<split ID>/<currency>", so the split ID
	// lives in the same ID namespace as journal entries.
	ErrSplitIDConflict = errors.New("ledger: split ID already used as a journal entry ID")
)

// SplitLeg is one currency movement inside a split: either a fixed
// AmountCents or a RatioBps share of the source's balance in Currency
// at split time. Exactly one of the two must be set; Currency must be a
// 3-letter uppercase ISO 4217 code (validated, not case-folded — the
// same strict convention every writer uses).
type SplitLeg struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	RatioBps    int64  `json:"ratio_bps,omitempty"`
}

// Split describes one atomic account split: the legs' amounts move from
// From to To, one journal entry per leg, and the source retains the
// remainder and keeps operating.
type Split struct {
	ID string `json:"split_id"`
	// From is the source account: each leg's amount leaves it; the
	// remainder stays and the account is not frozen or closed.
	From AccountID `json:"from_account"`
	// To is the target account: receives every leg.
	To             AccountID `json:"to_account"`
	Legs           []SplitLeg `json:"legs"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// SplitCommittedLeg is one executed movement inside a split: the
// effective amount that moved from From to To in Currency.
type SplitCommittedLeg struct {
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	// EntryID is the journal entry that booked this leg:
	// "<split ID>/<currency>". The split-ID prefix is what marks
	// journal entries as split legs.
	EntryID string `json:"entry_id"`
}

// SplitReceipt reports what SplitAccount committed.
//
// Legs and Entries are in commit order (legs sorted by currency), so the
// receipt is deterministic. Duplicate replays return the originally
// posted receipt with Duplicate == true and book nothing new.
type SplitReceipt struct {
	SplitID   string              `json:"split_id"`
	From      AccountID           `json:"from_account"`
	To        AccountID           `json:"to_account"`
	Legs      []SplitCommittedLeg `json:"legs"`
	Entries   []JournalEntry      `json:"entries"`
	Duplicate bool                `json:"duplicate"`
}

// splitRecord is the authoritative registry entry for one committed
// split, kept in Ledger.splits keyed by split ID. It feeds idempotent
// replays (via Ledger.splitKeys, which maps idempotency keys to split
// IDs), the Reconcile split history, and snapshot export. Split keys
// live in their own namespace, independent of the entry/transfer/hold/
// sweep/merge key namespaces, and expire with the TTL like every other
// key index (see pruneSplitKeysLocked).
type splitRecord struct {
	splitID   string
	from      AccountID
	to        AccountID
	legs      []SplitCommittedLeg
	entryIDs  []string
	createdAt time.Time
}

// SplitAccount atomically splits the source account into the target
// account, moving each leg's amount out of the source. The source
// retains the remainder and keeps operating — splitting never freezes
// or closes it.
//
// The check order is validation, then the idempotency replay check (a
// replayed key returns the original receipt and books nothing new),
// then the split-ID conflict check, then the closed/frozen/suspended
// checks on both accounts, then the overdraft check on the target (a
// documented no-op for positive legs — see the package comment), then
// the accounting-period gate. Rejected splits record nothing: no
// journal rows, no chain links, no version bump. A zero CreatedAt is
// filled with the current time.
func (l *Ledger) SplitAccount(s Split) (SplitReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if s.ID == "" {
		return SplitReceipt{}, ErrEmptySplitID
	}
	if s.From == "" {
		return SplitReceipt{}, ErrEmptySplitSource
	}
	if s.To == "" {
		return SplitReceipt{}, ErrEmptySplitTarget
	}
	if s.From == s.To {
		return SplitReceipt{}, ErrSplitSameAccount
	}
	if len(s.Legs) == 0 {
		return SplitReceipt{}, ErrSplitNoLegs
	}
	for i := range s.Legs {
		leg := &s.Legs[i]
		currency, err := normalizeCurrency(leg.Currency)
		if err != nil {
			return SplitReceipt{}, err
		}
		leg.Currency = currency
		if (leg.AmountCents == 0) == (leg.RatioBps == 0) {
			return SplitReceipt{}, ErrSplitLegAmountAndRatio
		}
		if leg.AmountCents < 0 {
			return SplitReceipt{}, ErrSplitNonPositiveAmount
		}
		if leg.RatioBps != 0 && (leg.RatioBps < 1 || leg.RatioBps > 10000) {
			return SplitReceipt{}, ErrSplitInvalidRatio
		}
	}

	// The leg plan is computed before any check that depends on ledger
	// state, but after field validation: balances are read under the
	// write lock, so the plan describes exactly what the commit will
	// book — no concurrent Post can slip in between.
	legs, err := l.splitLegsLocked(s)
	if err != nil {
		return SplitReceipt{}, err
	}

	if s.IdempotencyKey != "" {
		if splitID, ok := l.splitKeys[s.IdempotencyKey]; ok {
			rec := l.splits[splitID]
			entries := make([]JournalEntry, 0, len(rec.entryIDs))
			for _, id := range rec.entryIDs {
				entries = append(entries, l.entries[id])
			}
			return SplitReceipt{
				SplitID:   rec.splitID,
				From:      rec.from,
				To:        rec.to,
				Legs:      rec.legs,
				Entries:   entries,
				Duplicate: true,
			}, nil
		}
	}

	if _, exists := l.entries[s.ID]; exists {
		return SplitReceipt{}, ErrSplitIDConflict
	}
	for _, leg := range legs {
		if _, exists := l.entries[leg.EntryID]; exists {
			return SplitReceipt{}, ErrSplitIDConflict
		}
	}

	if l.closedLocked(s.From) || l.closedLocked(s.To) {
		return SplitReceipt{}, ErrAccountClosed
	}
	if l.frozenLocked(s.From) || l.frozenLocked(s.To) {
		return SplitReceipt{}, ErrAccountFrozen
	}
	if l.suspendedLocked(s.From) || l.suspendedLocked(s.To) {
		return SplitReceipt{}, ErrAccountSuspended
	}

	// Overdraft on the target: split legs move positive money into the
	// target, which can never take it below zero — the gate is a
	// documented no-op kept so the risk-control sequence stays explicit
	// (see the package comment).

	// The period gate is keyed on the split's effective post time (see
	// period.go): a backdated split landing in a closed accounting
	// period is rejected with ErrPeriodClosed. It runs after the
	// idempotency replay check above, so replaying a key split before
	// the period closed returns the original receipt.
	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if err := l.periodRejectedLocked(createdAt); err != nil {
		return SplitReceipt{}, err
	}

	// Atomic commit: every leg's journal row, per-account index entries,
	// balances, totals, chain links, and version bumps land together,
	// under the one write lock, after all checks passed. The split
	// registry entry lands in the same critical section: a reader never
	// sees split legs without the registry row. Any failure above
	// returned before the first mutation, so there is nothing to roll
	// back.
	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	versionBefore := l.version
	entries := make([]JournalEntry, 0, len(legs))
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): the legs are fully planned above, so the
	// touched set is exact. Nil when no hook is registered.
	hookTouched := make([]accountCurrency, 0, 2*len(legs))
	for _, leg := range legs {
		hookTouched = append(hookTouched,
			accountCurrency{account: s.From, currency: leg.Currency},
			accountCurrency{account: s.To, currency: leg.Currency})
	}
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, leg := range legs {
		e := JournalEntry{
			ID:             leg.EntryID,
			DebitAccount:   s.To,
			CreditAccount:  s.From,
			AmountCents:    leg.AmountCents,
			Currency:       leg.Currency,
			IdempotencyKey: leg.EntryID,
			Memo:           "split:" + s.ID,
			CreatedAt:      createdAt,
		}
		l.commitEntryLocked(e)
		entries = append(entries, e)
	}
	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.splits[s.ID] = splitRecord{
		splitID:   s.ID,
		from:      s.From,
		to:        s.To,
		legs:      legs,
		entryIDs:  entryIDs,
		createdAt: createdAt,
	}
	if s.IdempotencyKey != "" {
		l.splitKeys[s.IdempotencyKey] = s.ID
	}

	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), s.ID, "SplitAccount")
	l.emitAudit(AuditEvent{
		Op:            "split",
		Actor:         "SplitAccount",
		TraceID:       s.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{s.From, s.To},
		Details: map[string]any{
			"legs": len(legs),
		},
	})
	// Domain outbox: one entry_posted per split leg.
	l.emitOutboxEntriesLocked(entries)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, s.ID)

	return SplitReceipt{
		SplitID:   s.ID,
		From:      s.From,
		To:        s.To,
		Legs:      legs,
		Entries:   entries,
		Duplicate: false,
	}, nil
}

// splitLegsLocked plans the legs of a split: one committed leg per
// requested leg, currencies sorted for determinism, duplicate currencies
// rejected (legs book under "<split ID>/<currency>"). A fixed-amount leg
// moves exactly AmountCents; a ratio leg moves floor(balance *
// ratioBps / 10000), the same overflow-safe basis-point math the
// transfer fee policy uses (see policyFeeCents). Only positive source
// balances are splittable, and the effective amount must be positive
// and must not exceed the balance — a dust ratio that floors to zero or
// a fixed amount above the balance is rejected, never clamped.
// Callers must hold l.mu.
func (l *Ledger) splitLegsLocked(s Split) ([]SplitCommittedLeg, error) {
	seen := make(map[string]struct{}, len(s.Legs))
	legs := make([]SplitCommittedLeg, 0, len(s.Legs))
	for _, leg := range s.Legs {
		if _, dup := seen[leg.Currency]; dup {
			return nil, ErrSplitDuplicateCurrency
		}
		seen[leg.Currency] = struct{}{}
		bal := l.balances[accountCurrency{account: s.From, currency: leg.Currency}]
		if bal <= 0 {
			return nil, ErrSplitInsufficientBalance
		}
		var effective int64
		if leg.RatioBps != 0 {
			effective = policyFeeCents(bal, leg.RatioBps)
		} else {
			effective = leg.AmountCents
		}
		// Both operands are non-negative and effective <= bal <=
		// math.MaxInt64, so the comparisons below cannot overflow.
		if effective <= 0 {
			return nil, ErrSplitNonPositiveAmount
		}
		if effective > bal {
			return nil, ErrSplitInsufficientBalance
		}
		legs = append(legs, SplitCommittedLeg{
			From:        s.From,
			To:          s.To,
			Currency:    leg.Currency,
			AmountCents: effective,
			EntryID:     s.ID + "/" + leg.Currency,
		})
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].Currency < legs[j].Currency })
	return legs, nil
}

// pruneSplitKeysLocked evicts split idempotency keys older than the TTL,
// mirroring the entry-level key expiry: after the TTL, reposting a split
// key attempts a brand-new split. The split registry itself
// (Ledger.splits) is untouched: it is the audit history Reconcile
// reports, not a replay-detection index. A non-positive TTL disables
// pruning. Callers must hold l.mu (the write lock; the prune mutates the
// index).
func (l *Ledger) pruneSplitKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, splitID := range l.splitKeys {
		if rec, ok := l.splits[splitID]; ok && rec.createdAt.Before(cutoff) {
			delete(l.splitKeys, key)
			removed++
		}
	}
	return removed
}
