package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInstallmentPlanRuleValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanRule("ip-unknown", InstallmentWeekly); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-rule")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-rule")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanRule("ip-rule", InstallmentWeekly); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-rule")
	if _, err := l2.SettleInstallmentEarly("ip-rule", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanRule("ip-rule", InstallmentWeekly); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Invalid rules fail with the creation gate's error, and every
	// rejection leaves the original plan exactly as it was.
	l3 := New()
	orig3 := mustOpenInstallmentPlan(t, l3, "ip-rule")
	for _, bad := range []string{"yearly", "fortnightly", "", "Monthly"} {
		if _, err := l3.AmendInstallmentPlanRule("ip-rule", bad); !errors.Is(err, ErrInvalidInstallmentRule) {
			t.Errorf("rule %q: err = %v, want ErrInvalidInstallmentRule", bad, err)
		}
		got, _ := l3.InstallmentPlan("ip-rule")
		if !reflect.DeepEqual(got, orig3) {
			t.Errorf("plan after rule %q rejection = %+v, want %+v (untouched)", bad, got, orig3)
		}
	}

	// Amending to the current rule is a no-op, not a change.
	if got, err := l3.AmendInstallmentPlanRule("ip-rule", InstallmentMonthly); err != nil || !reflect.DeepEqual(got, orig3) {
		t.Errorf("same-rule amendment = %+v, %v, want %+v, nil", got, err, orig3)
	}
}

func TestAmendInstallmentPlanRuleSweepHonorsNewRule(t *testing.T) {
	// The new rule governs unposted periods from the next sweep:
	// period 1 posts monthly (due 2026-01-01), the rule moves to
	// weekly, and the next sweep posts periods 2 and 3 on weekly
	// due dates (2026-01-08, 2026-01-15) — under the old monthly
	// rule neither would be due until February.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-rule-sweep")
	res, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(res) != 1 || res[0].Period != 1 || res[0].AmountCents != 333 {
		t.Fatalf("first sweep = %+v, want one period 1 of 333", res)
	}
	// Baseline for the untouched-fields check: the plan as it stands
	// after period 1 posted, before the amendment.
	orig, _ := l.InstallmentPlan("ip-rule-sweep")

	amended, err := l.AmendInstallmentPlanRule("ip-rule-sweep", InstallmentWeekly)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanRule: %v", err)
	}
	if amended.Rule != InstallmentWeekly {
		t.Errorf("amended rule = %q, want %q", amended.Rule, InstallmentWeekly)
	}
	// Only the rule moves: principal, period count, first due date,
	// currency, legs, status, and posted periods are untouched.
	amended.Rule = orig.Rule
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended plan = %+v, want %+v except rule", amended, orig)
	}
	// The amendment itself posts nothing.
	if got := len(l.Entries()); got != 1 {
		t.Fatalf("entries after amendment = %d, want 1 (amendment posts nothing)", got)
	}

	res, err = l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("second sweep posted %d periods, want 2 (weekly periods 2 and 3)", len(res))
	}
	wantDue := []time.Time{
		time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
	}
	wantAmounts := []int64{333, 334}
	for i, r := range res {
		if r.Period != i+2 || !r.DueAt.Equal(wantDue[i]) || r.AmountCents != wantAmounts[i] || !r.Posted {
			t.Errorf("sweep result %d = %+v, want period %d due %v amount %d posted", i, r, i+2, wantDue[i], wantAmounts[i])
		}
	}

	// Period 1's journaled row is untouched: same due date, same
	// amount, same deterministic entry ID.
	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	first := entries[0]
	if first.ID != "installment/ip-rule-sweep/1" || first.AmountCents != 333 ||
		!first.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period 1 row = %+v, want installment/ip-rule-sweep/1 of 333 as of 2026-01-01", first)
	}
	plan, _ := l.InstallmentPlan("ip-rule-sweep")
	if plan.Status != InstallmentCompleted || plan.PostedPeriods != 3 {
		t.Errorf("plan = %+v, want completed with 3 posted periods", plan)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000", got)
	}
	if got := l.BalanceIn("buyer", "USD"); got != -1000 {
		t.Errorf("buyer balance = %d, want -1000", got)
	}
}

func TestAmendInstallmentPlanRuleOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan may
	// be amended, and the new rule governs the catch-up sweep.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-rule-overdue")
	// Post period 1, then freeze the payer so period 2 fails.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-rule-overdue")
	if plan.Status != InstallmentOverdue {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentOverdue)
	}
	// Amend while overdue: monthly -> daily moves period 2's due
	// date from 2026-02-01 to 2026-01-02 and period 3's from
	// 2026-03-01 to 2026-01-03.
	amended, err := l.AmendInstallmentPlanRule("ip-rule-overdue", InstallmentDaily)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanRule overdue: %v", err)
	}
	if amended.Rule != InstallmentDaily || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = rule %q status %q, want daily overdue", amended.Rule, amended.Status)
	}
	// Unfreeze: the catch-up sweep posts periods 2 and 3 on daily
	// due dates and recovers the plan.
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("catch-up sweep posted %d periods, want 2", len(res))
	}
	if !res[0].DueAt.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) ||
		!res[1].DueAt.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("catch-up due dates = %v, %v, want 2026-01-02 and 2026-01-03", res[0].DueAt, res[1].DueAt)
	}
	plan, _ = l.InstallmentPlan("ip-rule-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
}

func TestAmendInstallmentPlanRuleAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-rule-audit")
	// Post one period so the audit carries a posted-period count.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// The no-op revision emits nothing.
	if _, err := l.AmendInstallmentPlanRule("ip-rule-audit", InstallmentMonthly); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanRule("ip-rule-audit", InstallmentWeekly); err != nil {
		t.Fatalf("AmendInstallmentPlanRule: %v", err)
	}
	if _, err := l.AmendInstallmentPlanRule("ip-rule-audit", InstallmentDaily); err != nil {
		t.Fatalf("AmendInstallmentPlanRule second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["installment_rule_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-op must not audit)", len(events))
	}
	if events[0].TraceID != "ip-rule-audit" || events[0].Actor != "AmendInstallmentPlanRule" {
		t.Errorf("event trace/actor = %q/%q, want ip-rule-audit/AmendInstallmentPlanRule", events[0].TraceID, events[0].Actor)
	}
	d := events[0].Details
	if d["plan_id"] != "ip-rule-audit" ||
		d["previous_rule"] != "monthly" ||
		d["rule"] != "weekly" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("event details = %+v, want ip-rule-audit monthly->weekly posted_periods 1", d)
	}
	d = events[1].Details
	if d["previous_rule"] != "weekly" ||
		d["rule"] != "daily" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("second event details = %+v, want weekly->daily posted_periods 1", d)
	}
}
