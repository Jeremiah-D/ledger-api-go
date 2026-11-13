package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postPacs002XML posts a raw pacs.002 XML document to
// /exports/pacs002/apply with an x-ledger-actor header.
func postPacs002XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs002HTTPDoc renders a pacs.002.001.03 report answering msgID for
// uetr with the given TxSts and optional RJCT reason code.
func pacs002HTTPDoc(msgID, uetr, txSts, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.002.001.03">`)
	sb.WriteString(`<FIToFIPmtStsRpt><GrpHdr><MsgId>RPT-HTTP</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>` + msgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<TxInfAndSts><OrgnlEndToEndId>po-pacs002-apply-http-1</OrgnlEndToEndId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
	if reason != "" {
		sb.WriteString(`<StsRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></StsRsnInf>`)
	}
	sb.WriteString(`</TxInfAndSts></FIToFIPmtStsRpt></Document>`)
	return sb.String()
}

// Full flow through HTTP: draft a payout, apply an ACSC report, then
// an RJCT report carrying the bank's reason code, and the metrics
// counter moves.
func TestHTTPPacs002Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs002-apply-http-1"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	msgID := payoutID + "/pacs008"

	doc := pacs002HTTPDoc(msgID, uetr, "ACSC", "")
	code, body := postPacs002XML(t, srv.URL+"/exports/pacs002/apply", doc, "bank-status")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs002/apply ACSC: status=%d body=%v", code, body)
	}
	if body["status"] != "ACSC" {
		t.Errorf("track status = %v, want ACSC", body["status"])
	}
	if body["uetr"] != uetr {
		t.Errorf("track uetr = %v, want %q", body["uetr"], uetr)
	}

	doc = pacs002HTTPDoc(msgID, uetr, "RJCT", "AC04")
	code, body = postPacs002XML(t, srv.URL+"/exports/pacs002/apply", doc, "bank-status")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs002/apply RJCT: status=%d body=%v", code, body)
	}
	if body["status"] != "RJCT" {
		t.Errorf("track status = %v, want RJCT", body["status"])
	}
	if body["reason_code"] != "AC04" {
		t.Errorf("track reason_code = %v, want AC04", body["reason_code"])
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs002_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs002_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown UETR is a 404, a
// foreign MsgId is a 400, and an illegal TxSts, an RJCT without a
// reason, or a backward move out of a terminal state is a 422.
func TestHTTPPacs002ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs002-apply-http-2"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	msgID := payoutID + "/pacs008"
	applyURL := srv.URL + "/exports/pacs002/apply"

	if code, body := postPacs002XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pacs002HTTPDoc(msgID, "no-such-uetr", "ACSP", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	}
	doc = pacs002HTTPDoc("po-other-payout/pacs008", uetr, "ACSP", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("foreign MsgId: status=%d body=%v, want 400", code, body)
	}
	doc = pacs002HTTPDoc(msgID, uetr, "PDNG", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("illegal TxSts: status=%d body=%v, want 422", code, body)
	}
	doc = pacs002HTTPDoc(msgID, uetr, "RJCT", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("RJCT without reason: status=%d body=%v, want 422", code, body)
	}

	// Settle the track, then a late ACSC report is refused and the
	// track stays settled.
	doc = pacs002HTTPDoc(msgID, uetr, "ACCP", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusOK {
		t.Fatalf("ACCP apply: status=%d body=%v, want 200", code, body)
	}
	doc = pacs002HTTPDoc(msgID, uetr, "ACSC", "")
	if code, body := postPacs002XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("backward move: status=%d body=%v, want 422", code, body)
	}
	code, _, exportBody := getBody(t, srv.URL+"/exports/pacs002?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("re-export: status=%d", code)
	}
	if !strings.Contains(exportBody, "<TxSts>ACCP</TxSts>") {
		t.Errorf("re-export missing <TxSts>ACCP</TxSts>: %s", exportBody)
	}
}
