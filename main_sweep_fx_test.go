package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// sweepFXTestServer builds a server whose ledger has an FX clearing
// account and a USD->EUR rate.
func sweepFXTestServer() *httptest.Server {
	l := ledger.New(
		ledger.WithFXAccount("fx-clearing"),
		ledger.WithFXRateOption("USD", "EUR", 108, 100),
	)
	return httptest.NewServer(newRouter(l))
}

func TestSweepFXLifecycle(t *testing.T) {
	srv := sweepFXTestServer()
	defer srv.Close()

	// Fund the sub-accounts: sub-a in USD, sub-b in EUR (same as the
	// sweep's target currency).
	for _, f := range []struct{ acct, ccy string }{
		{"sub-a", "USD"}, {"sub-b", "EUR"},
	} {
		code, _ := postJSON(t, srv.URL+"/entries",
			`{"debit_account":"`+f.acct+`","credit_account":"funding","amount_cents":10000,"currency":"`+f.ccy+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("funding %s: status=%d, want 201", f.acct, code)
		}
	}

	// First sweep: 201 with the receipt; the FX disclosure carries the
	// conversion behind each leg.
	code, body := postJSON(t, srv.URL+"/sweeps/fx",
		`{"sweep_id":"fxsweep-1","from_accounts":["sub-a","sub-b"],"to_account":"treasury","to_currency":"EUR","idempotency_key":"fxsk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("sweep_fx: status=%d, want 201 (body=%v)", code, body)
	}
	if body["sweep_id"] != "fxsweep-1" || body["to_currency"] != "EUR" || body["fx_account"] != "fx-clearing" {
		t.Errorf("receipt head = %v, want fxsweep-1/EUR/fx-clearing", body)
	}
	fx, ok := body["fx"].([]any)
	if !ok || len(fx) != 2 {
		t.Fatalf("fx = %v, want two leg disclosures", body["fx"])
	}
	leg0 := fx[0].(map[string]any)
	if leg0["from_currency"] != "USD" || leg0["to_currency"] != "EUR" ||
		leg0["source_cents"] != float64(10000) || leg0["converted_cents"] != float64(10800) {
		t.Errorf("fx leg 0 = %v, want USD->EUR 10000->10800", leg0)
	}
	legs := body["legs"].([]any)
	leg1 := legs[1].(map[string]any)
	if leg1["source_currency"] != "EUR" || leg1["converted_cents"] != float64(10000) {
		t.Errorf("leg 1 = %v, want same-currency EUR", leg1)
	}

	// Replay of the same idempotency key: 200, original receipt, the FX
	// disclosure omitted (the rate table may have moved since).
	code, body = postJSON(t, srv.URL+"/sweeps/fx",
		`{"sweep_id":"fxsweep-1-retry","from_accounts":["sub-a"],"to_account":"treasury","to_currency":"EUR","idempotency_key":"fxsk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay sweep_fx: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true || body["sweep_id"] != "fxsweep-1" {
		t.Errorf("replay = %v, want duplicate of fxsweep-1", body)
	}
	if _, ok := body["fx"]; ok {
		t.Errorf("replay fx = %v, want omitted", body["fx"])
	}

	// Balances moved exactly once: USD converted, EUR swept same-currency.
	// (The trial-balance endpoint reports the per-currency breakdown.)
	code, body = getJSON(t, srv.URL+"/accounts/treasury/trial-balance")
	if code != http.StatusOK {
		t.Fatalf("trial-balance: status=%d", code)
	}
	var eurNet float64 = -1
	for _, row := range body["by_currency"].([]any) {
		r := row.(map[string]any)
		if r["currency"] == "EUR" {
			eurNet = r["net_balance_cents"].(float64)
		}
	}
	if eurNet != 20800 {
		t.Errorf("treasury EUR net = %v, want 20800", eurNet)
	}

	// Metrics carry the new counters.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	text := string(buf[:n])
	if !strings.Contains(text, "ledger_sweep_fx_total") ||
		!strings.Contains(text, "ledger_sweep_fx_idempotency_hits_total") {
		t.Error("/metrics missing ledger_sweep_fx_total / ledger_sweep_fx_idempotency_hits_total")
	}
}

func TestSweepFXMissingRate(t *testing.T) {
	srv := sweepFXTestServer()
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub-jpy","credit_account":"funding","amount_cents":10000,"currency":"JPY"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding sub-jpy: status=%d, want 201", code)
	}

	// No JPY->EUR rate configured: 422, nothing recorded.
	code, body := postJSON(t, srv.URL+"/sweeps/fx",
		`{"sweep_id":"fxsweep-jpy","from_accounts":["sub-jpy"],"to_account":"treasury","to_currency":"EUR"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("sweep_fx missing rate: status=%d, want 422 (body=%v)", code, body)
	}
}

func TestSweepFXFrozenRejected(t *testing.T) {
	srv := sweepFXTestServer()
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub-frz","credit_account":"funding","amount_cents":10000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding sub-frz: status=%d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/accounts/sub-frz/freeze", `{}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("freeze: status=%d", code)
	}

	code, _ = postJSON(t, srv.URL+"/sweeps/fx",
		`{"sweep_id":"fxsweep-frz","from_accounts":["sub-frz"],"to_account":"treasury","to_currency":"EUR"}`)
	if code != http.StatusForbidden {
		t.Fatalf("sweep_fx frozen source: status=%d, want 403", code)
	}
}
