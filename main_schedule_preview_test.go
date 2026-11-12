package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSchedulePreviewHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund alice first.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// Create a schedule with the first run 30 minutes out.
	nextRun := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "rent",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "`+nextRun+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201", code)
	}

	// Happy path: 200 with two admitted firings inside a 30h horizon
	// (nominal +30m and +24.5h; the third lands at +48.5h, outside).
	code, body := getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon=30h")
	if code != http.StatusOK {
		t.Fatalf("preview: status=%d, want 200 (body=%v)", code, body)
	}
	if body["schedule_id"] != "rent" {
		t.Errorf("schedule_id = %v, want rent", body["schedule_id"])
	}
	firings, ok := body["firings"].([]any)
	if !ok || len(firings) != 2 {
		t.Fatalf("firings = %v, want 2 rows", body["firings"])
	}
	for i, fr := range firings {
		f := fr.(map[string]any)
		if f["admitted"] != true {
			t.Errorf("firing %d: admitted = %v, want true", i, f["admitted"])
		}
		if f["transfer_id"] == "" || f["due_at"] == "" {
			t.Errorf("firing %d: missing transfer_id/due_at: %v", i, f)
		}
		if legs, ok := f["legs"].([]any); !ok || len(legs) == 0 {
			t.Errorf("firing %d: legs = %v, want non-empty", i, f["legs"])
		}
	}

	// Unknown schedule: 404.
	code, _ = getJSON(t, srv.URL+"/transfer-schedules/nope/preview")
	if code != http.StatusNotFound {
		t.Errorf("unknown id: status=%d, want 404", code)
	}

	// Invalid horizons: 400.
	for _, h := range []string{"abc", "-1h", "0s", "9000h"} {
		code, _ = getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon="+h)
		if code != http.StatusBadRequest {
			t.Errorf("horizon %q: status=%d, want 400", h, code)
		}
	}

	// The preview fired nothing: no run history was recorded.
	code, body = getJSON(t, srv.URL+"/transfer-schedules/rent/runs")
	if code != http.StatusOK {
		t.Fatalf("runs: status=%d, want 200", code)
	}
	if runs, _ := body["runs"].([]any); len(runs) != 0 {
		t.Errorf("runs = %d, want 0 (preview records nothing)", len(runs))
	}
}

// TestSchedulePreviewHTTPFollowsLiveHorizonCap pins that the HTTP
// preview bound tracks the live horizon cap (LG-399): loosening
// the cap via the Go API opens the window for the next HTTP
// preview, and tightening it closes the window again — the handler
// must not stay pinned to the construction-time 8760h.
func TestSchedulePreviewHTTPFollowsLiveHorizonCap(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	nextRun := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "rent",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "`+nextRun+`"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201", code)
	}

	// At the default 8760h cap, a 10000h horizon is refused.
	code, _ = getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon=10000h")
	if code != http.StatusBadRequest {
		t.Errorf("horizon=10000h at default cap: status=%d, want 400", code)
	}

	// Loosen to 17520h: the same horizon now previews (two
	// firings inside 10000h at a 24h cadence from +30m).
	if _, err := l.SetSchedulePreviewMaxHorizon(17520 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(17520h): %v", err)
	}
	code, body := getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon=10000h")
	if code != http.StatusOK {
		t.Fatalf("horizon=10000h after loosen: status=%d, want 200 (body=%v)", code, body)
	}
	if firings, ok := body["firings"].([]any); !ok || len(firings) != 417 {
		t.Fatalf("firings = %v, want 417 rows", body["firings"])
	}

	// Tighten to 48h: the 10000h horizon is refused again, and
	// even a 72h horizon is refused.
	if _, err := l.SetSchedulePreviewMaxHorizon(48 * time.Hour); err != nil {
		t.Fatalf("SetSchedulePreviewMaxHorizon(48h): %v", err)
	}
	for _, h := range []string{"10000h", "72h"} {
		code, _ = getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon="+h)
		if code != http.StatusBadRequest {
			t.Errorf("horizon=%s after tighten: status=%d, want 400", h, code)
		}
	}
	// A 24h horizon still previews inside the tightened cap.
	code, body = getJSON(t, srv.URL+"/transfer-schedules/rent/preview?horizon=24h")
	if code != http.StatusOK {
		t.Fatalf("horizon=24h after tighten: status=%d, want 200 (body=%v)", code, body)
	}
	if firings, ok := body["firings"].([]any); !ok || len(firings) != 1 {
		t.Fatalf("firings = %v, want 1 row", body["firings"])
	}
}
