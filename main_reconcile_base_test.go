package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newBaseCurrencyTestServer builds a ledger with EUR->USD 108/100 and one
// EUR posting (fees 900 / cash 900), served over HTTP.
func newBaseCurrencyTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, dup, err := l.Post(ledger.JournalEntry{
		ID: "bc-1", DebitAccount: "fees", CreditAccount: "cash",
		AmountCents: 900, Currency: "EUR", IdempotencyKey: "bck-1",
	}); err != nil || dup {
		t.Fatalf("seed Post(bc-1) = dup=%v err=%v", dup, err)
	}
	if err := l.SetFXRate("EUR", "USD", 108, 100); err != nil {
		t.Fatalf("SetFXRate(EUR->USD): %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func postReconcile(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, rdr)
	if err != nil {
		t.Fatalf("build POST /reconcile: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /reconcile: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode /reconcile body: %v", err)
	}
	return resp.StatusCode, decoded
}

// POST /reconcile with {"base_currency":"USD"} returns the report with a
// base-currency summary: 900 EUR * 108/100 = 972 USD, rate snapshot with
// the effective version, and fx_applied=true.
func TestHandleReconcileBaseCurrency(t *testing.T) {
	srv := newBaseCurrencyTestServer(t)
	defer srv.Close()

	code, body := postReconcile(t, srv.URL+"/reconcile", `{"base_currency":"USD"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%v)", code, body)
	}
	if body["fx_applied"] != true {
		t.Fatalf("fx_applied = %v, want true", body["fx_applied"])
	}
	sum, ok := body["base_currency_summary"].(map[string]any)
	if !ok {
		t.Fatalf("base_currency_summary missing or not an object: %v", body)
	}
	if sum["base_currency"] != "USD" {
		t.Errorf("base_currency = %v, want USD", sum["base_currency"])
	}
	if sum["total_debits_cents"] != float64(972) || sum["total_credits_cents"] != float64(972) {
		t.Errorf("base totals = %v/%v, want 972/972",
			sum["total_debits_cents"], sum["total_credits_cents"])
	}
	if sum["fx_incomplete"] != false {
		t.Errorf("fx_incomplete = %v, want false", sum["fx_incomplete"])
	}
	conv, ok := sum["conversions"].([]any)
	if !ok || len(conv) != 1 {
		t.Fatalf("conversions = %v, want the single EUR row", sum["conversions"])
	}
	row := conv[0].(map[string]any)
	if row["currency"] != "EUR" || row["total_debits_cents"] != float64(972) {
		t.Errorf("conversion row = %v, want EUR 972/972", row)
	}
	snap, ok := sum["fx_snapshot"].([]any)
	if !ok || len(snap) != 1 {
		t.Fatalf("fx_snapshot = %v, want the single EUR rate", sum["fx_snapshot"])
	}
	rate := snap[0].(map[string]any)
	if rate["currency"] != "EUR" || rate["rate_num"] != float64(108) ||
		rate["rate_den"] != float64(100) || rate["rate_asof_version"] != float64(1) {
		t.Errorf("fx_snapshot row = %v, want EUR 108/100 at version 1", rate)
	}
	missing, ok := sum["missing_rates"].([]any)
	if !ok || len(missing) != 0 {
		t.Errorf("missing_rates = %v, want empty list", sum["missing_rates"])
	}
}

// An empty body keeps the legacy report: fx_applied=false and no
// base_currency_summary key — existing cron jobs are unaffected.
func TestHandleReconcileBaseCurrencyEmptyBody(t *testing.T) {
	srv := newBaseCurrencyTestServer(t)
	defer srv.Close()

	code, body := postReconcile(t, srv.URL+"/reconcile", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%v)", code, body)
	}
	if body["fx_applied"] != false {
		t.Errorf("fx_applied = %v, want false without a body", body["fx_applied"])
	}
	if _, ok := body["base_currency_summary"]; ok {
		t.Errorf("base_currency_summary present without base_currency: %v", body["base_currency_summary"])
	}
	if body["accounting_equation_ok"] != true {
		t.Errorf("accounting_equation_ok = %v, want true", body["accounting_equation_ok"])
	}
}

// An invalid base_currency is a 400; an unknown field is a 400 too
// (strict decoding, like every other POST endpoint).
func TestHandleReconcileInvalidBaseCurrency(t *testing.T) {
	srv := newBaseCurrencyTestServer(t)
	defer srv.Close()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"lowercase", `{"base_currency":"usd"}`},
		{"too short", `{"base_currency":"US"}`},
		{"unknown field", `{"base_currency":"USD","bogus":1}`},
		{"trailing data", `{"base_currency":"USD"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := postReconcile(t, srv.URL+"/reconcile", tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %q", code, tc.body)
			}
		})
	}
}

// The summary also shows up in the archived (indented) JSON: the file an
// operator tees to disk carries the same fields.
func TestHandleReconcileBaseCurrencyWriteJSONShape(t *testing.T) {
	srv := newBaseCurrencyTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/reconcile",
		bytes.NewBufferString(`{"base_currency":"USD"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /reconcile: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`"fx_applied": true`,
		`"base_currency": "USD"`,
		`"rate_asof_version": 1`,
		`"missing_rates": []`,
		`"fx_incomplete": false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("archived JSON lacks %s:\n%s", want, out)
		}
	}
}
