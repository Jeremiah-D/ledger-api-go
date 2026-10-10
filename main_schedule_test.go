package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestTransferScheduleHTTPLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund alice first.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// Create a schedule: 201.
	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "rent",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-10-09T00:00:00Z",
		"idempotency_key": "sched-key-1"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	sched, ok := body["schedule"].(map[string]any)
	if !ok {
		t.Fatalf("schedule = %v, want object", body["schedule"])
	}
	if sched["id"] != "rent" || sched["status"] != "active" {
		t.Errorf("schedule = %v, want id rent active", sched)
	}
	if sched["interval"] != "24h0m0s" {
		t.Errorf("interval = %v, want 24h0m0s", sched["interval"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}

	// Replay the creation key: 200 + duplicate.
	code, body = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "rent-again",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 1,
		"interval": "24h",
		"next_run_at": "2026-10-09T00:00:00Z",
		"idempotency_key": "sched-key-1"
	}`)
	if code != http.StatusOK {
		t.Fatalf("replay schedule: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}
	if body["schedule"].(map[string]any)["id"] != "rent" {
		t.Errorf("replay returned wrong schedule: %v", body["schedule"])
	}

	// Bad interval: 400.
	code, _ = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "bad",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 100,
		"interval": "weekly",
		"next_run_at": "2026-10-09T00:00:00Z"
	}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad interval: status=%d, want 400", code)
	}

	// Get the schedule: 200.
	code, body = getJSON(t, srv.URL+"/transfer-schedules/rent")
	if code != http.StatusOK {
		t.Fatalf("get schedule: status=%d, want 200", code)
	}
	if body["id"] != "rent" {
		t.Errorf("get id = %v, want rent", body["id"])
	}

	// Unknown schedule: 404.
	code, _ = getJSON(t, srv.URL+"/transfer-schedules/nope")
	if code != http.StatusNotFound {
		t.Errorf("get unknown: status=%d, want 404", code)
	}

	// Runs: empty so far (the sweeper is not running in tests).
	code, body = getJSON(t, srv.URL+"/transfer-schedules/rent/runs")
	if code != http.StatusOK {
		t.Fatalf("list runs: status=%d, want 200", code)
	}
	if runs, ok := body["runs"].([]any); !ok || len(runs) != 0 {
		t.Errorf("runs = %v, want empty", body["runs"])
	}

	// Pause: 200, then pause again: 409.
	code, body = postJSON(t, srv.URL+"/transfer-schedules/rent/pause", `{}`)
	if code != http.StatusOK {
		t.Fatalf("pause: status=%d, want 200 (body=%v)", code, body)
	}
	if body["status"] != "paused" {
		t.Errorf("status = %v, want paused", body["status"])
	}
	code, _ = postJSON(t, srv.URL+"/transfer-schedules/rent/pause", `{}`)
	if code != http.StatusConflict {
		t.Errorf("double pause: status=%d, want 409", code)
	}

	// Resume: 200.
	code, body = postJSON(t, srv.URL+"/transfer-schedules/rent/resume", `{}`)
	if code != http.StatusOK {
		t.Fatalf("resume: status=%d, want 200", code)
	}
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}

	// Cancel: 200, then resume: 409.
	code, _ = postJSON(t, srv.URL+"/transfer-schedules/rent/cancel", `{}`)
	if code != http.StatusOK {
		t.Fatalf("cancel: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/transfer-schedules/rent/resume", `{}`)
	if code != http.StatusConflict {
		t.Errorf("resume cancelled: status=%d, want 409", code)
	}

	// Unknown schedule transition: 404.
	code, _ = postJSON(t, srv.URL+"/transfer-schedules/nope/pause", `{}`)
	if code != http.StatusNotFound {
		t.Errorf("pause unknown: status=%d, want 404", code)
	}

	// The sweeper counter is wired into /metrics (zero: the background
	// worker does not run in tests).
	code, metrics, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(metrics, "ledger_scheduled_transfers_total 0") {
		t.Errorf("metrics missing ledger_scheduled_transfers_total:\n%s", metrics)
	}
}
