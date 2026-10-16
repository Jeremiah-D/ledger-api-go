package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func postLargeTxReports(t *testing.T, srv *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	resp, err := http.Post(srv.URL+"/compliance/large-tx-reports", "application/json", rdr)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

func TestHandleLargeTxReports(t *testing.T) {
	l := ledger.New(ledger.WithLargeTxThresholds(map[string]int64{"USD": 100000}))
	srv := newServer(l)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Book a reportable flow directly on the ledger.
	if _, _, err := l.Post(ledger.JournalEntry{ID: "e1", DebitAccount: "A", CreditAccount: "B", AmountCents: 150000, Currency: "USD", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	status, out := postLargeTxReports(t, ts, `{"day":"`+time.Now().UTC().Format("2006-01-02")+`"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}
	if out["generated"] != float64(2) {
		t.Fatalf("generated = %v, want 2", out["generated"])
	}
	if out["duplicates"] != float64(0) {
		t.Fatalf("duplicates = %v, want 0", out["duplicates"])
	}
	if got := srv.metrics.LargeTxReportsTotal.Load(); got != 2 {
		t.Fatalf("metric = %d, want 2", got)
	}

	// Regeneration replays: no new reports, metric untouched.
	status, out = postLargeTxReports(t, ts, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}
	if out["generated"] != float64(0) || out["duplicates"] != float64(2) {
		t.Fatalf("replay: generated=%v duplicates=%v", out["generated"], out["duplicates"])
	}
	if got := srv.metrics.LargeTxReportsTotal.Load(); got != 2 {
		t.Fatalf("metric after replay = %d, want 2", got)
	}

	// The metric is exposed in Prometheus exposition format.
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "ledger_large_tx_reports_total 2\n") {
		t.Fatalf("/metrics missing ledger_large_tx_reports_total 2:\n%s", raw)
	}
}

func TestHandleLargeTxReportsBadInput(t *testing.T) {
	l := ledger.New()
	srv := newServer(l)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	if status, _ := postLargeTxReports(t, ts, `{"day":"not-a-day"}`); status != http.StatusBadRequest {
		t.Fatalf("bad day: status = %d, want 400", status)
	}
	if status, _ := postLargeTxReports(t, ts, `{"day":"2026-10-15","bogus":1}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", status)
	}
	if status, _ := postLargeTxReports(t, ts, `{"day":`); status != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status = %d, want 400", status)
	}
	// Trailing data after the JSON object.
	var buf bytes.Buffer
	buf.WriteString(`{"day":"2026-10-15"} {}`)
	if status, _ := postLargeTxReports(t, ts, buf.String()); status != http.StatusBadRequest {
		t.Fatalf("trailing data: status = %d, want 400", status)
	}
	if got := srv.metrics.LargeTxReportsTotal.Load(); got != 0 {
		t.Fatalf("metric = %d, want 0 after bad inputs", got)
	}
}

func TestHandleLargeTxReportsMethodNotAllowed(t *testing.T) {
	l := ledger.New()
	ts := httptest.NewServer(newRouter(l))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/compliance/large-tx-reports")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
