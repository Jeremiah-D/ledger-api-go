package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// LG-151: HTTP surface for period auto-close plans.

func TestAutoCloseEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	at := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	// Schedule a plan.
	code, body := postJSON(t, srv.URL+"/periods/2026-09/auto-close",
		`{"at":"`+at+`","closing":false}`)
	if code != http.StatusCreated {
		t.Fatalf("schedule: status=%d body=%v, want 201", code, body)
	}
	if body["period"] != "2026-09" || body["status"] != "pending" {
		t.Fatalf("schedule body = %v", body)
	}

	// Duplicate is 409.
	code, body = postJSON(t, srv.URL+"/periods/2026-09/auto-close",
		`{"at":"`+at+`","closing":false}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate: status=%d body=%v, want 409", code, body)
	}

	// Bad period ID and past time are 400.
	code, _ = postJSON(t, srv.URL+"/periods/nope/auto-close",
		`{"at":"`+at+`","closing":false}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad period: status=%d, want 400", code)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/periods/2026-09/auto-close",
		`{"at":"`+past+`","closing":false}`)
	if code != http.StatusBadRequest {
		t.Fatalf("past time: status=%d, want 400", code)
	}

	// Query the plan.
	code, body = getJSON(t, srv.URL+"/periods/2026-09/auto-close")
	if code != http.StatusOK || body["status"] != "pending" {
		t.Fatalf("get plan: status=%d body=%v", code, body)
	}
	code, _ = getJSON(t, srv.URL+"/periods/2026-10/auto-close")
	if code != http.StatusNotFound {
		t.Fatalf("get missing plan: status=%d, want 404", code)
	}

	// List plans.
	code, body = getJSON(t, srv.URL+"/periods/auto-close")
	if code != http.StatusOK {
		t.Fatalf("list plans: status=%d", code)
	}
	plans, ok := body["plans"].([]any)
	if !ok || len(plans) != 1 {
		t.Fatalf("list plans body = %v", body)
	}

	// Cancel the plan.
	code, body = postJSON(t, srv.URL+"/periods/2026-09/auto-close/cancel", "")
	if code != http.StatusOK || body["status"] != "cancelled" {
		t.Fatalf("cancel: status=%d body=%v", code, body)
	}
	// Cancelling again is 409.
	code, _ = postJSON(t, srv.URL+"/periods/2026-09/auto-close/cancel", "")
	if code != http.StatusConflict {
		t.Fatalf("cancel again: status=%d, want 409", code)
	}
	// Cancelling a missing plan is 404.
	code, _ = postJSON(t, srv.URL+"/periods/2026-10/auto-close/cancel", "")
	if code != http.StatusNotFound {
		t.Fatalf("cancel missing: status=%d, want 404", code)
	}
}

func TestAutoCloseEnvInterval(t *testing.T) {
	t.Setenv("LEDGER_PERIOD_AUTOCLOSE_INTERVAL", "not-a-duration")
	if _, ok := periodAutoCloseInterval(); ok {
		t.Fatalf("invalid interval accepted")
	}
	t.Setenv("LEDGER_PERIOD_AUTOCLOSE_INTERVAL", "0s")
	if _, ok := periodAutoCloseInterval(); ok {
		t.Fatalf("non-positive interval accepted")
	}
	t.Setenv("LEDGER_PERIOD_AUTOCLOSE_INTERVAL", "30s")
	d, ok := periodAutoCloseInterval()
	if !ok || d != 30*time.Second {
		t.Fatalf("interval = %v, %v", d, ok)
	}
}
