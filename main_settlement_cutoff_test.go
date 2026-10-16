package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newCutoffTestServer serves a ledger with a USD settlement cutoff.
func newCutoffTestServer(t *testing.T, hhmm, timezone string) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if err := l.SetSettlementCutoff("USD", hhmm, timezone); err != nil {
		t.Fatalf("SetSettlementCutoff: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestGetSettlementNextWindow(t *testing.T) {
	srv := newCutoffTestServer(t, "17:00", "")
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/settlement/next-window?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["currency"] != "USD" || body["cutoff"] != "17:00" || body["timezone"] != "UTC" {
		t.Fatalf("body = %v, want currency=USD cutoff=17:00 timezone=UTC", body)
	}
	win, ok := body["next_window"].(string)
	if !ok || !strings.HasSuffix(win, "Z") && !strings.Contains(win, "+") {
		t.Fatalf("next_window = %v, want RFC3339", body["next_window"])
	}
	if _, ok := body["is_today"].(bool); !ok {
		t.Fatalf("is_today = %v, want bool", body["is_today"])
	}

	// Missing currency is 400.
	if code, _ := getJSON(t, srv.URL+"/settlement/next-window"); code != http.StatusBadRequest {
		t.Fatalf("missing currency status = %d, want 400", code)
	}
	// An invalid currency code is 400.
	if code, _ := getJSON(t, srv.URL+"/settlement/next-window?currency=US"); code != http.StatusBadRequest {
		t.Fatalf("bad currency status = %d, want 400", code)
	}
	// A currency with no configured cutoff is 404: there is no window
	// to report.
	if code, body := getJSON(t, srv.URL+"/settlement/next-window?currency=EUR"); code != http.StatusNotFound {
		t.Fatalf("unconfigured currency status = %d (%v), want 404", code, body)
	}
}

func TestSettlementDeferralHTTPFlow(t *testing.T) {
	// Cutoff 00:00 UTC: every submission lands after the cutoff, so the
	// flow is deterministic without controlling the wall clock.
	srv := newCutoffTestServer(t, "00:00", "")
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-cut-1","from_account":"alice","to_account":"bob","amount_cents":2500,"currency":"USD","idempotency_key":"tk-cut-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["settlement_status"] != "scheduled" {
		t.Fatalf("settlement_status = %v, want scheduled", body["settlement_status"])
	}
	window, ok := body["settlement_window"].(string)
	if !ok || window == "" {
		t.Fatalf("settlement_window = %v, want the next window", body["settlement_window"])
	}

	// The operator lists the deferred transfer.
	code, body = getJSON(t, srv.URL+"/settlement/deferred?status=scheduled")
	if code != http.StatusOK {
		t.Fatalf("list deferred: status=%d (%v)", code, body)
	}
	rows := body["deferrals"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["transfer_id"] != "tx-cut-1" {
		t.Fatalf("deferrals = %v, want [tx-cut-1]", rows)
	}
	// An invalid status filter is 400.
	if code, _ = getJSON(t, srv.URL+"/settlement/deferred?status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d, want 400", code)
	}

	// Advancing at the window moves scheduled -> pending.
	code, body = postJSON(t, srv.URL+"/settlement/advance", `{"now":`+`"`+window+`"}`)
	if code != http.StatusOK {
		t.Fatalf("advance: status=%d (%v)", code, body)
	}
	if body["advanced_count"] != float64(1) {
		t.Fatalf("advanced_count = %v, want 1", body["advanced_count"])
	}
	// Re-advancing is idempotent.
	code, body = postJSON(t, srv.URL+"/settlement/advance", `{"now":`+`"`+window+`"}`)
	if code != http.StatusOK || body["advanced_count"] != float64(0) {
		t.Fatalf("re-advance = %d (%v), want 200 with count 0", code, body)
	}

	// Settling a pending deferral: the operator confirms the run.
	code, body = postJSONWithActor(t, srv.URL+"/settlement/deferred/tx-cut-1/settle", `{}`, "ops-chen")
	if code != http.StatusOK {
		t.Fatalf("settle: status=%d (%v)", code, body)
	}
	if body["status"] != "settled" {
		t.Fatalf("settle status = %v, want settled", body["status"])
	}
	// Settling twice is a 409 conflict.
	if code, _ = postJSONWithActor(t, srv.URL+"/settlement/deferred/tx-cut-1/settle", `{}`, "ops-chen"); code != http.StatusConflict {
		t.Fatalf("double settle = %d, want 409", code)
	}
	// Unknown transfer is 404.
	if code, _ = postJSONWithActor(t, srv.URL+"/settlement/deferred/nope/settle", `{}`, "ops"); code != http.StatusNotFound {
		t.Fatalf("unknown settle = %d, want 404", code)
	}

	// The settled history is still listed.
	code, body = getJSON(t, srv.URL+"/settlement/deferred?status=settled")
	if code != http.StatusOK {
		t.Fatalf("list settled: status=%d (%v)", code, body)
	}
	if rows := body["deferrals"].([]any); len(rows) != 1 {
		t.Fatalf("settled deferrals = %v, want 1", rows)
	}

	// The /metrics scrape carries the deferral counter.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "ledger_settlement_deferred_total 1") {
		t.Fatalf("/metrics missing ledger_settlement_deferred_total 1")
	}
	if !strings.Contains(string(raw), "ledger_settlement_next_window_queries_total") {
		t.Fatalf("/metrics missing ledger_settlement_next_window_queries_total")
	}
}
