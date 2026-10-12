package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// ofxImportDoc renders a minimal SGML-style OFX statement for the HTTP
// tests: one matching line (150.00, posted now) and one unmatched line.
func ofxImportDoc(posted string) string {
	return `OFXHEADER:100
DATA:OFXSGML
VERSION:102
SECURITY:NONE
ENCODING:USASCII
CHARSET:1252
COMPRESSION:NONE
OLDFILEUID:NONE
NEWFILEUID:NONE

<OFX>
<BANKMSGSRSV1>
<STMTTRNRS>
<STATUS>
<CODE>0
<SEVERITY>INFO
</STATUS>
<STMTRS>
<CURDEF>USD
<BANKACCTFROM>
<BANKID>LEDGER
<ACCTID>cash
<ACCTTYPE>CHECKING
</BANKACCTFROM>
<BANKTRANLIST>
<STMTTRN>
<TRNTYPE>CREDIT
<DTPOSTED>` + posted + `
<TRNAMT>150.00
<FITID>http-1
<MEMO>payroll
</STMTTRN>
<STMTTRN>
<TRNTYPE>DEBIT
<DTPOSTED>` + posted + `
<TRNAMT>-9.99
<FITID>http-2
</STMTTRN>
</BANKTRANLIST>
</STMTRS>
</STMTTRNRS>
</BANKMSGSRSV1>
</OFX>
`
}

func TestOFXImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"ofx-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	posted := time.Now().UTC().Format("20060102150405")
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": "cash", "ofx": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched line, one unmatched, not a duplicate.
	code, body := postJSON(t, srv.URL+"/ofx/import", importBody(ofxImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /ofx/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	if n, _ := body["unmatched"].(float64); n != 1 {
		t.Errorf("unmatched = %v, want 1", body["unmatched"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	first, _ := items[0].(map[string]any)
	if st, _ := first["status"].(string); st != "matched" {
		t.Errorf("item[0].status = %q, want matched", st)
	}
	draft, _ := first["draft"].(map[string]any)
	if id, _ := draft["id"].(string); id != "ofx/http-1" {
		t.Errorf("item[0].draft.id = %q, want ofx/http-1", id)
	}
	if da, _ := draft["debit_account"].(string); da != "cash" {
		t.Errorf("item[0].draft.debit_account = %q, want cash (inflow leg)", da)
	}
	txn, _ := first["transaction"].(map[string]any)
	if dir, _ := txn["direction"].(string); dir != "in" {
		t.Errorf("item[0].transaction.direction = %q, want in", dir)
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/ofx/import", importBody(ofxImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /ofx/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, malformed document,
	// statement for another account.
	for name, payload := range map[string]string{
		"missing account": `{"ofx":"<OFX></OFX>"}`,
		"empty document":  `{"account":"cash","ofx":""}`,
		"blank document":  `{"account":"cash","ofx":"   "}`,
		"malformed":       `{"account":"cash","ofx":"not an ofx document"}`,
		"empty trn":       `{"account":"cash","ofx":"<OFX><STMTTRNRS><CURDEF>USD</CURDEF></STMTTRNRS></OFX>"}`,
	} {
		if code, body := postJSON(t, srv.URL+"/ofx/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 (body=%v)", name, code, body)
		}
	}
	otherAcct := strings.Replace(ofxImportDoc(posted), "<ACCTID>cash", "<ACCTID>savings", 1)
	if code, _ := postJSON(t, srv.URL+"/ofx/import", importBody(otherAcct)); code != http.StatusBadRequest {
		t.Errorf("account mismatch: status=%d, want 400", code)
	}
	// Bad JSON is a 400 from the strict decoder, not a 500.
	resp, err := http.Post(srv.URL+"/ofx/import", "application/json", strings.NewReader("{bad json"))
	if err != nil {
		t.Fatalf("POST bad json: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON: status=%d, want 400", resp.StatusCode)
	}
}
