package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// In this ledger the debit account's balance increases and the credit
// account's balance decreases, so the account that can be overdrawn is the
// credit (payer) leg: "cash" (debit) is never the overdraft risk in these
// tests, "equity" (credit) is.

func TestOverdraftProtectionDisabledByDefault(t *testing.T) {
	l := New()
	// An unprotected credit account may go as negative as it likes:
	// the ledger keeps its classic bookkeeping semantics.
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post to unprotected credit account: %v", err)
	}
	if got := l.Balance("equity"); got != -1000 {
		t.Fatalf("Balance(equity) = %d, want -1000", got)
	}
}

func TestOverdraftProtectionRejectsNegativeResult(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("equity")
	if !l.OverdraftProtected("equity") {
		t.Fatal("OverdraftProtected(equity) = false after EnableOverdraftProtection")
	}

	// equity starts at 0: any positive amount would overdraw it.
	_, _, err := l.Post(validEntry())
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post overdrawing protected account: got %v, want ErrAccountOverdraft", err)
	}

	// The rejection booked nothing: balances untouched, no journal row,
	// version unbumped, chain unextended.
	if got := l.Balance("equity"); got != 0 {
		t.Errorf("Balance(equity) = %d, want 0 after overdraft rejection", got)
	}
	if n := len(l.Entries()); n != 0 {
		t.Errorf("Entries() = %d rows, want 0 after overdraft rejection", n)
	}
	if _, v := l.Snapshot("cash"); v != 0 {
		t.Errorf("version = %d, want 0 (rejected posts do not bump)", v)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on empty ledger: %v", err)
	}
}

func TestOverdraftProtectionAllowsExactBalance(t *testing.T) {
	l := New()
	// Fund equity first: debit equity / credit funding moves 1000 into
	// equity's balance.
	if _, _, err := l.Post(JournalEntry{
		ID: "e-0", DebitAccount: "equity", CreditAccount: "funding",
		AmountCents: 1000, CreatedAt: validEntry().CreatedAt,
	}); err != nil {
		t.Fatalf("funding post: %v", err)
	}
	l.EnableOverdraftProtection("equity")

	// Draining the account to exactly zero is not an overdraft.
	e := validEntry()
	e.ID = "e-2"
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post draining protected account to zero: %v", err)
	}
	if got := l.Balance("equity"); got != 0 {
		t.Fatalf("Balance(equity) = %d, want 0", got)
	}

	// One cent more would go negative: rejected.
	e = validEntry()
	e.ID = "e-3"
	e.IdempotencyKey = "key-3" // a fresh key: same key would replay, not overdraw
	e.AmountCents = 1
	if _, _, err := l.Post(e); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post 1 cent past zero: got %v, want ErrAccountOverdraft", err)
	}
}

func TestOverdraftProtectionOnlyWatchesCreditLeg(t *testing.T) {
	l := New()
	// Protect the debit account: the debit leg's balance only ever grows
	// here, so posting must still succeed even though "cash" is protected.
	l.EnableOverdraftProtection("cash")
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post with protected debit leg: %v", err)
	}
}

func TestOverdraftProtectionReplayBeforeProtection(t *testing.T) {
	l := New()
	// Post with the key before protection exists, then protect the
	// account: the replay must still return the original entry, because
	// a replay books nothing new.
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("first post: %v", err)
	}
	l.EnableOverdraftProtection("equity")

	posted, dup, err := l.Post(validEntry())
	if err != nil {
		t.Fatalf("replay after enabling protection: %v", err)
	}
	if !dup || posted.ID != "e-1" {
		t.Fatalf("replay = (%q, dup=%v), want (\"e-1\", true)", posted.ID, dup)
	}
}

func TestFrozenBeatsOverdraft(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("equity")
	l.Freeze("equity")
	// Frozen 403 outranks overdraft 422: the check order in Post is
	// frozen first.
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post through frozen+protected account: got %v, want ErrAccountFrozen", err)
	}
}

func TestOverdraftProtectionEnableDisableIdempotent(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("equity")
	l.EnableOverdraftProtection("equity")
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post after double enable: got %v, want ErrAccountOverdraft", err)
	}
	l.DisableOverdraftProtection("equity")
	l.DisableOverdraftProtection("equity")
	if l.OverdraftProtected("equity") {
		t.Fatal("OverdraftProtected(equity) = true after disable")
	}
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post after disable: %v", err)
	}
}

func TestWithOverdraftProtectionOption(t *testing.T) {
	l := New(WithOverdraftProtection("equity", "cash"))
	for _, a := range []AccountID{"equity", "cash"} {
		if !l.OverdraftProtected(a) {
			t.Errorf("OverdraftProtected(%q) = false, want true", a)
		}
	}
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post: got %v, want ErrAccountOverdraft", err)
	}
}

func TestReconcileReportsProtectedAccounts(t *testing.T) {
	l := New(WithOverdraftProtection("zeta", "alpha"))
	report := l.Reconcile(validEntry().CreatedAt)
	if len(report.OverdraftProtectedAccounts) != 2 ||
		report.OverdraftProtectedAccounts[0] != "alpha" ||
		report.OverdraftProtectedAccounts[1] != "zeta" {
		t.Fatalf("OverdraftProtectedAccounts = %v, want sorted [alpha zeta]",
			report.OverdraftProtectedAccounts)
	}
	// Trial balances carry the per-account flag too.
	for _, tb := range report.TrialBalances {
		if tb.OverdraftProtected != (tb.Account == "alpha" || tb.Account == "zeta") {
			t.Errorf("TrialBalance(%q).OverdraftProtected = %v", tb.Account, tb.OverdraftProtected)
		}
	}
	if got := l.TrialBalance("alpha").OverdraftProtected; !got {
		t.Error("TrialBalance(alpha).OverdraftProtected = false, want true")
	}
}

func TestOverdraftProtectionConcurrent(t *testing.T) {
	l := New()
	// Fund equity with 100_000 cents, then hammer it with 100 goroutines
	// each posting 1000 cents: exactly 100 posts fit, the rest overdraw.
	if _, _, err := l.Post(JournalEntry{
		ID: "fund", DebitAccount: "equity", CreditAccount: "funding",
		AmountCents: 100_000, CreatedAt: validEntry().CreatedAt,
	}); err != nil {
		t.Fatalf("funding post: %v", err)
	}
	l.EnableOverdraftProtection("equity")

	var wg sync.WaitGroup
	var mu sync.Mutex
	success, rejected := 0, 0
	for g := 0; g < 110; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			e := JournalEntry{
				ID:            fmt.Sprintf("c-%d", g),
				DebitAccount:  "cash",
				CreditAccount: "equity",
				AmountCents:   1000,
				CreatedAt:     validEntry().CreatedAt,
			}
			_, _, err := l.Post(e)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrAccountOverdraft) {
				rejected++
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			} else {
				success++
			}
		}(g)
	}
	wg.Wait()
	if success != 100 {
		t.Errorf("successful posts = %d, want 100 (100_000 / 1000)", success)
	}
	if rejected != 10 {
		t.Errorf("rejected posts = %d, want 10 (the oversubscribed ones)", rejected)
	}
	if got := l.Balance("equity"); got != 0 {
		t.Errorf("Balance(equity) = %d, want exactly 0 (never negative)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken: %v", err)
	}
}

func TestOverdraftHugeAmountCannotOverflow(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("equity")
	// balance - amount would overflow int64 if computed naively;
	// the check must use a comparison, not a subtraction.
	e := validEntry()
	e.ID = "e-huge"
	e.AmountCents = 1<<63 - 1
	if _, _, err := l.Post(e); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post of max-int64 amount: got %v, want ErrAccountOverdraft", err)
	}
}
