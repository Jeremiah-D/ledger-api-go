package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedECLServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(
		ledger.WithECLLossExpense("ecl-expense"),
		ledger.WithECLAllowance("ecl-allowance"),
	)
	return httptest.NewServer(newRouter(l))
}

func TestHTTPECLFlow(t *testing.T) {
	srv := seedECLServer(t)
	defer srv.Close()

	// Register a manual exposure.
	code, body := postJSON(t, srv.URL+"/ecl/exposures",
		`{"account":"borrower","currency":"USD","exposure_cents":100000,"days_past_due":10}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /ecl/exposures status = %d, want 201 (body %v)", code, body)
	}
	if body["stage"] != float64(1) || body["exposure_cents"] != float64(100_000) {
		t.Errorf("body = %v, want stage 1 / exposure 100000", body)
	}

	// A negative exposure is 400.
	code, _ = postJSON(t, srv.URL+"/ecl/exposures",
		`{"account":"borrower","currency":"USD","exposure_cents":-5,"days_past_due":0}`)
	if code != http.StatusBadRequest {
		t.Errorf("negative exposure status = %d, want 400", code)
	}

	// Update DPD into stage 2.
	code, _ = postJSON(t, srv.URL+"/ecl/exposures/dpd",
		`{"account":"borrower","currency":"USD","days_past_due":45}`)
	if code != http.StatusOK {
		t.Fatalf("POST /ecl/exposures/dpd status = %d, want 200", code)
	}

	// Unknown obligor DPD update is 404.
	code, _ = postJSON(t, srv.URL+"/ecl/exposures/dpd",
		`{"account":"ghost","currency":"USD","days_past_due":45}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown obligor dpd status = %d, want 404", code)
	}

	// Book the provision: stage 2 at 45 DPD, floor(100_000 * 1000 / 10000) = 10000.
	code, entry := postJSON(t, srv.URL+"/ecl/provisions?asof=2026-04-02T09:00:00Z",
		`{"account":"borrower","currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /ecl/provisions status = %d, want 201 (body %v)", code, entry)
	}
	if entry["amount_cents"] != float64(10_000) {
		t.Errorf("entry = %v, want amount_cents 10000", entry)
	}
	if entry["id"] != "ecl/borrower/USD/2026-04-02" {
		t.Errorf("entry id = %v, want deterministic ecl/<account>/<currency>/<date>", entry["id"])
	}

	// Re-running the same day replays with 200.
	code, entry2 := postJSON(t, srv.URL+"/ecl/provisions?asof=2026-04-02T09:00:00Z",
		`{"account":"borrower","currency":"USD"}`)
	if code != http.StatusOK {
		t.Errorf("replay status = %d, want 200", code)
	}
	if entry2["id"] != entry["id"] {
		t.Errorf("replay id = %v, want %v", entry2["id"], entry["id"])
	}

	// No tracked exposure is 404.
	code, _ = postJSON(t, srv.URL+"/ecl/provisions", `{"account":"ghost","currency":"USD"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown obligor provision status = %d, want 404", code)
	}

	// List exposures.
	resp, err := http.Get(srv.URL + "/ecl/exposures")
	if err != nil {
		t.Fatalf("GET /ecl/exposures: %v", err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode exposures: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ecl/exposures status = %d, want 200", resp.StatusCode)
	}
	if len(list) != 1 || list[0]["account"] != "borrower" || list[0]["stage"] != float64(2) {
		t.Errorf("exposures = %v, want one row borrower/stage 2", list)
	}

	// Set an invalid stage config: 400, and the config is unchanged.
	code, _ = postJSON(t, srv.URL+"/ecl/config",
		`{"stage2_dpd":90,"stage3_dpd":30,"rate_stage1_bps":100,"rate_stage2_bps":1000,"rate_stage3_bps":5000}`)
	if code != http.StatusBadRequest {
		t.Errorf("invalid config status = %d, want 400", code)
	}
	code, cfg := postJSON(t, srv.URL+"/ecl/config",
		`{"stage2_dpd":20,"stage3_dpd":60,"rate_stage1_bps":50,"rate_stage2_bps":2000,"rate_stage3_bps":8000}`)
	if code != http.StatusOK {
		t.Fatalf("valid config status = %d, want 200", code)
	}
	if cfg["stage2_dpd"] != float64(20) || cfg["rate_stage3_bps"] != float64(8000) {
		t.Errorf("config = %v", cfg)
	}
}

func TestHTTPECLFailClosed(t *testing.T) {
	l := ledger.New() // no expense/allowance accounts configured
	if _, err := l.RegisterECLExposure("b", "USD", 100_000, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/ecl/provisions", `{"account":"b","currency":"USD"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("unconfigured provision status = %d, want 500 (body %v)", code, body)
	}
}
