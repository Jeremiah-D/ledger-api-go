package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInstallmentPlanFirstDueAtValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-unknown", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-first-due")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-first-due")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-first-due")
	if _, err := l2.SettleInstallmentEarly("ip-first-due", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanFirstDueAt("ip-first-due", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// A zero first due date fails with the amendment's own gate
	// error, and every rejection leaves the original plan exactly
	// as it was.
	l3 := New()
	orig3 := mustOpenInstallmentPlan(t, l3, "ip-first-due")
	if _, err := l3.AmendInstallmentPlanFirstDueAt("ip-first-due", time.Time{}); !errors.Is(err, ErrInvalidInstallmentFirstDueAt) {
		t.Errorf("zero date: err = %v, want ErrInvalidInstallmentFirstDueAt", err)
	}
	got, _ := l3.InstallmentPlan("ip-first-due")
	if !reflect.DeepEqual(got, orig3) {
		t.Errorf("plan after zero-date rejection = %+v, want %+v (untouched)", got, orig3)
	}

	// Amending to the current first due date is a no-op, not a change.
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got, err := l3.AmendInstallmentPlanFirstDueAt("ip-first-due", same); err != nil || !reflect.DeepEqual(got, orig3) {
		t.Errorf("same-date amendment = %+v, %v, want %+v, nil", got, err, orig3)
	}
}

func TestAmendInstallmentPlanFirstDueAtSweepHonorsNewDate(t *testing.T) {
	// The new first due date governs unposted periods from the next
	// sweep: period 1 posts monthly (due 2026-01-01), the anchor
	// moves to 2026-01-20, and the next sweep posts periods 2 and 3
	// on monthly due dates from the new anchor (2026-02-20,
	// 2026-03-20) — under the old anchor period 2 would have been
	// due 2026-02-01.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-first-due-sweep")
	res, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(res) != 1 || res[0].Period != 1 || res[0].AmountCents != 333 {
		t.Fatalf("first sweep = %+v, want one period 1 of 333", res)
	}
	// Baseline for the untouched-fields check: the plan as it stands
	// after period 1 posted, before the amendment.
	orig, _ := l.InstallmentPlan("ip-first-due-sweep")

	amended, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-sweep", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AmendInstallmentPlanFirstDueAt: %v", err)
	}
	if !amended.FirstDueAt.Equal(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("amended first due date = %v, want 2026-01-20", amended.FirstDueAt)
	}
	// Only the first due date moves: principal, period count, rule,
	// currency, legs, status, and posted periods are untouched.
	amended.FirstDueAt = orig.FirstDueAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended plan = %+v, want %+v except first due date", amended, orig)
	}
	// The amendment itself posts nothing.
	if got := len(l.Entries()); got != 1 {
		t.Fatalf("entries after amendment = %d, want 1 (amendment posts nothing)", got)
	}

	// Under the old anchor period 2 would be due 2026-02-01; under
	// the new anchor it is due 2026-02-20, so this sweep posts
	// nothing.
	res, err = l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("second sweep posted %d periods, want 0 (period 2 not due until 2026-02-20)", len(res))
	}

	res, err = l.InstallmentsDue(time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("third sweep posted %d periods, want 2 (periods 2 and 3 from the new anchor)", len(res))
	}
	wantDue := []time.Time{
		time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
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
	if first.ID != "installment/ip-first-due-sweep/1" || first.AmountCents != 333 ||
		!first.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period 1 row = %+v, want installment/ip-first-due-sweep/1 of 333 as of 2026-01-01", first)
	}
	plan, _ := l.InstallmentPlan("ip-first-due-sweep")
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

func TestAmendInstallmentPlanFirstDueAtOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan may
	// be amended, and the new first due date governs the catch-up
	// sweep.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-first-due-overdue")
	// Post period 1, then freeze the payer so period 2 fails.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-first-due-overdue")
	if plan.Status != InstallmentOverdue {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentOverdue)
	}
	// Amend while overdue: moving the anchor to 2026-01-02 moves
	// period 2's due date from 2026-02-01 to 2026-02-02 and period
	// 3's from 2026-03-01 to 2026-03-02.
	amended, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-overdue", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AmendInstallmentPlanFirstDueAt overdue: %v", err)
	}
	if !amended.FirstDueAt.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = first due %v status %q, want 2026-01-02 overdue", amended.FirstDueAt, amended.Status)
	}
	// Unfreeze: the catch-up sweep posts period 2 on its new due
	// date and recovers the plan.
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("catch-up sweep posted %d periods, want 1 (period 2 only; period 3 due 2026-03-02)", len(res))
	}
	if res[0].Period != 2 || !res[0].DueAt.Equal(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)) || !res[0].Posted {
		t.Errorf("catch-up result = %+v, want period 2 due 2026-02-02 posted", res[0])
	}
	plan, _ = l.InstallmentPlan("ip-first-due-overdue")
	if plan.Status != InstallmentActive || plan.PostedPeriods != 2 {
		t.Errorf("plan = %+v, want active with 2 posted periods", plan)
	}
	// The final period posts on its new due date and completes.
	res, err = l.InstallmentsDue(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("final sweep: %v", err)
	}
	if len(res) != 1 || res[0].Period != 3 || !res[0].DueAt.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("final sweep = %+v, want period 3 due 2026-03-02", res)
	}
	plan, _ = l.InstallmentPlan("ip-first-due-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
}

func TestAmendInstallmentPlanFirstDueAtEarlierThanPosted(t *testing.T) {
	// Moving the anchor earlier can leave an unposted period's due
	// date earlier than an already-posted one: the due date only
	// decides sweep release order, it never reorders posted history.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-first-due-early")
	res, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("first sweep posted %d periods, want 2", len(res))
	}
	// Anchor 2025-11-15: period 3 is due 2026-01-15, earlier than
	// period 2's posted due date of 2026-02-01.
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-early", time.Date(2025, 11, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AmendInstallmentPlanFirstDueAt: %v", err)
	}
	res, err = l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(res) != 1 || res[0].Period != 3 || !res[0].DueAt.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) || !res[0].Posted {
		t.Fatalf("second sweep = %+v, want period 3 due 2026-01-15 posted", res)
	}
	// The posted rows keep their original due dates: history is
	// never rewritten, even though period 3's due date now precedes
	// period 2's.
	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if !entries[0].CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		!entries[1].CreatedAt.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) ||
		!entries[2].CreatedAt.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("entry due dates = %v, %v, %v, want 2026-01-01, 2026-02-01, 2026-01-15",
			entries[0].CreatedAt, entries[1].CreatedAt, entries[2].CreatedAt)
	}
	plan, _ := l.InstallmentPlan("ip-first-due-early")
	if plan.Status != InstallmentCompleted || plan.PostedPeriods != 3 {
		t.Errorf("plan = %+v, want completed with 3 posted periods", plan)
	}
}

func TestAmendInstallmentPlanFirstDueAtAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-first-due-audit")
	// Post one period so the audit carries a posted-period count.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// The no-op revision emits nothing.
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-audit", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-audit", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AmendInstallmentPlanFirstDueAt: %v", err)
	}
	if _, err := l.AmendInstallmentPlanFirstDueAt("ip-first-due-audit", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AmendInstallmentPlanFirstDueAt second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["installment_first_due_at_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-op must not audit)", len(events))
	}
	if events[0].TraceID != "ip-first-due-audit" || events[0].Actor != "AmendInstallmentPlanFirstDueAt" {
		t.Errorf("event trace/actor = %q/%q, want ip-first-due-audit/AmendInstallmentPlanFirstDueAt", events[0].TraceID, events[0].Actor)
	}
	d := events[0].Details
	if d["plan_id"] != "ip-first-due-audit" ||
		d["previous_first_due_at"] != "2026-01-01T00:00:00Z" ||
		d["first_due_at"] != "2026-02-01T00:00:00Z" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("event details = %+v, want ip-first-due-audit 2026-01-01->2026-02-01 posted_periods 1", d)
	}
	d = events[1].Details
	if d["previous_first_due_at"] != "2026-02-01T00:00:00Z" ||
		d["first_due_at"] != "2026-03-01T00:00:00Z" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("second event details = %+v, want 2026-02-01->2026-03-01 posted_periods 1", d)
	}
}
