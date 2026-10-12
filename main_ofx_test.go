package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestOFXExportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"ofx-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d body=%v", code, body)
	}
	id1, _ := body["id"].(string)
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"rent","credit_account":"cash","amount_cents":4000,"currency":"USD","idempotency_key":"ofx-e2"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e2: status=%d body=%v", code, body)
	}
	id2, _ := body["id"].(string)

	getOFX := func(url string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), sb.String()
	}

	code, ct, s := getOFX(srv.URL + "/accounts/cash/ofx?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /ofx: status=%d", code)
	}
	if !strings.HasPrefix(ct, "application/x-ofx") {
		t.Fatalf("Content-Type = %q, want application/x-ofx", ct)
	}
	for _, want := range []string{
		"OFXHEADER:100", "<CURDEF>USD</CURDEF>", "<TRNTYPE>CREDIT</TRNTYPE>",
		"<TRNAMT>150.00</TRNAMT>", "<TRNTYPE>DEBIT</TRNTYPE>", "<TRNAMT>-40.00</TRNAMT>",
		"<FITID>" + id1 + "</FITID>", "<FITID>" + id2 + "</FITID>",
		"<MEMO>seed</MEMO>", "<BALAMT>110.00</BALAMT>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("OFX body missing %q\n%s", want, s)
		}
	}

	// Resume via the last FITID returns an empty page: nothing after it.
	lastID := id1
	if strings.Index(s, "<FITID>"+id2+"</FITID>") > strings.Index(s, "<FITID>"+id1+"</FITID>") {
		lastID = id2
	}
	resp, err := http.Get(srv.URL + "/accounts/cash/ofx?currency=USD&after_entry_id=" + lastID)
	if err != nil {
		t.Fatalf("GET /ofx resumed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resumed GET /ofx: status=%d", resp.StatusCode)
	}
	if h := resp.Header.Get("X-Ledger-OFX-Checkpoint"); h != "" {
		t.Fatalf("empty page checkpoint = %q, want empty", h)
	}

	// Bad params are 400, not invented output.
	if code, _ := getStatus(t, srv.URL+"/accounts/cash/ofx?currency=XX"); code != http.StatusBadRequest {
		t.Fatalf("bad currency: status=%d, want 400", code)
	}
	if code, _ := getStatus(t, srv.URL+"/accounts/cash/ofx?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z"); code != http.StatusBadRequest {
		t.Fatalf("inverted range: status=%d, want 400", code)
	}
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Content-Type")
}
