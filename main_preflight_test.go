package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedPreflightLedger builds a ledger with one pending dual-control
// review (a blocker for the period close).
func seedPreflightLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New(ledger.WithReviewThreshold(1)) // every transfer routes to review
	if _, err := l.PostTransfer(ledger.Transfer{ID: "xfer-1", From: "ar-clearing",
		To: "cust", AmountCents: 10000, Currency: "USD", IdempotencyKey: "kx1",
		CreatedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	return l
}

// TestHandlePeriodPreflight: the operator endpoint reports the blockers
// without changing anything; a malformed period ID fails 400.
func TestHandlePeriodPreflight(t *testing.T) {
	srv := httptest.NewServer(newRouter(seedPreflightLedger(t)))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/periods/2026-10/preflight", "")
	if code != http.StatusOK {
		t.Fatalf("POST /preflight = %d, want 200", code)
	}
	if body["clean"] != false {
		t.Fatalf("body = %v, want clean=false", body)
	}
	blockers, ok := body["blockers"].([]any)
	if !ok || len(blockers) != 1 {
		t.Fatalf("blockers = %v, want 1", body["blockers"])
	}
	first := blockers[0].(map[string]any)
	if first["code"] != "pending_reviews" {
		t.Fatalf("blocker code = %v, want pending_reviews", first["code"])
	}

	code, _ = postJSON(t, srv.URL+"/periods/2026-13/preflight", "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /preflight bad ID = %d, want 400", code)
	}
}

// TestHandleClosePeriodPreflightGate: with ?preflight_clean=true a
// blocked close is refused 422 and the period stays open; without the
// gate the close proceeds.
func TestHandleClosePeriodPreflightGate(t *testing.T) {
	srv := httptest.NewServer(newRouter(seedPreflightLedger(t)))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/periods/2026-10/close?preflight_clean=true", "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("close?preflight_clean=true = %d, want 422 (body %v)", code, body)
	}
	// The period must still be open: a plain close succeeds afterwards.
	code, _ = postJSON(t, srv.URL+"/periods/2026-10/close", "")
	if code != http.StatusOK {
		t.Fatalf("close (gate off) = %d, want 200", code)
	}
}

// TestHandleClosePeriodPreflightGateClean: a clean ledger closes with the
// gate on.
func TestHandleClosePeriodPreflightGateClean(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/periods/2026-10/close?preflight_clean=true", "")
	if code != http.StatusOK {
		t.Fatalf("close?preflight_clean=true (clean) = %d, want 200", code)
	}
}

// TestHandleClosePeriodClosingPreflightGate: the ?closing=true path
// honors the preflight gate too (refused before booking anything).
func TestHandleClosePeriodClosingPreflightGate(t *testing.T) {
	srv := httptest.NewServer(newRouter(seedPreflightLedger(t)))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/periods/2026-10/close?closing=true&preflight_clean=true", "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("close?closing=true&preflight_clean=true = %d, want 422", code)
	}
}

// TestMetricsIncludesDunningEscalations: a fired escalation shows up in
// the /metrics exposition as ledger_dunning_escalations_total.
func TestMetricsIncludesDunningEscalations(t *testing.T) {
	l := ledger.New()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.Post(ledger.JournalEntry{ID: "d1", DebitAccount: "cust",
		CreditAccount: "ar-clearing", AmountCents: 10000, Currency: "USD",
		IdempotencyKey: "kd1", CreatedAt: now.Add(-100 * 24 * time.Hour)}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := l.SetDunningConfig(ledger.DunningConfig{Account: "cust", Currency: "USD",
		Level1Days: 30, Level2Days: 60, Level3Days: 90}); err != nil {
		t.Fatalf("SetDunningConfig: %v", err)
	}
	if esc := l.EscalateDunning(now); len(esc) != 1 {
		t.Fatalf("EscalateDunning = %v, want 1", esc)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_dunning_escalations_total 1") {
		t.Fatalf("metrics exposition missing dunning counter at 1:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "ledger_period_preflight_runs_total") {
		t.Fatal("metrics exposition missing ledger_period_preflight_runs_total")
	}
}
