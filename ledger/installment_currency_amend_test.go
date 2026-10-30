package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInstallmentPlanCurrencyValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendInstallmentPlanCurrency("ip-unknown", "EUR"); !errors.Is(err, ErrInstallmentNotFound) {
		t.Errorf("unknown plan: err = %v, want ErrInstallmentNotFound", err)
	}
	mustOpenInstallmentPlan(t, l, "ip-currency")

	// Terminal: completed. Sweeping all three monthly periods
	// completes the plan.
	if _, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("sweep to completion: %v", err)
	}
	plan, _ := l.InstallmentPlan("ip-currency")
	if plan.Status != InstallmentCompleted {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
	if _, err := l.AmendInstallmentPlanCurrency("ip-currency", "EUR"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("completed plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Terminal: settled_early.
	l2 := New()
	mustOpenInstallmentPlan(t, l2, "ip-currency")
	if _, err := l2.SettleInstallmentEarly("ip-currency", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("early settle: %v", err)
	}
	if _, err := l2.AmendInstallmentPlanCurrency("ip-currency", "EUR"); !errors.Is(err, ErrInstallmentBadTransition) {
		t.Errorf("settled_early plan: err = %v, want ErrInstallmentBadTransition", err)
	}

	// Invalid currencies fail with the creation gate's error, and
	// every rejection leaves the original plan exactly as it was.
	l3 := New()
	orig3 := mustOpenInstallmentPlan(t, l3, "ip-currency")
	for _, bad := range []string{"usd", "US", "USDD", "U$D", "eur"} {
		if _, err := l3.AmendInstallmentPlanCurrency("ip-currency", bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("currency %q: err = %v, want ErrInvalidCurrency", bad, err)
		}
		got, _ := l3.InstallmentPlan("ip-currency")
		if !reflect.DeepEqual(got, orig3) {
			t.Errorf("plan after currency %q rejection = %+v, want %+v (untouched)", bad, got, orig3)
		}
	}

	// Amending to the current currency is a no-op, not a change —
	// and an empty code normalizes to the default currency, so it is
	// a no-op on a USD plan too.
	if got, err := l3.AmendInstallmentPlanCurrency("ip-currency", "USD"); err != nil || !reflect.DeepEqual(got, orig3) {
		t.Errorf("same-currency amendment = %+v, %v, want %+v, nil", got, err, orig3)
	}
	if got, err := l3.AmendInstallmentPlanCurrency("ip-currency", ""); err != nil || !reflect.DeepEqual(got, orig3) {
		t.Errorf("empty-code amendment = %+v, %v, want %+v, nil (empty normalizes to USD)", got, err, orig3)
	}
}

func TestAmendInstallmentPlanCurrencySweepHonorsNewCurrency(t *testing.T) {
	// The new currency governs unposted periods from the next sweep:
	// period 1 posts in USD, the currency moves to EUR, and periods
	// 2 and 3 book in EUR — while period 1's journaled row stays
	// USD and the per-currency balances never mix.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-currency-sweep")
	res, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(res) != 1 || res[0].Period != 1 || res[0].AmountCents != 333 {
		t.Fatalf("first sweep = %+v, want one period 1 of 333", res)
	}
	// Baseline for the untouched-fields check: the plan as it stands
	// after period 1 posted, before the amendment.
	orig, _ := l.InstallmentPlan("ip-currency-sweep")

	amended, err := l.AmendInstallmentPlanCurrency("ip-currency-sweep", "EUR")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanCurrency: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want %q", amended.Currency, "EUR")
	}
	// Only the currency moves: payer, payee, principal, period
	// count, rule, first due date, legs, status, and posted periods
	// are untouched.
	amended.Currency = orig.Currency
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended plan = %+v, want %+v except currency", amended, orig)
	}
	// The amendment itself posts nothing.
	if got := len(l.Entries()); got != 1 {
		t.Fatalf("entries after amendment = %d, want 1 (amendment posts nothing)", got)
	}

	res, err = l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("second sweep posted %d periods, want 2", len(res))
	}

	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// Period 1's journaled row is untouched: still USD.
	if first := entries[0]; first.ID != "installment/ip-currency-sweep/1" || first.Currency != "USD" || first.AmountCents != 333 {
		t.Errorf("period 1 row = %+v, want installment/ip-currency-sweep/1 of 333 USD", first)
	}
	// Periods 2 and 3 book in the new currency.
	for i, want := range []struct {
		id  string
		amt int64
	}{
		{"installment/ip-currency-sweep/2", 333},
		{"installment/ip-currency-sweep/3", 334},
	} {
		if got := entries[i+1]; got.ID != want.id || got.Currency != "EUR" || got.AmountCents != want.amt {
			t.Errorf("period %d row = %+v, want %s of %d EUR", i+2, got, want.id, want.amt)
		}
	}
	plan, _ := l.InstallmentPlan("ip-currency-sweep")
	if plan.Status != InstallmentCompleted || plan.PostedPeriods != 3 {
		t.Errorf("plan = %+v, want completed with 3 posted periods", plan)
	}
	// Per-currency balances never mix: USD keeps only period 1,
	// EUR holds periods 2 and 3.
	if got := l.BalanceIn("merchant", "USD"); got != 333 {
		t.Errorf("merchant USD balance = %d, want 333", got)
	}
	if got := l.BalanceIn("buyer", "USD"); got != -333 {
		t.Errorf("buyer USD balance = %d, want -333", got)
	}
	if got := l.BalanceIn("merchant", "EUR"); got != 667 {
		t.Errorf("merchant EUR balance = %d, want 667", got)
	}
	if got := l.BalanceIn("buyer", "EUR"); got != -667 {
		t.Errorf("buyer EUR balance = %d, want -667", got)
	}
}

func TestAmendInstallmentPlanCurrencyOverdue(t *testing.T) {
	// Overdue is a hold, not a terminal state: an overdue plan may
	// be amended, and the new currency governs the catch-up sweep.
	l := New()
	mustOpenInstallmentPlan(t, l, "ip-currency-overdue")
	// Post period 1, then freeze the payer so period 2 fails.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	l.Freeze("buyer")
	if _, err := l.InstallmentsDue(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("sweep with frozen payer: want error, got nil")
	}
	plan, _ := l.InstallmentPlan("ip-currency-overdue")
	if plan.Status != InstallmentOverdue {
		t.Fatalf("status = %q, want %q", plan.Status, InstallmentOverdue)
	}
	// Amend while overdue: USD -> EUR.
	amended, err := l.AmendInstallmentPlanCurrency("ip-currency-overdue", "EUR")
	if err != nil {
		t.Fatalf("AmendInstallmentPlanCurrency overdue: %v", err)
	}
	if amended.Currency != "EUR" || amended.Status != InstallmentOverdue {
		t.Errorf("amended overdue = currency %q status %q, want EUR overdue", amended.Currency, amended.Status)
	}
	// Unfreeze: the catch-up sweep posts periods 2 and 3 in EUR and
	// recovers the plan. (Monthly rule: period 3 is not due until
	// 2026-03-01, so the catch-up sweep runs 2026-03-15.)
	l.Unfreeze("buyer")
	res, err := l.InstallmentsDue(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("catch-up sweep: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("catch-up sweep posted %d periods, want 2", len(res))
	}
	entries := l.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Currency != "USD" || entries[1].Currency != "EUR" || entries[2].Currency != "EUR" {
		t.Errorf("entry currencies = %q, %q, %q, want USD, EUR, EUR", entries[0].Currency, entries[1].Currency, entries[2].Currency)
	}
	plan, _ = l.InstallmentPlan("ip-currency-overdue")
	if plan.Status != InstallmentCompleted {
		t.Errorf("status = %q, want %q", plan.Status, InstallmentCompleted)
	}
}

func TestAmendInstallmentPlanCurrencyAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustOpenInstallmentPlan(t, l, "ip-currency-audit")
	// Post one period so the audit carries a posted-period count.
	if _, err := l.InstallmentsDue(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// The no-op revision emits nothing (same currency, and empty
	// normalizes to the plan's USD).
	if _, err := l.AmendInstallmentPlanCurrency("ip-currency-audit", "USD"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanCurrency("ip-currency-audit", ""); err != nil {
		t.Fatalf("empty-code amend: %v", err)
	}
	if _, err := l.AmendInstallmentPlanCurrency("ip-currency-audit", "EUR"); err != nil {
		t.Fatalf("AmendInstallmentPlanCurrency: %v", err)
	}
	if _, err := l.AmendInstallmentPlanCurrency("ip-currency-audit", "CNY"); err != nil {
		t.Fatalf("AmendInstallmentPlanCurrency second: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["installment_currency_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-ops must not audit)", len(events))
	}
	if events[0].TraceID != "ip-currency-audit" || events[0].Actor != "AmendInstallmentPlanCurrency" {
		t.Errorf("event trace/actor = %q/%q, want ip-currency-audit/AmendInstallmentPlanCurrency", events[0].TraceID, events[0].Actor)
	}
	d := events[0].Details
	if d["plan_id"] != "ip-currency-audit" ||
		d["previous_currency"] != "USD" ||
		d["currency"] != "EUR" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("event details = %+v, want ip-currency-audit USD->EUR posted_periods 1", d)
	}
	d = events[1].Details
	if d["previous_currency"] != "EUR" ||
		d["currency"] != "CNY" ||
		d["posted_periods"] != float64(1) {
		t.Errorf("second event details = %+v, want EUR->CNY posted_periods 1", d)
	}
}
