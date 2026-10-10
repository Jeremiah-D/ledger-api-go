package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// reviewHTTPServer returns a test server with a 10000-cent review
// threshold armed and alice funded with 100000 cents.
func reviewHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newRouter(ledger.New(ledger.WithReviewThreshold(10000))))
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		srv.Close()
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	return srv
}

func TestReviewHTTPLifecycle(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	// A transfer above the threshold freezes into pending review: 201
	// with the review receipt.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-big","from_account":"alice","to_account":"bob","amount_cents":30000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["review_status"] != "pending_review" {
		t.Errorf("review_status = %v, want pending_review", body["review_status"])
	}

	// Balances have not moved; available deducts the freeze.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(100000) {
		t.Errorf("alice balance = %v, want 100000", body["balance_cents"])
	}
	if body["available_cents"] != float64(70000) {
		t.Errorf("alice available_cents = %v, want 70000", body["available_cents"])
	}

	// Operator approves: 200 with the settlement receipt.
	code, body = postJSON(t, srv.URL+"/transfers/tx-big/approve", `{"reviewer":"op-alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve: status=%d, want 200 (body=%v)", code, body)
	}
	if body["review_status"] != "approved" {
		t.Errorf("review_status = %v, want approved", body["review_status"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(70000) {
		t.Errorf("alice balance after approve = %v, want 70000", body["balance_cents"])
	}

	// Re-approve is idempotent: 200 with the same receipt.
	code, body = postJSON(t, srv.URL+"/transfers/tx-big/approve", `{"reviewer":"op-bob"}`)
	if code != http.StatusOK {
		t.Fatalf("re-approve: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("re-approve duplicate = %v, want true", body["duplicate"])
	}

	// Metrics: one freeze, one approval (the idempotent re-approval is
	// not counted again).
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_review_pending_total"] != 1 {
		t.Errorf("ledger_review_pending_total = %d, want 1", m["ledger_review_pending_total"])
	}
	if m["ledger_review_approved_total"] != 1 {
		t.Errorf("ledger_review_approved_total = %d, want 1", m["ledger_review_approved_total"])
	}
	if m["ledger_review_rejected_total"] != 0 {
		t.Errorf("ledger_review_rejected_total = %d, want 0", m["ledger_review_rejected_total"])
	}
}

func TestReviewHTTPReject(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-nope","from_account":"alice","to_account":"bob","amount_cents":20000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers/tx-nope/reject",
		`{"reviewer":"op-carol","reason":"sanctions screen"}`)
	if code != http.StatusOK {
		t.Fatalf("reject: status=%d, want 200 (body=%v)", code, body)
	}
	if body["status"] != "rejected" {
		t.Errorf("status = %v, want rejected", body["status"])
	}
	if body["reviewed_by"] != "op-carol" {
		t.Errorf("reviewed_by = %v, want op-carol", body["reviewed_by"])
	}
	// Funds released.
	code, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if code != http.StatusOK {
		t.Fatalf("balance: status=%d", code)
	}
	if body["available_cents"] != float64(100000) {
		t.Errorf("alice available_cents = %v, want 100000", body["available_cents"])
	}
	// Approving a rejected review is a conflict.
	code, _ = postJSON(t, srv.URL+"/transfers/tx-nope/approve", `{"reviewer":"op"}`)
	if code != http.StatusConflict {
		t.Errorf("approve rejected: status=%d, want 409", code)
	}

	code, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_review_rejected_total"] != 1 {
		t.Errorf("ledger_review_rejected_total = %d, want 1", m["ledger_review_rejected_total"])
	}
}

func TestReviewHTTPNotFound(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers/tx-missing/approve", `{"reviewer":"op"}`)
	if code != http.StatusNotFound {
		t.Errorf("approve unknown: status=%d, want 404", code)
	}
	code, _ = postJSON(t, srv.URL+"/transfers/tx-missing/reject", `{"reviewer":"op"}`)
	if code != http.StatusNotFound {
		t.Errorf("reject unknown: status=%d, want 404", code)
	}
}

func TestReviewHTTPExpire(t *testing.T) {
	l := ledger.New(ledger.WithReviewThreshold(10000), ledger.WithReviewExpiry(1))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-old","from_account":"alice","to_account":"bob","amount_cents":20000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}
	// The 1ns expiry lapsed immediately: the manual sweep expires it.
	code, body := postJSON(t, srv.URL+"/transfers/reviews/expire", `{}`)
	if code != http.StatusOK {
		t.Fatalf("expire: status=%d, want 200 (body=%v)", code, body)
	}
	if body["expired"] != float64(1) {
		t.Errorf("expired = %v, want 1", body["expired"])
	}
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_review_rejected_total"] != 1 {
		t.Errorf("ledger_review_rejected_total = %d, want 1", m["ledger_review_rejected_total"])
	}
}

func TestReviewHTTPReplayReturnsReviewReceipt(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-k","from_account":"alice","to_account":"bob","amount_cents":25000,"idempotency_key":"rk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-k-retry","from_account":"alice","to_account":"bob","amount_cents":25000,"idempotency_key":"rk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}
	if body["review_status"] != "pending_review" {
		t.Errorf("replay review_status = %v, want pending_review", body["review_status"])
	}
	// The replay did not freeze twice: only one pending review exists.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_review_pending_total"] != 1 {
		t.Errorf("ledger_review_pending_total = %d, want 1", m["ledger_review_pending_total"])
	}
}
