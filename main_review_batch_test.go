package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// reviewBatchServer funds alice and freezes two threshold-crossing
// transfers into pending review, returning the server and the review
// IDs.
func reviewBatchServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := reviewHTTPServer(t)
	for _, id := range []string{"tx-b1", "tx-b2"} {
		code, body := postJSON(t, srv.URL+"/transfers",
			`{"transfer_id":"`+id+`","from_account":"alice","to_account":"bob","amount_cents":30000}`)
		if code != http.StatusCreated {
			srv.Close()
			t.Fatalf("transfer %s: status=%d, want 201 (body=%v)", id, code, body)
		}
		if body["review_status"] != "pending_review" {
			srv.Close()
			t.Fatalf("transfer %s review_status = %v, want pending_review", id, body["review_status"])
		}
	}
	return srv
}

func TestReviewBatchApproveHTTPSuccess(t *testing.T) {
	srv := reviewBatchServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/reviews/approve",
		`{"reviewer":"op-alice","review_ids":["tx-b1","tx-b2"]}`)
	if code != http.StatusOK {
		t.Fatalf("batch approve: status=%d, want 200 (body=%v)", code, body)
	}
	if body["reviewed_by"] != "op-alice" {
		t.Errorf("reviewed_by = %v, want op-alice", body["reviewed_by"])
	}
	if body["settled"] != float64(2) {
		t.Errorf("settled = %v, want 2", body["settled"])
	}
	results, ok := body["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %v, want 2 items", body["results"])
	}
	for i, id := range []string{"tx-b1", "tx-b2"} {
		r := results[i].(map[string]any)
		if r["transfer_id"] != id {
			t.Errorf("results[%d].transfer_id = %v, want %s", i, r["transfer_id"], id)
		}
		if r["outcome"] != "settled" {
			t.Errorf("results[%d].outcome = %v, want settled", i, r["outcome"])
		}
		rcpt := r["receipt"].(map[string]any)
		if rcpt["review_status"] != "approved" {
			t.Errorf("results[%d] receipt review_status = %v, want approved", i, rcpt["review_status"])
		}
	}

	// Metrics: one batch request, two approvals. alice funded 100000,
	// froze 60000; nothing settled yet through the single endpoint.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(40000) {
		t.Errorf("alice balance after batch = %v, want 40000", body["balance_cents"])
	}
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_review_batch_requests_total"] != 1 {
		t.Errorf("ledger_review_batch_requests_total = %d, want 1", m["ledger_review_batch_requests_total"])
	}
	if m["ledger_review_approved_total"] != 2 {
		t.Errorf("ledger_review_approved_total = %d, want 2", m["ledger_review_approved_total"])
	}
}

func TestReviewBatchApproveHTTPFailureIsolation(t *testing.T) {
	srv := reviewBatchServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/reviews/approve",
		`{"reviewer":"op-alice","review_ids":["ghost","tx-b1"]}`)
	if code != http.StatusOK {
		t.Fatalf("batch approve: status=%d, want 200 (body=%v)", code, body)
	}
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	r0 := results[0].(map[string]any)
	if r0["outcome"] != "failed" {
		t.Errorf("results[0].outcome = %v, want failed", r0["outcome"])
	}
	if r0["error"] == nil || r0["error"] == "" {
		t.Error("failed item has no error text")
	}
	r1 := results[1].(map[string]any)
	if r1["outcome"] != "settled" {
		t.Errorf("results[1].outcome = %v, want settled", r1["outcome"])
	}
	if body["failed"] != float64(1) || body["settled"] != float64(1) {
		t.Errorf("counts = %v, want failed=1 settled=1", body)
	}

	// tx-b2 is still pending: the failure did not stop the batch, and
	// the pending item stays reviewable through the single endpoint.
	code, body = postJSON(t, srv.URL+"/transfers/tx-b2/approve", `{"reviewer":"op-bob"}`)
	if code != http.StatusOK {
		t.Fatalf("single approve tx-b2: status=%d, want 200", code)
	}
	if body["review_status"] != "approved" {
		t.Errorf("tx-b2 review_status = %v, want approved", body["review_status"])
	}
}

func TestReviewBatchApproveHTTPReplay(t *testing.T) {
	srv := reviewBatchServer(t)
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/transfers/reviews/approve",
		`{"reviewer":"op-alice","review_ids":["tx-b1","tx-b2"]}`); code != http.StatusOK {
		t.Fatalf("first batch: status=%d, want 200", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers/reviews/approve",
		`{"reviewer":"op-bob","review_ids":["tx-b1","tx-b2"]}`)
	if code != http.StatusOK {
		t.Fatalf("second batch: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicates"] != float64(2) {
		t.Errorf("duplicates = %v, want 2", body["duplicates"])
	}
	results := body["results"].([]any)
	for i, r := range results {
		item := r.(map[string]any)
		if item["outcome"] != "duplicate" {
			t.Errorf("results[%d].outcome = %v, want duplicate", i, item["outcome"])
		}
		rcpt := item["receipt"].(map[string]any)
		if rcpt["duplicate"] != true {
			t.Errorf("results[%d] receipt duplicate = %v, want true", i, rcpt["duplicate"])
		}
	}

	// The replay settled nothing new: approvals counted once (the
	// single-review handler is not exercised here), batch requests
	// twice.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_review_batch_requests_total"] != 2 {
		t.Errorf("ledger_review_batch_requests_total = %d, want 2", m["ledger_review_batch_requests_total"])
	}
	if m["ledger_review_approved_total"] != 2 {
		t.Errorf("ledger_review_approved_total = %d, want 2 (replays not re-counted)", m["ledger_review_approved_total"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(40000) {
		t.Errorf("alice balance = %v, want 40000 (no double-settle)", body["balance_cents"])
	}
}

func TestReviewBatchApproveHTTPEmpty(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/reviews/approve",
		`{"reviewer":"op-alice","review_ids":[]}`)
	if code != http.StatusOK {
		t.Fatalf("empty batch: status=%d, want 200 (body=%v)", code, body)
	}
	if body["settled"] != float64(0) || body["failed"] != float64(0) {
		t.Errorf("empty batch counts = %v, want zeros", body)
	}
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	if m := parseExposition(t, mbody); m["ledger_review_batch_requests_total"] != 1 {
		t.Errorf("ledger_review_batch_requests_total = %d, want 1", m["ledger_review_batch_requests_total"])
	}
}

func TestReviewBatchApproveHTTPMethodNotAllowed(t *testing.T) {
	srv := reviewHTTPServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/transfers/reviews/approve")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}
