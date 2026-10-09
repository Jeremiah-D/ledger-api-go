package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// auditVerifyServer builds a server with the audit log enabled, posts one
// entry, flushes the log, and returns the server plus the audit dir.
func auditVerifyServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al))
	srv := httptest.NewServer(newRouter(l))

	if code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"cash","credit_account":"rev","amount_cents":100}`); code != http.StatusCreated {
		t.Fatalf("POST /entries: status=%d, want 201", code)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return srv, dir
}

func TestAuditVerifyEndpoint(t *testing.T) {
	srv, _ := auditVerifyServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/audit/verify")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/verify: status=%d, want 200", code)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true (body: %v)", body["ok"], body)
	}
	if body["checked_entries"] != float64(1) {
		t.Errorf("checked_entries = %v, want 1", body["checked_entries"])
	}
	if body["first_break"] != nil {
		t.Errorf("first_break = %v, want null", body["first_break"])
	}
	if body["skipped_lines"] != float64(0) {
		t.Errorf("skipped_lines = %v, want 0", body["skipped_lines"])
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, mbody)
	if m["ledger_audit_verify_total"] != 1 {
		t.Errorf("ledger_audit_verify_total = %d, want 1", m["ledger_audit_verify_total"])
	}
	if m["ledger_audit_verify_breaks_total"] != 0 {
		t.Errorf("ledger_audit_verify_breaks_total = %d, want 0", m["ledger_audit_verify_breaks_total"])
	}
}

func TestAuditVerifyEndpointDetectsBreak(t *testing.T) {
	srv, dir := auditVerifyServer(t)
	defer srv.Close()

	// Tamper with the sealed entry on disk: rewrite its amount while
	// keeping the stored seal, so verification reports a hash_mismatch.
	matches, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if len(matches) == 0 {
		t.Fatal("no audit file")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["details"].(map[string]any)["amount_cents"] = float64(777)
	tampered, _ := json.Marshal(m)
	if err := os.WriteFile(matches[0], append(tampered, '\n'), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, body := getJSON(t, srv.URL+"/audit/verify")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/verify: status=%d, want 200", code)
	}
	if body["ok"] != false {
		t.Errorf("ok = %v, want false after tampering", body["ok"])
	}
	brk, ok := body["first_break"].(map[string]any)
	if !ok || brk == nil {
		t.Fatalf("first_break missing: %v", body["first_break"])
	}
	if brk["line"] != float64(1) || brk["reason"] != "hash_mismatch" {
		t.Errorf("first_break = %v, want line=1 reason=hash_mismatch", brk)
	}
	if _, ok := brk["expected_prev"]; !ok {
		t.Errorf("first_break missing expected_prev: %v", brk)
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	if m := parseExposition(t, mbody); m["ledger_audit_verify_breaks_total"] != 1 {
		t.Errorf("ledger_audit_verify_breaks_total = %d, want 1", m["ledger_audit_verify_breaks_total"])
	}
}

func TestAuditVerifyEndpointDisabled(t *testing.T) {
	// No LEDGER_AUDIT_DIR / no attached log: the endpoint 404s
	// fail-closed instead of reporting a vacuous "ok".
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/audit/verify")
	if err != nil {
		t.Fatalf("GET /audit/verify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the audit log is disabled", resp.StatusCode)
	}
}
