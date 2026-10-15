package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedCreditLineServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(
		ledger.WithCreditInterestIncome("interest-income"),
		ledger.WithOverdraftProtection("trader"),
	)
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-trader", DebitAccount: "trader", CreditAccount: "equity",
		AmountCents: 100_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPCreditLineFlow(t *testing.T) {
	srv := seedCreditLineServer(t)
	defer srv.Close()

	// Grant a line.
	code, body := postJSON(t, srv.URL+"/credit-lines",
		`{"account":"trader","limit_cents":50000,"currency":"USD","rate_bps":800}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /credit-lines status = %d, want 201 (body %v)", code, body)
	}
	if body["limit_cents"] != float64(50_000) {
		t.Errorf("body = %v, want limit_cents 50000", body)
	}

	// Re-granting the same account+currency is 409.
	code, _ = postJSON(t, srv.URL+"/credit-lines",
		`{"account":"trader","limit_cents":60000,"currency":"USD","rate_bps":800}`)
	if code != http.StatusConflict {
		t.Errorf("re-grant status = %d, want 409", code)
	}

	// Query the line: utilization shows the full line available.
	code, line := getJSON(t, srv.URL+"/credit-lines?account=trader&currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /credit-lines status = %d", code)
	}
	if line["limit_cents"] != float64(50_000) || line["used_cents"] != float64(0) ||
		line["available_cents"] != float64(50_000) {
		t.Errorf("line = %v, want 50000/0/50000", line)
	}

	// Unknown line is 404.
	code, _ = getJSON(t, srv.URL+"/credit-lines?account=nobody&currency=USD")
	if code != http.StatusNotFound {
		t.Errorf("unknown line status = %d, want 404", code)
	}

	// Draw into the line: the journal accepts balance -40_000.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"vendor","credit_account":"trader","amount_cents":140000}`)
	if code != http.StatusCreated {
		t.Fatalf("draw status = %d", code)
	}

	// Utilization reflects the draw.
	code, line = getJSON(t, srv.URL+"/credit-lines?account=trader&currency=USD")
	if line["used_cents"] != float64(40_000) || line["available_cents"] != float64(10_000) {
		t.Errorf("line = %v, want used 40000 / available 10000", line)
	}

	// Breaching the floor is 422.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"vendor","credit_account":"trader","amount_cents":10001}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("breach status = %d, want 422", code)
	}

	// Accrue interest on the utilization.
	code, entry := postJSON(t, srv.URL+"/credit-lines/accrue-interest?account=trader&currency=USD&asof=2026-04-02T09:00:00Z", "")
	if code != http.StatusCreated {
		t.Fatalf("accrue-interest status = %d, want 201 (body %v)", code, entry)
	}
	if entry["id"] != "credit-interest/trader/USD/2026-04-02" {
		t.Errorf("interest entry id = %v, want deterministic ID", entry["id"])
	}

	// Reducing below the utilization is 422.
	code, _ = postJSON(t, srv.URL+"/credit-lines/reduce",
		`{"account":"trader","currency":"USD","limit_cents":30000}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("shrink-below-use status = %d, want 422", code)
	}

	// Reducing to the current utilization (40_000 draw + 8 interest)
	// succeeds.
	code, body = postJSON(t, srv.URL+"/credit-lines/reduce",
		`{"account":"trader","currency":"USD","limit_cents":40008}`)
	if code != http.StatusOK {
		t.Errorf("reduce status = %d, want 200", code)
	}

	// Bad grant payload is 400.
	code, _ = postJSON(t, srv.URL+"/credit-lines",
		`{"account":"trader","limit_cents":0,"currency":"USD","rate_bps":800}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad grant status = %d, want 400", code)
	}
}

func TestHTTPCreditLineRevoke(t *testing.T) {
	srv := seedCreditLineServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/credit-lines",
		`{"account":"trader","limit_cents":50000,"currency":"USD","rate_bps":800}`)
	if code != http.StatusCreated {
		t.Fatalf("grant status = %d", code)
	}
	// Zero utilization: revoke by reducing to 0.
	code, _ = postJSON(t, srv.URL+"/credit-lines/reduce",
		`{"account":"trader","currency":"USD","limit_cents":0}`)
	if code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", code)
	}
	code, _ = getJSON(t, srv.URL+"/credit-lines?account=trader&currency=USD")
	if code != http.StatusNotFound {
		t.Errorf("get after revoke status = %d, want 404", code)
	}
}
