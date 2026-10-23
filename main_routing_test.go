package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestRouteDecideEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/routing/decide",
		`{"amount_cents":5000,"currency":"USD","strategy":"cost"}`)
	if code != http.StatusOK {
		t.Fatalf("route: status=%d, want 200 (body %v)", code, body)
	}
	if body["rail"] != "ach" || body["reason"] != "cheapest" {
		t.Errorf("route = (%v, %v), want (ach, cheapest)", body["rail"], body["reason"])
	}
	if body["estimated_cost_cents"] != float64(5) {
		t.Errorf("estimated_cost_cents = %v, want 5", body["estimated_cost_cents"])
	}
	if body["feasible"] == nil {
		t.Errorf("decision must carry the feasibility list")
	}

	// An infeasible request returns 422 *with* the decision, so the
	// caller can read the per-rail feasibility list.
	code, body = postJSON(t, srv.URL+"/routing/decide",
		`{"amount_cents":100,"currency":"USD","strategy":"deadline","deadline":"2026-01-01T00:00:00Z"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("infeasible: status=%d, want 422 (body %v)", code, body)
	}
	if body["reason"] != "no-feasible-rail" || body["feasible"] == nil {
		t.Errorf("infeasible body = %v, want no-feasible-rail + feasibility list", body)
	}

	// A bad strategy is 400.
	code, _ = postJSON(t, srv.URL+"/routing/decide",
		`{"amount_cents":100,"strategy":"teleport"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad-strategy: status=%d, want 400", code)
	}
}

func TestFeeEstimateAndRoutingMetrics(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/transfers/fee-estimate",
		`{"from_account":"a","to_account":"b","amount_cents":100}`); code != http.StatusOK {
		t.Fatalf("estimate: status=%d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/routing/decide",
		`{"amount_cents":100,"currency":"USD"}`); code != http.StatusOK {
		t.Fatalf("route: status=%d", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var text strings.Builder
	if _, err := io.Copy(&text, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, want := range []string{
		"ledger_fee_estimates_total",
		"ledger_routing_requests_total",
		`ledger_routing_decisions_total{rail="ach",reason="cheapest"}`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
