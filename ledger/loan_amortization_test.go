package ledger

import (
	"errors"
	"testing"
)

func TestAmortizationEqualPrincipal_HandComputed(t *testing.T) {
	// 12000 cents, 12% annual, monthly (1% per period), 3 periods,
	// equal principal: slice 4000; interest 120 / 80 / 40.
	sched, err := GenerateLoanAmortizationSchedule(LoanAmortizationTerms{
		PrincipalCents: 12000, AnnualRateBps: 1200, Periods: 3,
		Frequency: AmortizationMonthly, Mode: AmortizationEqualPrincipal,
	})
	if err != nil {
		t.Fatalf("GenerateLoanAmortizationSchedule: %v", err)
	}
	want := []LoanAmortizationPayment{
		{Period: 1, PaymentCents: 4120, InterestCents: 120, PrincipalCents: 4000, RemainingCents: 8000},
		{Period: 2, PaymentCents: 4080, InterestCents: 80, PrincipalCents: 4000, RemainingCents: 4000},
		{Period: 3, PaymentCents: 4040, InterestCents: 40, PrincipalCents: 4000, RemainingCents: 0},
	}
	if len(sched.Payments) != len(want) {
		t.Fatalf("payments = %d rows, want %d", len(sched.Payments), len(want))
	}
	for i, w := range want {
		if sched.Payments[i] != w {
			t.Errorf("row %d = %+v, want %+v", i+1, sched.Payments[i], w)
		}
	}
	if sched.TotalInterestCents != 240 || sched.TotalPrincipalCents != 12000 || sched.TotalPaymentCents != 12240 {
		t.Errorf("totals = payment %d interest %d principal %d, want 12240/240/12000",
			sched.TotalPaymentCents, sched.TotalInterestCents, sched.TotalPrincipalCents)
	}
}

func TestAmortizationEqualInstallment_HandComputed(t *testing.T) {
	// 10000 cents, 12% annual, monthly (1% per period), 2 periods,
	// annuity: payment = round(10000*0.01*1.01^2/(1.01^2-1)) = 5075.
	// Period 1: interest 100, principal 4975, remaining 5025.
	// Period 2 (true-up): interest round(50.25) = 50, principal
	// 5025, payment 5075, remaining 0.
	sched, err := GenerateLoanAmortizationSchedule(LoanAmortizationTerms{
		PrincipalCents: 10000, AnnualRateBps: 1200, Periods: 2,
		Frequency: AmortizationMonthly, Mode: AmortizationEqualInstallment,
	})
	if err != nil {
		t.Fatalf("GenerateLoanAmortizationSchedule: %v", err)
	}
	want := []LoanAmortizationPayment{
		{Period: 1, PaymentCents: 5075, InterestCents: 100, PrincipalCents: 4975, RemainingCents: 5025},
		{Period: 2, PaymentCents: 5075, InterestCents: 50, PrincipalCents: 5025, RemainingCents: 0},
	}
	if len(sched.Payments) != len(want) {
		t.Fatalf("payments = %d rows, want %d", len(sched.Payments), len(want))
	}
	for i, w := range want {
		if sched.Payments[i] != w {
			t.Errorf("row %d = %+v, want %+v", i+1, sched.Payments[i], w)
		}
	}
	if sched.TotalInterestCents != 150 || sched.TotalPrincipalCents != 10000 || sched.TotalPaymentCents != 10150 {
		t.Errorf("totals = payment %d interest %d principal %d, want 10150/150/10000",
			sched.TotalPaymentCents, sched.TotalInterestCents, sched.TotalPrincipalCents)
	}
}

func TestAmortizationTailTrueUp(t *testing.T) {
	// 100 cents over 3 periods at 0%: equal-principal slices floor
	// to 33, and the final period trues up to the 34-cent remainder.
	// The annuity path rounds the same way (33, 33, 34).
	for _, mode := range []string{AmortizationEqualPrincipal, AmortizationEqualInstallment} {
		sched, err := GenerateLoanAmortizationSchedule(LoanAmortizationTerms{
			PrincipalCents: 100, AnnualRateBps: 0, Periods: 3,
			Frequency: AmortizationMonthly, Mode: mode,
		})
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		var principals []int64
		for _, p := range sched.Payments {
			principals = append(principals, p.PrincipalCents)
		}
		if len(principals) != 3 || principals[0] != 33 || principals[1] != 33 || principals[2] != 34 {
			t.Errorf("mode %s: principals = %v, want [33 33 34]", mode, principals)
		}
		if sched.Payments[2].RemainingCents != 0 {
			t.Errorf("mode %s: final remaining = %d, want 0", mode, sched.Payments[2].RemainingCents)
		}
	}
}

func TestAmortizationInvariants(t *testing.T) {
	cases := []LoanAmortizationTerms{
		{PrincipalCents: 36000000, AnnualRateBps: 450, Periods: 360, Frequency: AmortizationMonthly, Mode: AmortizationEqualInstallment},
		{PrincipalCents: 36000000, AnnualRateBps: 450, Periods: 360, Frequency: AmortizationMonthly, Mode: AmortizationEqualPrincipal},
		{PrincipalCents: 500000, AnnualRateBps: 2400, Periods: 20, Frequency: AmortizationQuarterly, Mode: AmortizationEqualInstallment},
		{PrincipalCents: 500000, AnnualRateBps: 2400, Periods: 20, Frequency: AmortizationQuarterly, Mode: AmortizationEqualPrincipal},
		{PrincipalCents: 999999, AnnualRateBps: 777, Periods: 7, Frequency: AmortizationAnnual, Mode: AmortizationEqualInstallment},
		{PrincipalCents: 5, AnnualRateBps: 1200, Periods: 12, Frequency: AmortizationMonthly, Mode: AmortizationEqualPrincipal},
	}
	for _, terms := range cases {
		sched, err := GenerateLoanAmortizationSchedule(terms)
		if err != nil {
			t.Fatalf("terms %+v: %v", terms, err)
		}
		var sumPay, sumInt, sumPrin int64
		for i, p := range sched.Payments {
			if p.Period != i+1 {
				t.Errorf("terms %+v: row %d has period %d", terms, i, p.Period)
			}
			if p.PaymentCents != p.InterestCents+p.PrincipalCents {
				t.Errorf("terms %+v: row %d payment %d != interest %d + principal %d",
					terms, i+1, p.PaymentCents, p.InterestCents, p.PrincipalCents)
			}
			if p.RemainingCents < 0 {
				t.Errorf("terms %+v: row %d remaining %d < 0", terms, i+1, p.RemainingCents)
			}
			sumPay += p.PaymentCents
			sumInt += p.InterestCents
			sumPrin += p.PrincipalCents
		}
		if sumPrin != terms.PrincipalCents {
			t.Errorf("terms %+v: principal sum %d != principal %d", terms, sumPrin, terms.PrincipalCents)
		}
		if sched.Payments[len(sched.Payments)-1].RemainingCents != 0 {
			t.Errorf("terms %+v: final remaining != 0", terms)
		}
		if sumPay != sched.TotalPaymentCents || sumInt != sched.TotalInterestCents || sumPrin != sched.TotalPrincipalCents {
			t.Errorf("terms %+v: totals mismatch: sums %d/%d/%d, struct %d/%d/%d",
				terms, sumPay, sumInt, sumPrin,
				sched.TotalPaymentCents, sched.TotalInterestCents, sched.TotalPrincipalCents)
		}
		// The schedule is pure: regenerating it yields identical rows.
		again, err := GenerateLoanAmortizationSchedule(terms)
		if err != nil {
			t.Fatalf("terms %+v on regenerate: %v", terms, err)
		}
		if len(again.Payments) != len(sched.Payments) {
			t.Fatalf("terms %+v: regenerate row count differs", terms)
		}
		for i := range sched.Payments {
			if again.Payments[i] != sched.Payments[i] {
				t.Errorf("terms %+v: regenerate row %d differs", terms, i+1)
			}
		}
	}
}

func TestAmortizationValidation(t *testing.T) {
	base := LoanAmortizationTerms{
		PrincipalCents: 10000, AnnualRateBps: 1200, Periods: 12,
		Frequency: AmortizationMonthly, Mode: AmortizationEqualInstallment,
	}
	cases := []struct {
		name  string
		mutate func(*LoanAmortizationTerms)
		want  error
	}{
		{"zero principal", func(t *LoanAmortizationTerms) { t.PrincipalCents = 0 }, ErrInvalidLoanPrincipal},
		{"negative principal", func(t *LoanAmortizationTerms) { t.PrincipalCents = -1 }, ErrInvalidLoanPrincipal},
		{"zero periods", func(t *LoanAmortizationTerms) { t.Periods = 0 }, ErrInvalidLoanPeriods},
		{"negative rate", func(t *LoanAmortizationTerms) { t.AnnualRateBps = -1 }, ErrInvalidLoanRate},
		{"bad frequency", func(t *LoanAmortizationTerms) { t.Frequency = "weekly" }, ErrInvalidAmortizationFrequency},
		{"bad mode", func(t *LoanAmortizationTerms) { t.Mode = "balloon" }, ErrInvalidAmortizationMode},
	}
	for _, tc := range cases {
		terms := base
		tc.mutate(&terms)
		_, err := GenerateLoanAmortizationSchedule(terms)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestAmortizationFrequencies(t *testing.T) {
	// Same loan at quarterly cadence: 12000 cents, 12% annual =
	// 3% per quarter, 2 quarters, equal principal: interest
	// 360 then 180, slices 6000 each.
	sched, err := GenerateLoanAmortizationSchedule(LoanAmortizationTerms{
		PrincipalCents: 12000, AnnualRateBps: 1200, Periods: 2,
		Frequency: AmortizationQuarterly, Mode: AmortizationEqualPrincipal,
	})
	if err != nil {
		t.Fatalf("GenerateLoanAmortizationSchedule: %v", err)
	}
	if len(sched.Payments) != 2 ||
		sched.Payments[0].InterestCents != 360 || sched.Payments[0].PaymentCents != 6360 ||
		sched.Payments[1].InterestCents != 180 || sched.Payments[1].PaymentCents != 6180 ||
		sched.Payments[1].RemainingCents != 0 {
		t.Errorf("quarterly schedule = %+v, want interest 360/180 and payments 6360/6180", sched.Payments)
	}
}
