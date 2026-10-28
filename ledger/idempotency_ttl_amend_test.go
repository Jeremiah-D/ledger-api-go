package ledger

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestSetIdempotencyTTLAppliesToLaterSweeps(t *testing.T) {
	l := New()
	now := time.Now()

	postKeyed(t, l, "ttl-old", "key-ttl-old", now.Add(-2*time.Hour))
	postKeyed(t, l, "ttl-fresh", "key-ttl-fresh", now.Add(-time.Minute))

	// Disabled by default: the explicit sweep removes nothing.
	if removed := l.ExpireIdempotencyKeys(); removed != 0 {
		t.Fatalf("disabled ExpireIdempotencyKeys removed %d keys, want 0", removed)
	}

	// Revising to one hour makes the next sweep evict only the
	// two-hour-old key; the fresh key survives.
	l.SetIdempotencyTTL(time.Hour)
	if removed := l.ExpireIdempotencyKeys(); removed != 1 {
		t.Fatalf("1h ExpireIdempotencyKeys removed %d keys, want 1", removed)
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-old"); ok {
		t.Errorf("old key survived the 1h sweep")
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-fresh"); !ok {
		t.Errorf("fresh key evicted by the 1h sweep")
	}

	// Shortening to a nanosecond makes the next sweep evict the
	// fresh key too: the new TTL governs the next sweep, not the
	// TTL that was live when the key was posted.
	l.SetIdempotencyTTL(time.Nanosecond)
	if removed := l.ExpireIdempotencyKeys(); removed != 1 {
		t.Fatalf("nanosecond ExpireIdempotencyKeys removed %d keys, want 1", removed)
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-fresh"); ok {
		t.Errorf("fresh key survived the nanosecond sweep")
	}

	// The journal is untouched by every sweep above: two entries
	// of 100 cents each still stand.
	if got := l.Balance("cash"); got != 200 {
		t.Errorf("Balance(cash) = %d, want 200 (sweeps must not touch the journal)", got)
	}

	// Lengthening (or disabling) the TTL never resurrects an
	// evicted key: both keys stay gone under a 24-hour TTL.
	l.SetIdempotencyTTL(24 * time.Hour)
	if removed := l.ExpireIdempotencyKeys(); removed != 0 {
		t.Fatalf("24h ExpireIdempotencyKeys removed %d keys, want 0", removed)
	}
	for _, key := range []string{"key-ttl-old", "key-ttl-fresh"} {
		if _, ok := l.GetByIdempotencyKey(key); ok {
			t.Errorf("evicted key %q resurrected by lengthening the TTL", key)
		}
	}

	// Reposting an evicted key books a brand-new entry.
	e := JournalEntry{
		ID: "ttl-old-retry", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 100, IdempotencyKey: "key-ttl-old", CreatedAt: now,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("repost after expiry = dup=%v err=%v, want fresh booking", dup, err)
	}
	if got := l.Balance("cash"); got != 300 {
		t.Errorf("Balance(cash) = %d, want 300 after the fresh repost", got)
	}
}

func TestSetIdempotencyTTLDisablesExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Nanosecond))
	postKeyed(t, l, "ttl-dis-1", "key-ttl-dis", time.Now().Add(-2*time.Hour))

	// Zero disables expiry: the stale key survives the sweep.
	l.SetIdempotencyTTL(0)
	if removed := l.ExpireIdempotencyKeys(); removed != 0 {
		t.Fatalf("0 TTL ExpireIdempotencyKeys removed %d keys, want 0", removed)
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-dis"); !ok {
		t.Errorf("key evicted despite the TTL being disabled")
	}

	// A negative TTL disables expiry the same way (LG-04).
	l.SetIdempotencyTTL(-time.Hour)
	if removed := l.ExpireIdempotencyKeys(); removed != 0 {
		t.Fatalf("negative TTL ExpireIdempotencyKeys removed %d keys, want 0", removed)
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-dis"); !ok {
		t.Errorf("key evicted despite the negative TTL disabling expiry")
	}
}

func TestSetIdempotencyTTLLazyPruneReadsLiveTTL(t *testing.T) {
	l := New()
	// Shrink the sweep interval so the lazy path triggers on the
	// next Post (the production interval is a minute).
	l.pruneInterval = 0

	// Posted while expiry is disabled: the pre-insert lazy sweep
	// is a no-op, so the stale key survives its own Post.
	postKeyed(t, l, "ttl-lazy-old", "key-ttl-lazy-old", time.Now().Add(-2*time.Hour))
	if _, ok := l.GetByIdempotencyKey("key-ttl-lazy-old"); !ok {
		t.Fatalf("old key missing before the lazy prune")
	}

	// Revising to one hour makes the next Post's pre-insert lazy
	// sweep evict the two-hour-old key — the lazy path reads the
	// revised TTL, not the disabled TTL live at posting time.
	l.SetIdempotencyTTL(time.Hour)
	postKeyed(t, l, "ttl-lazy-new", "key-ttl-lazy-new", time.Now())
	if _, ok := l.GetByIdempotencyKey("key-ttl-lazy-old"); ok {
		t.Errorf("lazy prune did not evict the stale key under the revised TTL")
	}
	if _, ok := l.GetByIdempotencyKey("key-ttl-lazy-new"); !ok {
		t.Errorf("fresh key-new missing after post")
	}
}

func TestSetIdempotencyTTLAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithIdempotencyTTL(time.Hour))

	ctx := ContextWithActor(context.Background(), "ops-desk")
	l.SetIdempotencyTTLCtx(ctx, 30*time.Minute)

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"idempotency_ttl"`) || !strings.Contains(buf.String(), "30m0s") {
		t.Errorf("snapshot config line lacks the amended idempotency_ttl 30m0s")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	// A 45-minute-old key expires under the restored 30-minute
	// TTL but would survive the original one-hour TTL, so this
	// sweep proves the amended TTL (not the original) survived.
	postKeyed(t, restored, "ttl-restored", "key-ttl-restored", time.Now().Add(-45*time.Minute))
	if removed := restored.ExpireIdempotencyKeys(); removed != 1 {
		t.Fatalf("restored ExpireIdempotencyKeys removed %d keys, want 1 (30m TTL)", removed)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["idempotency_ttl_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	if events[0].Details["previous_ttl"] != "1h0m0s" || events[0].Details["ttl"] != "30m0s" {
		t.Errorf("event details = %+v, want 1h0m0s -> 30m0s", events[0].Details)
	}
}
