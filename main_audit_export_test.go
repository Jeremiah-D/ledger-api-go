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

// getManifest fetches the raw manifest JSON from GET /audit/export and
// decodes it.
func getManifest(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return resp.StatusCode, m
}

func TestAuditExportEndpoint(t *testing.T) {
	srv, _ := auditVerifyServer(t)
	defer srv.Close()

	code, m := getManifest(t, srv.URL+"/audit/export")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/export: status=%d, want 200", code)
	}
	if m["format"] != "audit-chain-export/1" {
		t.Errorf("format = %v, want audit-chain-export/1", m["format"])
	}
	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("files = %v, want a non-empty list", m["files"])
	}
	rep, ok := m["report"].(map[string]any)
	if !ok {
		t.Fatalf("report = %v, want an object", m["report"])
	}
	if rep["ok"] != true {
		t.Errorf("report.ok = %v, want true", rep["ok"])
	}
	// Two entries: the audit_signing_disabled alert the unsigned log
	// records at construction, then the post.
	if rep["checked_entries"] != float64(2) {
		t.Errorf("checked_entries = %v, want 2", rep["checked_entries"])
	}
	// No LEDGER_AUDIT_SIGNING_KEYS in the test env: honestly unsigned.
	if m["signed"] != false {
		t.Errorf("signed = %v, want false without signing keys", m["signed"])
	}
	if m["signature"] != nil {
		t.Errorf("signature = %v, want null without signing keys", m["signature"])
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	mm := parseExposition(t, mbody)
	if mm["ledger_audit_export_total"] != 1 {
		t.Errorf("ledger_audit_export_total = %d, want 1", mm["ledger_audit_export_total"])
	}
	if mm["ledger_audit_export_breaks_total"] != 0 {
		t.Errorf("ledger_audit_export_breaks_total = %d, want 0", mm["ledger_audit_export_breaks_total"])
	}
}

func TestAuditExportEndpointDayFilter(t *testing.T) {
	srv, _ := auditVerifyServer(t)
	defer srv.Close()

	// The fixture's alert and post both land in today's file; a range
	// that excludes today exports nothing but still returns a manifest.
	code, m := getManifest(t, srv.URL+"/audit/export?from=2020-01-01&to=2020-01-02")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/export (filtered): status=%d, want 200", code)
	}
	if files := m["files"].([]any); len(files) != 0 {
		t.Errorf("files = %d, want 0 for a range with no audit files", len(files))
	}
	if m["from_day"] != "2020-01-01" || m["to_day"] != "2020-01-02" {
		t.Errorf("from_day/to_day = %v/%v, want the requested range echoed", m["from_day"], m["to_day"])
	}
}

func TestAuditExportEndpointBadDay(t *testing.T) {
	srv, _ := auditVerifyServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/audit/export?from=not-a-day")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a bad day format", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/audit/export?from=2026-10-10&to=2026-10-09")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for from_day after to_day", resp2.StatusCode)
	}
}

func TestAuditExportEndpointSigned(t *testing.T) {
	t.Setenv("LEDGER_AUDIT_SIGNING_KEYS", "k1:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	srv, _ := auditVerifyServer(t)
	defer srv.Close()

	code, m := getManifest(t, srv.URL+"/audit/export")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/export: status=%d, want 200", code)
	}
	if m["signed"] != true {
		t.Fatalf("signed = %v, want true with LEDGER_AUDIT_SIGNING_KEYS set", m["signed"])
	}
	sig, ok := m["signature"].(map[string]any)
	if !ok || sig["kid"] != "k1" {
		t.Fatalf("signature = %v, want kid k1", m["signature"])
	}
	mac, _ := sig["mac"].(string)
	if len(mac) != 64 || strings.Trim(mac, "0123456789abcdef") != "" {
		t.Errorf("signature.mac = %q, want 64 hex chars", mac)
	}
}

func TestAuditExportEndpointDetectsBreak(t *testing.T) {
	srv, dir := auditVerifyServer(t)
	defer srv.Close()

	// Tamper with the sealed post entry on disk (same technique as the
	// /audit/verify break test): the export still completes, but the
	// manifest's report shows not-ok.
	matches, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if len(matches) == 0 {
		t.Fatal("no audit file")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (alert + post)", len(lines))
	}
	var line2 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &line2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	line2["details"].(map[string]any)["amount_cents"] = float64(777)
	tampered, _ := json.Marshal(line2)
	if err := os.WriteFile(matches[0], []byte(lines[0]+"\n"+string(tampered)+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, m := getManifest(t, srv.URL+"/audit/export")
	if code != http.StatusOK {
		t.Fatalf("GET /audit/export: status=%d, want 200 (unhealthy findings are still 200)", code)
	}
	rep := m["report"].(map[string]any)
	if rep["ok"] != false {
		t.Errorf("report.ok = %v, want false after tampering", rep["ok"])
	}

	_, mbody, _ := getText(t, srv.URL+"/metrics")
	if mm := parseExposition(t, mbody); mm["ledger_audit_export_breaks_total"] != 1 {
		t.Errorf("ledger_audit_export_breaks_total = %d, want 1", mm["ledger_audit_export_breaks_total"])
	}
}

func TestAuditExportEndpointDisabled(t *testing.T) {
	// No LEDGER_AUDIT_DIR / no attached log: the endpoint 404s
	// fail-closed instead of exporting nothing.
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/audit/export")
	if err != nil {
		t.Fatalf("GET /audit/export: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the audit log is disabled", resp.StatusCode)
	}
}
