package ledger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// taxWithholdRule is the replacement rule used across the amendment
// tests: a fixed 100-cent withholding on every transfer, debited to
// the tax account and credited by the payer.
func taxWithholdRule() PostingRule {
	return PostingRule{
		Name:      "tax-withhold",
		EventKind: PostingEventTransfer,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "tax-withhold-account", Side: "debit", AmountExpr: "fixed:100"},
		},
	}
}

// zetaFeeRule is a second transfer rule whose name sorts after
// "tax-withhold": registering [zeta, tax] pins in-memory order, while
// a snapshot round-trip restores name order [tax, zeta].
func zetaFeeRule() PostingRule {
	return PostingRule{
		Name:      "zeta-fee",
		EventKind: PostingEventTransfer,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "zeta-fee-revenue", Side: "debit", AmountExpr: "bps:100"},
		},
	}
}

func postingRulesAmendTestLedger(t *testing.T, opts ...Option) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	base := append([]Option{WithAuditLog(al), WithPostingRules([]PostingRule{channelFeeRule()})}, opts...)
	l := New(base...)
	seedBalance(t, l, "merchant-alice", 100000)
	return l, al
}

func mustPostTransfer(t *testing.T, l *Ledger, id string, cents int64) TransferReceipt {
	t.Helper()
	rcpt, err := l.PostTransfer(Transfer{
		ID:          id,
		From:        "merchant-alice",
		To:          "platform",
		AmountCents: cents,
	})
	if err != nil {
		t.Fatalf("PostTransfer(%s): %v", id, err)
	}
	return rcpt
}

func ruleLegNames(rcpt TransferReceipt) []string {
	names := make([]string, 0, len(rcpt.RuleLegs))
	for _, info := range rcpt.RuleLegs {
		names = append(names, info.Rule)
	}
	return names
}

func installedRuleNames(l *Ledger) []string {
	rules := l.PostingRules()
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
	}
	return names
}

func dryRunHasLeg(legs []DryRunLeg, entryID string) bool {
	for _, leg := range legs {
		if leg.EntryID == entryID {
			return true
		}
	}
	return false
}

// auditStringSlice reads a []string detail back through the JSON
// round-trip of the audit log (which decodes it as []any).
func auditStringSlice(t *testing.T, ev *AuditEvent, key string) []string {
	t.Helper()
	raw, ok := ev.Details[key].([]any)
	if !ok {
		t.Fatalf("audit detail %q = %T (%v), want []any of strings", key, ev.Details[key], ev.Details[key])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("audit detail %q element = %T, want string", key, v)
		}
		out = append(out, s)
	}
	return out
}

func TestSetPostingRulesReplacementFiresNewRule(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	// The construct-time rule fires before the revision.
	rcpt := mustPostTransfer(t, l, "settle-1", 20000)
	if got := ruleLegNames(rcpt); len(got) != 1 || got[0] != "channel-fee" {
		t.Fatalf("rule legs before revision = %v, want [channel-fee]", got)
	}

	before := l.version
	if err := l.SetPostingRules([]PostingRule{taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRules: %v", err)
	}
	if l.version != before {
		t.Errorf("version = %d after amendment, want %d (structural: no version bump)", l.version, before)
	}
	if got := installedRuleNames(l); len(got) != 1 || got[0] != "tax-withhold" {
		t.Fatalf("installed rules = %v, want [tax-withhold]", got)
	}

	// The new rule fires; the old rule stops firing.
	rcpt = mustPostTransfer(t, l, "settle-2", 20000)
	if got := ruleLegNames(rcpt); len(got) != 1 || got[0] != "tax-withhold" {
		t.Fatalf("rule legs after revision = %v, want [tax-withhold]", got)
	}
	info := rcpt.RuleLegs[0]
	if info.EntryID != "settle-2/rule:tax-withhold" {
		t.Errorf("entry ID = %q, want settle-2/rule:tax-withhold", info.EntryID)
	}
	if info.AmountCents != 100 {
		t.Errorf("amount = %d, want 100 (fixed:100)", info.AmountCents)
	}

	// The already-posted leg from the old rule is untouched: the
	// journal is append-only, a revision never rewrites it.
	if _, ok := l.entries["settle-1/rule:channel-fee"]; !ok {
		t.Error("settle-1/rule:channel-fee vanished after the revision (posted legs are immutable)")
	}
}

func TestSetPostingRulesNoOpWritesNoAudit(t *testing.T) {
	l, al := postingRulesAmendTestLedger(t)

	// Revising to the identical set is a no-op.
	if err := l.SetPostingRules([]PostingRule{channelFeeRule()}); err != nil {
		t.Fatalf("SetPostingRules(identical) = %v, want nil no-op", err)
	}
	// Verify the no-op with a two-rule set installed in the same order.
	if err := l.SetPostingRules([]PostingRule{channelFeeRule(), taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRules(two rules): %v", err)
	}
	if err := l.SetPostingRules([]PostingRule{channelFeeRule(), taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRules(same two rules) = %v, want nil no-op", err)
	}
	// Order is part of the identity: swapping registration order is
	// a real revision, not a no-op.
	if err := l.SetPostingRules([]PostingRule{taxWithholdRule(), channelFeeRule()}); err != nil {
		t.Fatalf("SetPostingRules(swapped order): %v", err)
	}
	if got := installedRuleNames(l); len(got) != 2 || got[0] != "tax-withhold" || got[1] != "channel-fee" {
		t.Fatalf("installed rules = %v, want [tax-withhold channel-fee]", got)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "posting_rules_replaced"); n != 2 {
		t.Errorf("posting_rules_replaced events = %d, want 2 (no-ops write none)", n)
	}
}

func TestSetPostingRulesRejectsIllegalBatch(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	badKind := channelFeeRule()
	badKind.Name = "bad-kind"
	badKind.EventKind = "post"
	noLegs := channelFeeRule()
	noLegs.Name = "no-legs"
	noLegs.Legs = nil
	cases := []struct {
		name  string
		rules []PostingRule
		err   error
	}{
		{"bad event kind", []PostingRule{badKind}, ErrPostingRuleBadKind},
		{"zero legs", []PostingRule{noLegs}, ErrPostingRuleNoLegs},
		{"good rule + bad rule rejects whole batch", []PostingRule{taxWithholdRule(), badKind}, ErrPostingRuleBadKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := l.SetPostingRules(tc.rules); !errors.Is(err, tc.err) {
				t.Fatalf("SetPostingRules = %v, want %v", err, tc.err)
			}
		})
	}

	// The rejected batches leave the old set exactly as it was —
	// field-level and behavioral.
	if got := installedRuleNames(l); len(got) != 1 || got[0] != "channel-fee" {
		t.Fatalf("installed rules after rejections = %v, want [channel-fee]", got)
	}
	rcpt := mustPostTransfer(t, l, "settle-after-reject", 20000)
	if got := ruleLegNames(rcpt); len(got) != 1 || got[0] != "channel-fee" {
		t.Fatalf("rule legs after rejections = %v, want [channel-fee]", got)
	}
}

func TestSetPostingRulesRejectsDuplicateName(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	dup := taxWithholdRule()
	dup.EventKind = PostingEventCapture
	if err := l.SetPostingRules([]PostingRule{taxWithholdRule(), dup}); !errors.Is(err, ErrPostingRuleDuplicateName) {
		t.Fatalf("SetPostingRules(duplicate name) = %v, want ErrPostingRuleDuplicateName", err)
	}
	if got := installedRuleNames(l); len(got) != 1 || got[0] != "channel-fee" {
		t.Fatalf("installed rules after duplicate rejection = %v, want [channel-fee]", got)
	}
}

func TestSetPostingRulesEmptyClears(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	if err := l.SetPostingRules(nil); err != nil {
		t.Fatalf("SetPostingRules(nil): %v", err)
	}
	if got := installedRuleNames(l); len(got) != 0 {
		t.Fatalf("installed rules after clear = %v, want []", got)
	}
	rcpt := mustPostTransfer(t, l, "settle-cleared", 20000)
	if len(rcpt.RuleLegs) != 0 {
		t.Fatalf("rule legs after clearing = %v, want none", ruleLegNames(rcpt))
	}

	// Clearing an already-empty set is a no-op, and an explicit
	// empty slice clears exactly like nil.
	if err := l.SetPostingRules([]PostingRule{}); err != nil {
		t.Fatalf("SetPostingRules(empty) on cleared ledger: %v", err)
	}
}

func TestSetPostingRulesSnapshotRoundTrip(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	// Register in non-name order: the snapshot restores name order,
	// so the test pins the set (not post-restore order).
	if err := l.SetPostingRules([]PostingRule{zetaFeeRule(), taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRules: %v", err)
	}
	if got := installedRuleNames(l); len(got) != 2 || got[0] != "zeta-fee" || got[1] != "tax-withhold" {
		t.Fatalf("in-memory order = %v, want [zeta-fee tax-withhold]", got)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	restoredNames := installedRuleNames(restored)
	if len(restoredNames) != 2 || restoredNames[0] != "tax-withhold" || restoredNames[1] != "zeta-fee" {
		t.Fatalf("restored rules = %v, want name order [tax-withhold zeta-fee]", restoredNames)
	}

	// The restored ledger fires the replaced set: a transfer books
	// both rules' legs (zeta bps:100 on 20000 = 200, tax fixed 100).
	// The seed balance rides the snapshot, so no re-seeding: the
	// journal entry "seed-merchant-alice" is already there.
	rcpt, err := restored.PostTransfer(Transfer{
		ID: "settle-restored", From: "merchant-alice", To: "platform", AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("PostTransfer on restored ledger: %v", err)
	}
	if got := ruleLegNames(rcpt); len(got) != 2 {
		t.Fatalf("rule legs on restored ledger = %v, want both replaced rules", got)
	}
}

func TestSetPostingRulesDryRunSeesLiveRules(t *testing.T) {
	l, _ := postingRulesAmendTestLedger(t)

	// The DRY-RUN FIX: even the construct-time rule must show up in
	// a dry run — before the fix the clone carried no rules at all.
	res, err := l.DryRunTransfer(Transfer{
		ID: "dry-1", From: "merchant-alice", To: "platform", AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("DryRunTransfer (construct-time rule): %v", err)
	}
	if !res.WouldSucceed {
		t.Fatal("DryRunTransfer: want WouldSucceed")
	}
	if !dryRunHasLeg(res.Legs, "dry-1/rule:channel-fee") {
		t.Fatalf("dry-run legs = %v, want dry-1/rule:channel-fee", res.Legs)
	}

	// After replacement the dry run shows the new rule's leg and not
	// the old rule's.
	if err := l.SetPostingRules([]PostingRule{taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRules: %v", err)
	}
	res, err = l.DryRunTransfer(Transfer{
		ID: "dry-2", From: "merchant-alice", To: "platform", AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("DryRunTransfer (replaced rule): %v", err)
	}
	if !dryRunHasLeg(res.Legs, "dry-2/rule:tax-withhold") {
		t.Fatalf("dry-run legs = %v, want dry-2/rule:tax-withhold", res.Legs)
	}
	if dryRunHasLeg(res.Legs, "dry-2/rule:channel-fee") {
		t.Fatalf("dry-run legs = %v, unexpected channel-fee leg after replacement", res.Legs)
	}

	// After clearing, the dry run has no rule leg at all.
	if err := l.SetPostingRules(nil); err != nil {
		t.Fatalf("SetPostingRules(nil): %v", err)
	}
	res, err = l.DryRunTransfer(Transfer{
		ID: "dry-3", From: "merchant-alice", To: "platform", AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("DryRunTransfer (cleared): %v", err)
	}
	for _, leg := range res.Legs {
		if strings.Contains(leg.EntryID, "/rule:") {
			t.Fatalf("dry-run legs = %v, want no rule leg after clearing", res.Legs)
		}
	}

	// The dry runs booked nothing on the live ledger.
	for _, id := range []string{"dry-1", "dry-2", "dry-3"} {
		if _, ok := l.entries[id]; ok {
			t.Errorf("live ledger has entry %q after dry runs (dry runs must not book)", id)
		}
	}
}

func TestSetPostingRulesCtxRecordsOperator(t *testing.T) {
	l, al := postingRulesAmendTestLedger(t)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetPostingRulesCtx(ctx, []PostingRule{taxWithholdRule()}); err != nil {
		t.Fatalf("SetPostingRulesCtx: %v", err)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "posting_rules_replaced")
	if ev == nil {
		t.Fatal("no posting_rules_replaced event")
	}
	if ev.Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", ev.Actor)
	}
	if got := auditStringSlice(t, ev, "previous_rule_names"); len(got) != 1 || got[0] != "channel-fee" {
		t.Errorf("previous_rule_names = %v, want [channel-fee]", got)
	}
	if got := auditStringSlice(t, ev, "current_rule_names"); len(got) != 1 || got[0] != "tax-withhold" {
		t.Errorf("current_rule_names = %v, want [tax-withhold]", got)
	}
	if ev.Details["previous_rule_count"] != float64(1) || ev.Details["current_rule_count"] != float64(1) {
		t.Errorf("rule counts = %+v, want 1 -> 1", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version %d -> %d, want unchanged (structural amendment)", ev.VersionBefore, ev.VersionAfter)
	}
}
