package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain011TestServer builds a server whose mandates cover every
// pain.011 state gate: active, suspended, cancelled, expired, and
// one with no registered IBANs on either side.
func pain011TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust2", DebitAccount: "cust2", CreditAccount: "equity",
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
	mk := func(id, payer, payee string, from, until time.Time) {
		t.Helper()
		if _, err := l.CreateMandate(ledger.Mandate{
			ID: id, Payer: ledger.AccountID(payer), Payee: ledger.AccountID(payee),
			MaxAmountCents: 5000, Currency: "USD",
			Frequency:  ledger.MandateFrequencyPerMonth,
			ValidFrom:  from,
			ValidUntil: until,
		}); err != nil {
			t.Fatalf("CreateMandate %s: %v", id, err)
		}
	}
	mk("m-active", "cust", "merchant", day, day.AddDate(1, 0, 0))
	mk("m-suspended", "cust", "merchant", day, day.AddDate(1, 0, 0))
	mk("m-cancelled", "cust", "merchant", day, day.AddDate(1, 0, 0))
	mk("m-expired", "cust", "merchant", day.AddDate(0, -2, 0), day.AddDate(0, -1, 0))
	mk("m-noiban", "cust2", "merchant", day, day.AddDate(1, 0, 0))
	if _, err := l.SuspendMandate("m-suspended"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.CancelMandate("m-cancelled"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPPain011Export(t *testing.T) {
	srv := pain011TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain011?mandate=m-active&creditor_scheme_id=DE98ZZZ09999999999&reason=MD16")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.011.001.03"`,
		"<MndtCxlReq>",
		"<MsgId>pain011-m-active-DE98ZZZ09999999999</MsgId>",
		"<MndtId>m-active</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<CxlRsn><Rsn><Prtry>MD16</Prtry></Rsn></CxlRsn>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<IBAN>") {
		t.Error("pain.011 export carries an IBAN element")
	}

	// The counter counts this one served export.
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain011_exports_total 1") {
		t.Errorf("metrics missing ledger_pain011_exports_total 1")
	}
}

func TestHTTPPain011ExportErrors(t *testing.T) {
	srv := pain011TestServer(t)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing params", srv.URL + "/exports/pain011", http.StatusBadRequest},
		{"missing scheme", srv.URL + "/exports/pain011?mandate=m-active", http.StatusBadRequest},
		{"unknown mandate", srv.URL + "/exports/pain011?mandate=m-ghost&creditor_scheme_id=X", http.StatusNotFound},
		{"cancelled mandate", srv.URL + "/exports/pain011?mandate=m-cancelled&creditor_scheme_id=X", http.StatusUnprocessableEntity},
		{"expired mandate", srv.URL + "/exports/pain011?mandate=m-expired&creditor_scheme_id=X", http.StatusUnprocessableEntity},
		{"suspended mandate cancels", srv.URL + "/exports/pain011?mandate=m-suspended&creditor_scheme_id=X", http.StatusOK},
		{"no IBAN still cancels", srv.URL + "/exports/pain011?mandate=m-noiban&creditor_scheme_id=X", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getXML(t, tc.url)
			if code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", code, tc.want, body)
			}
		})
	}

	// A reasonless export omits CxlRsn over HTTP as well.
	code, doc := getXML(t, srv.URL+"/exports/pain011?mandate=m-active&creditor_scheme_id=X")
	if code != http.StatusOK {
		t.Fatalf("reasonless export: status=%d, want 200", code)
	}
	if strings.Contains(doc, "<CxlRsn>") {
		t.Errorf("reasonless export carries a CxlRsn block:\n%s", doc)
	}
}

func TestHTTPPain011ExportMetrics(t *testing.T) {
	srv := pain011TestServer(t)

	// The counter counts served exports, including rejected ones
	// (1 ok + 2 rejected = 3).
	if code, _ := getXML(t, srv.URL+"/exports/pain011?mandate=m-active&creditor_scheme_id=X"); code != http.StatusOK {
		t.Fatalf("ok export: status=%d, want 200", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain011"); code != http.StatusBadRequest {
		t.Fatalf("missing params: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain011?mandate=m-ghost&creditor_scheme_id=X"); code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d, want 404", code)
	}
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain011_exports_total 3") {
		t.Errorf("metrics missing ledger_pain011_exports_total 3")
	}
}
