package main

import (
	"testing"
)

func TestHTTPPayoutAmendCurrency(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-cur","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}
	if body["currency"] != "USD" {
		t.Fatalf("draft currency = %v, want USD default", body["currency"])
	}

	// Happy path: the currency moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-cur/currency",
		`{"currency":"EUR"}`)
	if code != 200 {
		t.Fatalf("amend currency: status=%d body=%v, want 200", code, body)
	}
	if body["currency"] != "EUR" || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) ||
		body["destination"] != "FPS:6222000012345678" {
		t.Fatalf("amend body = %v, want currency EUR / draft / original amount and destination", body)
	}

	// Invalid currency: 400 — lowercase, short, and empty codes are
	// all malformed, not semantically rejected.
	for _, bad := range []string{`{"currency":"eur"}`, `{"currency":"EU"}`, `{"currency":""}`} {
		code, _ = postJSON(t, srv.URL+"/payouts/po-cur/currency", bad)
		if code != 400 {
			t.Fatalf("amend %s: status=%d, want 400", bad, code)
		}
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/currency", `{"currency":"EUR"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved currency is the
	// currency the checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-cur/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-cur/currency", `{"currency":"GBP"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
