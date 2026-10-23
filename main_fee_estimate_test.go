package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func feeEstimateTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New(ledger.WithTransferFeeSchedule([]ledger.FeeTier{
		{MinAmountCents: 0, RateBps: 0},
		{MinAmountCents: 10000, RateBps: 250},
	}, "fee-revenue"))
	if err := l.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	if err := l.SetCorrespondentFee("USD", "EUR", 1500); err != nil {
		t.Fatalf("SetCorrespondentFee: %v", err)
	}
	return l
}

func TestFeeEstimateEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(feeEstimateTestLedger(t)))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfers/fee-estimate",
		`{"from_account":"alice","to_account":"bob","amount_cents":20000,"currency":"USD","to_currency":"EUR"}`)
	if code != http.StatusOK {
		t.Fatalf("fee-estimate: status=%d, want 200 (body %v)", code, body)
	}
	if body["fee_cents"] != float64(500) {
		t.Errorf("fee_cents = %v, want 500 (2.5%% tier)", body["fee_cents"])
	}
	if body["fx_converted_cents"] != float64(21600) {
		t.Errorf("fx_converted_cents = %v, want 21600", body["fx_converted_cents"])
	}
	if body["correspondent_cents"] != float64(1500) {
		t.Errorf("correspondent_cents = %v, want 1500", body["correspondent_cents"])
	}
	if body["total_cents"] != float64(2000) {
		t.Errorf("total_cents = %v, want 2000", body["total_cents"])
	}

	// The estimate booked nothing: the version is still zero and no
	// balance moved.
	code, body = postJSON(t, srv.URL+"/transfers/fee-estimate",
		`{"from_account":"alice","to_account":"bob","amount_cents":100}`)
	if code != http.StatusOK || body["fee_cents"] != float64(0) {
		t.Fatalf("dust estimate = (%d, %v), want (200, 0 fee)", code, body)
	}

	// A missing FX rate surfaces as 422, like the real submission.
	code, body = postJSON(t, srv.URL+"/transfers/fee-estimate",
		`{"from_account":"alice","to_account":"bob","amount_cents":100,"to_currency":"JPY"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-rate: status=%d, want 422 (body %v)", code, body)
	}

	// Bad fields are 400.
	code, _ = postJSON(t, srv.URL+"/transfers/fee-estimate",
		`{"from_account":"alice","to_account":"bob","amount_cents":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("zero-amount: status=%d, want 400", code)
	}
}
