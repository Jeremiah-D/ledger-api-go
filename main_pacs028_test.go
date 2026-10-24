package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Full pacs.028 flow through HTTP: draft a payout, request its status,
// and check the error mapping.
func TestHTTPPacs028Export(t *testing.T) {
	srv := gpiTestServer(t)
	uetr := draftPayoutHTTP(t, srv.URL, "po-pacs028-http-1")

	getXML := func(t *testing.T, url string) (int, string, string) {
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
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
	}

	// Missing uetr: 400.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs028?requester=ledger-ops"); code != http.StatusBadRequest {
		t.Errorf("missing uetr: status=%d, want 400", code)
	}
	// Missing requester: 400.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs028?uetr="+uetr); code != http.StatusBadRequest {
		t.Errorf("missing requester: status=%d, want 400", code)
	}
	// Unknown UETR: 404.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs028?uetr=no-such-uetr&requester=ledger-ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d, want 404", code)
	}

	code, contentType, body := getXML(t, srv.URL+"/exports/pacs028?uetr="+uetr+"&requester=ledger-ops")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if contentType != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", contentType)
	}
	for _, want := range []string{
		"FIToFIPmtStsReq",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<Id>ledger-ops</Id>",
		"pacs.008.001.02",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}
}
