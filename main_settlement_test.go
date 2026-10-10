package main

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newSettlementTestServer seeds a ledger with one merchant sale on
// 2026-10-10 (merch-a debited 3000 USD) and serves it over HTTP.
func newSettlementTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if err := l.SetSettlementChannel("merch-a", "alipay"); err != nil {
		t.Fatalf("SetSettlementChannel: %v", err)
	}
	if _, dup, err := l.Post(ledger.JournalEntry{
		ID: "st-1", DebitAccount: "merch-a", CreditAccount: "clearing",
		AmountCents: 3000, Currency: "USD", IdempotencyKey: "stk-1",
		CreatedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
	}); err != nil || dup {
		t.Fatalf("seed Post(st-1) = dup=%v err=%v", dup, err)
	}
	return httptest.NewServer(newRouter(l))
}

func getSettlement(t *testing.T, srv *httptest.Server, query string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/settlement" + query)
	if err != nil {
		t.Fatalf("GET /settlement%s: %v", query, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw), resp.Header
}

func TestGetSettlementJSON(t *testing.T) {
	srv := newSettlementTestServer(t)
	defer srv.Close()

	code, body, hdr := getSettlement(t, srv, "?day=2026-10-10")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report["day"] != "2026-10-10" {
		t.Fatalf("day = %v", report["day"])
	}
	cells := report["cells"].([]any)
	if len(cells) != 2 { // merch-a + clearing
		t.Fatalf("cells = %d, want 2", len(cells))
	}
	// The alert path: the settlement file says 2900, the ledger says 3000.
	code, body, _ = getSettlement(t, srv, "?day=2026-10-10&expected=merch-a:USD:2900")
	if code != http.StatusOK {
		t.Fatalf("expected query status = %d, body = %s", code, body)
	}
	report = map[string]any{}
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mismatches := report["mismatches"].([]any)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %d, want 1", len(mismatches))
	}
	m := mismatches[0].(map[string]any)
	if m["difference_cents"] != float64(100) || m["channel"] != "alipay" {
		t.Fatalf("mismatch row wrong: %v", m)
	}
	// Within tolerance: no alert, still 200.
	code, body, _ = getSettlement(t, srv, "?day=2026-10-10&expected=merch-a:USD:2900&tolerance=merch-a:USD:100:0")
	if code != http.StatusOK {
		t.Fatalf("tolerance query status = %d, body = %s", code, body)
	}
	report = map[string]any{}
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len(report["mismatches"].([]any)); n != 0 {
		t.Fatalf("mismatches with tolerance = %d, want 0", n)
	}
}

func TestGetSettlementCSVAndSQL(t *testing.T) {
	srv := newSettlementTestServer(t)
	defer srv.Close()

	code, body, hdr := getSettlement(t, srv, "?day=2026-10-10&format=csv")
	if code != http.StatusOK {
		t.Fatalf("csv status = %d, body = %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("csv content-type = %q", ct)
	}
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 3 || records[0][1] != "account" {
		t.Fatalf("csv rows = %d, header = %v", len(records), records[0])
	}

	code, body, hdr = getSettlement(t, srv, "?day=2026-10-10&format=sql")
	if code != http.StatusOK {
		t.Fatalf("sql status = %d, body = %s", code, body)
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("sql content-type = %q", hdr.Get("Content-Type"))
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS settlement_cells",
		"NOT a SQLite .db binary",
		"sqlite3 settlement.db < settlement.sql",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sql body missing %q", want)
		}
	}
}

func TestGetSettlementBadRequests(t *testing.T) {
	srv := newSettlementTestServer(t)
	defer srv.Close()

	for _, q := range []string{
		"?day=2026-13-01",
		"?format=xml",
		"?expected=merch-a:USD",       // missing cents
		"?expected=:USD:100",          // empty account
		"?expected=merch-a:USD:abc",   // non-integer cents
		"?tolerance=merch-a:USD:100",  // missing bps
		"?tolerance=merch-a:USD:-1:0", // negative abs: ledger rejects
		"?tolerance=merch-a:USD:0:10001",
		"?expected=merch-a:USDD:100", // bad currency: ledger rejects
	} {
		code, body, _ := getSettlement(t, srv, q)
		if code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400 (body %s)", q, code, body)
		}
	}
}

func TestSettlementMetrics(t *testing.T) {
	srv := newSettlementTestServer(t)
	defer srv.Close()

	getSettlement(t, srv, "?day=2026-10-10")
	getSettlement(t, srv, "?day=2026-10-10&expected=merch-a:USD:2900") // 1 alert

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"ledger_settlement_runs_total 2",
		"ledger_settlement_alerts_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}
