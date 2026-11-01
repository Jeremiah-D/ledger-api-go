package main

import (
	"testing"
)

func TestHTTPPayoutAmendPurposeCode(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-purp","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice","purpose_code":"SUPP"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}
	if body["purpose_code"] != "SUPP" {
		t.Fatalf("draft purpose = %v, want SUPP", body["purpose_code"])
	}

	// Happy path: the purpose moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-purp/purpose",
		`{"purpose_code":"SALA"}`)
	if code != 200 {
		t.Fatalf("amend purpose: status=%d body=%v, want 200", code, body)
	}
	if body["purpose_code"] != "SALA" || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) ||
		body["destination"] != "FPS:6222000012345678" ||
		body["currency"] != "USD" ||
		body["payee_account"] != "saver" {
		t.Fatalf("amend body = %v, want purpose SALA / draft / original amount, currency, destination and payee", body)
	}

	// An empty purpose clears it back to "no purpose stated".
	code, body = postJSON(t, srv.URL+"/payouts/po-purp/purpose",
		`{"purpose_code":""}`)
	if code != 200 {
		t.Fatalf("clear purpose: status=%d body=%v, want 200", code, body)
	}
	if body["purpose_code"] != "" && body["purpose_code"] != nil {
		t.Fatalf("cleared purpose = %v, want empty/absent (omitempty)", body["purpose_code"])
	}

	// A code outside the admitted ISO 20022 subset: 422.
	code, _ = postJSON(t, srv.URL+"/payouts/po-purp/purpose",
		`{"purpose_code":"BOGUS"}`)
	if code != 422 {
		t.Fatalf("amend bogus purpose: status=%d, want 422", code)
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/purpose", `{"purpose_code":"SALA"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved purpose is the purpose
	// the checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-purp/purpose",
		`{"purpose_code":"SUPP"}`)
	if code != 200 {
		t.Fatalf("restore purpose: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-purp/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-purp/purpose", `{"purpose_code":"SALA"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
