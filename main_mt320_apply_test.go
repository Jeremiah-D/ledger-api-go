package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postMT320Text posts a raw MT320 FIN text document to
// /exports/mt320/apply with an x-ledger-actor header.
func postMT320Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// Full flow through HTTP: open the deposit, export the
// confirmation, apply the counterparty's MT320 back, replay the
// same :20:, and the metrics counter moves.
func TestMT320ApplyEndpoint_ConfirmsAndReturnsConfirmation(t *testing.T) {
	l, td := mt320HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt320/apply"

	// The counterparty's confirmation is this ledger's own
	// export, sent back verbatim: the booked terms agree exactly.
	q := url.Values{}
	q.Set("deposit_id", td.ID)
	q.Set("counterparty_bic", "NWBKGB2L")
	code, doc, _ := getText(t, srv.URL+"/exports/mt320?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}

	code, body := postMT320Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt320/apply: status=%d body=%v", code, body)
	}
	deposit, ok := body["deposit"].(map[string]any)
	if !ok {
		t.Fatalf("deposit = %v, want an object", body["deposit"])
	}
	if deposit["id"] != td.ID {
		t.Errorf("deposit id = %v, want %q", deposit["id"], td.ID)
	}
	if deposit["status"] != "active" {
		t.Errorf("deposit status = %v, want active (confirmation moves no lifecycle state)", deposit["status"])
	}
	if body["common_ref"] != "DEUTNW1001NWBKDE" {
		t.Errorf("common_ref = %v, want DEUTNW1001NWBKDE", body["common_ref"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A rate mismatch is a 422: the whole confirmation is refused.
	badRate := strings.Replace(doc, ":37G:0,05", ":37G:0,06", 1)
	code, body = postMT320Text(t, applyURL, badRate, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("rate mismatch: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT320Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT320Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown deposit is a 422: the :20: names no booked
	// deposit, and a confirmation never opens one.
	unknown := strings.ReplaceAll(doc, td.ID, "td-mt320-9999")
	code, body = postMT320Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown deposit: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt320_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt320_applies_total 5:\n%s", metricsBody)
	}
}
