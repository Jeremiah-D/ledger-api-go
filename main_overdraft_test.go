package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestOverdraftPostReturns422AndCountsMetric(t *testing.T) {
	l := ledger.New(ledger.WithOverdraftProtection("equity"))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1000}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("overdraw post: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "overdraw") {
		t.Fatalf("422 body = %v, want an overdraft error", body)
	}

	// The rejection is counted in the Prometheus exposition.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_overdraft_rejections_total 1") {
		t.Fatalf("/metrics missing overdraft counter 1:\n%s", sb.String())
	}
}

func TestOverdraftPostStillSucceedsWhenFunded(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Fund equity, then protect it via the ledger API (the HTTP layer
	// exposes no operator endpoint for this; protection is set at
	// startup via LEDGER_NO_OVERDRAFT_ACCOUNTS).
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"equity","credit_account":"funding","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}
	l.EnableOverdraftProtection("equity")

	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("exact-balance post: status=%d, want 201", code)
	}

	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("overdraw post: status=%d, want 422", code)
	}
}
