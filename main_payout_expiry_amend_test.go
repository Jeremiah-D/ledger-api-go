package main

import (
	"testing"
	"time"
)

func TestHTTPPayoutAmendExpiresAt(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	later := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)

	code, body := postJSON(t, srv.URL+"/payouts",
		`{"id":"po-exp","payee_account":"saver","amount_cents":100000,"destination":"FPS:6222000012345678","maker":"alice","expires_at":"`+future+`"}`)
	if code != 201 {
		t.Fatalf("draft: status=%d body=%v, want 201", code, body)
	}
	if body["expires_at"] != future {
		t.Fatalf("draft expiry = %v, want %v", body["expires_at"], future)
	}

	// Happy path: the expiry moves, everything else stays.
	code, body = postJSON(t, srv.URL+"/payouts/po-exp/expires-at",
		`{"expires_at":"`+later+`"}`)
	if code != 200 {
		t.Fatalf("amend expiry: status=%d body=%v, want 200", code, body)
	}
	if body["expires_at"] != later || body["status"] != "draft" ||
		body["amount_cents"] != float64(100000) ||
		body["destination"] != "FPS:6222000012345678" ||
		body["currency"] != "USD" ||
		body["payee_account"] != "saver" {
		t.Fatalf("amend body = %v, want expiry %v / draft / original amount, currency, destination and payee", body, later)
	}

	// A zero expiry clears the deadline back to "never expires".
	// Note: the zero time.Time serializes as the zero RFC3339
	// instant (omitempty does not omit a zero struct) — the same
	// rendering a never-expires draft gets on create.
	code, body = postJSON(t, srv.URL+"/payouts/po-exp/expires-at",
		`{"expires_at":"0001-01-01T00:00:00Z"}`)
	if code != 200 {
		t.Fatalf("clear expiry: status=%d body=%v, want 200", code, body)
	}
	if body["expires_at"] != "0001-01-01T00:00:00Z" {
		t.Fatalf("cleared expiry = %v, want 0001-01-01T00:00:00Z", body["expires_at"])
	}

	// A past expiry: 422 — the revision gate is stricter than
	// creation and refuses an already-lapsed deadline.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/payouts/po-exp/expires-at",
		`{"expires_at":"`+past+`"}`)
	if code != 422 {
		t.Fatalf("amend past expiry: status=%d, want 422", code)
	}

	// Unknown payout: 404.
	code, _ = postJSON(t, srv.URL+"/payouts/nope/expires-at", `{"expires_at":"`+later+`"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}

	// Approved instruction: 422 — the approved deadline is the
	// deadline the checker signed off on.
	code, _ = postJSON(t, srv.URL+"/payouts/po-exp/expires-at",
		`{"expires_at":"`+later+`"}`)
	if code != 200 {
		t.Fatalf("restore expiry: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-exp/approve", `{"approver":"bob"}`)
	if code != 200 {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-exp/expires-at", `{"expires_at":"`+later+`"}`)
	if code != 422 {
		t.Fatalf("amend approved: status=%d, want 422", code)
	}
}
