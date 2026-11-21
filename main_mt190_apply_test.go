package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postMT190Text posts a raw MT190 FIN text document to
// /exports/mt190/apply with an x-ledger-actor header.
func postMT190Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt190HTTPDoc renders an MT190 charge advice: ref as :20:,
// payoutID as :21:, amount32A as the full :32A: body
// ("YYMMDDCCC<integer>,<ff>"), and details as the :71B: text.
// Empty details omits :71B: entirely.
func mt190HTTPDoc(ref, payoutID, amount32A, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I190BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":32A:" + amount32A + "\n")
	if details != "" {
		sb.WriteString(":71B:" + details + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// Full flow through HTTP: submit a payout, apply the
// correspondent bank's MT190 charge advice, replay the same :20:,
// and the metrics counter moves.
func TestMT190ApplyEndpoint_RecordsAndReturnsReceipt(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-mt190-http-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/mt190/apply"

	// The :32A: carries today's value date: the charge was
	// taken today, on the submitted payout's currency.
	amount32A := time.Now().UTC().Format("060102") + "USD15,00"
	doc := mt190HTTPDoc("MT190HTTP000001", payoutID, amount32A, "Correspondent handling fee")
	code, body := postMT190Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt190/apply: status=%d body=%v", code, body)
	}
	advice, ok := body["advice"].(map[string]any)
	if !ok {
		t.Fatalf("advice = %v, want an object", body["advice"])
	}
	if advice["msg_id"] != "MT190HTTP000001" || advice["payout_id"] != payoutID ||
		advice["currency"] != "USD" || advice["amount_cents"] != float64(1500) ||
		advice["value_date"] != time.Now().UTC().Format("060102") ||
		advice["details"] != "Correspondent handling fee" {
		t.Errorf("advice = %+v, want the parsed fields verbatim", advice)
	}
	if body["reference"] != "MT190HTTP000001" {
		t.Errorf("reference = %v, want MT190HTTP000001", body["reference"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT190Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT190Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown payout is a 422.
	unknown := mt190HTTPDoc("MT190HTTP000002", "no-such", amount32A, "Correspondent handling fee")
	code, body = postMT190Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown payout: status=%d body=%v, want 422", code, body)
	}
	// A :32A: currency that disagrees with the payout's is a
	// 422: never silently converted.
	mismatch := mt190HTTPDoc("MT190HTTP000003", payoutID, time.Now().UTC().Format("060102")+"EUR15,00", "Correspondent handling fee")
	code, body = postMT190Text(t, applyURL, mismatch, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("currency mismatch: status=%d body=%v, want 422", code, body)
	}
	// The same :20: with different terms is a 422 conflict.
	conflict := mt190HTTPDoc("MT190HTTP000001", payoutID, time.Now().UTC().Format("060102")+"USD25,00", "Correspondent handling fee")
	code, body = postMT190Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt190_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt190_applies_total 6:\n%s", metricsBody)
	}
}
