package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

// submittedPayoutHTTP drafts, approves, and submits a payout through
// HTTP: the issued-but-unsettled state a camt.056 cancellation
// addresses.
func submittedPayoutHTTP(t *testing.T, srvURL, id string) string {
	t.Helper()
	uetr := draftPayoutHTTP(t, srvURL, id)
	if code, _ := postJSON(t, srvURL+"/payouts/"+id+"/approve", `{"approver":"bob"}`); code != http.StatusOK {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	resp, err := http.Post(srvURL+"/payouts/"+id+"/submit", "application/json", nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: status=%d, want 200", resp.StatusCode)
	}
	return uetr
}

func getXMLBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// Full camt.056/camt.029 lifecycle through HTTP: request the
// cancellation, export the camt.056 document, and apply the
// counterparty's camt.029 resolution.
func TestHTTPCancellationLifecycle(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := submittedPayoutHTTP(t, srv.URL, "po-cxl-http-1")

	// Request the cancellation: 201 on first request.
	code, body := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`)
	if code != http.StatusCreated {
		t.Fatalf("request cancellation: status=%d body=%v, want 201", code, body)
	}
	if body["status"] != "requested" {
		t.Errorf("status = %v, want requested", body["status"])
	}
	msgID, _ := body["msg_id"].(string)
	if msgID == "" {
		t.Fatalf("response has no msg_id: %v", body)
	}

	// Repeat request: idempotent 200 with the same record.
	code, body = postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`)
	if code != http.StatusOK {
		t.Fatalf("replay request: status=%d body=%v, want 200", code, body)
	}
	if body["msg_id"] != msgID {
		t.Errorf("replay msg_id = %v, want %v", body["msg_id"], msgID)
	}

	// Export the camt.056 document.
	code, xmlBody := getXMLBody(t, srv.URL+"/exports/camt056?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("camt056 export: status=%d, want 200", code)
	}
	for _, want := range []string{
		"FIToFICxlReq",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<CxlId>" + msgID + "</CxlId>",
		"<Prtry>DUPL</Prtry>",
	} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("camt056 export missing %q", want)
		}
	}

	// Unknown UETR: 404 on request and export.
	if code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":"no-such","reason_code":"DUPL"}`); code != http.StatusNotFound {
		t.Errorf("unknown UETR request: status=%d, want 404", code)
	}
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt056?uetr=no-such"); code != http.StatusNotFound {
		t.Errorf("unknown UETR export: status=%d, want 404", code)
	}
	// Missing reason code: 400.
	if code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`"}`); code != http.StatusBadRequest {
		t.Errorf("missing reason_code: status=%d, want 400", code)
	}

	// Apply the counterparty's camt.029 acceptance: the payout is
	// marked cancelled.
	camt029 := xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.029.001.03">` +
		`<FIToFICxlStsRpt><Assgnmt><Id>bank-rpt-1</Id></Assgnmt><RslvdCxl>` +
		`<OrgnlUETR>` + uetr + `</OrgnlUETR><OrgnlMsgId>po-cxl-http-1</OrgnlMsgId>` +
		`<CxlSts><Cd>ACCR</Cd></CxlSts>` +
		`</RslvdCxl></FIToFICxlStsRpt></Document>`
	resp, err := http.Post(srv.URL+"/exports/camt029/apply", "application/xml", strings.NewReader(camt029))
	if err != nil {
		t.Fatalf("camt029 apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("camt029 apply: status=%d body=%s, want 200", resp.StatusCode, raw)
	}

	// The payout is now cancelled.
	code, payout := getJSONMap(t, srv.URL+"/payouts/po-cxl-http-1")
	if code != http.StatusOK {
		t.Fatalf("get payout: status=%d, want 200", code)
	}
	if payout["status"] != "cancelled" {
		t.Errorf("payout status = %v, want cancelled", payout["status"])
	}

	// Re-request after confirmation: 409 already cancelled.
	if code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`); code != http.StatusConflict {
		t.Errorf("re-request after confirm: status=%d, want 409", code)
	}
	// Export after confirmation: 404, the cancellation is resolved.
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt056?uetr="+uetr); code != http.StatusNotFound {
		t.Errorf("export after confirm: status=%d, want 404", code)
	}

	// Malformed camt.029: 400.
	resp, err = http.Post(srv.URL+"/exports/camt029/apply", "application/xml", strings.NewReader("<Document>"))
	if err != nil {
		t.Fatalf("malformed camt029: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed camt029: status=%d, want 400", resp.StatusCode)
	}
}

// camt.029 rejection through HTTP: the payout stays submitted.
func TestHTTPCancellationRejected(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := submittedPayoutHTTP(t, srv.URL, "po-cxl-http-2")

	if code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"CUST"}`); code != http.StatusCreated {
		t.Fatalf("request: status=%d, want 201", code)
	}
	camt029 := xml.Header + `<Document>` +
		`<FIToFICxlStsRpt><Assgnmt><Id>bank-rpt-2</Id></Assgnmt><RslvdCxl>` +
		`<OrgnlUETR>` + uetr + `</OrgnlUETR><OrgnlMsgId>po-cxl-http-2</OrgnlMsgId>` +
		`<CxlSts><Cd>RJCR</Cd></CxlSts>` +
		`<CxlStsRsnInf><Rsn><Cd>ARDT</Cd></Rsn><AddtlInf>already released</AddtlInf></CxlStsRsnInf>` +
		`</RslvdCxl></FIToFICxlStsRpt></Document>`
	resp, err := http.Post(srv.URL+"/exports/camt029/apply", "application/xml", strings.NewReader(camt029))
	if err != nil {
		t.Fatalf("camt029 apply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("camt029 apply: status=%d body=%s, want 200", resp.StatusCode, raw)
	}
	code, payout := getJSONMap(t, srv.URL+"/payouts/po-cxl-http-2")
	if code != http.StatusOK || payout["status"] != "submitted" {
		t.Errorf("payout after rejection: status=%d body=%v, want 200 submitted", code, payout["status"])
	}
}

// Unissued payouts cannot be cancelled via camt.056.
func TestHTTPCancellationNotIssued(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := draftPayoutHTTP(t, srv.URL, "po-cxl-http-3") // draft only
	if code, body := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("unissued request: status=%d body=%v, want 422", code, body)
	}
}
