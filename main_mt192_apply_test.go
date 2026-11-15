package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postMT192Text posts a raw MT192 FIN text document to
// /exports/mt192/apply with an x-ledger-actor header.
func postMT192Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt192HTTPDoc renders an MT192 cancellation request: ref as :20:,
// payoutID as :21:, valueDate11S as the :11S: body ("103" +
// YYMMDD), and reason as the :79: narrative. An empty reason
// omits :79: entirely.
func mt192HTTPDoc(ref, payoutID, valueDate11S, reason string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I192BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":11S:" + valueDate11S + "\n")
	if reason != "" {
		sb.WriteString(":79:" + reason + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// Full flow through HTTP: submit a payout, apply the
// correspondent bank's MT192 cancellation request, replay the
// same :20:, and the metrics counter moves.
func TestMT192ApplyEndpoint_CancelsAndReturnsCancellation(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-mt192-http-1"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/mt192/apply"

	// The :11S: is the payout's MT103 value date: the submit
	// day, by the export's default rule.
	valueDate := "103" + time.Now().UTC().Format("060102")
	doc := mt192HTTPDoc("MT192HTTP000001", payoutID, valueDate, "Duplicate payment")
	code, body := postMT192Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt192/apply: status=%d body=%v", code, body)
	}
	cancellation, ok := body["cancellation"].(map[string]any)
	if !ok {
		t.Fatalf("cancellation = %v, want an object", body["cancellation"])
	}
	if cancellation["status"] != "requested" {
		t.Errorf("cancellation status = %v, want requested", cancellation["status"])
	}
	if cancellation["payout_id"] != payoutID || cancellation["uetr"] != uetr {
		t.Errorf("cancellation = %+v, want payout %q / uetr %q", cancellation, payoutID, uetr)
	}
	if cancellation["reason_code"] != "Duplicate payment" {
		t.Errorf("reason_code = %v, want Duplicate payment", cancellation["reason_code"])
	}
	if body["reference"] != "MT192HTTP000001" {
		t.Errorf("reference = %v, want MT192HTTP000001", body["reference"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A value-date mismatch is a 422: the request names another
	// day's instruction.
	tomorrow := "103" + time.Now().UTC().AddDate(0, 0, 1).Format("060102")
	code, body = postMT192Text(t, applyURL, mt192HTTPDoc("MT192HTTP000002", payoutID, tomorrow, "Duplicate payment"), "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("value-date mismatch: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT192Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT192Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown payout is a 422.
	unknown := mt192HTTPDoc("MT192HTTP000003", "no-such", valueDate, "Duplicate payment")
	code, body = postMT192Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown payout: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt192_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt192_applies_total 5:\n%s", metricsBody)
	}
}
