package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestExternalMappingEndpoints(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Declare two mappings for the seeded cash account.
	code, body := postJSON(t, srv.URL+"/accounts/cash/external-mappings",
		`{"system":"bank","external_id":"DE89370400440532013000","note":"primary settlement"}`)
	if code != http.StatusOK {
		t.Fatalf("POST mapping: status=%d, want 200", code)
	}
	if body["account"] != "cash" || body["system"] != "bank" || body["external_id"] != "DE89370400440532013000" {
		t.Errorf("POST mapping body = %v, want account=cash system=bank", body)
	}
	code, body = postJSON(t, srv.URL+"/accounts/cash/external-mappings",
		`{"system":"psp","external_id":"MCH-8842"}`)
	if code != http.StatusOK {
		t.Fatalf("POST second mapping: status=%d, want 200", code)
	}

	// List: sorted by (system, external_id), bank before psp.
	code, body = getJSON(t, srv.URL+"/accounts/cash/external-mappings")
	if code != http.StatusOK {
		t.Fatalf("GET mappings: status=%d, want 200", code)
	}
	mappings, ok := body["mappings"].([]any)
	if !ok || len(mappings) != 2 {
		t.Fatalf("GET mappings body = %v, want 2 rows", body)
	}
	row0 := mappings[0].(map[string]any)
	if row0["system"] != "bank" || row0["external_id"] != "DE89370400440532013000" || row0["note"] != "primary settlement" {
		t.Errorf("row 0 = %v, want bank/DE89370400440532013000", row0)
	}
	if mappings[1].(map[string]any)["system"] != "psp" {
		t.Errorf("row 1 = %v, want psp", mappings[1])
	}

	// An account with no mappings returns an empty list, not 404.
	code, body = getJSON(t, srv.URL+"/accounts/equity/external-mappings")
	if code != http.StatusOK {
		t.Fatalf("GET empty mappings: status=%d, want 200", code)
	}
	if ms, ok := body["mappings"].([]any); !ok || len(ms) != 0 {
		t.Errorf("empty mappings body = %v, want empty list", body)
	}

	// The Reconcile report discloses the mapping table.
	code, body = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d, want 200", code)
	}
	ext, ok := body["external_mappings"].([]any)
	if !ok || len(ext) != 2 {
		t.Errorf("reconcile external_mappings = %v, want 2 rows", body["external_mappings"])
	}

	// Remove one row via query parameters.
	code, body = deleteJSON(t, srv.URL+"/accounts/cash/external-mappings?system=psp&external_id=MCH-8842")
	if code != http.StatusOK {
		t.Fatalf("DELETE mapping: status=%d, want 200", code)
	}
	if body["removed"] != true {
		t.Errorf("DELETE body = %v, want removed=true", body)
	}
	code, body = getJSON(t, srv.URL+"/accounts/cash/external-mappings")
	if code != http.StatusOK {
		t.Fatalf("GET after delete: status=%d, want 200", code)
	}
	if ms := body["mappings"].([]any); len(ms) != 1 {
		t.Errorf("mappings after delete = %d rows, want 1", len(ms))
	}

	// Removing again is 404.
	code, _ = deleteJSON(t, srv.URL+"/accounts/cash/external-mappings?system=psp&external_id=MCH-8842")
	if code != http.StatusNotFound {
		t.Errorf("DELETE missing mapping: status=%d, want 404", code)
	}

	// The change counter counts the two sets plus the one remove.
	code, metricsBody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d, want 200", code)
	}
	if !strings.Contains(metricsBody, "ledger_external_mapping_changes_total 3\n") {
		t.Errorf("metrics missing ledger_external_mapping_changes_total 3:\n%s", metricsBody)
	}
}

func TestExternalMappingEndpointValidation(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Bad system charset is 400.
	code, _ := postJSON(t, srv.URL+"/accounts/cash/external-mappings",
		`{"system":"bank!","external_id":"123"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad system: status=%d, want 400", code)
	}
	// Empty external ID is 400.
	code, _ = postJSON(t, srv.URL+"/accounts/cash/external-mappings",
		`{"system":"bank","external_id":""}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty external_id: status=%d, want 400", code)
	}
	// Unknown JSON fields are 400 (strict body, like every write endpoint).
	code, _ = postJSON(t, srv.URL+"/accounts/cash/external-mappings",
		`{"system":"bank","external_id":"123","bogus":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// DELETE without the query parameters is 400.
	code, _ = deleteJSON(t, srv.URL+"/accounts/cash/external-mappings?system=bank")
	if code != http.StatusBadRequest {
		t.Errorf("DELETE without external_id: status=%d, want 400", code)
	}
	// Nothing was counted: every request above was rejected.
	code, metricsBody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d, want 200", code)
	}
	if !strings.Contains(metricsBody, "ledger_external_mapping_changes_total 0\n") {
		t.Errorf("metrics missing ledger_external_mapping_changes_total 0:\n%s", metricsBody)
	}
}

func TestExternalMappingMethodNotAllowed(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()
	// Method guard: PUT on the external-mappings endpoint is 405 (the
	// mux rejects it before any handler runs).
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/accounts/cash/external-mappings", nil)
	if err != nil {
		t.Fatalf("PUT mappings: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT mappings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT mappings: status=%d, want 405", resp.StatusCode)
	}
}
