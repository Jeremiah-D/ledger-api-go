package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		e := ledger.JournalEntry{
			ID:             "e-http-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "key-http-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * 24 * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	return httptest.NewServer(newRouter(l))
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
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

func TestHandleSnapshot(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/cash/snapshot")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["account"] != "cash" {
		t.Errorf("account = %v, want cash", body["account"])
	}
	if body["balance_cents"] != float64(300) {
		t.Errorf("balance_cents = %v, want 300", body["balance_cents"])
	}
	if body["version"] != float64(3) {
		t.Errorf("version = %v, want 3", body["version"])
	}

	// Unknown account snapshots to zero at the same ledger version.
	_, body = getJSON(t, srv.URL+"/accounts/nobody/snapshot")
	if body["balance_cents"] != float64(0) || body["version"] != float64(3) {
		t.Errorf("unknown account snapshot = %v, want balance 0 version 3", body)
	}
}

func TestHandleTrialBalance(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// The seed posts debit cash / credit equity 100 × 3.
	code, body := getJSON(t, srv.URL+"/accounts/cash/trial-balance")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["account"] != "cash" {
		t.Errorf("account = %v, want cash", body["account"])
	}
	if body["total_debits_cents"] != float64(300) {
		t.Errorf("total_debits_cents = %v, want 300", body["total_debits_cents"])
	}
	if body["total_credits_cents"] != float64(0) {
		t.Errorf("total_credits_cents = %v, want 0", body["total_credits_cents"])
	}
	if body["net_balance_cents"] != float64(300) {
		t.Errorf("net_balance_cents = %v, want 300", body["net_balance_cents"])
	}
	if body["version"] != float64(3) {
		t.Errorf("version = %v, want 3", body["version"])
	}

	// The credit side mirrors: equity was credited 300 across the same posts.
	_, body = getJSON(t, srv.URL+"/accounts/equity/trial-balance")
	if body["total_debits_cents"] != float64(0) ||
		body["total_credits_cents"] != float64(300) ||
		body["net_balance_cents"] != float64(-300) {
		t.Errorf("equity trial balance = %v, want debits 0 credits 300 net -300", body)
	}

	// Unknown accounts report zero totals at the current ledger version.
	_, body = getJSON(t, srv.URL+"/accounts/nobody/trial-balance")
	if body["total_debits_cents"] != float64(0) ||
		body["total_credits_cents"] != float64(0) ||
		body["net_balance_cents"] != float64(0) ||
		body["version"] != float64(3) {
		t.Errorf("unknown account trial balance = %v, want zeros version 3", body)
	}
}

func TestHandleListEntries(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	type listResp struct {
		Entries    []map[string]any `json:"entries"`
		NextCursor string           `json:"next_cursor"`
	}
	fetch := func(t *testing.T, url string) (int, listResp) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		var lr listResp
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
		return resp.StatusCode, lr
	}

	// Full export, no params.
	code, lr := fetch(t, srv.URL+"/entries")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(lr.Entries) != 3 || lr.NextCursor != "" {
		t.Fatalf("full export = %d entries cursor %q, want 3 entries empty cursor", len(lr.Entries), lr.NextCursor)
	}
	if lr.Entries[0]["id"] != "e-http-0" || lr.Entries[2]["id"] != "e-http-2" {
		t.Fatalf("entries not in time order: %v", lr.Entries)
	}

	// Two pages of limit=2 walk every entry exactly once.
	var ids []string
	cursor := ""
	for {
		_, lr = fetch(t, srv.URL+"/entries?limit=2&cursor="+cursor)
		for _, e := range lr.Entries {
			ids = append(ids, e["id"].(string))
		}
		if lr.NextCursor == "" {
			break
		}
		cursor = lr.NextCursor
	}
	if len(ids) != 3 || ids[0] != "e-http-0" || ids[1] != "e-http-1" || ids[2] != "e-http-2" {
		t.Fatalf("paginated walk = %v", ids)
	}

	// Time window filtering.
	_, lr = fetch(t, srv.URL+"/entries?since=2026-10-02T00:00:00Z&until=2026-10-03T00:00:00Z")
	if len(lr.Entries) != 1 || lr.Entries[0]["id"] != "e-http-1" {
		t.Fatalf("windowed export = %v, want [e-http-1]", lr.Entries)
	}

	// Malformed inputs are 400.
	for _, url := range []string{
		srv.URL + "/entries?since=not-a-time",
		srv.URL + "/entries?until=2026-13-99T00:00:00Z",
		srv.URL + "/entries?limit=banana",
		srv.URL + "/entries?limit=0",
		srv.URL + "/entries?cursor=!!!",
	} {
		if code, _ := getJSON(t, url); code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", url, code)
		}
	}
}

func postEntry(t *testing.T, url, raw string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode POST %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestHandleCreateEntryStrictDecoding(t *testing.T) {
	valid := `{"debit_account":"cash","credit_account":"equity","amount_cents":100,"idempotency_key":"strict-1"}`

	t.Run("valid body is accepted", func(t *testing.T) {
		srv := httptest.NewServer(newRouter(ledger.New()))
		defer srv.Close()
		code, body := postEntry(t, srv.URL+"/entries", valid)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		if body["id"] == nil || body["id"] == "" {
			t.Errorf("response missing generated id: %v", body)
		}
	})

	t.Run("unknown field is rejected with the field named", func(t *testing.T) {
		srv := httptest.NewServer(newRouter(ledger.New()))
		defer srv.Close()
		raw := `{"debit_account":"cash","credit_account":"equity","amount_cents":100,"idempotency_key":"strict-2","amout_cents":100}`
		code, body := postEntry(t, srv.URL+"/entries", raw)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, "amout_cents") {
			t.Errorf("error %q does not name the offending field", msg)
		}
	})

	t.Run("trailing garbage is rejected", func(t *testing.T) {
		srv := httptest.NewServer(newRouter(ledger.New()))
		defer srv.Close()
		code, _ := postEntry(t, srv.URL+"/entries", valid+`{"injected":true}`)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("oversized body is rejected with 413", func(t *testing.T) {
		t.Setenv("LEDGER_MAX_BODY_BYTES", "64")
		srv := httptest.NewServer(newRouter(ledger.New()))
		defer srv.Close()
		// 64 bytes is far below even the valid entry payload.
		code, body := postEntry(t, srv.URL+"/entries", valid)
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", code)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "too large") {
			t.Errorf("error %q does not mention size", msg)
		}
	})
}

func TestServerReportSweeperHandle(t *testing.T) {
	// main.go stores the started report sweeper on srv at startup
	// (LG-316) so existing operator paths can retune the live scan
	// interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.reportSweeper != nil {
		t.Fatalf("reportSweeper = %v, want nil before startup", srv.reportSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartReportSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.reportSweeper = sw

	prev, err := srv.reportSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.reportSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.reportSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.reportSweeper = %v, want 20ms", got)
	}
}

func TestServerScheduleSweeperHandle(t *testing.T) {
	// main.go stores the started schedule sweeper on srv at startup
	// (LG-317) so existing operator paths can retune the live scan
	// interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.scheduleSweeper != nil {
		t.Fatalf("scheduleSweeper = %v, want nil before startup", srv.scheduleSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartScheduleSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.scheduleSweeper = sw

	prev, err := srv.scheduleSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.scheduleSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.scheduleSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.scheduleSweeper = %v, want 20ms", got)
	}
}

func TestServerHoldSweeperHandle(t *testing.T) {
	// main.go stores the started hold sweeper on srv at startup
	// (LG-318) so existing operator paths can retune the live scan
	// interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.holdSweeper != nil {
		t.Fatalf("holdSweeper = %v, want nil before startup", srv.holdSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartHoldSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.holdSweeper = sw

	prev, err := srv.holdSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.holdSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.holdSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.holdSweeper = %v, want 20ms", got)
	}
}

func TestServerFreezeSweeperHandle(t *testing.T) {
	// main.go stores the started freeze sweeper on srv at startup
	// (LG-319) so existing operator paths can retune the live scan
	// interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.freezeSweeper != nil {
		t.Fatalf("freezeSweeper = %v, want nil before startup", srv.freezeSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartFreezeSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.freezeSweeper = sw

	prev, err := srv.freezeSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.freezeSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.freezeSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.freezeSweeper = %v, want 20ms", got)
	}
}

func TestServerPeriodAutoCloseSweeperHandle(t *testing.T) {
	// main.go stores the started period auto-close sweeper on srv at
	// startup (LG-320) so existing operator paths can retune the live
	// scan interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.periodAutoCloseSweeper != nil {
		t.Fatalf("periodAutoCloseSweeper = %v, want nil before startup", srv.periodAutoCloseSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartPeriodAutoCloseSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.periodAutoCloseSweeper = sw

	prev, err := srv.periodAutoCloseSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.periodAutoCloseSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.periodAutoCloseSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.periodAutoCloseSweeper = %v, want 20ms", got)
	}
}

func TestServerReviewSweeperHandle(t *testing.T) {
	// main.go stores the started review-expiry sweeper on srv at
	// startup (LG-321) so existing operator paths can retune the live
	// scan interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.reviewSweeper != nil {
		t.Fatalf("reviewSweeper = %v, want nil before startup", srv.reviewSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartReviewSweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.reviewSweeper = sw

	prev, err := srv.reviewSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.reviewSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.reviewSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.reviewSweeper = %v, want 20ms", got)
	}
}

func TestServerSLASweeperHandle(t *testing.T) {
	// main.go stores the started SLA sweeper on srv at startup
	// (LG-322) so existing operator paths can retune the live
	// evaluation interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.slaSweeper != nil {
		t.Fatalf("slaSweeper = %v, want nil before startup", srv.slaSweeper)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := ledger.StartSLASweeper(ctx, srv.ledger, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	srv.slaSweeper = sw

	prev, err := srv.slaSweeper.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.slaSweeper: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.slaSweeper.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.slaSweeper = %v, want 20ms", got)
	}
}

func TestServerFXRateWatchHandle(t *testing.T) {
	// main.go stores the started FX rate watch on srv at startup
	// (LG-323) so existing operator paths can revise the live
	// stale-warning threshold in place instead of restarting the
	// server. The same handle carries the scan interval revision
	// (LG-324).
	srv := &server{ledger: ledger.New()}
	if srv.fxRateWatch != nil {
		t.Fatalf("fxRateWatch = %v, want nil before startup", srv.fxRateWatch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := ledger.StartFXRateWatch(ctx, srv.ledger, 500*time.Millisecond, 72*time.Hour, nil)
	if w == nil {
		t.Fatalf("watch not started")
	}
	defer w.Stop()
	srv.fxRateWatch = w

	prev, err := srv.fxRateWatch.SetWarningThreshold(time.Hour)
	if err != nil {
		t.Fatalf("SetWarningThreshold through srv.fxRateWatch: %v", err)
	}
	if prev != 72*time.Hour {
		t.Fatalf("returned previous threshold = %v, want 72h", prev)
	}
	if got := srv.fxRateWatch.WarningThreshold(); got != time.Hour {
		t.Fatalf("WarningThreshold() through srv.fxRateWatch = %v, want 1h", got)
	}

	prev, err = srv.fxRateWatch.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.fxRateWatch: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.fxRateWatch.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.fxRateWatch = %v, want 20ms", got)
	}
}

func TestServerFXConsistencyWatchHandle(t *testing.T) {
	// main.go stores the started FX consistency watch on srv at
	// startup (LG-325) so existing operator paths can revise the
	// live triangular-consistency threshold in place instead of
	// restarting the server. The same handle carries the scan
	// interval revision (LG-326).
	srv := &server{ledger: ledger.New()}
	if srv.fxConsistencyWatch != nil {
		t.Fatalf("fxConsistencyWatch = %v, want nil before startup", srv.fxConsistencyWatch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := ledger.StartFXConsistencyWatch(ctx, srv.ledger, 500*time.Millisecond, 50, nil)
	if w == nil {
		t.Fatalf("watch not started")
	}
	defer w.Stop()
	srv.fxConsistencyWatch = w

	prev, err := srv.fxConsistencyWatch.SetThresholdBps(25)
	if err != nil {
		t.Fatalf("SetThresholdBps through srv.fxConsistencyWatch: %v", err)
	}
	if prev != 50 {
		t.Fatalf("returned previous threshold = %d, want 50", prev)
	}
	if got := srv.fxConsistencyWatch.ThresholdBps(); got != 25 {
		t.Fatalf("ThresholdBps() through srv.fxConsistencyWatch = %d, want 25", got)
	}

	prevInterval, err := srv.fxConsistencyWatch.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.fxConsistencyWatch: %v", err)
	}
	if prevInterval != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prevInterval)
	}
	if got := srv.fxConsistencyWatch.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.fxConsistencyWatch = %v, want 20ms", got)
	}
}

func TestServerHealthWatchHandle(t *testing.T) {
	// main.go stores the started balance-health watchdog on srv at
	// startup (LG-327) so existing operator paths can revise the
	// live scan interval in place instead of restarting the server.
	srv := &server{ledger: ledger.New()}
	if srv.healthWatch != nil {
		t.Fatalf("healthWatch = %v, want nil before startup", srv.healthWatch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hw := ledger.StartHealthWatch(ctx, srv.ledger, 500*time.Millisecond, nil)
	if hw == nil {
		t.Fatalf("watch not started")
	}
	defer hw.Stop()
	srv.healthWatch = hw

	prev, err := srv.healthWatch.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.healthWatch: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.healthWatch.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.healthWatch = %v, want 20ms", got)
	}
}

func TestServerTSAAnchorerHandle(t *testing.T) {
	// main.go stores the started TSA anchoring worker on srv at
	// startup (LG-328) so existing operator paths can revise the
	// live anchoring interval in place instead of restarting the
	// server. No tick fires during this test: the 500ms cadence
	// outlives it and the worker is stopped at the end.
	srv := &server{ledger: ledger.New()}
	if srv.tsaAnchorer != nil {
		t.Fatalf("tsaAnchorer = %v, want nil before startup", srv.tsaAnchorer)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := ledger.StartTimestampAnchoring(ctx, srv.ledger, 500*time.Millisecond, &ledger.RFC3161Client{URL: "http://127.0.0.1:1"}, nil)
	if a == nil {
		t.Fatalf("anchorer not started")
	}
	defer a.Stop()
	srv.tsaAnchorer = a

	prev, err := srv.tsaAnchorer.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.tsaAnchorer: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.tsaAnchorer.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.tsaAnchorer = %v, want 20ms", got)
	}
}

func TestServerSnapshotBackupHandle(t *testing.T) {
	// main.go stores the started snapshot backup worker on srv at
	// startup (LG-329) so existing operator paths can revise the
	// live backup interval in place instead of restarting the
	// server. No tick fires during this test: the 500ms cadence
	// outlives it and the worker is stopped at the end.
	srv := &server{ledger: ledger.New()}
	if srv.snapshotBackup != nil {
		t.Fatalf("snapshotBackup = %v, want nil before startup", srv.snapshotBackup)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bk := ledger.StartSnapshotBackup(ctx, srv.ledger, ledger.SnapshotBackupConfig{
		Dir: t.TempDir(), Interval: 500 * time.Millisecond,
	}, nil)
	if bk == nil {
		t.Fatalf("backup worker not started")
	}
	defer bk.Stop()
	srv.snapshotBackup = bk

	prev, err := srv.snapshotBackup.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.snapshotBackup: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.snapshotBackup.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.snapshotBackup = %v, want 20ms", got)
	}
}

func TestServerSnapshotBackupRetentionHandle(t *testing.T) {
	// The same srv handle (LG-329) also carries the live retention
	// depths (LG-332): an operator revises them in place through
	// srv.snapshotBackup instead of restarting the server. No tick
	// fires during this test: the 500ms cadence outlives it and the
	// worker is stopped at the end.
	srv := &server{ledger: ledger.New()}
	if srv.snapshotBackup != nil {
		t.Fatalf("snapshotBackup = %v, want nil before startup", srv.snapshotBackup)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bk := ledger.StartSnapshotBackup(ctx, srv.ledger, ledger.SnapshotBackupConfig{
		Dir: t.TempDir(), Interval: 500 * time.Millisecond,
	}, nil)
	if bk == nil {
		t.Fatalf("backup worker not started")
	}
	defer bk.Stop()
	srv.snapshotBackup = bk

	// No KeepFull / KeepIncremental configured, so the live depths
	// start at the defaults (7 full, 24 incremental).
	if got := srv.snapshotBackup.KeepFull(); got != 7 {
		t.Fatalf("KeepFull() through srv.snapshotBackup = %d, want 7", got)
	}
	if got := srv.snapshotBackup.KeepIncremental(); got != 24 {
		t.Fatalf("KeepIncremental() through srv.snapshotBackup = %d, want 24", got)
	}
	prev, err := srv.snapshotBackup.SetKeepFull(3)
	if err != nil {
		t.Fatalf("SetKeepFull through srv.snapshotBackup: %v", err)
	}
	if prev != 7 {
		t.Fatalf("returned previous keep-full = %d, want 7", prev)
	}
	if got := srv.snapshotBackup.KeepFull(); got != 3 {
		t.Fatalf("KeepFull() through srv.snapshotBackup = %d, want 3", got)
	}
	prev, err = srv.snapshotBackup.SetKeepIncremental(72)
	if err != nil {
		t.Fatalf("SetKeepIncremental through srv.snapshotBackup: %v", err)
	}
	if prev != 24 {
		t.Fatalf("returned previous keep-incremental = %d, want 24", prev)
	}
	if got := srv.snapshotBackup.KeepIncremental(); got != 72 {
		t.Fatalf("KeepIncremental() through srv.snapshotBackup = %d, want 72", got)
	}
}

func TestServerBalanceHistoryRecorderHandle(t *testing.T) {
	// main.go stores the started balance-history recorder on srv at
	// startup (LG-330) so existing operator paths can revise the
	// live sampling interval in place instead of restarting the
	// server. No tick fires during this test: the 500ms cadence
	// outlives it and the recorder is stopped at the end.
	srv := &server{ledger: ledger.New()}
	if srv.balanceHistoryRecorder != nil {
		t.Fatalf("balanceHistoryRecorder = %v, want nil before startup", srv.balanceHistoryRecorder)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := ledger.StartBalanceHistoryRecorder(ctx, srv.ledger, ledger.BalanceHistoryConfig{
		Dir: t.TempDir(), Interval: 500 * time.Millisecond,
	}, nil)
	if rec == nil {
		t.Fatalf("balance-history recorder not started")
	}
	defer rec.Stop()
	srv.balanceHistoryRecorder = rec

	prev, err := srv.balanceHistoryRecorder.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval through srv.balanceHistoryRecorder: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := srv.balanceHistoryRecorder.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() through srv.balanceHistoryRecorder = %v, want 20ms", got)
	}
}

func TestServerBalanceHistoryRecorderMaxAgeHandle(t *testing.T) {
	// The same srv handle (LG-330) also carries the live retention
	// window (LG-331): an operator revises it in place through
	// srv.balanceHistoryRecorder instead of restarting the server.
	// No tick fires during this test: the 500ms cadence outlives it
	// and the recorder is stopped at the end.
	srv := &server{ledger: ledger.New()}
	if srv.balanceHistoryRecorder != nil {
		t.Fatalf("balanceHistoryRecorder = %v, want nil before startup", srv.balanceHistoryRecorder)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := ledger.StartBalanceHistoryRecorder(ctx, srv.ledger, ledger.BalanceHistoryConfig{
		Dir: t.TempDir(), Interval: 500 * time.Millisecond,
	}, nil)
	if rec == nil {
		t.Fatalf("balance-history recorder not started")
	}
	defer rec.Stop()
	srv.balanceHistoryRecorder = rec

	// No MaxAgeMs configured, so the live window starts at the
	// 90-day default.
	if got := srv.balanceHistoryRecorder.MaxAge(); got != 90*24*time.Hour {
		t.Fatalf("MaxAge() through srv.balanceHistoryRecorder = %v, want 90d", got)
	}
	prev, err := srv.balanceHistoryRecorder.SetMaxAge(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("SetMaxAge through srv.balanceHistoryRecorder: %v", err)
	}
	if prev != 90*24*time.Hour {
		t.Fatalf("returned previous window = %v, want 90d", prev)
	}
	if got := srv.balanceHistoryRecorder.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() through srv.balanceHistoryRecorder = %v, want 30d", got)
	}
}
