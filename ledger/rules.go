package ledger

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Automatic journaling rules (opt-in).
//
// A journal rule matches raw journal postings (Post) on the debit account,
// the credit account, the currency, and the amount range, and appends
// derived journal legs — the classic payment-fintech automations: tax
// withholding on a revenue posting, a platform service fee on a merchant
// settlement, a regulatory reserve on a large inflow. Rules are opt-in: a
// ledger without rules posts exactly as before, and each rule must be
// explicitly enabled before it fires.
//
// Rules fire on Post (raw journal entries) only: PostTransfer has its own
// fee policy (see transfer.go), and batch/sweep/merge postings book through
// their own paths. Rules evaluate against the principal entry only —
// derived legs never re-trigger rules, so a rule set cannot cascade.
//
// Gate order is the full Post validation order: a rule fires only after
// the principal entry clears validation, the idempotency replay check,
// frozen, overdraft, daily-limit, and period. Each derived leg then re-runs
// the account risk gates in the same order — frozen, then overdraft and
// daily-limit on the aggregate per-credit-account outflow (principal +
// derived legs, like PostTransfer's fee leg), then the period gate on the
// shared timestamp. A derived leg that fails any gate rejects the whole
// Post, and the commit is atomic across the principal and its derived legs:
// readers never observe a principal without its rule legs. Rejected posts —
// a failing principal or a failing derived leg alike — record nothing: no
// journal rows, no chain links, no version bump.
//
// A derived leg is a full journal entry: it carries its own chain link, it
// bumps the version, and its ID is deterministic —
// "<principal ID>/rule/<rule ID>/<leg index>" — with a memo of
// "rule:<rule ID>" (plus ":<label>" when the leg has one), so rule
// postings are discoverable through the ordinary journal listing and the
// ?memo= filter. A derived leg that computes to zero cents (a tiny amount
// under a low basis-point rate) books nothing, mirroring the transfer fee
// policy's dust tiers; a rule whose legs could never book anything is
// rejected at construction instead.
//
// Rule changes are structural config, like Freeze or SetDailyLimit: they
// do not bump the journal version, they are recorded in the structured
// audit log's hash chain (ops "rule_set" / "rule_delete", see audit.go),
// and they round-trip through the disaster-recovery snapshot config
// record (see snapshot.go) together with the rule-set mutation counter.
// Illegal rules are rejected at construction — SetJournalRule returns the
// error and stores nothing — and an illegal rule in a snapshot fails the
// import instead of installing a rule that could never have been created.

// Validation errors for journal rules.
var (
	// ErrJournalRuleEmptyID is returned by SetJournalRule when the rule
	// has no ID.
	ErrJournalRuleEmptyID = errors.New("ledger: journal rule ID must not be empty")
	// ErrJournalRuleIDTooLong is returned by SetJournalRule when the rule
	// ID exceeds maxJournalRuleIDLen characters. The bound keeps the
	// generated derived-leg IDs and memos ("<id>/rule/<ruleID>/<i>",
	// "rule:<ruleID>:<label>") comfortably inside their limits.
	ErrJournalRuleIDTooLong = errors.New("ledger: journal rule ID must be at most 64 characters")
	// ErrJournalRuleIDSeparator is returned by SetJournalRule when the
	// rule ID contains '/': the separator is reserved for derived-leg
	// IDs, so a rule ID containing it would produce ambiguous journal
	// entry IDs.
	ErrJournalRuleIDSeparator = errors.New("ledger: journal rule ID must not contain '/'")
	// ErrJournalRuleNoLegs is returned by SetJournalRule when the rule
	// defines no derived legs: a rule that appends nothing is a caller
	// bug, not a no-op.
	ErrJournalRuleNoLegs = errors.New("ledger: journal rule must define at least one derived leg")
	// ErrJournalRuleLegAccounts is returned by SetJournalRule when a
	// derived leg has an empty debit or credit account, or names the same
	// account on both sides.
	ErrJournalRuleLegAccounts = errors.New("ledger: journal rule leg debit and credit accounts must be non-empty and differ")
	// ErrJournalRuleLegAmount is returned by SetJournalRule when a
	// derived leg does not set exactly one positive amount mode: either a
	// positive fixed_cents or a rate_bps in 1..10000 (a rate above 100% of
	// the principal is a configuration bug, not a clampable fee).
	ErrJournalRuleLegAmount = errors.New("ledger: journal rule leg must set exactly one of fixed_cents (positive) or rate_bps (1..10000)")
	// ErrJournalRuleMatchBounds is returned by SetJournalRule when a match
	// bound is negative, or when min_amount_cents exceeds a positive
	// max_amount_cents (a rule that can never match is a caller bug).
	ErrJournalRuleMatchBounds = errors.New("ledger: journal rule match bounds must be non-negative and min_amount_cents must not exceed max_amount_cents")
	// ErrJournalRuleLabelTooLong is returned by SetJournalRule when a leg
	// label exceeds maxJournalRuleLegLabelLen characters (see
	// ErrJournalRuleIDTooLong).
	ErrJournalRuleLabelTooLong = errors.New("ledger: journal rule leg label must be at most 64 characters")
	// ErrJournalRuleDescriptionTooLong is returned by SetJournalRule when
	// the rule description exceeds maxJournalRuleDescriptionLen
	// characters.
	ErrJournalRuleDescriptionTooLong = errors.New("ledger: journal rule description must be at most 255 characters")
	// ErrJournalRuleLegConflict is returned by Post when a derived rule
	// leg's deterministic ID collides with an existing journal entry.
	// The whole Post is rejected and records nothing — silently
	// overwriting a journaled entry is never an option.
	ErrJournalRuleLegConflict = errors.New("ledger: derived journal rule leg ID collides with an existing journal entry")
)

// maxJournalRuleIDLen bounds rule IDs so generated derived-leg IDs and
// memos stay short and unambiguous.
const maxJournalRuleIDLen = 64

// maxJournalRuleLegLabelLen bounds leg labels for the same reason.
const maxJournalRuleLegLabelLen = 64

// maxJournalRuleDescriptionLen bounds rule descriptions like entry memos.
const maxJournalRuleDescriptionLen = 255

// RuleLeg is one derived journal leg a journal rule appends: debit
// DebitAccount and credit CreditAccount AmountCents, in the principal
// entry's currency. The amount is either a fixed cent value (FixedCents)
// or a proportion of the principal entry's amount (RateBps basis points —
// 800 = 8% — computed as floor(principal * rate / 10000), overflow-safe
// like the transfer fee policy). Exactly one of the two must be set, and
// a computed amount of zero books no leg for that posting.
type RuleLeg struct {
	DebitAccount  AccountID `json:"debit_account"`
	CreditAccount AccountID `json:"credit_account"`
	FixedCents    int64     `json:"fixed_cents,omitempty"`
	RateBps       int64     `json:"rate_bps,omitempty"`
	Label         string    `json:"label,omitempty"`
}

// JournalRule is one opt-in automatic journaling rule. The match predicate
// is a conjunction: every non-zero field must fit the principal entry for
// the rule to fire. Empty match accounts or currency match any value;
// MinAmountCents 0 means no lower bound; MaxAmountCents 0 means no upper
// bound.
type JournalRule struct {
	ID                 string     `json:"id"`
	Enabled            bool       `json:"enabled"`
	Description        string     `json:"description,omitempty"`
	MatchDebitAccount  AccountID  `json:"match_debit_account,omitempty"`
	MatchCreditAccount AccountID  `json:"match_credit_account,omitempty"`
	MatchCurrency      string     `json:"match_currency,omitempty"`
	MinAmountCents     int64      `json:"min_amount_cents,omitempty"`
	MaxAmountCents     int64      `json:"max_amount_cents,omitempty"`
	Legs               []RuleLeg  `json:"legs"`
}

// validateJournalRuleLeg checks one derived leg: non-empty distinct legs,
// a label within bounds, and exactly one positive amount mode.
func validateJournalRuleLeg(leg RuleLeg) error {
	if leg.DebitAccount == "" || leg.CreditAccount == "" ||
		leg.DebitAccount == leg.CreditAccount {
		return ErrJournalRuleLegAccounts
	}
	if utf8.RuneCountInString(leg.Label) > maxJournalRuleLegLabelLen {
		return ErrJournalRuleLabelTooLong
	}
	fixed := leg.FixedCents > 0
	rate := leg.RateBps > 0
	if fixed == rate {
		// Both set is ambiguous; neither set (or a non-positive value)
		// would never book a leg.
		return ErrJournalRuleLegAmount
	}
	if leg.FixedCents < 0 || leg.RateBps < 0 || leg.RateBps > 10000 {
		return ErrJournalRuleLegAmount
	}
	return nil
}

// ValidateJournalRule rejects illegal rules at construction: a bad rule
// can never be stored, installed via snapshot, or served over HTTP.
// Callers: SetJournalRule, the snapshot import, and the HTTP rule
// endpoint.
func ValidateJournalRule(r JournalRule) error {
	if r.ID == "" {
		return ErrJournalRuleEmptyID
	}
	if utf8.RuneCountInString(r.ID) > maxJournalRuleIDLen {
		return ErrJournalRuleIDTooLong
	}
	if strings.Contains(r.ID, "/") {
		return ErrJournalRuleIDSeparator
	}
	if utf8.RuneCountInString(r.Description) > maxJournalRuleDescriptionLen {
		return ErrJournalRuleDescriptionTooLong
	}
	if len(r.Legs) == 0 {
		return ErrJournalRuleNoLegs
	}
	for _, leg := range r.Legs {
		if err := validateJournalRuleLeg(leg); err != nil {
			return err
		}
	}
	if r.MinAmountCents < 0 || r.MaxAmountCents < 0 {
		return ErrJournalRuleMatchBounds
	}
	if r.MaxAmountCents > 0 && r.MinAmountCents > r.MaxAmountCents {
		return ErrJournalRuleMatchBounds
	}
	if r.MatchCurrency != "" {
		if _, err := normalizeCurrency(r.MatchCurrency); err != nil {
			return err
		}
	}
	return nil
}

// SetJournalRule creates or replaces the journal rule r. Illegal rules are
// rejected with a validation error and stored nothing —
// SetJournalRule is fail-fast like SetDailyLimit, not panicking like the
// construction-time fee schedule, because rules are runtime operator
// config. Replacing an existing rule ID overwrites it (the mutation
// counter still bumps, so the change is visible to snapshot consumers).
// Setting a rule is structural: it does not bump the journal version, and
// the change is recorded in the structured audit log's hash chain (op
// "rule_set").
func (l *Ledger) SetJournalRule(r JournalRule) error {
	if err := ValidateJournalRule(r); err != nil {
		return err
	}
	currency := r.MatchCurrency
	if currency != "" {
		// Validated above; normalize so matching compares canonical
		// codes, like Post's currency normalization.
		var err error
		currency, err = normalizeCurrency(currency)
		if err != nil {
			return err
		}
	}
	legs := make([]RuleLeg, len(r.Legs))
	copy(legs, r.Legs)
	stored := r
	stored.MatchCurrency = currency
	stored.Legs = legs
	l.mu.Lock()
	defer l.mu.Unlock()
	l.journalRules[r.ID] = stored
	l.journalRuleVersion++
	l.emitAudit(AuditEvent{
		Op:            "rule_set",
		Actor:         "SetJournalRule",
		TraceID:       r.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"enabled":      r.Enabled,
			"legs":         len(r.Legs),
			"rule_version": l.journalRuleVersion,
		},
	})
	return nil
}

// DeleteJournalRule removes the journal rule id. Deleting a rule that was
// never set is a no-op returning false. Like SetJournalRule it is
// structural (no journal version bump) and it is recorded in the audit
// log's hash chain (op "rule_delete").
func (l *Ledger) DeleteJournalRule(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.journalRules[id]; !ok {
		return false
	}
	delete(l.journalRules, id)
	l.journalRuleVersion++
	l.emitAudit(AuditEvent{
		Op:            "rule_delete",
		Actor:         "DeleteJournalRule",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"rule_version": l.journalRuleVersion,
		},
	})
	return true
}

// GetJournalRule returns the rule id and whether it is currently
// installed. The returned rule is a copy: mutating it does not affect the
// ledger.
func (l *Ledger) GetJournalRule(id string) (JournalRule, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.journalRules[id]
	if !ok {
		return JournalRule{}, false
	}
	legs := make([]RuleLeg, len(r.Legs))
	copy(legs, r.Legs)
	r.Legs = legs
	return r, true
}

// JournalRules returns every installed journal rule, sorted by rule ID.
// The returned rules are copies.
func (l *Ledger) JournalRules() []JournalRule {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.journalRulesLocked()
}

// journalRulesLocked returns every installed rule, sorted by rule ID, as
// copies. Callers must hold l.mu; the read lock suffices. The snapshot
// export calls this directly so the config record is deterministic.
func (l *Ledger) journalRulesLocked() []JournalRule {
	out := make([]JournalRule, 0, len(l.journalRules))
	for _, r := range l.journalRules {
		legs := make([]RuleLeg, len(r.Legs))
		copy(legs, r.Legs)
		r.Legs = legs
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// JournalRuleVersion returns the rule-set mutation counter: every
// SetJournalRule and DeleteJournalRule bumps it. The counter round-trips
// through the snapshot config record, so a restored ledger carries the
// same rule-set generation as the source.
func (l *Ledger) JournalRuleVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.journalRuleVersion
}

// RuleLegsCommitted reports how many derived journal legs the rules engine
// has committed on this ledger. Operational metric (surfaced on GET
// /metrics), not books: it is in-memory only and does not round-trip
// through snapshots.
func (l *Ledger) RuleLegsCommitted() uint64 {
	return l.ruleLegsCommitted.Load()
}

// ruleMatchesEntry reports whether rule r fires on principal entry e. e
// must already be validated and currency-normalized (the state Post holds
// after validateJournalEntry).
func ruleMatchesEntry(r JournalRule, e JournalEntry) bool {
	if !r.Enabled {
		return false
	}
	if r.MatchDebitAccount != "" && r.MatchDebitAccount != e.DebitAccount {
		return false
	}
	if r.MatchCreditAccount != "" && r.MatchCreditAccount != e.CreditAccount {
		return false
	}
	if r.MatchCurrency != "" && r.MatchCurrency != e.Currency {
		return false
	}
	if e.AmountCents < r.MinAmountCents {
		return false
	}
	if r.MaxAmountCents > 0 && e.AmountCents > r.MaxAmountCents {
		return false
	}
	return true
}

// ruleLegMemo builds the memo for a derived leg: "rule:<rule ID>" plus
// ":<label>" when the leg has one. Inputs are length-bounded at rule
// construction, so the memo always fits the journal memo limit.
func ruleLegMemo(ruleID, label string) string {
	if label == "" {
		return "rule:" + ruleID
	}
	return "rule:" + ruleID + ":" + label
}

// derivedRuleLegsLocked evaluates the journal rules against principal
// entry e and builds the derived legs every matching enabled rule
// appends, in (rule ID, leg index) order. e must already have cleared the
// full Post gate order — validation, idempotency replay, frozen,
// overdraft, daily-limit, period — with its currency normalized and
// CreatedAt filled.
//
// The derived legs then run the same risk-gate order Post uses: field
// validation, the frozen check on every account they touch, the overdraft
// check on the aggregate outflow per protected credit account (principal +
// derived legs, mirroring PostTransfer's payer-total check — the
// comparison never subtracts, so it cannot overflow), the daily-limit
// check on the same aggregate outflow, and the period gate on the shared
// timestamp (already cleared by the principal, documented here for the
// ordering contract). The idempotency replay check does not apply to
// derived legs: they carry no key, and the principal's replay returns the
// original entry without re-firing rules.
//
// Any failure returns an error and no legs: the caller records nothing.
// Callers must hold the write lock.
func (l *Ledger) derivedRuleLegsLocked(e JournalEntry) (derived []JournalEntry, ruleIDs []string, err error) {
	if len(l.journalRules) == 0 {
		return nil, nil, nil
	}
	var matched []JournalRule
	for _, r := range l.journalRules {
		if ruleMatchesEntry(r, e) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return nil, nil, nil
	}
	// Deterministic firing order: map iteration is random, and the
	// journal must not depend on it (derived-leg IDs embed the rule ID,
	// and multi-rule posts must replay identically).
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	for _, r := range matched {
		fired := false
		for i, leg := range r.Legs {
			amount := leg.FixedCents
			if leg.RateBps > 0 {
				// Overflow-safe floor(principal * rate / 10000),
				// the same helper the transfer fee policy uses.
				amount = policyFeeCents(e.AmountCents, leg.RateBps)
			}
			if amount <= 0 {
				// A computed zero leg books nothing for this
				// posting (a tiny amount under a low rate),
				// mirroring the fee policy's dust tiers. The
				// rule itself was validated to be capable of
				// booking, so this is a per-posting skip, not
				// a misconfiguration.
				continue
			}
			id := e.ID + "/rule/" + r.ID + "/" + strconv.Itoa(i)
			if _, exists := l.entries[id]; exists {
				return nil, nil, ErrJournalRuleLegConflict
			}
			derived = append(derived, JournalEntry{
				ID:            id,
				DebitAccount:  leg.DebitAccount,
				CreditAccount: leg.CreditAccount,
				AmountCents:   amount,
				Currency:      e.Currency,
				Memo:          ruleLegMemo(r.ID, leg.Label),
				CreatedAt:     e.CreatedAt,
			})
			fired = true
		}
		if fired {
			ruleIDs = append(ruleIDs, r.ID)
		}
	}
	if len(derived) == 0 {
		return nil, nil, nil
	}

	// Field validation, defense in depth: the legs are built from
	// validated rules, but they enter the journal like any posting.
	for i := range derived {
		if err := validateJournalEntry(&derived[i]); err != nil {
			return nil, nil, err
		}
	}

	// Frozen: every account a derived leg touches, mirroring
	// PostTransfer's check on payer, payee, and fee account.
	for _, d := range derived {
		if l.closedLocked(d.DebitAccount) || l.closedLocked(d.CreditAccount) {
			return nil, nil, ErrAccountClosed
		}
		if l.frozenLocked(d.DebitAccount) || l.frozenLocked(d.CreditAccount) {
			return nil, nil, ErrAccountFrozen
		}
	}

	// Aggregate outflow per (credit account, currency) across the
	// principal and all derived legs: overdraft protection, daily
	// limits, and monthly spend budgets guard total outflow, not
	// per-leg slices — a principal that passes alone must still fail
	// when its rule legs push the payer over the line. Totals saturate
	// instead of overflowing; the gates below only compare.
	outflow := make(map[accountCurrency]int64)
	addOutflow := func(a AccountID, amount int64) {
		k := accountCurrency{account: a, currency: e.Currency}
		if outflow[k] > math.MaxInt64-amount {
			outflow[k] = math.MaxInt64
		} else {
			outflow[k] += amount
		}
	}
	addOutflow(e.CreditAccount, e.AmountCents)
	for _, d := range derived {
		addOutflow(d.CreditAccount, d.AmountCents)
	}
	for k, total := range outflow {
		if l.noOverdraft[k.account] && l.balances[k] < total {
			return nil, nil, ErrAccountOverdraft
		}
	}
	for k, total := range outflow {
		if l.dailyLimitRejectedLocked(k.account, k.currency, total, e.CreatedAt) {
			return nil, nil, ErrDailyLimitExceeded
		}
	}
	for k, total := range outflow {
		if l.budgetRejectedLocked(k.account, k.currency, total, e.CreatedAt) {
			return nil, nil, ErrBudgetExceeded
		}
	}
	// Period gate: derived legs share the principal's timestamp, which
	// already cleared periodRejectedLocked in Post — a closed period
	// rejects the principal before rules are even evaluated.

	return derived, ruleIDs, nil
}
