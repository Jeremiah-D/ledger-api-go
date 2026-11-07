package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// pingCounter counts ": ping" lines on an SSE reader until the stream
// closes. One counter owns one reader: the counting goroutine runs for
// the stream's lifetime, so successive windows on the same stream read
// one shared counter instead of racing two readers.
type pingCounter struct {
	n    atomic.Int64
	done chan struct{}
}

func startPingCounter(t *testing.T, r *bufio.Reader) *pingCounter {
	t.Helper()
	c := &pingCounter{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.Contains(line, ": ping") {
				c.n.Add(1)
			}
		}
	}()
	return c
}

func (c *pingCounter) count() int { return int(c.n.Load()) }

func TestSetOutboxStreamHeartbeatInterval(t *testing.T) {
	t.Run("invalid values are rejected and the live interval is unchanged", func(t *testing.T) {
		srv := &server{ledger: ledger.New(), outboxStreamHeartbeatInterval: 15 * time.Second}
		for _, d := range []time.Duration{0, -time.Second, -time.Nanosecond} {
			prev, err := srv.SetOutboxStreamHeartbeatInterval(d)
			if !errors.Is(err, ErrInvalidOutboxStreamHeartbeatInterval) {
				t.Errorf("SetOutboxStreamHeartbeatInterval(%v) err = %v, want ErrInvalidOutboxStreamHeartbeatInterval", d, err)
			}
			if prev != 15*time.Second {
				t.Errorf("SetOutboxStreamHeartbeatInterval(%v) prev = %v, want 15s", d, prev)
			}
			if got := srv.OutboxStreamHeartbeatInterval(); got != 15*time.Second {
				t.Errorf("after rejected SetOutboxStreamHeartbeatInterval(%v), OutboxStreamHeartbeatInterval() = %v, want 15s", d, got)
			}
		}
		// The Ctx variant rejects the same way.
		if prev, err := srv.SetOutboxStreamHeartbeatIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidOutboxStreamHeartbeatInterval) || prev != 15*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatIntervalCtx(0) = (%v, %v), want (15s, ErrInvalidOutboxStreamHeartbeatInterval)", prev, err)
		}
	})

	t.Run("same value is a no-op", func(t *testing.T) {
		srv := &server{ledger: ledger.New(), outboxStreamHeartbeatInterval: 15 * time.Second}
		prev, err := srv.SetOutboxStreamHeartbeatInterval(15 * time.Second)
		if err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(15s) err = %v", err)
		}
		if prev != 15*time.Second {
			t.Errorf("prev = %v, want 15s", prev)
		}
		if got := srv.OutboxStreamHeartbeatInterval(); got != 15*time.Second {
			t.Errorf("OutboxStreamHeartbeatInterval() = %v, want 15s", got)
		}
	})

	t.Run("getter follows the live value", func(t *testing.T) {
		srv := &server{ledger: ledger.New(), outboxStreamHeartbeatInterval: 15 * time.Second}
		prev, err := srv.SetOutboxStreamHeartbeatInterval(5 * time.Second)
		if err != nil || prev != 15*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(5s) = (%v, %v), want (15s, nil)", prev, err)
		}
		if got := srv.OutboxStreamHeartbeatInterval(); got != 5*time.Second {
			t.Fatalf("OutboxStreamHeartbeatInterval() = %v, want 5s", got)
		}
		prev, err = srv.SetOutboxStreamHeartbeatInterval(15 * time.Second)
		if err != nil || prev != 5*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(15s) = (%v, %v), want (5s, nil)", prev, err)
		}
	})

	t.Run("tightening makes a new stream ping denser", func(t *testing.T) {
		srv, ts := newOutboxStreamTestServer(t)
		if prev, err := srv.SetOutboxStreamHeartbeatInterval(50 * time.Millisecond); err != nil || prev != 15*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(50ms) = (%v, %v), want (15s, nil)", prev, err)
		}
		cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancel()
		readSSEUntil(t, r, ": ping", 5*time.Second)
	})

	t.Run("loosening makes a new stream ping sparser", func(t *testing.T) {
		srv, ts := newOutboxStreamTestServer(t)
		if prev, err := srv.SetOutboxStreamHeartbeatInterval(10 * time.Second); err != nil || prev != 15*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(10s) = (%v, %v), want (15s, nil)", prev, err)
		}
		cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancel()
		counter := startPingCounter(t, r)
		// A 10s heartbeat cannot ping inside 5s; a single ping here
		// would mean the new stream still runs the old interval.
		time.Sleep(5 * time.Second)
		if got := counter.count(); got != 0 {
			t.Fatalf("new stream at 10s pinged %d times in 5s, want 0", got)
		}
	})

	t.Run("revision does not touch a stream already open", func(t *testing.T) {
		srv, ts := newOutboxStreamTestServer(t)
		if _, err := srv.SetOutboxStreamHeartbeatInterval(50 * time.Millisecond); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(50ms): %v", err)
		}
		cancelA, rA := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancelA()
		counterA := startPingCounter(t, rA)
		time.Sleep(time.Second)
		before := counterA.count()
		if before < 10 {
			t.Fatalf("stream at 50ms pinged %d times in 1s, want >= 10", before)
		}
		// Loosen to 10s while stream A is open: its ticker was built
		// under 50ms and must keep that rhythm.
		if _, err := srv.SetOutboxStreamHeartbeatInterval(10 * time.Second); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(10s): %v", err)
		}
		time.Sleep(time.Second)
		after := counterA.count()
		if after-before < 10 {
			t.Fatalf("open stream pinged %d times in the 1s after loosening to 10s, want >= 10 (old ticker untouched)", after-before)
		}
		// A stream opened after the revision runs the new interval:
		// no ping inside 2s.
		cancelB, rB := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancelB()
		counterB := startPingCounter(t, rB)
		time.Sleep(2 * time.Second)
		if got := counterB.count(); got != 0 {
			t.Fatalf("stream opened after loosening pinged %d times in 2s, want 0", got)
		}
	})

	t.Run("concurrent revisions and stream opens do not race", func(t *testing.T) {
		srv, ts := newOutboxStreamTestServer(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					if _, err := srv.SetOutboxStreamHeartbeatInterval(time.Duration(50+i*10+j) * time.Millisecond); err != nil {
						t.Errorf("SetOutboxStreamHeartbeatInterval: %v", err)
						return
					}
				}
			}(i)
		}
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
					readSSEUntil(t, r, ": connected", 5*time.Second)
					cancel()
				}
			}()
		}
		wg.Wait()
		if got := srv.OutboxStreamHeartbeatInterval(); got <= 0 {
			t.Fatalf("OutboxStreamHeartbeatInterval() = %v after concurrent revisions, want positive", got)
		}
	})

	t.Run("ctx actor is recorded in the revision log", func(t *testing.T) {
		var buf bytes.Buffer
		old := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(old)
		srv := &server{ledger: ledger.New(), outboxStreamHeartbeatInterval: 15 * time.Second}
		ctx := ledger.ContextWithActor(context.Background(), "op-7")
		if prev, err := srv.SetOutboxStreamHeartbeatIntervalCtx(ctx, 5*time.Second); err != nil || prev != 15*time.Second {
			t.Fatalf("SetOutboxStreamHeartbeatIntervalCtx = (%v, %v), want (15s, nil)", prev, err)
		}
		if !strings.Contains(buf.String(), "previous=15s current=5s actor=op-7") {
			t.Errorf("log %q does not record previous/current/actor", buf.String())
		}
		mark := buf.Len()
		if _, err := srv.SetOutboxStreamHeartbeatInterval(10 * time.Second); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(10s): %v", err)
		}
		if !strings.Contains(buf.String()[mark:], "previous=5s current=10s actor=system") {
			t.Errorf("log %q does not default the actor to system", buf.String()[mark:])
		}
		// A same-value revision is a no-op and logs nothing.
		mark = buf.Len()
		if _, err := srv.SetOutboxStreamHeartbeatInterval(10 * time.Second); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(10s) again: %v", err)
		}
		if buf.Len() != mark {
			t.Errorf("no-op revision logged %q, want silence", buf.String()[mark:])
		}
	})

	t.Run("the handler reads the interval through the getter", func(t *testing.T) {
		src, err := os.ReadFile("main.go")
		if err != nil {
			t.Fatalf("read main.go: %v", err)
		}
		if got := strings.Count(string(src), "time.NewTicker(outboxStreamHeartbeatInterval)"); got != 0 {
			t.Errorf("found %d direct package-var reads in the handler, want 0", got)
		}
		if got := strings.Count(string(src), "time.NewTicker(s.OutboxStreamHeartbeatInterval())"); got != 1 {
			t.Errorf("found %d handler reads through the getter, want 1", got)
		}
		if got := strings.Count(string(src), "outboxStreamHeartbeatInterval: outboxStreamHeartbeatInterval"); got != 1 {
			t.Errorf("found %d newServer seedings from the package var, want 1", got)
		}
	})

	t.Run("newServer still assembles the 15s default", func(t *testing.T) {
		if got := newServer(ledger.New()).OutboxStreamHeartbeatInterval(); got != 15*time.Second {
			t.Errorf("newServer OutboxStreamHeartbeatInterval() = %v, want 15s", got)
		}
	})

	t.Run("revision leaves the rest of the stream semantics alone", func(t *testing.T) {
		srv, ts := newOutboxStreamTestServer(t)
		if _, err := srv.SetOutboxStreamHeartbeatInterval(50 * time.Millisecond); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(50ms): %v", err)
		}
		cancel, r := openSSE(t, ts.URL+"/events/stream?account=cash", nil)
		defer cancel()
		readSSEUntil(t, r, ": connected", 5*time.Second)

		postLedgerEntry(t, ts.URL, "cash", "equity", 100)
		postLedgerEntry(t, ts.URL, "other", "equity", 500)

		// The cash event arrives live; the other-account event must not.
		out := readSSEUntil(t, r, `"account":"cash"`, 5*time.Second)
		if strings.Contains(out, `"account":"other"`) {
			t.Errorf("account filter leaked other-account event after revision:\n%s", out)
		}

		// Without an outbox the stream still 404s fail-closed after
		// a revision: the 404 path returns before any ticker exists.
		srv2 := newServer(ledger.New())
		if _, err := srv2.SetOutboxStreamHeartbeatInterval(50 * time.Millisecond); err != nil {
			t.Fatalf("SetOutboxStreamHeartbeatInterval(50ms) on no-outbox server: %v", err)
		}
		ts2 := httptest.NewServer(srv2.handler())
		defer ts2.Close()
		resp, err := http.Get(ts2.URL + "/events/stream")
		if err != nil {
			t.Fatalf("GET /events/stream: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status=%d, want 404", resp.StatusCode)
		}
	})
}

// waitSSECommentDiscarding reads SSE lines until one contains substr,
// discarding everything else: a slow-consumer burst can precede the
// disconnect comment with megabytes of event frames, so nothing is
// accumulated (unlike readSSEUntil, which keeps the whole transcript
// and would scan it quadratically).
func waitSSECommentDiscarding(t *testing.T, r *bufio.Reader, substr string, timeout time.Duration) {
	t.Helper()
	type result struct{ found bool }
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{found: false}
				return
			}
			if strings.Contains(line, substr) {
				ch <- result{found: true}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if !res.found {
			t.Fatalf("stream closed before %q arrived", substr)
		}
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %q in SSE stream", substr)
	}
}

// newOutboxStreamBufferTestServer builds a stream test server and
// hands back the outbox handle, so the test can Emit directly (no HTTP
// round trip per event) while the stream reads through the server.
func newOutboxStreamBufferTestServer(t *testing.T) (*server, *httptest.Server, *ledger.Outbox) {
	t.Helper()
	dir := t.TempDir()
	ob, err := ledger.NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	srv := newServer(ledger.New(ledger.WithOutbox(ob)))
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return srv, ts, ob
}

func TestSetOutboxStreamBufferSizeStream(t *testing.T) {
	t.Run("widened buffer keeps a slow new stream alive past 512 events", func(t *testing.T) {
		srv, ts, ob := newOutboxStreamBufferTestServer(t)
		if prev, err := srv.ledger.SetOutboxStreamBufferSize(2048); err != nil || prev != 512 {
			t.Fatalf("SetOutboxStreamBufferSize(2048) = (%d, %v), want (512, nil)", prev, err)
		}
		cancel, _ := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancel()
		waitForGauge(t, srv, 1, 5*time.Second)
		// 600 events, never read: at the default 512 the 513th would
		// drop the stream; at 2048 the subscription holds them all.
		for i := 0; i < 600; i++ {
			if !ob.Emit(ledger.OutboxEvent{Type: ledger.OutboxEntryPosted}) {
				t.Fatalf("Emit %d blocked or failed", i)
			}
		}
		if drops, ok := srv.ledger.OutboxSlowDrops(); !ok || drops != 0 {
			t.Fatalf("OutboxSlowDrops() = %d, %v; want 0, true", drops, ok)
		}
		// The stream is still open: the handler has not returned.
		waitForGauge(t, srv, 1, 5*time.Second)
	})

	t.Run("tightened buffer disconnects a slow new stream fast", func(t *testing.T) {
		srv, ts, ob := newOutboxStreamBufferTestServer(t)
		if prev, err := srv.ledger.SetOutboxStreamBufferSize(4); err != nil || prev != 512 {
			t.Fatalf("SetOutboxStreamBufferSize(4) = (%d, %v), want (512, nil)", prev, err)
		}
		cancel, r := openSSE(t, ts.URL+"/events/stream", nil)
		defer cancel()
		waitForGauge(t, srv, 1, 5*time.Second)
		// Flood the stream without reading: each event carries a
		// 200KB pad, so ~30MB of frames far exceed the socket
		// buffers — the handler stalls on its write, the 4-deep
		// queue fills, and Emit drops the subscription.
		pad := strings.Repeat("x", 200_000)
		for i := 0; i < 150; i++ {
			if !ob.Emit(ledger.OutboxEvent{Type: "buffer_probe", Details: map[string]any{"pad": pad}}) {
				t.Fatalf("Emit %d blocked or failed", i)
			}
		}
		if drops, ok := srv.ledger.OutboxSlowDrops(); !ok || drops != 1 {
			t.Fatalf("OutboxSlowDrops() = %d, %v; want 1, true", drops, ok)
		}
		// Reading again drains the stalled socket, so the handler
		// reaches the closed channel and ends the stream with the
		// terminal comment.
		waitSSECommentDiscarding(t, r, ": slow-consumer-disconnected", 15*time.Second)
		waitForGauge(t, srv, 0, 5*time.Second)
	})
}
