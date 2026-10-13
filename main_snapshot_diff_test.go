package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// writeBackupFull exports l into dir under the backup worker's filename
// convention.
func writeBackupFull(t *testing.T, dir string, l *ledger.Ledger) uint64 {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	v := l.Version()
	name := fmt.Sprintf("snapshot-full-v%020d-20261012T000000Z.jsonl", v)
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return v
}

var mainDiffPostSeq int

func mustPostMain(t *testing.T, l *ledger.Ledger, debit, credit string, cents int64) {
	t.Helper()
	mainDiffPostSeq++
	if _, _, err := l.Post(ledger.JournalEntry{
		ID:            fmt.Sprintf("main-diff-e%d", mainDiffPostSeq),
		DebitAccount:  ledger.AccountID(debit),
		CreditAccount: ledger.AccountID(credit),
		AmountCents:   cents,
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
}

func TestSnapshotDiffEndpoint(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	mustPostMain(t, l, "acct-a", "acct-b", 1000)
	l.Freeze("acct-f")
	v2 := writeBackupFull(t, dir, l)

	mustPostMain(t, l, "acct-a", "acct-c", 250)
	l.Unfreeze("acct-f")
	v5 := writeBackupFull(t, dir, l)

	srv := newServer(ledger.New())
	srv.snapshotBackupDir = dir
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(fmt.Sprintf("%s/snapshots/diff?from=%d&to=%d", ts.URL, v2, v5))
	if err != nil {
		t.Fatalf("GET /snapshots/diff: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var diff struct {
		FromVersion uint64 `json:"from_version"`
		ToVersion   uint64 `json:"to_version"`
		Balances    struct {
			Added   []map[string]any `json:"added"`
			Deleted []map[string]any `json:"deleted"`
			Changed []map[string]any `json:"changed"`
		} `json:"balances"`
		Entries struct {
			Added   []map[string]any `json:"added"`
			Deleted []map[string]any `json:"deleted"`
			Changed []map[string]any `json:"changed"`
		} `json:"entries"`
		Frozen struct {
			Added   []map[string]any `json:"added"`
			Deleted []map[string]any `json:"deleted"`
			Changed []map[string]any `json:"changed"`
		} `json:"frozen"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&diff); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if diff.FromVersion != v2 || diff.ToVersion != v5 {
		t.Fatalf("versions = %d->%d, want %d->%d", diff.FromVersion, diff.ToVersion, v2, v5)
	}
	if len(diff.Entries.Added) != 1 {
		t.Fatalf("entries.added = %d, want 1", len(diff.Entries.Added))
	}
	if len(diff.Balances.Changed) == 0 {
		t.Fatalf("balances.changed empty, want acct-a movement")
	}
	if len(diff.Frozen.Deleted) != 1 || diff.Frozen.Deleted[0]["account"] != "acct-f" {
		t.Fatalf("frozen.deleted = %+v, want acct-f", diff.Frozen.Deleted)
	}

	// The diff counted exactly one served request.
	mresp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	var mbody bytes.Buffer
	mbody.ReadFrom(mresp.Body)
	if !bytes.Contains(mbody.Bytes(), []byte("ledger_snapshot_diffs_total 1\n")) {
		t.Fatalf("metrics missing ledger_snapshot_diffs_total 1:\n%s", mbody.String())
	}
}

func TestSnapshotDiffEndpointErrors(t *testing.T) {
	dir := t.TempDir()
	l := ledger.New()
	mustPostMain(t, l, "acct-a", "acct-b", 100)
	v2 := writeBackupFull(t, dir, l)

	srv := newServer(ledger.New())
	srv.snapshotBackupDir = dir
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"missing from", "/snapshots/diff?to=2", http.StatusBadRequest},
		{"missing to", "/snapshots/diff?from=2", http.StatusBadRequest},
		{"non-numeric from", "/snapshots/diff?from=abc&to=2", http.StatusBadRequest},
		{"unknown version", fmt.Sprintf("/snapshots/diff?from=%d&to=99", v2), http.StatusNotFound},
		{"version below every full", "/snapshots/diff?from=1&to=2", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s: status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
		})
	}

	// Unconfigured backup directory: 404 fail-closed.
	srv2 := newServer(ledger.New())
	ts2 := httptest.NewServer(srv2.handler())
	defer ts2.Close()
	resp, err := http.Get(ts2.URL + "/snapshots/diff?from=1&to=2")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured dir: status = %d, want 404", resp.StatusCode)
	}
}

// writeBackupFullEncrypted exports l, seals it under the key ring (LG-77),
// and writes it under the backup worker's filename convention.
func writeBackupFullEncrypted(t *testing.T, dir string, l *ledger.Ledger, keys ledger.BackupEncryptionConfig) uint64 {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	sealed, err := ledger.EncryptSnapshotBackup(buf.Bytes(), keys)
	if err != nil {
		t.Fatalf("EncryptSnapshotBackup: %v", err)
	}
	v := l.Version()
	name := fmt.Sprintf("snapshot-full-v%020d-20261012T000000Z.jsonl", v)
	if err := os.WriteFile(filepath.Join(dir, name), sealed, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return v
}

func TestSnapshotDiffEndpointEncrypted(t *testing.T) {
	const keyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	keys, err := ledger.ParseBackupEncryptionKeys("k1:" + keyHex)
	if err != nil {
		t.Fatalf("ParseBackupEncryptionKeys: %v", err)
	}

	dir := t.TempDir()
	l := ledger.New()
	mustPostMain(t, l, "acct-a", "acct-b", 1000)
	v1 := writeBackupFullEncrypted(t, dir, l, keys)
	mustPostMain(t, l, "acct-a", "acct-c", 250)
	v2 := writeBackupFullEncrypted(t, dir, l, keys)

	srv := newServer(ledger.New())
	srv.snapshotBackupDir = dir
	srv.backupKeys = keys.Keys
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Encrypted backups decrypt transparently for the diff endpoint.
	resp, err := http.Get(fmt.Sprintf("%s/snapshots/diff?from=%d&to=%d", ts.URL, v1, v2))
	if err != nil {
		t.Fatalf("GET /snapshots/diff: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var diff struct {
		Entries struct {
			Added []map[string]any `json:"added"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&diff); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(diff.Entries.Added) != 1 {
		t.Fatalf("entries.added = %d, want 1", len(diff.Entries.Added))
	}

	// Without the key the encrypted backups fail closed: no key ring
	// means the envelope cannot open, and the diff refuses (422), not
	// a half-decrypted 200.
	srv2 := newServer(ledger.New())
	srv2.snapshotBackupDir = dir
	ts2 := httptest.NewServer(srv2.handler())
	defer ts2.Close()
	resp2, err := http.Get(fmt.Sprintf("%s/snapshots/diff?from=%d&to=%d", ts2.URL, v1, v2))
	if err != nil {
		t.Fatalf("GET /snapshots/diff: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no-key diff: status = %d, want 422", resp2.StatusCode)
	}
}
