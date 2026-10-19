package ledger

import (
	"fmt"
	"testing"
	"time"
)

func mustOpenInstallmentPlan(t *testing.T, l *Ledger, id string) InstallmentPlan {
	t.Helper()
	plan, err := l.OpenInstallmentPlan(OpenInstallmentPlanInput{
		ID:             id,
		Payer:          "buyer",
		Payee:          "merchant",
		PrincipalCents: 1000,
		NumPeriods:     3,
		Rule:           InstallmentMonthly,
		FirstDueAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Currency:       "USD",
	})
	if err != nil {
		t.Fatalf("OpenInstallmentPlan: %v", err)
	}
	return plan
}

func TestOpenInstallmentPlanValidation(t *testing.T) {
	base := func() OpenInstallmentPlanInput {
		return OpenInstallmentPlanInput{
			ID:             "ip-valid",
			Payer:          "buyer",
			Payee:          "merchant",
			PrincipalCents: 1000,
			NumPeriods:     3,
			Rule:           InstallmentMonthly,
			FirstDueAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Currency:       "USD",
		}
	}
	cases := []struct {
		name   string
		mutate func(*OpenInstallmentPlanInput)
		want   error
	}{
		{"empty ID", func(in *OpenInstallmentPlanInput) { in.ID = "" }, ErrEmptyInstallmentID},
		{"empty payer", func(in *OpenInstallmentPlanInput) { in.Payer = "" }, ErrEmptyInstallmentPayer},
		{"empty payee", func(in *OpenInstallmentPlanInput) { in.Payee = "" }, ErrEmptyInstallmentPayee},
		{"same account", func(in *OpenInstallmentPlanInput) { in.Payee = in.Payer }, ErrInstallmentSameAccount},
		{"zero principal", func(in *OpenInstallmentPlanInput) { in.PrincipalCents = 0 }, ErrInvalidInstallmentAmount},
		{"negative principal", func(in *OpenInstallmentPlanInput) { in.PrincipalCents = -5 }, ErrInvalidInstallmentAmount},
		{"zero periods", func(in *OpenInstallmentPlanInput) { in.NumPeriods = 0 }, ErrInvalidInstallmentPeriods},
		{"bad rule", func(in *OpenInstallmentPlanInput) { in.Rule = "yearly" }, ErrInvalidInstallmentRule},
		{"zero due date", func(in *OpenInstallmentPlanInput) { in.FirstDueAt = time.Time{} }, ErrEmptyInstallmentDueDate},
		{"bad currency", func(in *OpenInstallmentPlanInput) { in.Currency = "usd" }, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			in := base()
			tc.mutate(&in)
			if _, err := l.OpenInstallmentPlan(in); err != tc.want {
				t.Errorf("OpenInstallmentPlan err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenInstallmentPlanDuplicateID(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-dup")
	in := OpenInstallmentPlanInput{
		ID: "ip-dup", Payer: "a", Payee: "b", PrincipalCents: 100,
		NumPeriods: 1, Rule: InstallmentDaily,
		FirstDueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := l.OpenInstallmentPlan(in); err != ErrInstallmentIDTaken {
		t.Errorf("second open err = %v, want %v", err, ErrInstallmentIDTaken)
	}
}

func TestOpenInstallmentPlanIdempotentKey(t *testing.T) {
	l := New()
	in := OpenInstallmentPlanInput{
		ID: "ip-key", Payer: "buyer", Payee: "merchant", PrincipalCents: 500,
		NumPeriods: 2, Rule: InstallmentWeekly,
		FirstDueAt:     time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		IdempotencyKey: "open-ip-key",
	}
	first, err := l.OpenInstallmentPlan(in)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := l.OpenInstallmentPlan(in)
	if err != nil {
		t.Fatalf("replay open: %v", err)
	}
	if first != second {
		t.Errorf("replay returned a different plan: %+v vs %+v", first, second)
	}
	if got := len(l.InstallmentPlans()); got != 1 {
		t.Errorf("InstallmentPlans() = %d, want 1", got)
	}
}

func TestInstallmentsDuePostsPeriodsInOrder(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-sweep")

	// Sweep before anything is due: nothing posts.
	res, err := l.InstallmentsDue(time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("InstallmentsDue: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("early sweep posted %d periods, want 0", len(res))
	}

	// Staggered sweeps: one period per due date.
	sweepAt := []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	}
	wantAmounts := []int64{333, 333, 334}
	for i, at := range sweepAt {
		res, err = l.InstallmentsDue(at)
		if err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
		if len(res) != 1 {
			t.Fatalf("sweep %d posted %d periods, want 1", i+1, len(res))
		}
		r := res[0]
		if r.Period != i+1 || r.AmountCents != wantAmounts[i] || !r.Posted || r.Duplicate {
			t.Errorf("sweep %d result = %+v, want period %d amount %d posted", i+1, r, i+1, wantAmounts[i])
		}
		wantDue := time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)
		if !r.DueAt.Equal(wantDue) {
			t.Errorf("sweep %d due = %v, want %v", i+1, r.DueAt, wantDue)
		}
		if wantID := fmt.Sprintf("installment/ip-sweep/%d", i+1); r.EntryID != wantID {
			t.Errorf("sweep %d entry ID = %q, want %q", i+1, r.EntryID, wantID)
		}
	}

	plan, ok := l.InstallmentPlan("ip-sweep")
	if !ok {
		t.Fatal("plan missing after sweeps")
	}
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if plan.PostedPeriods != 3 {
		t.Errorf("posted periods = %d, want 3", plan.PostedPeriods)
	}
	// Payee receives: debit the payee (balance grows); payer pays:
	// credit the payer (balance falls).
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000", got)
	}
	if got := l.BalanceIn("buyer", "USD"); got != -1000 {
		t.Errorf("buyer balance = %d, want -1000", got)
	}

	// A further sweep posts nothing new: the plan is terminal.
	nEntries := len(l.Entries())
	res, err = l.InstallmentsDue(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("terminal sweep: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("terminal sweep returned %d results, want 0", len(res))
	}
	if got := len(l.Entries()); got != nEntries {
		t.Errorf("terminal sweep journaled %d new entries, want 0", got-nEntries)
	}
}

func TestInstallmentsDueWeeklyRule(t *testing.T) {
	l := New()
	_, err := l.OpenInstallmentPlan(OpenInstallmentPlanInput{
		ID: "ip-weekly", Payer: "buyer", Payee: "merchant", PrincipalCents: 200,
		NumPeriods: 2, Rule: InstallmentWeekly,
		FirstDueAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	res, err := l.InstallmentsDue(time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("sweep posted %d periods, want 2", len(res))
	}
	if !res[1].DueAt.Equal(time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period 2 due = %v, want 2026-01-12", res[1].DueAt)
	}
}

func TestInstallmentsDueOverdueAndRecover(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-overdue")

	// Freeze the payer: the period posting is rejected and the plan
	// goes overdue.
	l.Freeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	if len(res) != 1 || res[0].Posted {
		t.Fatalf("sweep results = %+v, want one unposted period", res)
	}
	plan, _ := l.InstallmentPlan("ip-overdue")
	if plan.Status != InstallmentOverdue {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentOverdue)
	}
	if plan.PostedPeriods != 0 {
		t.Errorf("posted periods = %d, want 0", plan.PostedPeriods)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("failed sweep journaled %d entries, want 0", got)
	}

	// Unfreeze and sweep again: the plan recovers and catches up.
	l.Unfreeze("buyer")
	res, err = l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("recovery sweep: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("recovery sweep posted %d periods, want 3", len(res))
	}
	plan, _ = l.InstallmentPlan("ip-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}

	// The overdue and recovery transitions were audited.
	if err := al.Close(); err != nil {
		t.Fatalf("close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.TraceID == "ip-overdue" {
			seen[ev.Op] = true
		}
	}
	for _, op := range []string{"installment_overdue", "installment_recovered"} {
		if !seen[op] {
			t.Errorf("audit log missing %q for ip-overdue", op)
		}
	}
}

func TestSettleInstallmentEarly(t *testing.T) {
	l := New()
	_, err := l.OpenInstallmentPlan(OpenInstallmentPlanInput{
		ID: "ip-early", Payer: "buyer", Payee: "merchant", PrincipalCents: 1000,
		NumPeriods: 4, Rule: InstallmentMonthly,
		FirstDueAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Post period 1 normally.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Early-settle the rest: 1000 - 250 = 750 in one row.
	plan, err := l.SettleInstallmentEarly("ip-early", time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if plan.Status != InstallmentSettledEarly {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentSettledEarly)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000", got)
	}
	if got := l.BalanceIn("buyer", "USD"); got != -1000 {
		t.Errorf("buyer balance = %d, want -1000", got)
	}
	// The early-settle row is the deterministic ID.
	found := false
	for _, e := range l.Entries() {
		if e.ID == "installment/ip-early/early-settle" && e.AmountCents == 750 {
			found = true
		}
	}
	if !found {
		t.Error("early-settle row (750) not found in the journal")
	}
	// Re-running is idempotent.
	n := len(l.Entries())
	again, err := l.SettleInstallmentEarly("ip-early", time.Now())
	if err != nil {
		t.Fatalf("second early settle: %v", err)
	}
	if again.Status != InstallmentSettledEarly {
		t.Errorf("second early settle status = %q", again.Status)
	}
	if got := len(l.Entries()); got != n {
		t.Errorf("second early settle journaled %d new entries", got-n)
	}
	// Sweeps after early settlement post nothing.
	res, err := l.InstallmentsDue(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(res) != 0 {
		t.Errorf("post-settle sweep = %d results, err %v; want 0, nil", len(res), err)
	}
}

func TestSettleInstallmentEarlyNotFound(t *testing.T) {
	l := New()
	if _, err := l.SettleInstallmentEarly("nope", time.Now()); err != ErrInstallmentNotFound {
		t.Errorf("err = %v, want %v", err, ErrInstallmentNotFound)
	}
}

func TestInstallmentPlansSorted(t *testing.T) {
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-b")
	mustOpenInstallmentPlan(t, l, "ip-a")
	plans := l.InstallmentPlans()
	if len(plans) != 2 || plans[0].ID != "ip-a" || plans[1].ID != "ip-b" {
		t.Errorf("plans not sorted by ID: %+v", plans)
	}
	if _, ok := l.InstallmentPlan("missing"); ok {
		t.Error("InstallmentPlan(missing) returned ok")
	}
}
