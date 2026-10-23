package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full accounting export flow through HTTP: post entries, export the
// Xero CSV and the QuickBooks IIF, and exercise the error mapping.
func TestHTTPAccountingExports(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	get := func(t *testing.T, url string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", url, err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}

	code, body1 := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"saver","credit_account":"funding","amount_cents":250000,"currency":"USD","memo":"salary"}`)
	if code != http.StatusCreated {
		t.Fatalf("post 1: status=%d body=%v, want 201", code, body1)
	}
	id1, _ := body1["id"].(string)
	if id1 == "" {
		t.Fatalf("post 1 response has no id: %v", body1)
	}
	code, body2 := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"expenses","credit_account":"saver","amount_cents":7550,"currency":"USD","memo":"groceries"}`)
	if code != http.StatusCreated {
		t.Fatalf("post 2: status=%d body=%v, want 201", code, body2)
	}
	id2, _ := body2["id"].(string)
	if id2 == "" {
		t.Fatalf("post 2 response has no id: %v", body2)
	}

	// Xero: account is required.
	if code, _, _ := get(t, srv.URL+"/exports/accounting/xero"); code != http.StatusBadRequest {
		t.Errorf("xero without account: status=%d, want 400", code)
	}
	// Xero happy path with a name mapping.
	code, hdr, body := get(t, srv.URL+"/exports/accounting/xero?account=saver&name=saver:Savings+Account&name=funding:Initial+Funding&name=expenses:Operating+Expenses")
	if code != http.StatusOK {
		t.Fatalf("xero: status=%d body=%s, want 200", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("xero content-type = %q", ct)
	}
	for _, want := range []string{
		"Date,Amount,Payee,Description,Reference,CheckNumber",
		"-2500.00,Initial Funding,salary," + id1 + ",",
		"75.50,Operating Expenses,groceries," + id2 + ",",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("xero body missing %q", want)
		}
	}
	if sum := hdr.Get("X-Accounting-Summary"); sum != "USD debit=2500.00 credit=75.50 count=2" {
		t.Errorf("xero summary header = %q", sum)
	}

	// QuickBooks IIF happy path.
	code, hdr, body = get(t, srv.URL+"/exports/accounting/quickbooks")
	if code != http.StatusOK {
		t.Fatalf("quickbooks: status=%d body=%s, want 200", code, body)
	}
	for _, want := range []string{
		"!TRNS\tTRNSTYPE\tDATE\tACCNT\tNAME\tAMOUNT\tDOCNUM\tMEMO",
		"TRNS\tGENERAL JOURNAL",
		"SPL\tGENERAL JOURNAL",
		"ENDTRNS",
		"\tsaver\tfunding\t2500.00\t" + id1 + "\tsalary",
		"\tfunding\tsaver\t-2500.00\t" + id1 + "\tsalary",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("iif body missing %q", want)
		}
	}
	if sum := hdr.Get("X-Accounting-Summary"); sum != "USD debit=2575.50 credit=2575.50 count=2" {
		t.Errorf("iif summary header = %q", sum)
	}

	// Bad since: 400.
	if code, _, _ := get(t, srv.URL+"/exports/accounting/xero?account=saver&since=not-a-date"); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}
	// Bad name mapping: 400.
	if code, _, _ := get(t, srv.URL+"/exports/accounting/xero?account=saver&name=nosuchformat"); code != http.StatusBadRequest {
		t.Errorf("bad name mapping: status=%d, want 400", code)
	}
	// Unknown format is a routing 404, not an export 400 — the route
	// table is the vocabulary.
	if code, _, _ := get(t, srv.URL+"/exports/accounting/sage?account=saver"); code != http.StatusNotFound {
		t.Errorf("unknown format route: status=%d, want 404", code)
	}
}
