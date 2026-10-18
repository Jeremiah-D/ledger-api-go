package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Cost allocation rules (opt-in).
//
// A cost allocation rule splits one expense amount across cost centers in
// fixed proportions — the classic payment-fintech month-end job: a shared
// platform fee booked against a clearing account is allocated to each
// merchant's cost center by basis points. AllocateCost posts the split as
// N double-entry legs in one atomic commit: debit each cost-center target,
// credit the rule's source (expense) account.
//
// The legs walk the full Post gate order — field validation,
// sanctions/denylist screening, chart direction, closed/frozen,
// overdraft, daily-limit, monthly-budget, and the accounting-period gate —
// evaluated per leg with intra-allocation aggregate simulation (earlier
// legs consume the source account's daily/budget/overdraft headroom for
// later ones), exactly like PostBatch. The commit is atomic across all
// legs: one failing leg rejects the whole allocation and records nothing.
//
// Two things deliberately do NOT happen, and both are by design:
//   - Journal rules (see rules.go) never fire on allocation legs. An
//     allocation is a system posting, like an accrual (see accrual.go):
//     operator-configured rules must not cascade off machine-generated
//     postings, so the legs bypass derivedRuleLegsLocked entirely.
//   - Dual-control review (see review.go) never engages. Review is a
//     PostTransfer gate; allocations post through the raw journal path,
//     so a large month-end allocation settles immediately instead of
//     parking in pending review.
//
// Idempotency is deterministic: one rule may allocate once per UTC
// calendar month, keyed "alloc/<ruleID>/<YYYY-MM>". Replaying the same
// rule and month returns the original receipt with Duplicate == true and
// books nothing new. The receipt registry is its own idempotency
// namespace, expiring with the TTL like every other key index, and
// round-trips through disaster-recovery snapshots. Allocation rules
// themselves are structural config, like journal rules: they round-trip
// through the snapshot config record together with the rule-set mutation
// counter, and an illegal rule in a snapshot fails the import.

// Validation errors for cost allocation rules.
var (
	// ErrAllocationRuleEmptyID is returned by SetAllocationRule when the
	// rule has no ID.
	ErrAllocationRuleEmptyID = errors.New("ledger: allocation rule ID must not be empty")
	// ErrAllocationRuleIDTooLong is returned by SetAllocationRule when
	// the rule ID exceeds maxAllocationRuleIDLen characters. The bound
	// keeps the generated leg IDs ("alloc/<ruleID>/<YYYY-MM>/leg/<i>")
	// short and unambiguous.
	ErrAllocationRuleIDTooLong = errors.New("ledger: allocation rule ID must be at most 64 characters")
	// ErrAllocationRuleIDSeparator is returned by SetAllocationRule when
	// the rule ID contains '/': the separator is reserved for the
	// deterministic leg IDs, so a rule ID containing it would produce
	// ambiguous journal entry IDs.
	ErrAllocationRuleIDSeparator = errors.New("ledger: allocation rule ID must not contain '/'")
	// ErrAllocationRuleNoTargets is returned by SetAllocationRule when
	// the rule defines no cost-center targets: a rule that splits
	// nothing is a caller bug, not a no-op.
	ErrAllocationRuleNoTargets = errors.New("ledger: allocation rule must define at least one target")
	// ErrAllocationRuleTargetAccounts is returned by SetAllocationRule
	// when a target names an empty account, repeats an account, or names
	// the rule's source account: the source is credited once in total,
	// so a target on the source would net against itself.
	ErrAllocationRuleTargetAccounts = errors.New("ledger: allocation rule target accounts must be non-empty, distinct, and differ from the source account")
	// ErrAllocationRuleTargetWeight is returned by SetAllocationRule when
	// a target's basis-point weight is outside 1..10000: a zero weight
	// books nothing and a weight above 100% of the amount is a
	// configuration bug, not a clampable share.
	ErrAllocationRuleTargetWeight = errors.New("ledger: allocation rule target weight must be between 1 and 10000 basis points")
	// ErrAllocationRuleWeightsSum is returned by SetAllocationRule when
	// the targets' weights do not sum to exactly 10000 basis points: a
	// split that does not cover the whole amount is a caller bug.
	ErrAllocationRuleWeightsSum = errors.New("ledger: allocation rule target weights must sum to exactly 10000 basis points")
	// ErrAllocationRuleDescriptionTooLong is returned by SetAllocationRule
	// when the rule description exceeds maxAllocationRuleDescriptionLen
	// characters.
	ErrAllocationRuleDescriptionTooLong = errors.New("ledger: allocation rule description must be at most 255 characters")
	// ErrAllocationRuleSourceEmpty is returned by SetAllocationRule when
	// the rule names no source account.
	ErrAllocationRuleSourceEmpty = errors.New("ledger: allocation rule source account must not be empty")
	// ErrAllocationNotFound is returned by AllocateCost when no rule with
	// the given ID is registered.
	ErrAllocationNotFound = errors.New("ledger: allocation rule not found")
	// ErrAllocationNonPositiveAmount is returned by AllocateCost when the
	// amount is not positive.
	ErrAllocationNonPositiveAmount = errors.New("ledger: allocation amount must be greater than zero")
	// ErrAllocationDustLeg is returned by AllocateCost when the amount is
	// so small that a target's proportional share computes to zero cents.
	// Unlike journal rules (which silently drop dust legs), an allocation
	// must cover the whole amount — a dropped leg would break the
	// debit == credit invariant — so the whole allocation is rejected and
	// the caller must use a larger amount or fewer targets.
	ErrAllocationDustLeg = errors.New("ledger: allocation amount too small: a target share computes to zero cents")
	// ErrAllocationLegConflict is returned by AllocateCost when a
	// deterministic leg ID collides with an existing journal entry. The
	// whole allocation is rejected and records nothing — silently
	// overwriting a journaled entry is never an option.
	ErrAllocationLegConflict = errors.New("ledger: allocation leg ID collides with an existing journal entry")
)

// maxAllocationRuleIDLen bounds rule IDs so generated leg IDs stay short
// and unambiguous.
const maxAllocationRuleIDLen = 64

// maxAllocationRuleDescriptionLen bounds rule descriptions like entry memos.
const maxAllocationRuleDescriptionLen = 255

// allocationLegIDSeparator joins the deterministic idempotency key with
// the leg index: "alloc/<ruleID>/<YYYY-MM>/leg/<i>".
const allocationLegIDSeparator = "/leg/"

// AllocationTarget is one cost center in an allocation rule: PctBps basis
// points (1..10000) of the allocated amount are debited to Account. The
// targets' weights must sum to exactly 10000.
type AllocationTarget struct {
	Account AccountID `json:"account"`
	PctBps  int64     `json:"pct_bps"`
}

// AllocationRule splits an expense amount across cost centers. Source is
// the expense account credited in full (every leg credits it); each
// target is debited its proportional share.
type AllocationRule struct {
	ID          string             `json:"id"`
	Source      AccountID          `json:"source_account"`
	Targets     []AllocationTarget `json:"targets"`
	Description string             `json:"description,omitempty"`
}

// AllocationReceipt reports what AllocateCost committed: the legs in
// commit (target) order, the deterministic idempotency key's month, and
// whether the call was an idempotent replay.
type AllocationReceipt struct {
	RuleID     string         `json:"rule_id"`
	Month      string         `json:"month"`
	EntryIDs   []string       `json:"entry_ids"`
	Entries    []JournalEntry `json:"entries"`
	TotalCents int64          `json:"total_cents"`
	Currency   string         `json:"currency"`
	Duplicate  bool           `json:"duplicate"`
}

// allocRecord is the idempotency-registry row for one committed
// allocation: the deterministic key "alloc/<ruleID>/<YYYY-MM>" maps to the
// legs' journal entry IDs, so a replay rebuilds the exact original
// receipt. createdAt drives the TTL expiry, like the other key
// namespaces.
type allocRecord struct {
	ruleID    string
	month     string
	entryIDs  []string
	createdAt time.Time
}

// allocationIdempotencyKey is the deterministic idempotency key for one
// rule and UTC calendar month: at most one allocation per rule per month.
func allocationIdempotencyKey(ruleID, month string) string {
	return "alloc/" + ruleID + "/" + month
}

// allocationMonth formats the UTC calendar month of t as YYYY-MM, the
// month dimension of the deterministic idempotency key.
func allocationMonth(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// ValidateAllocationRule reports whether r could be registered: a
// non-empty short ID without the leg-ID separator, a non-empty source,
// at least one target, distinct non-empty target accounts that differ
// from the source, every weight in 1..10000, and weights summing to
// exactly 10000 basis points.
func ValidateAllocationRule(r AllocationRule) error {
	if r.ID == "" {
		return ErrAllocationRuleEmptyID
	}
	if len(r.ID) > maxAllocationRuleIDLen {
		return ErrAllocationRuleIDTooLong
	}
	for i := 0; i < len(r.ID); i++ {
		if r.ID[i] == '/' {
			return ErrAllocationRuleIDSeparator
		}
	}
	if r.Source == "" {
		return ErrAllocationRuleSourceEmpty
	}
	if len(r.Targets) == 0 {
		return ErrAllocationRuleNoTargets
	}
	if len(r.Description) > maxAllocationRuleDescriptionLen {
		return ErrAllocationRuleDescriptionTooLong
	}
	seen := make(map[AccountID]bool, len(r.Targets))
	var sum int64
	for _, t := range r.Targets {
		if t.Account == "" || t.Account == r.Source || seen[t.Account] {
			return ErrAllocationRuleTargetAccounts
		}
		seen[t.Account] = true
		if t.PctBps < 1 || t.PctBps > 10000 {
			return ErrAllocationRuleTargetWeight
		}
		sum += t.PctBps
	}
	if sum != 10000 {
		return ErrAllocationRuleWeightsSum
	}
	return nil
}

// SetAllocationRule registers (or replaces) a cost allocation rule. Rules
// are structural config: registration does not bump the journal version,
// and the rule-set mutation counter (see AllocationRuleVersion) moves so
// snapshots carry the generation. Illegal rules are rejected and store
// nothing.
func (l *Ledger) SetAllocationRule(r AllocationRule) error {
	if err := ValidateAllocationRule(r); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allocRules[r.ID] = copyAllocationRule(r)
	l.allocRuleVersion++
	l.emitAudit(AuditEvent{
		Op:           "allocation_rule_set",
		Actor:        "SetAllocationRule",
		TraceID:      r.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"rule_id": r.ID,
			"targets": len(r.Targets),
		},
	})
	return nil
}

// DeleteAllocationRule removes a cost allocation rule. It reports whether
// a rule was present. Deletion is structural config like registration: no
// version bump, but the mutation counter moves.
func (l *Ledger) DeleteAllocationRule(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.allocRules[id]; !ok {
		return false
	}
	delete(l.allocRules, id)
	l.allocRuleVersion++
	l.emitAudit(AuditEvent{
		Op:           "allocation_rule_delete",
		Actor:        "DeleteAllocationRule",
		TraceID:      id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"rule_id": id},
	})
	return true
}

// GetAllocationRule returns the registered rule with the given ID. The
// returned rule is a copy: mutating it does not affect the registry.
func (l *Ledger) GetAllocationRule(id string) (AllocationRule, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.allocRules[id]
	if !ok {
		return AllocationRule{}, false
	}
	return copyAllocationRule(r), true
}

// copyAllocationRule deep-copies a rule's target slice so registry reads
// never alias stored state.
func copyAllocationRule(r AllocationRule) AllocationRule {
	targets := make([]AllocationTarget, len(r.Targets))
	copy(targets, r.Targets)
	r.Targets = targets
	return r
}

// AllocationRules returns every registered allocation rule, sorted by ID.
func (l *Ledger) AllocationRules() []AllocationRule {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.allocationRulesLocked()
}

func (l *Ledger) allocationRulesLocked() []AllocationRule {
	out := make([]AllocationRule, 0, len(l.allocRules))
	for _, r := range l.allocRules {
		out = append(out, copyAllocationRule(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AllocationRuleVersion reports the allocation rule-set mutation counter:
// every set or delete moves it, so snapshot restores carry the same
// generation as the source ledger.
func (l *Ledger) AllocationRuleVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.allocRuleVersion
}

// AllocationsCommitted reports how many cost allocations the ledger has
// committed (replays excluded).
func (l *Ledger) AllocationsCommitted() uint64 {
	return l.allocationsCommitted.Load()
}

// AllocateCost splits amountCents across the rule's cost-center targets
// for the current UTC calendar month, posting one double-entry leg per
// target atomically. See AllocateCostAt for the timestamped body.
func (l *Ledger) AllocateCost(ruleID string, amountCents int64, currency string) (AllocationReceipt, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allocateCostLocked(ruleID, amountCents, currency, time.Now())
}

// AllocateCostAt is AllocateCost's body with an explicit timestamp: the
// UTC calendar month of now is the allocation's month dimension, and now
// is the legs' CreatedAt. Callers must hold the write lock.
//
// The legs are built with deterministic IDs
// "alloc/<ruleID>/<YYYY-MM>/leg/<i>" and a memo of "alloc:<ruleID>" so
// allocations are discoverable through the ordinary journal listing and
// the ?memo= filter. Each leg debits its cost-center target and credits
// the rule's source account; the shares are floor(amount * bps / 10000)
// computed overflow-safe like the transfer fee policy, and the dust
// remainder lands on the first target so the legs always sum to the full
// amount.
//
// Gate order is the full Post order — validation, denylist screening,
// entry-ID conflict, chart direction, closed/frozen, overdraft (with
// intra-allocation aggregate simulation, so a protected source cannot be
// overdrawn by splitting the outflow across legs), daily-limit (subtree
// simulation), monthly-budget (aggregate simulation), and the period gate
// on the shared timestamp. Journal rules never fire and dual-control
// review never engages (see the package comment): the commit below is
// direct, atomic across all legs, and readers never observe a partial
// allocation.
func (l *Ledger) allocateCostLocked(ruleID string, amountCents int64, currency string, now time.Time) (AllocationReceipt, bool, error) {
	rule, ok := l.allocRules[ruleID]
	if !ok {
		return AllocationReceipt{}, false, ErrAllocationNotFound
	}
	if amountCents <= 0 {
		return AllocationReceipt{}, false, ErrAllocationNonPositiveAmount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return AllocationReceipt{}, false, err
	}

	month := allocationMonth(now)
	key := allocationIdempotencyKey(ruleID, month)

	// Receipt-level idempotency: a replayed month returns the original
	// receipt before any gate work — the replay books nothing new, so it
	// must not fail on state that changed since.
	if rec, ok := l.allocKeys[key]; ok {
		entries := make([]JournalEntry, 0, len(rec.entryIDs))
		for _, id := range rec.entryIDs {
			entries = append(entries, l.entries[id])
		}
		// The receipt reports the journaled legs' currency, not the
		// replay call's parameter: the entries are the source of truth.
		replayCurrency := cur
		if len(entries) > 0 {
			replayCurrency = entries[0].Currency
		}
		return AllocationReceipt{
			RuleID:     rec.ruleID,
			Month:      rec.month,
			EntryIDs:   append([]string(nil), rec.entryIDs...),
			Entries:    entries,
			TotalCents: allocationTotal(entries),
			Currency:   replayCurrency,
			Duplicate:  true,
		}, true, nil
	}

	// Build the legs: proportional shares with the dust remainder on the
	// first target so the legs always sum to the full amount.
	legs := make([]JournalEntry, len(rule.Targets))
	var assigned int64
	for i, t := range rule.Targets {
		share := policyFeeCents(amountCents, t.PctBps)
		if share <= 0 {
			return AllocationReceipt{}, false, fmt.Errorf("%w: target %q", ErrAllocationDustLeg, t.Account)
		}
		assigned += share
		legs[i] = JournalEntry{
			ID:            key + allocationLegIDSeparator + strconv.Itoa(i),
			DebitAccount:  t.Account,
			CreditAccount: rule.Source,
			AmountCents:   share,
			Currency:      cur,
			Memo:          "alloc:" + ruleID,
			CreatedAt:     now,
		}
	}
	legs[0].AmountCents += amountCents - assigned

	// 1. Field validation + currency normalization for every leg,
	// before anything is recorded: one bad leg rejects the whole
	// allocation.
	for i := range legs {
		if err := validateJournalEntry(&legs[i]); err != nil {
			return AllocationReceipt{}, false, err
		}
	}

	// 2. Sanctions/denylist screening on every leg's accounts, before
	// anything books — a hit rejects the whole allocation and leaves no
	// idempotency trace, audited as "denylist_rejected".
	for i := range legs {
		e := &legs[i]
		if acct, entry, denied := l.denylistRejectedLocked(e.DebitAccount, e.CreditAccount); denied {
			return AllocationReceipt{}, false, l.deniedRejectionLocked("AllocateCost", key, acct, entry, []AccountID{e.DebitAccount, e.CreditAccount})
		}
	}

	// 3. Leg-ID conflicts: a deterministic leg ID must not collide with
	// a journaled entry — silently overwriting a journaled row is never
	// an option.
	for i := range legs {
		if _, exists := l.entries[legs[i].ID]; exists {
			return AllocationReceipt{}, false, ErrAllocationLegConflict
		}
	}

	// 4. Chart direction + closed/frozen checks on every leg, in Post
	// order. Expired timed freezes release lazily before the frozen
	// gates, like in Post.
	l.releaseExpiredFreezesLocked(now, "write_path")
	for i := range legs {
		e := &legs[i]
		if err := l.chartDirectionRejectedLocked(*e); err != nil {
			return AllocationReceipt{}, false, err
		}
		if l.closedLocked(e.DebitAccount) || l.closedLocked(e.CreditAccount) {
			return AllocationReceipt{}, false, ErrAccountClosed
		}
		if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
			return AllocationReceipt{}, false, ErrAccountFrozen
		}
		if l.suspendedLocked(e.DebitAccount) || l.suspendedLocked(e.CreditAccount) {
			return AllocationReceipt{}, false, ErrAccountSuspended
		}
	}

	// 5. Overdraft simulation in leg order: each leg's credit side (the
	// rule's source account) is checked against the balance as the
	// allocation's earlier legs left it. A protected source cannot dodge
	// the guard by splitting one overdrawing outflow into small legs.
	sim := newBatchBalanceSim()
	for i := range legs {
		e := &legs[i]
		if l.noOverdraft[e.CreditAccount] {
			k := accountCurrency{account: e.CreditAccount, currency: e.Currency}
			if sim.balance(l, k) < e.AmountCents {
				return AllocationReceipt{}, false, ErrAccountOverdraft
			}
		}
		sim.apply(e)
	}

	// 6. Daily-limit simulation with subtree semantics (see LG-80):
	// earlier legs consume the UTC day's budget for later ones.
	allocOutflow := make(map[dailyLimitKey]int64)
	for i := range legs {
		e := &legs[i]
		if l.subtreeLimitsViolatedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt, allocOutflow) {
			return AllocationReceipt{}, false, ErrDailyLimitExceeded
		}
		bucket := dailyLimitKey{account: e.CreditAccount, currency: e.Currency, day: utcDay(e.CreatedAt)}
		allocOutflow[bucket] = satAddInt64(allocOutflow[bucket], e.AmountCents)
	}

	// 7. Monthly-budget simulation: same sequential semantics — earlier
	// legs consume the UTC month's budget for later ones.
	allocBudgetSpend := make(map[monthlySpendKey]int64)
	for i := range legs {
		e := &legs[i]
		if l.budgetViolatedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt, allocBudgetSpend) {
			return AllocationReceipt{}, false, ErrBudgetExceeded
		}
		bucket := monthlySpendKey{account: e.CreditAccount, currency: e.Currency, month: utcMonth(e.CreatedAt)}
		allocBudgetSpend[bucket] = satAddInt64(allocBudgetSpend[bucket], e.AmountCents)
	}

	// 8. Period gate on the shared timestamp: an allocation landing in a
	// closed accounting period is rejected with ErrPeriodClosed.
	if err := l.periodRejectedLocked(now); err != nil {
		return AllocationReceipt{}, false, err
	}

	// 9. Atomic commit: every leg's journal row, per-account index rows,
	// balances, totals, chain links, and version bumps land together,
	// under the one write lock, after all checks passed. Any failure
	// above returned before the first mutation, so there is nothing to
	// roll back. Journal rules are deliberately not evaluated and the
	// dual-control review gate is deliberately not consulted (see the
	// package comment).
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version
	hookTouched := touchedAccounts(legs)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	entryIDs := make([]string, 0, len(legs))
	accounts := make([]AccountID, 0, 2*len(legs))
	seenAccounts := make(map[AccountID]bool)
	for i := range legs {
		e := &legs[i]
		l.commitEntryLocked(*e)
		l.addDailyOutflowLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt)
		l.addMonthlySpendLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt)
		entryIDs = append(entryIDs, e.ID)
		for _, a := range []AccountID{e.DebitAccount, e.CreditAccount} {
			if !seenAccounts[a] {
				seenAccounts[a] = true
				accounts = append(accounts, a)
			}
		}
	}
	l.allocKeys[key] = allocRecord{
		ruleID:    ruleID,
		month:     month,
		entryIDs:  append([]string(nil), entryIDs...),
		createdAt: now,
	}
	l.allocationsCommitted.Add(1)
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only. Advisory only.
	l.evaluateBalanceWatchesLocked(hookTouched, key, "AllocateCost")
	l.emitAudit(AuditEvent{
		Op:            "allocate_cost",
		Actor:         "AllocateCost",
		TraceID:       key,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      accounts,
		Details: map[string]any{
			"rule_id":     ruleID,
			"month":       month,
			"legs":        len(legs),
			"total_cents": amountCents,
			"currency":    cur,
		},
	})
	// Domain outbox: one entry_posted per committed leg.
	l.emitOutboxEntriesLocked(legs)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only.
	l.fireBalanceHooksLocked(hookTouched, hookOld, key)

	committed := make([]JournalEntry, len(legs))
	copy(committed, legs)
	return AllocationReceipt{
		RuleID:     ruleID,
		Month:      month,
		EntryIDs:   entryIDs,
		Entries:    committed,
		TotalCents: amountCents,
		Currency:   cur,
		Duplicate:  false,
	}, false, nil
}

// allocationTotal sums committed legs' amounts for a replayed receipt.
func allocationTotal(entries []JournalEntry) int64 {
	var total int64
	for _, e := range entries {
		total += e.AmountCents
	}
	return total
}

// pruneAllocKeysLocked evicts allocation receipt keys older than the TTL.
// A non-positive TTL disables pruning. Callers must hold l.mu.
func (l *Ledger) pruneAllocKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.allocKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.allocKeys, key)
			removed++
		}
	}
	return removed
}
