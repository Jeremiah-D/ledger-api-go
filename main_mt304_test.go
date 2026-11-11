package main

import (
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt304HTTPSetup books one forward with registered BICs on both
// parties, ready for GET /exports/mt304.
func mt304HTTPSetup(t *testing.T) (*ledger.Ledger, *ledger.FXForward) {
	t.Helper()
	l := ledger.New(ledger.WithFXAccount("fx"))
	if err := l.SetAccountIBAN("payer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN payer: %v", err)
	}
	if err := l.SetAccountIBAN("payee", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN payee: %v", err)
	}
	c, err := l.BookForward(ledger.ForwardBooking{
		ID:            "fwd-http-3001",
		From:          "payer",
		To:            "payee",
		FromCurrency:  "USD",
		ToCurrency:    "EUR",
		Rate:          big.NewRat(11, 10),
		NotionalCents: 10000,
		SettleAt:      time.Now().UTC().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BookForward: %v", err)
	}
	return l, c
}

// Full MT304 instruction flow through HTTP: book the forward,
// export the instruction to the custodian, and verify the FIN body,
// the byte-identical repeat, and the metrics counter.
func TestHTTPMT304Export(t *testing.T) {
	l, c := mt304HTTPSetup(t)
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
	q.Set("forward_id", c.ID)
	q.Set("custodian_bic", "CITIUS33")

	code, hdr, body := getText(t, srv.URL+"/exports/mt304?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I304CITIUS33XXXU3003}",
		"{3:{108:fwd-http-3001}}",
		":15A:",
		":20:fwd-http-3001",
		":22A:NEWT",
		":94A:ADVI",
		":83A:DEUTDEFF",
		":82A:DEUTDEFF",
		":87A:NWBKGB2L",
		":15B:",
		":36:1,1",
		":32B:EUR110,00",
		":53A:NWBKGB2L",
		":33B:USD100,00",
		":57A:NWBKGB2L",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{":22C:", ":21:", ":15C:", ":15D:", ":15E:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("export must not carry %s, body:\n%s", forbidden, body)
		}
	}

	// Repeat export is byte-identical: every field is pinned to
	// the stored contract and the caller-given options.
	_, _, body2 := getText(t, srv.URL+"/exports/mt304?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt304_exports_total 2") {
		t.Errorf("metrics missing ledger_mt304_exports_total 2")
	}
}

// Error mapping for GET /exports/mt304: a missing forward_id or
// custodian_bic is a 400, an illegal scope is a 400, an unknown
// forward a 404, a cancelled forward a 422 — and rejected requests
// still count in the metrics.
func TestHTTPMT304ExportErrors(t *testing.T) {
	l, c := mt304HTTPSetup(t)
	if _, err := l.CancelForward(c.ID); err != nil {
		t.Fatalf("CancelForward: %v", err)
	}
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

	// Missing forward_id: 400.
	if code := getCode(t, srv.URL+"/exports/mt304?custodian_bic=CITIUS33"); code != http.StatusBadRequest {
		t.Errorf("missing forward_id: status=%d, want 400", code)
	}
	// Missing custodian_bic: 400.
	missingCustodian := url.Values{}
	missingCustodian.Set("forward_id", c.ID)
	if code := getCode(t, srv.URL+"/exports/mt304?"+missingCustodian.Encode()); code != http.StatusBadRequest {
		t.Errorf("missing custodian_bic: status=%d, want 400", code)
	}
	// Illegal scope: 400.
	badScope := url.Values{}
	badScope.Set("forward_id", c.ID)
	badScope.Set("custodian_bic", "CITIUS33")
	badScope.Set("scope", "BOGUS")
	if code := getCode(t, srv.URL+"/exports/mt304?"+badScope.Encode()); code != http.StatusBadRequest {
		t.Errorf("illegal scope: status=%d, want 400", code)
	}
	// Unknown forward: 404.
	unknown := url.Values{}
	unknown.Set("forward_id", "fwd-http-9999")
	unknown.Set("custodian_bic", "CITIUS33")
	if code := getCode(t, srv.URL+"/exports/mt304?"+unknown.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown forward: status=%d, want 404", code)
	}
	// Cancelled forward: 422.
	cancelled := url.Values{}
	cancelled.Set("forward_id", c.ID)
	cancelled.Set("custodian_bic", "CITIUS33")
	if code := getCode(t, srv.URL+"/exports/mt304?"+cancelled.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("cancelled forward: status=%d, want 422", code)
	}

	// Rejected requests still count in the metrics (5 above).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt304_exports_total 5") {
		t.Errorf("metrics missing ledger_mt304_exports_total 5")
	}
}
