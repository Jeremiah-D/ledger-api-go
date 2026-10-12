package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestCloseAccountEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":10000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d body=%v", code, body)
	}

	// Close the account.
	code, body = postJSON(t, srv.URL+"/accounts/cash/close", "")
	if code != http.StatusOK {
		t.Fatalf("close: status=%d body=%v", code, body)
	}
	if body["closed"] != true || body["already_closed"] != false {
		t.Fatalf("close body = %v, want closed=true already_closed=false", body)
	}

	// Idempotent re-close.
	code, body = postJSON(t, srv.URL+"/accounts/cash/close", "")
	if code != http.StatusOK || body["already_closed"] != true {
		t.Fatalf("re-close: status=%d body=%v, want already_closed=true", code, body)
	}

	// A post through the closed account is 403.
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100,"currency":"USD"}`)
	if code != http.StatusForbidden {
		t.Fatalf("post through closed: status=%d, want 403", code)
	}
	if !strings.Contains(body["error"].(string), "closed") {
		t.Fatalf("403 body = %v, want a closed-account error", body)
	}

	// A transfer through the closed account is 403 too.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"cash","to_account":"rent","amount_cents":100,"currency":"USD"}`)
	if code != http.StatusForbidden {
		t.Fatalf("transfer through closed: status=%d, want 403", code)
	}

	// Reads keep working and surface the closed flag.
	_, body = getJSON(t, srv.URL+"/accounts/cash/trial-balance")
	if body["closed"] != true {
		t.Fatalf("trial-balance closed = %v, want true", body["closed"])
	}
	_, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if body["balance_cents"] != float64(10000) {
		t.Fatalf("balance on closed = %v, want 10000", body)
	}

	// Cascade closes the subtree: link child under merchant, close
	// merchant with cascade, and the child reports closed too.
	code, body = postJSON(t, srv.URL+"/accounts/child/parent", `{"parent":"merchant"}`)
	if code != http.StatusOK {
		t.Fatalf("set parent: status=%d body=%v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/accounts/merchant/close", `{"cascade":true}`)
	if code != http.StatusOK || body["cascade"] != true {
		t.Fatalf("cascade close: status=%d body=%v", code, body)
	}
	_, body = getJSON(t, srv.URL+"/accounts/child/trial-balance")
	if body["closed"] != true {
		t.Fatalf("child trial-balance closed = %v, want true", body["closed"])
	}
}
