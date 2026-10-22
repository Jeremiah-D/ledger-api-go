package ledger

import (
	"strings"
	"testing"
	"time"
)

// registerTestRule registers a valid enabled rule for tests, failing the
// test on error.
func registerTestRule(t *testing.T, l *Ledger, rule AutoSweepRule) {
	t.Helper()
	rule.Enabled = true
	if err := l.RegisterAutoSweepRule(rule); err != nil {
		t.Fatalf("RegisterAutoSweepRule(%+v): %v", rule, err)
	}
}

func TestAutoSweepRuleValidation(t *testing.T) {
	base := AutoSweepRule{
		ID:             "nightly",
		Kind:           AutoSweepThreshold,
		From:           []AccountID{"sub-a"},
		To:             "treasury",
		ThresholdCents: 80000,
		TargetCents:    50000,
	}
	cases := []struct {
		name    string
		mutate  func(*AutoSweepRule)
		wantErr error
	}{
		{"empty ID", func(r *AutoSweepRule) { r.ID = "" }, ErrEmptyAutoSweepRuleID},
		{"bad ID charset", func(r *AutoSweepRule) { r.ID = "nightly/sweep" }, ErrInvalidAutoSweepRuleID},
		{"unknown kind", func(r *AutoSweepRule) { r.Kind = "weekly" }, ErrInvalidAutoSweepRuleKind},
		{"no sources", func(r *AutoSweepRule) { r.From = nil }, ErrEmptyAutoSweepRuleSources},
		{"empty source", func(r *AutoSweepRule) { r.From = []AccountID{""} }, ErrEmptyAutoSweepRuleSource},
		{"no target", func(r *AutoSweepRule) { r.To = "" }, ErrEmptyAutoSweepRuleTarget},
		{"target is source", func(r *AutoSweepRule) { r.To = "sub-a" }, ErrAutoSweepRuleTargetIsSource},
		{"duplicate source", func(r *AutoSweepRule) { r.From = []AccountID{"sub-a", "sub-a"} }, ErrDuplicateAutoSweepRuleSource},
		{"threshold not positive", func(r *AutoSweepRule) { r.ThresholdCents = 0 }, ErrInvalidAutoSweepRuleParams},
		{"target above threshold", func(r *AutoSweepRule) { r.TargetCents = 90000 }, ErrInvalidAutoSweepRuleParams},
		{"negative target", func(r *AutoSweepRule) { r.TargetCents = -1 }, ErrInvalidAutoSweepRuleParams},
		{"threshold with cutoff", func(r *AutoSweepRule) { r.Cutoff = "23:55" }, ErrInvalidAutoSweepRuleParams},
		{"target-balance with threshold", func(r *AutoSweepRule) {
			r.Kind = AutoSweepTargetBalance
			r.ThresholdCents = 100
		}, ErrInvalidAutoSweepRuleParams},
		{"target-balance negative target", func(r *AutoSweepRule) {
			r.Kind = AutoSweepTargetBalance
			r.ThresholdCents = 0
			r.TargetCents = -1
		}, ErrInvalidAutoSweepRuleParams},
		{"periodic without cutoff", func(r *AutoSweepRule) {
			r.Kind = AutoSweepPeriodic
			r.ThresholdCents = 0
			r.TargetCents = 0
		}, ErrInvalidAutoSweepRuleParams},
		{"periodic bad cutoff", func(r *AutoSweepRule) {
			r.Kind = AutoSweepPeriodic
			r.ThresholdCents = 0
			r.TargetCents = 0
			r.Cutoff = "25:00"
		}, ErrInvalidAutoSweepRuleParams},
		{"periodic with threshold", func(r *AutoSweepRule) {
			r.Kind = AutoSweepPeriodic
			r.TargetCents = 0
			r.Cutoff = "23:55"
		}, ErrInvalidAutoSweepRuleParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			rule := base
			tc.mutate(&rule)
			if err := l.RegisterAutoSweepRule(rule); err != tc.wantErr {
				t.Errorf("RegisterAutoSweepRule() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAutoSweepRuleDuplicateID(t *testing.T) {
	l := New()
	rule := AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000, Enabled: true,
	}
	if err := l.RegisterAutoSweepRule(rule); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := l.RegisterAutoSweepRule(rule); err != ErrDuplicateAutoSweepRule {
		t.Errorf("second Register = %v, want ErrDuplicateAutoSweepRule", err)
	}
}

func TestAutoSweepThresholdFires(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	preview, err := l.PreviewAutoSweepRule("nightly", now)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.Eligible {
		t.Fatalf("preview eligible = false, reason %q", preview.Reason)
	}
	if len(preview.Legs) != 1 || preview.Legs[0].AmountCents != 50000 {
		t.Fatalf("preview legs = %+v, want one leg of 50000", preview.Legs)
	}

	results := l.RunAutoSweepRules(now)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if !res.Fired || res.Error != nil {
		t.Fatalf("result = %+v, want fired without error", res)
	}
	if !strings.HasPrefix(res.SweepID, "autosweep/nightly/") {
		t.Errorf("sweep ID = %q, want autosweep/nightly/ prefix", res.SweepID)
	}
	if got := l.Balance("sub-a"); got != 50000 {
		t.Errorf("sub-a balance = %d, want 50000 (target left behind)", got)
	}
	if got := l.Balance("treasury"); got != 50000 {
		t.Errorf("treasury balance = %d, want 50000", got)
	}

	// The rule is no longer eligible: the balance sits at the target.
	again := l.RunAutoSweepRules(now.Add(time.Hour))
	if len(again) != 1 || again[0].Fired {
		t.Errorf("second run = %+v, want not fired", again)
	}
	if got := l.Balance("sub-a"); got != 50000 {
		t.Errorf("sub-a balance after second run = %d, want 50000", got)
	}
}

func TestAutoSweepThresholdBelowTrigger(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 79999)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	preview, err := l.PreviewAutoSweepRule("nightly", now)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if preview.Eligible {
		t.Errorf("preview eligible = true below the trigger")
	}
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || results[0].Fired {
		t.Errorf("results = %+v, want single not-fired result", results)
	}
	if got := l.Balance("sub-a"); got != 79999 {
		t.Errorf("sub-a balance = %d, want 79999 (untouched)", got)
	}
}

func TestAutoSweepTargetBalance(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 30000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "float", Kind: AutoSweepTargetBalance,
		From: []AccountID{"sub-a"}, To: "treasury",
		TargetCents: 10000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("results = %+v, want one clean firing", results)
	}
	if len(results[0].Legs) != 1 || results[0].Legs[0].AmountCents != 20000 {
		t.Errorf("legs = %+v, want one leg of 20000", results[0].Legs)
	}
	if got := l.Balance("sub-a"); got != 10000 {
		t.Errorf("sub-a balance = %d, want 10000 (target float kept)", got)
	}
}

func TestAutoSweepPeriodicDailyCutoff(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 10000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "eod", Kind: AutoSweepPeriodic,
		From: []AccountID{"sub-a"}, To: "treasury",
		Cutoff: "23:55",
	})

	before := time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(before); len(res) != 1 || res[0].Fired {
		t.Fatalf("run before cutoff = %+v, want not fired", res)
	}

	after := time.Date(2026, 10, 9, 23, 56, 0, 0, time.UTC)
	res := l.RunAutoSweepRules(after)
	if len(res) != 1 || !res[0].Fired || res[0].Error != nil {
		t.Fatalf("run after cutoff = %+v, want one clean firing", res)
	}
	if len(res[0].Legs) != 1 || res[0].Legs[0].AmountCents != 10000 {
		t.Errorf("legs = %+v, want the full 10000 balance", res[0].Legs)
	}

	// Same day, second run: already ran after today's cutoff.
	again := time.Date(2026, 10, 9, 23, 58, 0, 0, time.UTC)
	if res := l.RunAutoSweepRules(again); len(res) != 1 || res[0].Fired {
		t.Errorf("second run same day = %+v, want not fired", res)
	}

	// Next day after the cutoff: fires again (new balances swept).
	seedBalance(t, l, "sub-a", 5000)
	nextDay := time.Date(2026, 10, 10, 23, 56, 0, 0, time.UTC)
	res = l.RunAutoSweepRules(nextDay)
	if len(res) != 1 || !res[0].Fired || res[0].Error != nil {
		t.Fatalf("next-day run = %+v, want one clean firing", res)
	}
	if got := l.Balance("treasury"); got != 15000 {
		t.Errorf("treasury balance = %d, want 15000", got)
	}
}

func TestAutoSweepPreviewZeroSideEffects(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	versionBefore := l.version
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	preview, err := l.PreviewAutoSweepRule("nightly", now)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.Eligible || len(preview.Legs) != 1 {
		t.Fatalf("preview = %+v, want eligible with one leg", preview)
	}
	if l.version != versionBefore {
		t.Errorf("preview bumped version: %d -> %d", versionBefore, l.version)
	}
	if got := l.Balance("sub-a"); got != 100000 {
		t.Errorf("preview moved funds: sub-a = %d", got)
	}
	if _, err := l.PreviewAutoSweepRule("missing", now); err != ErrAutoSweepRuleNotFound {
		t.Errorf("preview of unknown rule = %v, want ErrAutoSweepRuleNotFound", err)
	}
}

func TestAutoSweepFrozenSourceRejected(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	l.Freeze("sub-a")
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	// The rule fires (it is eligible) but the sweep gate rejects it:
	// nothing is booked.
	if !res.Fired {
		t.Fatalf("result = %+v, want fired-with-error", res)
	}
	if res.Error != ErrAccountFrozen {
		t.Errorf("result error = %v, want ErrAccountFrozen", res.Error)
	}
	if got := l.Balance("sub-a"); got != 100000 {
		t.Errorf("sub-a balance = %d, want 100000 (nothing booked)", got)
	}
	if got := l.Balance("treasury"); got != 0 {
		t.Errorf("treasury balance = %d, want 0", got)
	}
}

func TestAutoSweepDisabledRuleNeverFires(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	rule := AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
		Enabled: false,
	}
	if err := l.RegisterAutoSweepRule(rule); err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if results := l.RunAutoSweepRules(now); len(results) != 0 {
		t.Errorf("results = %+v, want none for a disabled rule", results)
	}
	preview, err := l.PreviewAutoSweepRule("nightly", now)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.Eligible {
		t.Errorf("disabled rule preview eligible = false; preview ignores Enabled (firing does not)")
	}
}

func TestAutoSweepUnregister(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	if !l.UnregisterAutoSweepRule("nightly") {
		t.Fatal("Unregister returned false for a registered rule")
	}
	if l.UnregisterAutoSweepRule("nightly") {
		t.Error("second Unregister returned true, want false")
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if results := l.RunAutoSweepRules(now); len(results) != 0 {
		t.Errorf("results = %+v, want none after unregister", results)
	}
	if got := l.Balance("sub-a"); got != 100000 {
		t.Errorf("sub-a balance = %d, want 100000", got)
	}
}

func TestAutoSweepMultiCurrencyPerCurrencyTrigger(t *testing.T) {
	l := New()
	seedBalanceIn(t, l, "sub-a", 100000, "USD")
	seedBalanceIn(t, l, "sub-a", 10000, "EUR")
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	preview, err := l.PreviewAutoSweepRule("nightly", now)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	// Only USD is above the trigger; EUR stays put.
	if len(preview.Legs) != 1 || preview.Legs[0].Currency != "USD" || preview.Legs[0].AmountCents != 50000 {
		t.Fatalf("preview legs = %+v, want one USD leg of 50000", preview.Legs)
	}
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("results = %+v, want one clean firing", results)
	}
	if got := l.BalanceIn("sub-a", "EUR"); got != 10000 {
		t.Errorf("sub-a EUR balance = %d, want 10000 (untouched)", got)
	}
}

func TestAutoSweepListSorted(t *testing.T) {
	l := New()
	for _, id := range []string{"zulu", "alpha", "mike"} {
		registerTestRule(t, l, AutoSweepRule{
			ID: id, Kind: AutoSweepTargetBalance,
			From: []AccountID{"sub-a"}, To: "treasury",
			TargetCents: 1000,
		})
	}
	got := l.ListAutoSweepRules()
	if len(got) != 3 {
		t.Fatalf("listed = %d, want 3", len(got))
	}
	for i, want := range []string{"alpha", "mike", "zulu"} {
		if got[i].Rule.ID != want {
			t.Errorf("listed[%d].ID = %q, want %q", i, got[i].Rule.ID, want)
		}
	}
}

// A rule firing lands in the structured audit log's hash chain as
// "autosweep_triggered" (rule id, kind, sweep id, legs), next to the
// sweep's own "sweep" event.
func TestAutoSweepTriggerEntersAuditChain(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	results := l.RunAutoSweepRules(now)
	if len(results) != 1 || !results[0].Fired || results[0].Error != nil {
		t.Fatalf("results = %+v, want one clean firing", results)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Audit event timestamps are stamped at write time (the rule's
	// evaluation instant is fixed for determinism), so read the
	// current day's log file, not the rule's `now` day.
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var triggered, sweep *AuditEvent
	for i := range events {
		switch events[i].Op {
		case "autosweep_triggered":
			triggered = &events[i]
		case "sweep":
			sweep = &events[i]
		}
	}
	if triggered == nil {
		t.Fatal("no autosweep_triggered event in the audit log")
	}
	if sweep == nil {
		t.Fatal("no sweep event in the audit log")
	}
	if triggered.Details["rule_id"] != "nightly" {
		t.Errorf("triggered rule_id = %v, want nightly", triggered.Details["rule_id"])
	}
	if triggered.Details["kind"] != "threshold" {
		t.Errorf("triggered kind = %v, want threshold", triggered.Details["kind"])
	}
	if triggered.Details["sweep_id"] != results[0].SweepID {
		t.Errorf("triggered sweep_id = %v, want %q", triggered.Details["sweep_id"], results[0].SweepID)
	}
	if triggered.TraceID != results[0].SweepID {
		t.Errorf("triggered trace_id = %q, want the sweep ID", triggered.TraceID)
	}
	if triggered.VersionAfter <= triggered.VersionBefore {
		t.Errorf("triggered event did not bracket a version bump: %d -> %d",
			triggered.VersionBefore, triggered.VersionAfter)
	}
}

// A concurrent RunAutoSweepRules pass is race-clean: the rule fires at
// most once because the first firing drains the balance below the
// trigger.
func TestAutoSweepConcurrentRuns(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 100000)
	registerTestRule(t, l, AutoSweepRule{
		ID: "nightly", Kind: AutoSweepThreshold,
		From: []AccountID{"sub-a"}, To: "treasury",
		ThresholdCents: 80000, TargetCents: 50000,
	})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	done := make(chan []AutoSweepRunResult, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- l.RunAutoSweepRules(now) }()
	}
	fired := 0
	for i := 0; i < 8; i++ {
		for _, r := range <-done {
			if r.Fired && r.Error == nil {
				fired++
			}
		}
	}
	if fired != 1 {
		t.Errorf("clean firings = %d, want exactly 1", fired)
	}
	if got := l.Balance("treasury"); got != 50000 {
		t.Errorf("treasury balance = %d, want 50000 (single firing)", got)
	}
}
