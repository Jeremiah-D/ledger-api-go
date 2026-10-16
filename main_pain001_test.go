package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func pain001TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func getBody(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestPain001ExportEndpoint(t *testing.T) {
	srv := pain001TestServer(t)

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"hp1","from_account":"alice","to_account":"bob","amount_cents":12345,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("post transfer hp1: status=%d", code)
	}

	code, ct, s := getBody(t, srv.URL+"/exports/pain001?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain001: status=%d body=%s", code, s)
	}
	if !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("Content-Type = %q, want application/xml", ct)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.001.03"`,
		"<NbOfTxs>1</NbOfTxs>",
		"<CtrlSum>123.45</CtrlSum>",
		"<EndToEndId>hp1</EndToEndId>",
		"<IBAN>DE89370400440532013000</IBAN>",
		"<IBAN>GB29NWBK60161331926819</IBAN>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pain.001 body missing %q\n%s", want, s)
		}
	}

	// Debtor filter that matches nothing: valid empty document.
	code, _, s = getBody(t, srv.URL+"/exports/pain001?debtor=ghost")
	if code != http.StatusOK {
		t.Fatalf("empty filter export: status=%d", code)
	}
	if !strings.Contains(s, "<NbOfTxs>0</NbOfTxs>") {
		t.Errorf("empty export should report zero transactions\n%s", s)
	}

	// Bad params are 400, not invented output.
	for _, url := range []string{
		srv.URL + "/exports/pain001?from=not-a-time",
		srv.URL + "/exports/pain001?to=2026-13-99T00:00:00Z",
		srv.URL + "/exports/pain001?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z",
		srv.URL + "/exports/pain001?currency=XX",
	} {
		if code, _, _ := getBody(t, url); code != http.StatusBadRequest {
			t.Errorf("GET %s: status=%d, want 400", url, code)
		}
	}
}

// A transfer touching an account without a registered IBAN fails the
// whole export with 422 (fail-closed), not a forged identifier.
func TestPain001ExportMissingIBAN422(t *testing.T) {
	srv := pain001TestServer(t)

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"hp2","from_account":"alice","to_account":"stranger","amount_cents":100,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("post transfer hp2: status=%d", code)
	}
	code, _, body := getBody(t, srv.URL+"/exports/pain001")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("missing IBAN export: status=%d, want 422 (body %s)", code, body)
	}
}
