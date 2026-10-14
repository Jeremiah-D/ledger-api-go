package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newTSAHTTPServer builds a server whose ledger has an audit log (the
// durable home of tsa_anchors.jsonl) and a configured TSA URL, seeded
// with 2 journal entries. The TSA URL points at deadURL by default;
// pass a live test TSA URL for the happy path.
func newTSAHTTPServer(t *testing.T, tsaURL string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithTSAURL(tsaURL))
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e := ledger.JournalEntry{
			ID:             "e-tsa-http-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "key-tsa-http-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleTSAAnchorUnconfigured(t *testing.T) {
	// No TSA URL: the operator misconfigured the deployment → 500, and
	// the tsa section of /entries/verify reports configured:false.
	srv := newTSAHTTPServer(t, "")

	code, body := postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("tsa-anchor status = %d body=%v, want 500", code, body)
	}

	code, body = getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	tsa, ok := body["tsa"].(map[string]any)
	if !ok {
		t.Fatalf("verify has no tsa section: %v", body)
	}
	if tsa["configured"] != false {
		t.Errorf("tsa.configured = %v, want false", tsa["configured"])
	}
}

func TestHandleTSAAnchorTSADown(t *testing.T) {
	// TSA URL configured but the authority is unreachable: 502, and the
	// ledger records no anchor.
	srv := newTSAHTTPServer(t, "http://127.0.0.1:1/tsa")

	code, body := postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusBadGateway {
		t.Fatalf("tsa-anchor status = %d body=%v, want 502", code, body)
	}

	code, body = getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	tsa := body["tsa"].(map[string]any)
	if tsa["configured"] != true {
		t.Errorf("tsa.configured = %v, want true", tsa["configured"])
	}
	if n, _ := tsa["anchors"].(float64); n != 0 {
		t.Errorf("tsa.anchors = %v, want 0 after failed anchor", n)
	}
}

func TestMetricsExposeTSASeries(t *testing.T) {
	srv := newTSAHTTPServer(t, "http://127.0.0.1:1/tsa")
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	metrics := string(raw)
	for _, name := range []string{"ledger_tsa_anchors_total", "ledger_tsa_anchor_failures_total"} {
		if !strings.Contains(metrics, name) {
			t.Errorf("/metrics missing series %s", name)
		}
	}
}
