package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// pruneTestLedger builds a ledger with an idempotency TTL and an
// audit log in a fresh temp dir.
func pruneTestLedger(t *testing.T, ttl time.Duration) (*Ledger, *AuditLog, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return New(WithAuditLog(al), WithIdempotencyTTL(ttl)), al, dir
}

func TestSetKeyPruneIntervalInvalidRejected(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	for _, bad := range []time.Duration{0, -time.Minute, -time.Second} {
		prev, err := l.SetKeyPruneInterval(bad)
		if !errors.Is(err, ErrInvalidKeyPruneInterval) {
			t.Fatalf("SetKeyPruneInterval(%v) = %v, %v; want ErrInvalidKeyPruneInterval", bad, prev, err)
		}
		if prev != time.Minute {
			t.Fatalf("SetKeyPruneInterval(%v) returned previous %v, want live 1m", bad, prev)
		}
		if got := l.KeyPruneInterval(); got != time.Minute {
			t.Fatalf("KeyPruneInterval() = %v after rejected revision; want 1m", got)
		}
	}
	// The zero-interval rejection reason, pinned: a zero interval
	// cancels the throttle gate, so every Post full-scans the key
	// table (the anti-pattern the ledger.go docs name) — there is
	// no "disable sweeping" semantics in a zero interval.
	l.pruneInterval = 0
	now := time.Now()
	postKeyed(t, l, "old", "key-old", now.Add(-2*time.Hour))
	// Even with lastKeyPrune stamped "just now", the next Post
	// sweeps: the gate is gone.
	l.lastKeyPrune = time.Now()
	postKeyed(t, l, "new", "key-new", now)
	if _, ok := l.GetByIdempotencyKey("key-old"); ok {
		t.Fatal("interval 0 did not sweep on the next Post — the zero-interval trap is not what the code does")
	}
}

func TestSetKeyPruneIntervalSameValueNoop(t *testing.T) {
	l, al, dir := pruneTestLedger(t, time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	prev, err := l.SetKeyPruneInterval(time.Minute)
	if err != nil {
		t.Fatalf("SetKeyPruneInterval(1m) = %v, %v; want 1m, nil", prev, err)
	}
	if prev != time.Minute {
		t.Fatalf("same-value revision returned %v, want 1m", prev)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if byOp := auditEventsByOp(t, dir); len(byOp["key_prune_interval_retuned"]) != 0 {
		t.Fatal("same-value revision wrote a key_prune_interval_retuned event, want none")
	}
}

// pruneSeedExpired posts one key that is already past the TTL, so the
// next lazy sweep (when the interval gate opens) must evict it.
func pruneSeedExpired(t *testing.T, l *Ledger) {
	t.Helper()
	postKeyed(t, l, "old", "key-old", time.Now().Add(-2*time.Hour))
	if _, ok := l.GetByIdempotencyKey("key-old"); !ok {
		t.Fatal("key-old missing before the revision")
	}
}

func TestSetKeyPruneIntervalTightenPrunesOnNextPost(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	pruneSeedExpired(t, l)
	// The last prune ran 61s ago — stale under the 1m default, but
	// the point is the tightened 1s interval opens the gate at once.
	l.lastKeyPrune = time.Now().Add(-61 * time.Second)
	if _, err := l.SetKeyPruneInterval(time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(1s): %v", err)
	}
	postKeyed(t, l, "new", "key-new", time.Now())
	if _, ok := l.GetByIdempotencyKey("key-old"); ok {
		t.Error("tightened interval did not prune the expired key on the next Post")
	}
	if _, ok := l.GetByIdempotencyKey("key-new"); !ok {
		t.Error("fresh key-new missing after post")
	}
}

func TestSetKeyPruneIntervalWidenSkipsNextPost(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	pruneSeedExpired(t, l)
	l.lastKeyPrune = time.Now().Add(-61 * time.Second)
	// 61s is stale under the old 1m interval (a sweep would have
	// fired), but the widened 1h interval defers it.
	if _, err := l.SetKeyPruneInterval(time.Hour); err != nil {
		t.Fatalf("SetKeyPruneInterval(1h): %v", err)
	}
	postKeyed(t, l, "new", "key-new", time.Now())
	if _, ok := l.GetByIdempotencyKey("key-old"); !ok {
		t.Error("widened interval pruned the expired key — the next Post must skip the sweep")
	}
}

func TestSetKeyPruneIntervalMonthlySpendFollows(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()
	staleMonth := now.AddDate(0, -2, 0).Format("2006-01")
	key := monthlySpendKey{account: "cash", currency: "USD", month: staleMonth}
	l.mu.Lock()
	l.monthlySpend[key] = 500
	l.lastBudgetPrune = now.Add(-61 * time.Second)
	l.mu.Unlock()
	if _, err := l.SetKeyPruneInterval(time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(1s): %v", err)
	}
	l.mu.Lock()
	l.maybePruneMonthlySpendLocked(now)
	_, stillThere := l.monthlySpend[key]
	l.mu.Unlock()
	if stillThere {
		t.Error("tightened interval did not prune the stale monthly-spend bucket")
	}
	// Widen: a fresh stale bucket survives the same check.
	l.mu.Lock()
	l.monthlySpend[key] = 500
	l.lastBudgetPrune = now.Add(-61 * time.Second)
	l.mu.Unlock()
	if _, err := l.SetKeyPruneInterval(time.Hour); err != nil {
		t.Fatalf("SetKeyPruneInterval(1h): %v", err)
	}
	l.mu.Lock()
	l.maybePruneMonthlySpendLocked(now)
	_, stillThere = l.monthlySpend[key]
	l.mu.Unlock()
	if !stillThere {
		t.Error("widened interval pruned the stale monthly-spend bucket — the sweep must be deferred")
	}
}

func TestSetKeyPruneIntervalLargeTxReportsFollow(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()
	staleDay := now.AddDate(0, 0, -30).Format("2006-01-02")
	key := largeTxReportKey{account: "cash", currency: "USD", day: staleDay, direction: LargeTxOutflow}
	l.mu.Lock()
	l.largeTxReports[key] = LargeTxReport{Account: "cash", Day: staleDay, Currency: "USD", Direction: LargeTxOutflow, AmountCents: 100, ThresholdCents: 50}
	l.lastLargeTxPrune = now.Add(-61 * time.Second)
	l.mu.Unlock()
	if _, err := l.SetKeyPruneInterval(time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(1s): %v", err)
	}
	l.mu.Lock()
	l.maybePruneLargeTxReportsLocked(now)
	_, stillThere := l.largeTxReports[key]
	l.mu.Unlock()
	if stillThere {
		t.Error("tightened interval did not prune the stale large-tx report")
	}
}

func TestSetKeyPruneIntervalDailyOutflowFollows(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()
	staleDay := now.AddDate(0, 0, -30).Format("2006-01-02")
	key := dailyLimitKey{account: "cash", currency: "USD", day: staleDay}
	l.mu.Lock()
	l.dailyOutflow[key] = 100
	l.lastDailyPrune = now.Add(-61 * time.Second)
	l.mu.Unlock()
	if _, err := l.SetKeyPruneInterval(time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(1s): %v", err)
	}
	l.mu.Lock()
	l.maybePruneDailyOutflowLocked(now)
	_, stillThere := l.dailyOutflow[key]
	l.mu.Unlock()
	if stillThere {
		t.Error("tightened interval did not prune the stale daily-outflow bucket")
	}
}

func TestSetKeyPruneIntervalKeepsLastPruneTimestamps(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	mark := time.Now().Add(-61 * time.Second)
	l.lastKeyPrune = mark
	l.lastBudgetPrune = mark
	l.lastDailyPrune = mark
	l.lastLargeTxPrune = mark
	if _, err := l.SetKeyPruneInterval(5 * time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(5s): %v", err)
	}
	// The revision moves only the interval, never the clocks.
	if !l.lastKeyPrune.Equal(mark) || !l.lastBudgetPrune.Equal(mark) || !l.lastDailyPrune.Equal(mark) || !l.lastLargeTxPrune.Equal(mark) {
		t.Fatalf("revision moved a last-prune timestamp: key=%v budget=%v daily=%v largeTx=%v, want all %v",
			l.lastKeyPrune, l.lastBudgetPrune, l.lastDailyPrune, l.lastLargeTxPrune, mark)
	}
}

func TestSetKeyPruneIntervalDoesNotEvictOrChangeTTL(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	pruneSeedExpired(t, l)
	if _, err := l.SetKeyPruneInterval(time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(1s): %v", err)
	}
	// Stamp the last prune "just now", so the next Post must not
	// sweep even under the tightened interval: the revision itself
	// evicts nothing.
	l.lastKeyPrune = time.Now()
	postKeyed(t, l, "new", "key-new", time.Now())
	if _, ok := l.GetByIdempotencyKey("key-old"); !ok {
		t.Error("revision evicted a key before the new interval elapsed — the revision itself must evict nothing")
	}
	if got := l.idempotencyTTL; got != time.Hour {
		t.Fatalf("idempotencyTTL = %v after interval revision, want 1h (the TTL is LG-242's, untouched)", got)
	}
}

func TestSetKeyPruneIntervalConcurrent(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				interval := time.Duration(1+n%5) * time.Second
				if _, err := l.SetKeyPruneInterval(interval); err != nil {
					t.Errorf("SetKeyPruneInterval(%v): %v", interval, err)
					return
				}
				if _, _, err := l.Post(JournalEntry{ID: fmt.Sprintf("conc-%d-%d", n, j), DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1}); err != nil {
					t.Errorf("Post: %v", err)
					return
				}
				_ = l.KeyPruneInterval()
			}
		}(i)
	}
	wg.Wait()
	if got := l.KeyPruneInterval(); got <= 0 {
		t.Fatalf("KeyPruneInterval() after concurrent revisions = %v, want positive", got)
	}
}

func TestSetKeyPruneIntervalCtxActor(t *testing.T) {
	l, al, dir := pruneTestLedger(t, time.Hour)
	if _, err := l.SetKeyPruneInterval(5 * time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(5s): %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetKeyPruneIntervalCtx(ctx, 10*time.Second); err != nil {
		t.Fatalf("SetKeyPruneIntervalCtx(10s): %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["key_prune_interval_retuned"]
	if len(events) != 2 {
		t.Fatalf("key_prune_interval_retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != DefaultAuditActor {
		t.Fatalf("first revision Actor = %q, want %q (default system)", events[0].Actor, DefaultAuditActor)
	}
	if events[0].Details["previous_prune_interval"] != "1m0s" || events[0].Details["current_prune_interval"] != "5s" {
		t.Fatalf("first revision details = %v, want previous 1m0s / current 5s", events[0].Details)
	}
	if events[1].Actor != "ops-alan" {
		t.Fatalf("second revision Actor = %q, want ops-alan", events[1].Actor)
	}
	if events[1].Details["previous_prune_interval"] != "5s" || events[1].Details["current_prune_interval"] != "10s" {
		t.Fatalf("second revision details = %v, want previous 5s / current 10s", events[1].Details)
	}
}

func TestKeyPruneIntervalFollowsLiveValue(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	if got := l.KeyPruneInterval(); got != time.Minute {
		t.Fatalf("KeyPruneInterval() on a fresh ledger = %v, want 1m (restart restores the default)", got)
	}
	// Repeated revisions back and forth all take effect; the getter
	// follows the live value.
	for _, d := range []time.Duration{5 * time.Second, time.Hour, 30 * time.Second, time.Minute} {
		prev, err := l.SetKeyPruneInterval(d)
		if err != nil {
			t.Fatalf("SetKeyPruneInterval(%v): %v", d, err)
		}
		_ = prev
		if got := l.KeyPruneInterval(); got != d {
			t.Fatalf("KeyPruneInterval() = %v after revising to %v", got, d)
		}
	}
	// A dry-run twin opened after a revision carries the revised
	// interval; the twin clones the live value at clone time.
	if _, err := l.SetKeyPruneInterval(7 * time.Second); err != nil {
		t.Fatalf("SetKeyPruneInterval(7s): %v", err)
	}
	l.mu.Lock()
	twin := l.cloneForDryRunLocked()
	l.mu.Unlock()
	if twin.pruneInterval != 7*time.Second {
		t.Fatalf("dry-run twin pruneInterval = %v, want 7s (twin copies the revised live value)", twin.pruneInterval)
	}
}
