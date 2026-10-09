package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPFXTransferExpiredRate(t *testing.T) {
	l := ledger.New(ledger.WithFXAccount("fx-pnl"))
	rate, err := ledger.ParseFXRateDecimal("1.10")
	if err != nil {
		t.Fatal(err)
	}
	// A rate that lives for 60ms: the transfer below lands while live.
	if err := l.SetFXRateRat("USD", "EUR", rate, 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":100000,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("funding: status=%d", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-live","from_account":"payer","to_account":"payee","amount_cents":1000,"currency":"USD","to_currency":"EUR"}`)
	if code != http.StatusCreated {
		t.Fatalf("live-rate transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if fx := body["fx"].(map[string]any); fx["source_cents"] != float64(1000) {
		t.Errorf("fx.source_cents = %v, want 1000", fx["source_cents"])
	}

	time.Sleep(150 * time.Millisecond)
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"fx-stale","from_account":"payer","to_account":"payee","amount_cents":1000,"currency":"USD","to_currency":"EUR"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expired rate: status=%d, want 422 (body=%v)", code, body)
	}

	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d", code)
	}
	if m := parseExposition(t, text); m["ledger_currency_rejections_total"] != 1 {
		t.Errorf("ledger_currency_rejections_total = %d, want 1", m["ledger_currency_rejections_total"])
	}
}
