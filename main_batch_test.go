package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func newBatchTestServer(t *testing.T, l *ledger.Ledger) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newRouter(l))
}

func TestHTTPBatchHappyPath(t *testing.T) {
	srv := newBatchTestServer(t, ledger.New())
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"payroll-1","idempotency_key":"bk-1","entries":[
			{"entry_id":"e1","debit_account":"alice","credit_account":"payroll","amount_cents":500000,"currency":"USD"},
			{"entry_id":"e2","debit_account":"bob","credit_account":"payroll","amount_cents":450000,"currency":"USD"}
		]}`)
	if code != http.StatusCreated {
		t.Fatalf("batch: status=%d, want 201 (body=%v)", code, body)
	}
	if body["batch_id"] != "payroll-1" {
		t.Errorf("batch_id = %v, want payroll-1", body["batch_id"])
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].(map[string]any)["batch_id"] != "payroll-1" {
		t.Errorf("entry batch_id = %v, want payroll-1", entries[0].(map[string]any)["batch_id"])
	}

	// Balance landed.
	code, body = postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"ignored","idempotency_key":"bk-1","entries":[
			{"entry_id":"e9","debit_account":"x","credit_account":"y","amount_cents":1,"currency":"USD"}
		]}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Errorf("replay: status=%d duplicate=%v, want 200/true", code, body["duplicate"])
	}
	if body["batch_id"] != "payroll-1" {
		t.Errorf("replay batch_id = %v, want payroll-1", body["batch_id"])
	}

	// Counters.
	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d", code)
	}
	m := parseExposition(t, text)
	if m["ledger_batch_total"] != 2 {
		t.Errorf("ledger_batch_total = %d, want 2", m["ledger_batch_total"])
	}
	if m["ledger_batch_idempotency_hits_total"] != 1 {
		t.Errorf("ledger_batch_idempotency_hits_total = %d, want 1", m["ledger_batch_idempotency_hits_total"])
	}
}

func TestHTTPBatchGeneratedIDs(t *testing.T) {
	srv := newBatchTestServer(t, ledger.New())
	defer srv.Close()

	// No batch_id and no entry IDs: the server mints them.
	code, body := postJSON(t, srv.URL+"/entries/batch",
		`{"entries":[{"debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("batch: status=%d, want 201 (body=%v)", code, body)
	}
	if body["batch_id"] == "" || body["batch_id"] == nil {
		t.Errorf("batch_id not generated: %v", body)
	}
	entries := body["entries"].([]any)
	if entries[0].(map[string]any)["id"] == "" {
		t.Errorf("entry id not generated: %v", entries[0])
	}
}

func TestHTTPBatchValidation(t *testing.T) {
	srv := newBatchTestServer(t, ledger.New())
	defer srv.Close()

	// One bad leg: 400, whole batch rejected.
	code, _ := postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"bad","entries":[
			{"entry_id":"e1","debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD"},
			{"entry_id":"e2","debit_account":"c","credit_account":"d","amount_cents":0,"currency":"USD"}
		]}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad batch: status=%d, want 400", code)
	}

	// Empty batch: 400.
	code, _ = postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"empty","entries":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty batch: status=%d, want 400", code)
	}

	// Unknown field: strict decoding rejects.
	code, _ = postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"x","entries":[],"bogus":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
}

func TestHTTPBatchFrozenAndOverdraft(t *testing.T) {
	l := ledger.New()
	l.Freeze("frozen-acct")
	srv := newBatchTestServer(t, l)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries/batch",
		`{"batch_id":"f","entries":[
			{"entry_id":"e1","debit_account":"frozen-acct","credit_account":"b","amount_cents":100,"currency":"USD"}
		]}`)
	if code != http.StatusForbidden {
		t.Errorf("frozen batch: status=%d, want 403", code)
	}

	// Overdraft: fund a protected payer, then batch-spend past zero.
	l2 := ledger.New()
	l2.EnableOverdraftProtection("payer")
	srv2 := newBatchTestServer(t, l2)
	defer srv2.Close()
	if code, _ := postJSON(t, srv2.URL+"/entries",
		`{"debit_account":"payer","credit_account":"bank","amount_cents":10000,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("funding: status=%d", code)
	}
	code, _ = postJSON(t, srv2.URL+"/entries/batch",
		`{"batch_id":"od","entries":[
			{"entry_id":"e1","debit_account":"a","credit_account":"payer","amount_cents":6000,"currency":"USD"},
			{"entry_id":"e2","debit_account":"b","credit_account":"payer","amount_cents":5000,"currency":"USD"}
		]}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("overdraft batch: status=%d, want 422", code)
	}

	code, text, _ := getText(t, srv2.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d", code)
	}
	if m := parseExposition(t, text); m["ledger_overdraft_rejections_total"] != 1 {
		t.Errorf("ledger_overdraft_rejections_total = %d, want 1", m["ledger_overdraft_rejections_total"])
	}
}
