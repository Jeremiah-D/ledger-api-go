package ledger

import (
	"errors"
	"testing"
	"time"
)

// Cancelling an installment plan (LG-361): an active or overdue plan
// ends without settling its remaining principal — posted periods stay
// booked, future periods and late fees stop, and terminal plans
// re-cancel idempotently.

func TestCancelInstallmentPlanActive(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-cancel")

	// Post period 1, then cancel: period 1 stays booked, periods 2
	// and 3 must never post.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	plan, err := l.CancelInstallmentPlan("ip-cancel", "operator-1")
	if err != nil {
		t.Fatalf("CancelInstallmentPlan: %v", err)
	}
	if plan.Status != InstallmentCancelled {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCancelled)
	}
	if plan.PostedPeriods != 1 {
		t.Fatalf("posted periods = %d, want 1", plan.PostedPeriods)
	}
	entriesAfterCancel := len(l.Entries())

	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep after cancel: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("sweep after cancel posted %d periods, want 0", len(res))
	}
	if got := len(l.Entries()); got != entriesAfterCancel {
		t.Fatalf("entries after cancel sweep = %d, want %d", got, entriesAfterCancel)
	}
	plan, _ = l.InstallmentPlan("ip-cancel")
	if plan.Status != InstallmentCancelled || plan.PostedPeriods != 1 {
		t.Fatalf("plan after cancel sweep = %+v, want cancelled with 1 posted period", plan)
	}
}

func TestCancelInstallmentPlanOverdue(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-cancel-overdue")

	// Freeze the payer so the first sweep fails and the plan goes
	// overdue, then cancel it.
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, err := l.CancelInstallmentPlan("ip-cancel-overdue", "operator-1")
	if err != nil {
		t.Fatalf("CancelInstallmentPlan overdue: %v", err)
	}
	if plan.Status != InstallmentCancelled {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCancelled)
	}

	// Unfreeze and sweep: the cancelled plan stays silent.
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep after cancel: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("sweep after cancel posted %d periods, want 0", len(res))
	}
	if got := len(l.Entries()); got != 0 {
		t.Fatalf("cancelled overdue plan journaled %d entries, want 0", got)
	}
}

func TestCancelInstallmentPlanKeepsPostedPeriods(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-cancel-keep")

	// Post two of three periods (333 + 333), then cancel.
	if _, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	entriesBefore := len(l.Entries())
	plan, err := l.CancelInstallmentPlan("ip-cancel-keep", "operator-1")
	if err != nil {
		t.Fatalf("CancelInstallmentPlan: %v", err)
	}
	// The cancel itself books nothing; the posted sum does not move.
	if got := len(l.Entries()); got != entriesBefore {
		t.Fatalf("cancel journaled %d entries, want %d", got, entriesBefore)
	}
	if got := installmentPostedSum(plan); got != 666 {
		t.Fatalf("posted sum = %d, want 666 (two periods stay booked)", got)
	}
	if b := l.Balance("merchant"); b != 666 {
		t.Fatalf("merchant balance = %d, want 666", b)
	}
	if b := l.Balance("buyer"); b != -666 {
		t.Fatalf("buyer balance = %d, want -666", b)
	}
}

func TestCancelInstallmentPlanRemainingNotSettled(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-cancel-remain")

	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := l.CancelInstallmentPlan("ip-cancel-remain", "operator-1"); err != nil {
		t.Fatalf("CancelInstallmentPlan: %v", err)
	}
	// No early-settlement row: the remaining 667 cents were not
	// collected, unlike SettleInstallmentEarly.
	for _, e := range l.Entries() {
		if e.ID == installmentEarlySettleID("ip-cancel-remain") {
			t.Fatalf("cancel booked an early-settlement row %q", e.ID)
		}
	}
	if b := l.Balance("merchant"); b != 333 {
		t.Fatalf("merchant balance = %d, want 333 (period 1 only)", b)
	}
}

func TestCancelInstallmentPlanIdempotent(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-cancel-idem")

	first, err := l.CancelInstallmentPlan("ip-cancel-idem", "operator-1")
	if err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	entriesAfterFirst := len(l.Entries())
	second, err := l.CancelInstallmentPlan("ip-cancel-idem", "operator-1")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if second != first {
		t.Fatalf("re-cancel = %+v, want the original plan %+v", second, first)
	}
	if got := len(l.Entries()); got != entriesAfterFirst {
		t.Fatalf("re-cancel journaled %d entries, want %d", got, entriesAfterFirst)
	}
}

func TestCancelInstallmentPlanTerminalIdempotent(t *testing.T) {
	l := New()

	// A completed plan re-cancels unchanged.
	mustOpenInstallmentPlan(t, l, "ip-cancel-done")
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	done, _ := l.InstallmentPlan("ip-cancel-done")
	if done.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want completed", done.Status)
	}
	again, err := l.CancelInstallmentPlan("ip-cancel-done", "operator-1")
	if err != nil {
		t.Fatalf("cancel completed: %v", err)
	}
	if again != done {
		t.Fatalf("cancel completed = %+v, want %+v unchanged", again, done)
	}

	// A settled-early plan re-cancels unchanged.
	mustOpenInstallmentPlan(t, l, "ip-cancel-settled")
	settled, err := l.SettleInstallmentEarly("ip-cancel-settled", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SettleInstallmentEarly: %v", err)
	}
	again, err = l.CancelInstallmentPlan("ip-cancel-settled", "operator-1")
	if err != nil {
		t.Fatalf("cancel settled-early: %v", err)
	}
	if again != settled {
		t.Fatalf("cancel settled-early = %+v, want %+v unchanged", again, settled)
	}
}

func TestCancelInstallmentPlanNotFound(t *testing.T) {
	l := New()
	if _, err := l.CancelInstallmentPlan("no-such-plan", "operator-1"); !errors.Is(err, ErrInstallmentNotFound) {
		t.Fatalf("CancelInstallmentPlan unknown: err = %v, want ErrInstallmentNotFound", err)
	}
}

func TestCancelInstallmentPlanStopsLateFees(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-lf")
	if err := l.SetInstallmentLateFee("ip-lf", InstallmentLateFeeRule{
		FeeAccount: "fee-income", DailyRateBps: 3650, FixedCentsPerDay: 10,
	}); err != nil {
		t.Fatalf("SetInstallmentLateFee: %v", err)
	}

	// Period 1 is due 2026-01-01 and unposted: a fee accrues.
	res, err := l.AccrueInstallmentLateFees(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("accrue before cancel: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("late fees before cancel = %d, want 1", len(res))
	}
	entriesAfterFee := len(l.Entries())

	if _, err := l.CancelInstallmentPlan("ip-lf", "operator-1"); err != nil {
		t.Fatalf("CancelInstallmentPlan: %v", err)
	}
	// Later accruals find a cancelled plan and book nothing.
	res, err = l.AccrueInstallmentLateFees(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("accrue after cancel: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("late fees after cancel = %d, want 0", len(res))
	}
	if got := len(l.Entries()); got != entriesAfterFee {
		t.Fatalf("entries after cancel accrual = %d, want %d", got, entriesAfterFee)
	}
}

func TestCancelInstallmentPlanAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-cancel")
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := l.CancelInstallmentPlan("ip-cancel", "operator-1"); err != nil {
		t.Fatalf("CancelInstallmentPlan: %v", err)
	}

	var cancels []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "installment_plan_cancelled" {
			cancels = append(cancels, e)
		}
	}
	if len(cancels) != 1 {
		t.Fatalf("installment_plan_cancelled events = %d, want exactly 1", len(cancels))
	}
	ev := cancels[0]
	if ev.Actor != "operator-1" {
		t.Fatalf("audit actor = %q, want operator-1", ev.Actor)
	}
	if ev.TraceID != "ip-cancel" {
		t.Fatalf("audit trace ID = %q, want ip-cancel", ev.TraceID)
	}
	// Audit details round-trip through JSON, so the count reads back
	// as a float.
	if got, ok := ev.Details["posted_periods"].(float64); !ok || got != 1 {
		t.Fatalf("audit posted_periods = %v, want 1", ev.Details["posted_periods"])
	}
}
