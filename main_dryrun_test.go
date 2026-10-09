package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestDryRunEntryEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":500}`)
	if code != http.StatusOK {
		t.Fatalf("dry-run: status=%d, want 200", code)
	}
	if body["would_succeed"] != true || body["duplicate"] != false {
		t.Fatalf("dry-run body = %v, want would_succeed=true duplicate=false", body)
	}
	if body["version_before"] != float64(0) || body["version_after"] != float64(1) {
		t.Fatalf("dry-run version bracket = %v/%v, want 0/1", body["version_before"], body["version_after"])
	}
	legs, ok := body["legs"].([]any)
	if !ok || len(legs) != 1 {
		t.Fatalf("dry-run legs = %v, want 1 leg", body["legs"])
	}
	leg := legs[0].(map[string]any)
	if leg["debit_balance_before_cents"] != float64(0) || leg["debit_balance_after_cents"] != float64(500) {
		t.Fatalf("dry-run debit leg = %v, want 0->500", leg)
	}

	// The dry run booked nothing: no balance, no version movement.
	_, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if body["balance_cents"] != float64(0) {
		t.Fatalf("cash balance after dry run = %v, want 0", body["balance_cents"])
	}

	// The real call still works and lands exactly what the dry run
	// predicted.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":500}`)
	if code != http.StatusCreated {
		t.Fatalf("real post: status=%d, want 201", code)
	}
	_, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if body["balance_cents"] != float64(500) {
		t.Fatalf("cash balance after real post = %v, want 500", body["balance_cents"])
	}

	// A dry run that would fail returns the real call's status: frozen
	// account -> 403.
	if code, _ := postJSON(t, srv.URL+"/accounts/cash/freeze", ""); code != http.StatusOK {
		t.Fatalf("freeze: status=%d", code)
	}
	code, _ = postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":500}`)
	if code != http.StatusForbidden {
		t.Fatalf("dry-run through frozen account: status=%d, want 403", code)
	}
	// Bad bodies still fail 400 without touching the ledger.
	code, _ = postJSON(t, srv.URL+"/entries/dry-run", `{"debit_account":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("dry-run bad body: status=%d, want 400", code)
	}
}

func TestDryRunEntryIntoClosedPeriod(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// The dry run reports the same 422 the real call would return.
	current := time.Now().UTC().Format("2006-01")
	if code, _ := postJSON(t, srv.URL+"/periods/"+current+"/close", ""); code != http.StatusOK {
		t.Fatalf("close: status=%d", code)
	}
	code, _ := postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("dry-run into closed period: status=%d, want 422", code)
	}
	if code, _ := postJSON(t, srv.URL+"/periods/"+current+"/reopen", ""); code != http.StatusOK {
		t.Fatalf("reopen: status=%d", code)
	}
}

func TestDryRunTransferAndSweepEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Seed balances through the real endpoints.
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"equity","amount_cents":10000}`); code != http.StatusCreated {
		t.Fatalf("seed payer: status=%d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub","credit_account":"equity","amount_cents":300}`); code != http.StatusCreated {
		t.Fatalf("seed sub: status=%d", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers/dry-run",
		`{"from_account":"payer","to_account":"payee","amount_cents":4000}`)
	if code != http.StatusOK {
		t.Fatalf("transfer dry-run: status=%d, want 200", code)
	}
	if body["would_succeed"] != true {
		t.Fatalf("transfer dry-run body = %v, want would_succeed=true", body)
	}
	legs := body["legs"].([]any)
	if len(legs) != 1 {
		t.Fatalf("transfer dry-run legs = %d, want 1 (no fee policy on the test server)", len(legs))
	}

	code, body = postJSON(t, srv.URL+"/sweeps/dry-run",
		`{"from_accounts":["sub"],"to_account":"treasury"}`)
	if code != http.StatusOK {
		t.Fatalf("sweep dry-run: status=%d, want 200", code)
	}
	if body["would_succeed"] != true || body["version_after"] != float64(3) {
		t.Fatalf("sweep dry-run body = %v, want would_succeed=true version_after=3", body)
	}

	// Nothing booked: payee and treasury are still empty.
	_, body = getJSON(t, srv.URL+"/accounts/payee/balance")
	if body["balance_cents"] != float64(0) {
		t.Fatalf("payee balance after dry runs = %v, want 0", body["balance_cents"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/treasury/balance")
	if body["balance_cents"] != float64(0) {
		t.Fatalf("treasury balance after dry runs = %v, want 0", body["balance_cents"])
	}

	// A dry-run sweep naming a frozen source fails like the real call.
	if code, _ := postJSON(t, srv.URL+"/accounts/sub/freeze", ""); code != http.StatusOK {
		t.Fatalf("freeze: status=%d", code)
	}
	code, _ = postJSON(t, srv.URL+"/sweeps/dry-run",
		`{"from_accounts":["sub"],"to_account":"treasury"}`)
	if code != http.StatusForbidden {
		t.Fatalf("sweep dry-run through frozen source: status=%d, want 403", code)
	}

	// The new counters show up on /metrics (text/plain, so read it raw).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	text := sb.String()
	for _, name := range []string{"ledger_period_rejections_total", "ledger_dry_runs_total"} {
		if !strings.Contains(text, name) {
			t.Errorf("/metrics missing %s", name)
		}
	}
}
