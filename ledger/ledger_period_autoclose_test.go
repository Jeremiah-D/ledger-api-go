package ledger

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// LG-151: period auto-close plans.

func scheduleTestPlan(t *testing.T, l *Ledger, period string, at time.Time, withClosing bool) *PeriodAutoClosePlan {
	t.Helper()
	plan, err := l.SchedulePeriodAutoClose(period, at, withClosing)
	if err != nil {
		t.Fatalf("SchedulePeriodAutoClose(%s): %v", period, err)
	}
	return plan
}

func TestSchedulePeriodAutoCloseValidation(t *testing.T) {
	l := New()
	future := time.Now().UTC().Add(time.Hour)

	if _, err := l.SchedulePeriodAutoClose("not-a-period", future, false); !errors.Is(err, ErrInvalidPeriodID) {
		t.Fatalf("bad period ID err = %v, want ErrInvalidPeriodID", err)
	}
	if _, err := l.SchedulePeriodAutoClose("2026-09", time.Now().UTC().Add(-time.Hour), false); !errors.Is(err, ErrAutoClosePastTime) {
		t.Fatalf("past time err = %v, want ErrAutoClosePastTime", err)
	}
	if _, err := l.SchedulePeriodAutoClose("2026-09", time.Now().UTC(), false); !errors.Is(err, ErrAutoClosePastTime) {
		t.Fatalf("now time err = %v, want ErrAutoClosePastTime", err)
	}

	plan := scheduleTestPlan(t, l, "2026-09", future, true)
	if plan.Status != PeriodAutoClosePending {
		t.Fatalf("plan status = %s, want pending", plan.Status)
	}
	if plan.PeriodID != "2026-09" || !plan.WithClosing {
		t.Fatalf("plan = %+v, wrong fields", plan)
	}
	if !plan.CreatedAt.Equal(plan.CloseAt.Add(-time.Hour)) && !plan.CreatedAt.Before(plan.CloseAt) {
		t.Fatalf("created %v not before close %v", plan.CreatedAt, plan.CloseAt)
	}

	// Duplicate pending plan for the same period is rejected.
	if _, err := l.SchedulePeriodAutoClose("2026-09", future.Add(time.Hour), false); !errors.Is(err, ErrAutoCloseDuplicate) {
		t.Fatalf("duplicate err = %v, want ErrAutoCloseDuplicate", err)
	}

	// A different period is independent.
	if _, err := l.SchedulePeriodAutoClose("2026-10", future, false); err != nil {
		t.Fatalf("second period: %v", err)
	}

	// Query + list.
	got, ok := l.PeriodAutoClosePlan("2026-09")
	if !ok || got.Status != PeriodAutoClosePending {
		t.Fatalf("query = %+v, %v", got, ok)
	}
	if _, ok := l.PeriodAutoClosePlan("2026-11"); ok {
		t.Fatalf("query of unregistered period returned a plan")
	}
	plans := l.PeriodAutoClosePlans()
	if len(plans) != 2 || plans[0].PeriodID != "2026-09" || plans[1].PeriodID != "2026-10" {
		t.Fatalf("plans = %+v, want sorted 2026-09, 2026-10", plans)
	}
}

func TestCancelPeriodAutoClose(t *testing.T) {
	l := New()
	future := time.Now().UTC().Add(time.Hour)

	if _, err := l.CancelPeriodAutoClose("2026-09"); !errors.Is(err, ErrAutoCloseNoPlan) {
		t.Fatalf("cancel missing err = %v, want ErrAutoCloseNoPlan", err)
	}
	if _, err := l.CancelPeriodAutoClose("bad-id"); !errors.Is(err, ErrInvalidPeriodID) {
		t.Fatalf("cancel bad ID err = %v, want ErrInvalidPeriodID", err)
	}

	scheduleTestPlan(t, l, "2026-09", future, false)
	cancelled, err := l.CancelPeriodAutoClose("2026-09")
	if err != nil {
		t.Fatalf("CancelPeriodAutoClose: %v", err)
	}
	if cancelled.Status != PeriodAutoCloseCancelled || cancelled.DecidedAt.IsZero() {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if _, err := l.CancelPeriodAutoClose("2026-09"); !errors.Is(err, ErrAutoCloseNotPending) {
		t.Fatalf("second cancel err = %v, want ErrAutoCloseNotPending", err)
	}

	// A terminal plan can be replaced by a fresh registration.
	replan := scheduleTestPlan(t, l, "2026-09", future.Add(time.Hour), true)
	if replan.Status != PeriodAutoClosePending || !replan.WithClosing {
		t.Fatalf("replan = %+v", replan)
	}
}

func TestSweepPeriodAutoClosesPlain(t *testing.T) {
	l := New()
	future := time.Now().UTC().Add(time.Hour)
	scheduleTestPlan(t, l, "2026-09", future, false)
	scheduleTestPlan(t, l, "2026-11", future.Add(48*time.Hour), false)

	// A sweep before either plan is due decides nothing.
	if decided := l.SweepPeriodAutoCloses(future.Add(-time.Minute)); decided != 0 {
		t.Fatalf("early sweep decided %d, want 0", decided)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatalf("period closed before its plan fired")
	}

	// The first plan is due; the second is not.
	if decided := l.SweepPeriodAutoCloses(future.Add(time.Minute)); decided != 1 {
		t.Fatalf("sweep decided %d, want 1", decided)
	}
	plan, ok := l.PeriodAutoClosePlan("2026-09")
	if !ok || plan.Status != PeriodAutoCloseExecuted || plan.DecidedAt.IsZero() {
		t.Fatalf("plan after sweep = %+v", plan)
	}
	if !l.IsPeriodClosed("2026-09") {
		t.Fatalf("period 2026-09 not closed after its plan fired")
	}
	if l.IsPeriodClosed("2026-11") {
		t.Fatalf("period 2026-11 closed before its plan fired")
	}

	// Postings dated in the closed period are rejected.
	if _, _, err := l.Post(JournalEntry{
		ID:            "late",
		DebitAccount:  "cash",
		CreditAccount: "revenue",
		AmountCents:   100,
		Currency:      "USD",
		CreatedAt:     sept2026(),
	}); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("post into closed period err = %v, want ErrPeriodClosed", err)
	}
}

func TestSweepPeriodAutoClosesWithClosingEntries(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue", "expenses"}, "retained"); err != nil {
		t.Fatalf("SetClosingAccounts: %v", err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())
	postDated(t, l, "exp-1", "expenses", "cash", 200, "USD", sept2026())

	future := time.Now().UTC().Add(time.Hour)
	scheduleTestPlan(t, l, "2026-09", future, true)
	if decided := l.SweepPeriodAutoCloses(future.Add(time.Minute)); decided != 1 {
		t.Fatalf("sweep decided %d, want 1", decided)
	}
	if !l.IsPeriodClosed("2026-09") {
		t.Fatalf("period not closed by its auto-close plan")
	}
	if b := l.BalanceIn("revenue", "USD"); b != 0 {
		t.Fatalf("revenue = %d, want 0 (closing entries booked)", b)
	}
	if b := l.BalanceIn("retained", "USD"); b != -300 {
		t.Fatalf("retained = %d, want -300", b)
	}
	plan, _ := l.PeriodAutoClosePlan("2026-09")
	if plan.Status != PeriodAutoCloseExecuted || plan.FailureReason != "" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestSweepPeriodAutoCloseFailureKeepsPeriodOpen(t *testing.T) {
	l := New()
	// withClosing=true but no closing accounts configured: the close
	// fails like the endpoint's 400 path, and the period must stay open.
	future := time.Now().UTC().Add(time.Hour)
	scheduleTestPlan(t, l, "2026-09", future, true)
	if decided := l.SweepPeriodAutoCloses(future.Add(time.Minute)); decided != 1 {
		t.Fatalf("sweep decided %d, want 1", decided)
	}
	plan, _ := l.PeriodAutoClosePlan("2026-09")
	if plan.Status != PeriodAutoCloseFailed {
		t.Fatalf("plan status = %s, want failed", plan.Status)
	}
	if !strings.Contains(plan.FailureReason, "closing accounts") {
		t.Fatalf("failure reason = %q, want closing-accounts complaint", plan.FailureReason)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatalf("failed auto-close closed the period: half-close")
	}
	// A failed plan is terminal and cannot be cancelled.
	if _, err := l.CancelPeriodAutoClose("2026-09"); !errors.Is(err, ErrAutoCloseNotPending) {
		t.Fatalf("cancel failed plan err = %v, want ErrAutoCloseNotPending", err)
	}
}

func TestPeriodAutoCloseSweeperGoroutine(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var decided atomic.Uint64
	sw := StartPeriodAutoCloseSweeper(ctx, l, 20*time.Millisecond, func(d int) { decided.Add(uint64(d)) })
	if sw == nil {
		t.Fatalf("sweeper disabled with a positive interval")
	}
	defer sw.Stop()
	if StartPeriodAutoCloseSweeper(ctx, l, 0, nil) != nil {
		t.Fatalf("zero interval must disable the sweeper")
	}

	future := time.Now().UTC().Add(50 * time.Millisecond)
	if _, err := l.SchedulePeriodAutoClose("2026-09", future, false); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for decided.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if decided.Load() == 0 {
		t.Fatalf("sweeper never fired the due plan")
	}
	if plan, _ := l.PeriodAutoClosePlan("2026-09"); plan.Status != PeriodAutoCloseExecuted {
		t.Fatalf("plan = %+v", plan)
	}
	if sw.Ticks() == 0 {
		t.Fatalf("sweeper reported no ticks")
	}
	sw.Stop() // idempotent
	sw.Stop()
}

func TestPeriodAutoCloseAuditTrail(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	future := time.Now().UTC().Add(time.Hour)
	scheduleTestPlan(t, l, "2026-09", future, false)
	scheduleTestPlan(t, l, "2026-10", future, false)
	if _, err := l.CancelPeriodAutoClose("2026-10"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	l.SweepPeriodAutoCloses(future.Add(time.Minute))
	if err := al.Close(); err != nil {
		t.Fatalf("al.Close: %v", err)
	}

	seen := map[string]int{}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	for _, ev := range events {
		switch ev.Op {
		case "period_autoclose_scheduled", "period_autoclose_cancelled",
			"period_autoclose_executed", "period_autoclose_failed":
			seen[ev.Op]++
		}
	}
	if seen["period_autoclose_scheduled"] != 2 || seen["period_autoclose_cancelled"] != 1 ||
		seen["period_autoclose_executed"] != 1 || seen["period_autoclose_failed"] != 0 {
		t.Fatalf("audit ops = %+v", seen)
	}
}

func TestPeriodAutoCloseSnapshotRoundTrip(t *testing.T) {
	l := New()
	future := time.Now().UTC().Add(time.Hour)
	scheduleTestPlan(t, l, "2026-09", future, true)
	scheduleTestPlan(t, l, "2026-10", future.Add(time.Hour), false)
	if _, err := l.CancelPeriodAutoClose("2026-10"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	for _, id := range []string{"2026-09", "2026-10"} {
		orig, _ := l.PeriodAutoClosePlan(id)
		got, ok := restored.PeriodAutoClosePlan(id)
		if !ok {
			t.Fatalf("plan %s lost in snapshot round trip", id)
		}
		if got.Status != orig.Status || !got.CloseAt.Equal(orig.CloseAt) ||
			got.WithClosing != orig.WithClosing || !got.CreatedAt.Equal(orig.CreatedAt) ||
			!got.DecidedAt.Equal(orig.DecidedAt) {
			t.Fatalf("plan %s = %+v, want %+v", id, got, orig)
		}
	}
	// The restored pending plan still fires.
	restored.SweepPeriodAutoCloses(future.Add(time.Minute))
	if plan, _ := restored.PeriodAutoClosePlan("2026-09"); plan.Status != PeriodAutoCloseFailed {
		// withClosing=true with no closing accounts configured fails the
		// close — the point is the plan fired at all after restore.
		t.Fatalf("restored plan status = %s", plan.Status)
	}
}
