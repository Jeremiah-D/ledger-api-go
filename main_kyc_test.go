package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestKycSetGetLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Set a verified profile with a risk tier and an expiry.
	code, body := postJSON(t, srv.URL+"/accounts/acme/kyc",
		`{"status":"verified","risk_tier":"low","expires_at":"2030-01-01T00:00:00Z"}`)
	if code != 200 {
		t.Fatalf("set kyc: status=%d body=%v, want 200", code, body)
	}
	if body["status"] != "verified" || body["risk_tier"] != "low" {
		t.Fatalf("set kyc body = %v, want verified/low", body)
	}

	// GET returns the stored profile.
	code, body = getJSON(t, srv.URL+"/accounts/acme/kyc")
	if code != 200 {
		t.Fatalf("get kyc: status=%d body=%v, want 200", code, body)
	}
	if body["status"] != "verified" || body["account"] != "acme" {
		t.Fatalf("get kyc body = %v, want acme/verified", body)
	}

	// Unknown account: 404.
	code, _ = getJSON(t, srv.URL+"/accounts/nobody/kyc")
	if code != 404 {
		t.Fatalf("get kyc unknown: status=%d, want 404", code)
	}

	// Invalid status: 400.
	code, _ = postJSON(t, srv.URL+"/accounts/acme/kyc", `{"status":"gold"}`)
	if code != 400 {
		t.Fatalf("set kyc bad status: status=%d, want 400", code)
	}
	// The derived expired state cannot be set by hand: 400.
	code, _ = postJSON(t, srv.URL+"/accounts/acme/kyc", `{"status":"expired"}`)
	if code != 400 {
		t.Fatalf("set kyc expired: status=%d, want 400", code)
	}
}

func TestKycExpiredBlocksPostingHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Install a profile that already lapsed.
	code, _ := postJSON(t, srv.URL+"/accounts/cash/kyc",
		`{"status":"verified","expires_at":"2020-01-01T00:00:00Z"}`)
	if code != 200 {
		t.Fatalf("set kyc: status=%d, want 200", code)
	}

	// The write path's lazy sweep expires it: even a 1-cent post is
	// rejected with 422 and books nothing.
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":1}`)
	if code != 422 {
		t.Fatalf("post through expired KYC: status=%d body=%v, want 422", code, body)
	}
	if !strings.Contains(body["error"].(string), "KYC expired") {
		t.Fatalf("422 body = %v, want a KYC-expired error", body)
	}

	// The rejection is counted on /metrics (Prometheus exposition).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(metricsBytes), "ledger_kyc_rejections_total 1") {
		t.Fatalf("/metrics lacks ledger_kyc_rejections_total 1:\n%s", metricsBytes)
	}
}

func TestKycExpireSweepHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postJSON(t, srv.URL+"/accounts/a/kyc",
		`{"status":"verified","expires_at":"2020-01-01T00:00:00Z"}`)
	postJSON(t, srv.URL+"/accounts/b/kyc",
		`{"status":"verified","expires_at":"2030-01-01T00:00:00Z"}`)

	code, body := postJSON(t, srv.URL+"/kyc/expire", "")
	if code != 200 {
		t.Fatalf("expire kyc: status=%d, want 200", code)
	}
	if body["expired"] != float64(1) {
		t.Fatalf("expire kyc body = %v, want expired=1", body)
	}

	// The swept account now reports expired.
	_, body = getJSON(t, srv.URL+"/accounts/a/kyc")
	if body["status"] != "expired" {
		t.Fatalf("get kyc a = %v, want expired", body)
	}
	_, body = getJSON(t, srv.URL+"/accounts/b/kyc")
	if body["status"] != "verified" {
		t.Fatalf("get kyc b = %v, want verified (untouched)", body)
	}
}
