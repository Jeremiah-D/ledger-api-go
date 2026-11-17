package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postMT304Text posts a raw MT304 FIN text document to
// /exports/mt304/apply with an x-ledger-actor header.
func postMT304Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// mt304HTTPDoc renders one inbound MT304 instruction with the
// payer/payee parties and the CITIUS33 custodian.
func mt304HTTPDoc(ref, scope string, tradeDate, valueDate time.Time) string {
	yymmdd := func(t time.Time) string { return t.UTC().Format("060102") }
	return fmt.Sprintf("{1:F01DEUTDEFFXXX0000000000}\n"+
		"{2:I304CITIUS33XXXU3003}\n"+
		"{3:{108:%s}}\n"+
		"{4:\n"+
		":15A:\n"+
		":20:%s\n"+
		":22A:NEWT\n"+
		":94A:%s\n"+
		":83A:DEUTDEFF\n"+
		":82A:DEUTDEFF\n"+
		":87A:NWBKGB2L\n"+
		":15B:\n"+
		":30T:%s\n"+
		":30V:%s\n"+
		":36:1,1\n"+
		":32B:EUR110,00\n"+
		":53A:NWBKGB2L\n"+
		":33B:USD100,00\n"+
		":57A:NWBKGB2L\n"+
		"-}", ref, ref, scope, yymmdd(tradeDate), yymmdd(valueDate))
}

// Full flow through HTTP: an INST instruction books the forward,
// a replay is a duplicate, an illegal scope and an unknown broker
// are 422s, a malformed document is a 400, and the metrics
// counter moves.
func TestMT304ApplyEndpoint_BooksAndReplays(t *testing.T) {
	l, _ := mt304HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt304/apply"
	now := time.Now().UTC()
	doc := mt304HTTPDoc("fwd-mt304-3001", "INST", now, now.Add(48*time.Hour))

	code, body := postMT304Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt304/apply: status=%d body=%v", code, body)
	}
	forward, ok := body["forward"].(map[string]any)
	if !ok {
		t.Fatalf("forward = %v, want an object", body["forward"])
	}
	if forward["id"] != "fwd-mt304-3001" {
		t.Errorf("forward id = %v, want fwd-mt304-3001", forward["id"])
	}
	if forward["status"] != "booked" {
		t.Errorf("forward status = %v, want booked", forward["status"])
	}
	if forward["from_account"] != "payer" || forward["to_account"] != "payee" {
		t.Errorf("forward accounts = %v/%v, want payer/payee", forward["from_account"], forward["to_account"])
	}
	if forward["rate_num"] != float64(11) || forward["rate_den"] != float64(10) {
		t.Errorf("forward rate = %v/%v, want 11/10", forward["rate_num"], forward["rate_den"])
	}
	if forward["notional_cents"] != float64(10000) {
		t.Errorf("forward notional = %v, want 10000", forward["notional_cents"])
	}
	if body["scope"] != "INST" {
		t.Errorf("scope = %v, want INST", body["scope"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT304Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An illegal :94A: scope is a 422.
	badScope := strings.Replace(doc, ":94A:INST", ":94A:BOGUS", 1)
	code, body = postMT304Text(t, applyURL, badScope, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("illegal scope: status=%d body=%v, want 422", code, body)
	}
	// An unknown broker account fails closed with a 422.
	unknownBroker := strings.Replace(
		mt304HTTPDoc("fwd-mt304-3002", "INST", now, now.Add(48*time.Hour)),
		":87A:NWBKGB2L", ":87A:ZZZZUS33", 1)
	code, body = postMT304Text(t, applyURL, unknownBroker, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown broker: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT304Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt304_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt304_applies_total 5:\n%s", metricsBody)
	}
}

// An ADVI instruction through HTTP records the advice and books
// no forward.
func TestMT304ApplyEndpoint_AdviceOnly(t *testing.T) {
	l, _ := mt304HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt304/apply"
	now := time.Now().UTC()
	doc := mt304HTTPDoc("fwd-mt304-3003", "ADVI", now, now.Add(48*time.Hour))

	code, body := postMT304Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt304/apply: status=%d body=%v", code, body)
	}
	if body["scope"] != "ADVI" {
		t.Errorf("scope = %v, want ADVI", body["scope"])
	}
	if _, hasForward := body["forward"]; hasForward {
		t.Errorf("forward = %v, want absent (ADVI books nothing)", body["forward"])
	}
	// The advice is recorded under its :20:.
	if _, ok := l.MT304Advice("fwd-mt304-3003"); !ok {
		t.Error("MT304Advice: want the recorded advice, got none")
	}
}
