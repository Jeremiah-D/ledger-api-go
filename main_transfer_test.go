package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestTransferLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund alice through the raw entries endpoint first.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// First transfer: 201 with the receipt; entries carry the transfer ID.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-1","from_account":"alice","to_account":"bob","amount_cents":2500,"idempotency_key":"tk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["transfer_id"] != "tx-1" {
		t.Errorf("transfer_id = %v, want tx-1", body["transfer_id"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly one entry", body["entries"])
	}
	e := entries[0].(map[string]any)
	if e["id"] != "tx-1" || e["debit_account"] != "bob" || e["credit_account"] != "alice" {
		t.Errorf("entry = %v, want id tx-1 debit bob credit alice", e)
	}

	// Replay of the same idempotency key: 200, original receipt.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-1-retry","from_account":"alice","to_account":"bob","amount_cents":2500,"idempotency_key":"tk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay transfer: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}

	// Balances moved exactly once.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(7500) {
		t.Errorf("alice balance = %v, want 7500", body["balance_cents"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/bob/balance")
	if body["balance_cents"] != float64(2500) {
		t.Errorf("bob balance = %v, want 2500", body["balance_cents"])
	}

	// Metrics counted the attempts and the idempotency hit.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_transfers_total"] != 2 {
		t.Errorf("ledger_transfers_total = %d, want 2", m["ledger_transfers_total"])
	}
	if m["ledger_transfer_idempotency_hits_total"] != 1 {
		t.Errorf("ledger_transfer_idempotency_hits_total = %d, want 1", m["ledger_transfer_idempotency_hits_total"])
	}
}

func TestTransferGeneratesIDWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"from_account":"alice","to_account":"bob","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	id, ok := body["transfer_id"].(string)
	if !ok || id == "" {
		t.Fatalf("transfer_id = %v, want a generated non-empty ID", body["transfer_id"])
	}
	entries := body["entries"].([]any)
	if entries[0].(map[string]any)["id"] != id {
		t.Errorf("entry id does not match generated transfer_id %q", id)
	}
}

func TestTransferRejects(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{"same account", `{"from_account":"a","to_account":"a","amount_cents":1}`, http.StatusBadRequest},
		{"zero amount", `{"from_account":"a","to_account":"b","amount_cents":0}`, http.StatusBadRequest},
		{"missing to", `{"from_account":"a","amount_cents":1}`, http.StatusBadRequest},
		{"unknown field", `{"from_account":"a","to_account":"b","amount_cents":1,"bogus":true}`, http.StatusBadRequest},
		{"malformed json", `{"from_account":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := postJSON(t, srv.URL+"/transfers", tc.payload)
			if code != tc.want {
				t.Errorf("status=%d, want %d", code, tc.want)
			}
		})
	}
}

func TestTransferThroughFrozenAccountReturns403(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/accounts/bob/freeze", "")
	if code != http.StatusOK {
		t.Fatalf("freeze: status=%d, want 200", code)
	}
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"from_account":"alice","to_account":"bob","amount_cents":100}`)
	if code != http.StatusForbidden {
		t.Fatalf("transfer through frozen account: status=%d, want 403", code)
	}
	if !strings.Contains(body["error"].(string), "frozen") {
		t.Fatalf("403 body = %v, want a frozen-account error", body)
	}
}

func TestTransferOverdrawReturns422(t *testing.T) {
	l := ledger.New(ledger.WithOverdraftProtection("alice"))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"from_account":"alice","to_account":"bob","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("overdraw transfer: status=%d, want 422 (body=%v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "overdraw") {
		t.Fatalf("422 body = %v, want an overdraft error", body)
	}
}

func TestTransferWithExplicitFee(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-f1","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":25,"fee_account":"fees","idempotency_key":"tf-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer with fee: status=%d, want 201 (body=%v)", code, body)
	}
	if body["fee_cents"] != float64(25) {
		t.Errorf("fee_cents = %v, want 25", body["fee_cents"])
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (principal + fee)", len(entries))
	}
	fee := entries[1].(map[string]any)
	if fee["id"] != "tx-f1/fee" || fee["debit_account"] != "fees" || fee["credit_account"] != "alice" {
		t.Errorf("fee entry = %v, want id tx-f1/fee debit fees credit alice", fee)
	}

	// The payer's total outflow is amount + fee.
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["balance_cents"] != float64(8975) {
		t.Errorf("alice balance = %v, want 8975", body["balance_cents"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/fees/balance")
	if body["balance_cents"] != float64(25) {
		t.Errorf("fees balance = %v, want 25", body["balance_cents"])
	}

	// Replay returns the full receipt; the fee counter is not double-counted.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-f1-retry","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":25,"fee_account":"fees","idempotency_key":"tf-1"}`)
	if code != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("replay: status=%d duplicate=%v, want 200 true", code, body["duplicate"])
	}
	if len(body["entries"].([]any)) != 2 {
		t.Errorf("replay entries = %v, want the full 2-entry receipt", body["entries"])
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_transfer_fee_cents_total"] != 25 {
		t.Errorf("ledger_transfer_fee_cents_total = %d, want 25 (replay not double-counted)", m["ledger_transfer_fee_cents_total"])
	}
}

func TestTransferFeeValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	cases := []struct {
		name    string
		payload string
	}{
		{"negative fee", `{"from_account":"a","to_account":"b","amount_cents":100,"fee_cents":-1,"fee_account":"f"}`},
		{"fee without account", `{"from_account":"a","to_account":"b","amount_cents":100,"fee_cents":5}`},
		{"account without fee", `{"from_account":"a","to_account":"b","amount_cents":100,"fee_account":"f"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postJSON(t, srv.URL+"/transfers", tc.payload)
			if code != http.StatusBadRequest {
				t.Errorf("status=%d, want 400 (body=%v)", code, body)
			}
		})
	}
}

func TestTransferFeePolicyFromEnv(t *testing.T) {
	// The fee policy is deployment-configurable via LEDGER_TRANSFER_FEE.
	// newRouter takes a prebuilt ledger here to exercise the same code
	// path the env var wires up in main().
	l := ledger.New(ledger.WithTransferFeePolicy(1000, "platform-revenue")) // 10%
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":100000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// No explicit fee: the 10% policy applies.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-p1","from_account":"alice","to_account":"bob","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["fee_cents"] != float64(1000) {
		t.Errorf("fee_cents = %v, want 1000 (10%% policy)", body["fee_cents"])
	}

	// skip_fee opts out of the policy for one transfer.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-p2","from_account":"alice","to_account":"bob","amount_cents":10000,"skip_fee":true}`)
	if code != http.StatusCreated {
		t.Fatalf("skip_fee transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["fee_cents"] != float64(0) || len(body["entries"].([]any)) != 1 {
		t.Errorf("skip_fee receipt = %v, want no fee leg", body)
	}
}
