package ledger

import (
	"errors"
	"sort"
	"strings"
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
// A source's (account, currency) may carry an opt-in sweep reserve floor
// (see SetSweepReserve): then only the balance in excess of the floor
// moves, and the floor stays behind as the account's operating float. A
// floor that meets or exceeds the balance drops the leg entirely — the
// sweep still succeeds, with the receipt's leg list simply shorter — so
// reserves never sweep an account into debt.
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
	// RuleLegs discloses the automatic posting-rule legs the sweep
	// booked (see ledger/posting_rules.go): one row per derived leg
	// with the rule name, the journal entry ID, the accounts, and the
	// cents booked. Nil when no rule fired. Idempotent replays rebuild
	// the same disclosure from the journaled legs' "/rule:" IDs.
	RuleLegs []PostingRuleLegInfo `json:"rule_legs,omitempty"`
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
				RuleLegs:  postingRuleLegInfosFromEntries(entries),
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

	if l.closedLocked(s.To) {
		return SweepReceipt{}, ErrAccountClosed
	}
	for _, from := range s.From {
		if l.closedLocked(from) {
			return SweepReceipt{}, ErrAccountClosed
		}
	}
	if l.frozenLocked(s.To) {
		return SweepReceipt{}, ErrAccountFrozen
	}
	if l.suspendedLocked(s.To) {
		return SweepReceipt{}, ErrAccountSuspended
	}
	for _, from := range s.From {
		if l.frozenLocked(from) {
			return SweepReceipt{}, ErrAccountFrozen
		}
		if l.suspendedLocked(from) {
			return SweepReceipt{}, ErrAccountSuspended
		}
	}

	// Sweep reserve floors (see sweep_reserve.go) shrink the planned legs
	// after the frozen checks and before the budget checks: a leg whose
	// (source, currency) carries a reserve moves only the balance in
	// excess of the floor, and a leg the floor consumes entirely
	// contributes no journal row. The reserve calculation runs here — not
	// in sweepLegsLocked — so a frozen source rejects first: a frozen
	// account never silently sweeps a reduced amount, and a zero-leg
	// sweep still succeeds with an empty receipt.
	legs = l.applySweepReservesLocked(legs)

	// The sweep's effective post time is finalized before the budget and
	// period checks: spend is booked against the sweep's own UTC calendar
	// month, and the period gate is keyed on the same instant.
	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	// Monthly spend budgets guard every source leg: a sweep whose source
	// leg would take the account's cumulative spend for the UTC calendar
	// month above its configured budget is rejected with
	// ErrBudgetExceeded. Legs are checked in order with the sweep's
	// planned spend carried forward, so earlier legs consume the
	// month's budget for later ones. The check runs after the frozen
	// checks and before the period gate — like the other risk controls,
	// it only evaluates sweeps that book something new (idempotent
	// replays returned above).
	sweepBudgetSpend := make(map[monthlySpendKey]int64)
	for _, leg := range legs {
		if l.budgetViolatedLocked(leg.From, leg.Currency, leg.AmountCents, createdAt, sweepBudgetSpend) {
			return SweepReceipt{}, ErrBudgetExceeded
		}
		bucket := monthlySpendKey{account: leg.From, currency: leg.Currency, month: utcMonth(createdAt)}
		sweepBudgetSpend[bucket] = satAddInt64(sweepBudgetSpend[bucket], leg.AmountCents)
	}

	// The period gate is keyed on the sweep's effective post time (see
	// period.go): a backdated sweep landing in a closed accounting period
	// is rejected with ErrPeriodClosed. It runs after the idempotency
	// replay check above, so replaying a key swept before the period
	// closed returns the original receipt.
	if err := l.periodRejectedLocked(createdAt); err != nil {
		return SweepReceipt{}, err
	}

	// Automatic posting rules (see posting_rules.go): each sweep leg
	// that matches an installed rule appends its derived legs to the
	// commit — one rule evaluation per (source, currency) leg, so every
	// sub-merchant settlement books its own legs. Evaluated after every
	// gate and before the first mutation, so a failing rule leg rejects
	// the whole sweep with nothing recorded.
	var ruleLegs []JournalEntry
	var ruleLegInfos []PostingRuleLegInfo
	for _, leg := range legs {
		rl, ri, err := l.resolvePostingRuleLegsLocked(postingRuleContext{
			kind:        PostingEventSweep,
			payer:       leg.From,
			payee:       s.To,
			amountCents: leg.AmountCents,
			currency:    leg.Currency,
			triggerID:   leg.EntryID,
		}, createdAt)
		if err != nil {
			return SweepReceipt{}, err
		}
		ruleLegs = append(ruleLegs, rl...)
		ruleLegInfos = append(ruleLegInfos, ri...)
	}

	// Atomic commit: every leg's journal row, per-account index entries,
	// balances, totals, chain links, and version bumps land together,
	// under the one write lock, after all checks passed. Any failure
	// above returned before the first mutation, so there is nothing to
	// roll back.
	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version
	entries := make([]JournalEntry, 0, len(legs)+len(ruleLegs))
	for _, leg := range legs {
		entries = append(entries, JournalEntry{
			ID:            leg.EntryID,
			DebitAccount:  s.To,
			CreditAccount: leg.From,
			AmountCents:   leg.AmountCents,
			Currency:      leg.Currency,
			CreatedAt:     createdAt,
		})
	}
	entries = append(entries, ruleLegs...)
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): the legs are fully planned above, so the
	// touched set is exact. Nil when no hook is registered.
	hookTouched := touchedAccounts(entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	for _, leg := range legs {
		l.addMonthlySpendLocked(leg.From, leg.Currency, leg.AmountCents, createdAt)
	}
	l.countPostingRuleLegsLocked(ruleLegInfos)
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
	sweepEntryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		sweepEntryIDs = append(sweepEntryIDs, e.ID)
	}
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), s.ID, "PostSweep")
	l.emitAudit(AuditEvent{
		Op:            "sweep",
		Actor:         "PostSweep",
		TraceID:       s.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      sweepEntryIDs,
		Accounts:      append(append([]AccountID{}, s.From...), s.To),
		Details:       sweepDetails(len(legs), ruleLegInfos),
	})
	// Domain outbox: one entry_posted per sweep leg and rule leg.
	l.emitOutboxEntriesLocked(entries)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, s.ID)

	return SweepReceipt{
		SweepID:   s.ID,
		Legs:      legs,
		Entries:   entries,
		Duplicate: false,
		RuleLegs:  ruleLegInfos,
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

// sweepDetails renders the sweep's structured audit disclosure: the leg
// count plus the automatic posting-rule legs (see posting_rules.go).
func sweepDetails(legCount int, ruleLegs []PostingRuleLegInfo) map[string]any {
	d := map[string]any{"legs": legCount}
	if len(ruleLegs) > 0 {
		d["posting_rule_legs"] = postingRuleDetails(ruleLegs)
	}
	return d
}

// sweepLegsOf rebuilds the leg descriptors of a replayed sweep from its
// committed entries. The entry ID convention "<sweep ID>/<source>/<currency>"
// carries everything the leg needs; the source account is the credit leg.
// Rule legs (see posting_rules.go, "<...>/rule:<name>") are skipped: they
// are not sweep legs and rebuild separately into the receipt's RuleLegs.
func sweepLegsOf(entries []JournalEntry) []SweepLeg {
	legs := make([]SweepLeg, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(e.ID, "/rule:") {
			continue
		}
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
