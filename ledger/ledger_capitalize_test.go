package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var capitalizeDay = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestCapitalizeInterestHappyPath(t *testing.T) {
	l := New()
	// 3,650,000 cents at 10000 bps (100%/yr): 10,000/day.
	for i := 0; i < 3; i++ {
		day := capitalizeDay.Add(time.Duration(i) * 24 * time.Hour)
		if _, _, err := l.AccrueInterest("interest-expense", "interest-payable", 3650000, 10000, day, 0); err != nil {
			t.Fatalf("AccrueInterest day %d: %v", i, err)
		}
	}
	total, err := l.AccruedTotal("interest-expense", "interest-payable", AccrualKindInterest, time.Time{}, capitalizeDay.Add(3*24*time.Hour))
	if err != nil || total != 30000 {
		t.Fatalf("AccruedTotal = %d, %v; want 30000", total, err)
	}
	got, dup, err := l.CapitalizeInterest("interest-expense", "interest-payable", "loan-principal", capitalizeDay.Add(3*24*time.Hour))
	if err != nil || dup {
		t.Fatalf("CapitalizeInterest = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 30000 {
		t.Fatalf("capitalized = %d, want 30000", got.AmountCents)
	}
	// Legs: debit the payable, credit the principal.
	if got.DebitAccount != "interest-payable" || got.CreditAccount != "loan-principal" {
		t.Fatalf("legs = %s/%s, want interest-payable/loan-principal", got.DebitAccount, got.CreditAccount)
	}
	if got.Memo != "accrual-capitalization:interest" {
		t.Fatalf("memo = %q, want accrual-capitalization:interest", got.Memo)
	}
	if !strings.HasPrefix(got.ID, "accrual-capitalization/interest/interest-expense/interest-payable/loan-principal/") {
		t.Fatalf("ID = %q, want deterministic capitalization ID", got.ID)
	}
	// Balances: the payable is relieved, the principal grows.
	if b := l.Balance("interest-payable"); b != 0 {
		t.Fatalf("payable balance = %d, want 0", b)
	}
	if b := l.Balance("loan-principal"); b != -30000 {
		t.Fatalf("principal balance = %d, want -30000", b)
	}
	// After capitalization the outstanding total is zero.
	after, err := l.AccruedTotal("interest-expense", "interest-payable", AccrualKindInterest, time.Time{}, capitalizeDay.Add(4*24*time.Hour))
	if err != nil || after != 0 {
		t.Fatalf("AccruedTotal after capitalization = %d, %v; want 0", after, err)
	}
}

func TestCapitalizeInterestIdempotentPerDay(t *testing.T) {
	l := New()
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, capitalizeDay, 0); err != nil {
		t.Fatal(err)
	}
	// asof is after the accrual instant but on the same calendar day:
	// the window is [since, until), so the accrual is included.
	asof := capitalizeDay.Add(time.Hour)
	first, dup, err := l.CapitalizeInterest("ie", "ip", "principal", asof)
	if err != nil || dup {
		t.Fatalf("first = dup=%v err=%v", dup, err)
	}
	ver := l.Version()
	second, dup, err := l.CapitalizeInterest("ie", "ip", "principal", asof)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %q, want original %q", second.ID, first.ID)
	}
	if l.Version() != ver {
		t.Fatalf("replay bumped version %d -> %d", ver, l.Version())
	}
	// A new day prices the newly accrued interest only.
	nextAccrual := capitalizeDay.Add(24 * time.Hour)
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, nextAccrual, 0); err != nil {
		t.Fatal(err)
	}
	third, dup, err := l.CapitalizeInterest("ie", "ip", "principal", nextAccrual.Add(time.Hour))
	if err != nil || dup {
		t.Fatalf("next day = dup=%v err=%v", dup, err)
	}
	if third.AmountCents != 10000 {
		t.Fatalf("next-day capitalization = %d, want 10000 (only the new day's accrual)", third.AmountCents)
	}
}

func TestCapitalizeInterestNothingToCapitalize(t *testing.T) {
	l := New()
	_, _, err := l.CapitalizeInterest("ie", "ip", "principal", capitalizeDay)
	if !errors.Is(err, ErrNothingToCapitalize) {
		t.Fatalf("err = %v, want ErrNothingToCapitalize", err)
	}
	if l.Version() != 0 {
		t.Fatalf("version = %d, want 0 (nothing booked)", l.Version())
	}
}

func TestCapitalizeInterestNetOfReversal(t *testing.T) {
	l := New()
	for i := 0; i < 2; i++ {
		day := capitalizeDay.Add(time.Duration(i) * 24 * time.Hour)
		if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, day, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Reverse the first day's accrual: only the second day stays
	// outstanding.
	if _, _, err := l.ReverseAccrual("accrual/interest/ie/ip/"+capitalizeDay.UTC().Format("2006-01-02"), capitalizeDay); err != nil {
		t.Fatalf("ReverseAccrual: %v", err)
	}
	got, _, err := l.CapitalizeInterest("ie", "ip", "principal", capitalizeDay.Add(2*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("capitalized = %d, want 10000 (net of the reversal)", got.AmountCents)
	}
}

func TestCapitalizeInterestValidation(t *testing.T) {
	l := New()
	if _, _, err := l.CapitalizeInterest("", "ip", "principal", capitalizeDay); err == nil {
		t.Fatal("empty expense accepted")
	}
	if _, _, err := l.CapitalizeInterest("ie", "ip", "ie", capitalizeDay); !errors.Is(err, ErrInvalidCapitalizationAccount) {
		t.Fatalf("principal == expense: err = %v, want ErrInvalidCapitalizationAccount", err)
	}
	if _, _, err := l.CapitalizeInterest("ie", "ip", "ip", capitalizeDay); !errors.Is(err, ErrInvalidCapitalizationAccount) {
		t.Fatalf("principal == payable: err = %v, want ErrInvalidCapitalizationAccount", err)
	}
	if _, _, err := l.CapitalizeInterest("ie", "ip", "prin/cipal", capitalizeDay); !errors.Is(err, ErrInvalidCapitalizationAccount) {
		t.Fatalf("slash in principal: err = %v, want ErrInvalidCapitalizationAccount", err)
	}
	if _, _, err := l.CapitalizeInterest("ie", "ip", "", capitalizeDay); !errors.Is(err, ErrInvalidCapitalizationAccount) {
		t.Fatalf("empty principal: err = %v, want ErrInvalidCapitalizationAccount", err)
	}
}

func TestCapitalizeInterestReversalRestores(t *testing.T) {
	l := New()
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, capitalizeDay, 0); err != nil {
		t.Fatal(err)
	}
	cap, _, err := l.CapitalizeInterest("ie", "ip", "principal", capitalizeDay.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Reversing the capitalization restores the payable and the
	// outstanding total.
	if _, _, err := l.ReverseAccrual(cap.ID, capitalizeDay.Add(2*time.Hour)); err != nil {
		t.Fatalf("ReverseAccrual(capitalization): %v", err)
	}
	total, err := l.AccruedTotal("ie", "ip", AccrualKindInterest, time.Time{}, capitalizeDay.Add(24*time.Hour))
	if err != nil || total != 10000 {
		t.Fatalf("AccruedTotal after reversal = %d, %v; want 10000", total, err)
	}
	if b := l.Balance("ip"); b != -10000 {
		t.Fatalf("payable balance = %d, want -10000", b)
	}
}

func TestCapitalizeInterestEntersHashChain(t *testing.T) {
	l := New()
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, capitalizeDay, 0); err != nil {
		t.Fatal(err)
	}
	headBefore, _ := l.ChainHead()
	if _, _, err := l.CapitalizeInterest("ie", "ip", "principal", capitalizeDay.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	headAfter, links := l.ChainHead()
	if headAfter == headBefore {
		t.Fatal("chain head did not move after capitalization")
	}
	if links == 0 {
		t.Fatal("no chain links")
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestParseCapitalizationID(t *testing.T) {
	id := capitalizationEntryID("e", "p", "pr", capitalizeDay)
	exp, pay, pr, ok := parseCapitalizationID(id)
	if !ok || exp != "e" || pay != "p" || pr != "pr" {
		t.Fatalf("parse(%q) = %q,%q,%q,%v", id, exp, pay, pr, ok)
	}
	// Reversal rows are never mistaken for capitalizations.
	if _, _, _, ok := parseCapitalizationID(id + "/reversal"); ok {
		t.Fatalf("parse accepted a reversal row %q", id+"/reversal")
	}
	// Ordinary accrual IDs are not capitalizations.
	if _, _, _, ok := parseCapitalizationID("accrual/interest/e/p/2026-11-02"); ok {
		t.Fatal("parse accepted an ordinary accrual ID")
	}
	if _, _, _, ok := parseCapitalizationID("accrual-capitalization/interest/e/p"); ok {
		t.Fatal("parse accepted a truncated ID")
	}
}

func TestCapitalizeInterestZeroAsof(t *testing.T) {
	l := New()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if _, _, err := l.AccrueInterest("ie", "ip", 3650000, 10000, day, 0); err != nil {
		t.Fatal(err)
	}
	got, _, err := l.CapitalizeInterest("ie", "ip", "principal", time.Time{})
	if err != nil {
		t.Fatalf("zero asof: %v", err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("capitalized = %d, want 10000", got.AmountCents)
	}
}
