package main

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newCashFlowTestServer seeds a ledger with one cash-class account and one
// operating inflow on 2026-10-10, then serves it over HTTP.
func newCashFlowTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if err := l.MarkCashAccount("cash", ""); err != nil {
		t.Fatalf("MarkCashAccount: %v", err)
	}
	if err := l.RegisterAccountType("sales", ledger.AccountTypeRevenue); err != nil {
		t.Fatalf("RegisterAccountType: %v", err)
	}
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "cf-1", DebitAccount: "cash", CreditAccount: "sales",
		AmountCents: 5000, Currency: "USD", IdempotencyKey: "cfk-1",
		CreatedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func getCashFlow(t *testing.T, srv *httptest.Server, query string) (int, string, http.Header) {
	t.Helper()
	return getText(t, srv.URL+"/cash-flow"+query)
}

func TestGetCashFlowJSON(t *testing.T) {
	srv := newCashFlowTestServer(t)
	defer srv.Close()

	code, body, hdr := getCashFlow(t, srv, "?from=2026-10-10&to=2026-10-10&currency=USD")
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
	if report["currency"] != "USD" || report["from"] != "2026-10-10" || report["to"] != "2026-10-10" {
		t.Fatalf("report header = %v", report)
	}
	if report["net_flow_cents"] != float64(5000) {
		t.Fatalf("net_flow_cents = %v, want 5000", report["net_flow_cents"])
	}
	acts := report["activities"].([]any)
	if len(acts) != 3 || acts[0].(map[string]any)["activity"] != "operating" {
		t.Fatalf("activities = %v", acts)
	}
	if acts[0].(map[string]any)["net_cents"] != float64(5000) {
		t.Fatalf("operating net = %v, want 5000", acts[0].(map[string]any)["net_cents"])
	}
	discrepancies := report["discrepancies"].([]any)
	if len(discrepancies) != 0 {
		t.Fatalf("discrepancies = %v, want empty", discrepancies)
	}
}

func TestGetCashFlowEmptyWithoutMarks(t *testing.T) {
	// No cash-class marks: 200 with an empty report, not 404.
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body, _ := getCashFlow(t, srv, "?from=2026-10-10&to=2026-10-10&currency=USD")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report["net_flow_cents"] != float64(0) {
		t.Fatalf("net_flow_cents = %v, want 0", report["net_flow_cents"])
	}
}

func TestGetCashFlowBadRange(t *testing.T) {
	srv := newCashFlowTestServer(t)
	defer srv.Close()

	for _, q := range []string{
		"?from=2026-10-11&to=2026-10-10&currency=USD",            // from after to
		"?from=2026-13-01&to=2026-10-10&currency=USD",            // bad from
		"?from=2026-10-10&to=2026-10-10&currency=",               // empty currency
		"?from=2026-10-10&to=2026-10-10&currency=usd",            // bad currency
		"?from=2026-10-10&to=2026-10-10&currency=USD&format=xml", // bad format
	} {
		code, body, _ := getCashFlow(t, srv, q)
		if code != http.StatusBadRequest {
			t.Errorf("GET /cash-flow%s: status = %d, want 400 (body %s)", q, code, body)
		}
	}
}

func TestGetCashFlowCSV(t *testing.T) {
	srv := newCashFlowTestServer(t)
	defer srv.Close()

	code, body, hdr := getCashFlow(t, srv, "?from=2026-10-10&to=2026-10-10&currency=USD&format=csv")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 4 { // header + 1 account x 3 activities
		t.Fatalf("csv rows = %d, want 4", len(rows))
	}
	if rows[1][3] != "cash" || rows[1][4] != "operating" || rows[1][5] != "5000" {
		t.Fatalf("csv first data row = %v", rows[1])
	}
}

func TestGetCashFlowMetrics(t *testing.T) {
	srv := newCashFlowTestServer(t)
	defer srv.Close()

	if code, _, _ := getCashFlow(t, srv, "?from=2026-10-10&to=2026-10-10&currency=USD"); code != http.StatusOK {
		t.Fatalf("cash-flow status = %d", code)
	}
	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d", code)
	}
	m := parseExposition(t, body)
	if m["ledger_cash_flow_runs_total"] != 1 {
		t.Errorf("ledger_cash_flow_runs_total = %d, want 1", m["ledger_cash_flow_runs_total"])
	}
}
