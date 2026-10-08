package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func validEntry() JournalEntry {
	return JournalEntry{
		ID:             "e-1",
		DebitAccount:   "cash",
		CreditAccount:  "equity",
		AmountCents:    1000,
		IdempotencyKey: "key-1",
		CreatedAt:      time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
	}
}

func TestPostValidEntryUpdatesBalances(t *testing.T) {
	l := New()
	e := validEntry()

	posted, dup, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post returned unexpected error: %v", err)
	}
	if dup {
		t.Fatalf("Post reported duplicate for a first-time entry")
	}
	if posted != e {
		// Post normalizes an empty currency to the default before
		// committing, so the returned entry is the canonical journaled
		// form, not a byte copy of the input.
		want := e
		want.Currency = DefaultCurrency
		if posted != want {
			t.Fatalf("Post returned modified entry: got %+v, want %+v", posted, want)
		}
	}
	if got := l.Balance("cash"); got != 1000 {
		t.Errorf("Balance(cash) = %d, want 1000", got)
	}
	if got := l.Balance("equity"); got != -1000 {
		t.Errorf("Balance(equity) = %d, want -1000", got)
	}
	if got := l.Balance("unknown"); got != 0 {
		t.Errorf("Balance(unknown) = %d, want 0", got)
	}
}

func TestPostRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*JournalEntry)
		want   error
	}{
		{"empty ID", func(e *JournalEntry) { e.ID = "" }, ErrEmptyID},
		{"empty debit account", func(e *JournalEntry) { e.DebitAccount = "" }, ErrEmptyDebitAccount},
		{"empty credit account", func(e *JournalEntry) { e.CreditAccount = "" }, ErrEmptyCreditAccount},
		{"same debit and credit", func(e *JournalEntry) { e.CreditAccount = e.DebitAccount }, ErrSameAccount},
		{"zero amount", func(e *JournalEntry) { e.AmountCents = 0 }, ErrNonPositiveAmount},
		{"negative amount", func(e *JournalEntry) { e.AmountCents = -50 }, ErrNonPositiveAmount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			e := validEntry()
			tc.mutate(&e)

			if _, _, err := l.Post(e); !errors.Is(err, tc.want) {
				t.Fatalf("Post error = %v, want %v", err, tc.want)
			}
			// A rejected entry must leave all balances untouched.
			for _, acct := range []AccountID{"cash", "equity", e.DebitAccount, e.CreditAccount} {
				if got := l.Balance(acct); got != 0 {
					t.Errorf("Balance(%q) = %d after rejected post, want 0", acct, got)
				}
			}
			if n := len(l.Entries()); n != 0 {
				t.Errorf("ledger recorded %d entries after rejected post, want 0", n)
			}
		})
	}
}

func TestPostIdempotency(t *testing.T) {
	l := New()
	e := validEntry()

	first, dup, err := l.Post(e)
	if err != nil {
		t.Fatalf("first Post error: %v", err)
	}
	if dup {
		t.Fatalf("first Post reported duplicate")
	}

	// Same idempotency key, different entry ID: must return the original.
	retry := e
	retry.ID = "e-1-retry"
	second, dup, err := l.Post(retry)
	if err != nil {
		t.Fatalf("retry Post error: %v", err)
	}
	if !dup {
		t.Fatalf("retry Post did not report duplicate")
	}
	if second != first {
		t.Fatalf("retry Post returned %+v, want original %+v", second, first)
	}

	// Balances must reflect exactly one booking.
	if got := l.Balance("cash"); got != 1000 {
		t.Errorf("Balance(cash) = %d after duplicate post, want 1000", got)
	}
	if got := l.Balance("equity"); got != -1000 {
		t.Errorf("Balance(equity) = %d after duplicate post, want -1000", got)
	}
	if n := len(l.Entries()); n != 1 {
		t.Errorf("ledger recorded %d entries after duplicate post, want 1", n)
	}

	// GetByIdempotencyKey must resolve to the original entry.
	stored, ok := l.GetByIdempotencyKey("key-1")
	if !ok {
		t.Fatalf("GetByIdempotencyKey did not find posted key")
	}
	if stored != first {
		t.Fatalf("GetByIdempotencyKey returned %+v, want %+v", stored, first)
	}
	if _, ok := l.GetByIdempotencyKey("never-posted"); ok {
		t.Fatalf("GetByIdempotencyKey returned an entry for an unknown key")
	}
}

func TestPostIdempotencyDistinctKeys(t *testing.T) {
	l := New()
	for i, key := range []string{"k-a", "k-b", "k-c"} {
		e := validEntry()
		e.ID = "e-distinct-" + string(rune('a'+i))
		e.IdempotencyKey = key
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%q) = dup=%v err=%v", key, dup, err)
		}
	}
	if got := l.Balance("cash"); got != 3000 {
		t.Errorf("Balance(cash) = %d, want 3000", got)
	}
}

func TestPostConcurrent(t *testing.T) {
	l := New()
	const n = 50

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := JournalEntry{
				ID:             "e-conc",
				DebitAccount:   "cash",
				CreditAccount:  "equity",
				AmountCents:    1,
				IdempotencyKey: "key-conc",
				CreatedAt:      time.Now(),
			}
			// Same ID, unique idempotency key per goroutine.
			e.ID = "e-conc-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			e.IdempotencyKey = "key-conc-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			if _, _, err := l.Post(e); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Post error: %v", err)
	}

	if got := l.Balance("cash"); got != n {
		t.Errorf("Balance(cash) = %d after %d concurrent posts, want %d", got, n, n)
	}
	if got := l.Balance("equity"); got != -n {
		t.Errorf("Balance(equity) = %d after %d concurrent posts, want %d", got, n, -n)
	}
	if entries := l.Entries(); len(entries) != n {
		t.Errorf("ledger recorded %d entries, want %d", len(entries), n)
	}
}

func TestPostConcurrentSameIdempotencyKey(t *testing.T) {
	// All goroutines race with the same idempotency key: exactly one must win
	// and book, the rest must get the original back as duplicates.
	l := New()
	const n = 50

	var wg sync.WaitGroup
	type result struct {
		dup bool
		err error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := validEntry()
			e.ID = "e-race-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			_, dup, err := l.Post(e)
			results <- result{dup: dup, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	winners, dups := 0, 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent Post error: %v", r.err)
		}
		if r.dup {
			dups++
		} else {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if dups != n-1 {
		t.Fatalf("duplicates = %d, want %d", dups, n-1)
	}
	if got := l.Balance("cash"); got != 1000 {
		t.Errorf("Balance(cash) = %d after same-key race, want 1000 (booked once)", got)
	}
}
