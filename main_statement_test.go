package main

import (
	"net/http"
	"testing"
	"time"
)

// TestHandleStatement exercises GET /accounts/{id}/statement against the
// seeded server (cash: three 100-cent USD debits posted 2026-10-01,
// 2026-10-02, 2026-10-03 at 12:00 UTC).
func TestHandleStatement(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/cash/statement?from=2026-10-01T00:00:00Z&to=2026-10-04T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["account"] != "cash" {
		t.Errorf("account = %v, want cash", body["account"])
	}
	period := body["period"].(map[string]any)
	if period["from"] != "2026-10-01T00:00:00Z" || period["to"] != "2026-10-04T00:00:00Z" {
		t.Errorf("period = %v, want the requested window", period)
	}
	opening := body["opening_balances"].([]any)
	if len(opening) != 1 || opening[0].(map[string]any)["balance_cents"] != float64(0) {
		t.Errorf("opening_balances = %v, want one USD row at 0", opening)
	}
	closing := body["closing_balances"].([]any)
	if len(closing) != 1 || closing[0].(map[string]any)["balance_cents"] != float64(300) {
		t.Errorf("closing_balances = %v, want one USD row at 300", closing)
	}
	entries := body["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// (CreatedAt, ID) order: e-http-0, e-http-1, e-http-2.
	for i, want := range []string{"e-http-0", "e-http-1", "e-http-2"} {
		if entries[i].(map[string]any)["id"] != want {
			t.Errorf("entries[%d].id = %v, want %s", i, entries[i].(map[string]any)["id"], want)
		}
	}
	if fees, ok := body["fee_summary"].([]any); !ok || len(fees) != 0 {
		t.Errorf("fee_summary = %v, want empty", body["fee_summary"])
	}
	ev, ok := body["event_summary"].(map[string]any)
	if !ok {
		t.Fatalf("event_summary missing: %v", body)
	}
	if ev["overdraft_protected"] != false || ev["currently_frozen"] != false {
		t.Errorf("event_summary risk state = %v, want both false", ev)
	}
	if body["version"] != float64(3) {
		t.Errorf("version = %v, want 3", body["version"])
	}

	// Unknown account: a valid empty statement, not a 404.
	code, body = getJSON(t, srv.URL+"/accounts/nobody/statement?from=2026-10-01T00:00:00Z&to=2026-10-04T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("unknown account status = %d, want 200", code)
	}
	if len(body["entries"].([]any)) != 0 || len(body["opening_balances"].([]any)) != 0 {
		t.Errorf("unknown account statement not empty: %v", body)
	}
}

func TestHandleStatementDefaults(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// No from/to: from defaults to the first day of the current UTC
	// month at 00:00, to defaults to now. The seed entries are from
	// 2026-10-01..03 and today is in October 2026, so they are included.
	code, body := getJSON(t, srv.URL+"/accounts/cash/statement")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	now := time.Now().UTC()
	wantFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	period := body["period"].(map[string]any)
	if period["from"] != wantFrom {
		t.Errorf("default from = %v, want %s", period["from"], wantFrom)
	}
	if len(body["entries"].([]any)) != 3 {
		t.Errorf("default-window entries = %d, want 3", len(body["entries"].([]any)))
	}
}

func TestHandleStatementBadParams(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	for _, url := range []string{
		// Malformed timestamps fail fast with 400.
		srv.URL + "/accounts/cash/statement?from=not-a-time",
		srv.URL + "/accounts/cash/statement?to=2026-13-99",
		srv.URL + "/accounts/cash/statement?from=2026-10-01T00:00:00Z&to=yesterday",
		// Inverted window is a 400, not an empty statement.
		srv.URL + "/accounts/cash/statement?from=2026-10-04T00:00:00Z&to=2026-10-01T00:00:00Z",
	} {
		code, _ := getJSON(t, url)
		if code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", url, code)
		}
	}
}
