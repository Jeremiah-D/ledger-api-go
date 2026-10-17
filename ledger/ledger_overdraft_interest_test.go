package ledger

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// overdrawAccount posts a backdated entry that leaves account with a
// negative balance of negCents in currency.
func overdrawAccount(t *testing.T, l *Ledger, account string, negCents int64, currency string, at time.Time) {
	t.Helper()
	e := JournalEntry{
		ID:            "od-setup-" + account + "-" + currency + "-" + at.UTC().Format("20060102"),
		DebitAccount:  "funding",
		CreditAccount: AccountID(account),
		AmountCents:   negCents,
		Currency:      currency,
		CreatedAt:     at,
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("setup post: %v", err)
	}
}

func TestAccrueOverdraftInterestBasic(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "borrower", 1_000_000, "USD", at.Add(-24*time.Hour))

	got, dup, err := l.AccrueOverdraftInterest("borrower", "int-expense", "int-payable", "USD", 1000, false, at)
	if err != nil {
		t.Fatalf("AccrueOverdraftInterest: %v", err)
	}
	if dup {
		t.Fatal("first run must not be a duplicate")
	}
	// floor(1_000_000 * 1000 / 10000 / 365) = floor(100000/365) = 273.
	if want := int64(273); got.AmountCents != want {
		t.Fatalf("amount = %d, want %d", got.AmountCents, want)
	}
	if got.DebitAccount != "int-expense" || got.CreditAccount != "int-payable" {
		t.Fatalf("legs = %s/%s", got.DebitAccount, got.CreditAccount)
	}
	if got.Currency != "USD" {
		t.Fatalf("currency = %q", got.Currency)
	}
	if got.Memo != "accrual:overdraft-interest" {
		t.Fatalf("memo = %q", got.Memo)
	}
	if want := "accrual/overdraft-interest/borrower/USD/2026-10-10"; got.ID != want {
		t.Fatalf("id = %q, want %q", got.ID, want)
	}
	// The charged account is not a journal leg: its balance is untouched.
	if b := l.BalanceIn("borrower", "USD"); b != -1_000_000 {
		t.Fatalf("borrower balance = %d, want -1000000", b)
	}
	if b := l.BalanceIn("int-expense", "USD"); b != 273 {
		t.Fatalf("expense balance = %d, want 273", b)
	}
	if b := l.BalanceIn("int-payable", "USD"); b != -273 {
		t.Fatalf("payable balance = %d, want -273", b)
	}

	// Idempotency: re-running the same (account, currency, day) returns
	// the original entry.
	got2, dup, err := l.AccrueOverdraftInterest("borrower", "int-expense", "int-payable", "USD", 1000, false, at)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !dup || got2.ID != got.ID {
		t.Fatalf("replay must return the original entry, got dup=%v id=%q", dup, got2.ID)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestAccrueOverdraftInterestNotOverdrawn(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// Positive balance: debit the account.
	if _, _, err := l.Post(JournalEntry{ID: "pos1", DebitAccount: "saver", CreditAccount: "funding", AmountCents: 100, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AccrueOverdraftInterest("saver", "e", "p", "USD", 1000, false, at); err != ErrAccountNotOverdrawn {
		t.Fatalf("positive balance: err = %v, want ErrAccountNotOverdrawn", err)
	}
	// Zero balance (no activity at all) is also not overdrawn.
	if _, _, err := l.AccrueOverdraftInterest("ghost", "e", "p", "USD", 1000, false, at); err != ErrAccountNotOverdrawn {
		t.Fatalf("zero balance: err = %v, want ErrAccountNotOverdrawn", err)
	}
}

func TestAccrueOverdraftInterestProtectedAccountStillCharged(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "guarded", 1_000_000, "USD", at.Add(-24*time.Hour))
	l.EnableOverdraftProtection("guarded")

	// Overdraft protection guards the account's own postings; the
	// interest entry never touches it, so the charge still books.
	got, _, err := l.AccrueOverdraftInterest("guarded", "e", "p", "USD", 1000, false, at)
	if err != nil {
		t.Fatalf("protected account must still be charged: %v", err)
	}
	if got.AmountCents != 273 {
		t.Fatalf("amount = %d, want 273", got.AmountCents)
	}
	// And the protection still works for ordinary postings.
	if _, _, err := l.Post(JournalEntry{ID: "x1", DebitAccount: "funding", CreditAccount: "guarded", AmountCents: 1}); err != ErrAccountOverdraft {
		t.Fatalf("protection bypassed: err = %v", err)
	}
}

func TestAccrueOverdraftInterestCompound(t *testing.T) {
	day1 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	// Two ledgers: day 1 is charged on both, day 2 is plain on one and
	// compound on the other. (A single ledger cannot charge the same
	// (account, currency, day) twice — the idempotency key is the day.)
	setup := func() *Ledger {
		l := New()
		overdrawAccount(t, l, "borrower", 1_000_000, "USD", day1.Add(-24*time.Hour))
		if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, day1); err != nil {
			t.Fatal(err)
		}
		return l
	}
	// Non-compound day 2 charges the bare balance again: 273.
	plain, _, err := setup().AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, day2)
	if err != nil {
		t.Fatal(err)
	}
	if plain.AmountCents != 273 {
		t.Fatalf("plain day2 = %d, want 273", plain.AmountCents)
	}
	// Compound day 2 charges balance + day1's 273 of accrued interest:
	// floor(1_000_273 * 1000 / 10000 / 365) = 274.
	comp, _, err := setup().AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, true, day2)
	if err != nil {
		t.Fatal(err)
	}
	if comp.AmountCents != 274 {
		t.Fatalf("compound day2 = %d, want 274", comp.AmountCents)
	}
}

func TestAccrueOverdraftInterestReversalNetsOutstanding(t *testing.T) {
	l := New()
	day1 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "borrower", 1_000_000, "USD", day1.Add(-24*time.Hour))

	day1Entry, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, day1)
	if err != nil {
		t.Fatal(err)
	}
	// Reverse day 1 through the generic accrual reversal path.
	if _, _, err := l.ReverseAccrual(day1Entry.ID, day2); err != nil {
		t.Fatalf("ReverseAccrual: %v", err)
	}
	// Day 2 compound must see zero outstanding: same as plain.
	comp, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, true, day2)
	if err != nil {
		t.Fatal(err)
	}
	if comp.AmountCents != 273 {
		t.Fatalf("compound after reversal = %d, want 273", comp.AmountCents)
	}
}

func TestAccrueOverdraftInterestDust(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "tiny", 1, "USD", at.Add(-time.Hour))
	// floor(1 * 1 / 10000 / 365) = 0: a dust charge books nothing.
	if _, _, err := l.AccrueOverdraftInterest("tiny", "e", "p", "USD", 1, false, at); err != ErrNonPositiveAmount {
		t.Fatalf("dust: err = %v, want ErrNonPositiveAmount", err)
	}
}

func TestAccrueOverdraftInterestValidation(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "borrower", 1_000_000, "USD", at.Add(-time.Hour))

	for _, rate := range []int64{0, -5, 10001} {
		if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", rate, false, at); err != ErrInvalidAccrualRate {
			t.Fatalf("rate %d: err = %v, want ErrInvalidAccrualRate", rate, err)
		}
	}
	if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "US", 1000, false, at); err != ErrInvalidCurrency {
		t.Fatalf("bad currency: err = %v, want ErrInvalidCurrency", err)
	}
	if _, _, err := l.AccrueOverdraftInterest("", "e", "p", "USD", 1000, false, at); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty account: err = %v", err)
	}
	if _, _, err := l.AccrueOverdraftInterest("a/b", "e", "p", "USD", 1000, false, at); err != ErrAccrualAccountSeparator {
		t.Fatalf("slash account: err = %v, want ErrAccrualAccountSeparator", err)
	}
	if _, _, err := l.AccrueOverdraftInterest("borrower", "", "p", "USD", 1000, false, at); err == nil {
		t.Fatal("empty expense account must fail")
	}
	// Lowercase currency is rejected: the code must be 3 ASCII
	// uppercase letters (normalizeCurrency, see currency.go).
	if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "usd", 1000, false, at); err != ErrInvalidCurrency {
		t.Fatalf("lowercase currency: err = %v, want ErrInvalidCurrency", err)
	}
	// Empty currency means the default currency and shares the
	// idempotency key with its explicit form.
	if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "", 1000, false, at); err != nil {
		t.Fatal(err)
	}
	_, dup, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, at)
	if err != nil || !dup {
		t.Fatalf("default currency must share idempotency: dup=%v err=%v", dup, err)
	}
}

func TestAccrueOverdraftInterestCurrencyIsolation(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "borrower", 1_000_000, "USD", at.Add(-time.Hour))

	// No EUR overdraft: nothing to charge.
	if _, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "EUR", 1000, false, at); err != ErrAccountNotOverdrawn {
		t.Fatalf("EUR: err = %v, want ErrAccountNotOverdrawn", err)
	}
	got, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, at)
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != "USD" || !strings.Contains(got.ID, "/USD/") {
		t.Fatalf("entry currency/id mismatch: %+v", got)
	}
}

func TestAccrueOverdraftInterestConcurrent(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	overdrawAccount(t, l, "borrower", 1_000_000, "USD", base.Add(-48*time.Hour))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	// Eight distinct days race; each day books exactly once.
	for d := 0; d < 8; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			at := base.Add(time.Duration(d*24) * time.Hour)
			_, _, err := l.AccrueOverdraftInterest("borrower", "e", "p", "USD", 1000, false, at)
			errs <- err
		}(d)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent accrue: %v", err)
		}
	}
	total, err := l.overdraftInterestAccruedLocked("borrower", "USD", time.Time{}, base.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if total != 8*273 {
		t.Fatalf("total = %d, want %d", total, 8*273)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}
