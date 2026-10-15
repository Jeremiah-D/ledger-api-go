package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// LG-93 HTTP surface: fx_side and fx_income_account on POST /transfers
// (and the dry-run twin), the spread disclosure on the receipt, and the
// spread counters on /metrics.

// newSpreadTestServer builds a server whose ledger deals USD->EUR at mid
// 110/100, bid 108/100, ask 112/100, with an FX clearing account and an
// FX income account.
func newSpreadTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithFXAccount("fx-pnl"), ledger.WithFXIncomeAccount("fx-inc"))
	if err := l.SetFXRate("USD", "EUR", 110, 100); err != nil {
		t.Fatal(err)
	}
	if err := l.SetFXBidAskRate("USD", "EUR", 108, 100, 112, 100); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":1000000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	return srv
}

func TestHTTPFXSpreadSellAtBid(t *testing.T) {
	srv := newSpreadTestServer(t)
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxs-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","fx_side":"sell"}`)
	if code != http.StatusCreated {
		t.Fatalf("FX transfer: status=%d, want 201 (body=%v)", code, body)
	}
	fx, ok := body["fx"].(map[string]any)
	if !ok {
		t.Fatalf("receipt has no fx disclosure: %v", body)
	}
	if fx["direction"] != "sell" || fx["converted_cents"] != float64(10800) {
		t.Errorf("fx = %v, want direction=sell converted=10800", fx)
	}
	if fx["bid_num"] != float64(108) || fx["ask_num"] != float64(112) || fx["mid_num"] != float64(110) {
		t.Errorf("fx quote disclosure = %v", fx)
	}
	if fx["spread_cents"] != float64(200) || fx["spread_bps"] != float64(363) {
		t.Errorf("fx spread = %v, want 200 cents / 363 bps", fx)
	}
	if fx["fx_income_account"] != "fx-inc" {
		t.Errorf("fx_income_account = %v, want fx-inc", fx["fx_income_account"])
	}
	entries := body["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (principal + fx + spread)", len(entries))
	}
	spread := entries[2].(map[string]any)
	if spread["id"] != "fxs-http-1/spread" || spread["debit_account"] != "fx-pnl" ||
		spread["credit_account"] != "fx-inc" || spread["amount_cents"] != float64(200) ||
		spread["currency"] != "EUR" {
		t.Errorf("spread leg = %v", spread)
	}
}

func TestHTTPFXSpreadBuyAtAsk(t *testing.T) {
	srv := newSpreadTestServer(t)
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxb-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","fx_side":"buy"}`)
	if code != http.StatusCreated {
		t.Fatalf("FX transfer: status=%d, want 201 (body=%v)", code, body)
	}
	fx := body["fx"].(map[string]any)
	if fx["direction"] != "buy" || fx["converted_cents"] != float64(11200) {
		t.Errorf("fx = %v, want direction=buy converted=11200", fx)
	}
	if fx["spread_cents"] != float64(200) {
		t.Errorf("spread_cents = %v, want 200", fx["spread_cents"])
	}
}

func TestHTTPFXSpreadInvalidSide(t *testing.T) {
	srv := newSpreadTestServer(t)
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxv-http-1","from_account":"payer","to_account":"payee","amount_cents":100,"currency":"USD","to_currency":"EUR","fx_side":"sideways"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "fx_side") {
		t.Errorf("error = %v, want an fx_side complaint", body["error"])
	}
}

func TestHTTPFXSpreadFailClosed(t *testing.T) {
	// No FX income account anywhere: the spread has nowhere to go, so
	// the transfer is rejected with 400 and records nothing.
	l := ledger.New(ledger.WithFXAccount("fx-pnl"))
	if err := l.SetFXRate("USD", "EUR", 110, 100); err != nil {
		t.Fatal(err)
	}
	if err := l.SetFXBidAskRate("USD", "EUR", 108, 100, 112, 100); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":1000000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxf-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "fx_income_account") {
		t.Errorf("error = %v, want the income-account complaint", body["error"])
	}

	// The transfer-level override fixes it.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxf-http-2","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","fx_income_account":"desk-inc"}`)
	if code != http.StatusCreated {
		t.Fatalf("with fx_income_account: status=%d, want 201 (body=%v)", code, body)
	}
	if body["fx"].(map[string]any)["fx_income_account"] != "desk-inc" {
		t.Errorf("fx disclosure = %v", body["fx"])
	}
}

func TestHTTPFXSpreadDryRun(t *testing.T) {
	srv := newSpreadTestServer(t)
	// The dry-run twin decodes fx_side like the real transfer and would
	// book the spread leg — without recording anything.
	code, body := postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"fxd-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","fx_side":"buy"}`)
	if code != http.StatusOK {
		t.Fatalf("dry run: status=%d, want 200 (body=%v)", code, body)
	}
	if body["would_succeed"] != true {
		t.Errorf("dry run body = %v, want would_succeed=true", body)
	}
	// Garbage fx_side is a 400 on the dry run too.
	code, _ = postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"fxd-http-2","from_account":"payer","to_account":"payee","amount_cents":100,"currency":"USD","to_currency":"EUR","fx_side":"sideways"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("dry run bad side: status=%d, want 400", code)
	}
}

func TestHTTPFXSpreadMetrics(t *testing.T) {
	srv := newSpreadTestServer(t)
	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fxm-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR"}`)
	if code != http.StatusCreated {
		t.Fatalf("FX transfer: status=%d, want 201", code)
	}
	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics: status=%d", code)
	}
	for _, want := range []string{
		"ledger_fx_spread_bookings_total 1",
		"ledger_fx_spread_cents_total 200",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
