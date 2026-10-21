package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// LG-156: rolling velocity limits end-to-end.

func doJSON(t *testing.T, method, url, body string) (int, http.Header, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, out
}

func TestVelocityLimitHTTPCRUD(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Set a limit.
	code, _, body := doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"1h","max_count":2,"max_amount_cents":100000}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d (%v)", code, body)
	}
	if body["window"] != "1h0m0s" || body["max_count"] != float64(2) {
		t.Fatalf("PUT response = %v", body)
	}

	// List.
	code, _, body = doJSON(t, http.MethodGet, srv.URL+"/velocity-limits", "")
	if code != http.StatusOK {
		t.Fatalf("GET list: status = %d", code)
	}
	limits, ok := body["velocity_limits"].([]any)
	if !ok || len(limits) != 1 {
		t.Fatalf("list = %v", body)
	}

	// Invalid config is a 400, never half-applied.
	code, _, body = doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"1h","max_count":0,"max_amount_cents":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid PUT: status = %d, want 400 (%v)", code, body)
	}
	code, _, _ = doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"not-a-duration","max_count":2}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad window: status = %d, want 400", code)
	}

	// Usage: 404 before any post, 200 after.
	code, _, _ = doJSON(t, http.MethodGet, srv.URL+"/velocity-limits/usage?account=payer&currency=USD&direction=outflow", "")
	if code != http.StatusOK {
		t.Fatalf("usage: status = %d, want 200", code)
	}

	// Delete.
	code, _, _ = doJSON(t, http.MethodDelete, srv.URL+"/velocity-limits?account=payer&currency=USD", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE: status = %d", code)
	}
	code, _, _ = doJSON(t, http.MethodGet, srv.URL+"/velocity-limits/usage?account=payer", "")
	if code != http.StatusNotFound {
		t.Fatalf("usage after clear: status = %d, want 404", code)
	}
}

func TestVelocityLimitHTTPPostRejected(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, _, _ := doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"24h","max_count":1,"max_amount_cents":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d", code)
	}
	post := func(key string) int {
		c, _, _ := doJSON(t, http.MethodPost, srv.URL+"/entries",
			`{"debit_account":"cash","credit_account":"payer","amount_cents":10,"idempotency_key":"`+key+`"}`)
		return c
	}
	if c := post("vel-k1"); c != http.StatusCreated {
		t.Fatalf("first post: status = %d", c)
	}
	if c := post("vel-k2"); c != http.StatusUnprocessableEntity {
		t.Fatalf("second post: status = %d, want 422", c)
	}
	// The rejection counter is exported on /metrics.
	_, _, mbody := getRaw(t, srv.URL+"/metrics", "")
	if !strings.Contains(string(mbody), "ledger_velocity_limit_rejections_total 1") {
		t.Fatalf("/metrics missing velocity rejection counter:\n%s", mbody)
	}
}

func TestVelocityLimitHTTPTransferRejected(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Seed the payer with funds (unlimited), then cap outflow.
	code, _, _ := doJSON(t, http.MethodPost, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":100000,"idempotency_key":"vseed-k1"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed: status = %d", code)
	}
	code, _, _ = doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"24h","max_count":0,"max_amount_cents":50000}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d", code)
	}
	code, _, _ = doJSON(t, http.MethodPost, srv.URL+"/transfers",
		`{"transfer_id":"vt-1","from_account":"payer","to_account":"payee","amount_cents":60000,"idempotency_key":"vt-k1"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized transfer: status = %d, want 422", code)
	}
	code, _, _ = doJSON(t, http.MethodPost, srv.URL+"/transfers",
		`{"transfer_id":"vt-2","from_account":"payer","to_account":"payee","amount_cents":40000,"idempotency_key":"vt-k2"}`)
	if code != http.StatusCreated {
		t.Fatalf("fitting transfer: status = %d, want 201", code)
	}
}

func TestVelocityLimitReconcileListsLimits(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, _, _ := doJSON(t, http.MethodPut, srv.URL+"/velocity-limits",
		`{"account":"payer","currency":"USD","window":"10m","max_count":50,"max_amount_cents":100000}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d", code)
	}
	code, _, body := doJSON(t, http.MethodPost, srv.URL+"/reconcile", "")
	if code != http.StatusOK {
		t.Fatalf("reconcile: status = %d", code)
	}
	limits, ok := body["velocity_limits"].([]any)
	if !ok || len(limits) != 1 {
		t.Fatalf("reconcile velocity_limits = %v", body["velocity_limits"])
	}
	row := limits[0].(map[string]any)
	if row["account"] != "payer" || row["window"] != float64(600000000000) {
		t.Fatalf("reconcile row = %v", row)
	}
}
