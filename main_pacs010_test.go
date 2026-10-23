package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pacs010TestServer builds a server with IBANs, a live per-day
// mandate (m10-http), and two booked debits — ready for pacs.010
// export and reconciliation through HTTP.
func pacs010TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust10h", DebitAccount: "cust10h", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust10h", "DE89370400440532013000", ""); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant10h", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m10-http", Payer: "cust10h", Payee: "merchant10h",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency:      ledger.MandateFrequencyPerDay,
		ValidFrom:      day, ValidUntil: day.Add(96 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	for i, id := range []string{"d10a", "d10b"} {
		if _, err := l.DebitWithMandate("m10-http", ledger.MandateDebit{
			ID: id, AmountCents: 1000,
			CreatedAt: day.Add(time.Duration(10+i*24) * time.Hour),
		}); err != nil {
			t.Fatalf("DebitWithMandate %s: %v", id, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func TestPacs010ExportEndpoint(t *testing.T) {
	srv := pacs010TestServer(t)
	code, body := getXML(t, srv.URL+"/exports/pacs010?mandate=m10-http")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pacs010: status=%d body=%.200s", code, body)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02"`,
		"<FIDrctDbt>", "<MndtId>m10-http</MndtId>",
		"<EndToEndId>d10a</EndToEndId>", "<EndToEndId>d10b</EndToEndId>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pacs.010 document missing %q", want)
		}
	}
	// Missing mandate parameter: 400. Unknown mandate: 404.
	code, _ = getXML(t, srv.URL+"/exports/pacs010")
	if code != http.StatusBadRequest {
		t.Errorf("missing mandate: status=%d, want 400", code)
	}
	code, _ = getXML(t, srv.URL+"/exports/pacs010?mandate=nope")
	if code != http.StatusNotFound {
		t.Errorf("unknown mandate: status=%d, want 404", code)
	}
	// Bad settlement method: 400.
	code, _ = getXML(t, srv.URL+"/exports/pacs010?mandate=m10-http&sttlm_mtd=XX")
	if code != http.StatusBadRequest {
		t.Errorf("bad sttlm_mtd: status=%d, want 400", code)
	}
}

func TestPacs010ReconcileEndpoint(t *testing.T) {
	srv := pacs010TestServer(t)
	code, body := getJSONMap(t, srv.URL+"/mandates/m10-http/pacs010/reconcile")
	if code != http.StatusOK {
		t.Fatalf("GET reconcile: status=%d body=%v", code, body)
	}
	if n, _ := body["pending"].(float64); n != 2 {
		t.Fatalf("pending = %v, want 2", body["pending"])
	}
	if n, _ := body["notified"].(float64); n != 0 {
		t.Fatalf("notified = %v, want 0", body["notified"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Unknown mandate: 404.
	code, _ = getJSONMap(t, srv.URL+"/mandates/nope/pacs010/reconcile")
	if code != http.StatusNotFound {
		t.Fatalf("unknown mandate reconcile: status=%d, want 404", code, )
	}
}
