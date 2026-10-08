package ledger

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// TestTrialBalanceTotals checks that every Post accumulates per-account
// debit/credit totals, that net balance always equals debits − credits,
// and that the accounting equation verifies after a mix of postings.
func TestTrialBalanceTotals(t *testing.T) {
	l := New()

	post := func(id, key, debit, credit string, cents int64) {
		t.Helper()
		e := validEntry()
		e.ID, e.IdempotencyKey = id, key
		e.DebitAccount, e.CreditAccount = AccountID(debit), AccountID(credit)
		e.AmountCents = cents
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
		}
	}

	post("e-tb-1", "key-tb-1", "cash", "equity", 1000)
	post("e-tb-2", "key-tb-2", "cash", "equity", 500)
	post("e-tb-3", "key-tb-3", "expenses", "cash", 300)

	cash := l.TrialBalance("cash")
	if cash.TotalDebits != 1500 || cash.TotalCredits != 300 || cash.NetBalance != 1200 {
		t.Fatalf("TrialBalance(cash) = %+v, want debits 1500 credits 300 net 1200", cash)
	}
	if cash.NetBalance != cash.TotalDebits-cash.TotalCredits {
		t.Fatalf("TrialBalance(cash) net %d != debits %d - credits %d",
			cash.NetBalance, cash.TotalDebits, cash.TotalCredits)
	}
	if cash.Version != 3 {
		t.Fatalf("TrialBalance(cash).Version = %d, want 3", cash.Version)
	}

	equity := l.TrialBalance("equity")
	if equity.TotalDebits != 0 || equity.TotalCredits != 1500 || equity.NetBalance != -1500 {
		t.Fatalf("TrialBalance(equity) = %+v, want debits 0 credits 1500 net -1500", equity)
	}

	expenses := l.TrialBalance("expenses")
	if expenses.TotalDebits != 300 || expenses.NetBalance != 300 {
		t.Fatalf("TrialBalance(expenses) = %+v, want debits 300 net 300", expenses)
	}

	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestTrialBalanceUnknownAccount checks that an account never touched
// reports zero totals at the current ledger version, mirroring Balance
// and Snapshot semantics for unknown accounts.
func TestTrialBalanceUnknownAccount(t *testing.T) {
	l := New()
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post = %v", err)
	}
	tb := l.TrialBalance("nobody")
	if tb.Account != "nobody" {
		t.Fatalf("TrialBalance(nobody).Account = %q, want nobody", tb.Account)
	}
	if tb.TotalDebits != 0 || tb.TotalCredits != 0 || tb.NetBalance != 0 {
		t.Fatalf("TrialBalance(nobody) = %+v, want zero totals", tb)
	}
	if tb.Version != 1 {
		t.Fatalf("TrialBalance(nobody).Version = %d, want 1", tb.Version)
	}
}

// TestTrialBalanceIdempotentReplayDoesNotDoubleCount checks that replaying
// an idempotency key books nothing again: totals, net, and version stay put.
func TestTrialBalanceIdempotentReplayDoesNotDoubleCount(t *testing.T) {
	l := New()
	if _, dup, err := l.Post(validEntry()); err != nil || dup {
		t.Fatalf("first Post = dup=%v err=%v", dup, err)
	}
	retry := validEntry()
	retry.ID = "e-tb-retry"
	if _, dup, err := l.Post(retry); err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want dup=true", dup, err)
	}
	tb := l.TrialBalance("cash")
	if tb.TotalDebits != 1000 || tb.TotalCredits != 0 || tb.NetBalance != 1000 || tb.Version != 1 {
		t.Fatalf("TrialBalance(cash) after replay = %+v, want debits 1000 net 1000 version 1", tb)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestTrialBalanceRejectedEntriesChangeNothing checks the atomic-commit
// half of Post: entries that fail double-entry validation (same account on
// both legs, zero amount, empty leg) record nothing — no totals, no
// version bump.
func TestTrialBalanceRejectedEntriesChangeNothing(t *testing.T) {
	l := New()
	sameAccount := validEntry()
	sameAccount.DebitAccount = sameAccount.CreditAccount
	zeroAmount := validEntry()
	zeroAmount.AmountCents = 0
	emptyLeg := validEntry()
	emptyLeg.DebitAccount = ""
	for i, e := range []JournalEntry{sameAccount, zeroAmount, emptyLeg} {
		e.ID = fmt.Sprintf("e-tb-bad-%d", i)
		if _, _, err := l.Post(e); err == nil {
			t.Fatalf("case %d: expected rejection", i)
		}
	}
	if tb := l.TrialBalance("cash"); !reflect.DeepEqual(tb, TrialBalance{Account: "cash", Currency: "USD"}) {
		t.Fatalf("TrialBalance(cash) after rejections = %+v, want zero value", tb)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestVerifyAccountingEquationEmptyLedger documents the base case: an
// untouched ledger trivially balances.
func TestVerifyAccountingEquationEmptyLedger(t *testing.T) {
	if err := New().VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation on empty ledger = %v", err)
	}
}

// TestTrialBalanceConcurrentPosts checks the accounting equation and the
// per-account totals under concurrent posting: 20 goroutines × 500 entries
// booking 1 cent from equity to cash, with trial-balance reads interleaved.
// Run with -race to prove the totals are updated atomically with the
// balances — any torn update would break the equation.
func TestTrialBalanceConcurrentPosts(t *testing.T) {
	l := New()
	const goroutines = 20
	const perGoroutine = 500
	const total = int64(goroutines * perGoroutine)

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				e := JournalEntry{
					ID:             fmt.Sprintf("e-tbc-g%02d-%04d", g, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    1,
					IdempotencyKey: fmt.Sprintf("key-tbc-g%02d-%04d", g, i),
				}
				if _, dup, err := l.Post(e); err != nil || dup {
					errs <- fmt.Errorf("goroutine %d post %d: dup=%v err=%v", g, i, dup, err)
					return
				}
				if i%100 == 0 {
					_ = l.TrialBalance("cash")
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	cash := l.TrialBalance("cash")
	if cash.TotalDebits != total || cash.TotalCredits != 0 || cash.NetBalance != total {
		t.Fatalf("TrialBalance(cash) = %+v, want debits %d net %d", cash, total, total)
	}
	equity := l.TrialBalance("equity")
	if equity.TotalDebits != 0 || equity.TotalCredits != total || equity.NetBalance != -total {
		t.Fatalf("TrialBalance(equity) = %+v, want credits %d net %d", equity, total, -total)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation after %d concurrent posts = %v", total, err)
	}
}
