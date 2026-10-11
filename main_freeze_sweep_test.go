package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestFreezeEndpointAcceptsUntil(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/accounts/cash/freeze", `{"until":`+strconv.Quote(until)+`}`)
	if code != http.StatusOK {
		t.Fatalf("freeze with until: status=%d, want 200 (body=%v)", code, body)
	}
	if body["frozen"] != true {
		t.Fatalf("freeze with until: body=%v, want frozen=true", body)
	}
	got, ok := body["auto_unfreeze_at"].(string)
	if !ok || got == "" {
		t.Fatalf("freeze with until: body=%v, want auto_unfreeze_at echoed", body)
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("auto_unfreeze_at = %q, want RFC3339: %v", got, err)
	}
}

func TestFreezeEndpointRejectsPastUntil(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, body := postJSON(t, srv.URL+"/accounts/cash/freeze", `{"until":`+strconv.Quote(past)+`}`)
	if code != http.StatusBadRequest {
		t.Fatalf("freeze with past until: status=%d, want 400 (body=%v)", code, body)
	}

	code, body = postJSON(t, srv.URL+"/accounts/cash/freeze", `{"until":"not-a-time"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("freeze with malformed until: status=%d, want 400 (body=%v)", code, body)
	}
}

func TestFreezeEndpointEmptyBodyIsManual(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/accounts/cash/freeze", "")
	if code != http.StatusOK || body["frozen"] != true {
		t.Fatalf("freeze without body: status=%d body=%v, want 200 frozen=true", code, body)
	}
	if _, ok := body["auto_unfreeze_at"]; ok {
		t.Fatalf("freeze without body: body=%v, want no auto_unfreeze_at", body)
	}
	if _, ok := l.FreezeAutoUnfreezeAt("cash"); ok {
		t.Fatal("bodyless freeze registered a deadline: must stay manual for backward compatibility")
	}
}

func TestFreezeMetricsCountsAutoRelease(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	until := time.Now().Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	if code, _ := postJSON(t, srv.URL+"/accounts/cash/freeze", `{"until":`+strconv.Quote(until)+`}`); code != http.StatusOK {
		t.Fatalf("freeze with until: status=%d, want 200", code)
	}
	time.Sleep(200 * time.Millisecond)

	// The write path releases the expired freeze lazily; the post succeeds.
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("post after deadline: status=%d, want success (lazy unfreeze)", code)
	}

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
	if !strings.Contains(sb.String(), "ledger_freeze_auto_releases_total 1\n") {
		t.Fatalf("/metrics missing ledger_freeze_auto_releases_total 1:\n%s", sb.String())
	}
}
