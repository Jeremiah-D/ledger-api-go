package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt101HTTPSetup posts two transfers from alice (registered IBAN
// and BIC) to bob (registered IBAN, no BIC) and carol (registered
// IBAN and BIC), ready for GET /exports/mt101.
func mt101HTTPSetup(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	if err := l.SetAccountIBAN("carol", "FR1420041010050500013M02606", "DEUTDEFF500"); err != nil {
		t.Fatalf("SetAccountIBAN carol: %v", err)
	}
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for _, tr := range []ledger.Transfer{
		{ID: "pay-1", From: "alice", To: "bob", AmountCents: 15000, Currency: "USD", CreatedAt: base},
		{ID: "pay-2", From: "alice", To: "carol", AmountCents: 25050, Currency: "USD", CreatedAt: base.Add(time.Hour)},
	} {
		if _, err := l.PostTransfer(tr); err != nil {
			t.Fatalf("PostTransfer(%s): %v", tr.ID, err)
		}
	}
	return l
}

// Full MT101 request flow through HTTP: post the transfers, export
// the instruction, and verify the FIN body, the byte-identical
// repeat, and the metrics counter.
func TestHTTPMT101Export(t *testing.T) {
	l := mt101HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getText := func(t *testing.T, raw string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}
	q := url.Values{}
	q.Set("debtor", "alice")
	q.Set("execute_on", "2026-11-12")

	code, hdr, body := getText(t, srv.URL+"/exports/mt101?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I101DEUTDEFFXXXU3003}",
		":15A:",
		":20:MT101-",
		":28D:1/1",
		":50H:/DE89370400440532013000",
		":52A:DEUTDEFF",
		":30:261112",
		":15B:",
		":21:pay-1",
		":32B:USD150,00",
		":57D:bob",
		":59:/GB29NWBK60161331926819",
		":71A:SHA",
		":21:pay-2",
		":32B:USD250,50",
		":57A:DEUTDEFF500",
		":59:/FR1420041010050500013M02606",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{":70:", ":33B:", ":36:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("export must not carry %s, body:\n%s", forbidden, body)
		}
	}

	// Repeat export is byte-identical: the reference derives from
	// the content and the pinned execution day, not the clock.
	_, _, body2 := getText(t, srv.URL+"/exports/mt101?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt101_exports_total 2") {
		t.Errorf("metrics missing ledger_mt101_exports_total 2")
	}
}

// Error mapping for GET /exports/mt101: a missing debtor is a 400,
// a bad charge bearer is a 400, a bad execute_on is a 400, and a
// debtor with no matching payments is a 400 — and rejected
// requests still count in the metrics.
func TestHTTPMT101ExportErrors(t *testing.T) {
	l := mt101HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getCode := func(t *testing.T, raw string) int {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		return resp.StatusCode
	}

	// Missing debtor: 400.
	if code := getCode(t, srv.URL+"/exports/mt101"); code != http.StatusBadRequest {
		t.Errorf("missing debtor: status=%d, want 400", code)
	}
	// Bad charge bearer: 400.
	badBearer := url.Values{}
	badBearer.Set("debtor", "alice")
	badBearer.Set("charge_bearer", "BOGUS")
	if code := getCode(t, srv.URL+"/exports/mt101?"+badBearer.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad charge_bearer: status=%d, want 400", code)
	}
	// Bad execute_on: 400.
	badDate := url.Values{}
	badDate.Set("debtor", "alice")
	badDate.Set("execute_on", "not-a-date")
	if code := getCode(t, srv.URL+"/exports/mt101?"+badDate.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad execute_on: status=%d, want 400", code)
	}
	// Debtor with no matching payments: 400.
	noPayments := url.Values{}
	noPayments.Set("debtor", "alice")
	noPayments.Set("currency", "EUR")
	if code := getCode(t, srv.URL+"/exports/mt101?"+noPayments.Encode()); code != http.StatusBadRequest {
		t.Errorf("zero payments: status=%d, want 400", code)
	}

	// Rejected requests still count in the metrics (4 above).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt101_exports_total 4") {
		t.Errorf("metrics missing ledger_mt101_exports_total 4")
	}
}
