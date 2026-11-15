package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestHandleResumeMandate covers POST /mandates/{id}/resume end to end:
// suspend -> resume returns the mandate active, debits flow again,
// and the error mapping holds (active -> 422, cancelled -> 422,
// unknown -> 404).
func TestHandleResumeMandate(t *testing.T) {
	srv, _ := seedMandateServer(t)

	// Resume while active: 422.
	code, _ := postJSON(t, srv.URL+"/mandates/m-http/resume", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("resume active status = %d, want 422", code)
	}
	// Suspend, then resume: 200 with the mandate active again.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/suspend", `{}`)
	if code != http.StatusOK {
		t.Fatalf("suspend status = %d, want 200", code)
	}
	code, body := postJSON(t, srv.URL+"/mandates/m-http/resume", `{}`)
	if code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", code)
	}
	if body["id"] != "m-http" || body["status"] != "active" {
		t.Errorf("resume body = %v, want id m-http status active", body)
	}
	// Debits flow again after the resume.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-1","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Errorf("debit after resume status = %d, want 201", code)
	}
	// Cancel, then resume: 422 — cancellation is terminal.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/cancel", `{}`)
	if code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/resume", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("resume cancelled status = %d, want 422", code)
	}
	// Unknown mandate: 404.
	code, _ = postJSON(t, srv.URL+"/mandates/nope/resume", `{}`)
	if code != http.StatusNotFound {
		t.Errorf("resume unknown status = %d, want 404", code)
	}
}

// TestHandleResumeMandateExpiredWindow covers the resume gate over
// HTTP: a suspended mandate whose validity window lapses before the
// resume is refused with 422.
func TestHandleResumeMandateExpiredWindow(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	now := time.Now().UTC()
	code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"cust","credit_account":"bank","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed fund status = %d, want 201", code)
	}
	// A mandate that lapses one second from now: suspend it while the
	// window is still open, then wait it out.
	code, _ = postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-short","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Second).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("seed mandate status = %d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/mandates/m-short/suspend", `{}`)
	if code != http.StatusOK {
		t.Fatalf("suspend status = %d, want 200", code)
	}
	time.Sleep(1500 * time.Millisecond)
	code, _ = postJSON(t, srv.URL+"/mandates/m-short/resume", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("resume expired-window status = %d, want 422", code)
	}
}
