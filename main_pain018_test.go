package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestHTTPPain018Export(t *testing.T) {
	srv := pain011TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain018?mandate=m-active&creditor_scheme_id=DE98ZZZ09999999999&reason=SUSP")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.018.001.03"`,
		"<MndtSspnsnReq>",
		"<MsgId>pain018-m-active-DE98ZZZ09999999999</MsgId>",
		"<SspnsnReqId>pain018-m-active-DE98ZZZ09999999999</SspnsnReqId>",
		"<MndtId>m-active</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<SspnsnRsn><Rsn><Prtry>SUSP</Prtry></Rsn></SspnsnRsn>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<IBAN>") {
		t.Error("pain.018 export carries an IBAN element")
	}

	// The counter counts this one served export.
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain018_exports_total 1") {
		t.Errorf("metrics missing ledger_pain018_exports_total 1")
	}
}

func TestHTTPPain018ExportErrors(t *testing.T) {
	srv := pain011TestServer(t)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing params", srv.URL + "/exports/pain018", http.StatusBadRequest},
		{"missing scheme", srv.URL + "/exports/pain018?mandate=m-active&reason=SUSP", http.StatusBadRequest},
		{"missing reason", srv.URL + "/exports/pain018?mandate=m-active&creditor_scheme_id=X", http.StatusBadRequest},
		{"unknown mandate", srv.URL + "/exports/pain018?mandate=m-ghost&creditor_scheme_id=X&reason=SUSP", http.StatusNotFound},
		{"cancelled mandate", srv.URL + "/exports/pain018?mandate=m-cancelled&creditor_scheme_id=X&reason=SUSP", http.StatusUnprocessableEntity},
		{"expired mandate", srv.URL + "/exports/pain018?mandate=m-expired&creditor_scheme_id=X&reason=SUSP", http.StatusUnprocessableEntity},
		{"suspended mandate refused", srv.URL + "/exports/pain018?mandate=m-suspended&creditor_scheme_id=X&reason=SUSP", http.StatusUnprocessableEntity},
		{"no IBAN still suspends", srv.URL + "/exports/pain018?mandate=m-noiban&creditor_scheme_id=X&reason=SUSP", http.StatusOK},
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

func TestHTTPPain018ExportMetrics(t *testing.T) {
	srv := pain011TestServer(t)

	// The counter counts served exports, including rejected ones
	// (1 ok + 2 rejected = 3).
	if code, _ := getXML(t, srv.URL+"/exports/pain018?mandate=m-active&creditor_scheme_id=X&reason=SUSP"); code != http.StatusOK {
		t.Fatalf("ok export: status=%d, want 200", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain018"); code != http.StatusBadRequest {
		t.Fatalf("missing params: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain018?mandate=m-ghost&creditor_scheme_id=X&reason=SUSP"); code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d, want 404", code)
	}
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain018_exports_total 3") {
		t.Errorf("metrics missing ledger_pain018_exports_total 3")
	}
}
