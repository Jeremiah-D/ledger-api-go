package ledger

import (
	"math"
	"testing"
	"time"
)

// paceTestMonth returns a pinned mid-month instant in the current real
// UTC month, so the monthly-spend bucket the test's postings land in is
// the same bucket BudgetPace(now) reads: postings are keyed by their
// own UTC month.
func paceTestMonth(t *testing.T) (now, mid time.Time) {
	t.Helper()
	now = time.Now().UTC()
	mid = time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)
	return now, mid
}

// mustPaceSpend posts spend for the budget-pace tests through the real
// posting path (pinned CreatedAt), so the budget's monthly bucket fills
// exactly like production spend.
func mustPaceSpend(t *testing.T, l *Ledger, id string, credit AccountID, amount int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  "merchant",
		CreditAccount: credit,
		AmountCents:   amount,
		Currency:      "USD",
		CreatedAt:     at,
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
}

// The projection is the booked month-to-date spend linearly
// extrapolated over the elapsed share of the UTC month.
func TestBudgetPaceProjection(t *testing.T) {
	l := New()
	_, mid := paceTestMonth(t)
	// 60k booked at mid-month projects to ~115k–128k (28–31 day
	// months) on a 100k budget: an overrun for every month length.
	mustPaceSpend(t, l, "p1", "payer", 30000, mid)
	mustPaceSpend(t, l, "p2", "payer", 30000, mid)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	rows := l.BudgetPace(mid)
	if len(rows) != 1 {
		t.Fatalf("BudgetPace rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.UsedCents != 60000 {
		t.Fatalf("UsedCents = %d, want 60000", r.UsedCents)
	}
	monthStart := time.Date(mid.Year(), mid.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	elapsed := float64(mid.Sub(monthStart)) / float64(monthEnd.Sub(monthStart))
	if math.Abs(r.ElapsedFraction-elapsed) > 1e-9 {
		t.Fatalf("ElapsedFraction = %v, want %v", r.ElapsedFraction, elapsed)
	}
	// int64 truncation of used/elapsed: within 1 cent of the exact
	// ratio.
	want := 60000.0 / elapsed
	if math.Abs(float64(r.ProjectedCents)-want) > 1 {
		t.Fatalf("ProjectedCents = %d, want ~%.1f", r.ProjectedCents, want)
	}
	if math.Abs(r.ProjectedUtilization-float64(r.ProjectedCents)/100000) > 1e-9 {
		t.Fatalf("ProjectedUtilization = %v, want projected/100000", r.ProjectedUtilization)
	}
	// ~115k–128k projected on a 100k budget: over budget at the current
	// pace, for every month length.
	if !r.PaceAlert {
		t.Fatalf("PaceAlert = false, want true (projected %d > budget 100000)", r.ProjectedCents)
	}
}

// The pace alert is a once-per-month transition: the first BudgetPace
// call that projects an overrun fires the audit event and bumps the
// counter; later calls in the same month stay silent.
func TestBudgetPaceAlertFiresOnce(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	_, mid := paceTestMonth(t)
	mustPaceSpend(t, l, "p1", "payer", 60000, mid)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	if got := l.BudgetPaceAlertCount(); got != 0 {
		t.Fatalf("alert count before = %d, want 0", got)
	}
	rows := l.BudgetPace(mid)
	if !rows[0].PaceAlert {
		t.Fatalf("PaceAlert = false, want true")
	}
	if got := l.BudgetPaceAlertCount(); got != 1 {
		t.Fatalf("alert count = %d, want 1", got)
	}
	// Second call in the same month: the projection is unchanged, the
	// alert must not fire again.
	l.BudgetPace(mid)
	if got := l.BudgetPaceAlertCount(); got != 1 {
		t.Fatalf("alert count after second call = %d, want 1", got)
	}

	now := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	alerts := 0
	for _, ev := range events {
		if ev.Op == "budget_pace_alert" {
			alerts++
			if ev.Details["account"] != "payer" || ev.Details["currency"] != "USD" {
				t.Fatalf("alert details = %v, want payer/USD", ev.Details)
			}
			if ev.VersionBefore != ev.VersionAfter {
				t.Fatalf("alert moved version %d -> %d, want structural (no bump)", ev.VersionBefore, ev.VersionAfter)
			}
		}
	}
	if alerts != 1 {
		t.Fatalf("budget_pace_alert events = %d, want 1", alerts)
	}
}

// The alert ratio is opt-in configurable: a lower ratio pages earlier.
func TestBudgetPaceAlertRatio(t *testing.T) {
	for _, bad := range []float64{0, -0.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		l := New()
		if err := l.SetBudgetPaceAlertRatio(bad); err == nil {
			t.Fatalf("SetBudgetPaceAlertRatio(%v): want error", bad)
		}
	}
	l := New()
	_, mid := paceTestMonth(t)
	mustPaceSpend(t, l, "p1", "payer", 45000, mid)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	// 45k at half month projects to ~90k: under the default 1.0
	// threshold, over a 0.8 early-warning threshold.
	if rows := l.BudgetPace(mid); rows[0].PaceAlert {
		t.Fatalf("default ratio: PaceAlert = true, want false (projected %d)", rows[0].ProjectedCents)
	}
	if got := l.BudgetPaceAlertRatio(); got != DefaultBudgetPaceAlertRatio {
		t.Fatalf("BudgetPaceAlertRatio = %v, want %v", got, DefaultBudgetPaceAlertRatio)
	}
	if err := l.SetBudgetPaceAlertRatio(0.8); err != nil {
		t.Fatalf("SetBudgetPaceAlertRatio(0.8): %v", err)
	}
	if got := l.BudgetPaceAlertRatio(); got != 0.8 {
		t.Fatalf("BudgetPaceAlertRatio = %v, want 0.8", got)
	}
	// The 0.8 call above already fired the alert once (90k > 80k); the
	// flag must read true and the counter must be exactly 1.
	rows := l.BudgetPace(mid)
	if !rows[0].PaceAlert {
		t.Fatalf("ratio 0.8: PaceAlert = false, want true")
	}
	if got := l.BudgetPaceAlertCount(); got != 1 {
		t.Fatalf("alert count = %d, want 1", got)
	}
}

// Reconcile discloses every budget's pace but never fires alerts: it
// runs under the read lock and is read-only.
func TestBudgetPaceInReconcile(t *testing.T) {
	l := New()
	_, mid := paceTestMonth(t)
	mustPaceSpend(t, l, "p1", "payer", 60000, mid)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	rep := l.Reconcile(mid)
	if len(rep.BudgetPace) != 1 {
		t.Fatalf("reconcile BudgetPace rows = %d, want 1", len(rep.BudgetPace))
	}
	r := rep.BudgetPace[0]
	if r.Account != "payer" || r.Currency != "USD" || r.BudgetCents != 100000 || r.UsedCents != 60000 {
		t.Fatalf("reconcile pace row = %+v", r)
	}
	if !r.PaceAlert {
		t.Fatalf("reconcile PaceAlert = false, want true")
	}
	if got := l.BudgetPaceAlertCount(); got != 0 {
		t.Fatalf("alert count after Reconcile = %d, want 0", got)
	}
}

// At the month's first instant there is no run-rate: the projection is
// the booked spend. Past month end the trajectory is closed and the
// projection is the final booked spend.
func TestBudgetPaceMonthBoundaries(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	mustPaceSpend(t, l, "p1", "payer", 10000, monthStart)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	r := l.BudgetPace(monthStart)[0]
	if r.ElapsedFraction != 0 {
		t.Fatalf("ElapsedFraction at month start = %v, want 0", r.ElapsedFraction)
	}
	if r.ProjectedCents != 10000 {
		t.Fatalf("ProjectedCents at month start = %d, want 10000", r.ProjectedCents)
	}
	if r.PaceAlert {
		t.Fatalf("PaceAlert at month start = true, want false")
	}
	// Past month end collapses to the projection-equals-booked-spend
	// rule via projectPace (see TestProjectPaceBoundaries): the
	// defensive elapsed >= 1 branch cannot be reached through the
	// public entry, because utcMonth(now) always names now's own month.
}

// The projection math is exact and saturating: zero elapsed is the
// booked spend, full elapsed is the booked spend, and absurd run-rates
// saturate at MaxInt64 instead of overflowing.
func TestProjectPaceBoundaries(t *testing.T) {
	if got := projectPace(1000, 0); got != 1000 {
		t.Fatalf("projectPace(1000, 0) = %d, want 1000", got)
	}
	if got := projectPace(1000, 1); got != 1000 {
		t.Fatalf("projectPace(1000, 1) = %d, want 1000", got)
	}
	if got := projectPace(1000, -0.5); got != 1000 {
		t.Fatalf("projectPace(1000, -0.5) = %d, want 1000", got)
	}
	if got := projectPace(100, 0.5); got != 200 {
		t.Fatalf("projectPace(100, 0.5) = %d, want 200", got)
	}
	if got := projectPace(math.MaxInt64, 0.5); got != math.MaxInt64 {
		t.Fatalf("projectPace(MaxInt64, 0.5) = %d, want MaxInt64", got)
	}
}

// A zero budget admits no spend, so the projection is necessarily 0
// and the utilization ratio is 0/0 -> 0: no alert, no division panic.
func TestBudgetPaceZeroBudget(t *testing.T) {
	l := New()
	_, mid := paceTestMonth(t)
	if err := l.SetAccountBudget("payer", "USD", 0); err != nil {
		t.Fatalf("SetAccountBudget(0): %v", err)
	}
	r := l.BudgetPace(mid)[0]
	if r.ProjectedCents != 0 || r.ProjectedUtilization != 0 || r.PaceAlert {
		t.Fatalf("zero-budget pace = %+v, want all-zero and no alert", r)
	}
}

// The pace is advisory: calling BudgetPace never blocks or permits
// postings — a projected overrun does not reject, and the hard
// ErrBudgetExceeded gate still rejects actual overruns.
func TestBudgetPaceDoesNotGate(t *testing.T) {
	l := New()
	_, mid := paceTestMonth(t)
	if err := l.SetAccountBudget("payer", "USD", 100000); err != nil {
		t.Fatalf("SetAccountBudget: %v", err)
	}
	// 60k at half month: projected ~120k > budget — alert fires, but the
	// budget's own accounting is untouched.
	mustPaceSpend(t, l, "p1", "payer", 60000, mid)
	rows := l.BudgetPace(mid)
	if !rows[0].PaceAlert {
		t.Fatalf("PaceAlert = false, want true")
	}
	// A posting that would actually breach the budget is still rejected
	// by the hard gate — the pace alert did not consume or move budget.
	if _, _, err := l.Post(JournalEntry{
		ID: "breach", DebitAccount: "merchant", CreditAccount: "payer",
		AmountCents: 50000, Currency: "USD", CreatedAt: mid,
	}); err == nil {
		t.Fatalf("breaching post: want ErrBudgetExceeded, got nil")
	}
}
