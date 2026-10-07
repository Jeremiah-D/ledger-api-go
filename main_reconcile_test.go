package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// POST /reconcile on the seeded server: 200 with a full reconciliation
// report in the body. The seed posts cash→equity 100 three times, so the
// books are healthy: equation ok, no discrepancies, two trial balances
// (cash and equity).
func TestHandleReconcile(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/reconcile", nil)
	if err != nil {
		t.Fatalf("build POST /reconcile: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /reconcile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /reconcile body: %v", err)
	}

	if body["accounting_equation_ok"] != true {
		t.Errorf("accounting_equation_ok = %v, want true", body["accounting_equation_ok"])
	}
	if _, ok := body["accounting_error"]; ok {
		t.Errorf("accounting_error present on healthy ledger: %v", body["accounting_error"])
	}
	if body["version"] != float64(3) {
		t.Errorf("version = %v, want 3", body["version"])
	}
	if body["total_debits_cents"] != float64(300) || body["total_credits_cents"] != float64(300) {
		t.Errorf("totals = %v/%v, want 300/300", body["total_debits_cents"], body["total_credits_cents"])
	}

	// The seed posts debit cash / credit equity, so exactly those two
	// accounts appear, sorted.
	tbs, ok := body["trial_balances"].([]any)
	if !ok {
		t.Fatalf("trial_balances missing or not a list: %v", body["trial_balances"])
	}
	if len(tbs) != 2 {
		t.Fatalf("trial_balances = %d entries, want 2 (cash, equity)", len(tbs))
	}
	first, _ := tbs[0].(map[string]any)["account"]
	second, _ := tbs[1].(map[string]any)["account"]
	if first != "cash" || second != "equity" {
		t.Errorf("trial balance order = [%v %v], want [cash equity]", first, second)
	}

	disc, ok := body["discrepancies"].([]any)
	if !ok {
		t.Fatalf("discrepancies missing or not a list: %v", body["discrepancies"])
	}
	if len(disc) != 0 {
		t.Errorf("discrepancies = %v, want empty on healthy ledger", disc)
	}

	chain, ok := body["audit_chain"].(map[string]any)
	if !ok {
		t.Fatalf("audit_chain missing or not an object: %v", body["audit_chain"])
	}
	if chain["verify_ok"] != true || chain["links"] != float64(3) || chain["head_consistent"] != true {
		t.Errorf("audit_chain = %v, want verify_ok links=3 head_consistent", chain)
	}

	keys, ok := body["idempotency_keys"].(map[string]any)
	if !ok {
		t.Fatalf("idempotency_keys missing or not an object: %v", body["idempotency_keys"])
	}
	if keys["ttl_configured"] != false || keys["total_keys"] != float64(3) {
		t.Errorf("idempotency_keys = %v, want ttl_configured=false total_keys=3", keys)
	}
}

// GET on the operator endpoint is rejected; /reconcile is POST-only. The
// method+path mux pattern answers 405 itself (its own plain-text body,
// same as GET /entries), so the handler's in-function method guard is
// defensive and never reached through the router.
func TestHandleReconcileMethodNotAllowed(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/reconcile")
	if err != nil {
		t.Fatalf("GET /reconcile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// Each POST /reconcile bumps ledger_reconcile_runs_total on /metrics.
func TestHandleReconcileCounter(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	post := func() {
		resp, err := http.Post(srv.URL+"/reconcile", "application/json", nil)
		if err != nil {
			t.Fatalf("POST /reconcile: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	post()
	post()

	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, body)
	if m["ledger_reconcile_runs_total"] != 2 {
		t.Errorf("ledger_reconcile_runs_total = %d, want 2", m["ledger_reconcile_runs_total"])
	}
}
