package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// postMT321Text posts a raw MT321 FIN text document to
// /exports/mt321/apply with an x-ledger-actor header.
func postMT321Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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
// instruction, apply the fund manager's MT321 back, replay the
// same SEME, and the metrics counter moves.
func TestMT321ApplyEndpoint_RegistersAndReturnsInstruction(t *testing.T) {
	l, td := mt320HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt321/apply"

	// The fund manager's instruction is this ledger's own
	// export, sent back verbatim: the booked terms agree exactly.
	q := url.Values{}
	q.Set("deposit_id", td.ID)
	q.Set("custodian_bic", "CHASUS33")
	q.Set("counterparty_bic", "NWBKGB2L")
	code, doc, _ := getText(t, srv.URL+"/exports/mt321?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}

	code, body := postMT321Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt321/apply: status=%d body=%v", code, body)
	}
	instruction, ok := body["instruction"].(map[string]any)
	if !ok {
		t.Fatalf("instruction = %v, want an object", body["instruction"])
	}
	if instruction["reference"] != td.ID {
		t.Errorf("instruction reference = %v, want %q", instruction["reference"], td.ID)
	}
	if instruction["common_ref"] != "DEUTNW1001NWBKDE" {
		t.Errorf("instruction common_ref = %v, want DEUTNW1001NWBKDE", instruction["common_ref"])
	}
	if body["deposit_id"] != td.ID {
		t.Errorf("deposit_id = %v, want %q", body["deposit_id"], td.ID)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// An LDCR mismatch is a 422: the whole instruction is refused.
	badLDCR := strings.Replace(doc, ":20C::LDCR//DEUTNW1001NWBKDE", ":20C::LDCR//DEUTNW1001NWBKDF", 1)
	code, body = postMT321Text(t, applyURL, badLDCR, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("LDCR mismatch: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT321Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same SEME with the same terms: idempotent,
	// duplicate set.
	code, body = postMT321Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown deposit is a 404: the SEME names no booked
	// deposit, and an instruction never opens one.
	unknown := strings.ReplaceAll(doc, td.ID, "td-mt320-9999")
	code, body = postMT321Text(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown deposit: status=%d body=%v, want 404", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt321_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt321_applies_total 5:\n%s", metricsBody)
	}
}

// An early-withdrawn deposit is a 422 through HTTP: the
// instruction predates the withdrawal but the deposit is gone.
func TestMT321ApplyEndpoint_WithdrawnDeposit422(t *testing.T) {
	l, td := mt320HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt321/apply"

	q := url.Values{}
	q.Set("deposit_id", td.ID)
	q.Set("custodian_bic", "CHASUS33")
	q.Set("counterparty_bic", "NWBKGB2L")
	code, doc, _ := getText(t, srv.URL+"/exports/mt321?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if _, err := l.WithdrawTermDeposit(td.ID, 0, time.Now().UTC()); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}

	code, body := postMT321Text(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("withdrawn deposit: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts the rejected apply.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt321_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_mt321_applies_total 1:\n%s", metricsBody)
	}
}
