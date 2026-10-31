package main

import (
	"testing"
)

func TestHTTPPayoutAmendAmount(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-amt","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}

	// Happy path: the amount moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-amt/amount",
		`{"amount_cents":250000}`)
	if code != 200 {
		t.Fatalf("amend amount: status=%d body=%v, want 200", code, body)
	}
	if body["amount_cents"] != float64(250000) || body["status"] != "draft" ||
		body["destination"] != "FPS:6222000012345678" {
		t.Fatalf("amend body = %v, want amount 250000 / draft / original destination", body)
	}

	// Non-positive amount: 422.
	code, _ = postJSON(t, srv.URL+"/payouts/po-amt/amount", `{"amount_cents":0}`)
	if code != 422 {
		t.Fatalf("amend zero: status=%d, want 422", code)
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/amount", `{"amount_cents":1000}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved amount is the amount
	// the checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-amt/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-amt/amount", `{"amount_cents":300000}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}

