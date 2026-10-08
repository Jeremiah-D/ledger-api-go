package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSweepLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund two sub-accounts through the raw entries endpoint first.
	for _, acct := range []string{"sub-a", "sub-b"} {
		code, _ := postJSON(t, srv.URL+"/entries",
			`{"debit_account":"`+acct+`","credit_account":"funding","amount_cents":10000}`)
		if code != http.StatusCreated {
			t.Fatalf("funding %s: status=%d, want 201", acct, code)
		}
	}

	// First sweep: 201 with the receipt; legs carry the sweep-ID prefix.
	code, body := postJSON(t, srv.URL+"/sweeps",
		`{"sweep_id":"sweep-1","from_accounts":["sub-a","sub-b"],"to_account":"treasury","idempotency_key":"sk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("sweep: status=%d, want 201 (body=%v)", code, body)
	}
	if body["sweep_id"] != "sweep-1" {
		t.Errorf("sweep_id = %v, want sweep-1", body["sweep_id"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %v, want exactly two sweep legs", body["entries"])
	}
	e := entries[0].(map[string]any)
	if e["id"] != "sweep-1/sub-a/USD" || e["debit_account"] != "treasury" || e["credit_account"] != "sub-a" {
		t.Errorf("leg entry = %v, want id sweep-1/sub-a/USD debit treasury credit sub-a", e)
	}

	// Replay of the same idempotency key: 200, original receipt.
	code, body = postJSON(t, srv.URL+"/sweeps",
		`{"sweep_id":"sweep-1-retry","from_accounts":["sub-a"],"to_account":"treasury","idempotency_key":"sk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay sweep: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}

	// Balances moved exactly once.
	_, body = getJSON(t, srv.URL+"/accounts/treasury/balance")
	if body["balance_cents"] != float64(20000) {
		t.Errorf("treasury balance = %v, want 20000", body["balance_cents"])
	}

	// Metrics counted the attempts and the idempotency hit.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_sweeps_total"] != 2 {
		t.Errorf("ledger_sweeps_total = %d, want 2", m["ledger_sweeps_total"])
	}
	if m["ledger_sweep_idempotency_hits_total"] != 1 {
		t.Errorf("ledger_sweep_idempotency_hits_total = %d, want 1", m["ledger_sweep_idempotency_hits_total"])
	}
}

func TestSweepFrozenSource(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub-a","credit_account":"funding","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/accounts/sub-a/freeze", `{}`)
	if code != http.StatusOK {
		t.Fatalf("freeze: status=%d, want 200", code)
	}

	code, body := postJSON(t, srv.URL+"/sweeps",
		`{"from_accounts":["sub-a"],"to_account":"treasury"}`)
	if code != http.StatusForbidden {
		t.Errorf("sweep through frozen source: status=%d, want 403 (body=%v)", code, body)
	}
}

func TestSweepValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// No sources.
	code, _ := postJSON(t, srv.URL+"/sweeps",
		`{"to_account":"treasury"}`)
	if code != http.StatusBadRequest {
		t.Errorf("sweep without sources: status=%d, want 400", code)
	}
	// Target among the sources.
	code, _ = postJSON(t, srv.URL+"/sweeps",
		`{"from_accounts":["treasury"],"to_account":"treasury"}`)
	if code != http.StatusBadRequest {
		t.Errorf("sweep with target as source: status=%d, want 400", code)
	}
}
