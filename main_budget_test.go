package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func metricsBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
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
	return sb.String()
}

// POST /entries past the payer's monthly spend budget returns 422 and is
// counted by ledger_budget_rejections_total.
func TestBudgetPostReturns422AndCountsMetric(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("post at budget: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":1}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("post over budget: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
	if !strings.Contains(metricsBody(t, srv.URL), "ledger_budget_rejections_total 1") {
		t.Fatalf("/metrics missing budget counter 1:\n%s", metricsBody(t, srv.URL))
	}
}

// POST /transfers past the payer's monthly budget (amount + fee) returns
// 422 and is counted by the same counter.
func TestBudgetTransferReturns422(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer at budget: status=%d, want 201 (body check skipped)", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t2","from_account":"payer","to_account":"payee","amount_cents":1}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer over budget: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
	if !strings.Contains(metricsBody(t, srv.URL), "ledger_budget_rejections_total 1") {
		t.Fatalf("/metrics missing budget counter 1:\n%s", metricsBody(t, srv.URL))
	}
}

// POST /entries/batch past a credit account's monthly budget returns 422.
func TestBudgetBatchReturns422(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("payroll", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"b1","entries":[
			{"entry_id":"e1","debit_account":"alice","credit_account":"payroll","amount_cents":600,"currency":"USD"},
			{"entry_id":"e2","debit_account":"bob","credit_account":"payroll","amount_cents":500,"currency":"USD"}
		]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("batch over budget: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
}

// POST /sweeps whose source leg would breach the source's monthly budget
// returns 422.
func TestBudgetSweepReturns422(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("sub-a", "USD", 100))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Fund sub-a first: funding books spend on the funder, not sub-a.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"sub-a","credit_account":"bank","amount_cents":5000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding sub-a: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/sweeps",
		`{"sweep_id":"sweep-1","from_accounts":["sub-a"],"to_account":"treasury"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("sweep over budget: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
}

// POST /holds/{id}/capture past the hold account's monthly budget returns
// 422.
func TestBudgetCaptureReturns422(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("card", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"card","credit_account":"bank","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding card: status=%d, want 201", code)
	}
	// The hold itself is a reservation, not spend: it must not trip the
	// budget.
	code, _ = postJSON(t, srv.URL+"/holds",
		fmt.Sprintf(`{"hold_id":"h1","account":"card","amount_cents":6000,"currency":"USD","expires_at":%q}`,
			"2099-01-01T00:00:00Z"))
	if code != http.StatusCreated {
		t.Fatalf("hold: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/holds/h1/capture",
		`{"capture_id":"c1","to_account":"merchant","amount_cents":6000}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("capture over budget: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
	if !strings.Contains(metricsBody(t, srv.URL), "ledger_budget_rejections_total 1") {
		t.Fatalf("/metrics missing budget counter 1:\n%s", metricsBody(t, srv.URL))
	}
}

// POST /entries/dry-run previews the budget impact of a simulated posting.
func TestBudgetDryRunPreviewsImpact(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":600}`)
	if code != http.StatusCreated {
		t.Fatalf("post 600: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":300}`)
	if code != http.StatusOK {
		t.Fatalf("dry-run: status=%d, want 200 (body=%v)", code, body)
	}
	impact, ok := body["budget_impact"].([]any)
	if !ok || len(impact) != 1 {
		t.Fatalf("budget_impact = %v, want exactly one row", body["budget_impact"])
	}
	row := impact[0].(map[string]any)
	if row["account"] != "payer" || row["currency"] != "USD" {
		t.Fatalf("budget_impact row = %v, want payer/USD", row)
	}
	if row["budget_cents"] != float64(1000) || row["used_before_cents"] != float64(600) ||
		row["used_after_cents"] != float64(900) {
		t.Fatalf("budget_impact row = %v, want budget 1000 before 600 after 900", row)
	}

	// A dry run that would breach the budget fails 422 like the real call.
	code, body = postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"merchant","credit_account":"payer","amount_cents":401}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("dry-run over budget: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
}

// POST /transfers/dry-run that would breach the budget fails 422 like the
// real call.
func TestBudgetDryRunTransferReturns422(t *testing.T) {
	l := ledger.New(ledger.WithAccountBudget("payer", "USD", 1000))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":1001}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("dry-run transfer over budget: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "monthly spend budget") {
		t.Fatalf("422 body = %v, want a monthly-budget error", body)
	}
}
