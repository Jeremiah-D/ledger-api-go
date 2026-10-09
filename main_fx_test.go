package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func newFXTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithFXAccount("fx-pnl"))
	if err := l.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPFXTransfer(t *testing.T) {
	srv := newFXTestServer(t)
	defer srv.Close()

	// Fund the payer in USD.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-http-1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","idempotency_key":"fxk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("FX transfer: status=%d, want 201 (body=%v)", code, body)
	}
	fx, ok := body["fx"].(map[string]any)
	if !ok {
		t.Fatalf("receipt has no fx disclosure: %v", body)
	}
	if fx["to_currency"] != "EUR" || fx["converted_cents"] != float64(10800) {
		t.Errorf("fx disclosure = %v, want EUR/10800", fx)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	e := entries[0].(map[string]any)
	if e["debit_account"] != "payee" || e["currency"] != "EUR" || e["amount_cents"] != float64(10800) {
		t.Errorf("principal = %v", e)
	}

	// Replay: 200, duplicate, no fx disclosure.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-http-1b","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","to_currency":"EUR","idempotency_key":"fxk-1"}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Errorf("replay: status=%d duplicate=%v, want 200/true", code, body["duplicate"])
	}

	// FX counter observed the attempts.
	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d", code)
	}
	if got := parseExposition(t, text)["ledger_fx_transfers_total"]; got != 2 {
		t.Errorf("ledger_fx_transfers_total = %d, want 2", got)
	}
}

func TestHTTPFXTransferMissingRate(t *testing.T) {
	srv := newFXTestServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-norate","from_account":"p","to_account":"q","amount_cents":100,"currency":"USD","to_currency":"JPY"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("missing rate: status=%d, want 422 (body=%v)", code, body)
	}
}

func TestHTTPFXTransferNoAccountConfigured(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New())) // no FX account, no rates
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-noacct","from_account":"p","to_account":"q","amount_cents":100,"currency":"USD","to_currency":"EUR"}`)
	// No rate configured either: the rate check fires first (422).
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", code)
	}
}
