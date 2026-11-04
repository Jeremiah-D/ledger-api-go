package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestAuditLogQueueSizeGetterFollowsLiveValue pins the live getter: the
// default capacity is DefaultAuditQueueSize, the construction-time
// WithAuditLogQueueSize value is reported, and a runtime revision moves
// both the getter and the installed channel's capacity.
func TestAuditLogQueueSizeGetterFollowsLiveValue(t *testing.T) {
	l, al, _ := retuneTestLedger(t, WithAuditLogQueueSize(8))
	if got := al.QueueSize(); got != 8 {
		t.Fatalf("QueueSize() = %d, want 8", got)
	}
	if got, ok := l.AuditLogQueueSize(); !ok || got != 8 {
		t.Fatalf("AuditLogQueueSize() = %d, %v; want 8, true", got, ok)
	}
	if got := cap(al.queue); got != 8 {
		t.Fatalf("cap(queue) = %d, want 8", got)
	}
	if err := l.SetAuditLogQueueSize(16); err != nil {
		t.Fatalf("SetAuditLogQueueSize(16): %v", err)
	}
	if got := al.QueueSize(); got != 16 {
		t.Fatalf("QueueSize() after revision = %d, want 16", got)
	}
	if got, ok := l.AuditLogQueueSize(); !ok || got != 16 {
		t.Fatalf("AuditLogQueueSize() after revision = %d, %v; want 16, true", got, ok)
	}
	if got := cap(al.queue); got != 16 {
		t.Fatalf("cap(queue) after revision = %d, want 16", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A log built without the option reports the default.
	_, al2, _ := retuneTestLedger(t)
	if got := al2.QueueSize(); got != DefaultAuditQueueSize {
		t.Fatalf("QueueSize() default = %d, want %d", got, DefaultAuditQueueSize)
	}
	if err := al2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSetAuditLogQueueSizeInvalidRejected pins the fail-closed gate: a
// non-positive capacity is rejected and the live capacity is left
// untouched, with no amendment event in the trail.
func TestSetAuditLogQueueSizeInvalidRejected(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(8))
	for _, n := range []int{0, -1, -4096} {
		if err := l.SetAuditLogQueueSize(n); !errors.Is(err, ErrInvalidAuditLogQueueSize) {
			t.Fatalf("SetAuditLogQueueSize(%d) = %v, want ErrInvalidAuditLogQueueSize", n, err)
		}
	}
	if got := al.QueueSize(); got != 8 {
		t.Fatalf("QueueSize() after rejected revisions = %d, want 8", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["audit_log_queue_size_amended"]); n != 0 {
		t.Fatalf("rejected revisions wrote %d amendment events, want 0", n)
	}
}

// TestSetAuditLogQueueSizeNoAuditLog pins the unconfigured rejection:
// a ledger with no audit log attached cannot revise a queue that does
// not exist.
func TestSetAuditLogQueueSizeNoAuditLog(t *testing.T) {
	l := New()
	if err := l.SetAuditLogQueueSize(16); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("SetAuditLogQueueSize without audit log = %v, want ErrAuditLogNotConfigured", err)
	}
	if err := l.SetAuditLogQueueSizeCtx(ContextWithActor(context.Background(), "ops"), 16); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("SetAuditLogQueueSizeCtx without audit log = %v, want ErrAuditLogNotConfigured", err)
	}
	if got, ok := l.AuditLogQueueSize(); ok || got != 0 {
		t.Fatalf("AuditLogQueueSize() without audit log = %d, %v; want 0, false", got, ok)
	}
}

// TestSetAuditLogQueueSizeSameValueNoop pins the no-op: revising to the
// current capacity returns nil and writes no audit event.
func TestSetAuditLogQueueSizeSameValueNoop(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(8))
	if err := l.SetAuditLogQueueSize(8); err != nil {
		t.Fatalf("SetAuditLogQueueSize(8) same value: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["audit_log_queue_size_amended"]); n != 0 {
		t.Fatalf("same-value revision wrote %d amendment events, want 0", n)
	}
}

// TestSetAuditLogQueueSizeCtxActorAttribution pins LG-73 attribution on
// the Ctx variant: the operator identity replaces the entrypoint name,
// while the plain setter keeps it. The amendment details carry the
// previous and new capacities.
func TestSetAuditLogQueueSizeCtxActorAttribution(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(8))
	ctx := ContextWithActor(context.Background(), "ops-queue")
	if err := l.SetAuditLogQueueSizeCtx(ctx, 32); err != nil {
		t.Fatalf("SetAuditLogQueueSizeCtx: %v", err)
	}
	if err := l.SetAuditLogQueueSize(64); err != nil {
		t.Fatalf("SetAuditLogQueueSize: %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 amendments
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	amended := auditEventsByOp(t, dir)["audit_log_queue_size_amended"]
	if len(amended) != 2 {
		t.Fatalf("audit_log_queue_size_amended events = %d, want 2", len(amended))
	}
	if amended[0].Actor != "ops-queue" {
		t.Errorf("Ctx amendment actor = %q, want ops-queue", amended[0].Actor)
	}
	if amended[0].Details["previous_queue_size"] != float64(8) {
		t.Errorf("Ctx amendment previous_queue_size = %v, want 8", amended[0].Details["previous_queue_size"])
	}
	if amended[0].Details["queue_size"] != float64(32) {
		t.Errorf("Ctx amendment queue_size = %v, want 32", amended[0].Details["queue_size"])
	}
	if amended[1].Actor != "SetAuditLogQueueSize" {
		t.Errorf("plain amendment actor = %q, want SetAuditLogQueueSize", amended[1].Actor)
	}
	if amended[1].Details["previous_queue_size"] != float64(32) {
		t.Errorf("plain amendment previous_queue_size = %v, want 32", amended[1].Details["previous_queue_size"])
	}
	if amended[1].Details["queue_size"] != float64(64) {
		t.Errorf("plain amendment queue_size = %v, want 64", amended[1].Details["queue_size"])
	}
}

// TestAuditLogQueueSizeExpandStopsDrops pins the expand path: a burst
// that overflows the tiny construction-time queue drops events, and
// after the capacity is revised up the same burst drops nothing.
//
// The writer is stalled deterministically by holding signMu: write()
// snapshots the signer under signMu.RLock for every event, so with the
// lock held the writer blocks on the first event it takes and the
// queue fills up from Log alone. No timing is involved — only the
// exact drop count is scheduling-dependent, so only the "> 0" /
// "unchanged" bounds are pinned.
func TestAuditLogQueueSizeExpandStopsDrops(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir, WithAuditLogQueueSize(2))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}

	// Phase 1: stall the writer, overflow the capacity-2 queue.
	al.signMu.Lock()
	for i := 0; i < 200; i++ {
		al.Log(AuditEvent{Op: "burst", TraceID: fmt.Sprintf("pre-%d", i)})
	}
	_, droppedBefore := al.Stats()
	al.signMu.Unlock()
	if droppedBefore == 0 {
		t.Fatalf("dropped = 0 after overflowing a capacity-2 queue, want > 0")
	}
	// Let the writer drain before the revision, so the second phase
	// measures only its own drops.
	waitAuditWritten(t, al, uint64(1+200-droppedBefore)) // alert + enqueued

	// Phase 2: expand, stall the writer again, repeat the burst at
	// 250x the size — the new capacity swallows it whole.
	previous, changed := al.setQueueSize(100000)
	if !changed || previous != 2 {
		t.Fatalf("setQueueSize(100000) = (%d, %v), want (2, true)", previous, changed)
	}
	al.signMu.Lock()
	for i := 0; i < 50000; i++ {
		al.Log(AuditEvent{Op: "burst", TraceID: fmt.Sprintf("post-%d", i)})
	}
	_, droppedAfter := al.Stats()
	al.signMu.Unlock()
	if droppedAfter != droppedBefore {
		t.Fatalf("dropped = %d after expanding to 100000, want %d (unchanged)", droppedAfter, droppedBefore)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAuditLogQueueSizeShrinkPreservesBuffered pins the shrink drain
// deterministically, without the writer: a closed log's buffered
// events move to pending in order when the capacity is revised down
// below the buffered depth, and the new channel carries the new cap.
func TestAuditLogQueueSizeShrinkPreservesBuffered(t *testing.T) {
	a := &AuditLog{
		dir:       t.TempDir(),
		queue:     make(chan AuditEvent, 8),
		queueSize: 8,
		resizeReq: make(chan queueResizeRequest, 1),
		done:      make(chan struct{}),
	}
	a.closed.Store(true)
	for i := 0; i < 5; i++ {
		a.queue <- AuditEvent{Op: "buffered", TraceID: fmt.Sprintf("buf-%d", i)}
	}
	previous, changed := a.setQueueSize(2)
	if !changed || previous != 8 {
		t.Fatalf("setQueueSize(2) = (%d, %v), want (8, true)", previous, changed)
	}
	if got := a.QueueSize(); got != 2 {
		t.Fatalf("QueueSize() after shrink = %d, want 2", got)
	}
	if got := cap(a.queue); got != 2 {
		t.Fatalf("cap(queue) after shrink = %d, want 2", got)
	}
	if got := len(a.pending); got != 5 {
		t.Fatalf("pending events = %d, want 5 (all buffered events survive the shrink)", got)
	}
	for i, ev := range a.pending {
		if want := fmt.Sprintf("buf-%d", i); ev.TraceID != want {
			t.Fatalf("pending[%d].TraceID = %q, want %q (order must survive the drain)", i, ev.TraceID, want)
		}
	}
	// Same value is a no-op even on the closed path: nothing moves.
	if _, changed := a.setQueueSize(2); changed {
		t.Fatalf("same-value setQueueSize on closed log reported changed")
	}
	if got := len(a.pending); got != 5 {
		t.Fatalf("pending events after no-op = %d, want 5", got)
	}
}

// TestAuditLogQueueSizeShrinkKeepsEventsOnDisk pins the live shrink
// end to end: a burst fills the queue, the capacity is revised down,
// and every event Log accepted still lands on disk — the shrink drops
// nothing, and the hash chain verifies across the revision.
func TestAuditLogQueueSizeShrinkKeepsEventsOnDisk(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(512))
	var ids []string
	enqueued := 0
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("shrink-%d", i)
		if al.Log(AuditEvent{Op: "shrink", TraceID: id}) {
			enqueued++
			ids = append(ids, id)
		}
	}
	if err := l.SetAuditLogQueueSize(4); err != nil {
		t.Fatalf("SetAuditLogQueueSize(4): %v", err)
	}
	waitAuditWritten(t, al, uint64(1+enqueued)) // alert + enqueued
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	onDisk := make(map[string]bool, len(events))
	for _, ev := range events {
		onDisk[ev.TraceID] = true
	}
	for _, id := range ids {
		if !onDisk[id] {
			t.Fatalf("enqueued event %s missing from disk after shrink", id)
		}
	}
	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("chain broken after shrink: ok=%v firstBreak=%+v", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != len(events) {
		t.Fatalf("checked entries = %d, want %d (all events on disk)", rep.CheckedEntries, len(events))
	}
}

// TestAuditLogQueueSizeShrinkDropsUntilDrained pins the post-shrink
// admission rule: while the writer is still flushing the backlog a
// shrink drained, new events are dropped by plain channel semantics
// whenever the (smaller) new channel is full — and every event Log
// accepted still lands on disk once the backlog drains.
func TestAuditLogQueueSizeShrinkDropsUntilDrained(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(512))
	enqueuedPre := 0
	var preIDs []string
	for i := 0; i < 3000; i++ {
		id := fmt.Sprintf("pre-%d", i)
		if al.Log(AuditEvent{Op: "pre", TraceID: id}) {
			enqueuedPre++
			preIDs = append(preIDs, id)
		}
	}

	// A concurrent burst starts just before the shrink: its early
	// events land in the old channel and are drained, its later events
	// hit the capacity-2 channel while the writer is still flushing
	// the backlog — those must drop, not stall the poster.
	var wg sync.WaitGroup
	enqueuedDuring, droppedDuring := 0, 0
	var duringIDs []string
	var idsMu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20000; i++ {
			id := fmt.Sprintf("during-%d", i)
			if al.Log(AuditEvent{Op: "during", TraceID: id}) {
				idsMu.Lock()
				enqueuedDuring++
				duringIDs = append(duringIDs, id)
				idsMu.Unlock()
			} else {
				idsMu.Lock()
				droppedDuring++
				idsMu.Unlock()
			}
		}
	}()
	if err := l.SetAuditLogQueueSize(2); err != nil {
		t.Fatalf("SetAuditLogQueueSize(2): %v", err)
	}
	wg.Wait()
	if droppedDuring <= 1000 {
		t.Fatalf("dropped during shrink backlog = %d, want > 1000 (new events must drop while the writer drains the backlog)", droppedDuring)
	}
	waitAuditWritten(t, al, uint64(1+enqueuedPre+enqueuedDuring))
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	onDisk := make(map[string]bool, len(events))
	for _, ev := range events {
		onDisk[ev.TraceID] = true
	}
	for _, id := range append(preIDs, duringIDs...) {
		if !onDisk[id] {
			t.Fatalf("enqueued event %s missing from disk after shrink", id)
		}
	}
	if rep, err := VerifyAuditLog(dir); err != nil || !rep.Ok {
		t.Fatalf("VerifyAuditLog after shrink: rep=%+v err=%v", rep, err)
	}
}

// TestSetAuditLogQueueSizeHashChainContinuous pins the chain across
// revisions: expand then shrink mid-stream, and the on-disk chain
// verifies end to end — the amendment events themselves are chained,
// and no resize loses or reorders a link.
func TestSetAuditLogQueueSizeHashChainContinuous(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogQueueSize(16))
	logBatch := func(prefix string) {
		for i := 0; i < 3; i++ {
			if !al.Log(AuditEvent{Op: "chain", TraceID: fmt.Sprintf("%s-%d", prefix, i)}) {
				t.Fatalf("Log(%s-%d) dropped on a quiet log", prefix, i)
			}
		}
	}
	logBatch("a")
	waitAuditWritten(t, al, 4) // alert + 3
	if err := l.SetAuditLogQueueSize(64); err != nil {
		t.Fatalf("SetAuditLogQueueSize(64): %v", err)
	}
	logBatch("b")
	waitAuditWritten(t, al, 8) // + amendment + 3
	if err := l.SetAuditLogQueueSize(4); err != nil {
		t.Fatalf("SetAuditLogQueueSize(4): %v", err)
	}
	logBatch("c")
	waitAuditWritten(t, al, 12) // + amendment + 3
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	// alert + 9 posts + 2 amendments.
	if len(events) != 12 {
		t.Fatalf("events = %d, want 12 (alert + 9 posts + 2 amendments)", len(events))
	}
	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil || rep.CheckedEntries != 12 {
		t.Fatalf("chain after resizes: ok=%v checked=%d firstBreak=%+v", rep.Ok, rep.CheckedEntries, rep.FirstBreak)
	}
	amended := auditEventsByOp(t, dir)["audit_log_queue_size_amended"]
	if len(amended) != 2 {
		t.Fatalf("audit_log_queue_size_amended events = %d, want 2", len(amended))
	}
	if amended[0].Details["previous_queue_size"] != float64(16) || amended[0].Details["queue_size"] != float64(64) {
		t.Errorf("first amendment details = %v, want previous 16 / 64", amended[0].Details)
	}
	if amended[1].Details["previous_queue_size"] != float64(64) || amended[1].Details["queue_size"] != float64(4) {
		t.Errorf("second amendment details = %v, want previous 64 / 4", amended[1].Details)
	}
}
