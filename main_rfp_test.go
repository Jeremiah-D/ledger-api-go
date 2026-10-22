package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full RfP flow through HTTP: issue, accept (payout posts), re-accept
// replay, reject, and the error mapping.
func TestHTTPRfPFlow(t *testing.T) {
	l := ledger.New()
	if _, dup, err := l.Post(ledger.JournalEntry{
		ID: "rfp-fund", DebitAccount: "customer", CreditAccount: "equity",
		AmountCents: 10000, Currency: "USD", CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil || dup {
		t.Fatalf("seed: %v dup=%v", err, dup)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Issue: 201.
	body := fmt.Sprintf(`{"id":"rfp-http-1","creditor":"merchant","debtor":"customer","amount_cents":5000,"currency":"USD","expires_at":%q}`,
		time.Now().Add(time.Hour).Format(time.RFC3339))
	code, resp := postJSON(t, srv.URL+"/rfp", body)
	if code != http.StatusCreated {
		t.Fatalf("issue: status=%d, want 201 (%v)", code, resp)
	}
	if resp["status"] != "issued" {
		t.Fatalf("issue: status=%v, want issued", resp["status"])
	}
	// Re-issue the same ID: 200 replay.
	code, _ = postJSON(t, srv.URL+"/rfp", body)
	if code != http.StatusOK {
		t.Fatalf("re-issue: status=%d, want 200", code)
	}
	// Bad issue: same party -> 400.
	code, _ = postJSON(t, srv.URL+"/rfp", `{"id":"x","creditor":"a","debtor":"a","amount_cents":1,"expires_at":"2030-01-01T00:00:00Z"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("same-party issue: status=%d, want 400", code)
	}

	// Accept: the payout posts debtor -> creditor.
	code, arec := postJSON(t, srv.URL+"/rfp/rfp-http-1/accept", `{}`)
	if code != http.StatusOK {
		t.Fatalf("accept: status=%d, want 200 (%v)", code, arec)
	}
	if arec["transfer_id"] != "rfp/rfp-http-1/payout" {
		t.Fatalf("accept: transfer_id=%v", arec["transfer_id"])
	}
	// Re-accept replays the receipt.
	code, arec2 := postJSON(t, srv.URL+"/rfp/rfp-http-1/accept", `{}`)
	if code != http.StatusOK || arec2["transfer_id"] != "rfp/rfp-http-1/payout" {
		t.Fatalf("re-accept: status=%d (%v)", code, arec2)
	}
	// Balances moved exactly once.
	code, bal := getJSON(t, srv.URL+"/accounts/customer/balance")
	if code != http.StatusOK || bal["balance_cents"] != float64(5000) {
		t.Fatalf("customer balance: status=%d (%v)", code, bal)
	}

	// Reject an issued RfP, then reject/accept again -> 422.
	code, _ = postJSON(t, srv.URL+"/rfp", `{"id":"rfp-http-2","creditor":"m","debtor":"c","amount_cents":10,"expires_at":"2030-01-01T00:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue rfp-http-2: status=%d", code)
	}
	code, rrec := postJSON(t, srv.URL+"/rfp/rfp-http-2/reject", `{}`)
	if code != http.StatusOK || rrec["status"] != "rejected" {
		t.Fatalf("reject: status=%d (%v)", code, rrec)
	}
	code, _ = postJSON(t, srv.URL+"/rfp/rfp-http-2/reject", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("double reject: status=%d, want 422", code)
	}
	code, _ = postJSON(t, srv.URL+"/rfp/rfp-http-2/accept", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("accept after reject: status=%d, want 422", code)
	}

	// Unknown RfP: 404.
	code, _ = postJSON(t, srv.URL+"/rfp/nope/accept", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("accept unknown: status=%d, want 404", code)
	}
	code, _ = postJSON(t, srv.URL+"/rfp/nope/reject", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("reject unknown: status=%d, want 404", code)
	}
}
