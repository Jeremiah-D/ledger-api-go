package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedValueDateServer builds a server with a funded payer for the
// value-date endpoints.
func seedValueDateServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-payer", DebitAccount: "payer", CreditAccount: "equity",
		AmountCents: 50000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

// nextWeekday returns the next occurrence of wd after now (UTC), at
// noon, so convention tests never depend on the run day.
func nextWeekday(wd time.Weekday) time.Time {
	now := time.Now().UTC()
	d := (int(wd) - int(now.Weekday()) + 7) % 7
	if d == 0 {
		d = 7
	}
	return time.Date(now.Year(), now.Month(), now.Day()+d, 12, 0, 0, 0, time.UTC)
}

func TestHTTPValueDatePost(t *testing.T) {
	srv := seedValueDateServer(t)
	defer srv.Close()

	// An explicit value date on a business day: 201, and the entry
	// echoes it.
	monday := nextWeekday(time.Monday).Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date":"`+monday+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201 (body %v)", code, body)
	}
	if body["value_date"] == nil {
		t.Errorf("entry does not echo value_date: %v", body)
	}

	// Saturday + following: 201, and the journaled value date is the
	// following Monday.
	sat := nextWeekday(time.Saturday)
	wantMonday := sat.AddDate(0, 0, 2).Format(time.RFC3339)
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date":"`+sat.Format(time.RFC3339)+`","value_date_convention":"following"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries following status = %d, want 201 (body %v)", code, body)
	}
	if body["value_date"] != wantMonday {
		t.Errorf("value_date = %v, want Monday %v", body["value_date"], wantMonday)
	}

	// A value date 60 days out: 422, fail-closed.
	far := time.Now().UTC().Add(60 * 24 * time.Hour).Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date":"`+far+`"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("window violation status = %d, want 422", code)
	}

	// An unknown convention: 422.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date":"`+monday+`","value_date_convention":"nearest"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bad convention status = %d, want 422", code)
	}

	// A convention with no value date: 422.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date_convention":"following"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("convention-without-date status = %d, want 422", code)
	}
}

func TestHTTPValueDateDryRun(t *testing.T) {
	srv := seedValueDateServer(t)
	defer srv.Close()

	// The dry run resolves the same convention the real call would
	// journal, and journals nothing itself.
	sat := nextWeekday(time.Saturday)
	wantMonday := sat.AddDate(0, 0, 2).Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/entries/dry-run",
		`{"debit_account":"payer","credit_account":"payee","amount_cents":100,"currency":"USD","value_date":"`+sat.Format(time.RFC3339)+`","value_date_convention":"following"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /entries/dry-run status = %d, want 200 (body %v)", code, body)
	}
	legs, _ := body["legs"].([]interface{})
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}
	leg, _ := legs[0].(map[string]interface{})
	if leg["value_date"] != wantMonday {
		t.Errorf("dry-run leg value_date = %v, want Monday %v", leg["value_date"], wantMonday)
	}
}
