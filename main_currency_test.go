package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestHTTPPostEntryWithCurrency checks that POST /entries accepts an
// explicit currency, journals it, and returns it on the posted entry.
func TestHTTPPostEntryWithCurrency(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":500,"currency":"EUR"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%v)", code, body)
	}
	if body["currency"] != "EUR" {
		t.Errorf("currency = %v, want EUR", body["currency"])
	}

	// The default-currency balance is untouched by the EUR posting.
	_, bbody := getJSON(t, srv.URL+"/accounts/cash/balance")
	if bbody["balance_cents"] != float64(0) {
		t.Errorf("cash USD balance = %v, want 0", bbody["balance_cents"])
	}
}

// TestHTTPPostEntryInvalidCurrency checks that a malformed currency code
// is a 400 and bumps the currency-rejections metric.
func TestHTTPPostEntryInvalidCurrency(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":500,"currency":"euro"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%v)", code, body)
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_currency_rejections_total"] != 1 {
		t.Errorf("ledger_currency_rejections_total = %d, want 1", m["ledger_currency_rejections_total"])
	}
	if m["ledger_posts_total"] != 1 {
		t.Errorf("ledger_posts_total = %d, want 1", m["ledger_posts_total"])
	}
}

// TestHTTPTransferWithCurrency checks that POST /transfers books the
// transfer in the requested currency and the receipt entries carry it.
func TestHTTPTransferWithCurrency(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000,"currency":"EUR"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-cur","from_account":"alice","to_account":"bob","amount_cents":2500,"currency":"EUR","idempotency_key":"tk-cur"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly one entry", body["entries"])
	}
	if entries[0].(map[string]any)["currency"] != "EUR" {
		t.Errorf("transfer entry currency = %v, want EUR", entries[0].(map[string]any)["currency"])
	}
}

// TestHTTPTransferInvalidCurrency checks that POST /transfers rejects a
// malformed currency code with 400.
func TestHTTPTransferInvalidCurrency(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-cur-bad","from_account":"alice","to_account":"bob","amount_cents":100,"currency":"EURO"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	if m := parseExposition(t, mbody); m["ledger_currency_rejections_total"] != 1 {
		t.Errorf("ledger_currency_rejections_total = %d, want 1", m["ledger_currency_rejections_total"])
	}
}

// TestHTTPTrialBalanceByCurrency checks the grouped trial-balance JSON:
// top-level totals are the default-currency view, by_currency lists every
// currency the account holds.
func TestHTTPTrialBalanceByCurrency(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	for _, req := range []string{
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1000}`,
		`{"debit_account":"cash","credit_account":"equity","amount_cents":2000,"currency":"EUR"}`,
	} {
		if code, _ := postJSON(t, srv.URL+"/entries", req); code != http.StatusCreated {
			t.Fatalf("seed post %s: status=%d, want 201", req, code)
		}
	}

	code, body := getJSON(t, srv.URL+"/accounts/cash/trial-balance")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["currency"] != "USD" {
		t.Errorf("currency = %v, want USD (default-currency view)", body["currency"])
	}
	if body["net_balance_cents"] != float64(1000) {
		t.Errorf("net_balance_cents = %v, want 1000", body["net_balance_cents"])
	}
	rows, ok := body["by_currency"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("by_currency = %v, want 2 rows", body["by_currency"])
	}
	got := map[string]float64{}
	for _, r := range rows {
		row := r.(map[string]any)
		got[row["currency"].(string)] = row["net_balance_cents"].(float64)
	}
	if got["USD"] != 1000 || got["EUR"] != 2000 {
		t.Errorf("by_currency nets = %v, want USD=1000 EUR=2000", got)
	}
}

// TestHTTPReconcileCurrencyTotals checks that POST /reconcile reports
// per-currency totals: each currency's debits equal its credits, and the
// top-level totals stay the default-currency row.
func TestHTTPReconcileCurrencyTotals(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	for _, req := range []string{
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1000}`,
		`{"debit_account":"cash","credit_account":"equity","amount_cents":2000,"currency":"EUR"}`,
	} {
		if code, _ := postJSON(t, srv.URL+"/entries", req); code != http.StatusCreated {
			t.Fatalf("seed post %s: status=%d, want 201", req, code)
		}
	}

	code, body := postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["accounting_equation_ok"] != true {
		t.Errorf("accounting_equation_ok = %v, want true (body=%v)", body["accounting_equation_ok"], body)
	}
	rows, ok := body["currency_totals"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("currency_totals = %v, want 2 rows", body["currency_totals"])
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["total_debits_cents"] != row["total_credits_cents"] {
			t.Errorf("currency %v unbalanced: %v", row["currency"], row)
		}
	}
	if body["total_debits_cents"] != float64(1000) {
		t.Errorf("total_debits_cents = %v, want 1000 (default-currency row)", body["total_debits_cents"])
	}
}
