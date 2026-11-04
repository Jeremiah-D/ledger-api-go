package ledger

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func ptrInt64(v int64) *int64       { return &v }
func ptrString(v string) *string    { return &v }
func ptrBool(v bool) *bool          { return &v }
func ptrAccountID(v AccountID) *AccountID { return &v }

// revisedRuleEvents reads the current day's audit log and returns the
// autosweep_rule_revised events in order.
func revisedRuleEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "autosweep_rule_revised" {
			out = append(out, ev)
		}
	}
	return out
}

// A tightened threshold takes effect on the very next evaluation: a
// balance below the old trigger but above the new one fires.
func TestReviseAutoSweepRuleThresholdTightened(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 60000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(now); len(res) != 1 || res[0].Fired {
		t.Fatalf("run before revision = %+v, want not fired", res)
	}

	revised, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		ThresholdCents: ptrInt64(50000),
		TargetCents:    ptrInt64(40000),
	})
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if revised.ThresholdCents != 50000 || revised.TargetCents != 40000 {
		t.Errorf("revised rule = %+v, want threshold 50000 target 40000", revised)
	}
	if revised.Kind != AutoSweepThreshold || revised.ID != "nightly" {
		t.Errorf("revision moved identity: %+v", revised)
	}

	results := l.RunAutoSweepRules(now.Add(time.Hour))
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("run after revision = %+v, want one clean firing", results)
	}
	if len(results[0].Legs) != 1 || results[0].Legs[0].AmountCents != 20000 {
		t.Errorf("legs = %+v, want one leg of 20000 (60000 - 40000)", results[0].Legs)
	}
	if got := l.Balance("sub-a"); got != 40000 {
		t.Errorf("sub-a balance = %d, want 40000 (new target left behind)", got)
	}
}

// A revised target changes the sweep-down-to level: the next firing
// leaves the new target behind, not the old one.
func TestReviseAutoSweepRuleTarget(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		TargetCents: ptrInt64(30000),
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("results = %+v, want one clean firing", results)
	}
	if len(results[0].Legs) != 1 || results[0].Legs[0].AmountCents != 70000 {
		t.Errorf("legs = %+v, want one leg of 70000 (100000 - 30000)", results[0].Legs)
	}
	if got := l.Balance("sub-a"); got != 30000 {
		t.Errorf("sub-a balance = %d, want 30000 (new target)", got)
	}
}

// A revised cutoff moves the periodic firing time: the rule fires at
// the new cutoff on the same day it would have waited until 23:55.
func TestReviseAutoSweepRuleCutoff(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 10000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "eod", Kind: AutoSweepPeriodic,
		From: []AccountID{"sub-a"}, To: "treasury",
		Cutoff: "23:55",
	})
	noon := time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(noon); len(res) != 1 || res[0].Fired {
		t.Fatalf("run before revision = %+v, want not fired", res)
	}
	if _, err := l.ReviseAutoSweepRule("eod", AutoSweepRuleRevision{
		Cutoff: ptrString("12:00"),
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	res := l.RunAutoSweepRules(noon)
	if len(res) != 1 || !res[0].Fired || res[0].Error != nil {
		t.Fatalf("run after revision = %+v, want one clean firing", res)
	}
	if len(res[0].Legs) != 1 || res[0].Legs[0].AmountCents != 10000 {
		t.Errorf("legs = %+v, want the full 10000 balance", res[0].Legs)
	}
}

// Disabling a rule stops it firing while keeping it listed: the
// operator can re-enable it later without re-registering.
func TestReviseAutoSweepRuleDisable(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		Enabled: ptrBool(false),
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if results := l.RunAutoSweepRules(now); len(results) != 0 {
		t.Errorf("results = %+v, want none for a disabled rule", results)
	}
	listed := l.ListAutoSweepRules()
	if len(listed) != 1 {
		t.Fatalf("listed = %d, want 1 (disabled rule stays registered)", len(listed))
	}
	if listed[0].Rule.Enabled {
		t.Error("listed rule still enabled after disabling revision")
	}
	if got := l.Balance("sub-a"); got != 100000 {
		t.Errorf("sub-a balance = %d, want 100000 (untouched)", got)
	}
	// Re-enabling fires again on the next evaluation.
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		Enabled: ptrBool(true),
	}); err != nil {
		t.Fatalf("re-enable Revise: %v", err)
	}
	if results := l.RunAutoSweepRules(now.Add(time.Hour)); len(results) != 1 || !results[0].Fired {
		t.Errorf("run after re-enable = %+v, want one firing", results)
	}
}

// An illegal merged rule is rejected and the live rule stays exactly
// as it was: a runtime revision never silently normalizes.
func TestReviseAutoSweepRuleInvalidMergeRejected(t *testing.T) {
	base := AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000, Enabled: true,
	}
	cases := []struct {
		name  string
		patch AutoSweepRuleRevision
	}{
		{"target above threshold", AutoSweepRuleRevision{TargetCents: ptrInt64(90000)}},
		{"threshold zeroed", AutoSweepRuleRevision{ThresholdCents: ptrInt64(0)}},
		{"threshold rule given a cutoff", AutoSweepRuleRevision{Cutoff: ptrString("23:55")}},
		{"target below zero", AutoSweepRuleRevision{TargetCents: ptrInt64(-1)}},
		{"sources emptied", AutoSweepRuleRevision{From: []AccountID{}}},
		{"target is a source", AutoSweepRuleRevision{To: ptrAccountID("sub-a")}},
		{"duplicate sources", AutoSweepRuleRevision{From: []AccountID{"sub-a", "sub-a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			registerTestRule(t, l, base)
			versionBefore := l.version
			if _, err := l.ReviseAutoSweepRule("nightly", tc.patch); !errors.Is(err, ErrInvalidAutoSweepRuleRevision) {
				t.Errorf("Revise = %v, want ErrInvalidAutoSweepRuleRevision", err)
			}
			listed := l.ListAutoSweepRules()
			if len(listed) != 1 || !autoSweepRulesEqual(listed[0].Rule, base) {
				t.Errorf("rule after rejected revision = %+v, want unchanged %+v", listed, base)
			}
			if l.version != versionBefore {
				t.Errorf("rejected revision bumped version: %d -> %d", versionBefore, l.version)
			}
		})
	}

	// A periodic rule given a threshold is equally illegal.
	t.Run("periodic given a threshold", func(t *testing.T) {
		l := New()
		registerTestRule(t, l, AutoSweepRule{
			ID: "eod", Kind: AutoSweepPeriodic,
			From: []AccountID{"sub-a"}, To: "treasury",
			Cutoff: "23:55",
		})
		if _, err := l.ReviseAutoSweepRule("eod", AutoSweepRuleRevision{
			ThresholdCents: ptrInt64(1000),
		}); !errors.Is(err, ErrInvalidAutoSweepRuleRevision) {
			t.Errorf("Revise = %v, want ErrInvalidAutoSweepRuleRevision", err)
		}
		if got := l.ListAutoSweepRules()[0].Rule; got.Cutoff != "23:55" || got.ThresholdCents != 0 {
			t.Errorf("rule after rejected revision = %+v, want cutoff 23:55 untouched", got)
		}
	})
}

// Revising an unknown rule ID is a not-found error, not a silent
// no-op.
func TestReviseAutoSweepRuleUnknownID(t *testing.T) {
	l := New()
	if _, err := l.ReviseAutoSweepRule("missing", AutoSweepRuleRevision{
		ThresholdCents: ptrInt64(1000),
	}); err != ErrAutoSweepRuleNotFound {
		t.Errorf("Revise = %v, want ErrAutoSweepRuleNotFound", err)
	}
}

// A same-value revision and an all-nil patch are no-ops: the rule is
// returned unchanged and no autosweep_rule_revised event is written,
// so a retried revision cannot spam the audit trail.
func TestReviseAutoSweepRuleNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	rule := AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	}
	registerTestRule(t, l, rule)
	versionBefore := l.version
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{}); err != nil {
		t.Fatalf("all-nil Revise: %v", err)
	}
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		ThresholdCents: ptrInt64(80000),
		TargetCents:    ptrInt64(50000),
		Enabled:         ptrBool(true),
		To:              ptrAccountID("treasury"),
		From:            []AccountID{"sub-a"},
	}); err != nil {
		t.Fatalf("same-value Revise: %v", err)
	}
	if l.version != versionBefore {
		t.Errorf("no-op revision bumped version: %d -> %d", versionBefore, l.version)
	}
	if events := revisedRuleEvents(t, al); len(events) != 0 {
		t.Errorf("autosweep_rule_revised events = %d, want 0 for no-op revisions", len(events))
	}
}

// A revision preserves lastRunAt and the rule's history: unlike
// Unregister + Register, the periodic rule does not fire twice for
// one cutoff day after being revised.
func TestReviseAutoSweepRulePreservesLastRunAt(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 10000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "eod", Kind: AutoSweepPeriodic,
		From: []AccountID{"sub-a"}, To: "treasury",
		Cutoff: "23:55",
	})
	after := time.Date(2026, 10, 9, 23, 56, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(after); len(res) != 1 || !res[0].Fired {
		t.Fatalf("first run = %+v, want one firing", res)
	}
	before := l.ListAutoSweepRules()
	if len(before) != 1 || before[0].LastRunAt != after {
		t.Fatalf("lastRunAt before revision = %v, want %v", before[0].LastRunAt, after)
	}
	if _, err := l.ReviseAutoSweepRule("eod", AutoSweepRuleRevision{
		Cutoff: ptrString("23:30"),
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	afterRev := l.ListAutoSweepRules()
	if len(afterRev) != 1 || afterRev[0].LastRunAt != after {
		t.Errorf("lastRunAt after revision = %v, want %v (preserved)", afterRev[0].LastRunAt, after)
	}
	// Same day, later run: still not due — the revision did not reset
	// the rule's firing history.
	later := time.Date(2026, 10, 9, 23, 58, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(later); len(res) != 1 || res[0].Fired {
		t.Errorf("second run same day = %+v, want not fired", res)
	}
}

// Revising the source list and the target reroutes the next firing:
// both sources sweep into the new target account.
func TestReviseAutoSweepRuleFromAndTo(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	seedBalance(t, l, "sub-b", 90000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	revised, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		From: []AccountID{"sub-a", "sub-b"},
		To:   ptrAccountID("treasury-2"),
	})
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if !reflect.DeepEqual(revised.From, []AccountID{"sub-a", "sub-b"}) || revised.To != "treasury-2" {
		t.Errorf("revised rule = %+v, want From [sub-a sub-b] To treasury-2", revised)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("results = %+v, want one clean firing", results)
	}
	if len(results[0].Legs) != 2 {
		t.Fatalf("legs = %+v, want two legs (one per source)", results[0].Legs)
	}
	if got := l.Balance("treasury-2"); got != 90000 {
		t.Errorf("treasury-2 balance = %d, want 90000 (50000 + 40000)", got)
	}
	if got := l.Balance("treasury"); got != 0 {
		t.Errorf("treasury balance = %d, want 0 (no longer the target)", got)
	}
}

// A revision racing concurrent runs is race-clean and conserves
// funds: every run sees the old rule or the new rule, never a
// half-revised one, and each firing drains the balance it swept.
func TestReviseAutoSweepRuleConcurrentRuns(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Alternate the target between 40000 and 30000; each
			// revision is valid on its own, and the run loop must
			// see a coherent rule either way.
			target := int64(40000)
			if i%2 == 1 {
				target = 30000
			}
			for j := 0; j < 25; j++ {
				if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
					TargetCents: ptrInt64(target),
				}); err != nil {
					t.Errorf("concurrent Revise: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				l.RunAutoSweepRules(now)
			}
		}()
	}
	wg.Wait()
	// Funds are conserved across every interleaving: whatever fired
	// moved money from sub-a to treasury, nothing vanished.
	if got := l.Balance("sub-a") + l.Balance("treasury"); got != 100000 {
		t.Errorf("sub-a + treasury = %d, want 100000 (conserved)", got)
	}
	final := l.ListAutoSweepRules()
	if len(final) != 1 {
		t.Fatalf("listed = %d, want 1", len(final))
	}
	if final[0].Rule.TargetCents != 30000 && final[0].Rule.TargetCents != 40000 {
		t.Errorf("final target = %d, want 30000 or 40000", final[0].Rule.TargetCents)
	}
}

// The Ctx variant attributes the operator (LG-73); the plain variant
// keeps the entrypoint name.
func TestReviseAutoSweepRuleCtxOperator(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	ctx := ContextWithActor(context.Background(), "ops-lead")
	if _, err := l.ReviseAutoSweepRuleCtx(ctx, "nightly", AutoSweepRuleRevision{
		ThresholdCents: ptrInt64(70000),
	}); err != nil {
		t.Fatalf("ReviseCtx: %v", err)
	}
	if _, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		TargetCents: ptrInt64(40000),
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	events := revisedRuleEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("autosweep_rule_revised events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-lead" {
		t.Errorf("Ctx revision actor = %q, want ops-lead", events[0].Actor)
	}
	if events[1].Actor != "ReviseAutoSweepRule" {
		t.Errorf("plain revision actor = %q, want ReviseAutoSweepRule", events[1].Actor)
	}
	// The first event carries per-changed-field previous/current
	// values; the second carries only its own changed field.
	// Audit details round-trip through JSON, so numbers decode as
	// float64.
	if events[0].Details["previous_threshold_cents"] != float64(80000) ||
		events[0].Details["current_threshold_cents"] != float64(70000) {
		t.Errorf("Ctx event threshold details = %v, want previous 80000 current 70000", events[0].Details)
	}
	if _, ok := events[0].Details["previous_target_cents"]; ok {
		t.Errorf("Ctx event should not carry target details, got %v", events[0].Details)
	}
	if events[1].Details["previous_target_cents"] != float64(50000) ||
		events[1].Details["current_target_cents"] != float64(40000) {
		t.Errorf("plain event target details = %v, want previous 50000 current 40000", events[1].Details)
	}
	if events[0].Details["rule_id"] != "nightly" || events[0].Details["kind"] != "threshold" {
		t.Errorf("event rule_id/kind = %v / %v, want nightly / threshold",
			events[0].Details["rule_id"], events[0].Details["kind"])
	}
	if events[0].TraceID != "nightly" {
		t.Errorf("event trace_id = %q, want the rule ID", events[0].TraceID)
	}
	if events[0].VersionAfter != events[0].VersionBefore {
		t.Errorf("revision bumped version: %d -> %d", events[0].VersionBefore, events[0].VersionAfter)
	}
}

// The returned rule is a copy: mutating its From list must not reach
// the stored rule.
func TestReviseAutoSweepRuleReturnIsCopy(t *testing.T) {
	l := New()
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	revised, err := l.ReviseAutoSweepRule("nightly", AutoSweepRuleRevision{
		TargetCents: ptrInt64(40000),
	})
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	revised.From[0] = "tampered"
	listed := l.ListAutoSweepRules()
	if len(listed) != 1 || len(listed[0].Rule.From) != 1 || listed[0].Rule.From[0] != "sub-a" {
		t.Errorf("stored rule From = %v after mutating the return, want [sub-a]", listed[0].Rule.From)
	}
}
