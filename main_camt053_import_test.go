package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// camt053ImportDoc renders a minimal camt.053 statement for the HTTP
// tests: one matching entry (150.00, posted now) and one unmatched
// entry.
func camt053ImportDoc(posted string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08">
  <BkToCstmrStmt>
    <GrpHdr><MsgId>HTTP-1</MsgId></GrpHdr>
    <Stmt>
      <Id>HTTP-STMT</Id>
      <Acct><Id><IBAN>DE89370400440532013000</IBAN></Id></Acct>
      <Ntry>
        <Amt Ccy="EUR">150.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
        <NtryDtls><TxDtls>
          <Refs><EndToEndId>HTTP-E2E-1</EndToEndId></Refs>
          <RmtInf><Ustrd>payroll</Ustrd></RmtInf>
        </TxDtls></NtryDtls>
      </Ntry>
      <Ntry>
        <Amt Ccy="EUR">9.99</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
      </Ntry>
    </Stmt>
  </BkToCstmrStmt>
</Document>
`
}

func TestCAMT053ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "DE89370400440532013000"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"EUR","memo":"seed","idempotency_key":"camt-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	posted := time.Now().UTC().Format("2006-01-02")
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "camt053": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched entry, one unmatched, not a
	// duplicate.
	code, body := postJSON(t, srv.URL+"/camt053/import", importBody(camt053ImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /camt053/import: status=%d body=%v", code, body)
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
	if id, _ := draft["id"].(string); id != "camt053/HTTP-STMT/1" {
		t.Errorf("item[0].draft.id = %q, want camt053/HTTP-STMT/1", id)
	}
	if da, _ := draft["debit_account"].(string); da != acct {
		t.Errorf("item[0].draft.debit_account = %q, want the statement account (inflow leg)", da)
	}
	txn, _ := first["transaction"].(map[string]any)
	if dir, _ := txn["direction"].(string); dir != "in" {
		t.Errorf("item[0].transaction.direction = %q, want in", dir)
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/camt053/import", importBody(camt053ImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /camt053/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, malformed document,
	// statement for another account.
	for name, payload := range map[string]string{
		"missing account": `{"camt053":"<Document/>"}`,
		"empty document":  `{"account":"` + acct + `","camt053":""}`,
		"blank document":  `{"account":"` + acct + `","camt053":"   "}`,
		"malformed":       `{"account":"` + acct + `","camt053":"<Document><broken"}`,
		"wrong account":   `{"account":"cash","camt053":` + mustMarshal(t, camt053ImportDoc(posted)) + `}`,
	} {
		if code, body := postJSON(t, srv.URL+"/camt053/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}
}

func mustMarshal(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
