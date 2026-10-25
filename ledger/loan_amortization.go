package ledger

import (
	"errors"
	"math/big"
)

// Loan amortization schedule (LG-211, fintech: lending).
//
// A loan amortization schedule is a *forecast*, not a booking: it
// answers "if this loan runs to term at this rate, what does each
// instalment split into?" without posting anything. The rows it
// produces are never journal entries — actual money still moves only
// through Post / transfer (see schedule.go for the recurring-transfer
// execution plan, which is a different thing: a schedule of transfers
// to *execute*, while this module computes the *amounts* of a loan's
// instalments). Keeping the forecast pure is what makes it safe to
// show a borrower before any money moves: generating a schedule
// cannot create, move, or promise funds.
//
// This module is orthogonal to fixed-asset depreciation (see
// depreciation.go, LG-141): depreciation amortizes a capitalized
// asset's book value, this amortizes a loan's principal — same word,
// different balance sheet side, different math, no shared code.
//
// Amounts are int64 cents throughout, and every computation is exact
// integer / rational arithmetic — no float64 anywhere. A float
// amortization drifts by cents over a long term, and a drift that
// lands on the borrower (or the lender) is a billing dispute, not a
// rounding error. Interest for one period is the exact rational
// product `remaining * annualBps / (10000 * periodsPerYear)`,
// rounded to the nearest cent, halves away from zero (a borrower
// statement that rounds banker's-style on some rows and half-up on
// others is unauditable; one rule, stated here, applied everywhere).
//
// Two repayment modes, the two the backlog names:
//
//   - equal-installment (等额本息): every instalment is the same
//     amount (the annuity payment), so early instalments are mostly
//     interest and late ones mostly principal.
//   - equal-principal (等额本金): every instalment repays the same
//     principal slice, so the instalment shrinks as interest falls.
//
// Tail true-up, the invariant the whole module is built around: the
// final period's principal is always the *remaining* balance, never a
// computed slice — so the principal column sums to exactly the loan
// principal and the final remaining balance is exactly zero, whatever
// rounding did to the earlier rows. Without the true-up, a 360-month
// mortgage would end with a stray few cents either owed or
// overpaid; with it, the schedule closes clean by construction. The
// equal-principal mode additionally floors the per-period slice
// (`principal / periods`), so its true-up also absorbs the division
// remainder (a 100-cent loan over 3 periods repays 33, 33, 34).
//
// Zero-rate loans are not a special case in the caller: the annuity
// payment becomes `round(principal / periods)` and the true-up
// still closes the schedule, so a 0% loan and a 12% loan go through
// the same code path and the same tests.

// Repayment frequencies: how many instalments fall in one year.
const (
	// AmortizationMonthly is one instalment per month (12 per year).
	AmortizationMonthly = "monthly"
	// AmortizationQuarterly is one instalment per quarter (4 per year).
	AmortizationQuarterly = "quarterly"
	// AmortizationSemiannual is one instalment per half-year (2 per year).
	AmortizationSemiannual = "semiannual"
	// AmortizationAnnual is one instalment per year (1 per year).
	AmortizationAnnual = "annual"
)

// Repayment modes.
const (
	// AmortizationEqualInstallment is the annuity mode (等额本息):
	// a constant instalment, interest-heavy early, principal-heavy late.
	AmortizationEqualInstallment = "equal-installment"
	// AmortizationEqualPrincipal is the straight-principal mode
	// (等额本金): a constant principal slice, so instalments shrink
	// as the interest base falls.
	AmortizationEqualPrincipal = "equal-principal"
)

// Validation errors for loan amortization schedules.
var (
	// ErrInvalidLoanPrincipal is returned when the loan principal is
	// not positive: there is nothing to amortize.
	ErrInvalidLoanPrincipal = errors.New("ledger: loan principal must be positive")
	// ErrInvalidLoanPeriods is returned when the period count is not
	// positive: a schedule with no instalments is not a schedule.
	ErrInvalidLoanPeriods = errors.New("ledger: loan periods must be positive")
	// ErrInvalidLoanRate is returned when the annual rate is negative:
	// a negative rate would pay the borrower to borrow, which is a
	// different product (a subsidy), not a loan schedule.
	ErrInvalidLoanRate = errors.New("ledger: loan annual rate must be non-negative")
	// ErrInvalidAmortizationFrequency is returned when the frequency
	// is not monthly, quarterly, semiannual, or annual.
	ErrInvalidAmortizationFrequency = errors.New("ledger: amortization frequency must be \"monthly\", \"quarterly\", \"semiannual\", or \"annual\"")
	// ErrInvalidAmortizationMode is returned when the mode is not
	// equal-installment or equal-principal.
	ErrInvalidAmortizationMode = errors.New("ledger: amortization mode must be \"equal-installment\" or \"equal-principal\"")
	// ErrAmortizationUnderpayment is returned when an annuity
	// instalment would not cover one period's interest: the
	// schedule could never amortize, so it is refused instead of
	// emitting rows whose principal column goes negative.
	ErrAmortizationUnderpayment = errors.New("ledger: amortization instalment does not cover period interest")
)

// LoanAmortizationTerms is the input to a schedule: the loan's
// principal in cents, its annual rate in basis points (1200 =
// 12.00%), the number of instalments, the repayment frequency, and
// the repayment mode.
type LoanAmortizationTerms struct {
	PrincipalCents int64  `json:"principal_cents"`
	AnnualRateBps  int64  `json:"annual_rate_bps"`
	Periods        int    `json:"periods"`
	Frequency      string `json:"frequency"`
	Mode           string `json:"mode"`
}

// LoanAmortizationPayment is one instalment row: 1-based period,
// the instalment split into interest and principal, and the balance
// remaining after the instalment. PaymentCents always equals
// InterestCents + PrincipalCents, and the final row's
// RemainingCents is always zero (the tail true-up).
type LoanAmortizationPayment struct {
	Period         int   `json:"period"`
	PaymentCents   int64 `json:"payment_cents"`
	InterestCents  int64 `json:"interest_cents"`
	PrincipalCents int64 `json:"principal_cents"`
	RemainingCents int64 `json:"remaining_cents"`
}

// LoanAmortizationSchedule is the generated forecast: the rows plus
// the column totals. TotalPrincipalCents always equals the loan
// principal, and TotalPaymentCents always equals TotalInterestCents
// + TotalPrincipalCents — both are checked by construction, not by
// the caller.
type LoanAmortizationSchedule struct {
	Terms               LoanAmortizationTerms     `json:"terms"`
	Payments            []LoanAmortizationPayment `json:"payments"`
	TotalPaymentCents   int64                     `json:"total_payment_cents"`
	TotalInterestCents  int64                     `json:"total_interest_cents"`
	TotalPrincipalCents int64                     `json:"total_principal_cents"`
}

// amortizationPeriodsPerYear maps a frequency to its instalments
// per year, or 0 for an unknown frequency.
func amortizationPeriodsPerYear(frequency string) int64 {
	switch frequency {
	case AmortizationMonthly:
		return 12
	case AmortizationQuarterly:
		return 4
	case AmortizationSemiannual:
		return 2
	case AmortizationAnnual:
		return 1
	default:
		return 0
	}
}

// roundHalfUpRat rounds a non-negative rational to the nearest
// integer, halves away from zero: floor(x + 1/2). All inputs here
// are non-negative (principal, rate, and balances are validated
// positive / non-negative before any rounding runs), so the
// non-negative branch is the whole function — a negative input
// would be a caller bug, not a rounding case.
func roundHalfUpRat(x *big.Rat) int64 {
	shifted := new(big.Rat).Add(x, big.NewRat(1, 2))
	num := shifted.Num()
	den := shifted.Denom()
	q := new(big.Int).Quo(num, den)
	return q.Int64()
}

// amortizationPeriodInterest returns one period's interest in cents:
// round(remaining * annualBps / (10000 * periodsPerYear)), computed
// exactly in big.Rat so no float error ever enters the schedule.
func amortizationPeriodInterest(remaining, annualRateBps, periodsPerYear int64) int64 {
	interest := new(big.Rat).SetInt64(remaining)
	interest.Mul(interest, big.NewRat(annualRateBps, 10000*periodsPerYear))
	return roundHalfUpRat(interest)
}

// amortizationAnnuityPayment returns the constant instalment for
// the equal-installment mode: round(P * r * (1+r)^n /
// ((1+r)^n - 1)) with r the per-period rate as an exact rational,
// or round(P / n) for a zero rate (the formula's 0/0 limit).
func amortizationAnnuityPayment(principalCents, annualRateBps, periodsPerYear int64, periods int) int64 {
	if annualRateBps == 0 {
		exact := new(big.Rat).SetFrac64(principalCents, int64(periods))
		return roundHalfUpRat(exact)
	}
	rate := big.NewRat(annualRateBps, 10000*periodsPerYear)
	onePlus := new(big.Rat).Add(big.NewRat(1, 1), rate)
	pow := new(big.Rat).SetInt64(1)
	for i := 0; i < periods; i++ {
		pow.Mul(pow, onePlus)
	}
	numerator := new(big.Rat).SetInt64(principalCents)
	numerator.Mul(numerator, rate)
	numerator.Mul(numerator, pow)
	denominator := new(big.Rat).Sub(pow, big.NewRat(1, 1))
	exact := new(big.Rat).Quo(numerator, denominator)
	return roundHalfUpRat(exact)
}

// GenerateLoanAmortizationSchedule builds the forecast schedule for
// the given terms. It is pure and read-only: it touches no ledger
// state, posts no entries, and bumps no version — the schedule is a
// prediction the caller may show, store, or discard, and the actual
// repayments, when they happen, still go through Post / transfer.
//
// Fail-closed validation, in order: principal, periods, rate,
// frequency, mode. A schedule that cannot close (an annuity
// instalment that would not cover one period's interest, or an
// intermediate principal slice that would overrun the balance) is
// refused with ErrAmortizationUnderpayment rather than emitted
// with a negative or overrun row.
func GenerateLoanAmortizationSchedule(terms LoanAmortizationTerms) (LoanAmortizationSchedule, error) {
	if terms.PrincipalCents <= 0 {
		return LoanAmortizationSchedule{}, ErrInvalidLoanPrincipal
	}
	if terms.Periods <= 0 {
		return LoanAmortizationSchedule{}, ErrInvalidLoanPeriods
	}
	if terms.AnnualRateBps < 0 {
		return LoanAmortizationSchedule{}, ErrInvalidLoanRate
	}
	periodsPerYear := amortizationPeriodsPerYear(terms.Frequency)
	if periodsPerYear == 0 {
		return LoanAmortizationSchedule{}, ErrInvalidAmortizationFrequency
	}
	if terms.Mode != AmortizationEqualInstallment && terms.Mode != AmortizationEqualPrincipal {
		return LoanAmortizationSchedule{}, ErrInvalidAmortizationMode
	}

	payments := make([]LoanAmortizationPayment, 0, terms.Periods)
	remaining := terms.PrincipalCents
	var totalPayment, totalInterest, totalPrincipal int64

	annuityPayment := int64(0)
	equalPrincipalBase := int64(0)
	if terms.Mode == AmortizationEqualInstallment {
		annuityPayment = amortizationAnnuityPayment(terms.PrincipalCents, terms.AnnualRateBps, periodsPerYear, terms.Periods)
	} else {
		// Floored slice; the division remainder is absorbed by
		// the final period's true-up, never spread as fractions
		// of a cent across the schedule.
		equalPrincipalBase = terms.PrincipalCents / int64(terms.Periods)
	}

	for period := 1; period <= terms.Periods; period++ {
		last := period == terms.Periods
		interest := amortizationPeriodInterest(remaining, terms.AnnualRateBps, periodsPerYear)
		var principal, payment int64
		if last {
			// Tail true-up: the final principal is the whole
			// remaining balance, so the schedule closes exact.
			principal = remaining
			payment = interest + principal
		} else if terms.Mode == AmortizationEqualInstallment {
			principal = annuityPayment - interest
			if principal <= 0 || principal > remaining {
				return LoanAmortizationSchedule{}, ErrAmortizationUnderpayment
			}
			payment = annuityPayment
		} else {
			principal = equalPrincipalBase
			payment = interest + principal
		}
		remaining -= principal
		payments = append(payments, LoanAmortizationPayment{
			Period:         period,
			PaymentCents:   payment,
			InterestCents:  interest,
			PrincipalCents: principal,
			RemainingCents: remaining,
		})
		totalPayment += payment
		totalInterest += interest
		totalPrincipal += principal
	}

	return LoanAmortizationSchedule{
		Terms:               terms,
		Payments:            payments,
		TotalPaymentCents:   totalPayment,
		TotalInterestCents:  totalInterest,
		TotalPrincipalCents: totalPrincipal,
	}, nil
}
