package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedPurposeServer builds a server with a funded payer for the
// purpose-code endpoints.
func seedPurposeServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-payer", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 50000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPPurposeCodeTransfer(t *testing.T) {
	srv := seedPurposeServer(t)
	defer srv.Close()

	// A transfer with an admitted purpose code: 201, and the purpose
	// rides the principal entry.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":10000,"currency":"USD","purpose_code":"SALA"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers status = %d, want 201 (body %v)", code, body)
	}
	entries, _ := body["entries"].([]interface{})
	if len(entries) == 0 {
		t.Fatalf("transfer receipt has no entries: %v", body)
	}
	principal, _ := entries[0].(map[string]interface{})
	if principal["purpose_code"] != "SALA" {
		t.Errorf("principal entry = %v, want purpose_code SALA", principal)
	}

	// An invalid purpose code: 422, fail-closed.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t2","from_account":"payer","to_account":"payee","amount_cents":100,"currency":"USD","purpose_code":"BOGUS"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bad purpose status = %d, want 422", code)
	}
}

func TestHTTPPurposeCodePost(t *testing.T) {
	srv := seedPurposeServer(t)
	defer srv.Close()

	// POST /entries with an invalid purpose: 422.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD","purpose_code":"nope"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("POST /entries bad purpose = %d, want 422", code)
	}
	// A valid one posts fine.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD","purpose_code":"GDDS"}`)
	if code != http.StatusCreated {
		t.Errorf("POST /entries good purpose = %d, want 201", code)
	}
}

func TestHTTPPurposeCodePayout(t *testing.T) {
	srv := seedPurposeServer(t)
	defer srv.Close()

	// A payout with an admitted purpose: 201, echoed back.
	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"p1","payee_account":"ops","amount_cents":5000,"currency":"USD","destination":"FPS:123","maker":"alice","purpose_code":"SUPP"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /payouts status = %d, want 201 (body %v)", code, body)
	}
	if body["purpose_code"] != "SUPP" {
		t.Errorf("payout body = %v, want purpose_code SUPP", body)
	}

	// An invalid purpose code: 422, fail-closed.
	code, _ = postJSON(t, srv.URL+"/payouts",
		`{"id":"p2","payee_account":"ops","amount_cents":5000,"currency":"USD","destination":"FPS:123","maker":"alice","purpose_code":"BOGUS"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bad purpose payout = %d, want 422", code)
	}
}
