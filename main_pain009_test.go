package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain009MandateServer builds a server with a single mandate
// (m-009) in the state the caller arranges: IBANs on/off, validity
// window, and optional suspension — the 422 fixtures for the
// pain.009 export.
func pain009MandateServer(t *testing.T, withIBAN bool, from, until time.Time, suspend bool) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if withIBAN {
		if err := l.SetAccountIBAN("cust", "DE89370400440532013000", ""); err != nil {
			t.Fatalf("SetAccountIBAN cust: %v", err)
		}
		if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", ""); err != nil {
			t.Fatalf("SetAccountIBAN merchant: %v", err)
		}
	}
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m-009", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency: ledger.MandateFrequencyPerMonth,
		ValidFrom: from, ValidUntil: until,
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if suspend {
		if _, err := l.SuspendMandate("m-009"); err != nil {
			t.Fatalf("SuspendMandate: %v", err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// GET /exports/pain009 renders the pain.009.001.02 initiation
// request for the registered mandate.
func TestHTTPPain009Export(t *testing.T) {
	srv := pain008TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain009?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.001.02"`,
		"<MndtInitnReq>",
		"<MndtId>m-http</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<IBAN>DE89370400440532013000</IBAN>",
		"<IBAN>GB29NWBK60161331926819</IBAN>",
		"<MsgId>pain009-m-http-DE98ZZZ09999999999</MsgId>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	// A repeat export carries the same deterministic MsgId.
	_, second := getXML(t, srv.URL+"/exports/pain009?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999")
	if !strings.Contains(second, "<MsgId>pain009-m-http-DE98ZZZ09999999999</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestHTTPPain009ExportErrors(t *testing.T) {
	srv := pain008TestServer(t)
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// A suspended mandate authorizes nothing — including its own
	// initiation request.
	suspended := pain009MandateServer(t, true, day, day.AddDate(1, 0, 0), true)
	// A mandate whose validity already ended is expired on every
	// read.
	expired := pain009MandateServer(t, true, day.AddDate(0, -2, 0), day.AddDate(0, -2, 0).Add(24*time.Hour), false)
	// No registered IBANs: the initiation request cannot name the
	// accounts, so it is refused rather than rendered with forged
	// identifiers.
	noIBAN := pain009MandateServer(t, false, day, day.AddDate(1, 0, 0), false)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing mandate", srv.URL + "/exports/pain009?creditor_scheme_id=DE98ZZZ09999999999", http.StatusBadRequest},
		{"missing scheme", srv.URL + "/exports/pain009?mandate=m-http", http.StatusBadRequest},
		{"unknown mandate", srv.URL + "/exports/pain009?mandate=m-ghost&creditor_scheme_id=DE98ZZZ09999999999", http.StatusNotFound},
		{"suspended mandate", suspended.URL + "/exports/pain009?mandate=m-009&creditor_scheme_id=DE98ZZZ09999999999", http.StatusUnprocessableEntity},
		{"expired mandate", expired.URL + "/exports/pain009?mandate=m-009&creditor_scheme_id=DE98ZZZ09999999999", http.StatusUnprocessableEntity},
		{"missing IBAN", noIBAN.URL + "/exports/pain009?mandate=m-009&creditor_scheme_id=DE98ZZZ09999999999", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getXML(t, tc.url)
			if code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", code, tc.want, body)
			}
		})
	}
}
