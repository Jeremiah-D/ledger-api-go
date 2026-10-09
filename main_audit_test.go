package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestAuditLogEndToEnd(t *testing.T) {
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// A mutating op and a reconcile run through HTTP.
	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"rev","amount_cents":100}`); code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
	if code, _ := postJSON(t, srv.URL+"/merges",
		`{"merge_id":"mg1","from_account":"cash","to_account":"treasury"}`); code != http.StatusCreated {
		t.Fatalf("POST /merges: status=%d, want 201", code)
	}
	if code, _ := postJSON(t, srv.URL+"/reconcile", `{}`); code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d, want 200", code)
	}
	// Reads add nothing.
	if code, _ := getJSON(t, srv.URL+"/accounts/treasury/balance"); code != http.StatusOK {
		t.Fatalf("GET /balance: status=%d, want 200", code)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ledger.ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	ops := map[string]int{}
	for _, ev := range events {
		ops[ev.Op]++
	}
	for op, want := range map[string]int{"post": 1, "merge": 1, "reconcile": 1} {
		if ops[op] != want {
			t.Errorf("op %q: got %d events, want %d (all ops: %v)", op, ops[op], want, ops)
		}
	}

	// The audit counters are synced into /metrics on scrape.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, want := range []string{"ledger_audit_events_total 3", "ledger_audit_dropped_total 0"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestAuditMaxBytesEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", ledger.DefaultAuditMaxBytes},
		{"1048576", 1048576},
		{"garbage", ledger.DefaultAuditMaxBytes},
		{"0", ledger.DefaultAuditMaxBytes},
		{"-5", ledger.DefaultAuditMaxBytes},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv("LEDGER_AUDIT_MAX_BYTES", tc.value)
			if got := auditMaxBytes(); got != tc.want {
				t.Errorf("auditMaxBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}
