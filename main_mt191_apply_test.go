package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postMT191Text posts a raw MT191 FIN text document to
// /exports/mt191/apply with an x-ledger-actor header.
func postMT191Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt191HTTPDoc renders an MT191 charge request: ref as :20:,
// payoutID as :21:, amount32B as the full :32B: body
// ("CCC<integer>,<ff>"), and details as the :71B: text.
// Empty details omits :71B: entirely.
func mt191HTTPDoc(ref, payoutID, amount32B, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I191BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":32B:" + amount32B + "\n")
	if details != "" {
		sb.WriteString(":71B:" + details + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// Full flow through HTTP: submit a payout, apply the
// correspondent bank's MT191 charge request, replay the same :20:,
// and the metrics counter moves.
func TestMT191ApplyEndpoint_RecordsAndReturnsReceipt(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-mt191-http-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/mt191/apply"

	// The :32B: carries the requested amount in the submitted
	// payout's currency — no value date: a request asks to be
	// paid, nothing has been taken.
	doc := mt191HTTPDoc("MT191HTTP000001", payoutID, "USD15,00", "Correspondent handling fee")
	code, body := postMT191Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt191/apply: status=%d body=%v", code, body)
	}
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request = %v, want an object", body["request"])
	}
	if request["msg_id"] != "MT191HTTP000001" || request["payout_id"] != payoutID ||
		request["currency"] != "USD" || request["amount_cents"] != float64(1500) ||
		request["details"] != "Correspondent handling fee" ||
		request["status"] != "pending" {
		t.Errorf("request = %+v, want the parsed fields verbatim, status pending", request)
	}
	if body["reference"] != "MT191HTTP000001" {
		t.Errorf("reference = %v, want MT191HTTP000001", body["reference"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT191Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT191Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown payout is a 422.
	unknown := mt191HTTPDoc("MT191HTTP000002", "no-such", "USD15,00", "Correspondent handling fee")
	code, body = postMT191Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown payout: status=%d body=%v, want 422", code, body)
	}
	// A :32B: currency that disagrees with the payout's is a
	// 422: never silently converted.
	mismatch := mt191HTTPDoc("MT191HTTP000003", payoutID, "EUR15,00", "Correspondent handling fee")
	code, body = postMT191Text(t, applyURL, mismatch, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("currency mismatch: status=%d body=%v, want 422", code, body)
	}
	// The same :20: with different terms is a 422 conflict.
	conflict := mt191HTTPDoc("MT191HTTP000001", payoutID, "USD25,00", "Correspondent handling fee")
	code, body = postMT191Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt191_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt191_applies_total 6:\n%s", metricsBody)
	}
}
