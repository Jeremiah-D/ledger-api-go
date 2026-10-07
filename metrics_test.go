package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// parseExposition extracts <metric> -> <value> from a Prometheus text
// exposition body, ignoring HELP/TYPE comment lines.
func parseExposition(t *testing.T, body string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("malformed exposition line %q", line)
		}
		v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			t.Fatalf("metric %s value %q is not an integer: %v", name, value, err)
		}
		out[name] = v
	}
	return out
}

func getText(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(b), resp.Header
}

func TestMetricsEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	post := func(body string) int {
		resp, err := http.Post(srv.URL+"/entries", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /entries: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	entry := `{"debit_account":"cash","credit_account":"equity","amount_cents":500,"idempotency_key":"m-1"}`
	if code := post(entry); code != http.StatusCreated {
		t.Fatalf("first post status = %d, want 201", code)
	}
	// Replay of the same idempotency key: 200, books nothing, counts a hit.
	if code := post(entry); code != http.StatusOK {
		t.Fatalf("replay post status = %d, want 200", code)
	}
	// A rejected post still counts as a POST attempt.
	if code := post(`{"debit_account":"x","credit_account":"x","amount_cents":1}`); code != http.StatusBadRequest {
		t.Fatalf("invalid post status = %d, want 400", code)
	}

	if code, _, _ := getText(t, srv.URL+"/accounts/cash/balance"); code != http.StatusOK {
		t.Fatalf("balance status = %d, want 200", code)
	}
	if code, _, _ := getText(t, srv.URL+"/accounts/cash/balance"); code != http.StatusOK {
		t.Fatalf("balance status = %d, want 200", code)
	}
	// Snapshot reads are not balance queries; they must not move the counter.

	code, body, hdr := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("metrics Content-Type = %q, want Prometheus exposition media type", ct)
	}
	m := parseExposition(t, body)
	if m["ledger_posts_total"] != 3 {
		t.Errorf("ledger_posts_total = %d, want 3", m["ledger_posts_total"])
	}
	if m["ledger_idempotency_hits_total"] != 1 {
		t.Errorf("ledger_idempotency_hits_total = %d, want 1", m["ledger_idempotency_hits_total"])
	}
	if m["ledger_balance_queries_total"] != 2 {
		t.Errorf("ledger_balance_queries_total = %d, want 2", m["ledger_balance_queries_total"])
	}
}

func TestMetricsStartAtZero(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, body)
	for _, name := range []string{
		"ledger_posts_total",
		"ledger_idempotency_hits_total",
		"ledger_balance_queries_total",
		"ledger_verify_requests_total",
	} {
		v, ok := m[name]
		if !ok {
			t.Errorf("metric %s missing from exposition output", name)
			continue
		}
		if v != 0 {
			t.Errorf("metric %s = %d on a fresh server, want 0", name, v)
		}
	}
}
