package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postMT195Text posts a raw MT195 FIN text document to
// /exports/mt195/apply with an x-ledger-actor header.
func postMT195Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt195HTTPDoc renders an MT195 query: ref as :20:, payoutID as
// :21:, queries as the :75: coded question, and narrative as the
// :79: description. An empty queries omits :75: entirely; an
// empty narrative omits :79: entirely.
func mt195HTTPDoc(ref, payoutID, queries, narrative string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I195BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	if queries != "" {
		sb.WriteString(":75:" + queries + "\n")
	}
	if narrative != "" {
		sb.WriteString(":79:" + narrative + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// Full flow through HTTP: submit a payout, ask the correspondent
// bank's MT195 query, replay the same :20:, and the metrics
// counter moves.
func TestMT195ApplyEndpoint_ReturnsAnswer(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-mt195-http-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/mt195/apply"

	// The answer is ledger facts: the payout is submitted, its
	// MT103 value date is the submit day, and its gpi track is
	// "tracked".
	valueDate := time.Now().UTC().Format("060102")
	doc := mt195HTTPDoc("MT195HTTP000001", payoutID, "STAT", "Please confirm status of payment")
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt195/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "MT195HTTP000001" {
		t.Errorf("reference = %v, want MT195HTTP000001", body["reference"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	if body["payout_status"] != "submitted" {
		t.Errorf("payout_status = %v, want submitted", body["payout_status"])
	}
	if body["value_date"] != valueDate {
		t.Errorf("value_date = %v, want %q", body["value_date"], valueDate)
	}
	if body["gpi_status"] != "tracked" {
		t.Errorf("gpi_status = %v, want tracked", body["gpi_status"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT195Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown payout is a 422 (parked fail-closed in the
	// repair queue).
	unknown := mt195HTTPDoc("MT195HTTP000002", "no-such", "STAT", "Please confirm status")
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown payout: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt195_applies_total 4") {
		t.Errorf("metrics exposition missing ledger_mt195_applies_total 4:\n%s", metricsBody)
	}
}
