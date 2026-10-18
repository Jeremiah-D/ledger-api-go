package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSuspendResumeLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Suspend is idempotent and reports the new state.
	code, body := postJSON(t, srv.URL+"/accounts/cash/suspend", "")
	if code != http.StatusOK || body["suspended"] != true {
		t.Fatalf("suspend: status=%d body=%v, want 200 suspended=true", code, body)
	}
	code, body = postJSON(t, srv.URL+"/accounts/cash/suspend", "")
	if code != http.StatusOK || body["already_suspended"] != true {
		t.Fatalf("re-suspend: status=%d body=%v, want 200 already_suspended=true", code, body)
	}

	// A post through the suspended account is rejected with 403 and
	// books nothing.
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusForbidden {
		t.Fatalf("post through suspended account: status=%d, want 403", code)
	}
	if !strings.Contains(body["error"].(string), "suspended") {
		t.Fatalf("403 body = %v, want a suspended-account error", body)
	}

	// Read endpoints keep working on the suspended account.
	_, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if body["balance_cents"] != float64(0) {
		t.Errorf("balance on suspended account = %v, want 0 cents", body)
	}

	// The metrics moved: exactly one new suspension counted (the
	// idempotent replay was not counted), and one rejection.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_account_suspends_total"] != 1 {
		t.Errorf("ledger_account_suspends_total = %d, want 1", m["ledger_account_suspends_total"])
	}
	if m["ledger_suspended_rejections_total"] != 1 {
		t.Errorf("ledger_suspended_rejections_total = %d, want 1", m["ledger_suspended_rejections_total"])
	}

	// Resume restores posting.
	code, body = postJSON(t, srv.URL+"/accounts/cash/resume", "")
	if code != http.StatusOK || body["suspended"] != false || body["resumed"] != true {
		t.Fatalf("resume: status=%d body=%v, want 200 suspended=false resumed=true", code, body)
	}
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("post after resume: status=%d, want 201", code)
	}

	// Resuming a non-suspended account is a no-op.
	code, body = postJSON(t, srv.URL+"/accounts/cash/resume", "")
	if code != http.StatusOK || body["resumed"] != false {
		t.Fatalf("re-resume: status=%d body=%v, want 200 resumed=false", code, body)
	}
}
