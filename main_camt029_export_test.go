package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

// End-to-end GET /exports/camt029 lifecycle through HTTP: request the
// cancellation, apply the counterparty's camt.029 resolution, then
// export the recorded resolution as the outbound camt.029 document.
func TestHTTPCAMT029Export(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := submittedPayoutHTTP(t, srv.URL, "po-c029-http-1")

	// Request the cancellation.
	if code, _ := postJSON(t, srv.URL+"/cancellations", `{"uetr":"`+uetr+`","reason_code":"DUPL"}`); code != http.StatusCreated {
		t.Fatalf("request cancellation: status=%d, want 201", code)
	}

	// Export before resolution: 404, there is no verdict yet.
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt029?cancellation="+uetr); code != http.StatusNotFound {
		t.Fatalf("unresolved export: status=%d, want 404", code)
	}

	// Apply the counterparty's camt.029 rejection.
	camt029 := xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.029.001.03">` +
		`<FIToFICxlStsRpt><Assgnmt><Id>bank-rpt-1</Id></Assgnmt><RslvdCxl>` +
		`<OrgnlUETR>` + uetr + `</OrgnlUETR><OrgnlMsgId>po-c029-http-1</OrgnlMsgId>` +
		`<CxlSts><Cd>RJCR</Cd></CxlSts>` +
		`<CxlStsRsnInf><Rsn><Cd>RJCR</Cd></Rsn><AddtlInf>duplicate payment</AddtlInf></CxlStsRsnInf>` +
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

	// Export the recorded resolution.
	code, xmlBody := getXMLBody(t, srv.URL+"/exports/camt029?cancellation="+uetr)
	if code != http.StatusOK {
		t.Fatalf("camt029 export: status=%d, want 200", code)
	}
	for _, want := range []string{
		"FIToFICxlStsRpt",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<Cd>RJCR</Cd>",
		"duplicate payment",
	} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("camt029 export missing %q", want)
		}
	}

	// Missing parameter: 400. Unknown UETR: 404.
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt029"); code != http.StatusBadRequest {
		t.Errorf("missing cancellation: status=%d, want 400", code)
	}
	if code, _ := getXMLBody(t, srv.URL+"/exports/camt029?cancellation=no-such"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d, want 404", code)
	}

	// The export counter moved, including the rejected attempts above.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_camt029_exports_total"] != 4 {
		t.Errorf("ledger_camt029_exports_total = %d, want 4 (1 served + 3 rejected)", m["ledger_camt029_exports_total"])
	}
}
