package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPAmendCollateralHaircut(t *testing.T) {
	l := ledger.New()
	if _, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500); err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Happy path: the haircut moves, everything else stays.
	code, body := postJSON(t, srv.URL+"/collateral/alice/bond-1/haircut",
		`{"haircut_bps":5000}`)
	if code != http.StatusOK {
		t.Fatalf("amend haircut: status=%d body=%v, want 200", code, body)
	}
	if body["haircut_bps"] != float64(5000) || body["quantity"] != float64(10) ||
		body["unit_value_cents"] != float64(10000) || body["asset_id"] != "bond-1" {
		t.Fatalf("amend body = %v, want haircut 5000 / quantity 10 / value 10000 / bond-1", body)
	}

	// Same-as-current value: no-op, still 200.
	code, _ = postJSON(t, srv.URL+"/collateral/alice/bond-1/haircut",
		`{"haircut_bps":5000}`)
	if code != http.StatusOK {
		t.Fatalf("no-op amend: status=%d, want 200", code)
	}

	// Haircut outside 0..10000: 422.
	code, _ = postJSON(t, srv.URL+"/collateral/alice/bond-1/haircut",
		`{"haircut_bps":10001}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("amend 10001: status=%d, want 422", code)
	}

	// Unknown pledge: 404.
	code, _ = postJSON(t, srv.URL+"/collateral/alice/no-such-asset/haircut",
		`{"haircut_bps":5000}`)
	if code != http.StatusNotFound {
		t.Fatalf("amend unknown: status=%d, want 404", code)
	}
}
