package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Automatic sweep rules (LG-178): a scheduled rules engine on top of
// the manual PostSweep (LG-21).
//
// A rule watches a set of source accounts and, when its trigger fires,
// sweeps money into a target account — the treasury pattern: merchant
// sub-accounts swept into the master account on a schedule or when
// balances pile up. Three trigger kinds:
//
//   - threshold: when a source's (account, currency) balance exceeds
//     ThresholdCents, sweep the excess down to TargetCents.
//   - periodic: once per UTC day, at Cutoff ("HH:MM"), sweep every
//     source's full positive balances — the day-end cutoff sweep.
//   - target-balance: whenever a source's (account, currency) balance
//     exceeds TargetCents, sweep the excess down to TargetCents,
//     keeping the account at its target float.
//
// Execution reuses the LG-21 sweep machinery: each firing books through
// postSweepLegsLocked with the same gate chain as a manual PostSweep —
// field validation, idempotency namespaces, frozen/closed/suspended
// checks, monthly spend budgets, the period gate, automatic posting
// rules — and the same atomic commit. A rule firing never charges a
// fee: auto-sweeps are internal treasury movements. Sweep-reserve
// floors (see sweep_reserve.go) do NOT apply to rule executions: the
// rule's own target is the authoritative floor (configure the rule's
// target to encode the operating float).
//
// Thresholds are evaluated per (account, currency) balance, each in
// its own minor units: a threshold of 100000 means 100000 cents of USD
// and 100000 minor units of every other currency the source holds.
//
// Rules are in-memory operational config (like the hold sweeper's
// interval): they do not round-trip through disaster-recovery
// snapshots. Register them at process start, next to the other
// operator wiring.
//
// Scheduling: RunAutoSweepRules(now) is the cron entry point — wire it
// to the operator's scheduler (cron/systemd/equivalent), the same way
// POST /reconcile drives the day-end reconciliation job. There is no
// in-process ticker: the caller owns the schedule, so the firing
// instants stay auditable in one place.
//
// Every firing emits an "autosweep_triggered" audit event (rule id,
// kind, sweep id, legs) in addition to the sweep's own "sweep" event;
// rule registration and removal emit "autosweep_rule_registered" /
// "autosweep_rule_unregistered". PreviewAutoSweepRule is the dry-run:
// it plans the legs under a read lock and books nothing — no journal
// rows, no audit events, no version bump.

// AutoSweepRuleKind selects the trigger semantics of an auto-sweep
// rule.
type AutoSweepRuleKind string

const (
	// AutoSweepThreshold fires when a source balance exceeds
	// ThresholdCents, sweeping the excess down to TargetCents.
	AutoSweepThreshold AutoSweepRuleKind = "threshold"
	// AutoSweepPeriodic fires once per UTC day at Cutoff, sweeping
	// every source's full positive balances.
	AutoSweepPeriodic AutoSweepRuleKind = "periodic"
	// AutoSweepTargetBalance fires whenever a source balance exceeds
	// TargetCents, sweeping the excess down to TargetCents.
	AutoSweepTargetBalance AutoSweepRuleKind = "target-balance"
)

// Validation errors returned by RegisterAutoSweepRule.
var (
	// ErrEmptyAutoSweepRuleID is returned when the rule carries no ID.
	ErrEmptyAutoSweepRuleID = errors.New("ledger: auto-sweep rule ID must not be empty")
	// ErrInvalidAutoSweepRuleID is returned when the rule ID contains
	// characters outside [A-Za-z0-9_-]: the ID is embedded in the
	// generated sweep ID ("autosweep/<rule ID>/<unixnano>").
	ErrInvalidAutoSweepRuleID = errors.New("ledger: auto-sweep rule ID must match [A-Za-z0-9_-]+")
	// ErrDuplicateAutoSweepRule is returned when a rule with the same
	// ID is already registered.
	ErrDuplicateAutoSweepRule = errors.New("ledger: auto-sweep rule ID already registered")
	// ErrInvalidAutoSweepRuleKind is returned for an unknown rule kind.
	ErrInvalidAutoSweepRuleKind = errors.New("ledger: unknown auto-sweep rule kind")
	// ErrEmptyAutoSweepRuleSources is returned when the rule names no
	// source account.
	ErrEmptyAutoSweepRuleSources = errors.New("ledger: auto-sweep rule must name at least one source account")
	// ErrEmptyAutoSweepRuleSource is returned when one of the source
	// accounts is empty.
	ErrEmptyAutoSweepRuleSource = errors.New("ledger: auto-sweep rule source account must not be empty")
	// ErrEmptyAutoSweepRuleTarget is returned when the rule names no
	// target account.
	ErrEmptyAutoSweepRuleTarget = errors.New("ledger: auto-sweep rule must name a target account")
	// ErrAutoSweepRuleTargetIsSource is returned when the target also
	// appears among the sources.
	ErrAutoSweepRuleTargetIsSource = errors.New("ledger: auto-sweep rule target must not be one of the source accounts")
	// ErrDuplicateAutoSweepRuleSource is returned when a source
	// account is named twice.
	ErrDuplicateAutoSweepRuleSource = errors.New("ledger: auto-sweep rule source accounts must be distinct")
	// ErrInvalidAutoSweepRuleParams is returned when the kind-specific
	// parameters are missing or inconsistent: a threshold rule needs
	// ThresholdCents > 0 with 0 <= TargetCents <= ThresholdCents and
	// no Cutoff; a target-balance rule needs TargetCents >= 0, no
	// ThresholdCents and no Cutoff; a periodic rule needs a valid
	// "HH:MM" Cutoff and no ThresholdCents/TargetCents.
	ErrInvalidAutoSweepRuleParams = errors.New("ledger: auto-sweep rule parameters invalid for its kind")
	// ErrAutoSweepRuleNotFound is returned by PreviewAutoSweepRule for
	// an unregistered rule ID.
	ErrAutoSweepRuleNotFound = errors.New("ledger: auto-sweep rule not found")
)

// AutoSweepRule is one registered automatic sweep rule. See the
// package doc above for the trigger semantics of each kind.
type AutoSweepRule struct {
	// ID is the stable rule identifier, [A-Za-z0-9_-]+. It is embedded
	// in the generated sweep IDs, so re-registering the same ID after
	// removal starts a fresh rule history.
	ID   string           `json:"id"`
	Kind AutoSweepRuleKind `json:"kind"`
	// From lists the source accounts in sweep order; legs commit in
	// this order with currencies sorted within each source.
	From []AccountID `json:"from_accounts"`
	// To is the target account that receives every leg.
	To AccountID `json:"to_account"`
	// ThresholdCents is the trigger level for the threshold kind:
	// a source balance above it fires the rule.
	ThresholdCents int64 `json:"threshold_cents,omitempty"`
	// TargetCents is the sweep-down-to level for the threshold and
	// target-balance kinds: the rule leaves this much behind.
	TargetCents int64 `json:"target_cents,omitempty"`
	// Cutoff is the daily firing time for the periodic kind, "HH:MM"
	// in UTC (e.g. "23:55" for the day-end cutoff sweep).
	Cutoff string `json:"cutoff,omitempty"`
	// Enabled gates firing: disabled rules are kept registered but
	// never fire and never appear in RunAutoSweepRules results.
	Enabled bool `json:"enabled"`
}

// AutoSweepRuleStatus pairs a registered rule with its last firing
// time (zero when the rule never fired in this process).
type AutoSweepRuleStatus struct {
	Rule      AutoSweepRule `json:"rule"`
	LastRunAt time.Time     `json:"last_run_at,omitempty"`
}

// AutoSweepLeg is one planned movement of a rule firing: AmountCents
// of Currency moves from From to the rule's target.
type AutoSweepLeg struct {
	From        AccountID `json:"from_account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
}

// AutoSweepPreview is the dry-run of a rule at a point in time:
// Eligible reports whether the rule would fire, Legs the exact
// movements it would book, Reason why it would or would not fire.
// Planning is read-only: no journal rows, no audit events, no version
// bump.
type AutoSweepPreview struct {
	RuleID   string           `json:"rule_id"`
	Kind     AutoSweepRuleKind `json:"kind"`
	Eligible bool             `json:"eligible"`
	Reason   string           `json:"reason"`
	Legs     []AutoSweepLeg   `json:"legs,omitempty"`
}

// AutoSweepRunResult reports one rule's evaluation inside a
// RunAutoSweepRules pass. Fired=false means the rule did not fire
// (Reason explains why — not due, no balance over the trigger,
// disabled); Fired=true with Error set means the rule fired but the
// sweep was rejected by a risk gate — nothing was booked.
type AutoSweepRunResult struct {
	RuleID  string         `json:"rule_id"`
	Fired   bool           `json:"fired"`
	Reason  string         `json:"reason,omitempty"`
	SweepID string         `json:"sweep_id,omitempty"`
	Legs    []AutoSweepLeg `json:"legs,omitempty"`
	Entries []JournalEntry `json:"entries,omitempty"`
	Error   error          `json:"-"`
}

// autoSweepRuleState is the registered rule plus its firing history.
// Callers must hold l.mu.
type autoSweepRuleState struct {
	rule      AutoSweepRule
	lastRunAt time.Time
}

// validateAutoSweepRule checks the rule's fields without touching
// ledger state.
func validateAutoSweepRule(rule AutoSweepRule) error {
	if rule.ID == "" {
		return ErrEmptyAutoSweepRuleID
	}
	for _, c := range rule.ID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ErrInvalidAutoSweepRuleID
		}
	}
	switch rule.Kind {
	case AutoSweepThreshold, AutoSweepPeriodic, AutoSweepTargetBalance:
	default:
		return ErrInvalidAutoSweepRuleKind
	}
	if len(rule.From) == 0 {
		return ErrEmptyAutoSweepRuleSources
	}
	if rule.To == "" {
		return ErrEmptyAutoSweepRuleTarget
	}
	seen := make(map[AccountID]bool, len(rule.From))
	for _, from := range rule.From {
		if from == "" {
			return ErrEmptyAutoSweepRuleSource
		}
		if from == rule.To {
			return ErrAutoSweepRuleTargetIsSource
		}
		if seen[from] {
			return ErrDuplicateAutoSweepRuleSource
		}
		seen[from] = true
	}
	switch rule.Kind {
	case AutoSweepThreshold:
		if rule.ThresholdCents <= 0 || rule.TargetCents < 0 ||
			rule.TargetCents > rule.ThresholdCents || rule.Cutoff != "" {
			return ErrInvalidAutoSweepRuleParams
		}
	case AutoSweepTargetBalance:
		if rule.TargetCents < 0 || rule.ThresholdCents != 0 || rule.Cutoff != "" {
			return ErrInvalidAutoSweepRuleParams
		}
	case AutoSweepPeriodic:
		if rule.ThresholdCents != 0 || rule.TargetCents != 0 {
			return ErrInvalidAutoSweepRuleParams
		}
		if _, err := parseAutoSweepCutoff(rule.Cutoff); err != nil {
			return err
		}
	}
	return nil
}

// parseAutoSweepCutoff parses a strict "HH:MM" 24-hour UTC time into
// minutes since midnight.
func parseAutoSweepCutoff(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, ErrInvalidAutoSweepRuleParams
	}
	digit := func(c byte) (int, bool) {
		if c < '0' || c > '9' {
			return 0, false
		}
		return int(c - '0'), true
	}
	h1, ok1 := digit(s[0])
	h2, ok2 := digit(s[1])
	m1, ok3 := digit(s[3])
	m2, ok4 := digit(s[4])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return 0, ErrInvalidAutoSweepRuleParams
	}
	h, m := h1*10+h2, m1*10+m2
	if h > 23 || m > 59 {
		return 0, ErrInvalidAutoSweepRuleParams
	}
	return h*60 + m, nil
}

// RegisterAutoSweepRule validates and registers a rule. Registering is
// a structural change: it does not bump the journal version but emits
// an "autosweep_rule_registered" audit event.
func (l *Ledger) RegisterAutoSweepRule(rule AutoSweepRule) error {
	if err := validateAutoSweepRule(rule); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.autoSweepRules[rule.ID]; exists {
		return ErrDuplicateAutoSweepRule
	}
	rule.From = append([]AccountID(nil), rule.From...)
	versionBefore := l.version
	l.autoSweepRules[rule.ID] = &autoSweepRuleState{rule: rule}
	l.emitAudit(AuditEvent{
		Op:            "autosweep_rule_registered",
		Actor:         "RegisterAutoSweepRule",
		TraceID:       rule.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		Accounts:      append(append([]AccountID{}, rule.From...), rule.To),
		Details: map[string]any{
			"rule_id": string(rule.ID),
			"kind":    string(rule.Kind),
		},
	})
	return nil
}

// UnregisterAutoSweepRule removes a rule. It reports whether a rule was
// registered under the ID, and emits "autosweep_rule_unregistered" when
// one was. Removal is structural: no version bump. A removed rule
// never fires again; its past sweep IDs stay in the journal.
func (l *Ledger) UnregisterAutoSweepRule(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, exists := l.autoSweepRules[id]
	if !exists {
		return false
	}
	versionBefore := l.version
	delete(l.autoSweepRules, id)
	l.emitAudit(AuditEvent{
		Op:            "autosweep_rule_unregistered",
		Actor:         "UnregisterAutoSweepRule",
		TraceID:       id,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		Accounts:      append(append([]AccountID{}, st.rule.From...), st.rule.To),
		Details: map[string]any{
			"rule_id": string(st.rule.ID),
			"kind":    string(st.rule.Kind),
		},
	})
	return true
}

// ListAutoSweepRules returns every registered rule with its last
// firing time, sorted by rule ID for determinism.
func (l *Ledger) ListAutoSweepRules() []AutoSweepRuleStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]AutoSweepRuleStatus, 0, len(l.autoSweepRules))
	for _, st := range l.autoSweepRules {
		out = append(out, AutoSweepRuleStatus{Rule: st.rule, LastRunAt: st.lastRunAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule.ID < out[j].Rule.ID })
	return out
}

// PreviewAutoSweepRule dry-runs a rule at now: it reports whether the
// rule would fire and the exact legs it would book, without booking
// anything — no journal rows, no audit events, no version bump.
func (l *Ledger) PreviewAutoSweepRule(id string, now time.Time) (AutoSweepPreview, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	st, ok := l.autoSweepRules[id]
	if !ok {
		return AutoSweepPreview{}, ErrAutoSweepRuleNotFound
	}
	legs, eligible, reason := l.planAutoSweepLegsLocked(st, now)
	return AutoSweepPreview{
		RuleID:   st.rule.ID,
		Kind:     st.rule.Kind,
		Eligible: eligible,
		Reason:   reason,
		Legs:     legs,
	}, nil
}

// planAutoSweepLegsLocked evaluates a rule against current balances and
// plans the legs a firing would book. It is read-only: preview and
// execution share it, so the dry-run sees exactly what the run would
// do. Callers must hold l.mu (either lock suffices).
func (l *Ledger) planAutoSweepLegsLocked(st *autoSweepRuleState, now time.Time) (legs []AutoSweepLeg, eligible bool, reason string) {
	rule := st.rule
	// positiveBalance is one source currency balance above zero.
	type positiveBalance struct {
		currency string
		cents    int64
	}
	// balancesOf collects the source's positive per-currency balances,
	// currencies sorted for determinism.
	balancesOf := func(from AccountID) []positiveBalance {
		var out []positiveBalance
		for k, bal := range l.balances {
			if k.account == from && bal > 0 {
				out = append(out, positiveBalance{currency: k.currency, cents: bal})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].currency < out[j].currency })
		return out
	}
	switch rule.Kind {
	case AutoSweepPeriodic:
		cutoffMin, err := parseAutoSweepCutoff(rule.Cutoff)
		if err != nil {
			return nil, false, "invalid cutoff"
		}
		midnight := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
		todayCutoff := midnight.Add(time.Duration(cutoffMin) * time.Minute)
		if now.Before(todayCutoff) {
			return nil, false, fmt.Sprintf("not due: cutoff %s UTC not reached", rule.Cutoff)
		}
		if !st.lastRunAt.Before(todayCutoff) {
			return nil, false, "not due: already ran after today's cutoff"
		}
		for _, from := range rule.From {
			for _, b := range balancesOf(from) {
				legs = append(legs, AutoSweepLeg{
					From:        from,
					Currency:    b.currency,
					AmountCents: b.cents,
				})
			}
		}
		return legs, true, "due: past daily cutoff"
	case AutoSweepThreshold, AutoSweepTargetBalance:
		trigger := rule.ThresholdCents
		if rule.Kind == AutoSweepTargetBalance {
			trigger = rule.TargetCents
		}
		for _, from := range rule.From {
			for _, b := range balancesOf(from) {
				if b.cents > trigger {
					legs = append(legs, AutoSweepLeg{
						From:        from,
						Currency:    b.currency,
						AmountCents: b.cents - rule.TargetCents,
					})
				}
			}
		}
		if len(legs) == 0 {
			return nil, false, "no source balance above the trigger"
		}
		return legs, true, "source balance above the trigger"
	default:
		return nil, false, "unknown rule kind"
	}
}

// RunAutoSweepRules evaluates every enabled rule at now and fires the
// eligible ones, in rule-ID order. Each firing books through the LG-21
// sweep gate chain (see postSweepLegsLocked) with a generated sweep ID
// "autosweep/<rule ID>/<unixnano>", then emits an
// "autosweep_triggered" audit event. A rule whose sweep is rejected by
// a risk gate (frozen account, closed period, ...) reports the error
// and books nothing; evaluation continues with the next rule.
func (l *Ledger) RunAutoSweepRules(now time.Time) []AutoSweepRunResult {
	l.mu.RLock()
	states := make([]*autoSweepRuleState, 0, len(l.autoSweepRules))
	for _, st := range l.autoSweepRules {
		if st.rule.Enabled {
			states = append(states, st)
		}
	}
	l.mu.RUnlock()
	sort.Slice(states, func(i, j int) bool { return states[i].rule.ID < states[j].rule.ID })
	results := make([]AutoSweepRunResult, 0, len(states))
	for _, st := range states {
		results = append(results, l.runAutoSweepRule(st, now))
	}
	return results
}

// runAutoSweepRule evaluates and fires a single rule. The rule is
// re-read under the write lock: it may have been unregistered or
// disabled between the RunAutoSweepRules snapshot and this call.
func (l *Ledger) runAutoSweepRule(st *autoSweepRuleState, now time.Time) AutoSweepRunResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.autoSweepRules[st.rule.ID]
	if !ok || !cur.rule.Enabled {
		return AutoSweepRunResult{RuleID: st.rule.ID, Fired: false, Reason: "rule unregistered or disabled"}
	}
	legs, eligible, reason := l.planAutoSweepLegsLocked(cur, now)
	if !eligible {
		return AutoSweepRunResult{RuleID: cur.rule.ID, Fired: false, Reason: reason}
	}
	// The sweep ID embeds the rule ID and the firing instant. It is
	// unique per (rule, firing): a rule cannot fire twice for the same
	// instant — periodic rules fire at most once per cutoff, and
	// balance-triggered rules stop being eligible once their legs are
	// booked — so no two firings share an ID.
	sweepID := "autosweep/" + cur.rule.ID + "/" + fmt.Sprintf("%d", now.UnixNano())
	from := make([]AccountID, 0, len(legs))
	seenFrom := make(map[AccountID]bool, len(legs))
	sweepLegs := make([]SweepLeg, 0, len(legs))
	for _, leg := range legs {
		if !seenFrom[leg.From] {
			seenFrom[leg.From] = true
			from = append(from, leg.From)
		}
		sweepLegs = append(sweepLegs, SweepLeg{
			From:        leg.From,
			Currency:    leg.Currency,
			AmountCents: leg.AmountCents,
			EntryID:     sweepID + "/" + string(leg.From) + "/" + leg.Currency,
		})
	}
	sweep := Sweep{
		ID:        sweepID,
		From:      from,
		To:        cur.rule.To,
		CreatedAt: now,
	}
	versionBefore := l.version
	receipt, err := l.postSweepLegsLocked(sweep, sweepLegs, false)
	if err != nil {
		return AutoSweepRunResult{
			RuleID: cur.rule.ID,
			Fired:  true,
			Reason: reason,
			Error:  err,
		}
	}
	cur.lastRunAt = now
	entryIDs := make([]string, 0, len(receipt.Entries))
	for _, e := range receipt.Entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.emitAudit(AuditEvent{
		Op:            "autosweep_triggered",
		Actor:         "AutoSweep",
		TraceID:       sweepID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      append(append([]AccountID{}, cur.rule.From...), cur.rule.To),
		Details: map[string]any{
			"rule_id":  string(cur.rule.ID),
			"kind":     string(cur.rule.Kind),
			"sweep_id": sweepID,
			"legs":     len(legs),
		},
	})
	return AutoSweepRunResult{
		RuleID:  cur.rule.ID,
		Fired:   true,
		Reason:  reason,
		SweepID: sweepID,
		Legs:    legs,
		Entries: receipt.Entries,
	}
}
