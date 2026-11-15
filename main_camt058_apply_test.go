package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postCAMT058XML posts a raw camt.058 XML document to
// /exports/camt058/apply with an x-ledger-actor header.
func postCAMT058XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(xmlDoc))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/xml")
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

// camt058HTTPDoc renders a camt.058 cancellation advice of the
// given schema version, withdrawing the camt.057 notification
// named by orgnlMsgID/orgnlNtfctnID, with the free-form reason
// (an empty reason omits the CxlRsn block).
func camt058HTTPDoc(version, msgID, orgnlMsgID, orgnlNtfctnID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.058.` + version + `">`)
	sb.WriteString(`<NtfctnToRcvCxlAdvc><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlNtfctn><OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlNtfctnId>` + orgnlNtfctnID + `</OrgnlNtfctnId></OrgnlNtfctn>`)
	if reason != "" {
		sb.WriteString(`<CxlRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsn>`)
	}
	sb.WriteString(`</NtfctnToRcvCxlAdvc></Document>`)
	return sb.String()
}

// registerCAMT057HTTP registers one camt.057 notification through
// the HTTP apply route, the notification a later camt.058 advice
// withdraws.
func registerCAMT057HTTP(t *testing.T, applyURL, msgID, ntfctnID string) {
	t.Helper()
	doc := camt057HTTPDoc("001.07", msgID, ntfctnID,
		`<IBAN>GB29NWBK60161331926819</IBAN>`, "250.00", "USD", "2026-11-20",
		`<Pty><Nm>ACME Corp</Nm></Pty>`)
	code, body := postCAMT057XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("register %s: status=%d body=%v", msgID, code, body)
	}
}

// Full flow through HTTP: register a counterparty's camt.057
// notification, withdraw it with a camt.058 advice, replay the
// same advice MsgId, and the metrics counter moves. The server
// ledger is the camt.057 export fixture, so the GET export stays
// live on the same server.
func TestHTTPCAMT058Apply(t *testing.T) {
	l, sched := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/camt058/apply"
	registerCAMT057HTTP(t, srv.URL+"/exports/camt057/apply", "msg-http-n1", "ntfctn-http-n1")

	doc := camt058HTTPDoc("001.08", "msg-http-a1", "msg-http-n1", "ntfctn-http-n1",
		"Client withdrew \u2014 duplicate notice")
	code, body := postCAMT058XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt058/apply: status=%d body=%v", code, body)
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["msg_id"] != "msg-http-n1" || receipt["account"] != "bob" {
		t.Errorf("receipt = %+v, want msg-http-n1/bob", receipt)
	}
	if receipt["amount_cents"] != float64(25000) || receipt["currency"] != "USD" {
		t.Errorf("receipt = %+v, want 25000 USD", receipt)
	}
	if receipt["cancelled"] != true || receipt["cancel_msg_id"] != "msg-http-a1" {
		t.Errorf("receipt = %+v, want cancelled msg-http-a1", receipt)
	}
	if receipt["cancel_reason"] != "Client withdrew \u2014 duplicate notice" {
		t.Errorf("receipt = %+v, want the verbatim reason", receipt)
	}
	// Replay the same advice MsgId: idempotent, duplicate set.
	code, body = postCAMT058XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// The metrics counter counts both applies.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt058_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt058_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: pause the
	// schedule (the camt.058 gate is the opposite of camt.057's)
	// and it still renders its advice.
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	code, _, exportBody := getBody(t, srv.URL+"/exports/camt058?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("GET /exports/camt058: status=%d", code)
	}
	if !strings.Contains(exportBody, "camt058-sched-1-0") {
		t.Errorf("export body missing camt058-sched-1-0:\n%s", exportBody)
	}
}

// Error mapping for POST /exports/camt058/apply: a malformed
// document is a 400 with a repair-queue record, an unknown
// original notification is a 404 with a repair-queue record, a
// disagreeing OrgnlNtfctnId is a 422 with a repair-queue record,
// and a reused advice MsgId naming another original is a 422.
func TestHTTPCAMT058ApplyErrors(t *testing.T) {
	l, _ := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/camt058/apply"

	// Wrong schema version: unparsable, but the document still
	// names its MsgId, so it parks.
	bad := camt058HTTPDoc("001.07", "msg-http-bad", "msg-http-n1", "ntfctn-http-n1", "reason")
	code, body := postCAMT058XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Unknown original notification: no camt.057 was ever
	// registered under it.
	unknown := camt058HTTPDoc("001.08", "msg-http-unk", "msg-http-never", "ntfctn-http-never", "reason")
	code, body = postCAMT058XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown original: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Register two notifications for the mismatch and conflict
	// shapes below.
	registerCAMT057HTTP(t, srv.URL+"/exports/camt057/apply", "msg-http-n1", "ntfctn-http-n1")
	registerCAMT057HTTP(t, srv.URL+"/exports/camt057/apply", "msg-http-n2", "ntfctn-http-n2")
	// OrgnlNtfctnId disagreeing with the stored receipt.
	mismatch := camt058HTTPDoc("001.08", "msg-http-mm", "msg-http-n1", "ntfctn-http-other", "reason")
	code, body = postCAMT058XML(t, applyURL, mismatch, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// The advice applies, then the same advice MsgId naming the
	// other notification is a conflict.
	first := camt058HTTPDoc("001.08", "msg-http-a1", "msg-http-n1", "ntfctn-http-n1", "reason")
	code, body = postCAMT058XML(t, applyURL, first, "ops")
	if code != http.StatusOK {
		t.Fatalf("first advice: status=%d body=%v, want 200", code, body)
	}
	conflict := camt058HTTPDoc("001.08", "msg-http-a1", "msg-http-n2", "ntfctn-http-n2", "reason")
	code, body = postCAMT058XML(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The failed and successful applies all count in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt058_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_camt058_applies_total 5:\n%s", metricsBody)
	}
}
