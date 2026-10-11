package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var accrualDay = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func TestAccrueInterestDailyMath(t *testing.T) {
	l := New()
	// 3,650,000 cents at 10000 bps (100%/yr): annual = 3,650,000, daily
	// = 10,000 exactly.
	got, dup, err := l.AccrueInterest("interest-expense", "interest-payable", 3650000, 10000, accrualDay, 0)
	if err != nil || dup {
		t.Fatalf("AccrueInterest = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("accrued = %d, want 10000", got.AmountCents)
	}
	if got.DebitAccount != "interest-expense" || got.CreditAccount != "interest-payable" {
		t.Fatalf("legs = %s/%s, want interest-expense/interest-payable", got.DebitAccount, got.CreditAccount)
	}
	if !strings.HasPrefix(got.Memo, "accrual:interest") {
		t.Fatalf("memo = %q, want accrual:interest prefix", got.Memo)
	}
	// The entry ID is deterministic: same kind/accounts/date -> same ID.
	if !strings.HasSuffix(got.ID, "/2026-10-11") {
		t.Fatalf("accrual ID = %q, want date-suffixed deterministic ID", got.ID)
	}
	// Balances moved like an ordinary posting.
	if b := l.Balance("interest-expense"); b != 10000 {
		t.Fatalf("expense balance = %d, want 10000", b)
	}
	if b := l.Balance("interest-payable"); b != -10000 {
		t.Fatalf("payable balance = %d, want -10000", b)
	}
}

func TestAccrueInterestIdempotentPerDay(t *testing.T) {
	l := New()
	first, dup, err := l.AccrueInterest("ie", "ip", 3650000, 10000, accrualDay, 0)
	if err != nil || dup {
		t.Fatalf("first = dup=%v err=%v", dup, err)
	}
	ver := l.Version()
	second, dup, err := l.AccrueInterest("ie", "ip", 3650000, 10000, accrualDay, 0)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %q, want original %q", second.ID, first.ID)
	}
	if l.Version() != ver {
		t.Fatalf("replay bumped version %d -> %d", ver, l.Version())
	}
	// A different calendar day books a fresh accrual.
	day2 := accrualDay.Add(24 * time.Hour)
	third, dup, err := l.AccrueInterest("ie", "ip", 3650000, 10000, day2, 0)
	if err != nil || dup {
		t.Fatalf("next day = dup=%v err=%v", dup, err)
	}
	if third.ID == first.ID {
		t.Fatal("different day must produce a different entry ID")
	}
}

func TestAccrueFee(t *testing.T) {
	l := New()
	got, dup, err := l.AccrueFee("fee-expense", "fee-payable", 2500, accrualDay, 0)
	if err != nil || dup {
		t.Fatalf("AccrueFee = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 2500 {
		t.Fatalf("accrued = %d, want 2500", got.AmountCents)
	}
	if got.Memo != "accrual:fee" {
		t.Fatalf("memo = %q, want accrual:fee", got.Memo)
	}
}

func TestAccrualValidation(t *testing.T) {
	l := New()
	if _, _, err := l.AccrueInterest("e", "p", 0, 500, accrualDay, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("zero principal = %v, want ErrNonPositiveAmount", err)
	}
	if _, _, err := l.AccrueInterest("e", "p", 1000, 0, accrualDay, 0); !errors.Is(err, ErrInvalidAccrualRate) {
		t.Fatalf("zero rate = %v, want ErrInvalidAccrualRate", err)
	}
	if _, _, err := l.AccrueInterest("e", "p", 1000, 10001, accrualDay, 0); !errors.Is(err, ErrInvalidAccrualRate) {
		t.Fatalf("rate >100%% = %v, want ErrInvalidAccrualRate", err)
	}
	if _, _, err := l.AccrueFee("e", "p", -5, accrualDay, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("negative fee = %v, want ErrNonPositiveAmount", err)
	}
	if _, _, err := l.AccrueFee("e", "e", 100, accrualDay, 0); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("same account = %v, want ErrSameAccount", err)
	}
	if _, _, err := l.AccrueFee("e/x", "p", 100, accrualDay, 0); !errors.Is(err, ErrAccrualAccountSeparator) {
		t.Fatalf("slash account = %v, want ErrAccrualAccountSeparator", err)
	}
	if _, _, err := l.AccrueFee("e", "p", 100, accrualDay, -1); !errors.Is(err, ErrInvalidAccrualCap) {
		t.Fatalf("negative cap = %v, want ErrInvalidAccrualCap", err)
	}
	// Dust interest (rounds to zero cents for the day) books nothing.
	if _, _, err := l.AccrueInterest("e", "p", 1, 1, accrualDay, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("dust interest = %v, want ErrNonPositiveAmount", err)
	}
	if _, err := l.AccruedTotal("e", "p", "bogus", time.Time{}, time.Now()); !errors.Is(err, ErrInvalidAccrualKind) {
		t.Fatalf("bad kind = %v, want ErrInvalidAccrualKind", err)
	}
}

func TestAccruedTotalAndReversal(t *testing.T) {
	l := New()
	// accrualDay sits ahead of the test runner's wall clock, so the
	// open-ended totals use an explicit far-future upper bound.
	farFuture := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	d1 := accrualDay
	d2 := accrualDay.Add(24 * time.Hour)
	d3 := accrualDay.Add(48 * time.Hour)
	for _, d := range []time.Time{d1, d2, d3} {
		if _, _, err := l.AccrueFee("fe", "fp", 1000, d, 0); err != nil {
			t.Fatalf("accrue %v: %v", d, err)
		}
	}
	total, err := l.AccruedTotal("fe", "fp", AccrualKindFee, time.Time{}, farFuture)
	if err != nil {
		t.Fatalf("AccruedTotal: %v", err)
	}
	if total != 3000 {
		t.Fatalf("total = %d, want 3000", total)
	}
	// Window filter: only d2's accrual falls in [d2, d3).
	total, err = l.AccruedTotal("fe", "fp", AccrualKindFee, d2, d3)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1000 {
		t.Fatalf("windowed total = %d, want 1000", total)
	}

	// Reverse d2's accrual: mirror legs, same amount, deterministic ID.
	origID := accrualEntryID(AccrualKindFee, "fe", "fp", d2)
	rev, dup, err := l.ReverseAccrual(origID, d3)
	if err != nil || dup {
		t.Fatalf("ReverseAccrual = dup=%v err=%v", dup, err)
	}
	if rev.ID != origID+"/reversal" {
		t.Fatalf("reversal ID = %q", rev.ID)
	}
	if rev.DebitAccount != "fp" || rev.CreditAccount != "fe" || rev.AmountCents != 1000 {
		t.Fatalf("reversal legs = %s/%s/%d", rev.DebitAccount, rev.CreditAccount, rev.AmountCents)
	}
	// The net total drops by the reversed day's accrual.
	total, err = l.AccruedTotal("fe", "fp", AccrualKindFee, time.Time{}, farFuture)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2000 {
		t.Fatalf("total after reversal = %d, want 2000", total)
	}
	// Reversing twice returns the existing reversal idempotently.
	rev2, dup, err := l.ReverseAccrual(origID, d3)
	if err != nil || !dup {
		t.Fatalf("double reversal = dup=%v err=%v, want dup=true", dup, err)
	}
	if rev2.ID != rev.ID {
		t.Fatalf("double reversal returned %q, want %q", rev2.ID, rev.ID)
	}
	// Reversing a non-accrual, or a reversal, fails.
	if _, _, err := l.ReverseAccrual("nope", d3); !errors.Is(err, ErrAccrualNotFound) {
		t.Fatalf("reversal of unknown = %v, want ErrAccrualNotFound", err)
	}
	if _, _, err := l.ReverseAccrual(rev.ID, d3); !errors.Is(err, ErrAccrualNotFound) {
		t.Fatalf("reversal of reversal = %v, want ErrAccrualNotFound", err)
	}
}

func TestAccrualCap(t *testing.T) {
	l := New()
	// Cap at 2500: two 1000-cent accruals pass, the third breaches.
	if _, _, err := l.AccrueFee("ce", "cp", 1000, accrualDay, 2500); err != nil {
		t.Fatalf("accrual 1: %v", err)
	}
	if _, _, err := l.AccrueFee("ce", "cp", 1000, accrualDay.Add(24*time.Hour), 2500); err != nil {
		t.Fatalf("accrual 2: %v", err)
	}
	if _, _, err := l.AccrueFee("ce", "cp", 1000, accrualDay.Add(48*time.Hour), 2500); !errors.Is(err, ErrAccrualCapExceeded) {
		t.Fatalf("accrual 3 = %v, want ErrAccrualCapExceeded", err)
	}
	// The breaching run booked nothing.
	if b := l.Balance("ce"); b != 2000 {
		t.Fatalf("expense balance after breach = %d, want 2000", b)
	}
	// Reversing the second day frees cap room: net is 1000, so a new
	// 1000-cent accrual fits again under the 2500 cap.
	if _, _, err := l.ReverseAccrual(accrualEntryID(AccrualKindFee, "ce", "cp", accrualDay.Add(24*time.Hour)), accrualDay.Add(72*time.Hour)); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if _, _, err := l.AccrueFee("ce", "cp", 1000, accrualDay.Add(96*time.Hour), 2500); err != nil {
		t.Fatalf("accrual after reversal: %v", err)
	}
}

func TestAccrualSkipsJournalRules(t *testing.T) {
	l := New()
	// A rule matching the accrual's legs: any ordinary Post of the same
	// shape would fire it.
	rule := JournalRule{
		ID:                "tax-withholding",
		Enabled:           true,
		MatchDebitAccount: "re",
		Legs: []RuleLeg{
			{DebitAccount: "re", CreditAccount: "tax-payable", FixedCents: 10, Label: "withholding"},
		},
	}
	if err := l.SetJournalRule(rule); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	before := l.RuleLegsCommitted()
	if _, _, err := l.AccrueFee("re", "rp", 1000, accrualDay, 0); err != nil {
		t.Fatalf("AccrueFee: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != before {
		t.Fatalf("rule legs committed = %d, want %d (accruals must not fire rules)", got, before)
	}
	for _, e := range l.Entries() {
		if strings.Contains(e.ID, "/rule/") {
			t.Fatalf("accrual produced derived rule leg %q", e.ID)
		}
	}
	// But a hand-posted entry with the same legs does fire the rule —
	// the skip is specific to accruals, not to the legs.
	if _, _, err := l.Post(JournalEntry{ID: "manual-1", DebitAccount: "re", CreditAccount: "rp", AmountCents: 1000}); err != nil {
		t.Fatalf("manual Post: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != before+1 {
		t.Fatalf("manual post fired %d rule legs, want 1", got-before)
	}
}

func TestAccrualChainAndVersion(t *testing.T) {
	l := New()
	ver0 := l.Version()
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, accrualDay, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AccrueFee("fe", "fp", 500, accrualDay, 0); err != nil {
		t.Fatal(err)
	}
	// Accruals are full journal rows: version bumps, chain covers them.
	if l.Version() != ver0+2 {
		t.Fatalf("version = %d, want %d", l.Version(), ver0+2)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after accruals: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation after accruals: %v", err)
	}
	report := l.Reconcile(time.Now())
	if !report.AccountingEquationOK {
		t.Fatalf("reconcile: %s", report.AccountingError)
	}
}

func TestAccrualChartInterplay(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("interest-expense", AccountTypeExpense); err != nil {
		t.Fatal(err)
	}
	if err := l.RegisterAccountType("interest-payable", AccountTypeLiability); err != nil {
		t.Fatal(err)
	}
	// Debit expense / credit liability is the normal direction for both
	// legs, so accruals clear the chart gate.
	if _, _, err := l.AccrueInterest("interest-expense", "interest-payable", 3650000, 10000, accrualDay, 0); err != nil {
		t.Fatalf("accrual under chart: %v", err)
	}
	// The reversal debits the liability — against the strict chart that
	// is a direction violation, documented in ReverseAccrual. Contra
	// accounts that book reversals stay unregistered.
	origID := accrualEntryID(AccrualKindInterest, "interest-expense", "interest-payable", accrualDay)
	if _, _, err := l.ReverseAccrual(origID, accrualDay.Add(time.Hour)); !errors.Is(err, ErrAccountDirectionMismatch) {
		t.Fatalf("reversal under strict chart = %v, want ErrAccountDirectionMismatch", err)
	}
}
