package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestReversePartialLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, ebody := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1000,"idempotency_key":"e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post entry: status=%d, want 201", code)
	}
	entryID, _ := ebody["id"].(string)
	if entryID == "" {
		t.Fatalf("post entry: empty id in %v", ebody)
	}

	path := srv.URL + "/entries/" + entryID + "/reverse-partial"
	revFullPath := srv.URL + "/entries/" + entryID + "/reverse"

	// First partial reversal books e1/reversal/1.
	code, body := postJSON(t, path, `{"amount_cents":300,"idempotency_key":"p1"}`)
	if code != http.StatusCreated {
		t.Fatalf("partial reverse: status=%d body=%v, want 201", code, body)
	}
	if body["id"] != entryID+"/reversal/1" {
		t.Errorf("partial id = %v, want %s", body["id"], entryID+"/reversal/1")
	}

	// Same idempotency key replays.
	code, body = postJSON(t, path, `{"amount_cents":300,"idempotency_key":"p1"}`)
	if code != http.StatusOK || body["id"] != entryID+"/reversal/1" {
		t.Fatalf("replay: status=%d body=%v, want 200 %s", code, body, entryID+"/reversal/1")
	}

	// Second partial books the next index.
	code, body = postJSON(t, path, `{"amount_cents":400}`)
	if code != http.StatusCreated || body["id"] != entryID+"/reversal/2" {
		t.Fatalf("second partial: status=%d body=%v, want 201 %s", code, body, entryID+"/reversal/2")
	}

	// 300+400 booked; 400 more would exceed the 1000 cap.
	code, _ = postJSON(t, path, `{"amount_cents":400}`)
	if code != http.StatusConflict {
		t.Fatalf("over-cap partial: status=%d, want 409", code)
	}

	// A full reversal would over-correct the 700 already reversed.
	code, _ = postJSON(t, revFullPath, `{}`)
	if code != http.StatusConflict {
		t.Fatalf("full after partial: status=%d, want 409", code)
	}

	// Non-positive amounts are field errors.
	code, _ = postJSON(t, path, `{"amount_cents":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("zero amount: status=%d, want 400", code)
	}

	// Unknown entry.
	code, _ = postJSON(t, srv.URL+"/entries/nope/reverse-partial", `{"amount_cents":10}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown entry: status=%d, want 404", code)
	}

	// The kind dimension moved: two partials committed, no full.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(raw), "ledger_reversals_total{kind=\"partial\"} 2\n") {
		t.Fatalf("/metrics missing ledger_reversals_total{kind=\"partial\"} 2:\n%s", raw)
	}
	if !strings.Contains(string(raw), "ledger_reversals_total{kind=\"full\"} 0\n") {
		t.Fatalf("/metrics missing ledger_reversals_total{kind=\"full\"} 0:\n%s", raw)
	}
}
