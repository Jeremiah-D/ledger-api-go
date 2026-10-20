package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSplitLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund the source account through the raw entries endpoint.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"parent-corp","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding: status=%d, want 201", code)
	}

	// First split: 201 with the receipt; the leg carries the split-ID
	// prefix and the source keeps operating (not frozen).
	code, body := postJSON(t, srv.URL+"/accounts/parent-corp/split",
		`{"split_id":"sp-1","to_account":"child-corp","legs":[{"currency":"USD","amount_cents":4000}],"idempotency_key":"sk-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("split: status=%d, want 201 (body=%v)", code, body)
	}
	if body["split_id"] != "sp-1" || body["duplicate"] != false {
		t.Errorf("receipt = %v, want sp-1 non-duplicate", body)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly one split leg", body["entries"])
	}
	e := entries[0].(map[string]any)
	if e["id"] != "sp-1/USD" || e["debit_account"] != "child-corp" || e["credit_account"] != "parent-corp" {
		t.Errorf("leg entry = %v, want id sp-1/USD debit child-corp credit parent-corp", e)
	}
	if e["memo"] != "split:sp-1" {
		t.Errorf("leg memo = %v, want split:sp-1", e["memo"])
	}

	// Replay of the same idempotency key: 200 with the original receipt.
	code, body = postJSON(t, srv.URL+"/accounts/parent-corp/split",
		`{"split_id":"sp-1-retry","to_account":"child-corp","legs":[{"currency":"USD","amount_cents":4000}],"idempotency_key":"sk-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay split: status=%d, want 200 (body=%v)", code, body)
	}
	if body["duplicate"] != true || body["split_id"] != "sp-1" {
		t.Errorf("replay = %v, want duplicate of sp-1", body)
	}

	// A second split from the same source works: the source was not
	// frozen by the first split.
	code, _ = postJSON(t, srv.URL+"/accounts/parent-corp/split",
		`{"split_id":"sp-2","to_account":"child-corp","legs":[{"currency":"USD","ratio_bps":5000}]}`)
	if code != http.StatusCreated {
		t.Fatalf("second split: status=%d, want 201", code)
	}

	// Balances moved exactly: 4000 fixed + 3000 ratio-of-6000.
	_, body = getJSON(t, srv.URL+"/accounts/child-corp/balance")
	if body["balance_cents"] != float64(7000) {
		t.Errorf("child-corp balance = %v, want 7000", body["balance_cents"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/parent-corp/balance")
	if body["balance_cents"] != float64(3000) {
		t.Errorf("parent-corp balance = %v, want 3000", body["balance_cents"])
	}

	// An oversized split: 400.
	code, _ = postJSON(t, srv.URL+"/accounts/parent-corp/split",
		`{"split_id":"sp-3","to_account":"child-corp","legs":[{"currency":"USD","amount_cents":99999}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("oversized split: status=%d, want 400", code)
	}

	// The reconcile report lists the splits.
	code, body = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile: status=%d, want 200", code)
	}
	splits, ok := body["splits"].([]any)
	if !ok || len(splits) != 2 {
		t.Fatalf("reconcile splits = %v, want 2", body["splits"])
	}
	sp := splits[0].(map[string]any)
	if sp["split_id"] != "sp-1" || sp["from_account"] != "parent-corp" || sp["to_account"] != "child-corp" {
		t.Errorf("split = %v", sp)
	}
}
