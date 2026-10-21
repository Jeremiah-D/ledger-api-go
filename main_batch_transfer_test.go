package main

import (
	"net/http"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPBatchTransferHappyPath(t *testing.T) {
	srv := newBatchTestServer(t, ledger.New())
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"payroll-1","idempotency_key":"btk-1","transfers":[
			{"transfer_id":"ht1","from_account":"payroll","to_account":"alice","amount_cents":500000,"currency":"USD"},
			{"transfer_id":"ht2","from_account":"payroll","to_account":"bob","amount_cents":450000,"currency":"USD","fee_cents":200,"fee_account":"fees"}
		]}`)
	if code != http.StatusCreated {
		t.Fatalf("batch transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["batch_id"] != "payroll-1" {
		t.Errorf("batch_id = %v, want payroll-1", body["batch_id"])
	}
	receipts := body["receipts"].([]any)
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	r2 := receipts[1].(map[string]any)
	if r2["fee_cents"] != float64(200) {
		t.Errorf("ht2 fee_cents = %v, want 200", r2["fee_cents"])
	}
	entries := r2["entries"].([]any)
	if len(entries) != 2 {
		t.Errorf("ht2 entries = %d, want 2 (principal + fee)", len(entries))
	}

	// Batch-level idempotent replay.
	code, body = postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"ignored","idempotency_key":"btk-1","transfers":[
			{"transfer_id":"other","from_account":"x","to_account":"y","amount_cents":1,"currency":"USD"}
		]}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("replay: status=%d duplicate=%v, want 200/true (body=%v)", code, body["duplicate"], body)
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
	if m["ledger_transfer_batch_total"] != 2 {
		t.Errorf("ledger_transfer_batch_total = %d, want 2", m["ledger_transfer_batch_total"])
	}
	if m["ledger_transfer_batch_idempotency_hits_total"] != 1 {
		t.Errorf("ledger_transfer_batch_idempotency_hits_total = %d, want 1", m["ledger_transfer_batch_idempotency_hits_total"])
	}
}

func TestHTTPBatchTransferAtomic(t *testing.T) {
	l := ledger.New()
	l.EnableOverdraftProtection("payroll")
	srv := newBatchTestServer(t, l)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"atomic-1","transfers":[
			{"transfer_id":"at1","from_account":"payroll","to_account":"alice","amount_cents":100000,"currency":"USD"},
			{"transfer_id":"at2","from_account":"payroll","to_account":"bob","amount_cents":900000,"currency":"USD"}
		]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("atomic: status=%d, want 422 (body=%v)", code, body)
	}
	// Nothing landed: the first transfer's payee has no balance.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(0) {
		t.Errorf("alice balance = %v after rejected batch, want 0", body["balance_cents"])
	}
}

func TestHTTPBatchTransferValidation(t *testing.T) {
	srv := newBatchTestServer(t, ledger.New())
	defer srv.Close()

	// Empty transfers.
	code, _ := postJSON(t, srv.URL+"/transfers/batch", `{"batch_id":"v1","transfers":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty batch: status=%d, want 400", code)
	}
	// Duplicate transfer IDs.
	code, _ = postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"v2","transfers":[
			{"transfer_id":"dup","from_account":"a","to_account":"b","amount_cents":100,"currency":"USD"},
			{"transfer_id":"dup","from_account":"a","to_account":"c","amount_cents":100,"currency":"USD"}
		]}`)
	if code != http.StatusBadRequest {
		t.Errorf("duplicate IDs: status=%d, want 400", code)
	}
	// One bad transfer poisons the batch.
	code, _ = postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"v3","transfers":[
			{"transfer_id":"ok1","from_account":"a","to_account":"b","amount_cents":100,"currency":"USD"},
			{"transfer_id":"bad1","from_account":"a","to_account":"b","amount_cents":-5,"currency":"USD"}
		]}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad transfer: status=%d, want 400", code)
	}
}

func TestHTTPBatchTransferFrozen(t *testing.T) {
	l := ledger.New()
	l.Freeze("payroll")
	srv := newBatchTestServer(t, l)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/transfers/batch",
		`{"batch_id":"fr1","transfers":[
			{"transfer_id":"frt1","from_account":"payroll","to_account":"alice","amount_cents":100,"currency":"USD"}
		]}`)
	if code != http.StatusForbidden {
		t.Errorf("frozen payer: status=%d, want 403", code)
	}
}
