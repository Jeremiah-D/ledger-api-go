package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedRevalServer builds a server with a funded EUR balance, a live
// EUR->USD rate, and the revaluation P&L account configured.
func seedRevalServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithRevaluationAccount("fx-pnl"))
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-eur", DebitAccount: "trader", CreditAccount: "bank",
		AmountCents: 10000, Currency: "EUR",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetFXRate("EUR", "USD", 11, 10); err != nil {
		t.Fatalf("seed SetFXRate: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHandleCreateRevaluationLifecycle(t *testing.T) {
	srv := seedRevalServer(t)
	defer srv.Close()

	// First run: books the initial recognition (trader + bank legs).
	code, body := postJSON(t, srv.URL+"/revaluations", `{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%v)", code, body)
	}
	if body["base_currency"] != "USD" {
		t.Fatalf("base_currency = %v", body["base_currency"])
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	// Idempotent repeat for the same asOf: zero new entries.
	code, body = postJSON(t, srv.URL+"/revaluations", `{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("repeat status = %d", code)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 0 {
		t.Fatalf("repeat entries = %d, want 0", len(entries))
	}

	// The trader's USD carrying value is visible on the trial balance.
	code, body = getJSON(t, srv.URL+"/accounts/trader/trial-balance")
	if code != http.StatusOK {
		t.Fatalf("trial-balance status = %d", code)
	}
	rows, _ := body["by_currency"].([]any)
	var usdRow map[string]any
	for _, r := range rows {
		if m, _ := r.(map[string]any); m["currency"] == "USD" {
			usdRow = m
		}
	}
	if usdRow == nil {
		t.Fatalf("no USD row in by_currency: %v", rows)
	}
	if usdRow["net_balance_cents"] != float64(11000) {
		t.Fatalf("trader USD net = %v, want 11000", usdRow["net_balance_cents"])
	}
}

func TestHandleCreateRevaluationBadBaseCurrency(t *testing.T) {
	srv := seedRevalServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/revaluations", `{"base_currency":"US"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	code, _ = postJSON(t, srv.URL+"/revaluations", `{"asof":"not-a-time","base_currency":"USD"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad asof status = %d, want 400", code)
	}
}

func TestHandleCreateRevaluationNotConfigured(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/revaluations", `{"base_currency":"USD"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "not configured") {
		t.Fatalf("error = %v, want revaluation-account complaint", body["error"])
	}
}

func TestHandleCreateRevaluationMetrics(t *testing.T) {
	srv := seedRevalServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/revaluations", `{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	// A no-op repeat run moves the counter by zero.
	code, _ = postJSON(t, srv.URL+"/revaluations", `{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("repeat status = %d", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(raw), "ledger_revaluations_total 2\n") {
		t.Fatalf("/metrics missing ledger_revaluations_total 2:\n%s", raw)
	}
}

func TestHandleCreateRevaluationInReconcile(t *testing.T) {
	srv := seedRevalServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/revaluations", `{"asof":"2026-10-11T12:00:00Z","base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("revalue status = %d", code)
	}
	code, body := postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("reconcile status = %d", code)
	}
	if body["revaluation_account"] != "fx-pnl" {
		t.Fatalf("revaluation_account = %v, want fx-pnl", body["revaluation_account"])
	}
	impact, _ := body["revaluation_impact"].([]any)
	if len(impact) != 2 {
		t.Fatalf("revaluation_impact rows = %d, want 2", len(impact))
	}
}
