package ledger

import (
	"fmt"
	"sync"
	"testing"
)

func postTimeTravelEntry(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, currency string) {
	t.Helper()
	_, _, err := l.Post(JournalEntry{
		ID:           id,
		DebitAccount: debit,
		CreditAccount: credit,
		AmountCents:  cents,
		Currency:     currency,
	})
	if err != nil {
		t.Fatalf("Post(%s) failed: %v", id, err)
	}
}

func TestBalanceAtReproducesHistory(t *testing.T) {
	l := New()
	postTimeTravelEntry(t, l, "e1", "alice", "bob", 1000, "USD") // v1: alice +1000, bob -1000
	postTimeTravelEntry(t, l, "e2", "alice", "bob", 500, "USD")   // v2: alice +1500, bob -1500
	postTimeTravelEntry(t, l, "e3", "bob", "alice", 200, "USD")   // v3: alice +1300, bob -1300

	cases := []struct {
		account  AccountID
		version  uint64
		expected int64
	}{
		{"alice", 0, 0},      // genesis
		{"alice", 1, 1000},   // after e1
		{"alice", 2, 1500},   // after e2
		{"alice", 3, 1300},   // after e3
		{"bob", 1, -1000},    // credit leg goes negative
		{"bob", 2, -1500},
		{"bob", 3, -1300},
		{"carol", 3, 0},      // unknown account: zero at every version
		{"carol", 0, 0},
	}
	for _, c := range cases {
		got, err := l.BalanceAt(c.account, "USD", c.version)
		if err != nil {
			t.Fatalf("BalanceAt(%q, USD, %d) error: %v", c.account, c.version, err)
		}
		if got != c.expected {
			t.Errorf("BalanceAt(%q, USD, %d) = %d, want %d", c.account, c.version, got, c.expected)
		}
	}
}

func TestBalanceAtMatchesSnapshotVersion(t *testing.T) {
	// The version Snapshot reports fed back into BalanceAt must reproduce
	// the balance Snapshot reported: the round-trip is the whole point.
	l := New()
	postTimeTravelEntry(t, l, "e1", "alice", "bob", 700, "USD")
	snapBalance, snapVersion := l.Snapshot("alice")
	at, err := l.BalanceAt("alice", "USD", snapVersion)
	if err != nil {
		t.Fatalf("BalanceAt at snapshot version %d: %v", snapVersion, err)
	}
	if at != snapBalance {
		t.Errorf("BalanceAt(alice, USD, %d) = %d, snapshot reported %d", snapVersion, at, snapBalance)
	}
}

func TestBalanceAtCurrentVersionEqualsBalance(t *testing.T) {
	l := New()
	postTimeTravelEntry(t, l, "e1", "alice", "bob", 1000, "USD")
	postTimeTravelEntry(t, l, "e2", "alice", "carol", 250, "EUR")
	_, version := l.Snapshot("alice")
	for _, cur := range []string{"USD", "EUR"} {
		at, err := l.BalanceAt("alice", cur, version)
		if err != nil {
			t.Fatalf("BalanceAt(alice, %s, %d): %v", cur, version, err)
		}
		if at != l.BalanceIn("alice", cur) {
			t.Errorf("BalanceAt(alice, %s, current) = %d, BalanceIn = %d", cur, at, l.BalanceIn("alice", cur))
		}
	}
}

func TestBalanceAtCurrencyIsolation(t *testing.T) {
	l := New()
	postTimeTravelEntry(t, l, "e1", "alice", "bob", 1000, "USD")
	postTimeTravelEntry(t, l, "e2", "alice", "bob", 400, "EUR")

	usd, err := l.BalanceAt("alice", "USD", 2)
	if err != nil || usd != 1000 {
		t.Errorf("BalanceAt(alice, USD, 2) = %d, %v; want 1000", usd, err)
	}
	eur, err := l.BalanceAt("alice", "EUR", 2)
	if err != nil || eur != 400 {
		t.Errorf("BalanceAt(alice, EUR, 2) = %d, %v; want 400", eur, err)
	}
	// Empty currency means the default currency.
	def, err := l.BalanceAt("alice", "", 2)
	if err != nil || def != 1000 {
		t.Errorf("BalanceAt(alice, \"\", 2) = %d, %v; want 1000 (default USD)", def, err)
	}
}

func TestBalanceAtFutureVersionRejected(t *testing.T) {
	l := New()
	postTimeTravelEntry(t, l, "e1", "alice", "bob", 1000, "USD")
	if _, err := l.BalanceAt("alice", "USD", 2); err != ErrVersionInFuture {
		t.Errorf("BalanceAt future version: got %v, want ErrVersionInFuture", err)
	}
	// An empty ledger rejects even version 1.
	empty := New()
	if _, err := empty.BalanceAt("alice", "USD", 1); err != ErrVersionInFuture {
		t.Errorf("BalanceAt on empty ledger: got %v, want ErrVersionInFuture", err)
	}
	if bal, err := empty.BalanceAt("alice", "USD", 0); err != nil || bal != 0 {
		t.Errorf("BalanceAt genesis on empty ledger = %d, %v; want 0, nil", bal, err)
	}
}

func TestBalanceAtConcurrentReads(t *testing.T) {
	// Readers hold one RLock for the whole prefix scan; concurrent Posts
	// must neither race nor corrupt the historical view.
	l := New()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _, err := l.Post(JournalEntry{
					ID:            fmt.Sprintf("w%d-e%d", w, i),
					DebitAccount:  "alice",
					CreditAccount: "bob",
					AmountCents:   10,
					Currency:      "USD",
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d post %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, version := l.Snapshot("alice")
				if _, err := l.BalanceAt("alice", "USD", version); err != nil {
					errs <- fmt.Errorf("concurrent BalanceAt: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after concurrent load: %v", err)
	}
}
