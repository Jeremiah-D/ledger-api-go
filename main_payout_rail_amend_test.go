package main

import (
	"testing"
)

func TestHTTPPayoutAmendRail(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-rail","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice","rail":"swift"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}
	if body["rail"] != "swift" {
		t.Fatalf("draft rail = %v, want swift", body["rail"])
	}

	// Happy path: the rail moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-rail/rail",
		`{"rail":"sepa"}`)
	if code != 200 {
		t.Fatalf("amend rail: status=%d body=%v, want 200", code, body)
	}
	if body["rail"] != "sepa" || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) ||
		body["destination"] != "FPS:6222000012345678" ||
		body["currency"] != "USD" {
		t.Fatalf("amend body = %v, want rail sepa / draft / original amount, currency and destination", body)
	}

	// The rail is canonicalized: case and whitespace do not survive.
	code, body = postJSON(t, srv.URL+"/payouts/po-rail/rail",
		`{"rail":"  FedNow "}`)
	if code != 200 || body["rail"] != "fednow" {
		t.Fatalf("amend canonicalized: status=%d body=%v, want 200 / rail fednow", code, body)
	}

	// Empty clears the rail back to unspecified.
	code, body = postJSON(t, srv.URL+"/payouts/po-rail/rail",
		`{"rail":""}`)
	if code != 200 || body["rail"] != nil {
		t.Fatalf("amend empty: status=%d body=%v, want 200 / rail absent", code, body)
	}

	// A rail outside the SLA vocabulary: 422.
	for _, bad := range []string{`{"rail":"zelle"}`, `{"rail":"fedwire"}`} {
		code, _ = postJSON(t, srv.URL+"/payouts/po-rail/rail", bad)
		if code != 422 {
			t.Fatalf("amend %s: status=%d, want 422", bad, code)
		}
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/rail", `{"rail":"sepa"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved rail is the rail the
	// checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-rail/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-rail/rail", `{"rail":"rtp"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
