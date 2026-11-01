package ledger

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestAmendInstallmentPlanNumPeriodsValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanNumPeriods("ip-unknown", 4); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-periods")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-periods")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanNumPeriods("ip-periods", 4); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-periods")
	if _, err := l2.SettleInstallmentEarly("ip-periods", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanNumPeriods("ip-periods", 4); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Posted periods: amending a plan that has already journaled a
	// period would restate history, so it fails closed — and the
	// plan is left exactly as it was.
	l3 := New()
	mustOpenInstallmentPlan(t, l3, "ip-periods")
	if _, err := l3.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	orig3, _ := l3.InstallmentPlan("ip-periods")
	if orig3.PostedPeriods != 1 {
		t.Fatalf("posted periods = %d, want 1", orig3.PostedPeriods)
	}
	if _, err := l3.AmendInstallmentPlanNumPeriods("ip-periods", 4); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("plan with posted periods: err = %v, want ErrInstallmentBadTransition", err)
	}
	got, _ := l3.InstallmentPlan("ip-periods")
	if !reflect.DeepEqual(got, orig3) {
		t.Errorf("plan after posted-period rejection = %+v, want %+v (untouched)", got, orig3)
	}

	// Invalid period counts fail with the creation gate's error,
	// and every rejection leaves the original plan exactly as it
	// was.
	l4 := New()
	orig4 := mustOpenInstallmentPlan(t, l4, "ip-periods")
	for _, bad := range []int{0, -1, -4} {
		if _, err := l4.AmendInstallmentPlanNumPeriods("ip-periods", bad); !errors.Is(err, ErrInvalidInstallmentPeriods) {
			t.Errorf("periods %d: err = %v, want ErrInvalidInstallmentPeriods", bad, err)
		}
		got, _ := l4.InstallmentPlan("ip-periods")
		if !reflect.DeepEqual(got, orig4) {
			t.Errorf("plan after periods %d rejection = %+v, want %+v (untouched)", bad, got, orig4)
		}
	}

	// Amending to the current period count is a no-op, not a
	// change.
	if got, err := l4.AmendInstallmentPlanNumPeriods("ip-periods", 3); err != nil || !reflect.DeepEqual(got, orig4) {
		t.Errorf("same-period-count amendment = %+v, %v, want %+v, nil", got, err, orig4)
	}
}

func TestAmendInstallmentPlanNumPeriodsSweepHonorsNewCount(t *testing.T) {
	// The new period count governs every period from the next
	// sweep: the plan opens at 1000 over 3 periods, the count moves
	// to 6 before any period posts, and the sweep books
	// 166/166/166/166/166/170 — floor division with the final
	// period absorbing the remainder — summing to exactly the
	// principal, with due dates re-anchored monthly from the first
	// due date across the new six periods.
	l := New()
	orig := mustOpenInstallmentPlan(t, l, "ip-periods-sweep")

	amended, err := l.AmendInstallmentPlanNumPeriods("ip-periods-sweep", 6)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanNumPeriods: %v", err)
	}
	if amended.NumPeriods != 6 {
		t.Errorf("amended periods = %d, want 6", amended.NumPeriods)
	}
	// Only the period count moves: payer, payee, principal, rule,
	// first due date, currency, legs, status, and posted periods
	// are untouched.
	amended.NumPeriods = orig.NumPeriods
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended plan = %+v, want %+v except period count", amended, orig)
	}
	// The amendment itself posts nothing.
	if got := len(l.Entries()); got != 0 {
		t.Fatalf("entries after amendment = %d, want 0 (amendment posts nothing)", got)
	}

	res, err := l.InstallmentsDue(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res) != 6 {
		t.Fatalf("sweep posted %d periods, want 6", len(res))
	}
	entries := l.Entries()
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6", len(entries))
	}
	// Entries() ranges a map, so order is nondeterministic: sort
	// by ID before asserting per-period amounts.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for i, want := range []struct {
		id  string
		amt int64
		due time.Time
	}{
		{"installment/ip-periods-sweep/1", 166, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"installment/ip-periods-sweep/2", 166, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"installment/ip-periods-sweep/3", 166, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"installment/ip-periods-sweep/4", 166, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{"installment/ip-periods-sweep/5", 166, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
		{"installment/ip-periods-sweep/6", 170, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if got := entries[i]; got.ID != want.id || got.AmountCents != want.amt || !got.CreatedAt.Equal(want.due) {
			t.Errorf("period %d row = %+v, want %s of %d due %v", i+1, got, want.id, want.amt, want.due)
		}
		if got := res[i]; got.Period != i+1 || got.AmountCents != want.amt || !got.DueAt.Equal(want.due) {
			t.Errorf("period %d result = %+v, want period %d of %d due %v", i+1, got, i+1, want.amt, want.due)
		}
	}
	plan, _ := l.InstallmentPlan("ip-periods-sweep")
	if plan.Status != InstallmentCompleted || plan.PostedPeriods != 6 {
		t.Errorf("plan = %+v, want completed with 6 posted periods", plan)
	}
	// Periods sum to exactly the principal: no dust created or
	// lost, and the count change alone never moves the total.
	if got := l.Balance("merchant"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000", got)
	}
	if got := l.Balance("buyer"); got != -1000 {
		t.Errorf("buyer balance = %d, want -1000", got)
	}
}

func TestAmendInstallmentPlanNumPeriodsOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan with
	// zero posted periods may be amended, and the new period count
	// governs the catch-up sweep.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-periods-overdue")
	// Freeze the payer before the first sweep so period 1 fails and
	// the plan goes overdue with nothing posted.
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-periods-overdue")
	if plan.Status != InstallmentOverdue || plan.PostedPeriods != 0 {
		t.Fatalf("plan = %+v, want overdue with 0 posted periods", plan)
	}
	// Amend while overdue: 3 -> 2 periods.
	amended, err := l.AmendInstallmentPlanNumPeriods("ip-periods-overdue", 2)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanNumPeriods overdue: %v", err)
	}
	if amended.NumPeriods != 2 || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = periods %d status %q, want 2 overdue", amended.NumPeriods, amended.Status)
	}
	// Unfreeze: the catch-up sweep posts periods 1-2 at 500 each
	// and completes the plan.
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("catch-up sweep posted %d periods, want 2", len(res))
	}
	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for i, e := range entries {
		if e.AmountCents != 500 {
			t.Errorf("period %d row = %+v, want 500", i+1, e)
		}
	}
	plan, _ = l.InstallmentPlan("ip-periods-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
}

func TestAmendInstallmentPlanNumPeriodsAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-periods-audit")
	// The no-op revision emits nothing (same period count).
	if _, err := l.AmendInstallmentPlanNumPeriods("ip-periods-audit", 3); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanNumPeriods("ip-periods-audit", 6); err != nil {
		t.Fatalf("AmendInstallmentPlanNumPeriods: %v", err)
	}
	if _, err := l.AmendInstallmentPlanNumPeriods("ip-periods-audit", 2); err != nil {
		t.Fatalf("AmendInstallmentPlanNumPeriods second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["installment_num_periods_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-ops must not audit)", len(events))
	}
	if events[0].TraceID != "ip-periods-audit" || events[0].Actor != "AmendInstallmentPlanNumPeriods" {
		t.Errorf("event trace/actor = %q/%q, want ip-periods-audit/AmendInstallmentPlanNumPeriods", events[0].TraceID, events[0].Actor)
	}
	d := events[0].Details
	if d["plan_id"] != "ip-periods-audit" ||
		d["previous_num_periods"] != float64(3) ||
		d["num_periods"] != float64(6) ||
		d["posted_periods"] != float64(0) {
		t.Errorf("event details = %+v, want ip-periods-audit 3->6 posted_periods 0", d)
	}
	d = events[1].Details
	if d["previous_num_periods"] != float64(6) ||
		d["num_periods"] != float64(2) ||
		d["posted_periods"] != float64(0) {
		t.Errorf("second event details = %+v, want 6->2 posted_periods 0", d)
	}
}
