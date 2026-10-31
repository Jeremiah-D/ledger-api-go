package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestInvoiceIssueGetLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Issue an invoice.
	code, body := postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-1","account":"acme","currency":"USD","amount_cents":5000,"due_date":"2026-12-31T00:00:00Z"}`)
	if code != 201 {
		t.Fatalf("issue invoice: status=%d body=%v, want 201", code, body)
	}
	if body["status"] != "issued" || body["account"] != "acme" {
		t.Fatalf("issue body = %v, want issued/acme", body)
	}

	// GET returns it.
	code, body = getJSON(t, srv.URL+"/invoices/inv-1")
	if code != 200 {
		t.Fatalf("get invoice: status=%d body=%v, want 200", code, body)
	}
	if body["amount_cents"] != float64(5000) {
		t.Fatalf("get body = %v, want 5000 cents", body)
	}

	// Unknown ID: 404.
	code, _ = getJSON(t, srv.URL+"/invoices/nope")
	if code != 404 {
		t.Fatalf("get unknown invoice: status=%d, want 404", code)
	}

	// Duplicate ID: 409.
	code, _ = postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-1","account":"acme","amount_cents":100,"due_date":"2026-12-31T00:00:00Z"}`)
	if code != 409 {
		t.Fatalf("duplicate invoice: status=%d, want 409", code)
	}

	// Bad payload: 400.
	code, _ = postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-2","account":"acme","amount_cents":0,"due_date":"2026-12-31T00:00:00Z"}`)
	if code != 400 {
		t.Fatalf("zero-amount invoice: status=%d, want 400", code)
	}
}

func TestInvoiceApplyPaymentHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-a","account":"acme","amount_cents":1000,"due_date":"2026-12-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-b","account":"acme","amount_cents":2000,"due_date":"2026-12-31T00:00:00Z"}`)

	// 2500 retires inv-a fully and inv-b partially.
	code, body := postJSON(t, srv.URL+"/invoices/apply",
		`{"account":"acme","currency":"USD","amount_cents":2500}`)
	if code != 200 {
		t.Fatalf("apply payment: status=%d body=%v, want 200", code, body)
	}
	if body["remainder_cents"] != float64(0) {
		t.Fatalf("apply body = %v, want remainder 0", body)
	}
	applied := body["applied"].([]any)
	if len(applied) != 2 {
		t.Fatalf("applied = %d entries, want 2", len(applied))
	}

	// Overpaying returns the remainder.
	code, body = postJSON(t, srv.URL+"/invoices/apply",
		`{"account":"acme","currency":"USD","amount_cents":1000}`)
	if code != 200 || body["remainder_cents"] != float64(500) {
		t.Fatalf("overpay: status=%d body=%v, want 200 remainder 500", code, body)
	}
}

func TestInvoiceVoidHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// inv-p is issued first so the FIFO apply lands on it, leaving
	// inv-v unpaid and voidable.
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-p","account":"acme","amount_cents":1000,"due_date":"2026-12-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-v","account":"acme","amount_cents":1000,"due_date":"2026-12-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices/apply",
		`{"account":"acme","currency":"USD","amount_cents":1000}`)

	// inv-v is untouched: voiding works.
	code, body := postJSON(t, srv.URL+"/invoices/inv-v/void", "")
	if code != 200 || body["status"] != "voided" {
		t.Fatalf("void unpaid: status=%d body=%v, want 200 voided", code, body)
	}
	// inv-p was paid by the FIFO apply: voiding is rejected with 422.
	code, body = postJSON(t, srv.URL+"/invoices/inv-p/void", "")
	if code != 422 {
		t.Fatalf("void paid: status=%d body=%v, want 422", code, body)
	}
	if !strings.Contains(body["error"].(string), "cannot be voided") {
		t.Fatalf("422 body = %v, want a void-rejection error", body)
	}
	// Unknown invoice: 404.
	code, _ = postJSON(t, srv.URL+"/invoices/nope/void", "")
	if code != 404 {
		t.Fatalf("void unknown: status=%d, want 404", code)
	}
}

func TestInvoiceAmendDueDateHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// inv-open is issued with an explicit issue date so a due date
	// before it is a deterministic 400. inv-p2 lives on its own
	// account so the FIFO apply retires it alone, leaving it paid.
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-open","account":"acme","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-void","account":"acme","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-p2","account":"other","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices/apply",
		`{"account":"other","currency":"USD","amount_cents":1000}`)
	postJSON(t, srv.URL+"/invoices/inv-void/void", "")

	// Happy path: the due date moves, everything else stays.
	code, body := postJSON(t, srv.URL+"/invoices/inv-open/due-date",
		`{"due_date":"2026-12-15T00:00:00Z"}`)
	if code != 200 {
		t.Fatalf("amend due date: status=%d body=%v, want 200", code, body)
	}
	if body["due_date"] != "2026-12-15T00:00:00Z" || body["status"] != "issued" {
		t.Fatalf("amend body = %v, want due 2026-12-15 / issued", body)
	}

	// Unknown invoice: 404.
	code, _ = postJSON(t, srv.URL+"/invoices/nope/due-date",
		`{"due_date":"2026-12-15T00:00:00Z"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}
	// Paid invoice: 422 (same rejection as voiding a paid invoice).
	code, _ = postJSON(t, srv.URL+"/invoices/inv-p2/due-date",
		`{"due_date":"2026-12-15T00:00:00Z"}`)
	if code != 422 {
		t.Fatalf("amend paid: status=%d, want 422", code)
	}
	// Voided invoice: 422.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-void/due-date",
		`{"due_date":"2026-12-15T00:00:00Z"}`)
	if code != 422 {
		t.Fatalf("amend voided: status=%d, want 422", code)
	}
	// Missing due date: 400.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-open/due-date", `{}`)
	if code != 400 {
		t.Fatalf("amend missing due date: status=%d, want 400", code)
	}
	// Due date before the issue date: 400.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-open/due-date",
		`{"due_date":"2026-09-01T00:00:00Z"}`)
	if code != 400 {
		t.Fatalf("amend due before issued: status=%d, want 400", code)
	}
}

func TestInvoiceAmendCurrencyHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// inv-open is issued in USD; inv-p2 lives on its own account so
	// the FIFO apply retires it alone, leaving it paid.
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-open","account":"acme","currency":"USD","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-void","account":"acme","currency":"USD","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-p2","account":"other","currency":"USD","amount_cents":1000,"issued_at":"2026-10-01T00:00:00Z","due_date":"2026-10-31T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices/apply",
		`{"account":"other","currency":"USD","amount_cents":1000}`)
	postJSON(t, srv.URL+"/invoices/inv-void/void", "")

	// Happy path: the currency moves, everything else stays.
	code, body := postJSON(t, srv.URL+"/invoices/inv-open/currency",
		`{"currency":"EUR"}`)
	if code != 200 {
		t.Fatalf("amend currency: status=%d body=%v, want 200", code, body)
	}
	if body["currency"] != "EUR" || body["status"] != "issued" || body["amount_cents"] != float64(1000) {
		t.Fatalf("amend body = %v, want currency EUR / issued / 1000 cents", body)
	}

	// Unknown invoice: 404.
	code, _ = postJSON(t, srv.URL+"/invoices/nope/currency",
		`{"currency":"EUR"}`)
	if code != 404 {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}
	// Paid invoice: 422 (same rejection as voiding a paid invoice).
	code, _ = postJSON(t, srv.URL+"/invoices/inv-p2/currency",
		`{"currency":"EUR"}`)
	if code != 422 {
		t.Fatalf("amend paid: status=%d, want 422", code)
	}
	// Voided invoice: 422.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-void/currency",
		`{"currency":"EUR"}`)
	if code != 422 {
		t.Fatalf("amend voided: status=%d, want 422", code)
	}
	// Lowercase currency: 400.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-open/currency",
		`{"currency":"eur"}`)
	if code != 400 {
		t.Fatalf("amend lowercase currency: status=%d, want 400", code)
	}
	// Two-letter currency: 400.
	code, _ = postJSON(t, srv.URL+"/invoices/inv-open/currency",
		`{"currency":"EU"}`)
	if code != 400 {
		t.Fatalf("amend two-letter currency: status=%d, want 400", code)
	}
	// Missing currency: 400 (no default on an amendment).
	code, _ = postJSON(t, srv.URL+"/invoices/inv-open/currency", `{}`)
	if code != 400 {
		t.Fatalf("amend missing currency: status=%d, want 400", code)
	}

	// No-op: amending to the currency it already carries is 200 and
	// leaves the invoice unchanged.
	code, body = postJSON(t, srv.URL+"/invoices/inv-open/currency",
		`{"currency":"EUR"}`)
	if code != 200 || body["currency"] != "EUR" {
		t.Fatalf("amend same currency: status=%d body=%v, want 200 EUR", code, body)
	}
}

func TestInvoiceSweepOverdueHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-old","account":"acme","amount_cents":1000,"issued_at":"2019-06-01T00:00:00Z","due_date":"2020-01-01T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-new","account":"acme","amount_cents":2000,"due_date":"2030-01-01T00:00:00Z"}`)

	code, body := postJSON(t, srv.URL+"/invoices/sweep-overdue", "")
	if code != 200 {
		t.Fatalf("sweep overdue: status=%d, want 200", code)
	}
	overdue := body["overdue"].([]any)
	if len(overdue) != 1 || overdue[0] != "inv-old" {
		t.Fatalf("sweep body = %v, want [inv-old]", body)
	}

	// The swept invoice reports overdue.
	_, body = getJSON(t, srv.URL+"/invoices/inv-old")
	if body["status"] != "overdue" {
		t.Fatalf("inv-old status = %v, want overdue", body["status"])
	}
}

func TestInvoiceListHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-1","account":"a1","amount_cents":100,"due_date":"2030-01-01T00:00:00Z"}`)
	postJSON(t, srv.URL+"/invoices",
		`{"id":"inv-2","account":"a2","amount_cents":200,"due_date":"2030-01-01T00:00:00Z"}`)

	_, body := getJSON(t, srv.URL+"/invoices?account=a1")
	invoices := body["invoices"].([]any)
	if len(invoices) != 1 {
		t.Fatalf("list filtered = %d, want 1", len(invoices))
	}
	_, body = getJSON(t, srv.URL+"/invoices")
	invoices = body["invoices"].([]any)
	if len(invoices) != 2 {
		t.Fatalf("list all = %d, want 2", len(invoices))
	}
}
