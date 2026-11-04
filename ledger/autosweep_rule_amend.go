package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Automatic sweep rule runtime revision (LG-307).
//
// RegisterAutoSweepRule fixes a rule's trigger parameters for the life
// of the registration: the only way to change a threshold, a target, a
// cutoff, or the source/target accounts was UnregisterAutoSweepRule +
// RegisterAutoSweepRule, which is a teardown, not a revision. Between
// the two calls the rule does not exist — a RunAutoSweepRules pass in
// the gap skips it entirely — the re-registration starts a fresh rule
// history (autoSweepRuleState's lastRunAt is lost, so a periodic rule
// can fire twice for one cutoff day), and the audit trail shows two
// lifecycle events instead of one revision. This module is the
// orthogonal runtime path: an operator revises any subset of a live
// rule's fields in place, instead of rebuilding the rule.
//
// Semantics, deliberately fail-closed throughout, along the LG-304 /
// LG-305 / LG-306 structural revisions:
//
//   - The revision is in place: the rule keeps its ID, its kind, its
//     registration, and its firing history. Only the patched fields
//     move; lastRunAt is untouched, so a periodic rule revised after
//     firing still fires at most once per cutoff day, and a
//     threshold rule revised after firing keeps the drained balance
//     it already produced. The revision governs only future
//     RunAutoSweepRules evaluations: a sweep already triggered is
//     never re-planned or re-booked.
//   - Kind is not revisable: there is no Kind field on the patch.
//     Changing a rule's trigger kind is Unregister + Register — a
//     different rule, not a revision of this one.
//   - Validation is merge-then-install: the requested partial patch
//     is merged over the live rule (nil fields keep the live value)
//     and the merged rule reuses validateAutoSweepRule's full gates
//     — threshold rules need ThresholdCents > 0 with
//     0 <= TargetCents <= ThresholdCents and no Cutoff, periodic
//     rules need a valid "HH:MM" Cutoff and no threshold/target
//     fields, From/To reuse the registration-time checks. A runtime
//     revision never silently normalizes (along LG-241 / LG-287):
//     an illegal merged rule is ErrInvalidAutoSweepRuleRevision and
//     the live rule stays exactly as it was.
//   - An all-nil patch, and a revision identical to the live rule on
//     every field, are no-ops: they return the live rule with a nil
//     error and write no audit event, so a retried or duplicated
//     revision cannot spam the audit trail.
//   - An unknown rule ID returns ErrAutoSweepRuleNotFound, the same
//     sentinel PreviewAutoSweepRule uses (UnregisterAutoSweepRule
//     reports only a bool, so there is no other error name to reuse).
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     autosweep_rule_revised audit event (rule_id, kind, and
//     previous_*/current_* per changed field). The Ctx variant
//     attributes the operator (LG-73).
//   - Locking is the ledger write lock for the whole lookup, merge,
//     validate, install, and audit — the same lock RunAutoSweepRules
//     takes per rule — so a revision racing a run can neither tear a
//     rule mid-evaluation nor lose a firing: the run sees the old
//     rule or the new rule, never a half-revised one.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     rule tuning is in-process structural config.
//   - Snapshots do not carry rules (along LG-141): auto-sweep rules
//     are in-memory operational config, so a restart restores no
//     rule — revised or otherwise — and rules must be re-registered
//     at process start. The revision events themselves stay in the
//     audit trail for traceability.
//
// Orthogonality: LG-178 owns the auto-sweep mechanism itself (the
// three trigger kinds, planning, firing — this module is its runtime
// entry point), LG-292/293/294/295 own the SAR knobs (a different
// ruleset), and SetDunningConfig owns the dunning configuration.

var (
	// ErrInvalidAutoSweepRuleRevision is returned by
	// ReviseAutoSweepRule when the merged rule fails
	// validateAutoSweepRule: the live rule is left untouched.
	ErrInvalidAutoSweepRuleRevision = errors.New("ledger: auto-sweep rule revision invalid for its kind")
)

// AutoSweepRuleRevision is a partial revision of one registered
// auto-sweep rule. A nil field keeps the live value; a non-nil field
// replaces it. All fields nil is a no-op. Kind is deliberately absent:
// changing a rule's kind is Unregister + Register, not a revision.
type AutoSweepRuleRevision struct {
	// ThresholdCents is the trigger level for threshold rules.
	// Must stay > 0 with 0 <= TargetCents <= ThresholdCents.
	ThresholdCents *int64
	// TargetCents is the sweep-down-to level for threshold and
	// target-balance rules.
	TargetCents *int64
	// Cutoff is the daily firing time for periodic rules, "HH:MM"
	// in UTC.
	Cutoff *string
	// Enabled gates firing: a disabled rule stays registered but
	// never fires.
	Enabled *bool
	// From replaces the source accounts when non-nil; nil keeps the
	// live list. A non-nil empty list is rejected by validation.
	From []AccountID
	// To replaces the target account when non-nil.
	To *AccountID
}

// mergeAutoSweepRuleRevision overlays a partial revision on the live
// rule: nil fields keep the live value. The From list is deep-copied
// so the installed rule never aliases the caller's slice.
func mergeAutoSweepRuleRevision(rule AutoSweepRule, patch AutoSweepRuleRevision) AutoSweepRule {
	merged := rule
	if patch.ThresholdCents != nil {
		merged.ThresholdCents = *patch.ThresholdCents
	}
	if patch.TargetCents != nil {
		merged.TargetCents = *patch.TargetCents
	}
	if patch.Cutoff != nil {
		merged.Cutoff = *patch.Cutoff
	}
	if patch.Enabled != nil {
		merged.Enabled = *patch.Enabled
	}
	if patch.From != nil {
		merged.From = append([]AccountID(nil), patch.From...)
	}
	if patch.To != nil {
		merged.To = *patch.To
	}
	return merged
}

// autoSweepRulesEqual reports whether two rules agree on every
// revisable field. ID and Kind are never revised, so they are part of
// the comparison only as a guard — equal rules are the no-op case.
func autoSweepRulesEqual(a, b AutoSweepRule) bool {
	if a.ID != b.ID || a.Kind != b.Kind ||
		a.ThresholdCents != b.ThresholdCents ||
		a.TargetCents != b.TargetCents ||
		a.Cutoff != b.Cutoff ||
		a.Enabled != b.Enabled ||
		a.To != b.To ||
		len(a.From) != len(b.From) {
		return false
	}
	for i := range a.From {
		if a.From[i] != b.From[i] {
			return false
		}
	}
	return true
}

// copyAutoSweepRule returns a copy whose From list does not alias the
// stored rule, so a caller holding the returned rule cannot mutate
// ledger state through it.
func copyAutoSweepRule(rule AutoSweepRule) AutoSweepRule {
	out := rule
	out.From = append([]AccountID(nil), rule.From...)
	return out
}

// ReviseAutoSweepRule revises one registered auto-sweep rule in place
// and returns the revised rule. See ReviseAutoSweepRuleCtx.
func (l *Ledger) ReviseAutoSweepRule(id string, patch AutoSweepRuleRevision) (AutoSweepRule, error) {
	return l.ReviseAutoSweepRuleCtx(context.Background(), id, patch)
}

// ReviseAutoSweepRuleCtx is ReviseAutoSweepRule with per-call audit
// actor attribution (LG-73). The patch merges over the live rule and
// the merged rule is re-validated with validateAutoSweepRule's full
// gates; an illegal merge returns ErrInvalidAutoSweepRuleRevision
// (wrapping the failed gate) with the live rule untouched. An unknown
// ID returns ErrAutoSweepRuleNotFound. An all-nil patch, and a
// revision identical to the live rule, are no-ops that return the
// live rule with a nil error and no audit event. A real revision
// installs the merged rule in place — lastRunAt and rule history are
// preserved — and emits one autosweep_rule_revised audit event
// (rule_id, kind, previous_*/current_* per changed field). The
// revision bumps neither the version nor the audit chain, writes no
// journal entry, and emits no outbox event; it governs only future
// RunAutoSweepRules evaluations.
func (l *Ledger) ReviseAutoSweepRuleCtx(ctx context.Context, id string, patch AutoSweepRuleRevision) (AutoSweepRule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.autoSweepRules[id]
	if !ok {
		return AutoSweepRule{}, ErrAutoSweepRuleNotFound
	}
	previous := st.rule
	merged := mergeAutoSweepRuleRevision(previous, patch)
	if err := validateAutoSweepRule(merged); err != nil {
		return AutoSweepRule{}, fmt.Errorf("%w: %v", ErrInvalidAutoSweepRuleRevision, err)
	}
	if autoSweepRulesEqual(previous, merged) {
		return copyAutoSweepRule(previous), nil
	}
	st.rule = merged
	details := map[string]any{
		"rule_id": string(merged.ID),
		"kind":    string(merged.Kind),
	}
	if merged.ThresholdCents != previous.ThresholdCents {
		details["previous_threshold_cents"] = previous.ThresholdCents
		details["current_threshold_cents"] = merged.ThresholdCents
	}
	if merged.TargetCents != previous.TargetCents {
		details["previous_target_cents"] = previous.TargetCents
		details["current_target_cents"] = merged.TargetCents
	}
	if merged.Cutoff != previous.Cutoff {
		details["previous_cutoff"] = previous.Cutoff
		details["current_cutoff"] = merged.Cutoff
	}
	if merged.Enabled != previous.Enabled {
		details["previous_enabled"] = previous.Enabled
		details["current_enabled"] = merged.Enabled
	}
	if !equalAccountIDs(previous.From, merged.From) {
		details["previous_from_accounts"] = append([]AccountID(nil), previous.From...)
		details["current_from_accounts"] = append([]AccountID(nil), merged.From...)
	}
	if merged.To != previous.To {
		details["previous_to_account"] = string(previous.To)
		details["current_to_account"] = string(merged.To)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "autosweep_rule_revised",
		Actor:         "ReviseAutoSweepRule",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      append(append([]AccountID{}, merged.From...), merged.To),
		Details:       details,
	})
	return copyAutoSweepRule(merged), nil
}

// equalAccountIDs reports whether two account lists hold the same
// accounts in the same order.
func equalAccountIDs(a, b []AccountID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
