package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// balanceHistoryTestServer builds a server with the history directory
// wired, and seeds it with two ticks of history for cash/USD.
func balanceHistoryTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	day := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	seed := []ledger.BalanceHistoryPoint{
		{Account: "cash", Currency: "USD", BalanceCents: 1000, Version: 5, At: day},
		{Account: "cash", Currency: "EUR", BalanceCents: 400, Version: 5, At: day},
		{Account: "cash", Currency: "USD", BalanceCents: 1500, Version: 9, At: day.Add(2 * time.Hour)},
	}
	if err := ledger.WriteBalanceHistory(dir, seed); err != nil {
		t.Fatalf("seed WriteBalanceHistory: %v", err)
	}
	srv := newServer(ledger.New())
	srv.balanceHistoryDir = dir
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, dir
}

func getBalanceHistory(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestHandleBalanceHistoryNotConfigured(t *testing.T) {
	ts := httptest.NewServer(newRouter(ledger.New()))
	defer ts.Close()

	code, body := getBalanceHistory(t, ts.URL+"/balance-history?account=cash")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 fail-closed", code)
	}
	if body["error"] == nil || body["error"] == "" {
		t.Errorf("body = %v, want an error message", body)
	}
}

func TestHandleBalanceHistory(t *testing.T) {
	ts, _ := balanceHistoryTestServer(t)

	// Full series for cash/USD: two ticks.
	code, body := getBalanceHistory(t, ts.URL+"/balance-history?account=cash&currency=USD")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	points, ok := body["points"].([]any)
	if !ok {
		t.Fatalf("points = %v (%T), want array", body["points"], body["points"])
	}
	if len(points) != 2 {
		t.Fatalf("len(points) = %d, want 2", len(points))
	}
	first := points[0].(map[string]any)
	if first["balance_cents"] != float64(1000) || first["version"] != float64(5) {
		t.Errorf("first point = %v, want 1000 cents at version 5", first)
	}
	second := points[1].(map[string]any)
	if second["balance_cents"] != float64(1500) || second["version"] != float64(9) {
		t.Errorf("second point = %v, want 1500 cents at version 9", second)
	}

	// Currency defaults to USD.
	code, body = getBalanceHistory(t, ts.URL+"/balance-history?account=cash")
	if code != http.StatusOK {
		t.Fatalf("default currency status = %d, want 200", code)
	}
	if n := len(body["points"].([]any)); n != 2 {
		t.Errorf("default currency points = %d, want 2", n)
	}

	// since/until window narrows to the second tick.
	code, body = getBalanceHistory(t,
		ts.URL+"/balance-history?account=cash&currency=USD&since=2026-10-10T13:00:00Z&until=2026-10-10T15:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("window status = %d, want 200", code)
	}
	points = body["points"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["balance_cents"] != float64(1500) {
		t.Errorf("window points = %v, want the 1500-cent tick", points)
	}

	// Unknown account: empty series, 200.
	code, body = getBalanceHistory(t, ts.URL+"/balance-history?account=nope")
	if code != http.StatusOK {
		t.Fatalf("unknown account status = %d, want 200", code)
	}
	if n := len(body["points"].([]any)); n != 0 {
		t.Errorf("unknown account points = %d, want 0", n)
	}
}

func TestHandleBalanceHistoryBadRequests(t *testing.T) {
	ts, _ := balanceHistoryTestServer(t)

	for _, url := range []string{
		ts.URL + "/balance-history",                          // missing account
		ts.URL + "/balance-history?account=",                 // empty account
		ts.URL + "/balance-history?account=cash&since=nope",  // bad since
		ts.URL + "/balance-history?account=cash&until=nope",  // bad until
		ts.URL + "/balance-history?account=cash&currency=XX", // bad currency
	} {
		code, _ := getBalanceHistory(t, url)
		if code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", url, code)
		}
	}
}

func TestBalanceHistoryConfig(t *testing.T) {
	setAll := func(dir, interval, maxAge string) {
		t.Helper()
		if dir == "" {
			t.Setenv("LEDGER_BALANCE_HISTORY_DIR", "")
		} else {
			t.Setenv("LEDGER_BALANCE_HISTORY_DIR", dir)
		}
		t.Setenv("LEDGER_BALANCE_HISTORY_INTERVAL", interval)
		t.Setenv("LEDGER_BALANCE_HISTORY_MAX_AGE_MS", maxAge)
	}

	setAll("", "", "")
	if _, ok := balanceHistoryConfig(); ok {
		t.Error("unset dir: want disabled")
	}

	setAll(t.TempDir(), "bogus", "")
	if _, ok := balanceHistoryConfig(); ok {
		t.Error("invalid interval: want disabled")
	}

	setAll(t.TempDir(), "0s", "")
	if _, ok := balanceHistoryConfig(); ok {
		t.Error("zero interval: want disabled")
	}

	dir := t.TempDir()
	setAll(dir, "5m", "")
	cfg, ok := balanceHistoryConfig()
	if !ok {
		t.Fatal("valid config: want enabled")
	}
	if cfg.Dir != dir || cfg.Interval != 5*time.Minute {
		t.Errorf("cfg = %+v, want dir %s interval 5m", cfg, dir)
	}
	if cfg.MaxAgeMs != ledger.DefaultBalanceHistoryMaxAgeMs {
		t.Errorf("MaxAgeMs = %d, want default %d", cfg.MaxAgeMs, ledger.DefaultBalanceHistoryMaxAgeMs)
	}

	setAll(dir, "5m", "bogus")
	cfg, ok = balanceHistoryConfig()
	if !ok {
		t.Fatal("invalid maxAgeMs: want still enabled")
	}
	if cfg.MaxAgeMs != ledger.DefaultBalanceHistoryMaxAgeMs {
		t.Errorf("bad maxAgeMs: MaxAgeMs = %d, want default %d", cfg.MaxAgeMs, ledger.DefaultBalanceHistoryMaxAgeMs)
	}

	setAll(dir, "5m", "3600000")
	cfg, ok = balanceHistoryConfig()
	if !ok {
		t.Fatal("maxAgeMs=3600000: want enabled")
	}
	if cfg.MaxAgeMs != 3600000 {
		t.Errorf("MaxAgeMs = %d, want 3600000", cfg.MaxAgeMs)
	}
}

func TestMetricsExposesBalanceHistorySnapshots(t *testing.T) {
	ts, _ := balanceHistoryTestServer(t)
	resp, err := http.Get(ts.URL + "/metrics")
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
	body := sb.String()
	if !strings.Contains(body, "ledger_balance_history_snapshots_total 0\n") {
		t.Errorf("/metrics missing ledger_balance_history_snapshots_total 0; body:\n%s", body)
	}
}
