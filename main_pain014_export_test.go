package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain014ExportTestServer builds a server whose RfPs cover every
// pain.014 export state gate: issued, paid, rejected, expired.
// No IBANs are needed: the response names no parties or agents,
// only the original request.
func pain014ExportTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	mk := func(id string) {
		t.Helper()
		if _, _, err := l.CreateRfP(ctx, ledger.RfP{
			ID: id, Creditor: ledger.AccountID("merchant"), Debtor: ledger.AccountID("cust"),
			AmountCents: 5000, Currency: "USD",
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateRfP %s: %v", id, err)
		}
	}
	mk("rfp-expired")
	// Sweep before creating the rest: the sweep expires every
	// issued RfP past the horizon, so the expired case must be
	// swept while it is the only RfP in the ledger.
	if n := l.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireRfPs = %d, want 1", n)
	}
	mk("rfp-issued")
	mk("rfp-paid")
	mk("rfp-rejected")
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

func TestHTTPPain014Export(t *testing.T) {
	srv := pain014ExportTestServer(t)

	// The paid RfP exports its ACCP verdict at the full amount.
	code, doc := getXML(t, srv.URL+"/exports/pain014?rfp_id=rfp-paid")
	if code != http.StatusOK {
		t.Fatalf("paid export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.014.001.07"`,
		"<CdtrPmtActvtnReqStsRpt>",
		"<MsgId>pain014-rfp-paid</MsgId>",
		"<OrgnlMsgId>pain013-rfp-paid</OrgnlMsgId>",
		"<OrgnlMsgNmId>pain.013.001.07</OrgnlMsgNmId>",
		"<OrgnlEndToEndId>rfp-paid</OrgnlEndToEndId>",
		"<TxSts>ACCP</TxSts>",
		`<AccptdAmt Ccy="USD">50.00</AccptdAmt>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<StsRsnInf>") {
		t.Error("ACCP export carries a StsRsnInf block")
	}

	// The rejected RfP exports its RJCT verdict with the reason
	// carried verbatim from the query parameter.
	code, doc = getXML(t, srv.URL+"/exports/pain014?rfp_id=rfp-rejected&reason_code=AC03")
	if code != http.StatusOK {
		t.Fatalf("rejected export: status=%d body=%s, want 200", code, doc)
	}
	for _, want := range []string{
		"<MsgId>pain014-rfp-rejected</MsgId>",
		"<TxSts>RJCT</TxSts>",
		// MarshalIndent separates the elements with newlines,
		// so the Cd value — not the compact nesting — is
		// asserted here; the ledger round-trip test nails the
		// nesting and the verbatim reason.
		"<Cd>AC03</Cd>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("body missing %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<AccptdAmt") {
		t.Error("RJCT export carries an accepted amount")
	}

	// The counter counts the two served exports.
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain014_exports_total 2") {
		t.Errorf("metrics missing ledger_pain014_exports_total 2")
	}
}

func TestHTTPPain014ExportErrors(t *testing.T) {
	srv := pain014ExportTestServer(t)

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing rfp_id", srv.URL + "/exports/pain014", http.StatusBadRequest},
		{"unknown rfp", srv.URL + "/exports/pain014?rfp_id=rfp-ghost", http.StatusNotFound},
		{"issued rfp", srv.URL + "/exports/pain014?rfp_id=rfp-issued", http.StatusUnprocessableEntity},
		{"expired rfp", srv.URL + "/exports/pain014?rfp_id=rfp-expired", http.StatusUnprocessableEntity},
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

func TestHTTPPain014ExportMetrics(t *testing.T) {
	srv := pain014ExportTestServer(t)

	// The counter counts served exports, including rejected ones
	// (1 ok + 2 rejected = 3).
	if code, _ := getXML(t, srv.URL+"/exports/pain014?rfp_id=rfp-paid"); code != http.StatusOK {
		t.Fatalf("ok export: status=%d, want 200", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain014"); code != http.StatusBadRequest {
		t.Fatalf("missing rfp_id: status=%d, want 400", code)
	}
	if code, _ := getXML(t, srv.URL+"/exports/pain014?rfp_id=rfp-ghost"); code != http.StatusNotFound {
		t.Fatalf("unknown rfp: status=%d, want 404", code)
	}
	code, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain014_exports_total 3") {
		t.Errorf("metrics missing ledger_pain014_exports_total 3")
	}
}

// TestHTTPPain014ApplyUnaffected proves the new GET export route
// did not disturb the LG-416 inbound apply: the same ACCP document
// still applies through POST /exports/pain014/apply and pays the
// RfP. The helpers come from main_pain014_apply_test.go.
func TestHTTPPain014ApplyUnaffected(t *testing.T) {
	srv, l := seedPain014ApplyServer(t)

	code, body := postPain014XML(t, srv.URL+"/exports/pain014/apply",
		pain014HTTPDoc("pain014-apply-check", "rfp-http", "ACCP", "50.00", "USD"), "debtorbank")
	if code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	got, ok := l.GetRfP("rfp-http")
	if !ok {
		t.Fatal("GetRfP: RfP vanished after apply")
	}
	if got.Status != ledger.RfPStatusPaid {
		t.Fatalf("RfP status = %q after ACCP apply, want paid", got.Status)
	}
	if fmt.Sprint(body["status"]) != "paid" {
		t.Errorf("apply response status = %v, want paid", body["status"])
	}
}
