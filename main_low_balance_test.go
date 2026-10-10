package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestMetricsLowBalanceBreaches drives a breach through the HTTP API and
// checks that GET /metrics exposes the synced ledger_low_balance_breaches_total
// counter (the alert fires inside the ledger, not in a handler, so the
// counter is synced on every scrape like the audit-log counters).
func TestMetricsLowBalanceBreaches(t *testing.T) {
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{ID: "seed", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postEntry(t, srv.URL+"/entries", `{"debit_account":"bank","credit_account":"cust-1","amount_cents":800,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201", code)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ledger_low_balance_breaches_total 1\n") {
		t.Fatalf("metrics missing ledger_low_balance_breaches_total 1:\n%s", body)
	}
}
