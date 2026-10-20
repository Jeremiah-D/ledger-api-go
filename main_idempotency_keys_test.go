package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)
// LG-152: HTTP surface for the idempotency key registry query.

func postKeyedEntryHTTP(t *testing.T, srvURL, key string) {
	t.Helper()
	code, body := postJSON(t, srvURL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":10000,"currency":"USD","idempotency_key":"`+key+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("post: status=%d body=%v", code, body)
	}
}

func TestIdempotencyKeysEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postKeyedEntryHTTP(t, srv.URL, "op-alpha-1")
	postKeyedEntryHTTP(t, srv.URL, "op-alpha-2")

	// Full listing.
	code, body := getJSON(t, srv.URL+"/idempotency-keys")
	if code != http.StatusOK {
		t.Fatalf("list: status=%d body=%v", code, body)
	}
	if body["count"] != float64(2) {
		t.Fatalf("count = %v, want 2", body["count"])
	}
	keys, ok := body["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys = %v", body["keys"])
	}
	first := keys[0].(map[string]any)
	for _, field := range []string{"namespace", "key", "maps_to"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("key record missing %q: %v", field, first)
		}
	}
	// No payload leaks: the entry amounts/accounts must not appear.
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "10000") || strings.Contains(string(raw), `"cash"`) {
		t.Fatalf("listing leaks business payload: %s", raw)
	}

	// Namespace + prefix + limit filters.
	code, body = getJSON(t, srv.URL+"/idempotency-keys?namespace=entries&prefix=op-alpha-1")
	if code != http.StatusOK || body["count"] != float64(1) {
		t.Fatalf("filtered: status=%d body=%v", code, body)
	}
	code, body = getJSON(t, srv.URL+"/idempotency-keys?limit=1")
	if code != http.StatusOK || body["count"] != float64(1) {
		t.Fatalf("limit=1: status=%d body=%v", code, body)
	}

	// Unknown namespace and bad limit are 400.
	code, _ = getJSON(t, srv.URL+"/idempotency-keys?namespace=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown namespace: status=%d, want 400", code)
	}
	code, _ = getJSON(t, srv.URL+"/idempotency-keys?limit=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("bad limit: status=%d, want 400", code)
	}

	// POST is not allowed (the mux itself answers 405 with a plain-text
	// body for a path+method mismatch, before the handler runs).
	resp, err := http.Post(srv.URL+"/idempotency-keys", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status=%d, want 405", resp.StatusCode)
	}
}
