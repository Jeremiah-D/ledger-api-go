package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestChartDirectionPostReturns422AndCountsMetric(t *testing.T) {
	l := ledger.New()
	if err := l.RegisterAccountType("cash", ledger.AccountTypeAsset); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Crediting an asset account violates the chart's posting direction.
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"supply","credit_account":"cash","amount_cents":1000}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("chart-direction post: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "chart-of-accounts") {
		t.Fatalf("422 body = %v, want a chart-of-accounts error", body)
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
	if !strings.Contains(sb.String(), "ledger_chart_direction_rejections_total 1") {
		t.Fatalf("/metrics missing chart-direction counter 1:\n%s", sb.String())
	}
}
