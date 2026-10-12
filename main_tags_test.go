package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newTagsServer builds an unseeded server: tag tests seed their own
// entries and assert exact tag-dimension totals.
func newTagsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newRouter(ledger.New()))
}

func TestAccountTagsEndpoints(t *testing.T) {
	srv := newTagsServer(t)
	defer srv.Close()

	// Tag two accounts.
	for acct, tags := range map[string]string{
		"retail-1": `{"tags":{"line":"retail"}}`,
		"retail-2": `{"tags":{"line":"retail","segment":"cross_border"}}`,
	} {
		code, body := putJSON(t, srv.URL+"/accounts/"+acct+"/tags", tags)
		if code != http.StatusOK {
			t.Fatalf("PUT /accounts/%s/tags: status=%d body=%v, want 200", acct, code, body)
		}
		if body["tags"].(map[string]any)["line"] != "retail" {
			t.Fatalf("PUT /accounts/%s/tags: body=%v, want tags.line=retail", acct, body)
		}
	}

	// An invalid tag key is 400.
	code, _ := putJSON(t, srv.URL+"/accounts/retail-1/tags", `{"tags":{"bad/key":"v"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT with bad tag key: status=%d, want 400", code)
	}

	// Seed balances: retail-1 +1000 USD, retail-2 +2000 USD +500 EUR,
	// untagged +9000 USD (excluded from the tag rollup).
	for i, tc := range []struct {
		acct, currency string
		amount         int
	}{
		{"retail-1", "USD", 1000}, {"retail-2", "USD", 2000}, {"retail-2", "EUR", 500}, {"untagged", "USD", 9000},
	} {
		code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"`+tc.acct+
			`","credit_account":"bank","amount_cents":`+itoa(tc.amount)+
			`,"currency":"`+tc.currency+`","idempotency_key":"tags-`+itoa(i)+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("seed post into %s: status=%d, want 201", tc.acct, code)
		}
	}

	// The tag-dimension rollup aggregates across accounts with no
	// hierarchy link between them.
	code, body := getJSON(t, srv.URL+"/rollup?tag=line:retail")
	if code != http.StatusOK {
		t.Fatalf("GET /rollup?tag=line:retail: status=%d, want 200", code)
	}
	if body["tag_key"] != "line" || body["tag_value"] != "retail" {
		t.Fatalf("tag key/value = %v/%v, want line/retail", body["tag_key"], body["tag_value"])
	}
	got := map[string]float64{}
	for _, r := range body["by_currency"].([]any) {
		row := r.(map[string]any)
		got[row["currency"].(string)] = row["balance_cents"].(float64)
	}
	if got["USD"] != 3000 || got["EUR"] != 500 {
		t.Fatalf("by_currency = %v, want USD=3000 EUR=500", got)
	}
	accts, ok := body["accounts"].([]any)
	if !ok || len(accts) != 2 {
		t.Fatalf("accounts = %v, want [retail-1 retail-2]", body["accounts"])
	}

	// A pair matching nothing is an empty table, not an error.
	code, body = getJSON(t, srv.URL+"/rollup?tag=line:nonexistent")
	if code != http.StatusOK {
		t.Fatalf("GET /rollup?tag=line:nonexistent: status=%d, want 200", code)
	}
	if len(body["accounts"].([]any)) != 0 {
		t.Fatalf("accounts = %v, want empty", body["accounts"])
	}

	// Malformed tag parameters are 400.
	for _, q := range []string{"/rollup", "/rollup?tag=line", "/rollup?tag=bad/key:v", "/rollup?tag=k:bad/v"} {
		code, _ := getJSON(t, srv.URL+q)
		if code != http.StatusBadRequest {
			t.Fatalf("GET %s: status=%d, want 400", q, code)
		}
	}

	// The trial balance carries the account's tags.
	code, body = getJSON(t, srv.URL+"/accounts/retail-2/trial-balance")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/retail-2/trial-balance: status=%d, want 200", code)
	}
	if body["tags"].(map[string]any)["segment"] != "cross_border" {
		t.Fatalf("trial balance tags = %v, want segment=cross_border", body["tags"])
	}

	// Clearing tags drops the rollup row.
	code, _ = putJSON(t, srv.URL+"/accounts/retail-1/tags", `{"tags":{}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT clear tags: status=%d, want 200", code)
	}
	code, body = getJSON(t, srv.URL+"/rollup?tag=line:retail")
	if code != http.StatusOK {
		t.Fatalf("GET /rollup after clear: status=%d, want 200", code)
	}
	accts = body["accounts"].([]any)
	if len(accts) != 1 || accts[0] != "retail-2" {
		t.Fatalf("accounts after clear = %v, want [retail-2]", accts)
	}
}
