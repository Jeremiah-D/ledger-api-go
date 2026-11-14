package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postCAMT026XML posts a raw camt.026 XML document to
// /exports/camt026/apply with an x-ledger-actor header.
func postCAMT026XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// camt026HTTPDoc renders a camt.026.001.07 Unable to Apply report:
// Assgnmt/Id msgID, the Initn underlying arm naming uetr and/or
// endToEndID, and a MssngOrIncrrctInf justification with reasonCd.
func camt026HTTPDoc(msgID, uetr, endToEndID, reasonCd string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.026.001.07">`)
	sb.WriteString(`<UblToApply><Assgnmt><Id>` + msgID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>BANKDEFFXXX</Nm></Assgnr><Assgne><Nm>our-bank</Nm></Assgne>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Undrlyg><Initn><OrgnlGrpInf><OrgnlMsgId>orig-msg-1</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.08</OrgnlMsgNmId></OrgnlGrpInf>`)
	if endToEndID != "" {
		sb.WriteString(`<OrgnlEndToEndId>` + endToEndID + `</OrgnlEndToEndId>`)
	}
	if uetr != "" {
		sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	}
	sb.WriteString(`<OrgnlInstdAmt Ccy="USD">2500.00</OrgnlInstdAmt>`)
	sb.WriteString(`</Initn></Undrlyg>`)
	sb.WriteString(`<Justfn><MssngOrIncrrctInf><MssngInf><Cd>` + reasonCd + `</Cd></MssngInf></MssngOrIncrrctInf></Justfn>`)
	sb.WriteString(`</UblToApply></Document>`)
	return sb.String()
}

// Full flow through HTTP: apply the counterparty's camt.026 report
// against a submitted payout, replay the same MsgId, and the
// metrics counter moves.
func TestHTTPCamt026Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-apply-http-1"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt026/apply"

	doc := camt026HTTPDoc("camt026-http-1", uetr, "", "MS01")
	code, body := postCAMT026XML(t, applyURL, doc, "bank-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt026/apply: status=%d body=%v", code, body)
	}
	if body["kind"] != "payout" || body["target_id"] != payoutID {
		t.Errorf("association = %v/%v, want payout/%s", body["kind"], body["target_id"], payoutID)
	}
	if body["reason_code"] != "MS01" {
		t.Errorf("reason_code = %v, want MS01", body["reason_code"])
	}
	if body["terminal"] != false {
		t.Errorf("terminal = %v, want false", body["terminal"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}

	// The payout itself is untouched: still submitted.
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK {
		t.Fatalf("GET /payouts/%s: status=%d", payoutID, code)
	}
	if payout["status"] != "submitted" {
		t.Errorf("payout status = %v, want submitted", payout["status"])
	}

	// Replay the same report: a no-op success with Duplicate set.
	code, body = postCAMT026XML(t, applyURL, doc, "bank-ops")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/camt026/apply: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay body = %v, want duplicate true", body)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt026_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt026_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: a malformed report is a 400, an unknown reference
// is a 404, and a MsgId reused for a different report is a 422.
func TestHTTPCamt026ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-apply-http-2"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt026/apply"

	if code, body := postCAMT026XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	if code, body := postCAMT026XML(t, applyURL, camt026HTTPDoc("m-1", "no-such-uetr", "", "MS01"), "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	}
	// Apply once, then reuse the MsgId for a different reason.
	if code, body := postCAMT026XML(t, applyURL, camt026HTTPDoc("m-2", uetr, "", "MS01"), "ops"); code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v", code, body)
	}
	if code, body := postCAMT026XML(t, applyURL, camt026HTTPDoc("m-2", uetr, "", "MS02"), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("MsgId conflict: status=%d body=%v, want 422", code, body)
	}
}

// The inbound half leaves the outbound half alone: after a camt.026
// report, GET /exports/camt056 still renders the open cancellation
// request for the same payout.
func TestHTTPCamt026ApplyLeavesExportIntact(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt026-apply-http-3"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt026/apply"

	doc := camt026HTTPDoc("camt026-http-2", uetr, "", "MS01")
	if code, body := postCAMT026XML(t, applyURL, doc, "bank-ops"); code != http.StatusOK {
		t.Fatalf("POST /exports/camt026/apply: status=%d body=%v", code, body)
	}
	// Open a cancellation for the same payout, then export it: the
	// camt.026 record does not disturb the camt.056 flow.
	code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":`+`"`+uetr+`"`+`,"reason_code":"DUPL"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /cancellations: status=%d, want 201", code)
	}
	code, xmlBody := getXMLBody(t, srv.URL+"/exports/camt056?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("GET /exports/camt056: status=%d, want 200", code)
	}
	for _, want := range []string{"FIToFICxlReq", uetr, "DUPL"} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("export missing %q:\n%s", want, xmlBody)
		}
	}
}
