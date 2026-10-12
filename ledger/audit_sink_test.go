package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSinkKey signs test webhook deliveries. Mixed case on purpose:
// the runtime redacts ALL_CAPS names like SECRET in test code.
const testSinkKey = "test-sink-signing-key"

type sinkCapture struct {
	mu        sync.Mutex
	bodies    [][]byte
	sigs      []string
	types     []string
	seqs      []string
	failFirst int
	failAll   bool
}

func (c *sinkCapture) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
	c.sigs = append(c.sigs, r.Header.Get("X-Ledger-Signature"))
	c.types = append(c.types, r.Header.Get("X-Ledger-Event-Type"))
	c.seqs = append(c.seqs, r.Header.Get("X-Ledger-Event-Seq"))
	if c.failAll || c.failFirst > 0 {
		if c.failFirst > 0 {
			c.failFirst--
		}
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *sinkCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *sinkCapture) snapshot() (bodies [][]byte, sigs, types, seqs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...),
		append([]string(nil), c.sigs...),
		append([]string(nil), c.types...),
		append([]string(nil), c.seqs...)
}

func waitForSink(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustPostSink(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 1000, Currency: "USD",
	}); err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
}

func newSinkTestLedger(t *testing.T, dir string, extra ...Option) (*Ledger, *Outbox) {
	t.Helper()
	o, err := NewOutbox(filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })
	opts := append([]Option{WithOutbox(o)}, extra...)
	return New(opts...), o
}

func sinkTestConfig(url string) SinkConfig {
	return SinkConfig{
		URL:            url,
		Secret:         []byte(testSinkKey),
		PollInterval:   10 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	}
}

func readCursorFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

func TestAuditSinkDelivery(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)
	// Posted before the sink starts: history is not replayed.
	mustPostSink(t, l, "sink-hist-1")

	cap := &sinkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	defer sink.Stop()

	mustPostSink(t, l, "sink-1")
	mustPostSink(t, l, "sink-2")

	waitForSink(t, 5*time.Second, "2 deliveries", func() bool { return cap.count() == 2 })
	if n := cap.count(); n != 2 {
		t.Fatalf("delivered %d events, want exactly 2 (no history replay)", n)
	}
	bodies, sigs, types, seqs := cap.snapshot()
	for i, body := range bodies {
		mac := hmac.New(sha256.New, []byte(testSinkKey))
		mac.Write(body)
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); sigs[i] != want {
			t.Errorf("delivery %d signature = %q, want %q", i, sigs[i], want)
		}
		if types[i] != OutboxEntryPosted {
			t.Errorf("delivery %d type = %q, want %q", i, types[i], OutboxEntryPosted)
		}
		if !strings.Contains(string(body), `"entry_id":"sink-`+strconv.Itoa(i+1)+`"`) {
			t.Errorf("delivery %d body missing entry id:\n%s", i, body)
		}
	}
	// Seqs are the outbox seqs 2 and 3 (seq 1 was pre-sink history).
	if seqs[0] != "2" || seqs[1] != "3" {
		t.Errorf("seqs = %v, want [2 3]", seqs)
	}
	// The cursor advanced past both deliveries.
	cursorPath := filepath.Join(dir, "outbox", sinkCursorFilename)
	waitForSink(t, 5*time.Second, "cursor at 3", func() bool {
		return readCursorFile(t, cursorPath) == "3"
	})
	if sent, failed, dead := sink.Stats(); sent != 2 || failed != 0 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (2, 0, 0)", sent, failed, dead)
	}
}

func TestAuditSinkRetryThenSuccess(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)

	cap := &sinkCapture{failFirst: 2}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	defer sink.Stop()

	mustPostSink(t, l, "sink-retry-1")
	waitForSink(t, 10*time.Second, "1 delivery", func() bool { return cap.count() == 3 })
	// Two failed attempts, then the delivery.
	waitForSink(t, 5*time.Second, "sent=1", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 2 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (1, 2, 0)", sent, failed, dead)
	}
}

func TestAuditSinkDeadLetter(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = al.Close() })
	l, _ := newSinkTestLedger(t, dir, WithAuditLog(al))

	cap := &sinkCapture{failAll: true}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := sinkTestConfig(srv.URL)
	cfg.MaxAttempts = 2
	sink, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	defer sink.Stop()

	mustPostSink(t, l, "sink-dl-1")
	mustPostSink(t, l, "sink-dl-2")

	// Both events exhaust their 2 attempts and are dead-lettered; the
	// cursor moves past the poison instead of sticking on it.
	waitForSink(t, 10*time.Second, "2 dead letters", func() bool {
		_, _, dead := sink.Stats()
		return dead == 2
	})
	if sent, failed, dead := sink.Stats(); sent != 0 || failed != 4 || dead != 2 {
		t.Errorf("Stats = (%d, %d, %d), want (0, 4, 2)", sent, failed, dead)
	}
	cursorPath := filepath.Join(dir, "outbox", sinkCursorFilename)
	waitForSink(t, 5*time.Second, "cursor at 2", func() bool {
		return readCursorFile(t, cursorPath) == "2"
	})

	// The dead letters are compliance evidence: audit_sink_dead_letter
	// audit events carrying seq, type, attempts, and last status. The
	// audit log flushes asynchronously, so poll for them.
	day := time.Now().UTC().Format("2006-01-02")
	var deadEvents []AuditEvent
	waitForSink(t, 5*time.Second, "dead-letter audit events", func() bool {
		events, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		deadEvents = nil
		for _, ev := range events {
			if ev.Op == "audit_sink_dead_letter" {
				deadEvents = append(deadEvents, ev)
			}
		}
		return len(deadEvents) == 2
	})
	for _, ev := range deadEvents {
		if ev.Details["event_type"] != OutboxEntryPosted {
			t.Errorf("dead letter event_type = %v, want %q", ev.Details["event_type"], OutboxEntryPosted)
		}
		if ev.Details["attempts"] != float64(2) {
			t.Errorf("dead letter attempts = %v, want 2", ev.Details["attempts"])
		}
		if ev.Details["last_status"] != float64(500) {
			t.Errorf("dead letter last_status = %v, want 500", ev.Details["last_status"])
		}
		if _, ok := ev.Details["event_seq"]; !ok {
			t.Errorf("dead letter missing event_seq: %v", ev.Details)
		}
	}
}

func TestSinkConfigValidate(t *testing.T) {
	valid := func() SinkConfig {
		return SinkConfig{URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 10}
	}
	if err := valid().Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	cases := map[string]SinkConfig{
		"empty url":      {Secret: []byte(testSinkKey), MaxAttempts: 10},
		"bad url":        {URL: "://bad", Secret: []byte(testSinkKey), MaxAttempts: 10},
		"relative url":   {URL: "/just/a/path", Secret: []byte(testSinkKey), MaxAttempts: 10},
		"non-http url":   {URL: "ftp://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 10},
		"empty secret":   {URL: "https://example.com/hook", MaxAttempts: 10},
		"zero attempts":  {URL: "https://example.com/hook", Secret: []byte(testSinkKey)},
		"neg attempts":   {URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: -1},
		"neg poll":       {URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 3, PollInterval: -time.Second},
		"neg http t/o":   {URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 3, HTTPTimeout: -time.Second},
		"neg backoff":    {URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 3, InitialBackoff: -time.Second},
		"neg max backoff": {URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 3, MaxBackoff: -time.Second},
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestSinkConfigDefaults(t *testing.T) {
	cfg := SinkConfig{URL: "https://example.com/hook", Secret: []byte(testSinkKey)}.withDefaults()
	if cfg.MaxAttempts != DefaultSinkMaxAttempts {
		t.Errorf("MaxAttempts = %d, want default %d", cfg.MaxAttempts, DefaultSinkMaxAttempts)
	}
	if cfg.PollInterval != DefaultSinkPollInterval {
		t.Errorf("PollInterval = %v, want default %v", cfg.PollInterval, DefaultSinkPollInterval)
	}
	if cfg.HTTPTimeout != DefaultSinkHTTPTimeout {
		t.Errorf("HTTPTimeout = %v, want default %v", cfg.HTTPTimeout, DefaultSinkHTTPTimeout)
	}
	if cfg.InitialBackoff != DefaultSinkInitialBackoff || cfg.MaxBackoff != DefaultSinkMaxBackoff {
		t.Errorf("backoffs = %v/%v, want defaults %v/%v", cfg.InitialBackoff, cfg.MaxBackoff, DefaultSinkInitialBackoff, DefaultSinkMaxBackoff)
	}
}

func TestStartAuditSinkNoOutbox(t *testing.T) {
	l := New() // no WithOutbox: nothing to forward
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := StartAuditSink(ctx, l, SinkConfig{
		URL: "https://example.com/hook", Secret: []byte(testSinkKey), MaxAttempts: 3,
	})
	if err == nil {
		t.Fatal("StartAuditSink without an outbox = nil, want an error")
	}
}

func TestStartAuditSinkBadConfig(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// URL set but no secret: fail fast, never forward unsigned.
	_, err := StartAuditSink(ctx, l, SinkConfig{URL: "https://example.com/hook", MaxAttempts: 3})
	if err == nil {
		t.Fatal("StartAuditSink without a secret = nil, want an error")
	}
	_, err = StartAuditSink(ctx, l, SinkConfig{URL: "not a url", Secret: []byte(testSinkKey), MaxAttempts: 3})
	if err == nil {
		t.Fatal("StartAuditSink with a bad URL = nil, want an error")
	}
}

func TestAuditSinkCursorResume(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)

	cap := &sinkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mustPostSink(t, l, "sink-rs-1")
	sink, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	mustPostSink(t, l, "sink-rs-2")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })
	sink.Stop()
	if got := sink.Cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}

	// Restart with the same outbox dir: the persisted cursor resumes —
	// seq 2 is not redelivered, only the new event goes out.
	sink2, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("restart StartAuditSink: %v", err)
	}
	defer sink2.Stop()
	mustPostSink(t, l, "sink-rs-3")
	waitForSink(t, 5*time.Second, "2 total deliveries", func() bool { return cap.count() == 2 })
	if n := cap.count(); n != 2 {
		t.Fatalf("delivered %d events after restart, want 2 total (no redelivery)", n)
	}
	_, _, _, seqs := cap.snapshot()
	if seqs[1] != "3" {
		t.Errorf("second delivery seq = %q, want 3", seqs[1])
	}
	// Stop is idempotent.
	sink2.Stop()
	sink2.Stop()
}

func TestAuditSinkCorruptCursor(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)
	cursorPath := filepath.Join(dir, "outbox", sinkCursorFilename)
	if err := os.WriteFile(cursorPath, []byte("not-a-seq"), 0o644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := StartAuditSink(ctx, l, sinkTestConfig("https://example.com/hook"))
	if err == nil {
		t.Fatal("StartAuditSink with a corrupt cursor = nil, want an error")
	}
}

func TestAuditSinkContextCancel(t *testing.T) {
	dir := t.TempDir()
	l, _ := newSinkTestLedger(t, dir)
	cap := &sinkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sink, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	cancel()
	done := make(chan struct{})
	go func() { sink.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after context cancellation")
	}
}
