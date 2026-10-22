package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain008TestServer builds a server with IBANs, a live per-day
// mandate (m-http), and one booked debit — ready for pain.008
// export through HTTP.
func pain008TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", ""); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m-http", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency:      ledger.MandateFrequencyPerDay,
		ValidFrom:      day, ValidUntil: day.Add(72 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, err := l.DebitWithMandate("m-http", ledger.MandateDebit{
		ID: "d-http", AmountCents: 1000, CreatedAt: day.Add(10 * time.Hour),
	}); err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func getXML(t *testing.T, url string) (int, string) {
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

// GET /exports/pain008 renders the pain.008.001.02 instruction for
// the mandate's booked collections.
func TestHTTPPain008Export(t *testing.T) {
	srv := pain008TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain008?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.008.001.02"`,
		"<MndtId>m-http</MndtId>",
		"<SeqTp>FRST</SeqTp>",
		"<EndToEndId>d-http</EndToEndId>",
		"<CdtrSchmeId><Id><PrtryId><Id>DE98ZZZ09999999999</Id></PrtryId></Id></CdtrSchmeId>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("document missing %q", want)
		}
	}

	// Missing parameters: 400.
	if code, _ := getXML(t, srv.URL+"/exports/pain008?mandate=m-http"); code != http.StatusBadRequest {
		t.Errorf("missing scheme: status=%d, want 400", code)
	}
	// Unknown mandate: 404.
	if code, _ := getXML(t, srv.URL+"/exports/pain008?mandate=nope&creditor_scheme_id=X"); code != http.StatusNotFound {
		t.Errorf("unknown mandate: status=%d, want 404", code)
	}
	// Bad window: 400.
	if code, _ := getXML(t, srv.URL+"/exports/pain008?mandate=m-http&creditor_scheme_id=X&from=not-a-time"); code != http.StatusBadRequest {
		t.Errorf("bad from: status=%d, want 400", code)
	}
}

// A suspended mandate refuses the export through HTTP too: 422.
func TestHTTPPain008SuspendedMandate(t *testing.T) {
	srv := pain008TestServer(t)
	if code, _ := postJSON(t, srv.URL+"/mandates/m-http/suspend", ""); code != http.StatusOK {
		t.Fatalf("suspend: status=%d", code)
	}
	code, body := getXML(t, srv.URL+"/exports/pain008?mandate=m-http&creditor_scheme_id=X")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("suspended mandate: status=%d body=%s, want 422", code, body)
	}
}
