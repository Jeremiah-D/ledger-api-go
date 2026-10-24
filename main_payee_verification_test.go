package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedPayeeServer returns a server whose ledger has a funded "payer"
// and a "payee" with the given holder name registered.
func seedPayeeServer(t *testing.T, holderName string) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "seed-1", DebitAccount: "payer", CreditAccount: "funding",
		AmountCents: 100000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if holderName != "" {
		if err := l.SetAccountHolderName("payee", holderName); err != nil {
			t.Fatalf("SetAccountHolderName: %v", err)
		}
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPTransferPayeeMismatch422(t *testing.T) {
	srv := seedPayeeServer(t, "Bob Jones")
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":100,"expected_payee_name":"Alice Smith"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "payee name") {
		t.Fatalf("error = %v, want a payee-name mismatch", body["error"])
	}

	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	if got := parseExposition(t, mbody)["ledger_payee_mismatch_rejections_total"]; got != 1 {
		t.Errorf("ledger_payee_mismatch_rejections_total = %d, want 1", got)
	}

	// The rejection books nothing.
	_, body = getJSON(t, srv.URL+"/accounts/payee/balance")
	if body["balance_cents"] != float64(0) {
		t.Errorf("payee balance = %v, want 0", body["balance_cents"])
	}
}

func TestHTTPTransferPayeeExactMatch201(t *testing.T) {
	srv := seedPayeeServer(t, "Bob Jones")
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":100,"expected_payee_name":"bob jones"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %v)", code, body)
	}
	if body["payee_match"] != "exact" {
		t.Errorf("payee_match = %v, want exact", body["payee_match"])
	}
}

func TestHTTPTransferPayeeMismatchConfirmed(t *testing.T) {
	srv := seedPayeeServer(t, "Bob Jones")
	defer srv.Close()

	// Secondary confirmation overrides the rejecting policy.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":100,"expected_payee_name":"Alice Smith","payee_name_confirmed":true}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %v)", code, body)
	}
	if body["payee_match"] != "no_match" {
		t.Errorf("payee_match = %v, want no_match", body["payee_match"])
	}
}

func TestHTTPDryRunPayeeMismatch422(t *testing.T) {
	srv := seedPayeeServer(t, "Bob Jones")
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"t1","from_account":"payer","to_account":"payee","amount_cents":100,"expected_payee_name":"Alice Smith"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("dry-run status = %d, want 422 (body %v)", code, body)
	}
	if !strings.Contains(body["error"].(string), "payee name") {
		t.Fatalf("error = %v, want a payee-name mismatch", body["error"])
	}
}
