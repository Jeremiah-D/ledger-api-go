package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func putJSON(t *testing.T, url, payload string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestClosingAccountsEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Empty by default.
	code, body := getJSON(t, srv.URL+"/closing-accounts")
	if code != http.StatusOK || body["retained_earnings"] != "" {
		t.Fatalf("GET: status=%d body=%v, want 200 with empty config", code, body)
	}

	// Misconfiguration fails 400.
	code, body = putJSON(t, srv.URL+"/closing-accounts",
		`{"pl_accounts":["revenue"],"retained_earnings":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("pl without retained: status=%d, want 400", code)
	}
	code, body = putJSON(t, srv.URL+"/closing-accounts",
		`{"pl_accounts":["retained"],"retained_earnings":"retained"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("retained in pl: status=%d, want 400", code)
	}

	// Happy path.
	code, body = putJSON(t, srv.URL+"/closing-accounts",
		`{"pl_accounts":["revenue","expenses"],"retained_earnings":"retained"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%v, want 200", code, body)
	}
	if body["retained_earnings"] != "retained" {
		t.Fatalf("PUT body = %v", body)
	}
	code, body = getJSON(t, srv.URL+"/closing-accounts")
	if code != http.StatusOK || body["retained_earnings"] != "retained" {
		t.Fatalf("GET: status=%d body=%v", code, body)
	}
	pls, ok := body["pl_accounts"].([]any)
	if !ok || len(pls) != 2 {
		t.Fatalf("GET pl_accounts = %v", body["pl_accounts"])
	}
}

func TestClosePeriodWithClosingEndpoint(t *testing.T) {
	// Seed September postings directly through the ledger: POST
	// /entries does not accept backdated timestamps.
	l := ledger.New()
	sept := func(id, debit, credit string, cents int64) {
		t.Helper()
		if _, _, err := l.Post(ledger.JournalEntry{
			ID:            id,
			DebitAccount:  ledger.AccountID(debit),
			CreditAccount: ledger.AccountID(credit),
			AmountCents:   cents,
			Currency:      "USD",
			CreatedAt:     mustParseTime(t, "2026-09-15T12:00:00Z"),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	sept("rev-1", "cash", "revenue", 500)
	sept("exp-1", "expenses", "cash", 200)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	// Without closing accounts configured, closing=true fails 400 and
	// the period stays open.
	code, body := postJSON(t, srv.URL+"/periods/2026-09/close?closing=true", "")
	if code != http.StatusBadRequest {
		t.Fatalf("unconfigured closing: status=%d, want 400", code)
	}

	// Configure, then close with closing entries.
	code, _ = putJSON(t, srv.URL+"/closing-accounts",
		`{"pl_accounts":["revenue","expenses"],"retained_earnings":"retained"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT closing-accounts: status=%d", code)
	}
	code, body = postJSON(t, srv.URL+"/periods/2026-09/close?closing=true", "")
	if code != http.StatusOK || body["closed"] != true || body["closing"] != true {
		t.Fatalf("close?closing=true: status=%d body=%v", code, body)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %v, want 2 closing entries", body["entries"])
	}

	// P&L accounts read zero; retained holds the net.
	code, body = getJSON(t, srv.URL+"/accounts/revenue/balance")
	if code != http.StatusOK || body["balance_cents"] != float64(0) {
		t.Fatalf("revenue balance = %v", body)
	}
	code, body = getJSON(t, srv.URL+"/accounts/retained/balance")
	if code != http.StatusOK || body["balance_cents"] != float64(-300) {
		t.Fatalf("retained balance = %v", body)
	}

	// Closing entries are queryable by memo.
	code, body = getJSON(t, srv.URL+"/entries?since=2026-09-01T00:00:00Z&until=2026-11-01T00:00:00Z&limit=100&memo=closing:2026-09")
	if code != http.StatusOK {
		t.Fatalf("memo query: status=%d", code)
	}
	items, ok := body["entries"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("memo query entries = %v, want 2", body["entries"])
	}

	// Re-closing is an idempotent no-op.
	code, body = postJSON(t, srv.URL+"/periods/2026-09/close?closing=true", "")
	if code != http.StatusOK || body["already_closed"] != true {
		t.Fatalf("re-close: status=%d body=%v", code, body)
	}

	// Metrics exposition carries the new counters.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, name := range []string{
		"ledger_period_closes_total",
		"ledger_closing_entries_total",
		"ledger_balance_health_violations_total",
	} {
		if !strings.Contains(string(raw), name) {
			t.Fatalf("/metrics missing %s", name)
		}
	}
}

func TestClosePeriodWithClosingFrozenEndpoint(t *testing.T) {
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID:            "exp-1",
		DebitAccount:  "expenses",
		CreditAccount: "cash",
		AmountCents:   200,
		Currency:      "USD",
		CreatedAt:     mustParseTime(t, "2026-09-15T12:00:00Z"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := putJSON(t, srv.URL+"/closing-accounts",
		`{"pl_accounts":["expenses"],"retained_earnings":"retained"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT closing-accounts: status=%d", code)
	}
	// Freeze the retained account: the close fails 403 and September
	// stays open (never a silent half-close).
	code, _ = postJSON(t, srv.URL+"/accounts/retained/freeze", "")
	if code != http.StatusOK {
		t.Fatalf("freeze: status=%d", code)
	}
	code, body := postJSON(t, srv.URL+"/periods/2026-09/close?closing=true", "")
	if code != http.StatusForbidden {
		t.Fatalf("frozen close: status=%d body=%v, want 403", code, body)
	}
	code, _ = postJSON(t, srv.URL+"/periods/2026-09/close", "")
	if code != http.StatusOK {
		t.Fatalf("plain close after failed closing close: status=%d", code)
	}
}
