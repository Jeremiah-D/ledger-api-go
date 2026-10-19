package ledger

import (
	"testing"
	"time"
)

// TestOutboxSubscribeLiveTail verifies the live-tail subscription
// contract (LG-133): events Emit commits after Subscribe are delivered
// in seq order, unsubscribe stops delivery, and a subscriber that
// falls behind past the queue depth is dropped (not blocking the
// commit path) with its channel closed.
func TestOutboxSubscribeLiveTail(t *testing.T) {
	ob, err := NewOutbox(t.TempDir())
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	defer ob.Close()

	ch, unsub, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	for i := 0; i < 3; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted, EntryID: "e"}) {
			t.Fatalf("Emit %d: false", i)
		}
	}
	for want := uint64(1); want <= 3; want++ {
		select {
		case ev := <-ch:
			if ev.Seq != want {
				t.Fatalf("event seq = %d, want %d", ev.Seq, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for event %d", want)
		}
	}
	// Unsubscribe is idempotent and stops delivery.
	unsub()
	unsub()
	if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
		t.Fatal("Emit after unsubscribe: false")
	}
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("received after unsubscribe: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after unsubscribe")
	}
}

// TestOutboxSubscribeSlowConsumerDropped verifies the backpressure
// contract: a subscriber that never reads is dropped once its queue
// fills, the commit path never blocks, and the drop is counted.
func TestOutboxSubscribeSlowConsumerDropped(t *testing.T) {
	ob, err := NewOutbox(t.TempDir())
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	defer ob.Close()

	ch, _, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	// Never read: Emit must stay non-blocking well past the 512-deep
	// queue, and the subscriber must be dropped exactly once.
	for i := 0; i < 600; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := ob.SlowDrops(); got != 1 {
		t.Fatalf("SlowDrops = %d, want 1", got)
	}
	// The channel is closed: the consumer reconnects with its cursor.
	if _, ok := <-ch; ok {
		// Drain the queued events first: close happens after the drop.
		for {
			if _, ok := <-ch; !ok {
				break
			}
		}
	}
	if drops, ok := New(WithOutbox(ob)).OutboxSlowDrops(); !ok || drops != 1 {
		t.Fatalf("OutboxSlowDrops = %d, %v; want 1, true", drops, ok)
	}
}

// TestOutboxSubscribeClosed verifies Subscribe fails closed on a
// closed outbox.
func TestOutboxSubscribeClosed(t *testing.T) {
	ob, err := NewOutbox(t.TempDir())
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	if err := ob.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, ok := ob.Subscribe(); ok {
		t.Fatal("Subscribe on closed outbox: ok")
	}
}
