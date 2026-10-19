package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func postPIIEntry(t *testing.T, srv *httptest.Server, memo string) {
	t.Helper()
	payload := `{"debit_account":"cash","credit_account":"rev","amount_cents":100,"idempotency_key":"pii-` + memo + `","memo":"` + memo + `"}`
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/entries", "", payload)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries = %d: %v", code, body)
	}
}

func TestPIIErasureHTTPFlow(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	postPIIEntry(t, srv, "invoice-42")

	// File the request as alice.
	newReq := func(method, url, actor, payload string) (int, map[string]any) {
		req, err := http.NewRequest(method, url, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", actor)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		var b map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, b
	}
	code, body := newReq(http.MethodPost, srv.URL+"/pii/erase", "alice", `{"account":"cash","fields":["memo"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /pii/erase = %d: %v", code, body)
	}
	reqID := body["request"].(map[string]any)["id"].(string)

	// Self-approval is forbidden (dual control).
	if code, body := newReq(http.MethodPost, srv.URL+"/pii/erase/"+reqID+"/approve", "alice", ""); code != http.StatusForbidden {
		t.Fatalf("self-approve = %d (%v), want 403", code, body)
	}
	// A different reviewer approves.
	if code, body := newReq(http.MethodPost, srv.URL+"/pii/erase/"+reqID+"/approve", "bob", ""); code != http.StatusOK {
		t.Fatalf("approve = %d: %v", code, body)
	}
	// Read path redacts.
	code, body = doKeyed(t, http.MethodGet, srv.URL+"/entries", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /entries = %d", code)
	}
	entries := body["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("no entries returned")
	}
	if got := entries[0].(map[string]any)["memo"]; got != ledger.ErasedMemoTombstone {
		t.Fatalf("memo = %v, want %q", got, ledger.ErasedMemoTombstone)
	}
	// Tombstones are queryable.
	code, body = doKeyed(t, http.MethodGet, srv.URL+"/pii/erasures?account=cash", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /pii/erasures = %d", code)
	}
	if len(body["erasures"].([]any)) != 1 {
		t.Fatalf("erasures = %v", body["erasures"])
	}
}

func TestPIIErasureHTTPValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Non-erasable field -> 400.
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/pii/erase", "", `{"account":"cash","fields":["amount_cents"]}`); code != http.StatusBadRequest {
		t.Fatalf("amount_cents = %d, want 400", code)
	}
	// Unknown request -> 404.
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/pii/erase/nope/approve", "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown approve = %d, want 404", code)
	}
	// Reject flow.
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/pii/erase", "", `{"account":"cash","fields":["holder_name"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("file = %d: %v", code, body)
	}
	reqID := body["request"].(map[string]any)["id"].(string)
	code, _ = doKeyed(t, http.MethodPost, srv.URL+"/pii/erase/"+reqID+"/reject", "", `{"reason":"nope"}`)
	if code != http.StatusOK {
		t.Fatalf("reject = %d, want 200", code)
	}
}

func TestPIIErasureScopedAuth(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")
	// /pii/* needs admin:audit: a read-only key is forbidden.
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"reader","scopes":["read:ledger"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %v", code, body)
	}
	reader := body["secret"].(string)
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/pii/erase", reader, `{"account":"cash","fields":["memo"]}`); code != http.StatusForbidden {
		t.Fatalf("reader POST /pii/erase = %d, want 403", code)
	}
}
