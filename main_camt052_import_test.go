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

// camt052ImportDoc renders a minimal camt.052 intraday report for the
// HTTP tests: one matching entry (150.00, posted now), one unmatched
// entry, and OPBD/CLBD balances that agree with the ledger.
func camt052ImportDoc(posted string) string {
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	return `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02">
  <BkToCstmrAcctRpt>
    <GrpHdr><MsgId>HTTP-052-1</MsgId></GrpHdr>
    <Rpt>
      <Id>HTTP-RPT</Id>
      <Acct><Id><IBAN>DE89370400440532013000</IBAN></Id></Acct>
      <Bal>
        <Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="EUR">0.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>` + posted + `</Dt></Dt>
      </Bal>
      <Bal>
        <Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp>
        <Amt Ccy="EUR">150.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <Dt><Dt>` + tomorrow + `</Dt></Dt>
      </Bal>
      <Ntry>
        <Amt Ccy="EUR">150.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
        <NtryDtls><TxDtls>
          <Refs><EndToEndId>HTTP-E2E-052-1</EndToEndId></Refs>
          <RmtInf><Ustrd>payroll</Ustrd></RmtInf>
        </TxDtls></NtryDtls>
      </Ntry>
      <Ntry>
        <Amt Ccy="EUR">9.99</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
      </Ntry>
    </Rpt>
  </BkToCstmrAcctRpt>
</Document>
`
}

// camt052ImportDocNoEntries renders a camt.052 report with balances
// but no Ntry rows: a zero-entry import must be rejected.
func camt052ImportDocNoEntries(posted string) string {
	doc := camt052ImportDoc(posted)
	start := strings.Index(doc, "      <Ntry>")
	end := strings.LastIndex(doc, "      </Ntry>")
	if start < 0 || end < 0 {
		return doc
	}
	return doc[:start] + doc[end+len("      </Ntry>\n"):]
}

func TestCAMT052ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "DE89370400440532013000"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"EUR","memo":"seed","idempotency_key":"camt052-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	posted := time.Now().UTC().Format("2006-01-02")
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "camt052": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched entry, one unmatched, not a
	// duplicate; the OPBD/CLBD balances ride along and agree.
	code, body := postJSON(t, srv.URL+"/camt052/import", importBody(camt052ImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /camt052/import: status=%d body=%v", code, body)
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
	if id, _ := draft["id"].(string); id != "camt053/HTTP-RPT/1" {
		t.Errorf("item[0].draft.id = %q, want camt053/HTTP-RPT/1", id)
	}
	bals, _ := body["balances"].([]any)
	if len(bals) != 2 {
		t.Fatalf("balances = %d, want OPBD+CLBD", len(bals))
	}
	clbd, _ := bals[1].(map[string]any)
	if typ, _ := clbd["Type"].(string); typ != "CLBD" {
		t.Errorf("balances[1].Type = %q, want CLBD", typ)
	}
	if amt, _ := clbd["AmountCents"].(float64); amt != 15000 {
		t.Errorf("balances[1].AmountCents = %v, want 15000", clbd["AmountCents"])
	}
	if _, ok := body["balance_mismatches"]; ok {
		t.Errorf("balance_mismatches present on an agreeing report: %v", body["balance_mismatches"])
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/camt052/import", importBody(camt052ImportDoc(posted)))
	if code != http.StatusOK {
		t.Fatalf("POST /camt052/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, malformed document,
	// zero-Ntry report, statement for another account.
	for name, payload := range map[string]string{
		"missing account": `{"camt052":"<Document/>"}`,
		"empty document":  `{"account":"` + acct + `","camt052":""}`,
		"blank document":  `{"account":"` + acct + `","camt052":"   "}`,
		"malformed":       `{"account":"` + acct + `","camt052":"<Document><broken"}`,
		"zero Ntry":       `{"account":"` + acct + `","camt052":` + mustMarshal(t, camt052ImportDocNoEntries(posted)) + `}`,
		"wrong account":   `{"account":"cash","camt052":` + mustMarshal(t, camt052ImportDoc(posted)) + `}`,
	} {
		if code, body := postJSON(t, srv.URL+"/camt052/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// Metrics: 2 successful imports + 6 rejected payloads = 8 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_camt052_imports_total 8") {
		t.Errorf("metrics missing ledger_camt052_imports_total 8")
	}
}
