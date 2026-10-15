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
