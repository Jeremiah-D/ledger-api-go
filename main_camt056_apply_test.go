package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

// postCAMT056XML posts a raw camt.056 XML document to
// /exports/camt056/apply with an x-ledger-actor header.
func postCAMT056XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// camt056HTTPDoc renders a camt.056 cancellation request of the
// given schema version, from assignor, recalling uetr (payoutID)
// for reason.
func camt056HTTPDoc(version, msgID, assignor, uetr, payoutID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.056.` + version + `">`)
	sb.WriteString(`<FIToFICxlReq><Assgnmt><Id>` + msgID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>` + assignor + `</Nm></Assgnr>`)
	sb.WriteString(`<Assgte><Nm>our-bank</Nm></Assgte>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Undrlyg><TxInf><CxlId>` + msgID + `</CxlId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + payoutID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	sb.WriteString(`<CxlRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsn>`)
	sb.WriteString(`</TxInf></Undrlyg></FIToFICxlReq></Document>`)
	return sb.String()
}

// camt029HTTPDoc renders a minimal camt.029.001.03 resolution for
// uetr with the given CxlSts.
func camt029HTTPDoc(uetr, payoutID, cxlSts string) string {
	return xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.029.001.03">` +
		`<FIToFICxlStsRpt><Assgnmt><Id>bank-rpt-56</Id></Assgnmt><RslvdCxl>` +
		`<OrgnlUETR>` + uetr + `</OrgnlUETR><OrgnlMsgId>` + payoutID + `</OrgnlMsgId>` +
		`<CxlSts><Cd>` + cxlSts + `</Cd></CxlSts>` +
		`</RslvdCxl></FIToFICxlStsRpt></Document>`
}

// postCAMT029XML posts a raw camt.029 XML document to
// /exports/camt029/apply and returns the status code.
func postCAMT029XML(t *testing.T, url, xmlDoc string) int {
	t.Helper()
	resp, err := http.Post(url, "application/xml", strings.NewReader(xmlDoc))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode
}

// Full flow through HTTP: submit a payout, apply the counterparty's
// camt.056 cancellation request, replay the same MsgId, export the
// open cancellation, resolve it with the counterparty's camt.029,
// and the metrics counter moves.
func TestHTTPCAMT056Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt056-apply-http-1"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt056/apply"

	doc := camt056HTTPDoc("001.08", "cxl-in-http-1", "BANKDEFFXXX", uetr, payoutID, "DUPL")
	code, body := postCAMT056XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt056/apply: status=%d body=%v", code, body)
	}
	if body["status"] != "requested" {
		t.Errorf("status = %v, want requested", body["status"])
	}
	if body["msg_id"] != uetr+"/cxl/1" {
		t.Errorf("msg_id = %v, want %s/cxl/1", body["msg_id"], uetr)
	}
	if body["seq"] != float64(1) {
		t.Errorf("seq = %v, want 1", body["seq"])
	}
	if body["reason_code"] != "DUPL" {
		t.Errorf("reason_code = %v, want DUPL", body["reason_code"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	// RequestedBy names the counterparty from the document, not
	// the operator who applied it.
	if body["requested_by"] != "BANKDEFFXXX" {
		t.Errorf("requested_by = %v, want BANKDEFFXXX", body["requested_by"])
	}

	// The GET export is unaffected: the open cancellation still
	// renders.
	code, xmlBody := getXMLBody(t, srv.URL+"/exports/camt056?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("GET /exports/camt056: status=%d, want 200", code)
	}
	for _, want := range []string{"FIToFICxlReq", uetr, "DUPL"} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("export missing %q:\n%s", want, xmlBody)
		}
	}

	// Replay the same MsgId: a no-op success with the same record.
	code, body = postCAMT056XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/camt056/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != uetr+"/cxl/1" || body["seq"] != float64(1) {
		t.Errorf("replay body = %v, want the same record", body)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt056_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt056_applies_total 2:\n%s", metricsBody)
	}

	// The counterparty's camt.029 resolves the cancellation the
	// counterparty itself opened: the payout is cancelled.
	if code := postCAMT029XML(t, srv.URL+"/exports/camt029/apply", camt029HTTPDoc(uetr, payoutID, "ACCR")); code != http.StatusOK {
		t.Fatalf("camt029 apply: status=%d, want 200", code)
	}
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK || payout["status"] != "cancelled" {
		t.Errorf("payout after resolve = status %d body %v, want 200 cancelled", code, payout)
	}
}

// Error mapping: malformed XML and a wrong schema version are 400,
// a missing UETR or an empty reason code are 400, an unknown UETR
// is a 404, an unissued or settled payout is a 422, and an
// already-cancelled payout is a 422.
func TestHTTPCAMT056ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt056-apply-http-2"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt056/apply"

	if code, body := postCAMT056XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := camt056HTTPDoc("001.02", "cxl-in-http-2", "BANKDEFFXXX", uetr, payoutID, "DUPL")
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("wrong version: status=%d body=%v, want 400", code, body)
	}
	doc = camt056HTTPDoc("001.08", "cxl-in-http-2", "BANKDEFFXXX", "", payoutID, "DUPL")
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("missing UETR: status=%d body=%v, want 400", code, body)
	}
	doc = camt056HTTPDoc("001.08", "cxl-in-http-2", "BANKDEFFXXX", uetr, payoutID, "")
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("empty reason: status=%d body=%v, want 400", code, body)
	}
	// Unknown UETR: 404.
	doc = camt056HTTPDoc("001.08", "cxl-in-http-2", "BANKDEFFXXX", "no-such-uetr", payoutID, "DUPL")
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	}
	// Draft payout: 422 — only issued money can be recalled.
	doc = camt056HTTPDoc("001.08", "cxl-in-http-2", "BANKDEFFXXX", uetr, payoutID, "DUPL")
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d body=%v, want 422", code, body)
	}

	// Settled payout: 422.
	settlePayoutHTTP(t, srv.URL, payoutID)
	if code, body := postCAMT056XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("settled payout: status=%d body=%v, want 422", code, body)
	}

	// Already-cancelled payout: 422.
	payoutID2 := "po-camt056-apply-http-3"
	uetr2 := submittedPayoutHTTP(t, srv.URL, payoutID2)
	doc2 := camt056HTTPDoc("001.08", "cxl-in-http-3", "BANKDEFFXXX", uetr2, payoutID2, "DUPL")
	if code, body := postCAMT056XML(t, applyURL, doc2, "ops"); code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	if code := postCAMT029XML(t, srv.URL+"/exports/camt029/apply", camt029HTTPDoc(uetr2, payoutID2, "ACCR")); code != http.StatusOK {
		t.Fatalf("camt029 apply: status=%d, want 200", code)
	}
	doc2 = strings.Replace(doc2, "cxl-in-http-3", "cxl-in-http-4", 1)
	if code, body := postCAMT056XML(t, applyURL, doc2, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("already cancelled: status=%d body=%v, want 422", code, body)
	}
}
