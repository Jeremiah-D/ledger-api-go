package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSweepReserveHTTPLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// PUT a reserve: 200 with the floor echoed.
	code, body := putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"USD","floor_cents":3000}`)
	if code != http.StatusOK {
		t.Fatalf("PUT reserve: status=%d, want 200 (body=%v)", code, body)
	}
	if body["account"] != "sub-a" || body["floor_cents"] != float64(3000) {
		t.Errorf("PUT response = %v, want account sub-a floor 3000", body)
	}

	// GET lists it.
	code, body = getJSON(t, srv.URL+"/sweep-reserves")
	if code != http.StatusOK {
		t.Fatalf("GET reserves: status=%d, want 200 (body=%v)", code, body)
	}
	rows, ok := body["sweep_reserves"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("sweep_reserves = %v, want one row", body["sweep_reserves"])
	}
	row := rows[0].(map[string]any)
	if row["account"] != "sub-a" || row["currency"] != "USD" || row["floor_cents"] != float64(3000) {
		t.Errorf("row = %v, want sub-a/USD/3000", row)
	}

	// GET with a non-matching filter: empty list, still 200.
	code, body = getJSON(t, srv.URL+"/sweep-reserves?account=sub-b")
	if code != http.StatusOK {
		t.Fatalf("GET filtered: status=%d, want 200 (body=%v)", code, body)
	}
	if rows := body["sweep_reserves"].([]any); len(rows) != 0 {
		t.Errorf("filtered rows = %d, want 0", len(rows))
	}

	// DELETE clears it.
	code, _ = deleteJSON(t, srv.URL+"/sweep-reserves?account=sub-a&currency=USD")
	if code != http.StatusOK {
		t.Fatalf("DELETE reserve: status=%d, want 200", code)
	}
	code, body = getJSON(t, srv.URL+"/sweep-reserves")
	if code != http.StatusOK {
		t.Fatalf("GET after delete: status=%d, want 200 (body=%v)", code, body)
	}
	if rows := body["sweep_reserves"].([]any); len(rows) != 0 {
		t.Errorf("rows after delete = %d, want 0", len(rows))
	}
}

func TestSweepReserveHTTPValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Negative floor: 400.
	code, _ := putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"USD","floor_cents":-1}`)
	if code != http.StatusBadRequest {
		t.Errorf("negative floor: status=%d, want 400", code)
	}
	// Zero floor: 400 (use DELETE to clear).
	code, _ = putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"USD","floor_cents":0}`)
	if code != http.StatusBadRequest {
		t.Errorf("zero floor: status=%d, want 400", code)
	}
	// Missing account: 400.
	code, _ = putJSON(t, srv.URL+"/sweep-reserves",
		`{"currency":"USD","floor_cents":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Unknown JSON field: 400 (strict decoding).
	code, _ = putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"USD","floor_cents":100,"bogus":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// Malformed JSON: 400.
	code, _ = putJSON(t, srv.URL+"/sweep-reserves", `{"account":`)
	if code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Bad currency: 400.
	code, _ = putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"US","floor_cents":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad currency: status=%d, want 400", code)
	}
	// DELETE without ?account=: 400.
	code, _ = deleteJSON(t, srv.URL+"/sweep-reserves")
	if code != http.StatusBadRequest {
		t.Errorf("DELETE without account: status=%d, want 400", code)
	}
}

func TestSweepReserveEndToEnd(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub-a","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}
	code, _ = putJSON(t, srv.URL+"/sweep-reserves",
		`{"account":"sub-a","currency":"USD","floor_cents":2500}`)
	if code != http.StatusOK {
		t.Fatalf("PUT reserve: status=%d, want 200", code)
	}

	code, body := postJSON(t, srv.URL+"/sweeps",
		`{"sweep_id":"sweep-e2e","from_accounts":["sub-a"],"to_account":"treasury"}`)
	if code != http.StatusCreated {
		t.Fatalf("sweep: status=%d, want 201 (body=%v)", code, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].(map[string]any)["amount_cents"] != float64(7500) {
		t.Errorf("swept amount = %v, want 7500", entries[0].(map[string]any)["amount_cents"])
	}

	// The floor stays behind.
	_, body = getJSON(t, srv.URL+"/accounts/sub-a/balance")
	if body["balance_cents"] != float64(2500) {
		t.Errorf("sub-a balance = %v, want 2500", body["balance_cents"])
	}

	// The reconcile report discloses the reserve.
	code, body = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d, want 200", code)
	}
	rows, ok := body["sweep_reserves"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("reconcile sweep_reserves = %v, want one row", body["sweep_reserves"])
	}
}
