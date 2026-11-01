package ledger

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestAmendInstallmentPlanPrincipalValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanPrincipal("ip-unknown", 1500); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-principal")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-principal")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanPrincipal("ip-principal", 1500); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-principal")
	if _, err := l2.SettleInstallmentEarly("ip-principal", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanPrincipal("ip-principal", 1500); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Posted periods: amending a plan that has already journaled a
	// period would restate history, so it fails closed — and the
	// plan is left exactly as it was.
	l3 := New()
	mustOpenInstallmentPlan(t, l3, "ip-principal")
	if _, err := l3.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	orig3, _ := l3.InstallmentPlan("ip-principal")
	if orig3.PostedPeriods != 1 {
		t.Fatalf("posted periods = %d, want 1", orig3.PostedPeriods)
	}
	if _, err := l3.AmendInstallmentPlanPrincipal("ip-principal", 1500); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("plan with posted periods: err = %v, want ErrInstallmentBadTransition", err)
	}
	got, _ := l3.InstallmentPlan("ip-principal")
	if !reflect.DeepEqual(got, orig3) {
		t.Errorf("plan after posted-period rejection = %+v, want %+v (untouched)", got, orig3)
	}

	// Invalid principals fail with the creation gate's error, and
	// every rejection leaves the original plan exactly as it was.
	l4 := New()
	orig4 := mustOpenInstallmentPlan(t, l4, "ip-principal")
	for _, bad := range []int64{0, -1, -500} {
		if _, err := l4.AmendInstallmentPlanPrincipal("ip-principal", bad); !errors.Is(err, ErrInvalidInstallmentAmount) {
			t.Errorf("principal %d: err = %v, want ErrInvalidInstallmentAmount", bad, err)
		}
		got, _ := l4.InstallmentPlan("ip-principal")
		if !reflect.DeepEqual(got, orig4) {
			t.Errorf("plan after principal %d rejection = %+v, want %+v (untouched)", bad, got, orig4)
		}
	}

	// Amending to the current principal is a no-op, not a change.
	if got, err := l4.AmendInstallmentPlanPrincipal("ip-principal", 1000); err != nil || !reflect.DeepEqual(got, orig4) {
		t.Errorf("same-principal amendment = %+v, %v, want %+v, nil", got, err, orig4)
	}
}

func TestAmendInstallmentPlanPrincipalSweepHonorsNewPrincipal(t *testing.T) {
	// The new principal governs every period from the next sweep:
	// the plan opens at 1000 over 3 periods, the principal moves to
	// 1501 before any period posts, and the sweep books 500/500/501
	// — floor division with the final period absorbing the
	// remainder — summing to exactly the new principal.
	l := New()
	orig := mustOpenInstallmentPlan(t, l, "ip-principal-sweep")

	amended, err := l.AmendInstallmentPlanPrincipal("ip-principal-sweep", 1501)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPrincipal: %v", err)
	}
	if amended.PrincipalCents != 1501 {
		t.Errorf("amended principal = %d, want 1501", amended.PrincipalCents)
	}
	// Only the principal moves: payer, payee, period count, rule,
	// first due date, currency, legs, status, and posted periods
	// are untouched.
	amended.PrincipalCents = orig.PrincipalCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended plan = %+v, want %+v except principal", amended, orig)
	}
	// The amendment itself posts nothing.
	if got := len(l.Entries()); got != 0 {
		t.Fatalf("entries after amendment = %d, want 0 (amendment posts nothing)", got)
	}

	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("sweep posted %d periods, want 3", len(res))
	}
	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// Entries() ranges a map, so order is nondeterministic: sort
	// by ID before asserting per-period amounts.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for i, want := range []struct {
		id  string
		amt int64
	}{
		{"installment/ip-principal-sweep/1", 500},
		{"installment/ip-principal-sweep/2", 500},
		{"installment/ip-principal-sweep/3", 501},
	} {
		if got := entries[i]; got.ID != want.id || got.AmountCents != want.amt {
			t.Errorf("period %d row = %+v, want %s of %d", i+1, got, want.id, want.amt)
		}
	}
	plan, _ := l.InstallmentPlan("ip-principal-sweep")
	if plan.Status != InstallmentCompleted || plan.PostedPeriods != 3 {
		t.Errorf("plan = %+v, want completed with 3 posted periods", plan)
	}
	// Periods sum to exactly the new principal: no dust created
	// or lost.
	if got := l.Balance("merchant"); got != 1501 {
		t.Errorf("merchant balance = %d, want 1501", got)
	}
	if got := l.Balance("buyer"); got != -1501 {
		t.Errorf("buyer balance = %d, want -1501", got)
	}
}

func TestAmendInstallmentPlanPrincipalOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan with
	// zero posted periods may be amended, and the new principal
	// governs the catch-up sweep.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-principal-overdue")
	// Freeze the payer before the first sweep so period 1 fails and
	// the plan goes overdue with nothing posted.
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-principal-overdue")
	if plan.Status != InstallmentOverdue || plan.PostedPeriods != 0 {
		t.Fatalf("plan = %+v, want overdue with 0 posted periods", plan)
	}
	// Amend while overdue: 1000 -> 900.
	amended, err := l.AmendInstallmentPlanPrincipal("ip-principal-overdue", 900)
	if err != nil {
		t.Fatalf("AmendInstallmentPlanPrincipal overdue: %v", err)
	}
	if amended.PrincipalCents != 900 || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = principal %d status %q, want 900 overdue", amended.PrincipalCents, amended.Status)
	}
	// Unfreeze: the catch-up sweep posts periods 1-3 at 300 each
	// and completes the plan.
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("catch-up sweep posted %d periods, want 3", len(res))
	}
	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, e := range entries {
		if e.AmountCents != 300 {
			t.Errorf("period %d row = %+v, want 300", i+1, e)
		}
	}
	plan, _ = l.InstallmentPlan("ip-principal-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
}

func TestAmendInstallmentPlanPrincipalAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-principal-audit")
	// The no-op revision emits nothing (same principal).
	if _, err := l.AmendInstallmentPlanPrincipal("ip-principal-audit", 1000); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPrincipal("ip-principal-audit", 1500); err != nil {
		t.Fatalf("AmendInstallmentPlanPrincipal: %v", err)
	}
	if _, err := l.AmendInstallmentPlanPrincipal("ip-principal-audit", 1200); err != nil {
		t.Fatalf("AmendInstallmentPlanPrincipal second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["installment_principal_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-ops must not audit)", len(events))
	}
	if events[0].TraceID != "ip-principal-audit" || events[0].Actor != "AmendInstallmentPlanPrincipal" {
		t.Errorf("event trace/actor = %q/%q, want ip-principal-audit/AmendInstallmentPlanPrincipal", events[0].TraceID, events[0].Actor)
	}
	d := events[0].Details
	if d["plan_id"] != "ip-principal-audit" ||
		d["previous_principal_cents"] != float64(1000) ||
		d["principal_cents"] != float64(1500) ||
		d["posted_periods"] != float64(0) {
		t.Errorf("event details = %+v, want ip-principal-audit 1000->1500 posted_periods 0", d)
	}
	d = events[1].Details
	if d["previous_principal_cents"] != float64(1500) ||
		d["principal_cents"] != float64(1200) ||
		d["posted_periods"] != float64(0) {
		t.Errorf("second event details = %+v, want 1500->1200 posted_periods 0", d)
	}
}
