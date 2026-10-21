package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// fedNowPacs008Doc renders a minimal pacs.008 document for the HTTP
// tests: one 250.00 USD credit transfer from acct-debtor to acct-payee.
func fedNowPacs008Doc() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>HTTP-FN-001</MsgId>
      <CreDtTm>2026-10-21T10:00:00Z</CreDtTm>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId><EndToEndId>HTTP-E2E-FN-1</EndToEndId><TxId>HTTP-TX-1</TxId></PmtId>
      <IntrBkSttlmAmt Ccy="USD">250.00</IntrBkSttlmAmt>
      <Dbtr><Nm>HTTP Payer</Nm></Dbtr>
      <DbtrAcct><Id><IBAN>acct-debtor</IBAN></Id></DbtrAcct>
      <Cdtr><Nm>HTTP Payee</Nm></Cdtr>
      <CdtrAcct><Id><IBAN>acct-payee</IBAN></Id></CdtrAcct>
      <RmtInf><Ustrd>invoice 123</Ustrd></RmtInf>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>
`
}

func TestFedNowPacs008Endpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Seed the payee account and configure the settlement account via
	// a direct ledger handle: the HTTP surface does not expose account
	// wiring, only the money movement.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"acct-payee","credit_account":"equity","amount_cents":1000,"currency":"USD","memo":"seed","idempotency_key":"fn-http-seed"}`)
	if code != http.StatusCreated {
		t.Fatalf("post seed: status=%d", code)
	}

	pacs008Body := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"pacs008": doc})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(raw)
	}

	// Without a configured settlement account the receipt must not
	// book: the endpoint still answers 200 with the per-item rejection.
	code, body := postJSON(t, srv.URL+"/fednow/pacs008", pacs008Body(fedNowPacs008Doc()))
	if code != http.StatusOK {
		t.Fatalf("POST /fednow/pacs008 (no settle acct): status=%d body=%v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	item, _ := results[0].(map[string]any)
	if errMsg, _ := item["error"].(string); errMsg == "" {
		t.Errorf("item error empty, want fail-closed rejection")
	}

	// A malformed document refuses the whole request with 400.
	code, _ = postJSON(t, srv.URL+"/fednow/pacs008", pacs008Body("<Document>"))
	if code != http.StatusBadRequest {
		t.Errorf("malformed doc: status=%d, want 400", code)
	}

	// An empty body refuses with 400.
	code, _ = postJSON(t, srv.URL+"/fednow/pacs008", `{"pacs008":""}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty doc: status=%d, want 400", code)
	}
}

func TestFedNowPacs008EndpointBooks(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	l.SetFedNowSettlementAccount("acct-fednow-settle")

	// Seed the payee so the receipt can land.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"acct-payee","credit_account":"equity","amount_cents":1000,"currency":"USD","memo":"seed","idempotency_key":"fn-http-seed2"}`)
	if code != http.StatusCreated {
		t.Fatalf("post seed: status=%d", code)
	}

	raw, _ := json.Marshal(map[string]string{"pacs008": fedNowPacs008Doc()})
	code, body := postJSON(t, srv.URL+"/fednow/pacs008", string(raw))
	if code != http.StatusOK {
		t.Fatalf("POST /fednow/pacs008: status=%d body=%v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	item, _ := results[0].(map[string]any)
	if errMsg, _ := item["error"].(string); errMsg != "" {
		t.Fatalf("item error = %q", errMsg)
	}
	if e2e, _ := item["end_to_end_id"].(string); e2e != "HTTP-E2E-FN-1" {
		t.Errorf("end_to_end_id = %q", e2e)
	}
	if eid, _ := item["entry_id"].(string); eid != "fednow-pacs008/HTTP-E2E-FN-1" {
		t.Errorf("entry_id = %q", eid)
	}
	if dup, _ := item["duplicate"].(bool); dup {
		t.Errorf("first post: duplicate = true")
	}
	// The payee received the 250.00.
	if bal := l.BalanceIn("acct-payee", "USD"); bal != 1000+25000 {
		t.Errorf("payee balance = %d, want %d", bal, 1000+25000)
	}
	// A replay returns the original entry with duplicate: true.
	code, body = postJSON(t, srv.URL+"/fednow/pacs008", string(raw))
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d", code)
	}
	results, _ = body["results"].([]any)
	item, _ = results[0].(map[string]any)
	if dup, _ := item["duplicate"].(bool); !dup {
		t.Errorf("replay: duplicate = false")
	}
	// And the balance did not move again.
	if bal := l.BalanceIn("acct-payee", "USD"); bal != 1000+25000 {
		t.Errorf("after replay: payee balance = %d, want %d", bal, 1000+25000)
	}
}
