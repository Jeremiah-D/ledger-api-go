package ledger

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestPostStress exercises the ledger under heavy concurrent load: 100
// goroutines each posting 1000 unique entries (100k posts total), with
// balance and snapshot reads interleaved. Run with -race to prove the
// absence of data races.
//
// Every entry is unique (distinct ID and idempotency key) and books exactly
// 1 cent from equity to cash, so the final balances are deterministic: any
// lost or double-booked posting would show up as a wrong total.
func TestPostStress(t *testing.T) {
	l := New()
	const goroutines = 100
	const perGoroutine = 1000
	const total = goroutines * perGoroutine

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				e := JournalEntry{
					ID:             fmt.Sprintf("e-stress-g%03d-%04d", g, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    1,
					IdempotencyKey: fmt.Sprintf("key-stress-g%03d-%04d", g, i),
					CreatedAt:      time.Now(),
				}
				if _, dup, err := l.Post(e); err != nil {
					errs <- fmt.Errorf("goroutine %d post %d: %w", g, i, err)
					return
				} else if dup {
					errs <- fmt.Errorf("goroutine %d post %d: unexpected duplicate", g, i)
					return
				}
				// Interleave reads so the read path is also exercised
				// while writers hold the mutex.
				if i%100 == 0 {
					_ = l.Balance("cash")
					_ = l.Balance("equity")
				}
			}
			// One full snapshot per goroutine stresses Entries() under -race.
			if n := len(l.Entries()); n > total {
				errs <- fmt.Errorf("goroutine %d: entries %d exceeds total %d", g, n, total)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("stress Post error: %v", err)
	}

	// Deterministic totals: no posting may be lost or booked twice.
	if got := l.Balance("cash"); got != total {
		t.Errorf("Balance(cash) = %d, want %d", got, total)
	}
	if got := l.Balance("equity"); got != -total {
		t.Errorf("Balance(equity) = %d, want %d", got, -total)
	}

	entries := l.Entries()
	if len(entries) != total {
		t.Errorf("ledger recorded %d entries, want %d", len(entries), total)
	}

	// Conservation invariant: the sum of all account balances is zero.
	accounts := make(map[AccountID]bool, 2)
	for _, e := range entries {
		accounts[e.DebitAccount] = true
		accounts[e.CreditAccount] = true
	}
	var sum int64
	for a := range accounts {
		sum += l.Balance(a)
	}
	if sum != 0 {
		t.Errorf("sum of all balances = %d, want 0", sum)
	}

	// Spot-check: idempotency keys resolve to the right entries after the storm.
	for g := 0; g < goroutines; g += 10 {
		key := fmt.Sprintf("key-stress-g%03d-%04d", g, 0)
		wantID := fmt.Sprintf("e-stress-g%03d-%04d", g, 0)
		stored, ok := l.GetByIdempotencyKey(key)
		if !ok {
			t.Errorf("GetByIdempotencyKey(%q) not found", key)
			continue
		}
		if stored.ID != wantID {
			t.Errorf("GetByIdempotencyKey(%q).ID = %q, want %q", key, stored.ID, wantID)
		}
	}
}
