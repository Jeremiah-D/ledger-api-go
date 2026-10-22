package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// The collateral coverage gate surfaces as 422 through HTTP: a draw
// that would leave utilization uncovered is rejected on POST
// /transfers (and its dry-run twin), with the rejection counted.
func TestHTTPCollateralRejection422(t *testing.T) {
	l := ledger.New()
	// 10 units @ 10000c, 25% haircut -> 75000c effective collateral.
	if _, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500); err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	if _, err := l.SetCollateralRequirement("alice", "USD", 12500); err != nil {
		t.Fatalf("SetCollateralRequirement: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":200000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	// A covered draw books: 250000 outflow leaves -50000 drawn,
	// needing 62500 coverage <= 75000.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"c-http-1","from_account":"alice","to_account":"bob","amount_cents":250000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("covered transfer: status=%d, want 201", code)
	}

	// An uncovered draw is 422: another 100000 leaves -150000 drawn,
	// needing 187500 coverage > 75000.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"c-http-2","from_account":"alice","to_account":"bob","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("uncovered transfer: status=%d, want 422 (body=%v)", code, body)
	}
	errMsg, _ := body["error"].(string)
	if !strings.Contains(strings.ToLower(errMsg), "collateral") {
		t.Errorf("422 body = %q, want a collateral error", errMsg)
	}

	// The dry-run twin sees the same gate.
	code, _ = postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"c-http-3","from_account":"alice","to_account":"bob","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("dry-run uncovered: status=%d, want 422", code)
	}
}
