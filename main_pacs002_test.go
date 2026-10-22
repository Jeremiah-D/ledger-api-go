package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Full pacs.002 flow through HTTP: draft a payout, export the status
// report, advance the track, and re-export.
func TestHTTPPacs002Export(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := draftPayoutHTTP(t, srv.URL, "po-pacs002-http-1")

	getXML := func(t *testing.T, url string) (int, string) {
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

	// Missing uetr: 400.
	if code, _ := getXML(t, srv.URL+"/exports/pacs002"); code != http.StatusBadRequest {
		t.Errorf("missing uetr: status=%d, want 400", code)
	}
	// Unknown UETR: 404.
	if code, _ := getXML(t, srv.URL+"/exports/pacs002?uetr=no-such-uetr"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d, want 404", code)
	}

	code, body := getXML(t, srv.URL+"/exports/pacs002?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	for _, want := range []string{
		"FIToFIPmtStsRpt",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<TxSts>ACSP</TxSts>",
		"pacs.008.001.02",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}

	// Advance the track to ACCP: the report follows the mirror.
	code, _ = postJSON(t, srv.URL+"/gpi/track/"+uetr+"/advance", `{"status":"ACCP"}`)
	if code != http.StatusOK {
		t.Fatalf("advance: status=%d, want 200", code)
	}
	code, body = getXML(t, srv.URL+"/exports/pacs002?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("re-export: status=%d, want 200", code)
	}
	if !strings.Contains(body, "<TxSts>ACCP</TxSts>") {
		t.Errorf("re-export missing <TxSts>ACCP</TxSts>: %s", body)
	}
}
