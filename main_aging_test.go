package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedAgingServer books two AR positions: cust-old (9000c, 100 days old,
// overdue) and cust-new (500c, 5 days old, current).
func seedAgingServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	post := func(id, debit, credit string, cents int64, at time.Time) {
		if _, dup, err := l.Post(ledger.JournalEntry{
			ID: id, DebitAccount: ledger.AccountID(debit), CreditAccount: ledger.AccountID(credit),
			AmountCents: cents, Currency: "USD", IdempotencyKey: "k-" + id, CreatedAt: at,
		}); err != nil || dup {
			t.Fatalf("seed Post %s = dup=%v err=%v", id, dup, err)
		}
	}
	post("ag-old", "cust-old", "ar-clearing", 9000, asof.Add(-100*24*time.Hour))
	post("ag-new", "cust-new", "ar-clearing", 500, asof.Add(-5*24*time.Hour))
	return httptest.NewServer(newRouter(l))
}

func agingBody(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	return getJSON(t, url)
}

func TestHandleAging(t *testing.T) {
	srv := seedAgingServer(t)
	defer srv.Close()

	code, body := agingBody(t, srv.URL+"/aging?asof=2026-10-10T12:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("GET /aging = %d, want 200", code)
	}
	accounts, ok := body["accounts"].([]any)
	if !ok || len(accounts) != 2 {
		t.Fatalf("accounts = %v, want 2 rows (cust-old, cust-new)", body["accounts"])
	}
	first := accounts[0].(map[string]any)
	if first["account"] != "cust-new" {
		t.Fatalf("first row account = %v, want cust-new (sorted order)", first["account"])
	}
	alerts, _ := body["alerts"].([]any)
	if len(alerts) != 0 {
		t.Fatalf("alerts = %v, want none (threshold disabled)", alerts)
	}
}

func TestHandleAgingAlert(t *testing.T) {
	srv := seedAgingServer(t)
	defer srv.Close()

	code, body := agingBody(t, srv.URL+"/aging?asof=2026-10-10T12:00:00Z&alert_overdue_cents=1000")
	if code != http.StatusOK {
		t.Fatalf("GET /aging = %d, want 200", code)
	}
	alerts, ok := body["alerts"].([]any)
	if !ok || len(alerts) != 1 {
		t.Fatalf("alerts = %v, want 1 (cust-old overdue 9000 > 1000)", body["alerts"])
	}
	alert := alerts[0].(map[string]any)
	if alert["account"] != "cust-old" || alert["overdue_cents"] != float64(9000) {
		t.Fatalf("alert = %v, want cust-old overdue 9000", alert)
	}
}

func TestHandleAgingAccountFilter(t *testing.T) {
	srv := seedAgingServer(t)
	defer srv.Close()

	code, body := agingBody(t, srv.URL+"/aging?asof=2026-10-10T12:00:00Z&account=cust-new")
	if code != http.StatusOK {
		t.Fatalf("GET /aging = %d, want 200", code)
	}
	accounts, ok := body["accounts"].([]any)
	if !ok || len(accounts) != 1 || accounts[0].(map[string]any)["account"] != "cust-new" {
		t.Fatalf("accounts = %v, want only cust-new", body["accounts"])
	}
}

func TestHandleAgingBadParams(t *testing.T) {
	srv := seedAgingServer(t)
	defer srv.Close()

	if code, _ := agingBody(t, srv.URL+"/aging?asof=not-a-date"); code != http.StatusBadRequest {
		t.Fatalf("bad asof = %d, want 400", code)
	}
	if code, _ := agingBody(t, srv.URL+"/aging?alert_overdue_cents=abc"); code != http.StatusBadRequest {
		t.Fatalf("bad alert_overdue_cents = %d, want 400", code)
	}
}
