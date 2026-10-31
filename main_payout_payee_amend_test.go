package main

import (
	"testing"
)

func TestHTTPPayoutAmendPayeeAccount(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-payee","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}
	if body["payee_account"] != "saver" {
		t.Fatalf("draft payee = %v, want saver", body["payee_account"])
	}

	// Happy path: the payee moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-payee/payee",
		`{"payee_account":"spender"}`)
	if code != 200 {
		t.Fatalf("amend payee: status=%d body=%v, want 200", code, body)
	}
	if body["payee_account"] != "spender" || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) ||
		body["destination"] != "FPS:6222000012345678" ||
		body["currency"] != "USD" {
		t.Fatalf("amend body = %v, want payee spender / draft / original amount, currency and destination", body)
	}

	// An empty payee: 422.
	code, _ = postJSON(t, srv.URL+"/payouts/po-payee/payee",
		`{"payee_account":""}`)
	if code != 422 {
		t.Fatalf("amend empty payee: status=%d, want 422", code)
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/payee", `{"payee_account":"spender"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved payee is the payee the
	// checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-payee/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-payee/payee", `{"payee_account":"saver"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
