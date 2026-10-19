package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newOutboxStreamTestServer builds a server with the domain-event
// outbox attached, handing back the *server so tests can read its
// metrics (the stream connection gauge).
func newOutboxStreamTestServer(t *testing.T, opts ...ledger.Option) (*server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	ob, err := ledger.NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	opts = append([]ledger.Option{ledger.WithOutbox(ob)}, opts...)
	srv := newServer(ledger.New(opts...))
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// openSSE opens a GET /events/stream request with a cancellable
// context and returns the cancel func plus a line reader over the
// stream. The caller must call cancel to end the stream.
func openSSE(t *testing.T, url string, headers map[string]string) (context.CancelFunc, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("GET %s: status=%d, want 200", url, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return cancel, bufio.NewReader(resp.Body)
}

// readSSEUntil reads SSE lines until substr appears in the accumulated
// output or the timeout elapses.
func readSSEUntil(t *testing.T, r *bufio.Reader, substr string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		out string
		err bool
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: true}
				return
			}
			sb.WriteString(line)
			if strings.Contains(sb.String(), substr) {
				ch <- result{out: sb.String()}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err {
			t.Fatalf("stream closed before %q arrived", substr)
		}
		return res.out
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %q in SSE stream", substr)
		return ""
	}
}

func postLedgerEntry(t *testing.T, url, debit, credit string, cents int) {
	t.Helper()
	code, _ := postJSON(t, url+"/entries",
		fmt.Sprintf(`{"debit_account":%q,"credit_account":%q,"amount_cents":%d}`, debit, credit, cents))
	if code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
}

func waitForGauge(t *testing.T, srv *server, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := srv.metrics.OutboxStreamConnections.Load(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("OutboxStreamConnections = %d, want %d",
		srv.metrics.OutboxStreamConnections.Load(), want)
}

func TestHandleOutboxStreamReplayAndLive(t *testing.T) {
	srv, ts := newOutboxStreamTestServer(t)
	postLedgerEntry(t, ts.URL, "cash", "equity", 100)
	postLedgerEntry(t, ts.URL, "cash", "equity", 200)

	cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
	defer cancel()

	out := readSSEUntil(t, r, `"seq":2`, 5*time.Second)
	if !strings.Contains(out, ": connected") {
		t.Errorf("stream missing : connected comment:\n%s", out)
	}
	if !strings.Contains(out, `"seq":1`) || !strings.Contains(out, `"type":"entry_posted"`) {
		t.Errorf("replay missing seq 1 entry_posted:\n%s", out)
	}
	if !strings.Contains(out, "id: 1") || !strings.Contains(out, "id: 2") {
		t.Errorf("replay missing SSE ids:\n%s", out)
	}
	waitForGauge(t, srv, 1, 5*time.Second)

	// Live push: a freeze committed after the stream opened arrives
	// without any client action.
	if code, _ := postJSON(t, ts.URL+"/accounts/mallory/freeze", ""); code != http.StatusOK {
		t.Fatalf("freeze: status=%d, want 200", code)
	}
	live := readSSEUntil(t, r, `"type":"account_frozen"`, 5*time.Second)
	if !strings.Contains(live, "id: 3") {
		t.Errorf("live event missing id 3:\n%s", live)
	}

	// The /metrics gauge is covered by TestHandleOutboxStreamMetricsGauge.
	cancel()
	waitForGauge(t, srv, 0, 5*time.Second)
}

func TestHandleOutboxStreamCursorResume(t *testing.T) {
	_, ts := newOutboxStreamTestServer(t)
	postLedgerEntry(t, ts.URL, "cash", "equity", 100)
	postLedgerEntry(t, ts.URL, "cash", "equity", 200)

	// Grab the cursor after the first event via GET /events.
	_, body := getJSON(t, ts.URL+"/events?limit=1")
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("GET /events: empty next_cursor")
	}

	cancel, r := openSSE(t, ts.URL+"/events/stream?cursor="+cursor, nil)
	defer cancel()
	// Only seq 2 replays: seq 1 must not appear.
	out := readSSEUntil(t, r, `"seq":2`, 5*time.Second)
	if strings.Contains(out, `"seq":1`) {
		t.Errorf("cursor resume replayed seq 1:\n%s", out)
	}
}

func TestHandleOutboxStreamLastEventID(t *testing.T) {
	_, ts := newOutboxStreamTestServer(t)
	postLedgerEntry(t, ts.URL, "cash", "equity", 100)
	postLedgerEntry(t, ts.URL, "cash", "equity", 200)

	// EventSource-style reconnect: Last-Event-ID carries the raw seq.
	cancel, r := openSSE(t, ts.URL+"/events/stream", map[string]string{"Last-Event-ID": "1"})
	defer cancel()
	out := readSSEUntil(t, r, `"seq":2`, 5*time.Second)
	if strings.Contains(out, `"seq":1`) {
		t.Errorf("Last-Event-ID resume replayed seq 1:\n%s", out)
	}
}

func TestHandleOutboxStreamAccountFilter(t *testing.T) {
	_, ts := newOutboxStreamTestServer(t)
	cancel, r := openSSE(t, ts.URL+"/events/stream?account=cash", nil)
	defer cancel()
	readSSEUntil(t, r, ": connected", 5*time.Second)

	postLedgerEntry(t, ts.URL, "cash", "equity", 100)
	postLedgerEntry(t, ts.URL, "other", "equity", 500)

	// The cash event arrives live; the other-account event must not.
	out := readSSEUntil(t, r, `"account":"cash"`, 5*time.Second)
	if strings.Contains(out, `"account":"other"`) {
		t.Errorf("account filter leaked other-account event:\n%s", out)
	}
}

func TestHandleOutboxStreamHeartbeat(t *testing.T) {
	old := outboxStreamHeartbeatInterval
	outboxStreamHeartbeatInterval = 50 * time.Millisecond
	defer func() { outboxStreamHeartbeatInterval = old }()

	_, ts := newOutboxStreamTestServer(t)
	cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
	defer cancel()
	readSSEUntil(t, r, ": ping", 5*time.Second)
}

func TestHandleOutboxStreamNoOutbox404(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/events/stream")
	if err != nil {
		t.Fatalf("GET /events/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHandleOutboxStreamBadCursor(t *testing.T) {
	_, ts := newOutboxStreamTestServer(t)
	resp, err := http.Get(ts.URL + "/events/stream?cursor=!!!not-base64!!!")
	if err != nil {
		t.Fatalf("GET /events/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
}

func TestHandleOutboxStreamAuth(t *testing.T) {
	dir := t.TempDir()
	ob, err := ledger.NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	l := ledger.New(ledger.WithAPIKeyAuth(), ledger.WithOutbox(ob))
	srv := httptest.NewServer(newServer(l).handler())
	defer srv.Close()

	// The bootstrap endpoint mints exactly one key (it closes once a
	// key exists): bootstrap an admin key, then mint the scoped keys
	// through it.
	adminKey := bootstrapKey(t, srv, "admin:keys")
	mintKey := func(scopes ...string) string {
		payload := `{"name":"t","scopes":[` + `"` + strings.Join(scopes, `","`) + `"]}`
		code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", adminKey, payload)
		if code != http.StatusCreated {
			t.Fatalf("POST /api-keys = %d: %v", code, body)
		}
		secret, _ := body["secret"].(string)
		return secret
	}
	readKey := mintKey("read:ledger")
	writeKey := mintKey("write:entries")

	getStream := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events/stream", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /events/stream: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := getStream(""); code != http.StatusUnauthorized {
		t.Errorf("no key: status=%d, want 401", code)
	}
	if code := getStream(writeKey); code != http.StatusForbidden {
		t.Errorf("write-only key: status=%d, want 403", code)
	}
	if code := getStream(readKey); code != http.StatusOK {
		t.Errorf("read key: status=%d, want 200", code)
	}
}

func TestHandleOutboxStreamMetricsGauge(t *testing.T) {
	srv, ts := newOutboxStreamTestServer(t)
	cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
	readSSEUntil(t, r, ": connected", 5*time.Second)

	resp, err := http.Get(ts.URL + "/metrics")
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
	body := sb.String()
	if !strings.Contains(body, "ledger_outbox_stream_connections 1") {
		t.Errorf("/metrics missing gauge =1:\n%s", body)
	}
	cancel()
	waitForGauge(t, srv, 0, 5*time.Second)
}
