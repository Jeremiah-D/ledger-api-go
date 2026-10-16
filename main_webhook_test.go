package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// webhookStubMain is an httptest receiver that records deliveries.
type webhookStubMain struct {
	mu     sync.Mutex
	count  int
	types  []string
	sigs   []string
	keyIDs []string
}

func (s *webhookStubMain) handler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.count++
		s.types = append(s.types, r.Header.Get("X-Ledger-Event-Type"))
		s.sigs = append(s.sigs, r.Header.Get("X-Ledger-Signature"))
		s.keyIDs = append(s.keyIDs, r.Header.Get("X-Ledger-Key-Id"))
		s.mu.Unlock()
		w.WriteHeader(status)
	})
}

func (s *webhookStubMain) delivered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// seedWebhookServer builds a test server with the outbox and a webhook
// dispatcher attached. The dispatcher is stopped on cleanup.
func seedWebhookServer(t *testing.T, cfgs ...ledger.WebhookEndpointConfig) (*httptest.Server, *ledger.WebhookDispatcher) {
	t.Helper()
	dir := t.TempDir()
	ob, err := ledger.NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	d, err := ledger.NewWebhookDispatcher(cfgs...)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(d.Stop)
	l := ledger.New(ledger.WithOutbox(ob), ledger.WithWebhooks(d))
	srv := newServer(l)
	srv.webhooks = d
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, d
}

func mustWebhookKeys(t *testing.T) *ledger.RotatingKey {
	t.Helper()
	k, err := ledger.NewRotatingKey("k1", []byte("main-test-secret"), time.Hour)
	if err != nil {
		t.Fatalf("NewRotatingKey: %v", err)
	}
	return k
}

func fastWebhookConfig(name, url string, keys *ledger.RotatingKey) ledger.WebhookEndpointConfig {
	return ledger.WebhookEndpointConfig{
		Name:           name,
		URL:            url,
		Keys:           keys,
		Enabled:        true,
		MaxAttempts:    3,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		HTTPTimeout:    2 * time.Second,
		QueueSize:      64,
	}
}

func waitForMainWebhook(t *testing.T, d *ledger.WebhookDispatcher, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", msg)
}

func TestHandleWebhookDeliveries(t *testing.T) {
	stub := &webhookStubMain{}
	recv := httptest.NewServer(stub.handler(200))
	defer recv.Close()

	keys := mustWebhookKeys(t)
	cfg := fastWebhookConfig("ops", recv.URL, keys)
	cfg.Events = []string{ledger.OutboxTransferPosted}
	srv, d := seedWebhookServer(t, cfg)

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"wh-tx-1","from_account":"alice","to_account":"bob","amount_cents":2500,"idempotency_key":"wh-k1"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers: status=%d, want 201", code)
	}

	waitForMainWebhook(t, d, func() bool { return stub.delivered() == 1 }, "transfer_posted webhook")

	stub.mu.Lock()
	typ, sig, keyID := stub.types[0], stub.sigs[0], stub.keyIDs[0]
	stub.mu.Unlock()
	if typ != ledger.OutboxTransferPosted {
		t.Errorf("event type = %q, want transfer_posted", typ)
	}
	if keyID != "k1" {
		t.Errorf("key id = %q, want k1", keyID)
	}
	if !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature = %q, want sha256= prefix", sig)
	}

	// The delivery record is queryable, latest-first.
	code, body := getJSON(t, srv.URL+"/webhooks/deliveries")
	if code != http.StatusOK {
		t.Fatalf("GET /webhooks/deliveries: status=%d, want 200", code)
	}
	recs, ok := body["deliveries"].([]any)
	if !ok || len(recs) != 1 {
		t.Fatalf("deliveries = %v, want 1 record", body["deliveries"])
	}
	rec := recs[0].(map[string]any)
	if rec["status"] != ledger.WebhookDeliveryDelivered || rec["endpoint"] != "ops" {
		t.Errorf("record = %v, want delivered/ops", rec)
	}
	if rec["attempts"] != float64(1) {
		t.Errorf("attempts = %v, want 1", rec["attempts"])
	}
	if seq, ok := rec["event_seq"].(float64); !ok || seq < 1 {
		t.Errorf("event_seq = %v, want positive", rec["event_seq"])
	}

	// Filters.
	if _, body := getJSON(t, srv.URL+"/webhooks/deliveries?status=failed"); len(body["deliveries"].([]any)) != 0 {
		t.Errorf("status=failed returned records, want none")
	}
	if _, body := getJSON(t, srv.URL+"/webhooks/deliveries?endpoint=ops"); len(body["deliveries"].([]any)) != 1 {
		t.Errorf("endpoint=ops returned %d records, want 1", len(body["deliveries"].([]any)))
	}
	if _, body := getJSON(t, srv.URL+"/webhooks/deliveries?endpoint=ghost"); len(body["deliveries"].([]any)) != 0 {
		t.Errorf("endpoint=ghost returned records, want none")
	}
	if _, body := getJSON(t, srv.URL+"/webhooks/deliveries?limit=1"); len(body["deliveries"].([]any)) != 1 {
		t.Errorf("limit=1 returned %d records, want 1", len(body["deliveries"].([]any)))
	}

	// Bad queries are 400.
	for _, q := range []string{"?status=bogus", "?limit=0", "?limit=101", "?limit=abc"} {
		if code, _ := getJSON(t, srv.URL+"/webhooks/deliveries"+q); code != http.StatusBadRequest {
			t.Errorf("GET /webhooks/deliveries%s: status=%d, want 400", q, code)
		}
	}
}

func TestHandleWebhookDeliveriesDisabled(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/webhooks/deliveries")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 fail-closed", code)
	}
	if !strings.Contains(body["error"].(string), "LEDGER_WEBHOOKS") {
		t.Errorf("error = %v, want it to name LEDGER_WEBHOOKS", body["error"])
	}
}

func TestWebhookFailureMetric(t *testing.T) {
	stub := &webhookStubMain{}
	recv := httptest.NewServer(stub.handler(500))
	defer recv.Close()

	keys := mustWebhookKeys(t)
	srv, d := seedWebhookServer(t, fastWebhookConfig("down", recv.URL, keys))

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"wh-tx-2","from_account":"alice","to_account":"bob","amount_cents":100,"idempotency_key":"wh-k2"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers: status=%d, want 201", code)
	}

	// The transfer committed fine; the delivery fails asynchronously.
	waitForMainWebhook(t, d, func() bool {
		_, _, failures, _ := d.Stats()
		return failures == 1
	}, "failure counted")

	code, body := getJSON(t, srv.URL+"/webhooks/deliveries?status=failed")
	if code != http.StatusOK {
		t.Fatalf("GET /webhooks/deliveries?status=failed: status=%d", code)
	}
	if recs := body["deliveries"].([]any); len(recs) != 1 {
		t.Fatalf("failed deliveries = %d, want 1", len(recs))
	}

	// /metrics mirrors the dispatcher counter.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	var failuresLine string
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, "ledger_webhook_failures_total ") {
			failuresLine = line
		}
	}
	if failuresLine != "ledger_webhook_failures_total 1" {
		t.Errorf("metrics line = %q, want \"ledger_webhook_failures_total 1\"", failuresLine)
	}
}
