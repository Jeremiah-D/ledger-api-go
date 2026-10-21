package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postPain002XML posts a raw pain.002 XML document to
// /exports/pain002/apply with an x-ledger-actor header.
func postPain002XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain002HTTPDoc renders a pain.002.001.03 report answering msgID for
// the given transfer verdicts.
func pain002HTTPDoc(msgID string, verdicts ...[3]string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.03">`)
	sb.WriteString(`<CstmrPmtStsRpt><GrpHdr><MsgId>RPT-HTTP</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-10-21T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>` + msgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId>`)
	sb.WriteString(`<GrpSts>ACSC</GrpSts></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><OrgnlPmtInfId>PMTINF-1</OrgnlPmtInfId>`)
	for _, v := range verdicts {
		e2e, txSts, reason := v[0], v[1], v[2]
		sb.WriteString(`<TxInfAndSts><OrgnlEndToEndId>` + e2e + `</OrgnlEndToEndId>`)
		sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
		if reason != "" {
			sb.WriteString(`<StsRsnInf><Rsn><Cd>` + reason + `</Cd></Rsn></StsRsnInf>`)
		}
		sb.WriteString(`</TxInfAndSts>`)
	}
	sb.WriteString(`</OrgnlPmtInfAndSts></CstmrPmtStsRpt></Document>`)
	return sb.String()
}

// pain002TxStateOf extracts a transfer's state from the apply response.
func pain002TxStateOf(t *testing.T, body map[string]any, txID string) string {
	t.Helper()
	txs, ok := body["txs"].([]any)
	if !ok {
		t.Fatalf("response has no txs: %v", body)
	}
	for _, item := range txs {
		m := item.(map[string]any)
		if m["tx_id"] == txID {
			return m["state"].(string)
		}
	}
	t.Fatalf("transfer %q not in response", txID)
	return ""
}

// Full flow through HTTP: export -> pain.002 apply -> states advance,
// the RJCT reason lands in the record, and the metrics counter moves.
func TestHTTPPain002Apply(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)

	doc := pain002HTTPDoc(msgID, [3]string{"st1", "ACSC", ""}, [3]string{"st2", "RJCT", "AC03"})
	code, body := postPain002XML(t, srv.URL+"/exports/pain002/apply", doc, "night-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain002/apply: status=%d body=%v", code, body)
	}
	if got := pain002TxStateOf(t, body, "st1"); got != "accepted" {
		t.Errorf("st1 state = %q, want accepted", got)
	}
	if got := pain002TxStateOf(t, body, "st2"); got != "rejected" {
		t.Errorf("st2 state = %q, want rejected", got)
	}
	if body["state"] != "exported" {
		t.Errorf("instruction state = %v, want exported", body["state"])
	}

	// The RJCT reason is visible on the transfer record.
	code, rec := pain001StatusOf(t, srv.URL, msgID)
	if code != http.StatusOK {
		t.Fatalf("GET status: %d", code)
	}
	found := false
	for _, item := range rec["txs"].([]any) {
		m := item.(map[string]any)
		if m["tx_id"] == "st2" {
			found = true
			if m["reason"] != "AC03" {
				t.Errorf("st2 reason = %v, want AC03", m["reason"])
			}
		}
	}
	if !found {
		t.Error("st2 missing from status response")
	}

	// Metrics: the apply was counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain002_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_pain002_applies_total 1:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown OrgnlMsgId is a
// 404.
func TestHTTPPain002ApplyErrors(t *testing.T) {
	srv := pain001TestServer(t)
	pain001StatusTestSetup(t, srv.URL) // one exported instruction exists

	if code, body := postPain002XML(t, srv.URL+"/exports/pain002/apply", "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pain002HTTPDoc("PAIN001-NOPE", [3]string{"st1", "ACSC", ""})
	if code, body := postPain002XML(t, srv.URL+"/exports/pain002/apply", doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown OrgnlMsgId: status=%d body=%v, want 404", code, body)
	}
}
