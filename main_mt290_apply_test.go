package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postMT290Text posts a raw MT290 FIN text document to
// /exports/mt290/apply with an x-ledger-actor header.
func postMT290Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt290HTTPDoc renders an MT290 charge advice: ref as :20:,
// mt202Ref as :21:, amount32B as the full :32B: body
// ("CCC<integer>,<ff>"), and details as the :71B: text.
func mt290HTTPDoc(ref, mt202Ref, amount32B, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I290BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + mt202Ref + "\n")
	sb.WriteString(":32B:" + amount32B + "\n")
	sb.WriteString(":71B:" + details + "\n")
	sb.WriteString("-}")
	return sb.String()
}

// Full flow through HTTP: apply a correspondent's MT202, apply
// the MT290 charge advice against it, replay the same :20:,
// and the metrics counter moves.
func TestMT290ApplyEndpoint_RecordsAndReturnsReceipt(t *testing.T) {
	l := mt202HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	mt202Ref := "MT202-HTTP-01"
	code, body := postMT202Text(t, srv.URL+"/exports/mt202/apply", mt202HTTPDoc(mt202Ref), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt202/apply setup: status=%d body=%v", code, body)
	}
	applyURL := srv.URL + "/exports/mt290/apply"

	doc := mt290HTTPDoc("MT290HTTP000001", mt202Ref, "USD15,00", "Correspondent handling fee")
	code, body = postMT290Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt290/apply: status=%d body=%v", code, body)
	}
	advice, ok := body["advice"].(map[string]any)
	if !ok {
		t.Fatalf("advice = %v, want an object", body["advice"])
	}
	if advice["msg_id"] != "MT290HTTP000001" || advice["mt202_ref"] != mt202Ref ||
		advice["currency"] != "USD" || advice["amount_cents"] != float64(1500) ||
		advice["details"] != "Correspondent handling fee" {
		t.Errorf("advice = %+v, want the parsed fields verbatim", advice)
	}
	if _, hasValueDate := advice["value_date"]; hasValueDate {
		t.Errorf("advice carries value_date = %v, want none (:32B: has no date)", advice["value_date"])
	}
	if body["reference"] != "MT290HTTP000001" {
		t.Errorf("reference = %v, want MT290HTTP000001", body["reference"])
	}
	if body["mt202_ref"] != mt202Ref {
		t.Errorf("mt202_ref = %v, want %q", body["mt202_ref"], mt202Ref)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT290Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT290Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown MT202 is a 422.
	unknown := mt290HTTPDoc("MT290HTTP000002", "no-such", "USD15,00", "Correspondent handling fee")
	code, body = postMT290Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown MT202: status=%d body=%v, want 422", code, body)
	}
	// A :32B: currency that disagrees with the MT202's is a
	// 422: never silently converted.
	mismatch := mt290HTTPDoc("MT290HTTP000003", mt202Ref, "EUR15,00", "Correspondent handling fee")
	code, body = postMT290Text(t, applyURL, mismatch, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("currency mismatch: status=%d body=%v, want 422", code, body)
	}
	// The same :20: with different terms is a 422 conflict.
	conflict := mt290HTTPDoc("MT290HTTP000001", mt202Ref, "USD25,00", "Correspondent handling fee")
	code, body = postMT290Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt290_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt290_applies_total 6:\n%s", metricsBody)
	}
}
