package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestHTTPPartialCaptureFlow(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":1000,"expires_at":%q}`, expiry))
	if code != http.StatusCreated {
		t.Fatalf("POST /holds status = %d, want 201", code)
	}

	// First partial capture: 201 with the deterministic entry ID.
	code, body := postJSON(t, srv.URL+"/holds/h1/capture/partial",
		`{"capture_seq":1,"to_account":"merchant","amount_cents":300}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds/h1/capture/partial status = %d, want 201 (body %v)", code, body)
	}
	if body["capture_id"] != "h1/capture/1" {
		t.Errorf("capture_id = %v, want h1/capture/1", body["capture_id"])
	}
	if body["captured_cents"] != float64(300) || body["remaining_cents"] != float64(700) {
		t.Errorf("receipt = %v, want captured 300 / remaining 700", body)
	}
	if body["duplicate"] == true {
		t.Errorf("first partial: duplicate=true, want false")
	}

	// Replay of the same (holdID, captureSeq): 200 with the original.
	code, dup := postJSON(t, srv.URL+"/holds/h1/capture/partial",
		`{"capture_seq":1,"to_account":"merchant","amount_cents":999}`)
	if code != http.StatusOK || dup["capture_id"] != "h1/capture/1" || dup["duplicate"] != true {
		t.Errorf("replay = status %d body %v, want 200 duplicate receipt", code, dup)
	}

	// Available reflects the still-reserved remainder: 9700 balance
	// minus 700 reserved.
	code, bal := getJSON(t, srv.URL+"/accounts/card/balance")
	if code != http.StatusOK {
		t.Fatalf("GET /balance status = %d", code)
	}
	if bal["available_cents"] != float64(9000) {
		t.Errorf("available_cents = %v, want 9000", bal["available_cents"])
	}

	// Overshooting the remaining cents: 422.
	code, _ = postJSON(t, srv.URL+"/holds/h1/capture/partial",
		`{"capture_seq":2,"to_account":"merchant","amount_cents":701}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("overshoot status = %d, want 422", code)
	}

	// Settling the remainder consumes the hold.
	code, body = postJSON(t, srv.URL+"/holds/h1/capture/partial",
		`{"capture_seq":2,"to_account":"merchant","amount_cents":700}`)
	if code != http.StatusCreated {
		t.Fatalf("final partial status = %d, want 201", code)
	}
	if body["remaining_cents"] != float64(0) {
		t.Errorf("final receipt remaining = %v, want 0 (hold consumed)", body["remaining_cents"])
	}

	// Unknown hold: 404.
	code, _ = postJSON(t, srv.URL+"/holds/nope/capture/partial",
		`{"capture_seq":1,"to_account":"merchant","amount_cents":10}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown hold status = %d, want 404", code)
	}

	// Bad request: missing capture_seq.
	code, _ = postJSON(t, srv.URL+"/holds/h1/capture/partial",
		`{"to_account":"merchant","amount_cents":10}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing seq status = %d, want 400", code)
	}

	// The /metrics exposition carries the new counter.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_partial_captures_total"] < 4 {
		t.Errorf("ledger_partial_captures_total = %d, want >= 4", m["ledger_partial_captures_total"])
	}
}

func TestHTTPPartialCaptureFrozen(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h2","account":"card","amount_cents":1000,"expires_at":%q}`, expiry))
	if code != http.StatusCreated {
		t.Fatalf("POST /holds status = %d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/accounts/card/freeze", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /accounts/card/freeze status = %d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/holds/h2/capture/partial",
		`{"capture_seq":1,"to_account":"merchant","amount_cents":100}`)
	if code != http.StatusForbidden {
		t.Errorf("frozen partial status = %d, want 403", code)
	}
}
