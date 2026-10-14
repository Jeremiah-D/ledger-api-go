package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestTransferFeeSplitHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund alice through the raw entries endpoint first.
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":1000000}`); code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// A transfer with a fee split: 201, receipt discloses the
	// per-recipient breakdown (fee 101: 51/50 with dust to the first).
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-http-split","from_account":"alice","to_account":"bob","amount_cents":10000,`+
			`"fee_cents":101,"idempotency_key":"http-split-key",`+
			`"fee_splits":[{"account":"rev-a","bps":5000},{"account":"rev-b","bps":5000}]}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer: status=%d, want 201 (body=%v)", code, body)
	}
	if body["fee_cents"] != float64(101) {
		t.Errorf("fee_cents = %v, want 101", body["fee_cents"])
	}
	splits, ok := body["fee_splits"].([]any)
	if !ok || len(splits) != 2 {
		t.Fatalf("fee_splits = %v, want 2 recipients", body["fee_splits"])
	}
	first := splits[0].(map[string]any)
	if first["account"] != "rev-a" || first["bps"] != float64(5000) || first["fee_cents"] != float64(51) {
		t.Errorf("fee_splits[0] = %v, want account rev-a bps 5000 fee_cents 51", first)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 3 {
		t.Fatalf("entries = %v, want 3 (principal + 2 split legs)", body["entries"])
	}

	// The operator fee-split API discloses the same breakdown.
	code, detail := getJSON(t, srv.URL+"/transfers/tx-http-split/fee-split")
	if code != http.StatusOK {
		t.Fatalf("GET fee-split: status=%d, want 200 (body=%v)", code, detail)
	}
	if detail["transfer_id"] != "tx-http-split" || detail["fee_cents"] != float64(101) {
		t.Errorf("fee-split detail = %v, want transfer tx-http-split fee 101", detail)
	}
	if got := detail["splits"]; len(got.([]any)) != 2 {
		t.Errorf("fee-split splits = %v, want 2", got)
	}

	// Idempotent replay: 200 with the original receipt, same split
	// breakdown, no new legs booked.
	code, body = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-http-split-retry","from_account":"alice","to_account":"bob","amount_cents":10000,`+
			`"fee_cents":101,"idempotency_key":"http-split-key",`+
			`"fee_splits":[{"account":"rev-a","bps":5000},{"account":"rev-b","bps":5000}]}`)
	if code != http.StatusOK {
		t.Fatalf("replay transfer: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}
	rsplits, ok := body["fee_splits"].([]any)
	if !ok || len(rsplits) != 2 || rsplits[0].(map[string]any)["fee_cents"] != float64(51) {
		t.Errorf("replay fee_splits = %v, want the original 51/50 breakdown", body["fee_splits"])
	}

	// Balances moved exactly once: rev-a got 51, rev-b got 50.
	_, abody := getJSON(t, srv.URL+"/accounts/rev-a/balance")
	if abody["balance_cents"] != float64(51) {
		t.Errorf("rev-a balance = %v, want 51", abody["balance_cents"])
	}
	_, bbody := getJSON(t, srv.URL+"/accounts/rev-b/balance")
	if bbody["balance_cents"] != float64(50) {
		t.Errorf("rev-b balance = %v, want 50", bbody["balance_cents"])
	}
}

func TestTransferFeeSplitHTTPValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":1000000}`); code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	cases := []struct {
		name    string
		payload string
	}{
		{"bps sum short", `{"transfer_id":"tx-v1","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":10,"fee_splits":[{"account":"a","bps":9000}]}`},
		{"bps sum over", `{"transfer_id":"tx-v2","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":10,"fee_splits":[{"account":"a","bps":6000},{"account":"b","bps":5000}]}`},
		{"zero account", `{"transfer_id":"tx-v3","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":10,"fee_splits":[{"account":"","bps":10000}]}`},
		{"fee account conflict", `{"transfer_id":"tx-v4","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":10,"fee_account":"fees","fee_splits":[{"account":"a","bps":10000}]}`},
		{"skip fee conflict", `{"transfer_id":"tx-v5","from_account":"alice","to_account":"bob","amount_cents":1000,"skip_fee":true,"fee_splits":[{"account":"a","bps":10000}]}`},
		{"zero fee rejected", `{"transfer_id":"tx-v6","from_account":"alice","to_account":"bob","amount_cents":1000,"skip_fee":true,"fee_splits":[{"account":"a","bps":10000}]}`},
	}
	for _, tc := range cases {
		code, body := postJSON(t, srv.URL+"/transfers", tc.payload)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400 (body=%v)", tc.name, code, body)
		}
	}
}

func TestTransferFeeSplitHTTPNotFound(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":1000000}`); code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	// A plain transfer with a single fee account: no split to disclose.
	if code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"tx-plain-fee","from_account":"alice","to_account":"bob","amount_cents":1000,"fee_cents":25,"fee_account":"fees"}`); code != http.StatusCreated {
		t.Fatalf("plain transfer: status=%d, want 201", code)
	}

	// Unknown transfer: 404.
	code, body := getJSON(t, srv.URL+"/transfers/tx-missing/fee-split")
	if code != http.StatusNotFound {
		t.Errorf("unknown transfer: status=%d, want 404", code)
	}
	// Transfer without a fee split: 404 with a hint.
	code, body = getJSON(t, srv.URL+"/transfers/tx-plain-fee/fee-split")
	if code != http.StatusNotFound {
		t.Fatalf("no-split transfer: status=%d, want 404", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "no fee split") {
		t.Errorf("no-split error = %q, want the 404 hint", msg)
	}
}
