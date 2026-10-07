package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func postJSON(t *testing.T, url, payload string) (int, map[string]any) {
	t.Helper()
	var resp *http.Response
	var err error
	if payload == "" {
		resp, err = http.Post(url, "application/json", nil)
	} else {
		resp, err = http.Post(url, "application/json", strings.NewReader(payload))
	}
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestFreezeUnfreezeLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Freeze is idempotent and reports the new state.
	code, body := postJSON(t, srv.URL+"/accounts/cash/freeze", "")
	if code != http.StatusOK || body["frozen"] != true {
		t.Fatalf("freeze: status=%d body=%v, want 200 frozen=true", code, body)
	}

	// A post through the frozen account is rejected with 403 and books
	// nothing.
	code, body = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusForbidden {
		t.Fatalf("post through frozen account: status=%d, want 403", code)
	}
	if !strings.Contains(body["error"].(string), "frozen") {
		t.Fatalf("403 body = %v, want a frozen-account error", body)
	}

	// Read endpoints keep working and surface the frozen flag.
	_, body = getJSON(t, srv.URL+"/accounts/cash/balance")
	if body["balance_cents"] != float64(0) || body["frozen"] != true {
		t.Errorf("balance on frozen account = %v, want 0 cents and frozen=true", body)
	}
	_, body = getJSON(t, srv.URL+"/accounts/cash/snapshot")
	if body["frozen"] != true || body["version"] != float64(0) {
		t.Errorf("snapshot on frozen account = %v, want frozen=true version=0", body)
	}
	_, body = getJSON(t, srv.URL+"/accounts/cash/trial-balance")
	if body["frozen"] != true {
		t.Errorf("trial-balance on frozen account = %v, want frozen=true", body)
	}

	// Unfreeze restores posting.
	code, body = postJSON(t, srv.URL+"/accounts/cash/unfreeze", "")
	if code != http.StatusOK || body["frozen"] != false {
		t.Fatalf("unfreeze: status=%d body=%v, want 200 frozen=false", code, body)
	}
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("post after unfreeze: status=%d, want 201", code)
	}

	// The frozen rejection was counted in /metrics.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_frozen_rejections_total 1\n") {
		t.Errorf("/metrics missing ledger_frozen_rejections_total 1:\n%s", sb.String())
	}
}

func TestFreezeEndpointValidation(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Method guard: GET on the freeze endpoint is 405.
	resp, err := http.Get(srv.URL + "/accounts/cash/freeze")
	if err != nil {
		t.Fatalf("GET freeze: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET freeze: status=%d, want 405", resp.StatusCode)
	}

	// A post naming the frozen account as the *credit* leg is rejected too.
	if code, _ := postJSON(t, srv.URL+"/accounts/equity/freeze", ""); code != http.StatusOK {
		t.Fatalf("freeze equity: status=%d, want 200", code)
	}
	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"equity","amount_cents":50}`)
	if code != http.StatusForbidden {
		t.Errorf("post with frozen credit leg: status=%d, want 403", code)
	}

	// Unrelated accounts are unaffected by another account's freeze.
	code, _ = postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"revenue","amount_cents":50}`)
	if code != http.StatusCreated {
		t.Errorf("post on unfrozen accounts: status=%d, want 201", code)
	}
}
