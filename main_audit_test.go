package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestAuditLogEndToEnd(t *testing.T) {
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// A mutating op and a reconcile run through HTTP.
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"rev","amount_cents":100}`); code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
	if code, _ := postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg1","from_account":"cash","to_account":"treasury"}`); code != http.StatusCreated {
		t.Fatalf("POST /merges: status=%d, want 201", code)
	}
	if code, _ := postJSON(t, srv.URL+"/reconcile", `{}`); code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d, want 200", code)
	}
	// Reads add nothing.
	if code, _ := getJSON(t, srv.URL+"/accounts/treasury/balance"); code != http.StatusOK {
		t.Fatalf("GET /balance: status=%d, want 200", code)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ledger.ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	ops := map[string]int{}
	for _, ev := range events {
		ops[ev.Op]++
	}
	for op, want := range map[string]int{"post": 1, "merge": 1, "reconcile": 1} {
		if ops[op] != want {
			t.Errorf("op %q: got %d events, want %d (all ops: %v)", op, ops[op], want, ops)
		}
	}

	// The audit counters are synced into /metrics on scrape.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, want := range []string{"ledger_audit_events_total 4", "ledger_audit_dropped_total 0"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestAuditSinkMaxAttemptsEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", ledger.DefaultSinkMaxAttempts},
		{"3", 3},
		{"1", 1},
		{"garbage", ledger.DefaultSinkMaxAttempts},
		{"0", ledger.DefaultSinkMaxAttempts},
		{"-5", ledger.DefaultSinkMaxAttempts},
		{"2.5", ledger.DefaultSinkMaxAttempts},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv("LEDGER_AUDIT_SINK_MAX_ATTEMPTS", tc.value)
			if got := auditSinkMaxAttempts(); got != tc.want {
				t.Errorf("auditSinkMaxAttempts() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAuditSinkDurationEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		def   time.Duration
		want  time.Duration
	}{
		{"LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "", ledger.DefaultSinkInitialBackoff, ledger.DefaultSinkInitialBackoff},
		{"LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "500ms", ledger.DefaultSinkInitialBackoff, 500 * time.Millisecond},
		{"LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "garbage", ledger.DefaultSinkInitialBackoff, ledger.DefaultSinkInitialBackoff},
		{"LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "-1s", ledger.DefaultSinkInitialBackoff, ledger.DefaultSinkInitialBackoff},
		{"LEDGER_AUDIT_SINK_MAX_BACKOFF", "2m", ledger.DefaultSinkMaxBackoff, 2 * time.Minute},
		{"LEDGER_AUDIT_SINK_MAX_BACKOFF", "garbage", ledger.DefaultSinkMaxBackoff, ledger.DefaultSinkMaxBackoff},
		{"LEDGER_AUDIT_SINK_POLL_INTERVAL", "250ms", ledger.DefaultSinkPollInterval, 250 * time.Millisecond},
		{"LEDGER_AUDIT_SINK_POLL_INTERVAL", "garbage", ledger.DefaultSinkPollInterval, ledger.DefaultSinkPollInterval},
		{"LEDGER_AUDIT_SINK_HTTP_TIMEOUT", "30s", ledger.DefaultSinkHTTPTimeout, 30 * time.Second},
		{"LEDGER_AUDIT_SINK_HTTP_TIMEOUT", "-1s", ledger.DefaultSinkHTTPTimeout, ledger.DefaultSinkHTTPTimeout},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if got := auditSinkDuration(tc.name, tc.def); got != tc.want {
				t.Errorf("auditSinkDuration(%s, %v) = %v, want %v", tc.name, tc.def, got, tc.want)
			}
		})
	}
}

// TestAuditSinkConfigEnvEndToEnd wires the five env vars through
// auditSinkConfig into a running sink: valid values take effect, and
// an InitialBackoff above MaxBackoff is clamped by withDefaults at
// StartAuditSink.
func TestAuditSinkConfigEnvEndToEnd(t *testing.T) {
	t.Setenv("LEDGER_AUDIT_SINK_MAX_ATTEMPTS", "3")
	t.Setenv("LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "10s")
	t.Setenv("LEDGER_AUDIT_SINK_MAX_BACKOFF", "1s")
	t.Setenv("LEDGER_AUDIT_SINK_POLL_INTERVAL", "250ms")
	t.Setenv("LEDGER_AUDIT_SINK_HTTP_TIMEOUT", "2s")

	dir := t.TempDir()
	outbox, err := ledger.NewOutbox(filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = outbox.Close() })
	l := ledger.New(ledger.WithOutbox(outbox))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink, err := ledger.StartAuditSink(ctx, l, auditSinkConfig("https://example.com/hook", "test-secret"))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	defer sink.Stop()

	pol := sink.DeliveryPolicy()
	if got := *pol.MaxAttempts; got != 3 {
		t.Errorf("MaxAttempts = %d, want 3", got)
	}
	if got := *pol.InitialBackoff; got != time.Second {
		t.Errorf("InitialBackoff = %v, want 1s (clamped to MaxBackoff)", got)
	}
	if got := *pol.MaxBackoff; got != time.Second {
		t.Errorf("MaxBackoff = %v, want 1s", got)
	}
	if got := *pol.HTTPTimeout; got != 2*time.Second {
		t.Errorf("HTTPTimeout = %v, want 2s", got)
	}
	if got := sink.PollInterval(); got != 250*time.Millisecond {
		t.Errorf("PollInterval() = %v, want 250ms", got)
	}
}

// TestAuditSinkConfigEnvInvalidFallsBackToDefaults wires five invalid
// env values through auditSinkConfig: the sink starts on the
// documented defaults and never fails startup over a bad env var.
func TestAuditSinkConfigEnvInvalidFallsBackToDefaults(t *testing.T) {
	t.Setenv("LEDGER_AUDIT_SINK_MAX_ATTEMPTS", "garbage")
	t.Setenv("LEDGER_AUDIT_SINK_INITIAL_BACKOFF", "garbage")
	t.Setenv("LEDGER_AUDIT_SINK_MAX_BACKOFF", "-5s")
	t.Setenv("LEDGER_AUDIT_SINK_POLL_INTERVAL", "garbage")
	t.Setenv("LEDGER_AUDIT_SINK_HTTP_TIMEOUT", "-1s")

	dir := t.TempDir()
	outbox, err := ledger.NewOutbox(filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = outbox.Close() })
	l := ledger.New(ledger.WithOutbox(outbox))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink, err := ledger.StartAuditSink(ctx, l, auditSinkConfig("https://example.com/hook", "test-secret"))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	defer sink.Stop()

	pol := sink.DeliveryPolicy()
	if got := *pol.MaxAttempts; got != ledger.DefaultSinkMaxAttempts {
		t.Errorf("MaxAttempts = %d, want default %d", got, ledger.DefaultSinkMaxAttempts)
	}
	if got := *pol.InitialBackoff; got != ledger.DefaultSinkInitialBackoff {
		t.Errorf("InitialBackoff = %v, want default %v", got, ledger.DefaultSinkInitialBackoff)
	}
	if got := *pol.MaxBackoff; got != ledger.DefaultSinkMaxBackoff {
		t.Errorf("MaxBackoff = %v, want default %v", got, ledger.DefaultSinkMaxBackoff)
	}
	if got := *pol.HTTPTimeout; got != ledger.DefaultSinkHTTPTimeout {
		t.Errorf("HTTPTimeout = %v, want default %v", got, ledger.DefaultSinkHTTPTimeout)
	}
	if got := sink.PollInterval(); got != ledger.DefaultSinkPollInterval {
		t.Errorf("PollInterval() = %v, want default %v", got, ledger.DefaultSinkPollInterval)
	}
}

func TestAuditMaxBytesEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", ledger.DefaultAuditMaxBytes},
		{"1048576", 1048576},
		{"garbage", ledger.DefaultAuditMaxBytes},
		{"0", ledger.DefaultAuditMaxBytes},
		{"-5", ledger.DefaultAuditMaxBytes},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv("LEDGER_AUDIT_MAX_BYTES", tc.value)
			if got := auditMaxBytes(); got != tc.want {
				t.Errorf("auditMaxBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}
