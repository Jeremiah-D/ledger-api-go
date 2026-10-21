package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// camt054ImportDoc renders a minimal camt.054 notification document for
// the HTTP tests: one 500.00 USD credit notification naming the given
// EndToEndId, booked today.
func camt054ImportDoc(e2e string) string {
	posted := time.Now().UTC().Format("2006-01-02")
	return `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.054.001.08">
  <BkToCstmrDbtCdtNtfctn>
    <GrpHdr><MsgId>HTTP-054-1</MsgId></GrpHdr>
    <Ntfctn>
      <Id>HTTP-NTF-054</Id>
      <Acct><Id><IBAN>acct-notify</IBAN></Id></Acct>
      <Ntry>
        <Amt Ccy="USD">500.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
        <NtryDtls><TxDtls>
          <Refs><EndToEndId>` + e2e + `</EndToEndId></Refs>
          <RmtInf><Ustrd>http test</Ustrd></RmtInf>
        </TxDtls></NtryDtls>
      </Ntry>
    </Ntfctn>
  </BkToCstmrDbtCdtNtfctn>
</Document>
`
}

func TestCAMT054ImportEndpoint(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Originate a transfer the notification's EndToEndId will name.
	xferID := "xfer-http-054"
	if _, err := l.PostTransfer(ledger.Transfer{
		ID:             xferID,
		From:           "acct-payer",
		To:             "acct-notify",
		AmountCents:    50000,
		Currency:       "USD",
		IdempotencyKey: "http-054-key2",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	camt054Body := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": "acct-notify", "camt054": doc})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		return string(raw)
	}

	// Import: 200, one matched notification.
	code, body := postJSON(t, srv.URL+"/camt054/import", camt054Body(camt054ImportDoc(xferID)))
	if code != http.StatusOK {
		t.Fatalf("POST /camt054/import: status=%d body=%v", code, body)
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item, _ := items[0].(map[string]any)
	if st, _ := item["status"].(string); st != "matched" {
		t.Errorf("status = %q, want matched", st)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true")
	}

	// Re-import: duplicate.
	code, body = postJSON(t, srv.URL+"/camt054/import", camt054Body(camt054ImportDoc(xferID)))
	if code != http.StatusOK {
		t.Fatalf("re-import: status=%d", code)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("re-import: duplicate = false")
	}

	// A document naming a different account is a 400.
	raw, _ := json.Marshal(map[string]string{"account": "acct-other", "camt054": camt054ImportDoc(xferID)})
	code, _ = postJSON(t, srv.URL+"/camt054/import", string(raw))
	if code != http.StatusBadRequest {
		t.Errorf("wrong account: status=%d, want 400", code)
	}

	// An empty document is a 400.
	raw, _ = json.Marshal(map[string]string{"account": "acct-notify", "camt054": ""})
	code, _ = postJSON(t, srv.URL+"/camt054/import", string(raw))
	if code != http.StatusBadRequest {
		t.Errorf("empty doc: status=%d, want 400", code)
	}
}
