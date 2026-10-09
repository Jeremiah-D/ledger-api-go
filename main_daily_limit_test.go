package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// POST /entries past the payer's daily outflow limit returns 422 and is
// counted by ledger_daily_limit_rejections_total.
func TestDailyLimitPostReturns422AndCountsMetric(t *testing.T) {
	l := ledger.New(ledger.WithDailyLimit("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("post at limit: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":1}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("post over limit: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "daily outflow limit") {
		t.Fatalf("422 body = %v, want a daily-limit error", body)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_daily_limit_rejections_total 1") {
		t.Fatalf("/metrics missing daily-limit counter 1:\n%s", sb.String())
	}
}

// POST /transfers past the payer's daily outflow limit (amount + fee)
// returns 422 and is counted by the same counter.
func TestDailyLimitTransferReturns422(t *testing.T) {
	l := ledger.New(ledger.WithDailyLimit("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer at limit: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t2","from_account":"payer","to_account":"payee","amount_cents":1}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer over limit: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "daily outflow limit") {
		t.Fatalf("422 body = %v, want a daily-limit error", body)
	}
}
