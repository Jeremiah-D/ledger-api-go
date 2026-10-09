package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestPeriodCloseReopenEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Malformed period IDs fail 400.
	code, _ := postJSON(t, srv.URL+"/periods/not-a-month/close", "")
	if code != http.StatusBadRequest {
		t.Fatalf("close malformed period: status=%d, want 400", code)
	}

	// Close the current month: new postings dated now are rejected 422.
	current := time.Now().UTC().Format("2006-01")
	code, body := postJSON(t, srv.URL+"/periods/"+current+"/close", "")
	if code != http.StatusOK || body["closed"] != true || body["period"] != current {
		t.Fatalf("close: status=%d body=%v, want 200 closed=true", code, body)
	}
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("post into closed period: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "closed") {
		t.Fatalf("422 body = %v, want a closed-period error", body)
	}

	// Reopen: posting works again.
	code, body = postJSON(t, srv.URL+"/periods/"+current+"/reopen", "")
	if code != http.StatusOK || body["closed"] != false {
		t.Fatalf("reopen: status=%d body=%v, want 200 closed=false", code, body)
	}
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("post after reopen: status=%d, want 201", code)
	}

	// The reconciliation report lists the period lock history: one past
	// month closed during the test.
	if code, _ := postJSON(t, srv.URL+"/periods/2025-05/close", ""); code != http.StatusOK {
		t.Fatalf("close 2025-05: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/reconcile", "")
	if code != http.StatusOK {
		t.Fatalf("reconcile: status=%d, want 200", code)
	}
}
