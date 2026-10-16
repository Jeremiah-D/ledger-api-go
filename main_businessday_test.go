package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestTransferScheduleBusinessDayCadenceHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "payroll",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-10-16T10:00:00Z",
		"business_day_cadence": true
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	sched, ok := body["schedule"].(map[string]any)
	if !ok {
		t.Fatalf("schedule = %v, want object", body["schedule"])
	}
	if sched["business_day_cadence"] != true {
		t.Fatalf("business_day_cadence = %v, want true", sched["business_day_cadence"])
	}
}

func TestTermDepositBusinessDayMaturityHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"saver","credit_account":"funding","amount_cents":1000000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/term-deposits", `{
		"id": "td-bd-http",
		"account": "saver",
		"amount_cents": 100000,
		"term_days": 30,
		"rate_bps": 500,
		"business_day_maturity": true
	}`)
	if code != http.StatusCreated {
		t.Fatalf("open term deposit: status=%d, want 201 (body=%v)", code, body)
	}
	// The handler returns the TermDeposit record directly.
	if body["business_day_maturity"] != true {
		t.Fatalf("business_day_maturity = %v, want true", body["business_day_maturity"])
	}
}
