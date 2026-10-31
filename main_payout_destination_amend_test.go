package main

import (
	"testing"
)

func TestHTTPPayoutAmendDestination(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-dst","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}

	// Happy path: the destination moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-dst/destination",
		`{"destination":"FPS:6222000099999999"}`)
	if code != 200 {
		t.Fatalf("amend destination: status=%d body=%v, want 200", code, body)
	}
	if body["destination"] != "FPS:6222000099999999" || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) {
		t.Fatalf("amend body = %v, want new destination / draft / amount 100000", body)
	}

	// Empty destination: 422.
	code, _ = postJSON(t, srv.URL+"/payouts/po-dst/destination", `{"destination":""}`)
	if code != 422 {
		t.Fatalf("amend empty: status=%d, want 422", code)
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/destination",
		`{"destination":"FPS:6222000099999999"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved destination is the
	// destination the checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-dst/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-dst/destination",
		`{"destination":"FPS:6222000011111111"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
