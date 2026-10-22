package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full pacs.009 flow through HTTP: fund, transfer, export the FI
// credit transfer, and exercise the error mapping.
func TestHTTPPacs009Export(t *testing.T) {
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
	if code, _ := getXML(t, srv.URL+"/exports/pacs009"); code != http.StatusBadRequest {
		t.Errorf("missing transfer: status=%d, want 400", code)
	}
	// Unknown transfer: 404.
	if code, _ := getXML(t, srv.URL+"/exports/pacs009?transfer=no-such-tx"); code != http.StatusNotFound {
		t.Errorf("unknown transfer: status=%d, want 404", code)
	}

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"p9-http-1","from_account":"alice","to_account":"bob","amount_cents":2500,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}

	// Bad vocabulary: 400.
	if code, _ := getXML(t, srv.URL+"/exports/pacs009?transfer=p9-http-1&sttlm_mtd=WIRE"); code != http.StatusBadRequest {
		t.Errorf("bad sttlm_mtd: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pacs009?transfer=p9-http-1&chrgs_br=NOBODY"); code != http.StatusBadRequest {
		t.Errorf("bad chrgs_br: status=%d, want 400", code)
	}

	code, body := getXML(t, srv.URL+"/exports/pacs009?transfer=p9-http-1")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	for _, want := range []string{
		"pacs.009.001.06",
		"<FICdtTrf>",
		"<MsgId>p9-http-1/ficdt</MsgId>",
		"<SttlmMtd>INDA</SttlmMtd>",
		"<ChrgsBr>SLEV</ChrgsBr>",
		`<IntrBkSttlmAmt Ccy="USD">25.00</IntrBkSttlmAmt>`,
		"<DbtrAgt>",
		"<CdtrAgt>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}

	// Custom vocabulary renders.
	code, body = getXML(t, srv.URL+"/exports/pacs009?transfer=p9-http-1&sttlm_mtd=COVE&chrgs_br=DEBT")
	if code != http.StatusOK {
		t.Fatalf("custom export: status=%d, want 200", code)
	}
	if !strings.Contains(body, "<SttlmMtd>COVE</SttlmMtd>") || !strings.Contains(body, "<ChrgsBr>DEBT</ChrgsBr>") {
		t.Errorf("custom export missing COVE/DEBT: %s", body)
	}

	// The export counter moved (the /metrics endpoint is authenticated
	// in some configs; the counter path is covered at the unit level —
	// here we only assert the export itself succeeded).
}
