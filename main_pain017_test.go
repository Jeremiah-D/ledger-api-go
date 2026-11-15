package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestHTTPPain017Export(t *testing.T) {
	srv := pain011TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain017?mandate=m-active&creditor_scheme_id=DE98ZZZ09999999999")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.017.001.03"`,
		"<MndtCpyReq>",
		"<MsgId>pain017-m-active-DE98ZZZ09999999999</MsgId>",
		"<UndrlygCpyReqDtls>",
		"<OrgnlMndt>",
		"<MndtId>m-active</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<MndtSts><Cd>ACTV</Cd></MndtSts>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<IBAN>") {
		t.Error("pain.017 export carries an IBAN element")
	}

	// The counter counts this one served export.
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain017_exports_total 1") {
		t.Errorf("metrics missing ledger_pain017_exports_total 1")
	}
}

func TestHTTPPain017ExportErrors(t *testing.T) {
	srv := pain011TestServer(t)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing params", srv.URL + "/exports/pain017", http.StatusBadRequest},
		{"missing scheme", srv.URL + "/exports/pain017?mandate=m-active", http.StatusBadRequest},
		{"unknown mandate", srv.URL + "/exports/pain017?mandate=m-ghost&creditor_scheme_id=X", http.StatusNotFound},
		// No state gate: all four mandate states are exportable.
		{"active mandate", srv.URL + "/exports/pain017?mandate=m-active&creditor_scheme_id=X", http.StatusOK},
		{"suspended mandate", srv.URL + "/exports/pain017?mandate=m-suspended&creditor_scheme_id=X", http.StatusOK},
		{"cancelled mandate", srv.URL + "/exports/pain017?mandate=m-cancelled&creditor_scheme_id=X", http.StatusOK},
		{"expired mandate", srv.URL + "/exports/pain017?mandate=m-expired&creditor_scheme_id=X", http.StatusOK},
		{"no IBAN still exports", srv.URL + "/exports/pain017?mandate=m-noiban&creditor_scheme_id=X", http.StatusOK},
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

func TestHTTPPain017ExportFourStatesCarryStatusCodes(t *testing.T) {
	srv := pain011TestServer(t)

	// The MndtSts code follows the mandate's effective status:
	// ACTV / SUSP / CANC / EXPI, the ExternalMandateStatus1Code
	// values verified against the ISO 20022 code set.
	cases := []struct {
		mandate string
		code    string
	}{
		{"m-active", "ACTV"},
		{"m-suspended", "SUSP"},
		{"m-cancelled", "CANC"},
		{"m-expired", "EXPI"},
	}
	for _, tc := range cases {
		t.Run(tc.mandate, func(t *testing.T) {
			status, doc := getXML(t, srv.URL+"/exports/pain017?mandate="+tc.mandate+"&creditor_scheme_id=X")
			if status != http.StatusOK {
				t.Fatalf("export %s: status=%d body=%s, want 200", tc.mandate, status, doc)
			}
			if want := "<MndtSts><Cd>" + tc.code + "</Cd></MndtSts>"; !strings.Contains(doc, want) {
				t.Errorf("body missing %q:\n%s", want, doc)
			}
		})
	}
}

func TestHTTPPain017ExportMetrics(t *testing.T) {
	srv := pain011TestServer(t)

	// The counter counts served exports, including rejected ones
	// (1 ok + 2 rejected = 3).
	if code, _ := getXML(t, srv.URL+"/exports/pain017?mandate=m-active&creditor_scheme_id=X"); code != http.StatusOK {
		t.Fatalf("ok export: status=%d, want 200", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain017"); code != http.StatusBadRequest {
		t.Fatalf("missing params: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain017?mandate=m-ghost&creditor_scheme_id=X"); code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d, want 404", code)
	}
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain017_exports_total 3") {
		t.Errorf("metrics missing ledger_pain017_exports_total 3")
	}
}
