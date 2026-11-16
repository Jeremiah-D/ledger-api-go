package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postMT300Text posts a raw MT300 FIN text document to
// /exports/mt300/apply with an x-ledger-actor header.
func postMT300Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(finDoc))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	if actor != "" {
		req.Header.Set("x-ledger-actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// Full flow through HTTP: book the forward, export the
// confirmation, apply the counterparty's MT300 back, replay the
// same :20:, and the metrics counter moves.
func TestMT300ApplyEndpoint_ConfirmsAndReturnsConfirmation(t *testing.T) {
	l, c := mt300HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt300/apply"

	// The counterparty's confirmation is this ledger's own
	// export, sent back verbatim: the booked terms agree exactly.
	q := url.Values{}
	q.Set("forward_id", c.ID)
	code, doc, _ := getText(t, srv.URL+"/exports/mt300?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}

	code, body := postMT300Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt300/apply: status=%d body=%v", code, body)
	}
	forward, ok := body["forward"].(map[string]any)
	if !ok {
		t.Fatalf("forward = %v, want an object", body["forward"])
	}
	if forward["id"] != c.ID {
		t.Errorf("forward id = %v, want %q", forward["id"], c.ID)
	}
	if forward["status"] != "booked" {
		t.Errorf("forward status = %v, want booked (confirmation moves no lifecycle state)", forward["status"])
	}
	if body["common_ref"] != "DEUTNW2001" {
		t.Errorf("common_ref = %v, want DEUTNW2001", body["common_ref"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A rate mismatch is a 422: the whole confirmation is refused.
	badRate := strings.Replace(doc, ":36:1,1", ":36:1,2", 1)
	code, body = postMT300Text(t, applyURL, badRate, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("rate mismatch: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT300Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT300Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown forward is a 422: the :22C: names no booked
	// contract, and a confirmation never books one.
	unknown := strings.Replace(doc, ":22C:DEUTNW2001", ":22C:DEUTNW9999", 1)
	code, body = postMT300Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown forward: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt300_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt300_applies_total 5:\n%s", metricsBody)
	}
}
