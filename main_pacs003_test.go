package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full pacs.003 flow through HTTP: fund, transfer, export the
// FI-to-FI customer direct debit, and exercise the error mapping.
func TestHTTPPacs003Export(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

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

	// Missing transfer: 400.
	if code, _ := getXML(t, srv.URL+"/exports/pacs003"); code != http.StatusBadRequest {
		t.Errorf("missing transfer: status=%d, want 400", code)
	}
	// Unknown transfer: 404.
	if code, _ := getXML(t, srv.URL+"/exports/pacs003?transfer=no-such-tx&dbtr_agt=BANKDEBB&cdtr_agt=BANKCRED&mndt_id=M1"); code != http.StatusNotFound {
		t.Errorf("unknown transfer: status=%d, want 404", code)
	}

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"p3-http-1","from_account":"alice","to_account":"bob","amount_cents":2500,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}

	// Missing agent / mandate: 400.
	base := srv.URL + "/exports/pacs003?transfer=p3-http-1"
	if code, _ := getXML(t, base+"&cdtr_agt=BANKCRED&mndt_id=M1"); code != http.StatusBadRequest {
		t.Errorf("missing dbtr_agt: status=%d, want 400", code)
	}
	if code, _ := getXML(t, base+"&dbtr_agt=BANKDEBB&mndt_id=M1"); code != http.StatusBadRequest {
		t.Errorf("missing cdtr_agt: status=%d, want 400", code)
	}
	if code, _ := getXML(t, base+"&dbtr_agt=BANKDEBB&cdtr_agt=BANKCRED"); code != http.StatusBadRequest {
		t.Errorf("missing mndt_id: status=%d, want 400", code)
	}
	// Malformed mandate date: 400.
	if code, _ := getXML(t, base+"&dbtr_agt=BANKDEBB&cdtr_agt=BANKCRED&mndt_id=M1&mndt_dt=15-01-2026"); code != http.StatusBadRequest {
		t.Errorf("bad mndt_dt: status=%d, want 400", code)
	}

	code, body := getXML(t, base+"&dbtr_agt=BANKDEBB&cdtr_agt=BANKCRED&mndt_id=M1&mndt_dt=2026-01-15")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	for _, want := range []string{
		"pacs.003.001.08",
		"<FIToFICstmrDrctDbt>",
		"<MsgId>p3-http-1/drctdbt</MsgId>",
		"<MndtId>M1</MndtId>",
		"<DtOfSgntr>2026-01-15</DtOfSgntr>",
		`<InstdAmt Ccy="USD">25.00</InstdAmt>`,
		"<DbtrAgt>",
		"<CdtrAgt>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}
}
