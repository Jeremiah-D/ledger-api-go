package ledger

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitForWebhook polls cond until it is true or the timeout lapses.
func waitForWebhook(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for webhook condition: %s", msg)
}

// webhookCapture is an httptest stub that records every delivery
// attempt. behavior decides the response status per attempt index.
type webhookCapture struct {
	mu       sync.Mutex
	bodies   [][]byte
	sigs     []string
	keyIDs   []string
	types    []string
	seqs     []string
	behavior func(attempt int) int
}

type webhookAttempt struct {
	body  []byte
	sig   string
	keyID string
	typ   string
	seq   string
}

func (c *webhookCapture) attempts() []webhookAttempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]webhookAttempt, len(c.bodies))
	for i := range c.bodies {
		out[i] = webhookAttempt{
			body:  c.bodies[i],
			sig:   c.sigs[i],
			keyID: c.keyIDs[i],
			typ:   c.types[i],
			seq:   c.seqs[i],
		}
	}
	return out
}

func (c *webhookCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		_ = r.Body.Close()
		c.mu.Lock()
		n := len(c.bodies)
		c.bodies = append(c.bodies, body)
		c.sigs = append(c.sigs, r.Header.Get("X-Ledger-Signature"))
		c.keyIDs = append(c.keyIDs, r.Header.Get("X-Ledger-Key-Id"))
		c.types = append(c.types, r.Header.Get("X-Ledger-Event-Type"))
		c.seqs = append(c.seqs, r.Header.Get("X-Ledger-Event-Seq"))
		c.mu.Unlock()
		status := 200
		if c.behavior != nil {
			status = c.behavior(n)
		}
		w.WriteHeader(status)
	})
}

// mustRotatingKey builds a single-key ring for tests.
func mustRotatingKey(t *testing.T, id, secret string) *RotatingKey {
	t.Helper()
	k, err := NewRotatingKey(id, []byte(secret), time.Hour)
	if err != nil {
		t.Fatalf("NewRotatingKey: %v", err)
	}
	return k
}

// webhookTestEndpoint builds a fast endpoint config against the stub:
// 5ms initial backoff, 20ms max, 2s HTTP timeout.
func webhookTestEndpoint(t *testing.T, name, url string, keys *RotatingKey) WebhookEndpointConfig {
	t.Helper()
	return WebhookEndpointConfig{
		Name:           name,
		URL:            url,
		Keys:           keys,
		Enabled:        true,
		MaxAttempts:    5,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		HTTPTimeout:    2 * time.Second,
		QueueSize:      64,
	}
}

// webhookTestLedger builds a ledger with an outbox and an attached
// dispatcher. The caller stops the dispatcher via cleanup.
func webhookTestLedger(t *testing.T, d *WebhookDispatcher) *Ledger {
	t.Helper()
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	return New(WithOutbox(ob), WithWebhooks(d))
}

// mustPostWebhookEntry posts one journal entry with a unique ID.
func mustPostWebhookEntry(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 100, Currency: "USD",
	}); err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
}

func TestRotatingKeySignVerify(t *testing.T) {
	k := mustRotatingKey(t, "k1", "test-secret-1")
	body := []byte(`{"seq":1,"type":"transfer_posted"}`)
	sig, keyID, err := k.Sign(body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if keyID != "k1" {
		t.Errorf("keyID = %q, want k1", keyID)
	}
	if !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("sig = %q, want sha256= prefix", sig)
	}
	if !k.Verify(body, sig) {
		t.Errorf("Verify(valid) = false, want true")
	}
	if !k.VerifyWithKey(body, "k1", sig) {
		t.Errorf("VerifyWithKey(k1, valid) = false, want true")
	}
	if k.Verify(append(body, 'x'), sig) {
		t.Errorf("Verify(tampered body) = true, want false")
	}
	if k.Verify(body, "sha256=deadbeef") {
		t.Errorf("Verify(bogus sig) = true, want false")
	}
	if k.Verify(body, "not-hex!!") {
		t.Errorf("Verify(malformed sig) = true, want false")
	}
	if k.VerifyWithKey(body, "nope", sig) {
		t.Errorf("VerifyWithKey(unknown id) = true, want false")
	}
}

func TestRotatingKeyRotateAndGrace(t *testing.T) {
	k := mustRotatingKey(t, "k1", "test-secret-1")
	body := []byte(`{"seq":2}`)

	sigOld, _, err := k.Sign(body)
	if err != nil {
		t.Fatalf("Sign before rotate: %v", err)
	}
	if err := k.Rotate("k2", []byte("test-secret-2")); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got := k.PrimaryID(); got != "k2" {
		t.Errorf("PrimaryID = %q, want k2", got)
	}
	sigNew, keyID, err := k.Sign(body)
	if err != nil {
		t.Fatalf("Sign after rotate: %v", err)
	}
	if keyID != "k2" {
		t.Errorf("keyID after rotate = %q, want k2", keyID)
	}
	if sigNew == sigOld {
		t.Errorf("signature did not change after rotation")
	}
	// The retired key still verifies inside the grace period, and the
	// new key verifies too.
	if !k.Verify(body, sigOld) {
		t.Errorf("Verify(old sig within grace) = false, want true")
	}
	if !k.Verify(body, sigNew) {
		t.Errorf("Verify(new sig) = false, want true")
	}
	if !k.VerifyWithKey(body, "k1", sigOld) {
		t.Errorf("VerifyWithKey(k1, within grace) = false, want true")
	}
	// Push the old key past its grace period: verification must fail
	// and the key must be purged.
	k.mu.Lock()
	k.keys["k1"].RetiredAt = time.Now().Add(-2 * time.Hour)
	k.mu.Unlock()
	if k.Verify(body, sigOld) {
		t.Errorf("Verify(old sig past grace) = true, want false")
	}
	if k.VerifyWithKey(body, "k1", sigOld) {
		t.Errorf("VerifyWithKey(k1, past grace) = true, want false")
	}
	ids := map[string]bool{}
	for _, key := range k.Keys() {
		ids[key.ID] = true
	}
	if ids["k1"] {
		t.Errorf("expired key k1 still in ring after purge")
	}
	if !ids["k2"] {
		t.Errorf("primary key k2 missing from ring")
	}
	// New key keeps working after the purge.
	if !k.Verify(body, sigNew) {
		t.Errorf("Verify(new sig after purge) = false, want true")
	}
}

func TestRotatingKeyValidation(t *testing.T) {
	if _, err := NewRotatingKey("", []byte("s"), 0); err == nil {
		t.Errorf("empty id: want error")
	}
	if _, err := NewRotatingKey("k", nil, 0); err == nil {
		t.Errorf("empty secret: want error")
	}
	if _, err := NewRotatingKey("k", []byte("s"), -time.Second); err == nil {
		t.Errorf("negative grace: want error")
	}
	k := mustRotatingKey(t, "k1", "test-secret-1")
	if err := k.Rotate("k1", []byte("other")); err == nil {
		t.Errorf("duplicate id rotate: want error")
	}
	if err := k.Rotate("", []byte("other")); err == nil {
		t.Errorf("empty id rotate: want error")
	}
	if err := k.Rotate("k2", nil); err == nil {
		t.Errorf("empty secret rotate: want error")
	}
}

func TestParseWebhookEndpoints(t *testing.T) {
	raw := `[
		{"name":"risk","url":"https://risk.example.com/hook","events":["transfer_posted","review_rejected"],"secret":"s3cr3t","max_attempts":3,"initial_backoff":"100ms"},
		{"name":"siem","url":"https://siem.example.com/hook","key_id":"a","secret":"aaa","enabled":false},
		{"name":"multi","url":"https://m.example.com/hook","keys":[
			{"id":"k2","secret":"new-secret"},
			{"id":"k1","secret":"old-secret","retired_at":"2020-01-01T00:00:00Z"}
		]}
	]`
	cfgs, err := ParseWebhookEndpoints(raw)
	if err != nil {
		t.Fatalf("ParseWebhookEndpoints: %v", err)
	}
	if len(cfgs) != 3 {
		t.Fatalf("parsed %d endpoints, want 3", len(cfgs))
	}
	if cfgs[0].Name != "risk" || !cfgs[0].Enabled {
		t.Errorf("endpoint 0 = %+v, want risk/enabled", cfgs[0])
	}
	if len(cfgs[0].Events) != 2 || cfgs[0].MaxAttempts != 3 {
		t.Errorf("endpoint 0 fields = %+v, want 2 events / 3 attempts", cfgs[0])
	}
	if cfgs[0].InitialBackoff != 100*time.Millisecond {
		t.Errorf("endpoint 0 backoff = %v, want 100ms", cfgs[0].InitialBackoff)
	}
	if cfgs[1].Enabled {
		t.Errorf("endpoint 1 enabled = true, want false")
	}
	if got := cfgs[1].Keys.PrimaryID(); got != "a" {
		t.Errorf("endpoint 1 key id = %q, want a", got)
	}
	// The multi-key endpoint: k2 is primary; k1 retired in 2020 is
	// long past the default grace and must be purged.
	keys := cfgs[2].Keys.Keys()
	if len(keys) != 1 || keys[0].ID != "k2" {
		t.Errorf("multi-key ring = %v, want [k2] only", keys)
	}

	bad := []string{
		`not json`,
		`[{"name":"x","url":"https://x.example.com/hook"}]`,                      // no keys
		`[{"name":"x","url":"ftp://x.example.com/hook","secret":"s"}]`,           // bad scheme
		`[{"name":"x","url":"https://x.example.com/hook","secret":"s"},` +
			`{"name":"x","url":"https://y.example.com/hook","secret":"s"}]`, // dup name
		`[{"name":"x","url":"https://x.example.com/hook","secret":"s","keys":[{"id":"k","secret":"s"}]}]`, // both
		`[{"name":"","url":"https://x.example.com/hook","secret":"s"}]`,        // empty name
		`[{"name":"x","url":"https://x.example.com/hook","secret":"s","initial_backoff":"bogus"}]`,
	}
	for i, raw := range bad {
		if _, err := ParseWebhookEndpoints(raw); err == nil {
			t.Errorf("bad config %d: want error, got nil", i)
		}
	}
}

func TestWebhookDeliverySigned(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, keys))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "wh-e0")

	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 1
	}, "one delivery")

	got := cap.attempts()
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want 1", len(got))
	}
	a := got[0]
	if a.typ != OutboxEntryPosted {
		t.Errorf("event type = %q, want %q", a.typ, OutboxEntryPosted)
	}
	if a.keyID != "k1" {
		t.Errorf("key id = %q, want k1", a.keyID)
	}
	if a.seq == "" || a.seq == "0" {
		t.Errorf("event seq header = %q, want nonzero", a.seq)
	}
	// The signature must verify against the exact body, and the body's
	// seq must match the header (the outbox seq downstream dedupes on).
	if !keys.Verify(a.body, a.sig) {
		t.Errorf("signature does not verify against the delivered body")
	}
	var ev OutboxEvent
	if err := json.Unmarshal(a.body, &ev); err != nil {
		t.Fatalf("body is not an OutboxEvent: %v", err)
	}
	if ev.Type != OutboxEntryPosted || ev.Account != "cash" {
		t.Errorf("payload = %+v, want entry_posted for cash", ev)
	}
	if a.seq != fmt.Sprintf("%d", ev.Seq) {
		t.Errorf("seq header %q != payload seq %d", a.seq, ev.Seq)
	}
	// The delivery record is queryable.
	recs := d.Deliveries(WebhookDeliveryQuery{Limit: 10})
	if len(recs) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Status != WebhookDeliveryDelivered || r.Attempts != 1 || r.Endpoint != "risk" {
		t.Errorf("record = %+v, want delivered/1/risk", r)
	}
	if r.EventSeq != ev.Seq {
		t.Errorf("record seq = %d, want %d", r.EventSeq, ev.Seq)
	}
}

func TestWebhookEventFilter(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "filtered", srv.URL, keys)
	cfg.Events = []string{OutboxTransferPosted}
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	// An entry_posted does not match the filter: nothing is delivered.
	mustPostWebhookEntry(t, l, "wh-e1")
	time.Sleep(150 * time.Millisecond)
	if dl, _, _, _ := d.Stats(); dl != 0 {
		t.Fatalf("delivered = %d after filtered-out event, want 0", dl)
	}
	if len(cap.attempts()) != 0 {
		t.Fatalf("stub got %d requests, want 0", len(cap.attempts()))
	}

	// A transfer_posted matches: exactly one delivery (the envelope;
	// the legs are entry_posted and stay filtered out).
	if _, err := l.PostTransfer(Transfer{
		ID: "wh-filter-1", From: "cash", To: "equity",
		AmountCents: 200, Currency: "USD",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 1
	}, "filtered transfer_posted delivery")
	for _, a := range cap.attempts() {
		if a.typ != OutboxTransferPosted {
			t.Errorf("delivered type = %q, want only transfer_posted", a.typ)
		}
	}
}

func TestWebhookRetryThenDelivered(t *testing.T) {
	var n atomic.Int32
	cap := &webhookCapture{behavior: func(attempt int) int {
		n.Add(1)
		if attempt < 2 {
			return 500
		}
		return 200
	}}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "flaky", srv.URL, keys))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "wh-e2")
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 1
	}, "delivery after retries")

	if got := n.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (2x500 then 200)", got)
	}
	recs := d.Deliveries(WebhookDeliveryQuery{Status: WebhookDeliveryDelivered, Limit: 10})
	if len(recs) != 1 || recs[0].Attempts != 3 {
		t.Errorf("delivered records = %+v, want one with 3 attempts", recs)
	}
	if _, _, failures, _ := d.Stats(); failures != 0 {
		t.Errorf("failures = %d, want 0 (recovered)", failures)
	}
}

func TestWebhookExhaustedCountsFailure(t *testing.T) {
	cap := &webhookCapture{behavior: func(attempt int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "down", srv.URL, keys)
	cfg.MaxAttempts = 3
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "wh-e3")
	// The posting itself succeeded: delivery failure never blocks the
	// ledger. The failure surfaces asynchronously in the counters.
	waitForWebhook(t, 5*time.Second, func() bool {
		_, _, failures, _ := d.Stats()
		return failures == 1
	}, "exhausted delivery counted")

	if dl, fa, failures, _ := d.Stats(); dl != 0 || fa != 3 || failures != 1 {
		t.Errorf("stats = delivered %d failedAttempts %d failures %d, want 0/3/1", dl, fa, failures)
	}
	recs := d.Deliveries(WebhookDeliveryQuery{Status: WebhookDeliveryFailed, Limit: 10})
	if len(recs) != 1 {
		t.Fatalf("failed records = %d, want 1", len(recs))
	}
	if recs[0].Attempts != 3 || recs[0].LastError != "http 500" {
		t.Errorf("failed record = %+v, want 3 attempts / http 500", recs[0])
	}
}

func TestWebhookClientErrorIsTerminal(t *testing.T) {
	cap := &webhookCapture{behavior: func(attempt int) int { return 400 }}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "rejecting", srv.URL, keys)
	cfg.MaxAttempts = 5
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "wh-e4")
	waitForWebhook(t, 5*time.Second, func() bool {
		_, _, failures, _ := d.Stats()
		return failures == 1
	}, "terminal 400 counted")

	// A 400 is the receiver rejecting the payload: exactly one
	// attempt, no retries.
	if got := len(cap.attempts()); got != 1 {
		t.Errorf("attempts = %d, want 1 (4xx is terminal)", got)
	}
	if _, fa, _, _ := d.Stats(); fa != 1 {
		t.Errorf("failedAttempts = %d, want 1", fa)
	}
}

func TestWebhookQueueFullDropsWithoutBlocking(t *testing.T) {
	unblock := make(chan struct{})
	var inFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		<-unblock
		w.WriteHeader(200)
	}))
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "slow", srv.URL, keys)
	cfg.QueueSize = 2
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()

	// Occupy the worker with a blocked first delivery.
	d.Notify(OutboxEvent{Seq: 1, Type: OutboxEntryPosted})
	waitForWebhook(t, 5*time.Second, func() bool { return inFlight.Load() == 1 },
		"worker stuck in first delivery")

	// The queue holds 2 more; the 4th notify must drop, not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Notify(OutboxEvent{Seq: 2, Type: OutboxEntryPosted})
		d.Notify(OutboxEvent{Seq: 3, Type: OutboxEntryPosted})
		d.Notify(OutboxEvent{Seq: 4, Type: OutboxEntryPosted})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Notify blocked on a full queue: the posting path must never block")
	}
	if _, _, _, dropped := d.Stats(); dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	close(unblock)
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 3
	}, "queued events delivered after unblock")
}

func TestWebhookPostingNeverBlockedByDownEndpoint(t *testing.T) {
	// Nothing listens here: every delivery attempt fails fast on
	// connection refused.
	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "dead", "http://127.0.0.1:1/hook", keys)
	cfg.MaxAttempts = 10
	cfg.InitialBackoff = time.Second
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	start := time.Now()
	mustPostWebhookEntry(t, l, "wh-e5")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Post took %v with a dead endpoint: the posting path must not wait for webhooks", elapsed)
	}
	// The failure is counted asynchronously, without touching Post's
	// result.
	waitForWebhook(t, 10*time.Second, func() bool {
		_, _, failures, _ := d.Stats()
		return failures == 1
	}, "dead-endpoint failure counted")
}

func TestWebhookDeliveriesQuery(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	d, err := NewWebhookDispatcher(
		webhookTestEndpoint(t, "a", srv.URL, keys),
		webhookTestEndpoint(t, "b", srv.URL, keys),
	)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "wh-e6")
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 2
	}, "both endpoints delivered")

	// Latest-first across endpoints.
	all := d.Deliveries(WebhookDeliveryQuery{Limit: 10})
	if len(all) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(all))
	}
	byEndpoint := d.Deliveries(WebhookDeliveryQuery{Endpoint: "a", Limit: 10})
	if len(byEndpoint) != 1 || byEndpoint[0].Endpoint != "a" {
		t.Errorf("endpoint filter = %+v, want one record for a", byEndpoint)
	}
	delivered := d.Deliveries(WebhookDeliveryQuery{Status: WebhookDeliveryDelivered, Limit: 10})
	if len(delivered) != 2 {
		t.Errorf("status=delivered = %d, want 2", len(delivered))
	}
	failed := d.Deliveries(WebhookDeliveryQuery{Status: WebhookDeliveryFailed, Limit: 10})
	if len(failed) != 0 {
		t.Errorf("status=failed = %d, want 0", len(failed))
	}
	limited := d.Deliveries(WebhookDeliveryQuery{Limit: 1})
	if len(limited) != 1 {
		t.Errorf("limit=1 = %d, want 1", len(limited))
	}
	unknown := d.Deliveries(WebhookDeliveryQuery{Endpoint: "nope", Limit: 10})
	if len(unknown) != 0 {
		t.Errorf("unknown endpoint filter = %d, want 0", len(unknown))
	}
}

func TestWebhookRegistry(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	d, err := NewWebhookDispatcher()
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "one", srv.URL, keys)
	if err := d.Register(cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := d.Register(cfg); err == nil {
		t.Errorf("duplicate Register: want error")
	}
	bad := webhookTestEndpoint(t, "bad", "://bogus", keys)
	if err := d.Register(bad); err == nil {
		t.Errorf("bad URL Register: want error")
	}
	nokey := webhookTestEndpoint(t, "nokey", srv.URL, keys)
	nokey.Keys = nil
	if err := d.Register(nokey); err == nil {
		t.Errorf("keyless Register: want error")
	}
	if names := d.EndpointNames(); len(names) != 1 || names[0] != "one" {
		t.Errorf("EndpointNames = %v, want [one]", names)
	}

	// Disabled endpoints receive nothing.
	if !d.SetEnabled("one", false) {
		t.Fatalf("SetEnabled: unknown endpoint")
	}
	if d.SetEnabled("ghost", true) {
		t.Errorf("SetEnabled(ghost): want false")
	}
	d.Notify(OutboxEvent{Seq: 9, Type: OutboxEntryPosted})
	time.Sleep(150 * time.Millisecond)
	if dl, _, _, _ := d.Stats(); dl != 0 {
		t.Errorf("delivered = %d while disabled, want 0", dl)
	}
	if !d.SetEnabled("one", true) {
		t.Fatalf("SetEnabled(true): unknown endpoint")
	}
	d.Notify(OutboxEvent{Seq: 10, Type: OutboxEntryPosted})
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 1
	}, "delivery after re-enable")

	// Key rotation on a live endpoint: new deliveries carry the new
	// key id and verify under it.
	if err := d.RotateKey("one", "k2", []byte("test-secret-2")); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	d.Notify(OutboxEvent{Seq: 11, Type: OutboxEntryPosted})
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 2
	}, "delivery after rotation")
	got := cap.attempts()
	last := got[len(got)-1]
	if last.keyID != "k2" {
		t.Errorf("key id after rotation = %q, want k2", last.keyID)
	}
	if !keys.Verify(last.body, last.sig) {
		t.Errorf("rotated signature does not verify")
	}
	if err := d.RotateKey("ghost", "k9", []byte("s")); err == nil {
		t.Errorf("RotateKey(ghost): want error")
	}

	if !d.Unregister("one") {
		t.Errorf("Unregister(one): want true")
	}
	if d.Unregister("one") {
		t.Errorf("Unregister(one) twice: want false")
	}
	if names := d.EndpointNames(); len(names) != 0 {
		t.Errorf("EndpointNames after unregister = %v, want []", names)
	}
}

func TestPain001StatusChangedOutboxEvents(t *testing.T) {
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	l := New(WithOutbox(ob))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})

	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
	}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}

	events, _, corrupt, err := ReadOutboxEvents(dir, OutboxQuery{
		Types: []string{OutboxPain001StatusChanged}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ReadOutboxEvents: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	// registered->exported (instruction), exported->accepted
	// (per-transfer), exported->accepted (instruction rollup).
	if len(events) != 3 {
		t.Fatalf("pain001_status_changed events = %d, want 3", len(events))
	}
	moves := [][3]string{}
	for _, ev := range events {
		from, _ := ev.Details["from"].(string)
		to, _ := ev.Details["to"].(string)
		txID, _ := ev.Details["tx_id"].(string)
		moves = append(moves, [3]string{from, to, txID})
		if ev.Details["msg_id"] != msgID {
			t.Errorf("event msg_id = %v, want %q", ev.Details["msg_id"], msgID)
		}
	}
	want := [][3]string{
		{Pain001StateRegistered, Pain001StateExported, ""},
		{Pain001StateExported, Pain001StateAccepted, "ps1"},
		{Pain001StateExported, Pain001StateAccepted, ""},
	}
	for i := range want {
		if moves[i] != want[i] {
			t.Errorf("move %d = %v, want %v", i, moves[i], want[i])
		}
	}
	// Seq order is commit order.
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("seq not monotonic: %+v", events)
			break
		}
	}
}

func TestWebhookForwardsPain001StatusChanged(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "test-secret-1")
	cfg := webhookTestEndpoint(t, "bank", srv.URL, keys)
	cfg.Events = []string{OutboxPain001StatusChanged}
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()

	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	l := New(WithOutbox(ob), WithWebhooks(d))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
	}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}

	// Only the 3 pain001_status_changed events match the filter (the
	// transfer's entry_posted/transfer_posted legs do not).
	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 3
	}, "3 pain001 webhooks")
	for _, a := range cap.attempts() {
		if a.typ != OutboxPain001StatusChanged {
			t.Errorf("delivered type = %q, want only pain001_status_changed", a.typ)
		}
		if !keys.Verify(a.body, a.sig) {
			t.Errorf("signature does not verify for pain001 event")
		}
	}
}
