package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPPain010Export(t *testing.T) {
	srv := pain008TestServer(t)

	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	code, doc := getXML(t, srv.URL+"/exports/pain010?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999&effective_date="+tomorrow+"&new_frequency=per-week&new_max_amount_cents=8000")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.010.001.02"`,
		"<MndtAmdmntReq>",
		"<MndtId>m-http</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		`<MaxAmt Ccy="USD">80.00</MaxAmt>`,
		"<Frqcy>per-week</Frqcy>",
		"<FrDt>" + tomorrow + "</FrDt>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
}

func TestHTTPPain010ExportErrors(t *testing.T) {
	srv := pain008TestServer(t)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing params", srv.URL + "/exports/pain010", http.StatusBadRequest},
		{"bad date", srv.URL + "/exports/pain010?mandate=m-http&creditor_scheme_id=X&effective_date=not-a-date&new_frequency=per-week", http.StatusBadRequest},
		{"bad amount", srv.URL + "/exports/pain010?mandate=m-http&creditor_scheme_id=X&effective_date=" + tomorrow + "&new_max_amount_cents=abc", http.StatusBadRequest},
		{"empty amendment", srv.URL + "/exports/pain010?mandate=m-http&creditor_scheme_id=X&effective_date=" + tomorrow, http.StatusBadRequest},
		{"unknown mandate", srv.URL + "/exports/pain010?mandate=m-ghost&creditor_scheme_id=X&effective_date=" + tomorrow + "&new_frequency=per-week", http.StatusNotFound},
		{"bad frequency", srv.URL + "/exports/pain010?mandate=m-http&creditor_scheme_id=X&effective_date=" + tomorrow + "&new_frequency=per-fortnight", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := getXML(t, tc.url)
			if code != tc.want {
				t.Errorf("status = %d, want %d", code, tc.want)
			}
		})
	}
}
