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

// TestIntradayLiquidityEndpoint drives GET /liquidity/intraday and checks
// the read-only forecast shape plus date validation.
func TestIntradayLiquidityEndpoint(t *testing.T) {
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{ID: "seed", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	today := time.Now().UTC().Format("2006-01-02")
	resp, err := http.Get(srv.URL + "/liquidity/intraday?date=" + today)
	if err != nil {
		t.Fatalf("GET /liquidity/intraday: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"date":"` + today + `"`, `"account":"cust"`, `"booked_inflow_cents":10000`, `"net_position_cents":10000`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("response missing %s:\n%s", want, body)
		}
	}

	// Absent date defaults to today.
	resp2, err := http.Get(srv.URL + "/liquidity/intraday")
	if err != nil {
		t.Fatalf("GET /liquidity/intraday (no date): %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body2), `"date":"`+today+`"`) {
		t.Errorf("no-date status = %d, want 200 with today's date; body: %s", resp2.StatusCode, body2)
	}

	// Malformed date is a 400.
	resp3, err := http.Get(srv.URL + "/liquidity/intraday?date=not-a-date")
	if err != nil {
		t.Fatalf("GET /liquidity/intraday (bad date): %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-date status = %d, want 400", resp3.StatusCode)
	}
}

// TestMetricsLowLiquidityAlerts checks that the alert counter surfaces on
// GET /metrics after an evaluation fires.
func TestMetricsLowLiquidityAlerts(t *testing.T) {
	l := ledger.New()
	if err := l.SetLiquidityAlertThreshold("cust", "USD", 50000); err != nil {
		t.Fatalf("SetLiquidityAlertThreshold: %v", err)
	}
	// No flows today: net 0 < 50000 → one alert on evaluation.
	if alerts := l.EvaluateLiquidityAlerts(time.Now()); len(alerts) != 1 {
		t.Fatalf("EvaluateLiquidityAlerts fired %d, want 1", len(alerts))
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ledger_low_liquidity_alerts_total 1\n") {
		t.Fatalf("metrics missing ledger_low_liquidity_alerts_total 1:\n%s", body)
	}
}
