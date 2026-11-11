package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

// getXMLBodyWithType is getXMLBody plus the response Content-Type:
// camt.055 must answer application/xml, like camt.056.
func getXMLBodyWithType(t *testing.T, url string) (int, string, string) {
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
	return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
}

// camt.055 export through HTTP: request the cancellation, export the
// customer-side document, resolve it, and confirm the export then
// 404s — the camt.056 lifecycle in the customer's shape.
func TestHTTPCAMT055Export(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := submittedPayoutHTTP(t, srv.URL, "po-cxl55-http-1")

	code, body := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`)
	if code != http.StatusCreated {
		t.Fatalf("request cancellation: status=%d body=%v, want 201", code, body)
	}
	msgID, _ := body["msg_id"].(string)
	if msgID == "" {
		t.Fatalf("response has no msg_id: %v", body)
	}

	// Export the camt.055 document.
	code, xmlBody, ctype := getXMLBodyWithType(t, srv.URL+"/exports/camt055?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("camt055 export: status=%d, want 200", code)
	}
	if ctype != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", ctype)
	}
	for _, want := range []string{
		"CstmrPmtCxlReq",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<CxlId>" + msgID + "</CxlId>",
		"<OrgnlInstrId>po-cxl55-http-1</OrgnlInstrId>",
		"<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId>",
		"<Prtry>DUPL</Prtry>",
	} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("camt055 export missing %q", want)
		}
	}

	// Missing uetr: 400. Unknown UETR: 404.
	if code, _, _ := getXMLBodyWithType(t, srv.URL+"/exports/camt055"); code != http.StatusBadRequest {
		t.Errorf("missing uetr: status=%d, want 400", code)
	}
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt055?uetr=no-such"); code != http.StatusNotFound {
		t.Errorf("unknown UETR export: status=%d, want 404", code)
	}

	// The metrics counter records the exports above, including the
	// rejected ones.
	if got := metricsBody(t, srv.URL); !strings.Contains(got, "ledger_camt055_exports_total") {
		t.Errorf("metrics missing ledger_camt055_exports_total:\n%s", got)
	}

	// Resolve the cancellation (counterparty accepts): the export
	// then 404s, the record is no longer open.
	camt029 := xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.029.001.03">` +
		`<FIToFICxlStsRpt><Assgnmt><Id>bank-rpt-55</Id></Assgnmt><RslvdCxl>` +
		`<OrgnlUETR>` + uetr + `</OrgnlUETR><OrgnlMsgId>po-cxl55-http-1</OrgnlMsgId>` +
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
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt055?uetr="+uetr); code != http.StatusNotFound {
		t.Errorf("export after confirm: status=%d, want 404", code)
	}
}
