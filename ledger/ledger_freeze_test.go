package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFreezeRejectsPostsOnEitherLeg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		froze AccountID
	}{
		{"frozen debit leg", "cash"},
		{"frozen credit leg", "equity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			l.Freeze(tc.froze)

			if !l.IsFrozen(tc.froze) {
				t.Fatalf("IsFrozen(%q) = false after Freeze", tc.froze)
			}

			_, _, err := l.Post(validEntry())
			if !errors.Is(err, ErrAccountFrozen) {
				t.Fatalf("Post through frozen account: got %v, want ErrAccountFrozen", err)
			}

			// The rejection booked nothing: balances untouched, no
			// journal row, version unbumped, chain unextended.
			if got := l.Balance("cash"); got != 0 {
				t.Errorf("Balance(cash) = %d, want 0 after frozen rejection", got)
			}
			if n := len(l.Entries()); n != 0 {
				t.Errorf("Entries() = %d rows, want 0 after frozen rejection", n)
			}
			if _, v := l.Snapshot("cash"); v != 0 {
				t.Errorf("version = %d, want 0 (rejected posts do not bump)", v)
			}
			if err := l.VerifyChain(); err != nil {
				t.Fatalf("VerifyChain on empty ledger: %v", err)
			}
		})
	}
}

func TestFreezeAndUnfreezeAreIdempotent(t *testing.T) {
	l := New()
	l.Freeze("cash")
	l.Freeze("cash") // second freeze changes nothing
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post after double freeze: got %v, want ErrAccountFrozen", err)
	}

	l.Unfreeze("cash")
	l.Unfreeze("cash") // unfreezing a non-frozen account is a no-op
	if l.IsFrozen("cash") {
		t.Fatalf("IsFrozen(cash) = true after Unfreeze")
	}
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post after Unfreeze: unexpected error %v", err)
	}
}

func TestFreezeDoesNotBumpVersionOrTouchReads(t *testing.T) {
	l := New()
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post: %v", err)
	}
	before := func() uint64 { _, v := l.Snapshot("cash"); return v }()
	l.Freeze("cash")
	l.Freeze("unseen-account") // freezing an unknown account is allowed
	l.Unfreeze("cash")
	l.Unfreeze("cash")
	if _, v := l.Snapshot("cash"); v != before {
		t.Errorf("version moved from %d to %d across freeze/unfreeze", before, v)
	}

	// Reads keep working on a frozen account.
	l.Freeze("cash")
	if got := l.Balance("cash"); got != 1000 {
		t.Errorf("Balance(cash) on frozen account = %d, want 1000", got)
	}
	if bal, _ := l.Snapshot("cash"); bal != 1000 {
		t.Errorf("Snapshot(cash) on frozen account = %d, want 1000", bal)
	}
	tb := l.TrialBalance("cash")
	if tb.NetBalance != 1000 || !tb.Frozen {
		t.Errorf("TrialBalance(cash) = %+v, want net 1000 and frozen=true", tb)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation on frozen account: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain on frozen account: %v", err)
	}
	report := l.Reconcile(time.Now())
	if !report.AccountingEquationOK {
		t.Errorf("Reconcile on frozen account: equation not OK: %s", report.AccountingError)
	}
	found := false
	for _, a := range report.FrozenAccounts {
		if a == "cash" {
			found = true
		}
	}
	if !found {
		t.Errorf("Reconcile report FrozenAccounts = %v, want it to contain cash", report.FrozenAccounts)
	}
}

func TestIdempotencyReplaySurvivesFreeze(t *testing.T) {
	// A key posted before the freeze replays successfully after the
	// freeze: the replay books nothing new, so the risk stop does not
	// break the idempotency contract for callers that retry.
	l := New()
	first, dup, err := l.Post(validEntry())
	if err != nil || dup {
		t.Fatalf("first Post: err=%v dup=%v", err, dup)
	}
	l.Freeze("cash")

	replayed, dup, err := l.Post(validEntry())
	if err != nil {
		t.Fatalf("replay after freeze: unexpected error %v", err)
	}
	if !dup {
		t.Fatalf("replay after freeze: dup = false, want true")
	}
	if replayed != first {
		t.Fatalf("replay after freeze returned modified entry")
	}
	// Still exactly one booking.
	if _, v := l.Snapshot("cash"); v != 1 {
		t.Errorf("version = %d, want 1 after replay", v)
	}
}

func TestFrozenRejectionBeatsFreshIdempotencyKey(t *testing.T) {
	// A brand-new key through a frozen account is rejected: the key must
	// not be recorded, so a later post after unfreeze is not mistaken
	// for a duplicate.
	l := New()
	l.Freeze("cash")
	e := validEntry()
	if _, _, err := l.Post(e); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post: got %v, want ErrAccountFrozen", err)
	}
	if _, ok := l.GetByIdempotencyKey(e.IdempotencyKey); ok {
		t.Fatalf("rejected post recorded its idempotency key")
	}
	l.Unfreeze("cash")
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post after unfreeze: err=%v dup=%v, want clean booking", err, dup)
	}
}

func TestFreezeUnderConcurrency(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := AccountID("acct")
			l.Freeze(a)
			l.IsFrozen(a)
			e := validEntry()
			e.ID = "e-conc"
			e.DebitAccount = a
			e.CreditAccount = "counter"
			_, _, _ = l.Post(e) // may be rejected; must not race
			l.Unfreeze(a)
			l.Balance(a)
			_, _ = l.Snapshot(a)
			l.TrialBalance(a)
			_ = l.Reconcile(time.Now())
		}(i)
	}
	wg.Wait()
}
