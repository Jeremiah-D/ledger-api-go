package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Outbound webhook endpoint queue-capacity runtime revision (LG-343):
// SetWebhookQueueSize revises the live per-endpoint delivery-queue
// capacity in place — widening takes effect from the next Notify,
// tightening never drops a buffered event (survivors drain in
// order) while new events drop by plain channel semantics until the
// backlog drains — with no restart and no endpoint teardown. Tests
// reuse webhookTestEndpoint / webhookTestLedger / mustRotatingKey
// from ledger_webhook_test.go and deliveryAmendTestLedger /
// deliveryRecordFor / auditEventsByOp / auditDetailFloat /
// timedWebhookCapture from webhook_delivery_amend_test.go.

// queueSizeTestEndpoint builds a fast endpoint config against the
// stub with the given queue capacity.
func queueSizeTestEndpoint(t *testing.T, name, url string, keys *RotatingKey, queueSize int) WebhookEndpointConfig {
	t.Helper()
	cfg := webhookTestEndpoint(t, name, url, keys)
	cfg.QueueSize = queueSize
	return cfg
}

// queueSizeTestDispatcher builds a dispatcher with one registered
// endpoint ("risk") against the given handler and the given queue
// capacity; tests drive Notify directly for determinism.
func queueSizeTestDispatcher(t *testing.T, handler http.Handler, queueSize int) *WebhookDispatcher {
	t.Helper()
	d, err := NewWebhookDispatcher(queueSizeTestEndpoint(t, "risk", mustQueueSizeTestServer(t, handler), mustRotatingKey(t, "primary", "s3cr3t"), queueSize))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	return d
}

func mustQueueSizeTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// gatedWebhookCapture is an httptest stub that blocks every delivery
// attempt until release is closed, so a test can fill the endpoint
// queue deterministically while the worker is busy on the first
// event. It records the delivered event seqs in arrival order.
type gatedWebhookCapture struct {
	mu          sync.Mutex
	seqs        []string
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func newGatedWebhookCapture() *gatedWebhookCapture {
	return &gatedWebhookCapture{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *gatedWebhookCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.enteredOnce.Do(func() { close(c.entered) })
		<-c.release
		c.mu.Lock()
		c.seqs = append(c.seqs, r.Header.Get("X-Ledger-Event-Seq"))
		c.mu.Unlock()
		w.WriteHeader(200)
	})
}

func (c *gatedWebhookCapture) seqsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seqs))
	copy(out, c.seqs)
	return out
}

// slowSeqWebhookCapture is an httptest stub that sleeps delay per
// delivery attempt (so the worker stays busy long enough for the
// test to revise the queue mid-flight) and records seqs. entered
// closes on the first attempt, before the sleep, so a test can wait
// until the worker has taken the first event before filling the
// queue.
type slowSeqWebhookCapture struct {
	mu          sync.Mutex
	seqs        []string
	delay       time.Duration
	entered     chan struct{}
	enteredOnce sync.Once
}

func (c *slowSeqWebhookCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.enteredOnce.Do(func() {
			if c.entered != nil {
				close(c.entered)
			}
		})
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		c.mu.Lock()
		c.seqs = append(c.seqs, r.Header.Get("X-Ledger-Event-Seq"))
		c.mu.Unlock()
		w.WriteHeader(200)
	})
}

func (c *slowSeqWebhookCapture) seqsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seqs))
	copy(out, c.seqs)
	return out
}

// notifyQueueSizeSeqs fans seq-numbered events straight into the
// dispatcher, bypassing the journal: the seqs are the outbox seq
// order the test asserts delivery order against.
func notifyQueueSizeSeqs(d *WebhookDispatcher, first, count int) {
	for i := 0; i < count; i++ {
		d.Notify(OutboxEvent{Type: "transfer_posted", Seq: uint64(first + i)})
	}
}

// waitForQueueSizeSeqs polls a capture until n seqs arrived, and
// returns them in delivery order.
func waitForQueueSizeSeqs(t *testing.T, snapshot func() []string, n int, msg string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d delivered seqs: %s (got %v)", n, msg, snapshot())
	return nil
}

// waitForQueueSizeStats polls the dispatcher until delivered+dropped
// reaches total — every notified event is either delivered or
// dropped — or fails the test.
func waitForQueueSizeStats(t *testing.T, d *WebhookDispatcher, total uint64, msg string) (delivered, dropped uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		delivered, _, _, dropped = d.Stats()
		if delivered+dropped >= total {
			return delivered, dropped
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events settled: %s (delivered=%d dropped=%d)", total, msg, delivered, dropped)
	return 0, 0
}

func assertQueueSizeSeqs(t *testing.T, got []string, want []string, msg string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: delivered seqs = %v, want %v", msg, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: delivered seqs = %v, want %v", msg, got, want)
		}
	}
}

func TestSetWebhookQueueSizeRejectsNonPositiveAndKeepsLive(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	for _, n := range []int{0, -1, -1024} {
		if got, err := d.SetQueueSize("risk", n); !errors.Is(err, ErrInvalidWebhookQueueSize) || got != 64 {
			t.Fatalf("SetQueueSize(%d) = (%d, %v), want (64, ErrInvalidWebhookQueueSize)", n, got, err)
		}
	}
	if got, ok := d.QueueSize("risk"); !ok || got != 64 {
		t.Fatalf("QueueSize() = (%d, %v) after rejected revisions, want (64, true)", got, ok)
	}

	l := webhookTestLedger(t, d)
	for _, n := range []int{0, -5} {
		if got, err := l.SetWebhookQueueSize("risk", n); !errors.Is(err, ErrInvalidWebhookQueueSize) || got != 64 {
			t.Fatalf("Ledger.SetWebhookQueueSize(%d) = (%d, %v), want (64, ErrInvalidWebhookQueueSize)", n, got, err)
		}
	}
	if got, ok := l.WebhookQueueSize("risk"); !ok || got != 64 {
		t.Fatalf("Ledger.WebhookQueueSize() = (%d, %v) after rejected revisions, want (64, true)", got, ok)
	}

	// Check order: the capacity gate beats the missing dispatcher
	// (an illegal capacity is rejected even with no dispatcher).
	bare := New()
	if got, err := bare.SetWebhookQueueSize("risk", 0); !errors.Is(err, ErrInvalidWebhookQueueSize) || got != 0 {
		t.Fatalf("SetWebhookQueueSize without dispatcher, n=0 = (%d, %v), want (0, ErrInvalidWebhookQueueSize)", got, err)
	}
	if got, err := bare.SetWebhookQueueSize("risk", 5); !errors.Is(err, ErrWebhooksNotConfigured) || got != 0 {
		t.Fatalf("SetWebhookQueueSize without dispatcher, n=5 = (%d, %v), want (0, ErrWebhooksNotConfigured)", got, err)
	}
	if got, ok := bare.WebhookQueueSize("risk"); ok || got != 0 {
		t.Fatalf("WebhookQueueSize() without dispatcher = (%d, %v), want (0, false)", got, ok)
	}
}

func TestSetWebhookQueueSizeUnknownEndpoint(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	if got, err := d.SetQueueSize("ghost", 5); !errors.Is(err, ErrWebhookEndpointNotFound) || got != 0 {
		t.Fatalf("SetQueueSize(ghost) = (%d, %v), want (0, ErrWebhookEndpointNotFound)", got, err)
	}
	if got, ok := d.QueueSize("ghost"); ok || got != 0 {
		t.Fatalf("QueueSize(ghost) = (%d, %v), want (0, false)", got, ok)
	}
	l := webhookTestLedger(t, d)
	if got, err := l.SetWebhookQueueSize("ghost", 5); !errors.Is(err, ErrWebhookEndpointNotFound) || got != 0 {
		t.Fatalf("Ledger.SetWebhookQueueSize(ghost) = (%d, %v), want (0, ErrWebhookEndpointNotFound)", got, err)
	}
	if got, ok := l.WebhookQueueSize("ghost"); ok || got != 0 {
		t.Fatalf("Ledger.WebhookQueueSize(ghost) = (%d, %v), want (0, false)", got, ok)
	}
}

func TestSetWebhookQueueSizeSameValueNoOpNoAudit(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	l, al, dir := deliveryAmendTestLedger(t, d)
	prev, err := l.SetWebhookQueueSize("risk", 64)
	if err != nil || prev != 64 {
		t.Fatalf("SetWebhookQueueSize(64) = (%d, %v), want (64, nil)", prev, err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_queue_size_retuned"]; len(events) != 0 {
		t.Fatalf("retuned events = %d for a same-value revision, want 0", len(events))
	}
}

func TestSetWebhookQueueSizeWidenTakesEffectImmediately(t *testing.T) {
	cap := newGatedWebhookCapture()
	d := queueSizeTestDispatcher(t, cap.handler(), 2)
	// Event 1 is in flight (blocked in the handler); events 2 and 3
	// fill the old capacity-2 queue, and event 4 drops.
	notifyQueueSizeSeqs(d, 1, 1)
	<-cap.entered
	notifyQueueSizeSeqs(d, 2, 2)
	notifyQueueSizeSeqs(d, 4, 1)
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("dropped = %d after filling the capacity-2 queue, want 1", dropped)
	}
	// Widen while the worker is blocked: the revision applies once
	// the in-flight delivery completes.
	type revisionResult struct {
		prev int
		err  error
	}
	resCh := make(chan revisionResult, 1)
	go func() {
		prev, err := d.SetQueueSize("risk", 4)
		resCh <- revisionResult{prev, err}
	}()
	close(cap.release)
	select {
	case res := <-resCh:
		if res.err != nil || res.prev != 2 {
			t.Fatalf("SetQueueSize(4) = (%d, %v), want (2, nil)", res.prev, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetQueueSize(4) did not return after the release")
	}
	if got, ok := d.QueueSize("risk"); !ok || got != 4 {
		t.Fatalf("QueueSize() = (%d, %v) after widening, want (4, true)", got, ok)
	}
	// Events 5 and 6 now fit the widened queue — the drop count
	// stops growing exactly where the old capacity stopped it.
	notifyQueueSizeSeqs(d, 5, 2)
	got := waitForQueueSizeSeqs(t, cap.seqsSnapshot, 5, "deliveries after widening")
	assertQueueSizeSeqs(t, got, []string{"1", "2", "3", "5", "6"}, "widened delivery order")
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("dropped = %d after widening, want 1 (no further drops)", dropped)
	}
}

func TestSetWebhookQueueSizeShrinkLosesNoBufferedEvent(t *testing.T) {
	cap := newGatedWebhookCapture()
	d := queueSizeTestDispatcher(t, cap.handler(), 4)
	// Event 1 is in flight (blocked in the handler); events 2–5 fill
	// the capacity-4 queue, and event 6 drops under the old capacity.
	notifyQueueSizeSeqs(d, 1, 1)
	<-cap.entered
	notifyQueueSizeSeqs(d, 2, 4)
	notifyQueueSizeSeqs(d, 6, 1)
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("dropped = %d after filling the capacity-4 queue, want 1", dropped)
	}
	// Shrink while the worker is blocked: the revision applies once
	// the in-flight delivery completes, draining 2–5 into pending.
	type revisionResult struct {
		prev int
		err  error
	}
	resCh := make(chan revisionResult, 1)
	go func() {
		prev, err := d.SetQueueSize("risk", 1)
		resCh <- revisionResult{prev, err}
	}()
	close(cap.release)
	select {
	case res := <-resCh:
		if res.err != nil || res.prev != 4 {
			t.Fatalf("SetQueueSize(1) = (%d, %v), want (4, nil)", res.prev, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetQueueSize(1) did not return after the release")
	}
	if got, ok := d.QueueSize("risk"); !ok || got != 1 {
		t.Fatalf("QueueSize() = (%d, %v) after shrinking, want (1, true)", got, ok)
	}
	// Every buffered event survives the shrink, in outbox seq order.
	got := waitForQueueSizeSeqs(t, cap.seqsSnapshot, 5, "deliveries after shrinking")
	assertQueueSizeSeqs(t, got, []string{"1", "2", "3", "4", "5"}, "shrunk delivery order")
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("dropped = %d after shrinking, want 1 (no buffered event dropped)", dropped)
	}
}

func TestSetWebhookQueueSizeShrinkDropsNewEventsUntilBacklogDrains(t *testing.T) {
	cap := &slowSeqWebhookCapture{delay: 150 * time.Millisecond, entered: make(chan struct{})}
	d := queueSizeTestDispatcher(t, cap.handler(), 2)
	// Event 1 is in flight (slow handler); events 2 and 3 fill the
	// capacity-2 queue, and event 4 drops under the old capacity.
	notifyQueueSizeSeqs(d, 1, 1)
	<-cap.entered
	notifyQueueSizeSeqs(d, 2, 2)
	notifyQueueSizeSeqs(d, 4, 1)
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("dropped = %d after filling the capacity-2 queue, want 1", dropped)
	}
	// Shrink to 1: the call blocks until the worker applies it at
	// its next select point (after the in-flight delivery), draining
	// whatever is still buffered into pending.
	if prev, err := d.SetQueueSize("risk", 1); err != nil || prev != 2 {
		t.Fatalf("SetQueueSize(1) = (%d, %v), want (2, nil)", prev, err)
	}
	// The backlog drains in order; every buffered event survives
	// the shrink.
	got := waitForQueueSizeSeqs(t, cap.seqsSnapshot, 3, "backlog draining after shrinking")
	assertQueueSizeSeqs(t, got, []string{"1", "2", "3"}, "backlog order after shrinking")
	// The worker is idle now, so event 5 is taken at once and keeps
	// it busy for one slow delivery; event 6 fits the new
	// capacity-1 channel, and event 7 drops by plain channel
	// semantics while the worker is still busy.
	notifyQueueSizeSeqs(d, 5, 3)
	got = waitForQueueSizeSeqs(t, cap.seqsSnapshot, 5, "deliveries after shrinking")
	assertQueueSizeSeqs(t, got, []string{"1", "2", "3", "5", "6"}, "delivery order across the shrink")
	if _, _, _, dropped := d.Stats(); dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (event 4 under the old capacity, event 7 under the new one)", dropped)
	}
	// Once the backlog drains, new events are accepted again.
	notifyQueueSizeSeqs(d, 8, 1)
	got = waitForQueueSizeSeqs(t, cap.seqsSnapshot, 6, "delivery after the backlog drained")
	assertQueueSizeSeqs(t, got, []string{"1", "2", "3", "5", "6", "8"}, "delivery order after the backlog drained")
	if _, _, _, dropped := d.Stats(); dropped != 2 {
		t.Fatalf("dropped = %d after the backlog drained, want 2 (no further drops)", dropped)
	}
}

func TestSetWebhookQueueSizeAuditsPreviousAndCurrent(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	l, al, dir := deliveryAmendTestLedger(t, d)
	prev, err := l.SetWebhookQueueSize("risk", 2048)
	if err != nil || prev != 64 {
		t.Fatalf("SetWebhookQueueSize(2048) = (%d, %v), want (64, nil)", prev, err)
	}
	if got, ok := l.WebhookQueueSize("risk"); !ok || got != 2048 {
		t.Fatalf("WebhookQueueSize() = (%d, %v), want (2048, true)", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_queue_size_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetWebhookQueueSize" {
		t.Fatalf("event Actor = %q, want SetWebhookQueueSize", ev.Actor)
	}
	if got, ok := ev.Details["endpoint"]; !ok || got != "risk" {
		t.Fatalf("event endpoint detail = %v, want risk", ev.Details["endpoint"])
	}
	if got := auditDetailFloat(t, ev, "previous_queue_size"); got != 64 {
		t.Fatalf("previous_queue_size = %v, want 64", got)
	}
	if got := auditDetailFloat(t, ev, "current_queue_size"); got != 2048 {
		t.Fatalf("current_queue_size = %v, want 2048", got)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetWebhookQueueSizeCtxRecordsOperator(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	l, al, dir := deliveryAmendTestLedger(t, d)
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetWebhookQueueSizeCtx(ctx, "risk", 512); err != nil {
		t.Fatalf("SetWebhookQueueSizeCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_queue_size_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("event Actor = %q, want ops-alan", events[0].Actor)
	}
}

func TestSetWebhookQueueSizeGetterFollowsLiveValue(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	if got, ok := d.QueueSize("risk"); !ok || got != 64 {
		t.Fatalf("QueueSize() on a fresh dispatcher = (%d, %v), want (64, true)", got, ok)
	}
	if _, err := d.SetQueueSize("risk", 128); err != nil {
		t.Fatalf("SetQueueSize(128): %v", err)
	}
	if got, ok := d.QueueSize("risk"); !ok || got != 128 {
		t.Fatalf("QueueSize() = (%d, %v) after revision, want (128, true)", got, ok)
	}
	l := webhookTestLedger(t, d)
	if got, ok := l.WebhookQueueSize("risk"); !ok || got != 128 {
		t.Fatalf("Ledger.WebhookQueueSize() = (%d, %v), want (128, true)", got, ok)
	}
	if _, err := l.SetWebhookQueueSize("risk", 256); err != nil {
		t.Fatalf("SetWebhookQueueSize(256): %v", err)
	}
	if got, ok := l.WebhookQueueSize("risk"); !ok || got != 256 {
		t.Fatalf("Ledger.WebhookQueueSize() = (%d, %v) after revision, want (256, true)", got, ok)
	}
}

func TestSetWebhookQueueSizeConcurrentRevisionNotifyAndDeliveryRaceFree(t *testing.T) {
	cap := &slowSeqWebhookCapture{delay: 5 * time.Millisecond}
	d := queueSizeTestDispatcher(t, cap.handler(), 2)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := d.SetQueueSize("risk", 1+(n+j)%8); err != nil {
					t.Errorf("SetQueueSize: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := uint64(1); seq <= 60; seq++ {
			d.Notify(OutboxEvent{Type: "transfer_posted", Seq: seq})
		}
	}()
	wg.Wait()
	delivered, dropped := waitForQueueSizeStats(t, d, 60, "concurrent revisions and notifies")
	got := cap.seqsSnapshot()
	if uint64(len(got)) != delivered {
		t.Fatalf("delivered seqs = %d, want %d (Stats delivered)", len(got), delivered)
	}
	// Per-endpoint delivery order still equals outbox seq order:
	// every delivered seq is strictly greater than the previous
	// one, so no event overtook an older one across a generation
	// swap, and none was delivered twice.
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("delivered seqs out of order: %v", got)
		}
	}
	live, ok := d.QueueSize("risk")
	if !ok || live < 1 || live > 8 {
		t.Fatalf("QueueSize() = (%d, %v) after concurrent revisions, want 1..8", live, ok)
	}
	_ = dropped
}

func TestSetWebhookQueueSizeInFlightDeliveryUnaffected(t *testing.T) {
	cap := &timedWebhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	d.Notify(OutboxEvent{Type: "transfer_posted", Seq: 1})
	// Wait until the first attempt is in flight, then revise the
	// queue size: the revision must wait for the delivery's select
	// point, never interrupt it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(cap.times()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(cap.times()) < 1 {
		t.Fatal("timed out waiting for the first delivery attempt")
	}
	type revisionResult struct {
		prev int
		err  error
	}
	resCh := make(chan revisionResult, 1)
	go func() {
		prev, err := d.SetQueueSize("risk", 2)
		resCh <- revisionResult{prev, err}
	}()
	// The in-flight delivery runs its snapshotted policy to
	// completion: 5 attempts against the 500 stub (the
	// webhookTestEndpoint default), then failed.
	rec := deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool {
		return r.Status == WebhookDeliveryFailed && r.Attempts == 5
	}, "in-flight delivery exhausting its snapshotted attempts")
	if rec.EventSeq != 1 {
		t.Fatalf("in-flight record seq = %d, want 1", rec.EventSeq)
	}
	select {
	case res := <-resCh:
		if res.err != nil || res.prev != 64 {
			t.Fatalf("SetQueueSize(2) = (%d, %v), want (64, nil)", res.prev, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetQueueSize(2) did not return after the in-flight delivery")
	}
	if len(cap.times()) != 5 {
		t.Fatalf("delivery attempts = %d, want 5 (in-flight policy kept)", len(cap.times()))
	}
	pol, ok := d.DeliveryPolicy("risk")
	if !ok || pol.MaxAttempts == nil || *pol.MaxAttempts != 5 {
		t.Fatalf("DeliveryPolicy after the revision = %+v, want MaxAttempts 5 (unchanged)", pol)
	}
}

func TestSetWebhookQueueSizeLeavesKnobsAndRingUntouched(t *testing.T) {
	d := queueSizeTestDispatcher(t, (&slowSeqWebhookCapture{}).handler(), 64)
	l := webhookTestLedger(t, d)
	beforePolicy, ok := d.DeliveryPolicy("risk")
	if !ok {
		t.Fatal("DeliveryPolicy(risk) unknown before the revision")
	}
	beforeMax := d.MaxRecords()
	delivered, failedAttempts, failures, dropped := d.Stats()
	if prev, err := l.SetWebhookQueueSize("risk", 7); err != nil || prev != 64 {
		t.Fatalf("SetWebhookQueueSize(7) = (%d, %v), want (64, nil)", prev, err)
	}
	afterPolicy, ok := d.DeliveryPolicy("risk")
	if !ok {
		t.Fatal("DeliveryPolicy(risk) unknown after the revision")
	}
	if *beforePolicy.MaxAttempts != *afterPolicy.MaxAttempts ||
		*beforePolicy.InitialBackoff != *afterPolicy.InitialBackoff ||
		*beforePolicy.MaxBackoff != *afterPolicy.MaxBackoff ||
		*beforePolicy.HTTPTimeout != *afterPolicy.HTTPTimeout {
		t.Fatalf("delivery policy moved %+v -> %+v, want unchanged", beforePolicy, afterPolicy)
	}
	if got := d.MaxRecords(); got != beforeMax {
		t.Fatalf("MaxRecords() = %d after the revision, want %d (unchanged)", got, beforeMax)
	}
	if gotDelivered, gotFailedAttempts, gotFailures, gotDropped := d.Stats(); gotDelivered != delivered || gotFailedAttempts != failedAttempts || gotFailures != failures || gotDropped != dropped {
		t.Fatalf("Stats moved (%d, %d, %d, %d) -> (%d, %d, %d, %d), want unchanged",
			delivered, failedAttempts, failures, dropped, gotDelivered, gotFailedAttempts, gotFailures, gotDropped)
	}
}

func TestSetWebhookQueueSizeRestartRestoresConstructionValueAndStopNeverDeadlocks(t *testing.T) {
	cfg := queueSizeTestEndpoint(t, "risk", mustQueueSizeTestServer(t, (&slowSeqWebhookCapture{}).handler()), mustRotatingKey(t, "primary", "s3cr3t"), 2)
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)
	if prev, err := l.SetWebhookQueueSize("risk", 9); err != nil || prev != 2 {
		t.Fatalf("SetWebhookQueueSize(9) = (%d, %v), want (2, nil)", prev, err)
	}
	// A restart restores the construction-time value: a fresh
	// dispatcher from the same config reports 2, not the revised 9.
	d2, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher (restart): %v", err)
	}
	t.Cleanup(func() { d2.Stop() })
	if got, ok := d2.QueueSize("risk"); !ok || got != 2 {
		t.Fatalf("QueueSize() after restart = (%d, %v), want (2, true)", got, ok)
	}
	// A revision after Stop takes the inline path and must return
	// instead of deadlocking on the exited worker.
	d.Stop()
	type revisionResult struct {
		prev int
		err  error
	}
	resCh := make(chan revisionResult, 1)
	go func() {
		prev, err := d.SetQueueSize("risk", 5)
		resCh <- revisionResult{prev, err}
	}()
	select {
	case res := <-resCh:
		if res.err != nil || res.prev != 9 {
			t.Fatalf("SetQueueSize(5) after Stop = (%d, %v), want (9, nil)", res.prev, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetQueueSize(5) after Stop deadlocked")
	}
	if got, ok := d.QueueSize("risk"); !ok || got != 5 {
		t.Fatalf("QueueSize() = (%d, %v) after the post-Stop revision, want (5, true)", got, ok)
	}
	// The Ledger form behaves the same after Stop.
	resCh2 := make(chan revisionResult, 1)
	go func() {
		prev, err := l.SetWebhookQueueSize("risk", 6)
		resCh2 <- revisionResult{prev, err}
	}()
	select {
	case res := <-resCh2:
		if res.err != nil || res.prev != 5 {
			t.Fatalf("Ledger.SetWebhookQueueSize(6) after Stop = (%d, %v), want (5, nil)", res.prev, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ledger.SetWebhookQueueSize(6) after Stop deadlocked")
	}
}
