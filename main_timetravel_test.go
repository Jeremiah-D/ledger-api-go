package main

import (
	"net/http"
	"strings"
	"testing"
)

// The seed posts debit cash / credit equity 100 × 3 (versions 1-3), so
// cash holds 100/200/300 across versions and equity -100/-200/-300.
func TestHandleBalanceAt(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	for _, tc := range []struct {
		version  string
		expected float64
	}{
		{"0", 0},
		{"1", 100},
		{"2", 200},
		{"3", 300},
	} {
		code, body := getJSON(t, srv.URL+"/accounts/cash/balance-at?version="+tc.version)
		if code != http.StatusOK {
			t.Fatalf("version=%s: status = %d, want 200", tc.version, code)
		}
		if body["balance_cents"] != tc.expected {
			t.Errorf("version=%s: balance_cents = %v, want %v", tc.version, body["balance_cents"], tc.expected)
		}
		if body["currency"] != "USD" {
			t.Errorf("version=%s: currency = %v, want USD (default)", tc.version, body["currency"])
		}
	}

	// Credit-leg account goes negative, symmetrically.
	code, body := getJSON(t, srv.URL+"/accounts/equity/balance-at?version=3")
	if code != http.StatusOK || body["balance_cents"] != float64(-300) {
		t.Errorf("equity@3 = %v (code %d), want -300", body["balance_cents"], code)
	}

	// Unknown account reports zero, like /balance and /snapshot.
	code, body = getJSON(t, srv.URL+"/accounts/nobody/balance-at?version=2")
	if code != http.StatusOK || body["balance_cents"] != float64(0) {
		t.Errorf("unknown account = %v (code %d), want 0", body["balance_cents"], code)
	}
}

func TestHandleBalanceAtValidation(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Missing, non-numeric, and negative versions are 400.
	for _, q := range []string{"", "abc", "-1", "1.5"} {
		code, _ := getJSON(t, srv.URL+"/accounts/cash/balance-at?version="+q)
		if code != http.StatusBadRequest {
			t.Errorf("version=%q: status = %d, want 400", q, code)
		}
	}

	// A version beyond the current ledger version (3) is 422: the future
	// has no balance yet.
	code, body := getJSON(t, srv.URL+"/accounts/cash/balance-at?version=4")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("future version: status = %d, want 422", code)
	}
	if body["error"] == nil || body["error"] == "" {
		t.Errorf("future version: missing error body: %v", body)
	}
}

func TestHandleBalanceAtMetric(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	if code, _ := getJSON(t, srv.URL+"/accounts/cash/balance-at?version=1"); code != http.StatusOK {
		t.Fatalf("balance-at status = %d, want 200", code)
	}
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
	if !strings.Contains(sb.String(), "ledger_balance_at_queries_total 1\n") {
		t.Errorf("metrics missing ledger_balance_at_queries_total 1:\n%s", sb.String())
	}
}
