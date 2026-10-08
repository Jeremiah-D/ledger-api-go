package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newHierarchyServer builds an unseeded server: hierarchy tests post
// their own entries and assert exact subtree totals.
func newHierarchyServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newRouter(ledger.New()))
}

func TestSetParentAndRollupEndpoints(t *testing.T) {
	srv := newHierarchyServer(t)
	defer srv.Close()

	// Link two sub-merchants under a merchant account.
	for child, parent := range map[string]string{"sub-1": "merchant", "sub-2": "merchant"} {
		code, body := postJSON(t, srv.URL+"/accounts/"+child+"/parent",
			`{"parent":"`+parent+`"}`)
		if code != http.StatusOK {
			t.Fatalf("POST /accounts/%s/parent: status=%d body=%v, want 200", child, code, body)
		}
		if body["parent"] != parent {
			t.Fatalf("parent link = %v, want %q", body, parent)
		}
	}

	// Post into the subtree: merchant +1000, sub-1 +200, sub-2 +300.
	for i, tc := range []struct{ acct, currency string; amount int }{
		{"merchant", "USD", 1000}, {"sub-1", "USD", 200}, {"sub-2", "EUR", 300},
	} {
		code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"`+tc.acct+
			`","credit_account":"bank","amount_cents":`+itoa(tc.amount)+
			`,"currency":"`+tc.currency+`","idempotency_key":"rollup-`+itoa(i)+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("seed post into %s: status=%d, want 201", tc.acct, code)
		}
	}

	code, body := getJSON(t, srv.URL+"/accounts/merchant/rollup")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/merchant/rollup: status=%d, want 200", code)
	}
	if body["descendant_count"] != float64(2) {
		t.Errorf("descendant_count = %v, want 2", body["descendant_count"])
	}
	rows, ok := body["by_currency"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("by_currency = %v, want 2 currency rows", body["by_currency"])
	}
	// Sorted by currency code; per-currency aggregation.
	got := map[string]float64{}
	for _, r := range rows {
		m := r.(map[string]any)
		got[m["currency"].(string)] = m["balance_cents"].(float64)
	}
	if got["EUR"] != 300 || got["USD"] != 1200 {
		t.Errorf("by_currency = %v, want EUR=300 USD=1200", got)
	}

	// Clearing the parent shrinks the merchant's subtree.
	code, _ = postJSON(t, srv.URL+"/accounts/sub-2/parent", `{"parent":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear parent: status=%d, want 200", code)
	}
	_, body = getJSON(t, srv.URL+"/accounts/merchant/rollup")
	if body["descendant_count"] != float64(1) {
		t.Errorf("after clear: descendant_count = %v, want 1", body["descendant_count"])
	}
}

func TestSetParentEndpointRejectsCycleAndSelf(t *testing.T) {
	srv := newHierarchyServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/accounts/a/parent", `{"parent":"b"}`)
	if code != http.StatusOK {
		t.Fatalf("link a -> b: status=%d, want 200", code)
	}
	// Closing the loop is a 422 (semantically unprocessable).
	code, body := postJSON(t, srv.URL+"/accounts/b/parent", `{"parent":"a"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("cycle link: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "cycle") {
		t.Fatalf("422 body = %v, want a cycle error", body)
	}
	// Self-parenting is a 400 (malformed request).
	code, _ = postJSON(t, srv.URL+"/accounts/c/parent", `{"parent":"c"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("self-parent: status=%d, want 400", code)
	}
	// Malformed JSON is a 400.
	code, _ = postJSON(t, srv.URL+"/accounts/c/parent", `{"parent":`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad JSON: status=%d, want 400", code)
	}
}

func TestRollupEndpointOnUnknownAccount(t *testing.T) {
	srv := newHierarchyServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/ghost/rollup")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/ghost/rollup: status=%d, want 200", code)
	}
	if body["account"] != "ghost" || body["descendant_count"] != float64(0) {
		t.Fatalf("rollup = %v, want account=ghost with no descendants", body)
	}
	if rows, _ := body["by_currency"].([]any); len(rows) != 0 {
		t.Fatalf("by_currency = %v, want empty", rows)
	}
}

func itoa(n int) string {
	// tiny local int->string to keep the test table readable without
	// pulling strconv into every call site.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
