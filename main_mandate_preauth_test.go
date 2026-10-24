package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedPreAuthServer is the mandate HTTP fixture plus a funded payer:
// mandate m-http (cust -> merchant, 5000 USD cap, per-day).
func seedPreAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := seedMandateServer(t)
	return srv
}

// createMandateHTTP installs one more mandate on the test server: the
// per-day frequency gate allows exactly one live reservation per
// mandate per window, so the HTTP cases each get their own mandate.
func createMandateHTTP(t *testing.T, srv *httptest.Server, id string) {
	t.Helper()
	now := time.Now().UTC()
	code, body := postJSON(t, srv.URL+"/mandates", fmt.Sprintf(
		`{"id":%q,"payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		id, now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("create mandate %s status = %d, want 201 (body %v)", id, code, body)
	}
}

func authorizePreAuthHTTP(t *testing.T, srv *httptest.Server, mandateID, preauthID string, amount int64) map[string]any {
	t.Helper()
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/mandates/"+mandateID+"/preauth",
		fmt.Sprintf(`{"id":%q,"amount_cents":%d,"currency":"USD","expires_at":%q}`, preauthID, amount, expires))
	if code != http.StatusCreated {
		t.Fatalf("authorize status = %d, want 201 (body %v)", code, body)
	}
	return body
}

func TestHandleAuthorizePreAuth(t *testing.T) {
	srv := seedPreAuthServer(t)

	body := authorizePreAuthHTTP(t, srv, "m-http", "pa-1", 2000)
	if body["id"] != "pa-1" || body["mandate_id"] != "m-http" || body["status"] != "active" {
		t.Errorf("body = %v", body)
	}
	if body["amount_cents"] != float64(2000) {
		t.Errorf("amount = %v, want 2000", body["amount_cents"])
	}

	// Idempotent replay: same key returns 200 with duplicate: true.
	createMandateHTTP(t, srv, "m-key")
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/mandates/m-key/preauth",
		fmt.Sprintf(`{"id":"pa-other","amount_cents":999,"currency":"USD","expires_at":%q,"idempotency_key":"http-auth-key"}`, expires))
	if code != http.StatusCreated {
		t.Fatalf("first keyed authorize status = %d, want 201", code)
	}
	code, body = postJSON(t, srv.URL+"/mandates/m-key/preauth",
		fmt.Sprintf(`{"id":"pa-other2","amount_cents":999,"currency":"USD","expires_at":%q,"idempotency_key":"http-auth-key"}`, expires))
	if code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", code)
	}
	if body["duplicate"] != true {
		t.Errorf("replay body = %v, want duplicate: true", body)
	}

	// Over the per-debit cap -> 422.
	createMandateHTTP(t, srv, "m-cap")
	code, _ = postJSON(t, srv.URL+"/mandates/m-cap/preauth",
		fmt.Sprintf(`{"id":"pa-cap","amount_cents":6000,"currency":"USD","expires_at":%q}`, expires))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("over-cap status = %d, want 422", code)
	}
	// Unknown mandate -> 404.
	code, _ = postJSON(t, srv.URL+"/mandates/nope/preauth",
		fmt.Sprintf(`{"id":"pa-x","amount_cents":100,"currency":"USD","expires_at":%q}`, expires))
	if code != http.StatusNotFound {
		t.Errorf("unknown mandate status = %d, want 404", code)
	}
	// Field error -> 400.
	code, _ = postJSON(t, srv.URL+"/mandates/m-cap/preauth",
		fmt.Sprintf(`{"id":"","amount_cents":100,"currency":"USD","expires_at":%q}`, expires))
	if code != http.StatusBadRequest {
		t.Errorf("empty id status = %d, want 400", code)
	}
	// Second authorization in the same per-day window -> 422: the
	// reservation occupies the window (m-http already holds pa-1).
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/preauth",
		fmt.Sprintf(`{"id":"pa-window","amount_cents":100,"currency":"USD","expires_at":%q}`, expires))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("windowed authorize status = %d, want 422", code)
	}
}

func TestHandlePreAuthLifecycleHTTP(t *testing.T) {
	srv := seedPreAuthServer(t)

	authorizePreAuthHTTP(t, srv, "m-http", "pa-1", 2000)

	// GET the pre-authorization.
	code, body := getJSON(t, srv.URL+"/mandates/m-http/preauth/pa-1")
	if code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", code)
	}
	if body["id"] != "pa-1" || body["status"] != "active" {
		t.Errorf("GET body = %v", body)
	}
	// GET under a different mandate -> 404.
	code, _ = getJSON(t, srv.URL+"/mandates/m-http2/preauth/pa-1")
	if code != http.StatusNotFound {
		t.Errorf("cross-mandate GET status = %d, want 404", code)
	}
	// List the mandate's pre-authorizations.
	code, body = getJSON(t, srv.URL+"/mandates/m-http/preauth")
	if code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", code)
	}
	list, ok := body["preauths"].([]any)
	if !ok || len(list) != 1 {
		t.Errorf("list = %v, want one pre-authorization", body)
	}
	// Unknown mandate on list -> 404.
	code, _ = getJSON(t, srv.URL+"/mandates/nope/preauth")
	if code != http.StatusNotFound {
		t.Errorf("unknown mandate list status = %d, want 404", code)
	}

	// Capture: 201 with the receipt.
	code, body = postJSON(t, srv.URL+"/mandates/m-http/preauth/pa-1/capture", `{"id":"cap-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("capture status = %d, want 201 (body %v)", code, body)
	}
	entry, ok := body["entry"].(map[string]any)
	if !ok || entry["id"] != "cap-1" {
		t.Errorf("capture receipt = %v, want entry cap-1", body)
	}
	if entry["memo"] != "preauth_of:pa-1 mandate_of:m-http" {
		t.Errorf("memo = %v, want preauth_of:pa-1 mandate_of:m-http", entry["memo"])
	}
	// Capture idempotency on a second mandate: a replayed capture key
	// -> 200 duplicate. (m-http's window is occupied by cap-1's debit.)
	createMandateHTTP(t, srv, "m-capkey")
	authorizePreAuthHTTP(t, srv, "m-capkey", "pa-k", 700)
	code, body = postJSON(t, srv.URL+"/mandates/m-capkey/preauth/pa-k/capture",
		`{"id":"cap-k1","idempotency_key":"http-cap-key"}`)
	if code != http.StatusCreated {
		t.Fatalf("first keyed capture status = %d, want 201", code)
	}
	code, body = postJSON(t, srv.URL+"/mandates/m-capkey/preauth/pa-k/capture",
		`{"id":"cap-k2","idempotency_key":"http-cap-key"}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Errorf("capture replay status = %d body = %v, want 200 duplicate", code, body)
	}
	// Double capture -> 422.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/preauth/pa-1/capture", `{"id":"cap-2"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("double capture status = %d, want 422", code)
	}

	// Release path on a fresh authorization (third mandate — both
	// other windows are occupied).
	createMandateHTTP(t, srv, "m-rel")
	authorizePreAuthHTTP(t, srv, "m-rel", "pa-9", 500)
	code, body = postJSON(t, srv.URL+"/mandates/m-rel/preauth/pa-9/release", `{}`)
	if code != http.StatusOK {
		t.Fatalf("release status = %d, want 200", code)
	}
	if body["status"] != "released" {
		t.Errorf("release body = %v, want released", body)
	}
	// Release under a different mandate -> 404.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/preauth/pa-9/release", `{}`)
	if code != http.StatusNotFound {
		t.Errorf("cross-mandate release status = %d, want 404", code)
	}
	// Capture after release -> 422.
	code, _ = postJSON(t, srv.URL+"/mandates/m-rel/preauth/pa-9/capture", `{"id":"cap-9"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("capture after release status = %d, want 422", code)
	}

	// The reconcile report discloses the pre-authorizations table.
	code, body = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile status = %d, want 200", code)
	}
	preauths, ok := body["preauths"].([]any)
	if !ok || len(preauths) != 3 {
		t.Errorf("reconcile preauths = %v, want 3 rows", body["preauths"])
	}

	// Metrics expose the pre-authorization counters: 3 authorized
	// (pa-1, pa-k, pa-9), 2 captured (pa-1, pa-k), 1 released (pa-9) —
	// replays and rejections never move the counters.
	code, metricsBody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, metricsBody)
	if m["ledger_preauth_authorized_total"] != 3 {
		t.Errorf("ledger_preauth_authorized_total = %d, want 3", m["ledger_preauth_authorized_total"])
	}
	if m["ledger_preauth_captured_total"] != 2 {
		t.Errorf("ledger_preauth_captured_total = %d, want 2", m["ledger_preauth_captured_total"])
	}
	if m["ledger_preauth_released_total"] != 1 {
		t.Errorf("ledger_preauth_released_total = %d, want 1", m["ledger_preauth_released_total"])
	}
}

func TestHandleExpirePreAuthsHTTP(t *testing.T) {
	srv := seedPreAuthServer(t)
	authorizePreAuthHTTP(t, srv, "m-http", "pa-1", 2000)

	// The sweep marks nothing while the reservation is live.
	code, body := postJSON(t, srv.URL+"/mandates/preauth/expire", `{}`)
	if code != http.StatusOK {
		t.Fatalf("sweep status = %d, want 200", code)
	}
	if body["expired"] != float64(0) {
		t.Errorf("sweep expired = %v, want 0", body["expired"])
	}
	// GET still reads active.
	code, body = getJSON(t, srv.URL+"/mandates/m-http/preauth/pa-1")
	if code != http.StatusOK || body["status"] != "active" {
		t.Errorf("GET = %d %v, want 200 active", code, body)
	}
}
