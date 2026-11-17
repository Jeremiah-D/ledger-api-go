package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postMT210Text posts a raw MT210 FIN text document to
// /exports/mt210/apply with an x-ledger-actor header.
func postMT210Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// Full flow through HTTP: export the notice, apply the
// counterparty's MT210 back, replay the same :20:, and the metrics
// counter moves.
func TestMT210ApplyEndpoint_RegistersAndReturnsReceipt(t *testing.T) {
	l, sched := mt210HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt210/apply"

	// The counterparty's notice is this ledger's own export, sent
	// back verbatim: the account is known, the principal agrees.
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	code, doc, _ := getText(t, srv.URL+"/exports/mt210?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}

	code, body := postMT210Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply: status=%d body=%v", code, body)
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["msg_id"] != "sched-1" {
		t.Errorf("receipt msg_id = %v, want sched-1", receipt["msg_id"])
	}
	if receipt["notification_id"] != "sched-1/run/0" {
		t.Errorf("receipt notification_id = %v, want sched-1/run/0", receipt["notification_id"])
	}
	if receipt["account"] != "bob" {
		t.Errorf("receipt account = %v, want bob", receipt["account"])
	}
	if receipt["amount_cents"] != float64(10000) {
		t.Errorf("receipt amount_cents = %v, want 10000", receipt["amount_cents"])
	}
	if receipt["expected_value_date"] != "2026-11-12" {
		t.Errorf("receipt expected_value_date = %v, want 2026-11-12", receipt["expected_value_date"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400 with no repair record: it
	// names no notification.
	code, body = postMT210Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] != nil {
		t.Errorf("malformed repair_id = %v, want none (no :20: named)", body["repair_id"])
	}
	// A document that names a :20: but fails the parse is a 400
	// with the repair record ID: the notice is parked, not dropped.
	broken := strings.Replace(doc, ":32B:USD100,00\n", "", 1)
	code, body = postMT210Text(t, applyURL, broken, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("broken: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("broken repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// An unknown :25: account is a 404 with the repair record ID:
	// the notice names an account the ledger never heard of. The
	// :20: is fresh here: a replayed :20: is a duplicate no-op
	// even when its account no longer resolves (the camt.057
	// rule), so the unknown-account path needs an unregistered
	// reference.
	unknown := strings.ReplaceAll(doc, "sched-1", "sched-9")
	unknown = strings.Replace(unknown, "GB29NWBK60161331926819", "FR1420041010050500013M02606", 1)
	code, body = postMT210Text(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown account: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("unknown repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Replay the same :20: with the same notification: idempotent,
	// duplicate set.
	code, body = postMT210Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt210_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt210_applies_total 5:\n%s", metricsBody)
	}
}
