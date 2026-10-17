package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestAliasEndpoints(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Bind an alias to the seeded cash account.
	code, body := putJSON(t, srv.URL+"/accounts/cash/alias", `{"alias":"ops_cash"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT alias: status=%d, want 200", code)
	}
	if body["account"] != "cash" || body["alias"] != "ops_cash" {
		t.Errorf("PUT alias body = %v, want account=cash alias=ops_cash", body)
	}

	// The alias resolves on the balance endpoint: the response carries
	// the canonical account ID and echoes the alias.
	code, body = getJSON(t, srv.URL+"/accounts/ops_cash/balance")
	if code != http.StatusOK {
		t.Fatalf("GET balance by alias: status=%d, want 200", code)
	}
	if body["account"] != "cash" {
		t.Errorf("balance account = %v, want cash (resolved)", body["account"])
	}
	if body["alias"] != "ops_cash" {
		t.Errorf("balance alias = %v, want ops_cash", body["alias"])
	}
	if body["balance_cents"] != float64(300) {
		t.Errorf("balance_cents = %v, want 300", body["balance_cents"])
	}

	// Snapshot and trial-balance resolve and echo the alias too.
	code, body = getJSON(t, srv.URL+"/accounts/ops_cash/snapshot")
	if code != http.StatusOK || body["account"] != "cash" || body["alias"] != "ops_cash" {
		t.Errorf("snapshot by alias: status=%d body=%v, want 200 account=cash alias=ops_cash", code, body)
	}
	code, body = getJSON(t, srv.URL+"/accounts/ops_cash/trial-balance")
	if code != http.StatusOK || body["account"] != "cash" || body["alias"] != "ops_cash" {
		t.Errorf("trial-balance by alias: status=%d body=%v, want 200 account=cash alias=ops_cash", code, body)
	}

	// Addressing the account by its ID still echoes its alias.
	code, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if code != http.StatusOK || body["alias"] != "ops_cash" {
		t.Errorf("balance by ID: status=%d body=%v, want 200 with alias echo", code, body)
	}

	// An unaliased account omits the alias key.
	code, body = getJSON(t, srv.URL+"/accounts/equity/balance")
	if code != http.StatusOK {
		t.Fatalf("GET balance: status=%d, want 200", code)
	}
	if _, ok := body["alias"]; ok {
		t.Errorf("unaliased balance body has alias key: %v", body)
	}

	// A conflicting bind is 409 and changes nothing.
	code, body = putJSON(t, srv.URL+"/accounts/equity/alias", `{"alias":"ops_cash"}`)
	if code != http.StatusConflict {
		t.Errorf("conflicting PUT: status=%d, want 409", code)
	}
	code, body = getJSON(t, srv.URL+"/accounts/equity/alias")
	if code != http.StatusNotFound {
		t.Errorf("GET alias after conflict: status=%d, want 404", code)
	}

	// A malformed alias is 400.
	code, _ = putJSON(t, srv.URL+"/accounts/cash/alias", `{"alias":"bad/key"}`)
	if code != http.StatusBadRequest {
		t.Errorf("malformed alias PUT: status=%d, want 400", code)
	}

	// GET the bound alias.
	code, body = getJSON(t, srv.URL+"/accounts/cash/alias")
	if code != http.StatusOK || body["alias"] != "ops_cash" {
		t.Errorf("GET alias: status=%d body=%v, want 200 alias=ops_cash", code, body)
	}

	// Clearing via an empty alias removes the binding.
	code, body = putJSON(t, srv.URL+"/accounts/cash/alias", `{"alias":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear alias: status=%d, want 200", code)
	}
	code, _ = getJSON(t, srv.URL+"/accounts/cash/alias")
	if code != http.StatusNotFound {
		t.Errorf("GET alias after clear: status=%d, want 404", code)
	}
	// The cleared alias falls back to a plain account ID: unknown
	// accounts keep their historical zero-balance behavior.
	code, body = getJSON(t, srv.URL+"/accounts/ops_cash/balance")
	if code != http.StatusOK || body["balance_cents"] != float64(0) {
		t.Errorf("cleared alias balance: status=%d body=%v, want 200 zeros", code, body)
	}
	if body["account"] != "ops_cash" {
		t.Errorf("cleared alias account = %v, want ops_cash (unresolved fallback)", body["account"])
	}
}

func TestAliasMetrics(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	if code, _ := putJSON(t, srv.URL+"/accounts/cash/alias", `{"alias":"ops_cash"}`); code != http.StatusOK {
		t.Fatalf("PUT alias: status=%d, want 200", code)
	}
	// A rejected bind must not move the counter.
	if code, _ := putJSON(t, srv.URL+"/accounts/equity/alias", `{"alias":"ops_cash"}`); code != http.StatusConflict {
		t.Fatalf("conflicting PUT: status=%d, want 409", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
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
	if !strings.Contains(sb.String(), "ledger_alias_sets_total 1\n") {
		t.Errorf("metrics missing ledger_alias_sets_total 1:\n%s", sb.String())
	}
}
