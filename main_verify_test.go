package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// GET /entries/verify on a healthy journal: 200 {"ok":true,"links":N,"head":...}.
func TestHandleVerifyEntriesHealthy(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}
	if body["links"] != float64(3) {
		t.Errorf("links = %v, want 3", body["links"])
	}
	head, _ := body["head"].(string)
	if len(head) != 64 {
		t.Errorf("head = %q, want 64 hex chars", head)
	}
}

// GET /entries/verify on an empty ledger: 200 with 0 links and the genesis
// (zero) head — an empty journal is intact, not broken.
func TestHandleVerifyEntriesEmptyLedger(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["ok"] != true || body["links"] != float64(0) {
		t.Fatalf("empty verify = %v, want ok=true links=0", body)
	}
	if head, _ := body["head"].(string); head != strings.Repeat("0", 64) {
		t.Errorf("empty head = %q, want 64 zeros", head)
	}
}
