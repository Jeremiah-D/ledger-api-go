package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// drainOutbox reads exactly n buffered events from ch, failing if the
// channel closes early (the subscriber was dropped). A test that
// expects the subscription to survive knows exactly how many events
// it emitted into the unread channel, so a bounded drain is exact.
func drainOutbox(t *testing.T, ch <-chan OutboxEvent, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, ok := <-ch; !ok {
			t.Fatalf("channel closed after %d/%d events: subscriber was dropped", i, n)
		}
	}
}

// assertOutboxOpen fails unless ch is open and drained empty: a
// receive must hit the default branch, not a closed channel and not
// an unexpected extra event.
func assertOutboxOpen(t *testing.T, ch <-chan OutboxEvent) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("channel closed: subscriber was dropped")
		}
		t.Fatal("unexpected extra event on an open channel")
	default:
	}
}

// TestSetOutboxStreamBufferSizeInvalidRejected pins the fail-closed
// gate: a non-positive capacity is rejected and the live capacity is
// left untouched. Zero is rejected, not a "close the queue" state —
// a zero-capacity channel would drop every live subscriber on the
// very next Emit.
func TestSetOutboxStreamBufferSizeInvalidRejected(t *testing.T) {
	l, ob, _ := newOutboxLedger(t)
	for _, n := range []int{0, -1, -4096} {
		prev, err := l.SetOutboxStreamBufferSize(n)
		if !errors.Is(err, ErrInvalidOutboxStreamBufferSize) {
			t.Fatalf("SetOutboxStreamBufferSize(%d) = (%d, %v), want ErrInvalidOutboxStreamBufferSize", n, prev, err)
		}
		if prev != outboxStreamBuffer {
			t.Fatalf("SetOutboxStreamBufferSize(%d) prev = %d, want %d (live)", n, prev, outboxStreamBuffer)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetOutboxStreamBufferSizeCtx(context.Background(), 0); !errors.Is(err, ErrInvalidOutboxStreamBufferSize) || prev != outboxStreamBuffer {
		t.Fatalf("SetOutboxStreamBufferSizeCtx(0) = (%d, %v), want (%d, ErrInvalidOutboxStreamBufferSize)", prev, err, outboxStreamBuffer)
	}
	if got := ob.StreamBufferSize(); got != outboxStreamBuffer {
		t.Fatalf("StreamBufferSize() after rejected revisions = %d, want %d", got, outboxStreamBuffer)
	}
	if got, ok := l.OutboxStreamBufferSize(); !ok || got != outboxStreamBuffer {
		t.Fatalf("OutboxStreamBufferSize() after rejected revisions = %d, %v; want %d, true", got, ok, outboxStreamBuffer)
	}
}

// TestSetOutboxStreamBufferSizeSameValueNoop pins the no-op: revising
// to the current capacity returns the live value with a nil error and
// writes no audit event.
func TestSetOutboxStreamBufferSizeSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l, _, _ := newOutboxLedger(t, WithAuditLog(al))
	prev, err := l.SetOutboxStreamBufferSize(outboxStreamBuffer)
	if err != nil {
		t.Fatalf("SetOutboxStreamBufferSize(%d) same value: %v", outboxStreamBuffer, err)
	}
	if prev != outboxStreamBuffer {
		t.Fatalf("prev = %d, want %d", prev, outboxStreamBuffer)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["outbox_stream_buffer_size_retuned"]); n != 0 {
		t.Fatalf("same-value revision wrote %d retune events, want 0", n)
	}
}

// TestSetOutboxStreamBufferSizeNoOutbox pins the unconfigured
// rejection: a ledger with no outbox attached cannot revise a
// capacity that does not exist.
func TestSetOutboxStreamBufferSizeNoOutbox(t *testing.T) {
	l := New()
	if prev, err := l.SetOutboxStreamBufferSize(1024); !errors.Is(err, ErrOutboxNotConfigured) || prev != 0 {
		t.Fatalf("SetOutboxStreamBufferSize without outbox = (%d, %v), want (0, ErrOutboxNotConfigured)", prev, err)
	}
	if prev, err := l.SetOutboxStreamBufferSizeCtx(ContextWithActor(context.Background(), "ops"), 1024); !errors.Is(err, ErrOutboxNotConfigured) || prev != 0 {
		t.Fatalf("SetOutboxStreamBufferSizeCtx without outbox = (%d, %v), want (0, ErrOutboxNotConfigured)", prev, err)
	}
	if got, ok := l.OutboxStreamBufferSize(); ok || got != 0 {
		t.Fatalf("OutboxStreamBufferSize() without outbox = %d, %v; want 0, false", got, ok)
	}
}

// TestSetOutboxStreamBufferSizeWidenKeepsSlowConsumer pins the widen
// path: a burst that overflows the default 512-deep queue drops a
// slow consumer, and after the capacity is revised up the same burst
// drops nothing.
func TestSetOutboxStreamBufferSizeWidenKeepsSlowConsumer(t *testing.T) {
	// Control: the default 512 drops a never-read subscriber on the
	// 513th event.
	_, obDefault, _ := newOutboxLedger(t)
	chDefault, _, ok := obDefault.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	_ = chDefault // the control asserts on SlowDrops, never reads
	for i := 0; i < 513; i++ {
		if !obDefault.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := obDefault.SlowDrops(); got != 1 {
		t.Fatalf("SlowDrops at default 512 = %d, want 1 (dropped on the 513th event)", got)
	}

	// Widened to 2048, the same 600-event burst (never read) drops
	// nothing: the channel still holds all 600 and stays open.
	l, ob, _ := newOutboxLedger(t)
	prev, err := l.SetOutboxStreamBufferSize(2048)
	if err != nil || prev != outboxStreamBuffer {
		t.Fatalf("SetOutboxStreamBufferSize(2048) = (%d, %v), want (512, nil)", prev, err)
	}
	ch, _, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	for i := 0; i < 600; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := ob.SlowDrops(); got != 0 {
		t.Fatalf("SlowDrops after widening to 2048 = %d, want 0", got)
	}
	drainOutbox(t, ch, 600)
	assertOutboxOpen(t, ch)
}

// TestSetOutboxStreamBufferSizeTightenDropsFaster pins the tighten
// path: a new subscription built at the tightened capacity drops as
// soon as the smaller queue fills.
func TestSetOutboxStreamBufferSizeTightenDropsFaster(t *testing.T) {
	l, ob, _ := newOutboxLedger(t)
	if prev, err := l.SetOutboxStreamBufferSize(4); err != nil || prev != outboxStreamBuffer {
		t.Fatalf("SetOutboxStreamBufferSize(4) = (%d, %v), want (512, nil)", prev, err)
	}
	ch, _, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	for i := 0; i < 5; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := ob.SlowDrops(); got != 1 {
		t.Fatalf("SlowDrops after tightening to 4 = %d, want 1 (dropped on the 5th event)", got)
	}
	// The channel holds the 4 buffered events, then reports closed.
	drainOutbox(t, ch, 4)
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after the drop")
	}
}

// TestSetOutboxStreamBufferSizeInFlightKeepsCapacity pins the
// future-only rule: a subscription already open keeps the capacity
// its channel was built with, while a subscription opened after the
// revision is built at the new capacity.
func TestSetOutboxStreamBufferSizeInFlightKeepsCapacity(t *testing.T) {
	l, ob, _ := newOutboxLedger(t)
	chOld, unsubOld, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	if _, err := l.SetOutboxStreamBufferSize(4); err != nil {
		t.Fatalf("SetOutboxStreamBufferSize(4): %v", err)
	}
	// The in-flight subscription still runs at 512: 512 unread
	// events fill it exactly without a drop (at 4 it would have
	// dropped on the 5th).
	for i := 0; i < 512; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := ob.SlowDrops(); got != 0 {
		t.Fatalf("SlowDrops = %d after 512 events into the in-flight subscription, want 0", got)
	}
	drainOutbox(t, chOld, 512)
	assertOutboxOpen(t, chOld)
	unsubOld()
	// A subscription opened after the revision is built at 4: the
	// 5th event drops it.
	chNew, _, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	for i := 0; i < 5; i++ {
		if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
			t.Fatalf("Emit %d blocked or failed", i)
		}
	}
	if got := ob.SlowDrops(); got != 1 {
		t.Fatalf("SlowDrops = %d after 5 events into the new subscription, want 1", got)
	}
	drainOutbox(t, chNew, 4)
	if _, ok := <-chNew; ok {
		t.Fatal("new channel still open after the drop")
	}
}

// TestSetOutboxStreamBufferSizeNeverBlocksEmit pins the commit path:
// tightening the capacity never makes Emit block — a slow consumer is
// dropped, not waited on — so a burst always completes.
func TestSetOutboxStreamBufferSizeNeverBlocksEmit(t *testing.T) {
	l, ob, _ := newOutboxLedger(t)
	if _, err := l.SetOutboxStreamBufferSize(1); err != nil {
		t.Fatalf("SetOutboxStreamBufferSize(1): %v", err)
	}
	ch, _, ok := ob.Subscribe()
	if !ok {
		t.Fatal("Subscribe: not ok")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 600; i++ {
			if !ob.Emit(OutboxEvent{Type: OutboxEntryPosted}) {
				t.Errorf("Emit %d blocked or failed", i)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Emit blocked behind a tightened capacity for 30s")
	}
	if got := ob.SlowDrops(); got != 1 {
		t.Fatalf("SlowDrops = %d, want 1 (exactly one slow-consumer drop)", got)
	}
	drainOutbox(t, ch, 1)
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after the drop")
	}
}

// TestSetOutboxStreamBufferSizeConcurrent pins the lock order under
// -race: concurrent revisions, emits, and subscribes never deadlock
// and never race — the setter takes only streamBufferSizeMu, never
// o.mu, and Subscribe reads the live capacity under o.mu.
func TestSetOutboxStreamBufferSizeConcurrent(t *testing.T) {
	l, ob, _ := newOutboxLedger(t)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := l.SetOutboxStreamBufferSize(64 + g*16 + i%8); err != nil {
					t.Errorf("SetOutboxStreamBufferSize: %v", err)
					return
				}
			}
		}(g)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ob.Emit(OutboxEvent{Type: OutboxEntryPosted})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ch, unsub, ok := ob.Subscribe()
				if !ok {
					continue
				}
				select {
				case <-ch:
				case <-time.After(100 * time.Millisecond):
				}
				unsub()
			}
		}()
	}
	wg.Wait()
	got, ok := l.OutboxStreamBufferSize()
	if !ok {
		t.Fatal("OutboxStreamBufferSize() not ok after concurrent revisions")
	}
	if got < 64 || got > 64+3*16+7 {
		t.Fatalf("OutboxStreamBufferSize() = %d, want one of the revised values (64..119)", got)
	}
	if got != ob.StreamBufferSize() {
		t.Fatalf("ledger getter %d != outbox getter %d", got, ob.StreamBufferSize())
	}
}

// TestSetOutboxStreamBufferSizeCtxActorAttribution pins LG-73
// attribution on the Ctx variant: the operator identity replaces the
// default actor, while the plain setter keeps the "system" default.
func TestSetOutboxStreamBufferSizeCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l, _, _ := newOutboxLedger(t, WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-buffer")
	if prev, err := l.SetOutboxStreamBufferSizeCtx(ctx, 1024); err != nil || prev != outboxStreamBuffer {
		t.Fatalf("SetOutboxStreamBufferSizeCtx(1024) = (%d, %v), want (512, nil)", prev, err)
	}
	if prev, err := l.SetOutboxStreamBufferSize(2048); err != nil || prev != 1024 {
		t.Fatalf("SetOutboxStreamBufferSize(2048) = (%d, %v), want (1024, nil)", prev, err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["outbox_stream_buffer_size_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("outbox_stream_buffer_size_retuned events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-buffer" {
		t.Errorf("Ctx retune actor = %q, want ops-buffer", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
	if retuned[0].Details["previous_buffer_size"] != float64(512) || retuned[0].Details["current_buffer_size"] != float64(1024) {
		t.Errorf("first retune details = %v, want previous 512 / current 1024", retuned[0].Details)
	}
	if retuned[1].Details["previous_buffer_size"] != float64(1024) || retuned[1].Details["current_buffer_size"] != float64(2048) {
		t.Errorf("second retune details = %v, want previous 1024 / current 2048", retuned[1].Details)
	}
}

// TestSetOutboxStreamBufferSizeAuditDetailsExact pins the audit
// payload: exactly the two integer capacities, previous and current —
// no event content ever rides in a retune event.
func TestSetOutboxStreamBufferSizeAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l, _, _ := newOutboxLedger(t, WithAuditLog(al))
	if _, err := l.SetOutboxStreamBufferSize(100); err != nil {
		t.Fatalf("SetOutboxStreamBufferSize(100): %v", err)
	}
	if _, err := l.SetOutboxStreamBufferSize(200); err != nil {
		t.Fatalf("SetOutboxStreamBufferSize(200): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["outbox_stream_buffer_size_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("outbox_stream_buffer_size_retuned events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_buffer_size": float64(512), "current_buffer_size": float64(100)},
		{"previous_buffer_size": float64(100), "current_buffer_size": float64(200)},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_buffer_size + current_buffer_size", i, ev.Details)
		}
		for k, v := range want[i] {
			if ev.Details[k] != v {
				t.Errorf("retune[%d] details[%q] = %v, want %v", i, k, ev.Details[k], v)
			}
		}
		if fmt.Sprintf("%v", ev.Details) == "" {
			t.Fatalf("retune[%d] details empty", i)
		}
	}
}

// TestOutboxStreamBufferSizeGetterFollowsLiveValue pins the live
// getter: it follows every revision, repeated revisions round-trip,
// and a fresh outbox (a restart) restores the 512 default.
func TestOutboxStreamBufferSizeGetterFollowsLiveValue(t *testing.T) {
	l, ob, dir := newOutboxLedger(t)
	if got, ok := l.OutboxStreamBufferSize(); !ok || got != outboxStreamBuffer {
		t.Fatalf("OutboxStreamBufferSize() = %d, %v; want 512, true", got, ok)
	}
	for _, n := range []int{2048, 64, 4096, 512} {
		prev, err := l.SetOutboxStreamBufferSize(n)
		if err != nil {
			t.Fatalf("SetOutboxStreamBufferSize(%d): %v", n, err)
		}
		_ = prev
		if got := ob.StreamBufferSize(); got != n {
			t.Fatalf("StreamBufferSize() after revision to %d = %d", n, got)
		}
		if got, ok := l.OutboxStreamBufferSize(); !ok || got != n {
			t.Fatalf("OutboxStreamBufferSize() after revision to %d = %d, %v", n, got, ok)
		}
	}
	// A restart restores the default: a fresh outbox on the same
	// directory seeds 512, regardless of the live revision.
	ob2, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox (restart): %v", err)
	}
	defer func() { _ = ob2.Close() }()
	if got := ob2.StreamBufferSize(); got != outboxStreamBuffer {
		t.Fatalf("restarted StreamBufferSize() = %d, want %d", got, outboxStreamBuffer)
	}
}
