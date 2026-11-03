package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Outbound webhook delivery-policy amendment (LG-304):
// SetWebhookDeliveryPolicy revises one registered endpoint's four
// delivery knobs in place — no Unregister/Register teardown, no
// delivery window — with a delivery already in flight keeping the
// policy it snapshotted at entry and only the next delivery reading
// the new values.

func ptrIntForTest(v int) *int             { return &v }
func ptrDurationForTest(v time.Duration) *time.Duration { return &v }

// deliveryAmendTestLedger builds a ledger with an audit log, an outbox,
// and an attached dispatcher, returning the ledger, the log, and the
// audit dir (for audit-trail assertions). The caller stops the
// dispatcher via cleanup.
func deliveryAmendTestLedger(t *testing.T, d *WebhookDispatcher) (*Ledger, *AuditLog, string) {
	t.Helper()
	base := t.TempDir()
	auditDir := filepath.Join(base, "audit")
	outboxDir := filepath.Join(base, "outbox")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatalf("MkdirAll audit: %v", err)
	}
	if err := os.MkdirAll(outboxDir, 0o755); err != nil {
		t.Fatalf("MkdirAll outbox: %v", err)
	}
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	ob, err := NewOutbox(outboxDir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	return New(WithAuditLog(al), WithOutbox(ob), WithWebhooks(d)), al, auditDir
}

// timedWebhookCapture is an httptest stub that records the arrival
// time of every delivery attempt, so a test can measure the retry
// interval a policy actually produced.
type timedWebhookCapture struct {
	mu        sync.Mutex
	attemptAt []time.Time
	behavior  func(attempt int) int
	slow      time.Duration
}

func (c *timedWebhookCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.slow > 0 {
			time.Sleep(c.slow)
		}
		c.mu.Lock()
		n := len(c.attemptAt)
		c.attemptAt = append(c.attemptAt, time.Now())
		c.mu.Unlock()
		status := 200
		if c.behavior != nil {
			status = c.behavior(n)
		}
		w.WriteHeader(status)
	})
}

func (c *timedWebhookCapture) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, len(c.attemptAt))
	copy(out, c.attemptAt)
	return out
}

// deliveryRecordFor polls the dispatcher's records for one endpoint
// until cond holds on the latest record, or fails the test.
func deliveryRecordFor(t *testing.T, d *WebhookDispatcher, endpoint string, cond func(DeliveryRecord) bool, msg string) DeliveryRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range d.Deliveries(WebhookDeliveryQuery{Endpoint: endpoint, Limit: 10}) {
			if cond(rec) {
				return rec
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for delivery record: %s", msg)
	return DeliveryRecord{}
}

// auditDetailFloat reads one numeric audit detail as float64 (audit
// details round-trip through JSON, so integers come back float64).
func auditDetailFloat(t *testing.T, ev AuditEvent, key string) float64 {
	t.Helper()
	v, ok := ev.Details[key]
	if !ok {
		t.Fatalf("audit event %q has no detail %q (details: %v)", ev.Op, key, ev.Details)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("audit detail %q = %T %v, want a JSON number", key, v, v)
	}
	return f
}

func TestSetWebhookDeliveryPolicyTightensMaxAttempts(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	// The pre-revision budget: 5 attempts against a permanent 500.
	mustPostWebhookEntry(t, l, "amend-max-1")
	rec := deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Status == WebhookDeliveryFailed }, "first delivery to exhaust")
	if rec.Attempts != 5 {
		t.Fatalf("pre-revision attempts = %d, want 5", rec.Attempts)
	}

	// Tighten to 2: the next delivery exhausts two attempts sooner.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2)}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy: %v", err)
	}
	if p, ok := l.WebhookDeliveryPolicy("risk"); !ok || p.MaxAttempts == nil || *p.MaxAttempts != 2 {
		t.Fatalf("WebhookDeliveryPolicy() = %+v, %v; want MaxAttempts 2", p, ok)
	}
	mustPostWebhookEntry(t, l, "amend-max-2")
	rec = deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool {
		return r.Status == WebhookDeliveryFailed && r.EventSeq != 0 && r.Attempts == 2
	}, "second delivery to exhaust at 2 attempts")
	if rec.Attempts != 2 {
		t.Fatalf("post-revision attempts = %d, want 2", rec.Attempts)
	}
}

func TestSetWebhookDeliveryPolicyLengthensBackoff(t *testing.T) {
	cap := &timedWebhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	// Lengthen the backoff past the max too, or the merged policy
	// would fail the InitialBackoff <= MaxBackoff gate.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{
		InitialBackoff: ptrDurationForTest(200 * time.Millisecond),
		MaxBackoff:     ptrDurationForTest(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy: %v", err)
	}
	mustPostWebhookEntry(t, l, "amend-backoff-1")
	deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Status == WebhookDeliveryFailed }, "delivery to exhaust")
	times := cap.times()
	if len(times) != 5 {
		t.Fatalf("attempts = %d, want 5 (the untouched MaxAttempts budget)", len(times))
	}
	// The first retry waited out the lengthened backoff: ~200ms plus
	// up to +25% jitter, not the registered 5ms.
	if gap := times[1].Sub(times[0]); gap < 150*time.Millisecond {
		t.Fatalf("retry gap = %s, want >= 150ms under the lengthened backoff", gap)
	}
}

func TestSetWebhookDeliveryPolicyShortensHTTPTimeout(t *testing.T) {
	cap := &timedWebhookCapture{slow: 300 * time.Millisecond}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	// A 300ms-slow endpoint still delivers under the 2s timeout.
	mustPostWebhookEntry(t, l, "amend-timeout-1")
	rec := deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Status == WebhookDeliveryDelivered }, "slow delivery to succeed")
	if rec.Attempts != 1 {
		t.Fatalf("pre-revision attempts = %d, want 1", rec.Attempts)
	}

	// Shorten the timeout below the endpoint's latency and cap the
	// budget at one attempt: the next try times out at once.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{
		MaxAttempts: ptrIntForTest(1),
		HTTPTimeout: ptrDurationForTest(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy: %v", err)
	}
	mustPostWebhookEntry(t, l, "amend-timeout-2")
	rec = deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool {
		return r.Status == WebhookDeliveryFailed && r.Attempts == 1 && strings.Contains(r.LastError, "Timeout")
	}, "timed-out delivery to fail after one attempt")
	if rec.Attempts != 1 || rec.Status != WebhookDeliveryFailed {
		t.Fatalf("post-revision record = %+v, want 1 attempt failed", rec)
	}
	if !strings.Contains(rec.LastError, "Timeout") {
		t.Fatalf("LastError = %q, want a client-timeout error", rec.LastError)
	}
}

func TestSetWebhookDeliveryPolicyUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	err = l.SetWebhookDeliveryPolicy("no-such-endpoint", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2)})
	if !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Fatalf("unknown endpoint err = %v, want ErrWebhookEndpointNotFound", err)
	}
	if _, err := d.SetDeliveryPolicy("no-such-endpoint", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2)}); !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Fatalf("dispatcher unknown endpoint err = %v, want ErrWebhookEndpointNotFound", err)
	}
	if _, ok := l.WebhookDeliveryPolicy("no-such-endpoint"); ok {
		t.Fatalf("WebhookDeliveryPolicy(unknown) returned ok")
	}
	// The registered endpoint is untouched by the failed call.
	if p, ok := l.WebhookDeliveryPolicy("risk"); !ok || *p.MaxAttempts != 5 {
		t.Fatalf("WebhookDeliveryPolicy(risk) = %+v, %v; want MaxAttempts 5", p, ok)
	}
}

func TestSetWebhookDeliveryPolicyWithoutDispatcher(t *testing.T) {
	l := New()
	err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2)})
	if !errors.Is(err, ErrWebhooksNotConfigured) {
		t.Fatalf("no-dispatcher err = %v, want ErrWebhooksNotConfigured", err)
	}
	if _, ok := l.WebhookDeliveryPolicy("risk"); ok {
		t.Fatalf("WebhookDeliveryPolicy without a dispatcher returned ok")
	}
}

func TestSetWebhookDeliveryPolicyValidationRejectsAndKeepsLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	bad := []WebhookDeliveryPolicy{
		{MaxAttempts: ptrIntForTest(0)},
		{MaxAttempts: ptrIntForTest(-1)},
		{InitialBackoff: ptrDurationForTest(-time.Second)},
		{HTTPTimeout: ptrDurationForTest(0)},
		{MaxBackoff: ptrDurationForTest(-time.Second)},
		// InitialBackoff above MaxBackoff: the construction-time
		// clamp must not silently normalize a runtime revision.
		{InitialBackoff: ptrDurationForTest(5 * time.Second), MaxBackoff: ptrDurationForTest(time.Second)},
	}
	for i, p := range bad {
		if err := l.SetWebhookDeliveryPolicy("risk", p); !errors.Is(err, ErrInvalidWebhookDeliveryPolicy) {
			t.Fatalf("bad policy %d err = %v, want ErrInvalidWebhookDeliveryPolicy", i, err)
		}
	}
	// Every rejection left the live policy exactly as registered.
	if p, ok := l.WebhookDeliveryPolicy("risk"); !ok ||
		*p.MaxAttempts != 5 || *p.InitialBackoff != 5*time.Millisecond ||
		*p.MaxBackoff != 20*time.Millisecond || *p.HTTPTimeout != 2*time.Second {
		t.Fatalf("live policy moved after rejections: %+v, %v", p, ok)
	}
	// And no audit event was written for any of them.
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_delivery_policy_retuned"]; len(events) != 0 {
		t.Fatalf("rejected revisions audited %d events, want 0", len(events))
	}
}

func TestSetWebhookDeliveryPolicyNoOpWritesNoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	// All-nil keeps every live value; a same-value revision is the
	// same no-op. Neither may spam the audit trail.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{}); err != nil {
		t.Fatalf("all-nil revision: %v", err)
	}
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{
		MaxAttempts:    ptrIntForTest(5),
		InitialBackoff: ptrDurationForTest(5 * time.Millisecond),
		MaxBackoff:     ptrDurationForTest(20 * time.Millisecond),
		HTTPTimeout:    ptrDurationForTest(2 * time.Second),
	}); err != nil {
		t.Fatalf("same-value revision: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_delivery_policy_retuned"]; len(events) != 0 {
		t.Fatalf("no-op revisions audited %d events, want 0", len(events))
	}
}

func TestSetWebhookDeliveryPolicyInFlightKeepsOldPolicy(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	cfg.MaxAttempts = 3
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "amend-inflight-1")
	// Wait until the first attempt has landed, then revise mid-flight:
	// the delivery must finish under the policy it started with.
	deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Attempts >= 1 }, "first attempt to land")
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(1)}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy mid-flight: %v", err)
	}
	rec := deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Status == WebhookDeliveryFailed }, "in-flight delivery to exhaust")
	if rec.Attempts != 3 {
		t.Fatalf("in-flight delivery attempts = %d, want 3 (the pre-revision budget)", rec.Attempts)
	}

	// The next delivery reads the new budget: one attempt, then done.
	mustPostWebhookEntry(t, l, "amend-inflight-2")
	rec = deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool {
		return r.Status == WebhookDeliveryFailed && r.Attempts == 1
	}, "next delivery to exhaust at 1 attempt")
	if rec.Attempts != 1 {
		t.Fatalf("next delivery attempts = %d, want 1", rec.Attempts)
	}
}

func TestSetWebhookDeliveryPolicyQueuedEventsKeepOrder(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "amend-order-1")
	// Revise while the first delivery is in flight or queued: the
	// queue, the order, and the records must all survive it.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2)}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy: %v", err)
	}
	mustPostWebhookEntry(t, l, "amend-order-2")
	waitForWebhook(t, 10*time.Second, func() bool {
		delivered, _, _, _ := d.Stats()
		return delivered == 2
	}, "both deliveries to land")
	attempts := cap.attempts()
	if len(attempts) != 2 {
		t.Fatalf("downstream attempts = %d, want 2", len(attempts))
	}
	if attempts[0].seq >= attempts[1].seq {
		t.Fatalf("delivery order broken: seqs %q then %q", attempts[0].seq, attempts[1].seq)
	}
	for _, rec := range d.Deliveries(WebhookDeliveryQuery{Endpoint: "risk"}) {
		if rec.Status != WebhookDeliveryDelivered || rec.Attempts != 1 {
			t.Fatalf("record = %+v, want delivered in 1 attempt", rec)
		}
	}
	if delivered, failedAttempts, failures, dropped := d.Stats(); delivered != 2 || failedAttempts != 0 || failures != 0 || dropped != 0 {
		t.Fatalf("Stats() = %d/%d/%d/%d, want 2/0/0/0", delivered, failedAttempts, failures, dropped)
	}
}

func TestSetWebhookDeliveryPolicyCtxActorAttribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := l.SetWebhookDeliveryPolicyCtx(ctx, "risk", WebhookDeliveryPolicy{
		MaxAttempts: ptrIntForTest(3),
		HTTPTimeout: ptrDurationForTest(5 * time.Second),
	}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicyCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if err := l.SetWebhookDeliveryPolicy("risk", WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(4)}); err != nil {
		t.Fatalf("SetWebhookDeliveryPolicy: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_delivery_policy_retuned"]
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	first := events[0]
	if first.Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", first.Actor)
	}
	if first.Details["endpoint"] != "risk" {
		t.Fatalf("first event endpoint = %v, want risk", first.Details["endpoint"])
	}
	if got := auditDetailFloat(t, first, "previous_max_attempts"); got != 5 {
		t.Fatalf("previous_max_attempts = %v, want 5", got)
	}
	if got := auditDetailFloat(t, first, "current_max_attempts"); got != 3 {
		t.Fatalf("current_max_attempts = %v, want 3", got)
	}
	if got := auditDetailFloat(t, first, "previous_http_timeout_ns"); got != float64(2*time.Second.Nanoseconds()) {
		t.Fatalf("previous_http_timeout_ns = %v, want %v", got, 2*time.Second.Nanoseconds())
	}
	if got := auditDetailFloat(t, first, "current_http_timeout_ns"); got != float64(5*time.Second.Nanoseconds()) {
		t.Fatalf("current_http_timeout_ns = %v, want %v", got, 5*time.Second.Nanoseconds())
	}
	// Only the changed fields are listed: the untouched backoffs are
	// absent from the first event's details.
	if _, ok := first.Details["previous_initial_backoff_ns"]; ok {
		t.Fatalf("unchanged initial_backoff must not appear in the details: %v", first.Details)
	}
	if first.VersionBefore != first.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", first.VersionBefore, first.VersionAfter)
	}
	if events[1].Actor != "SetWebhookDeliveryPolicy" {
		t.Fatalf("second event Actor = %q, want SetWebhookDeliveryPolicy", events[1].Actor)
	}
}

func TestSetWebhookDeliveryPolicyConcurrentRevisionNoRace(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	// Revisions race the delivery worker: every revision swaps the
	// policy under d.mu while deliver snapshots it under d.mu.RLock.
	// -race must stay silent, and deliveries must keep completing.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := WebhookDeliveryPolicy{MaxAttempts: ptrIntForTest(2 + (i+g)%2)}
				if err := l.SetWebhookDeliveryPolicy("risk", p); err != nil {
					t.Errorf("concurrent revision: %v", err)
					return
				}
				// Yield between revisions so the delivery worker
				// can take d.mu for its snapshots and records.
				time.Sleep(time.Millisecond)
			}
		}(g)
	}
	for i := 0; i < 5; i++ {
		mustPostWebhookEntry(t, l, "amend-race-"+string(rune('a'+i)))
	}
	waitForWebhook(t, 10*time.Second, func() bool {
		_, _, failures, _ := d.Stats()
		return failures == 5
	}, "all five deliveries to exhaust")
	close(stop)
	wg.Wait()
	if p, ok := l.WebhookDeliveryPolicy("risk"); !ok || (*p.MaxAttempts != 2 && *p.MaxAttempts != 3) {
		t.Fatalf("final policy = %+v, %v; want MaxAttempts 2 or 3", p, ok)
	}
}
