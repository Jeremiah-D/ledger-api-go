package ledger

import (
	"testing"
	"time"
)

// postKeyed posts an entry with an explicit CreatedAt and idempotency key.
func postKeyed(t *testing.T, l *Ledger, id, key string, at time.Time) {
	t.Helper()
	e := JournalEntry{
		ID:             id,
		DebitAccount:   "cash",
		CreditAccount:  "equity",
		AmountCents:    100,
		IdempotencyKey: key,
		CreatedAt:      at,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post(%s) = dup=%v err=%v", id, dup, err)
	}
}

func TestExpireIdempotencyKeysRemovesOnlyStaleKeys(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()

	postKeyed(t, l, "old-1", "key-old-1", now.Add(-2*time.Hour))
	postKeyed(t, l, "old-2", "key-old-2", now.Add(-90*time.Minute))
	postKeyed(t, l, "fresh", "key-fresh", now.Add(-time.Minute))

	// The lazy sweep inside Post is interval-guarded, so drive expiry
	// explicitly for determinism.
	if removed := l.ExpireIdempotencyKeys(); removed != 2 {
		t.Fatalf("ExpireIdempotencyKeys removed %d keys, want 2", removed)
	}

	for _, key := range []string{"key-old-1", "key-old-2"} {
		if _, ok := l.GetByIdempotencyKey(key); ok {
			t.Errorf("expired key %q still present", key)
		}
	}
	if _, ok := l.GetByIdempotencyKey("key-fresh"); !ok {
		t.Errorf("fresh key %q was evicted", "key-fresh")
	}

	// Reposting an expired key books a brand-new entry: the replay guard is
	// gone but the journal itself is intact.
	e := JournalEntry{
		ID:             "old-1-retry",
		DebitAccount:   "cash",
		CreditAccount:  "equity",
		AmountCents:    100,
		IdempotencyKey: "key-old-1",
		CreatedAt:      now,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("repost after expiry = dup=%v err=%v, want fresh booking", dup, err)
	}
	if _, ok := l.GetByIdempotencyKey("key-old-1"); !ok {
		t.Errorf("reposted key %q not indexed", "key-old-1")
	}
	// Balances moved twice for key-old-1's account pair: expiry must not
	// have touched the journal.
	if got := l.Balance("cash"); got != 400 {
		t.Errorf("Balance(cash) = %d, want 400 (4 live entries)", got)
	}
}

func TestExpireIdempotencyKeysDisabledByDefault(t *testing.T) {
	l := New()
	postKeyed(t, l, "e-1", "key-1", time.Now().Add(-1000*time.Hour))

	if removed := l.ExpireIdempotencyKeys(); removed != 0 {
		t.Fatalf("ExpireIdempotencyKeys with no TTL removed %d keys, want 0", removed)
	}
	if _, ok := l.GetByIdempotencyKey("key-1"); !ok {
		t.Fatalf("key-1 evicted despite expiry being disabled")
	}
}

func TestExpireIdempotencyKeysNonPositiveTTLIsDisabled(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Hour} {
		l := New(WithIdempotencyTTL(ttl))
		postKeyed(t, l, "e-1", "key-1", time.Now().Add(-1000*time.Hour))
		if removed := l.ExpireIdempotencyKeys(); removed != 0 {
			t.Fatalf("ttl=%v: removed %d keys, want 0", ttl, removed)
		}
	}
}

func TestLazyPruneOnPost(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Nanosecond))
	// Shrink the sweep interval so the lazy path triggers on the next Post.
	l.pruneInterval = 0

	postKeyed(t, l, "old", "key-old", time.Now().Add(-time.Hour))
	if _, ok := l.GetByIdempotencyKey("key-old"); !ok {
		t.Fatalf("key-old missing before lazy prune")
	}

	// This Post must sweep the stale key as a side effect.
	postKeyed(t, l, "new", "key-new", time.Now())
	if _, ok := l.GetByIdempotencyKey("key-old"); ok {
		t.Errorf("lazy prune did not evict stale key-old")
	}
	if _, ok := l.GetByIdempotencyKey("key-new"); !ok {
		t.Errorf("fresh key-new missing after post")
	}
}

func TestLazyPruneRespectsInterval(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	l.pruneInterval = time.Hour // never triggers during this test
	postKeyed(t, l, "old", "key-old", time.Now().Add(-2*time.Hour))
	postKeyed(t, l, "new", "key-new", time.Now())

	if _, ok := l.GetByIdempotencyKey("key-old"); !ok {
		t.Errorf("stale key-old evicted despite interval guard")
	}
}
