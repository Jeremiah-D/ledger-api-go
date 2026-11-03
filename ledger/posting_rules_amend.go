package ledger

import (
	"context"
	"fmt"
	"reflect"
)

// Automatic posting-rule set amendment (LG-299).
//
// WithPostingRules (posting_rules.go) installs the automatic posting
// rules at construction — validated fail-fast, panicking on a bad rule
// — and this module is the orthogonal runtime path: an operator
// replaces the whole rule set in place, instead of rebuilding the
// ledger and replaying its journal. Revising in place matters because
// a rebuild is a new ledger — a gap in which the old rules (or none)
// govern derived legs, plus a fresh identity that breaks the audit
// story — while an amendment keeps the same ledger, the same journal,
// and the same audit trail from old rule set to new.
//
// Semantics, deliberately fail-closed throughout, along the LG-292 /
// LG-296 structural revisions:
//
//   - The revision governs only later transfer/capture/sweep rule
//     legs: resolvePostingRuleLegsLocked reads l.postingRules live
//     inside the commit atomic section (transfer.go, hold.go,
//     sweep.go, sweep_fx.go, fx.go), so an event evaluated after the
//     amendment books legs under the new set, while an event
//     evaluated before it keeps the legs it booked. No posting-path
//     change was needed — the revision is clean exactly because the
//     readers were already live. The replacement holds l.mu with the
//     write lock, so a half-installed rule set is never evaluated.
//   - Already-posted rule legs are immutable: a derived leg is a
//     journal entry ("<triggerID>/rule:<name>"), and the journal is
//     append-only — replacing the rule set never rewrites or removes
//     one. Idempotency replay still rebuilds the receipt disclosure
//     from the "/rule:" IDs in the journal, regardless of whether
//     the rule that booked them is still installed.
//   - Validation is strict, like construction but without the panic:
//     every rule reuses the exported ValidatePostingRule (event-kind
//     closed set, leg templates, amount expressions all gated) and
//     the duplicate-name gate reuses ErrPostingRuleDuplicateName.
//     Validate-all-then-install: any illegal or duplicate rule
//     rejects the whole batch and the old rule set is left exactly
//     as it was — the operator fixes the batch and retries, the
//     ledger never runs on a half-installed rule set.
//   - An empty list is legal and clears all rules, consistent with
//     WithPostingRules ("empty list means no rules"): after clearing,
//     transfers post exactly as before, with no rule legs.
//   - Revising to a set deep-equal to the current one (including
//     order — evaluation order is registration order) is a no-op:
//     it returns nil and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, and no journal entry or outbox event fires. It lands
//     one posting_rules_replaced audit event carrying the previous
//     and current rule names and counts. The Ctx variant attributes
//     the operator (LG-73).
//   - The replaced set rides snapshots exactly like the original:
//     snapshot.go already writes, restores (with re-validation),
//     and diffs postingRules. A snapshot round-trip may change
//     registration order (the restore sorts by name), so callers
//     must not depend on post-restore order — only the in-memory
//     order is pinned by the revision.
//   - Dry runs see the live rule set: cloneForDryRunLocked deep-
//     copies the rules (LG-299 dry-run fix), so a DryRunTransfer /
//     DryRunPost on the twin evaluates the same rules the real call
//     would. Before this fix the clone carried no rules at all, so
//     dry runs silently dropped rule legs even for construct-time
//     rules.
//
// Orthogonality: WithPostingRules owns construction-time install,
// resolvePostingRuleLegsLocked owns evaluation, PostingRules owns
// the read, and this module owns only the runtime replacement of
// the set. There is no HTTP endpoint for the revision (per
// LG-241/242): the rule set is in-process structural config.

// validatePostingRuleSet validates every rule in the batch and
// returns a deep copy (each rule's Legs re-sliced) safe to install:
// the caller's slice and leg backing arrays can never alias the
// ledger's installed set. Any illegal rule or duplicate name fails
// the whole batch with the wrapped validation error.
func validatePostingRuleSet(rules []PostingRule) ([]PostingRule, error) {
	seen := make(map[string]bool, len(rules))
	out := make([]PostingRule, 0, len(rules))
	for _, r := range rules {
		nr, err := ValidatePostingRule(r)
		if err != nil {
			return nil, err
		}
		if seen[nr.Name] {
			return nil, fmt.Errorf("%w: %q", ErrPostingRuleDuplicateName, nr.Name)
		}
		seen[nr.Name] = true
		legs := make([]PostingRuleLeg, len(nr.Legs))
		copy(legs, nr.Legs)
		nr.Legs = legs
		out = append(out, nr)
	}
	return out, nil
}

// clonePostingRules deep-copies a rule set (slice + each rule's
// Legs), following the snapshot.go restore shape, so the copy can
// never alias the source's backing arrays.
func clonePostingRules(rules []PostingRule) []PostingRule {
	if rules == nil {
		return nil
	}
	out := make([]PostingRule, len(rules))
	for i, r := range rules {
		r.Legs = append([]PostingRuleLeg(nil), r.Legs...)
		out[i] = r
	}
	return out
}

// postingRuleNames returns the rule names in registration order, for
// the audit disclosure.
func postingRuleNames(rules []PostingRule) []string {
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

// postingRulesEqual reports whether two rule sets are deep-equal,
// including registration order. Empty and nil both mean "no rules".
func postingRulesEqual(a, b []PostingRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// SetPostingRules replaces the installed automatic posting rules in
// place. Only transfer/capture/sweep events evaluated after the call
// book legs under the new set; already-posted rule legs are untouched.
// Any illegal rule or duplicate name rejects the whole batch with the
// validation error and leaves the old rule set unchanged. An empty
// list clears all rules. Revising to a set deep-equal to the current
// one (including order) is a no-op with no audit event. The revision
// is audited as posting_rules_replaced.
func (l *Ledger) SetPostingRules(rules []PostingRule) error {
	return l.SetPostingRulesCtx(context.Background(), rules)
}

// SetPostingRulesCtx is SetPostingRules with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetPostingRulesCtx(ctx context.Context, rules []PostingRule) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	validated, err := validatePostingRuleSet(rules)
	if err != nil {
		return err
	}
	if postingRulesEqual(validated, l.postingRules) {
		return nil
	}
	previous := l.postingRules
	l.postingRules = validated
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "posting_rules_replaced",
		Actor:         "SetPostingRules",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_rule_names": postingRuleNames(previous),
			"current_rule_names":  postingRuleNames(validated),
			"previous_rule_count": len(previous),
			"current_rule_count":  len(validated),
		},
	})
	return nil
}
