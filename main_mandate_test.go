package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedMandateServer creates a server with a funded payer and one
// mandate m-http (cust -> merchant, 5000 USD cap, per-day). It returns
// the server and the underlying ledger for direct setup.
func seedMandateServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"cust","credit_account":"bank","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed fund status = %d, want 201", code)
	}
	now := time.Now().UTC()
	code, body := postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-http","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("seed mandate status = %d, want 201", code)
	}
	if body["status"] != "active" {
		t.Fatalf("seed mandate status = %v, want active", body)
	}
	return srv, l
}

func TestHandleCreateMandate(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	now := time.Now().UTC()
	valid := fmt.Sprintf(`{"id":"m-1","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-week","valid_from":%q,"valid_until":%q}`,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339))

	code, body := postJSON(t, srv.URL+"/mandates", valid)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if body["id"] != "m-1" || body["frequency"] != "per-week" || body["status"] != "active" {
		t.Errorf("body = %v", body)
	}

	// Duplicate ID.
	code, _ = postJSON(t, srv.URL+"/mandates", valid)
	if code != http.StatusBadRequest {
		t.Errorf("duplicate status = %d, want 400", code)
	}
	// Bad frequency.
	code, _ = postJSON(t, srv.URL+"/mandates", `{"id":"m-2","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"yearly","valid_from":"2026-01-01T00:00:00Z","valid_until":"2027-01-01T00:00:00Z"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad frequency status = %d, want 400", code)
	}
	// Unknown field rejected (strict decoding).
	code, _ = postJSON(t, srv.URL+"/mandates", `{"id":"m-3","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","bogus":1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400", code)
	}
	// GET the mandate.
	code, body = getJSON(t, srv.URL+"/mandates/m-1")
	if code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", code)
	}
	if body["payer"] != "cust" || body["payee"] != "merchant" {
		t.Errorf("GET body = %v", body)
	}
	// Unknown mandate.
	code, _ = getJSON(t, srv.URL+"/mandates/nope")
	if code != http.StatusNotFound {
		t.Errorf("unknown GET status = %d, want 404", code)
	}
}

func TestHandleDebitMandate(t *testing.T) {
	srv, _ := seedMandateServer(t)

	// Happy path: 201 with the receipt; the memo carries the stamp.
	code, body := postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-1","amount_cents":1200,"memo":"sub"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if body["mandate_id"] != "m-http" {
		t.Errorf("mandate_id = %v", body["mandate_id"])
	}
	entry, _ := body["entry"].(map[string]any)
	if entry["debit_account"] != "merchant" || entry["credit_account"] != "cust" {
		t.Errorf("entry legs = %v", entry)
	}
	if entry["memo"] != "sub mandate_of:m-http" {
		t.Errorf("entry memo = %v", entry["memo"])
	}

	// Idempotent replay: 200 with duplicate. A second mandate isolates
	// the per-day frequency window from the d-1 debit above.
	now2 := time.Now().UTC()
	code, _ = postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-idem","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now2.Add(-time.Hour).Format(time.RFC3339), now2.Add(24*time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("m-idem create status = %d, want 201", code)
	}
	code, body = postJSON(t, srv.URL+"/mandates/m-idem/debits", `{"id":"d-x","amount_cents":1,"idempotency_key":"k-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("first keyed debit status = %d, want 201", code)
	}
	code, body = postJSON(t, srv.URL+"/mandates/m-idem/debits", `{"id":"d-y","amount_cents":1,"idempotency_key":"k-1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", code)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}

	// Over the per-debit cap: 422.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-3","amount_cents":5001}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("over-cap status = %d, want 422", code)
	}
	// Unknown mandate: 404.
	code, _ = postJSON(t, srv.URL+"/mandates/nope/debits", `{"id":"d-4","amount_cents":10}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown mandate status = %d, want 404", code)
	}
	// Frequency window used (per-day, already debited today): 422.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-5","amount_cents":10}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("frequency status = %d, want 422", code)
	}
}

func TestHandleListMandateDebits(t *testing.T) {
	srv, l := seedMandateServer(t)
	// A dedicated mandate with a wide validity window; three debits in
	// distinct months straight into the ledger (per-day frequency would
	// block same-window debits via HTTP).
	now := time.Now()
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m-pages", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: ledger.MandateFrequencyPerDay,
		ValidFrom: now.Add(-400 * 24 * time.Hour), ValidUntil: now.Add(400 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	for i, month := range []time.Month{time.January, time.February, time.March} {
		at := time.Date(2026, month, 15, 12, 0, 0, 0, time.UTC)
		if _, err := l.DebitWithMandate("m-pages", ledger.MandateDebit{
			ID: fmt.Sprintf("d-%d", i), AmountCents: 100, CreatedAt: at,
		}); err != nil {
			t.Fatalf("debit %d: %v", i, err)
		}
	}

	code, body := getJSON(t, srv.URL+"/mandates/m-pages/debits?limit=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	debits, _ := body["debits"].([]any)
	if len(debits) != 2 {
		t.Fatalf("debits = %d, want 2", len(debits))
	}
	next, _ := body["next_cursor"].(string)
	if next == "" {
		t.Fatalf("next_cursor empty, want d-1")
	}
	code, body = getJSON(t, srv.URL+"/mandates/m-pages/debits?cursor="+next)
	if code != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", code)
	}
	debits, _ = body["debits"].([]any)
	if len(debits) != 1 {
		t.Errorf("page 2 debits = %d, want 1", len(debits))
	}
	if body["next_cursor"] != "" {
		t.Errorf("page 2 next_cursor = %v, want empty", body["next_cursor"])
	}
	// Unknown mandate: 404.
	code, _ = getJSON(t, srv.URL+"/mandates/nope/debits")
	if code != http.StatusNotFound {
		t.Errorf("unknown mandate status = %d, want 404", code)
	}
	// Bad limit: 400.
	code, _ = getJSON(t, srv.URL+"/mandates/m-pages/debits?limit=0")
	if code != http.StatusBadRequest {
		t.Errorf("bad limit status = %d, want 400", code)
	}
}

func TestHandleMandateLifecycle(t *testing.T) {
	srv, _ := seedMandateServer(t)

	// Suspend.
	code, body := postJSON(t, srv.URL+"/mandates/m-http/suspend", `{}`)
	if code != http.StatusOK {
		t.Fatalf("suspend status = %d, want 200", code)
	}
	if body["status"] != "suspended" {
		t.Errorf("status = %v, want suspended", body["status"])
	}
	// Debits while suspended: 422.
	code, _ = postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-9","amount_cents":10}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("suspended debit status = %d, want 422", code)
	}
	// Cancel from suspended.
	code, body = postJSON(t, srv.URL+"/mandates/m-http/cancel", `{}`)
	if code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", code)
	}
	if body["status"] != "cancelled" {
		t.Errorf("status = %v, want cancelled", body["status"])
	}
	// Unknown mandate: 404.
	code, _ = postJSON(t, srv.URL+"/mandates/nope/suspend", `{}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown suspend status = %d, want 404", code)
	}
}

func TestHandleExpireMandates(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	now := time.Now().UTC()
	// A mandate that lapsed an hour ago: still stored active, reads
	// expired.
	code, _ := postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-old","payer":"cust","payee":"merchant","max_amount_cents":100,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	code, body := getJSON(t, srv.URL+"/mandates/m-old")
	if code != http.StatusOK || body["status"] != "expired" {
		t.Fatalf("GET = %d %v, want 200 expired", code, body)
	}
	code, body = postJSON(t, srv.URL+"/mandates/expire", `{}`)
	if code != http.StatusOK {
		t.Fatalf("expire status = %d, want 200", code)
	}
	if body["expired"] != float64(1) {
		t.Errorf("expired = %v, want 1", body["expired"])
	}
}

func TestMandateMetrics(t *testing.T) {
	srv, _ := seedMandateServer(t)
	code, _ := postJSON(t, srv.URL+"/mandates/m-http/debits", `{"id":"d-1","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("debit status = %d, want 201", code)
	}
	// A keyed debit on a second mandate, then its replay: the replay
	// must not move the counter.
	now := time.Now().UTC()
	code, _ = postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-metrics","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("m-metrics create status = %d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/mandates/m-metrics/debits", `{"id":"d-2","amount_cents":100,"idempotency_key":"mk"}`)
	if code != http.StatusCreated {
		t.Fatalf("keyed debit status = %d, want 201", code)
	}
	code, _ = postJSON(t, srv.URL+"/mandates/m-metrics/debits", `{"id":"d-3","amount_cents":100,"idempotency_key":"mk"}`)
	if code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", code)
	}
	code, text, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	vals := parseExposition(t, text)
	if vals["ledger_mandate_debits_total"] != 2 {
		t.Errorf("ledger_mandate_debits_total = %d, want 2", vals["ledger_mandate_debits_total"])
	}
}
