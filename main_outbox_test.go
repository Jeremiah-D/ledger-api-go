package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedOutboxServer builds a test server with the domain-event outbox
// attached to a temp dir. The outbox is closed on test cleanup.
func seedOutboxServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	ob, err := ledger.NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	srv := httptest.NewServer(newRouter(ledger.New(ledger.WithOutbox(ob))))
	return srv, dir
}

func TestHandleOutboxEvents(t *testing.T) {
	srv, _ := seedOutboxServer(t)
	defer srv.Close()

	// Post two entries and freeze an account: 2 entry_posted + 1 account_frozen.
	for _, cents := range []int{100, 200} {
		code, _ := postJSON(t, srv.URL+"/entries",
			`{"debit_account":"cash","credit_account":"equity","amount_cents":`+fmt.Sprint(cents)+`}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /entries: status=%d, want 201", code)
		}
	}
	if code, _ := postJSON(t, srv.URL+"/accounts/mallory/freeze", ""); code != http.StatusOK {
		t.Fatalf("freeze: status=%d, want 200", code)
	}

	code, body := getJSON(t, srv.URL+"/events")
	if code != http.StatusOK {
		t.Fatalf("GET /events: status=%d body=%v, want 200", code, body)
	}
	events, ok := body["events"].([]any)
	if !ok || len(events) != 3 {
		t.Fatalf("events = %v, want 3", body["events"])
	}
	if body["next_cursor"] != "" {
		t.Errorf("next_cursor = %v, want empty (single page)", body["next_cursor"])
	}
	first := events[0].(map[string]any)
	if first["type"] != "entry_posted" || first["seq"] != float64(1) {
		t.Errorf("first event = %v, want entry_posted seq 1", first)
	}
	last := events[2].(map[string]any)
	if last["type"] != "account_frozen" {
		t.Errorf("last event = %v, want account_frozen", last)
	}

	// Type filter.
	_, body = getJSON(t, srv.URL+"/events?type=account_frozen")
	if events := body["events"].([]any); len(events) != 1 {
		t.Errorf("type filter events = %d, want 1", len(events))
	}
	// Account filter.
	_, body = getJSON(t, srv.URL+"/events?account=cash")
	if events := body["events"].([]any); len(events) != 2 {
		t.Errorf("account filter events = %d, want 2", len(events))
	}
	// Pagination: limit=2 gives a cursor; resuming yields the tail.
	_, body = getJSON(t, srv.URL+"/events?limit=2")
	if events := body["events"].([]any); len(events) != 2 {
		t.Fatalf("page1 events = %d, want 2", len(events))
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("page1 next_cursor empty, want cursor")
	}
	_, body = getJSON(t, srv.URL+"/events?limit=2&cursor="+cursor)
	events = body["events"].([]any)
	if len(events) != 1 || body["next_cursor"] != "" {
		t.Errorf("page2 = %d events next=%v, want 1 event and empty cursor", len(events), body["next_cursor"])
	}
	if events[0].(map[string]any)["seq"] != float64(3) {
		t.Errorf("page2 seq = %v, want 3", events[0].(map[string]any)["seq"])
	}
}

func TestHandleOutboxEventsDisabled(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/events")
	if code != http.StatusNotFound {
		t.Fatalf("GET /events without outbox: status=%d body=%v, want 404", code, body)
	}
	if !strings.Contains(body["error"].(string), "LEDGER_OUTBOX_DIR") {
		t.Errorf("error = %v, want it to name LEDGER_OUTBOX_DIR", body["error"])
	}
}

func TestHandleOutboxEventsBadParams(t *testing.T) {
	srv, _ := seedOutboxServer(t)
	defer srv.Close()

	code, _ := getJSON(t, srv.URL+"/events?cursor=!!!")
	if code != http.StatusBadRequest {
		t.Errorf("bad cursor: status=%d, want 400", code)
	}
	code, _ = getJSON(t, srv.URL+"/events?limit=0")
	if code != http.StatusBadRequest {
		t.Errorf("limit=0: status=%d, want 400", code)
	}
	code, _ = getJSON(t, srv.URL+"/events?limit=1001")
	if code != http.StatusBadRequest {
		t.Errorf("limit=1001: status=%d, want 400", code)
	}
	code, _ = getJSON(t, srv.URL+"/events?since=not-a-time")
	if code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}
}

func TestMetricsOutboxCounters(t *testing.T) {
	srv, _ := seedOutboxServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "ledger_outbox_events_total 1\n") {
		t.Errorf("/metrics missing ledger_outbox_events_total 1:\n%s", text)
	}
	if !strings.Contains(text, "ledger_outbox_dropped_total 0\n") {
		t.Errorf("/metrics missing ledger_outbox_dropped_total 0:\n%s", text)
	}
}
