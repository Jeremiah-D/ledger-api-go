package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedTermDepositServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(
		ledger.WithTermDepositInterestExpense("td-interest-expense"),
		ledger.WithTransferFeePolicy(0, "fee-revenue"),
	)
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPTermDepositFlow(t *testing.T) {
	srv := seedTermDepositServer(t)
	defer srv.Close()

	// Open a 30-day deposit.
	code, body := postJSON(t, srv.URL+"/term-deposits",
		`{"id":"td-1","account":"saver","amount_cents":400000,"term_days":30,"rate_bps":500,"idempotency_key":"k1"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /term-deposits status = %d, want 201 (body %v)", code, body)
	}
	if body["status"] != "active" || body["id"] != "td-1" {
		t.Errorf("body = %v, want active td-1", body)
	}

	// Available drops by the locked principal; the net balance is
	// untouched.
	code, bal := getJSON(t, srv.URL+"/accounts/saver/balance")
	if code != http.StatusOK {
		t.Fatalf("GET /balance status = %d", code)
	}
	if bal["balance_cents"] != float64(1_000_000) || bal["available_cents"] != float64(600_000) {
		t.Errorf("balance = %v, want balance_cents 1000000 / available_cents 600000", bal)
	}

	// Duplicate idempotency key replays the original deposit.
	code, dup := postJSON(t, srv.URL+"/term-deposits",
		`{"id":"td-2","account":"saver","amount_cents":1,"term_days":1,"rate_bps":1,"idempotency_key":"k1"}`)
	if code != http.StatusOK || dup["id"] != "td-1" {
		t.Errorf("replay = status %d body %v, want 200 with id td-1", code, dup)
	}

	// Accrue one day of interest.
	code, entry := postJSON(t, srv.URL+"/term-deposits/td-1/accrue?asof=2026-03-10T12:00:00Z", "")
	if code != http.StatusCreated {
		t.Fatalf("POST /accrue status = %d, want 201 (body %v)", code, entry)
	}
	if entry["id"] != "term-interest/td-1/2026-03-10" {
		t.Errorf("interest entry id = %v, want deterministic ID", entry["id"])
	}

	// Same day replays with 200.
	code, _ = postJSON(t, srv.URL+"/term-deposits/td-1/accrue?asof=2026-03-10T12:00:00Z", "")
	if code != http.StatusOK {
		t.Errorf("accrue replay status = %d, want 200", code)
	}

	// Maturing before maturity is 422.
	code, _ = postJSON(t, srv.URL+"/term-deposits/td-1/mature", "")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("early mature status = %d, want 422", code)
	}

	// Unknown deposit is 404.
	code, _ = postJSON(t, srv.URL+"/term-deposits/nope/accrue", "")
	if code != http.StatusNotFound {
		t.Errorf("unknown accrue status = %d, want 404", code)
	}
}

func TestHTTPTermDepositWithdraw(t *testing.T) {
	srv := seedTermDepositServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/term-deposits",
		`{"id":"td-w","account":"saver","amount_cents":100000,"term_days":90,"rate_bps":500}`)
	if code != http.StatusCreated {
		t.Fatalf("open status = %d", code)
	}
	// 1% early-withdrawal penalty: 1000 cents to the fee-revenue account.
	code, receipt := postJSON(t, srv.URL+"/term-deposits/td-w/withdraw", `{"penalty_bps":100}`)
	if code != http.StatusCreated {
		t.Fatalf("withdraw status = %d, want 201 (body %v)", code, receipt)
	}
	if receipt["penalty_cents"] != float64(1000) {
		t.Errorf("penalty_cents = %v, want 1000", receipt["penalty_cents"])
	}
	dep, _ := receipt["deposit"].(map[string]any)
	if dep["status"] != "withdrawn_early" {
		t.Errorf("deposit status = %v, want withdrawn_early", dep["status"])
	}
	// Principal unlocked; penalty moved to fee revenue.
	code, bal := getJSON(t, srv.URL+"/accounts/saver/balance")
	if bal["available_cents"] != float64(999_000) {
		t.Errorf("available = %v, want 999000", bal["available_cents"])
	}
	// Re-withdrawing is an idempotent 200.
	code, _ = postJSON(t, srv.URL+"/term-deposits/td-w/withdraw", `{"penalty_bps":100}`)
	if code != http.StatusOK {
		t.Errorf("re-withdraw status = %d, want 200", code)
	}
	// Bad penalty rate is 400.
	code, _ = postJSON(t, srv.URL+"/term-deposits/td-w/withdraw", `{"penalty_bps":10001}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad penalty status = %d, want 400", code)
	}
}

func TestHTTPTermDepositSweep(t *testing.T) {
	srv := seedTermDepositServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/term-deposits/sweep", "")
	if code != http.StatusOK {
		t.Fatalf("sweep status = %d", code)
	}
}

func TestHTTPTermDepositInsufficientFunds(t *testing.T) {
	srv := seedTermDepositServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/term-deposits",
		`{"id":"td-big","account":"saver","amount_cents":2000000,"term_days":7,"rate_bps":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("oversize open status = %d, want 422 (body %v)", code, body)
	}
}

func TestHTTPTermDepositGet(t *testing.T) {
	srv := seedTermDepositServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/term-deposits",
		`{"id":"td-g","account":"saver","amount_cents":100000,"term_days":7,"rate_bps":100}`)
	if code != http.StatusCreated {
		t.Fatalf("open status = %d", code)
	}
	code, body := getJSON(t, srv.URL+"/term-deposits/td-g")
	if code != http.StatusOK || body["id"] != "td-g" {
		t.Errorf("get = status %d body %v, want 200 td-g", code, body)
	}
	code, _ = getJSON(t, fmt.Sprintf("%s/term-deposits/nope", srv.URL))
	if code != http.StatusNotFound {
		t.Errorf("unknown get status = %d, want 404", code)
	}
}
