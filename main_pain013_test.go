package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain013TestServer builds a server whose RfPs cover every
// pain.013 state gate: issued, paid, rejected, expired, one with
// no registered IBANs, and one whose debtor IBAN carries no BIC.
func pain013TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	if err := l.SetAccountIBAN("cust2", "DE89370400440532013000", ""); err != nil {
		t.Fatalf("SetAccountIBAN cust2: %v", err)
	}
	mk := func(id, creditor, debtor string) {
		t.Helper()
		if _, _, err := l.CreateRfP(ctx, ledger.RfP{
			ID: id, Creditor: ledger.AccountID(creditor), Debtor: ledger.AccountID(debtor),
			AmountCents: 5000, Currency: "USD",
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateRfP %s: %v", id, err)
		}
	}
	mk("rfp-expired", "merchant", "cust")
	// Sweep before creating the rest: the sweep expires every
	// issued RfP past the horizon, so the expired case must be
	// swept while it is the only RfP in the ledger.
	if n := l.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireRfPs = %d, want 1", n)
	}
	mk("rfp-issued", "merchant", "cust")
	mk("rfp-paid", "merchant", "cust")
	mk("rfp-rejected", "merchant", "cust")
	mk("rfp-noiban", "merchant", "cust3")
	mk("rfp-nobic", "merchant", "cust2")
	if _, err := l.AcceptRfP(ctx, "rfp-paid", "cust"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	if _, err := l.RejectRfP(ctx, "rfp-rejected", "cust"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPPain013Export(t *testing.T) {
	srv := pain013TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain013?rfp_id=rfp-issued")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.013.001.07"`,
		"<CdtrPmtActvtnReq>",
		"<MsgId>pain013-rfp-issued</MsgId>",
		"<PmtInfId>pain013-rfp-issued</PmtInfId>",
		"<PmtMtd>TRF</PmtMtd>",
		"<EndToEndId>rfp-issued</EndToEndId>",
		`<InstdAmt Ccy="USD">50.00</InstdAmt>`,
		"<XpryDt><DtTm>",
		"<Dbtr><Nm>cust</Nm></Dbtr>",
		"<Cdtr><Nm>merchant</Nm></Cdtr>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}

	// The counter counts this one served export.
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain013_exports_total 1") {
		t.Errorf("metrics missing ledger_pain013_exports_total 1")
	}
}

func TestHTTPPain013ExportErrors(t *testing.T) {
	srv := pain013TestServer(t)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing rfp_id", srv.URL + "/exports/pain013", http.StatusBadRequest},
		{"unknown rfp", srv.URL + "/exports/pain013?rfp_id=rfp-ghost", http.StatusNotFound},
		{"paid rfp", srv.URL + "/exports/pain013?rfp_id=rfp-paid", http.StatusUnprocessableEntity},
		{"rejected rfp", srv.URL + "/exports/pain013?rfp_id=rfp-rejected", http.StatusUnprocessableEntity},
		{"expired rfp", srv.URL + "/exports/pain013?rfp_id=rfp-expired", http.StatusUnprocessableEntity},
		{"no IBAN", srv.URL + "/exports/pain013?rfp_id=rfp-noiban", http.StatusUnprocessableEntity},
		{"no agent BIC", srv.URL + "/exports/pain013?rfp_id=rfp-nobic", http.StatusUnprocessableEntity},
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

func TestHTTPPain013ExportMetrics(t *testing.T) {
	srv := pain013TestServer(t)

	// The counter counts served exports, including rejected ones
	// (1 ok + 2 rejected = 3).
	if code, _ := getXML(t, srv.URL+"/exports/pain013?rfp_id=rfp-issued"); code != http.StatusOK {
		t.Fatalf("ok export: status=%d, want 200", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain013"); code != http.StatusBadRequest {
		t.Fatalf("missing rfp_id: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain013?rfp_id=rfp-ghost"); code != http.StatusNotFound {
		t.Fatalf("unknown rfp: status=%d, want 404", code)
	}
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain013_exports_total 3") {
		t.Errorf("metrics missing ledger_pain013_exports_total 3")
	}
}
