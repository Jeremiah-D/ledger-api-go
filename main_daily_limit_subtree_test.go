package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedSubtreeServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if err := l.SetDailyLimit("parent", "USD", 1000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	if err := l.SetParent("child", "parent"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if _, _, err := l.Post(ledger.JournalEntry{ID: "fund-child", DebitAccount: "child", CreditAccount: "equity", AmountCents: 100000}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

// A child's posting that would breach the parent's subtree budget is
// 422, and the rejection is counted.
func TestSubtreeLimitRejectionHTTP(t *testing.T) {
	srv := seedSubtreeServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"child","amount_cents":600}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries 600: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"child","amount_cents":500}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /entries over subtree limit: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "daily outflow limit") {
		t.Fatalf("422 body=%v, want a daily-limit error", body)
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
		t.Fatalf("metrics missing ledger_daily_limit_rejections_total 1:\n%s", sb.String())
	}
}

// Linking a subtree under a parent whose limit the subtree would breach
// is 422, and the link is not installed.
func TestSetParentSubtreeValidationHTTP(t *testing.T) {
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{ID: "fund-child", DebitAccount: "child", CreditAccount: "equity", AmountCents: 100000}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"child","amount_cents":80000}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries 800: status=%d, want 201", code)
	}
	if err := l.SetDailyLimit("parent", "USD", 50000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	code, body := postJSON(t, srv.URL+"/accounts/child/parent", `{"parent":"parent"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /accounts/child/parent into breached limit: status=%d body=%v, want 422", code, body)
	}
	// The rejected link left no trace: the child is still parentless.
	code, body = getJSON(t, srv.URL+"/accounts/child/rollup")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/child/rollup: status=%d", code)
	}
	if len(body["accounts"].([]any)) != 1 {
		t.Fatalf("rollup accounts=%v, want just the child", body["accounts"])
	}
}

// POST /reconcile carries the subtree_limit_utilization section.
func TestReconcileSubtreeUtilizationHTTP(t *testing.T) {
	srv := seedSubtreeServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"merchant","credit_account":"child","amount_cents":250}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries 250: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/reconcile", "")
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d, want 200", code)
	}
	rows, ok := body["subtree_limit_utilization"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("subtree_limit_utilization=%v, want 1 row", body["subtree_limit_utilization"])
	}
	row := rows[0].(map[string]any)
	if row["account"] != "parent" || row["limit_cents"] != float64(1000) ||
		row["subtree_used_cents"] != float64(250) || row["subtree_accounts"] != float64(2) ||
		row["utilization"] != float64(0.25) || row["exceeded"] != false {
		t.Fatalf("subtree_limit_utilization row=%v, want parent/USD 1000/25000/2/25/false", row)
	}
}
