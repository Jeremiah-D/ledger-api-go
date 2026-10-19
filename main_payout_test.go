package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedPayoutServer(t *testing.T, opts ...ledger.Option) *httptest.Server {
	t.Helper()
	all := append([]ledger.Option{ledger.WithPayoutClearingAccount("payout-clearing")}, opts...)
	l := ledger.New(all...)
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPPayoutFullFlow(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	// Draft.
	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-http","payee_account":"saver","amount_cents":250000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %v)", code, body)
	}
	if body["status"] != "draft" {
		t.Errorf("status = %v, want draft", body["status"])
	}

	// Approve (no threshold configured: the maker may self-approve).
	code, body = postJSON(t, srv.URL+"/payouts/po-http/approve", `{"approver":"alice"}`)
	if code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("approve = status %d body %v, want 200 approved", code, body)
	}

	// Submit: the payee's balance drops, the clearing account holds the
	// earmark.
	code, body = postJSON(t, srv.URL+"/payouts/po-http/submit", "")
	if code != http.StatusOK || body["status"] != "submitted" {
		t.Fatalf("submit = status %d body %v, want 200 submitted", code, body)
	}
	code, bal := getJSON(t, srv.URL+"/accounts/saver/balance")
	if code != http.StatusOK || bal["balance_cents"] != float64(750_000) {
		t.Errorf("saver balance = %v, want 750000", bal)
	}

	// Settle against a matching bank statement line.
	code, body = postJSON(t, srv.URL+"/payouts/po-http/settle",
		fmt.Sprintf(`{"statement":{"id":"stmt-1","account":"saver","currency":"USD","direction":"out",`+
			`"amount_cents":250000,"posted_at":%q}}`, time.Now().UTC().Format(time.RFC3339)))
	if code != http.StatusOK || body["status"] != "settled" {
		t.Fatalf("settle = status %d body %v, want 200 settled", code, body)
	}
	code, bal = getJSON(t, srv.URL+"/accounts/payout-clearing/balance")
	if code != http.StatusOK || bal["balance_cents"] != float64(0) {
		t.Errorf("clearing balance = %v, want 0", bal)
	}

	// Read back.
	code, body = getJSON(t, srv.URL+"/payouts/po-http")
	if code != http.StatusOK || body["statement_line_id"] != "stmt-1" {
		t.Errorf("get = status %d body %v, want 200 with stmt-1", code, body)
	}
}

func TestHTTPPayoutErrors(t *testing.T) {
	srv := seedPayoutServer(t, ledger.WithReviewThreshold(100_000))
	defer srv.Close()

	// Unknown payout.
	code, _ := getJSON(t, srv.URL+"/payouts/nope")
	if code != http.StatusNotFound {
		t.Errorf("get unknown = %d, want 404", code)
	}
	// Validation error.
	code, _ = postJSON(t, srv.URL+"/payouts", `{"id":"bad","maker":"alice"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad create = %d, want 400", code)
	}
	// Maker-checker violation: at the 100k threshold the maker cannot
	// self-approve.
	code, _ = postJSON(t, srv.URL+"/payouts",
		`{"id":"po-mc","payee_account":"saver","amount_cents":100000,"destination":"x","maker":"alice"}`)
	if code != http.StatusCreated {
		t.Fatalf("create po-mc = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/approve", `{"approver":"alice"}`)
	if code != http.StatusForbidden {
		t.Errorf("maker self-approve = %d, want 403", code)
	}
	// Approve from the wrong status is 409 (still draft here — submit
	// first, then approve again).
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/approve", `{"approver":"bob"}`)
	if code != http.StatusOK {
		t.Fatalf("checker approve = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/approve", `{"approver":"carol"}`)
	if code != http.StatusOK {
		t.Errorf("re-approve = %d, want 200 idempotent", code)
	}
	// Settle with a non-matching statement is 422.
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/submit", "")
	if code != http.StatusOK {
		t.Fatalf("submit = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/settle",
		`{"statement":{"id":"stmt-x","account":"saver","currency":"USD","direction":"out","amount_cents":1}}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("mismatch settle = %d, want 422", code)
	}
	// Cancel from submitted is 409.
	code, _ = postJSON(t, srv.URL+"/payouts/po-mc/cancel", "")
	if code != http.StatusConflict {
		t.Errorf("cancel submitted = %d, want 409", code)
	}
	// Fail with a reason.
	code, body := postJSON(t, srv.URL+"/payouts/po-mc/fail", `{"reason":"bank timeout"}`)
	if code != http.StatusOK || body["status"] != "failed" {
		t.Errorf("fail = status %d body %v, want 200 failed", code, body)
	}
}

func TestHTTPPayoutSubmitWithoutClearing(t *testing.T) {
	// No clearing account configured: submit is fail-closed 500.
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-nc","payee_account":"saver","amount_cents":100,"destination":"x","maker":"alice"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-nc/approve", `{"approver":"bob"}`)
	if code != http.StatusOK {
		t.Fatalf("approve = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-nc/submit", "")
	if code != http.StatusInternalServerError {
		t.Errorf("submit without clearing = %d, want 500", code)
	}
}
