package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestPartialFreezeEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Funding: post through /entries so the account has a balance.
	// (The entries route is exercised by the freeze tests; here it
	// only sets up the 100000-cent balance.)
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"test-funding","amount_cents":100000}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("funding post: status=%d", code)
	}

	// Freeze 30000: remaining available is 70000.
	code, body := postJSON(t, srv.URL+"/accounts/alice/partial-freeze",
		`{"amount_cents":30000}`)
	if code != http.StatusOK || body["frozen_cents"] != float64(30000) || body["available_cents"] != float64(70000) {
		t.Fatalf("partial-freeze: status=%d body=%v, want frozen 30000 available 70000", code, body)
	}
	// Over-available freezes are 422 and freeze nothing new.
	code, _ = postJSON(t, srv.URL+"/accounts/alice/partial-freeze",
		`{"amount_cents":70001}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("over-available partial-freeze: status=%d, want 422", code)
	}
	// Over-release is 422 and releases nothing.
	code, _ = postJSON(t, srv.URL+"/accounts/alice/partial-unfreeze",
		`{"amount_cents":30001}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("over-release partial-unfreeze: status=%d, want 422", code)
	}
	// Partial release succeeds and the balance endpoint discloses
	// the remaining frozen amount.
	code, body = postJSON(t, srv.URL+"/accounts/alice/partial-unfreeze",
		`{"amount_cents":10000}`)
	if code != http.StatusOK || body["frozen_cents"] != float64(20000) || body["available_cents"] != float64(80000) {
		t.Fatalf("partial-unfreeze: status=%d body=%v, want frozen 20000 available 80000", code, body)
	}
	_, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if body["partial_frozen_cents"] != float64(20000) || body["available_cents"] != float64(80000) || body["frozen"] != false {
		t.Fatalf("balance: body=%v, want partial_frozen 20000 available 80000 frozen=false", body)
	}
}
