package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPPayoutExpire(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	// Draft with a past expiry, then approve.
	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-exp-http","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice","expires_at":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %v)", code, body)
	}
	if body["expires_at"] == nil {
		t.Errorf("create response lacks expires_at: %v", body)
	}
	code, body = postJSON(t, srv.URL+"/payouts/po-exp-http/approve", `{"approver":"bob"}`)
	if code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("approve = status %d body %v, want 200 approved", code, body)
	}

	// The sweep expires it and reports the ID.
	code, body = postJSON(t, srv.URL+"/payouts/expire", `{}`)
	if code != http.StatusOK || body["expired"] != float64(1) {
		t.Fatalf("expire = status %d body %v, want 200 expired=1", code, body)
	}
	// A second sweep is a no-op.
	code, body = postJSON(t, srv.URL+"/payouts/expire", `{}`)
	if code != http.StatusOK || body["expired"] != float64(0) {
		t.Fatalf("second expire = status %d body %v, want 200 expired=0", code, body)
	}
	// The expired instruction reads back as expired, not cancelled.
	code, body = getJSON(t, srv.URL+"/payouts/po-exp-http")
	if code != http.StatusOK || body["status"] != "expired" {
		t.Fatalf("get = status %d body %v, want 200 expired", code, body)
	}
	// Submitting an expired instruction is a conflict, not a booking.
	code, _ = postJSON(t, srv.URL+"/payouts/po-exp-http/submit", "")
	if code != http.StatusConflict {
		t.Fatalf("submit expired status = %d, want 409", code)
	}
}
