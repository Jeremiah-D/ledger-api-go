package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedHoldServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID:            "fund-card",
		DebitAccount:  "card",
		CreditAccount: "equity",
		AmountCents:   10000,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPHoldFlow(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":6000,"expires_at":%q,"idempotency_key":"k1"}`,
		expiry))
	if code != http.StatusCreated {
		t.Fatalf("POST /holds status = %d, want 201 (body %v)", code, body)
	}
	if body["status"] != "active" || body["id"] != "h1" {
		t.Errorf("hold body = %v, want active hold h1", body)
	}

	// Balance reports the journaled net and the spendable available side
	// by side.
	code, bal := getJSON(t, srv.URL+"/accounts/card/balance")
	if code != http.StatusOK {
		t.Fatalf("GET /balance status = %d", code)
	}
	if bal["balance_cents"] != float64(10000) || bal["available_cents"] != float64(4000) {
		t.Errorf("balance = %v, want balance_cents 10000 / available_cents 4000", bal)
	}

	// Duplicate idempotency key replays the original hold.
	code, dup := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h2","account":"card","amount_cents":6000,"expires_at":%q,"idempotency_key":"k1"}`,
		expiry))
	if code != http.StatusOK || dup["id"] != "h1" {
		t.Errorf("replay = status %d body %v, want 200 with id h1", code, dup)
	}

	// Capture settles 4000 to the merchant; 2000 is released.
	code, receipt := postJSON(t, srv.URL+"/holds/h1/capture",
		`{"capture_id":"c1","to_account":"merchant","amount_cents":4000,"idempotency_key":"ck1"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds/h1/capture status = %d, want 201 (body %v)", code, receipt)
	}
	if receipt["captured_cents"] != float64(4000) || receipt["released_cents"] != float64(2000) {
		t.Errorf("receipt = %v, want captured 4000 / released 2000", receipt)
	}

	code, bal = getJSON(t, srv.URL+"/accounts/card/balance")
	if bal["balance_cents"] != float64(6000) || bal["available_cents"] != float64(6000) {
		t.Errorf("balance after capture = %v, want 6000/6000", bal)
	}

	// A second capture on the consumed hold is rejected.
	code, _ = postJSON(t, srv.URL+"/holds/h1/capture",
		`{"capture_id":"c2","to_account":"merchant","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("second capture status = %d, want 422", code)
	}
}

func TestHTTPHoldRejections(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	// Insufficient available funds -> 422.
	code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"account":"card","amount_cents":10001,"expires_at":%q}`, expiry))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("oversized hold status = %d, want 422", code)
	}

	// Malformed request -> 400.
	code, _ = postJSON(t, srv.URL+"/holds", `{"account":"card"}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty hold status = %d, want 400", code)
	}

	// Capture on an unknown hold -> 404.
	code, _ = postJSON(t, srv.URL+"/holds/nope/capture",
		`{"to_account":"merchant","amount_cents":100}`)
	if code != http.StatusNotFound {
		t.Errorf("capture unknown hold status = %d, want 404", code)
	}

	// Capture exceeding the hold -> 422.
	if code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h9","account":"card","amount_cents":5000,"expires_at":%q}`, expiry)); code != http.StatusCreated {
		t.Fatalf("setup hold status = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/holds/h9/capture",
		`{"to_account":"merchant","amount_cents":5001}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("exceeding capture status = %d, want 422", code)
	}
}

func TestHTTPHoldFrozen(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/accounts/card/freeze", `{}`); code != http.StatusOK {
		t.Fatalf("freeze status = %d", code)
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"account":"card","amount_cents":100,"expires_at":%q}`, expiry))
	if code != http.StatusForbidden {
		t.Errorf("hold on frozen account status = %d, want 403", code)
	}
}

func TestHTTPHoldRelease(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":6000,"expires_at":%q}`, expiry)); code != http.StatusCreated {
		t.Fatalf("setup hold status = %d", code)
	}

	code, body := postJSON(t, srv.URL+"/holds/h1/release", `{}`)
	if code != http.StatusOK || body["status"] != "released" {
		t.Errorf("release = status %d body %v, want 200 released", code, body)
	}
	// Second release is an idempotent no-op.
	if code, body := postJSON(t, srv.URL+"/holds/h1/release", `{}`); code != http.StatusOK || body["status"] != "released" {
		t.Errorf("second release = status %d body %v, want 200 released", code, body)
	}
	if code, _ := postJSON(t, srv.URL+"/holds/nope/release", `{}`); code != http.StatusNotFound {
		t.Errorf("release unknown hold status = %d, want 404", code)
	}

	_, bal := getJSON(t, srv.URL+"/accounts/card/balance")
	if bal["available_cents"] != float64(10000) {
		t.Errorf("available after release = %v, want 10000", bal["available_cents"])
	}
}

func TestHTTPHoldExpireSweep(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	// A hold born with a past expiry is lazily expired; the sweep marks
	// it for observability.
	past := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	if code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":100,"expires_at":%q}`, past)); code != http.StatusCreated {
		t.Fatalf("setup hold status = %d", code)
	}
	code, body := postJSON(t, srv.URL+"/holds/expire", `{}`)
	if code != http.StatusOK || body["expired"] != float64(1) {
		t.Errorf("expire sweep = status %d body %v, want 200 {expired:1}", code, body)
	}
	if code, body := postJSON(t, srv.URL+"/holds/expire", `{}`); code != http.StatusOK || body["expired"] != float64(0) {
		t.Errorf("second sweep = status %d body %v, want 200 {expired:0}", code, body)
	}
}

func TestHTTPHoldMetrics(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":100,"expires_at":%q,"idempotency_key":"mk1"}`, expiry))
	postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h2","account":"card","amount_cents":100,"expires_at":%q,"idempotency_key":"mk1"}`, expiry))
	postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"account":"card","amount_cents":999999,"expires_at":%q}`, expiry))
	postJSON(t, srv.URL+"/holds/h1/capture", `{"to_account":"m","amount_cents":50}`)
	postJSON(t, srv.URL+"/holds/h1/release", `{}`)
	postJSON(t, srv.URL+"/holds/expire", `{}`)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := sb.String()
	for _, want := range []string{
		"ledger_holds_total 3",
		"ledger_hold_idempotency_hits_total 1",
		"ledger_hold_rejections_total 1",
		"ledger_captures_total 1",
		"ledger_releases_total 1",
		"ledger_hold_sweeps_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics missing %q\n%s", want, out)
		}
	}
}

func TestHTTPReconcileHeldTotals(t *testing.T) {
	srv := seedHoldServer(t)
	defer srv.Close()

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, _ := postJSON(t, srv.URL+"/holds", fmt.Sprintf(
		`{"hold_id":"h1","account":"card","amount_cents":2500,"expires_at":%q}`, expiry)); code != http.StatusCreated {
		t.Fatalf("setup hold status = %d", code)
	}
	code, body := postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile status = %d", code)
	}
	rows, ok := body["held_totals"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("held_totals = %v, want one row", body["held_totals"])
	}
	row := rows[0].(map[string]any)
	if row["currency"] != "USD" || row["held_cents"] != float64(2500) || row["active_holds"] != float64(1) {
		t.Errorf("held_totals row = %v, want {USD 2500 1}", row)
	}
}
