package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// lateFeeSeed opens an installment plan (buyer pays merchant 1000 USD
// in 3 monthly periods, first due 2026-01-01) and funds buyer.
func lateFeeSeed(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	seedBalance(t, l, "buyer", 100000)
	mustOpenInstallmentPlan(t, l, "ip-lf")
	return l
}

func mustSetLateFee(t *testing.T, l *Ledger, rule InstallmentLateFeeRule) {
	t.Helper()
	if err := l.SetInstallmentLateFee("ip-lf", rule); err != nil {
		t.Fatalf("SetInstallmentLateFee: %v", err)
	}
}

func TestSetInstallmentLateFeeValidation(t *testing.T) {
	l := lateFeeSeed(t)
	good := InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: 3650, FixedCentsPerDay: 10}

	cases := []struct {
		name string
		rule InstallmentLateFeeRule
	}{
		{"empty fee account", InstallmentLateFeeRule{DailyRateBps: 3650}},
		{"fee account with slash", InstallmentLateFeeRule{FeeAccount: "fee/income", DailyRateBps: 3650}},
		{"rate out of range", InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: 10001}},
		{"negative rate", InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: -1}},
		{"negative fixed", InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: -5}},
		{"negative cap", InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: 3650, MaxTotalCents: -1}},
		{"zero rule books nothing", InstallmentLateFeeRule{FeeAccount: "fee-income"}},
	}
	for _, tc := range cases {
		if err := l.SetInstallmentLateFee("ip-lf", tc.rule); !errors.Is(err, ErrInvalidInstallmentLateFeeRule) {
			t.Errorf("%s: err = %v, want ErrInvalidInstallmentLateFeeRule", tc.name, err)
		}
	}
	if err := l.SetInstallmentLateFee("no-such-plan", good); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}

	mustSetLateFee(t, l, good)
	got, ok := l.InstallmentLateFee("ip-lf")
	if !ok || got != good {
		t.Fatalf("InstallmentLateFee = %+v, %v, want %+v, true", got, ok, good)
	}
	if _, ok := l.InstallmentLateFee("no-such-plan"); ok {
		t.Error("InstallmentLateFee on unknown plan returned a rule")
	}

	// Replace, then clear.
	replacement := InstallmentLateFeeRule{FeeAccount: "fee-income-2", FixedCentsPerDay: 5}
	mustSetLateFee(t, l, replacement)
	if got, _ := l.InstallmentLateFee("ip-lf"); got.FeeAccount != "fee-income-2" {
		t.Errorf("replaced rule = %+v, want fee-income-2", got)
	}
	if err := l.ClearInstallmentLateFee("ip-lf"); err != nil {
		t.Fatalf("ClearInstallmentLateFee: %v", err)
	}
	if _, ok := l.InstallmentLateFee("ip-lf"); ok {
		t.Error("rule still present after clear")
	}
	// Clearing again is a no-op; clearing an unknown plan errors.
	if err := l.ClearInstallmentLateFee("ip-lf"); err != nil {
		t.Errorf("re-clear: %v, want nil", err)
	}
	if err := l.ClearInstallmentLateFee("no-such-plan"); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("clear unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
}

func TestAccrueInstallmentLateFeeFixed(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	// Period 1 due 2026-01-01, never posted: the sweep of 2026-01-05
	// accrues one day's fixed fee for it.
	asof := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	results, err := l.AccrueInstallmentLateFees(asof)
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if r.PlanID != "ip-lf" || r.Period != 1 {
		t.Errorf("result = %+v, want plan ip-lf period 1", r)
	}
	if r.AmountCents != 10 || !r.Posted || r.Duplicate || r.Capped {
		t.Errorf("result = %+v, want amount 10 posted duplicate=false capped=false", r)
	}
	wantID := "installment-latefee/ip-lf/1/2026-01-05"
	if r.EntryID != wantID {
		t.Errorf("EntryID = %q, want %q", r.EntryID, wantID)
	}
	if r.OutstandingCents != 333 {
		t.Errorf("OutstandingCents = %d, want 333", r.OutstandingCents)
	}

	// The fee leg direction: debit the fee account, credit the payer.
	var entry JournalEntry
	found := false
	for _, e := range l.Entries() {
		if e.ID == wantID {
			entry, found = e, true
			break
		}
	}
	if !found {
		t.Fatalf("fee entry %q not in journal", wantID)
	}
	if entry.DebitAccount != "fee-income" || entry.CreditAccount != "buyer" {
		t.Errorf("legs = %q/%q, want fee-income/buyer", entry.DebitAccount, entry.CreditAccount)
	}
	if entry.AmountCents != 10 || entry.Currency != "USD" {
		t.Errorf("entry = %+v, want amount 10 USD", entry)
	}
	if entry.IdempotencyKey != wantID {
		t.Errorf("IdempotencyKey = %q, want the entry ID", entry.IdempotencyKey)
	}
	if got := l.Balance("buyer"); got != 100000-10 {
		t.Errorf("buyer balance = %d, want %d", got, 100000-10)
	}
	if got := l.Balance("fee-income"); got != 10 {
		t.Errorf("fee-income balance = %d, want 10", got)
	}
}

func TestAccrueInstallmentLateFeeIdempotentPerDay(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	asof := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	first, err := l.AccrueInstallmentLateFees(asof)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	second, err := l.AccrueInstallmentLateFees(asof)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(second) != 1 || !second[0].Duplicate || second[0].AmountCents != first[0].AmountCents {
		t.Errorf("second sweep = %+v, want one duplicate of %v", second, first[0])
	}
	if got := l.Balance("buyer"); got != 100000-10 {
		t.Errorf("buyer balance after replay = %d, want %d (no double-booking)", got, 100000-10)
	}

	// A new day accrues a new row with a new deterministic ID.
	third, err := l.AccrueInstallmentLateFees(asof.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("next-day sweep: %v", err)
	}
	if len(third) != 1 || third[0].Duplicate || third[0].EntryID != "installment-latefee/ip-lf/1/2026-01-06" {
		t.Errorf("next-day sweep = %+v, want fresh 2026-01-06 row", third)
	}
	if got := l.Balance("buyer"); got != 100000-20 {
		t.Errorf("buyer balance = %d, want %d", got, 100000-20)
	}
}

func TestAccrueInstallmentLateFeeRate(t *testing.T) {
	l := New()
	seedBalance(t, l, "buyer", 10000000)
	plan, err := l.OpenInstallmentPlan(OpenInstallmentPlanInput{
		ID:             "ip-rate",
		Payer:          "buyer",
		Payee:          "merchant",
		PrincipalCents: 109500,
		NumPeriods:     3,
		Rule:           InstallmentMonthly,
		FirstDueAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Currency:       "USD",
	})
	if err != nil {
		t.Fatalf("OpenInstallmentPlan: %v", err)
	}
	if installmentPeriodAmount(plan, 1) != 36500 {
		t.Fatalf("period amount = %d, want 36500", installmentPeriodAmount(plan, 1))
	}
	// 10000 bps = 100% annual -> 36500 * 10000 / 10000 / 365 = 100/day.
	if err := l.SetInstallmentLateFee("ip-rate", InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: 10000}); err != nil {
		t.Fatalf("SetInstallmentLateFee: %v", err)
	}

	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].AmountCents != 100 {
		t.Errorf("rate fee = %d, want 100", results[0].AmountCents)
	}
}

func TestAccrueInstallmentLateFeeSkipsNotDue(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	// Before the first due date: nothing is overdue, nothing accrues.
	results, err := l.AccrueInstallmentLateFees(time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 before any due date", len(results))
	}
}

func TestAccrueInstallmentLateFeeSkipsWithoutRule(t *testing.T) {
	l := lateFeeSeed(t)
	// No rule installed: the sweep ignores the plan entirely.
	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 without a rule", len(results))
	}
}

func TestAccrueInstallmentLateFeeSkipsTerminal(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	// Settle early: the plan is terminal and all periods are marked
	// posted — the sweep must not invent fees on the past.
	if _, err := l.SettleInstallmentEarly("ip-lf", time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("SettleInstallmentEarly: %v", err)
	}
	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 on a terminal plan", len(results))
	}
}

func TestAccrueInstallmentLateFeeOnlyUnpostedPeriods(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	// Post period 1 honestly first: the fee sweep must only charge
	// period 2 (due 2026-02-01), not the settled one.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("InstallmentsDue: %v", err)
	}
	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (period 2 only)", len(results))
	}
	if results[0].Period != 2 {
		t.Errorf("period = %d, want 2", results[0].Period)
	}
	if results[0].OutstandingCents != 333 {
		t.Errorf("OutstandingCents = %d, want 333", results[0].OutstandingCents)
	}
}

func TestAccrueInstallmentLateFeeCap(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10, MaxTotalCents: 15})

	d1 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	r1, err := l.AccrueInstallmentLateFees(d1)
	if err != nil {
		t.Fatalf("day 1: %v", err)
	}
	if len(r1) != 1 || r1[0].AmountCents != 10 || r1[0].Capped {
		t.Fatalf("day 1 = %+v, want full 10 uncapped", r1)
	}

	// Day 2: only 5 of the 10 fit under the cap of 15.
	r2, err := l.AccrueInstallmentLateFees(d1.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("day 2: %v", err)
	}
	if len(r2) != 1 || r2[0].AmountCents != 5 || !r2[0].Capped {
		t.Fatalf("day 2 = %+v, want 5 capped", r2)
	}

	// Day 3: the cap is exhausted — nothing books, but the outcome is
	// reported as capped.
	r3, err := l.AccrueInstallmentLateFees(d1.AddDate(0, 0, 2))
	if err != nil {
		t.Fatalf("day 3: %v", err)
	}
	if len(r3) != 1 || r3[0].Posted || r3[0].AmountCents != 0 || !r3[0].Capped {
		t.Fatalf("day 3 = %+v, want unposted capped with 0", r3)
	}
	if got := l.Balance("fee-income"); got != 15 {
		t.Errorf("fee-income balance = %d, want 15 (capped total)", got)
	}
}

func TestAccrueInstallmentLateFeeCapNetsReversals(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10, MaxTotalCents: 15})

	d1 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	if _, err := l.AccrueInstallmentLateFees(d1); err != nil {
		t.Fatalf("day 1: %v", err)
	}
	// Reverse the day-1 fee: the cap net falls back to 0, so day 2 can
	// book the full 10 again.
	if _, _, err := l.ReverseEntryPartial("installment-latefee/ip-lf/1/2026-01-05", 10, "rev-lf-1"); err != nil {
		t.Fatalf("ReverseEntryPartial: %v", err)
	}
	r2, err := l.AccrueInstallmentLateFees(d1.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("day 2: %v", err)
	}
	if len(r2) != 1 || r2[0].AmountCents != 10 || r2[0].Capped {
		t.Fatalf("day 2 = %+v, want full 10 uncapped after reversal", r2)
	}
}

func TestAccrueInstallmentLateFeeFrozenPayer(t *testing.T) {
	l := lateFeeSeed(t)
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})

	l.Freeze("buyer")
	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("AccrueInstallmentLateFees on a frozen payer returned nil error")
	}
	if !strings.Contains(err.Error(), "frozen") {
		t.Errorf("err = %v, want a frozen-account refusal", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 when the fee is blocked", len(results))
	}
	if got := l.Balance("fee-income"); got != 0 {
		t.Errorf("fee-income balance = %d, want 0", got)
	}
}

func TestAccrueInstallmentLateFeeRateDust(t *testing.T) {
	l := lateFeeSeed(t)
	// 1 bps annual on 333 cents: floor(333*1/10000/365) = 0 — the
	// period is overdue but books nothing, silently.
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", DailyRateBps: 1})

	results, err := l.AccrueInstallmentLateFees(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccrueInstallmentLateFees: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 for rate dust", len(results))
	}
}

func TestInstallmentLateFeeAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	seedBalance(t, l, "buyer", 100000)
	mustOpenInstallmentPlan(t, l, "ip-lf")
	mustSetLateFee(t, l, InstallmentLateFeeRule{FeeAccount: "fee-income", FixedCentsPerDay: 10})
	if err := l.ClearInstallmentLateFee("ip-lf"); err != nil {
		t.Fatalf("ClearInstallmentLateFee: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	byOp := auditEventsByOp(t, dir)
	set := byOp["installment_late_fee_set"]
	if len(set) != 1 {
		t.Fatalf("set events = %d, want 1", len(set))
	}
	if set[0].Details["fee_account"] != "fee-income" {
		t.Errorf("set details = %v, want fee_account=fee-income", set[0].Details)
	}
	if len(byOp["installment_late_fee_cleared"]) != 1 {
		t.Fatalf("cleared events = %d, want 1", len(byOp["installment_late_fee_cleared"]))
	}
}
