package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPostEntry(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  "alice",
		CreditAccount: "bob",
		AmountCents:   100,
		Currency:      "USD",
	}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

// testBackupWorker builds a backup worker with a long ticker interval so
// the goroutine never fires during the test; tests drive backupOnce
// directly for determinism.
func testBackupWorker(t *testing.T, l *Ledger, dir string, cfg SnapshotBackupConfig) *SnapshotBackup {
	t.Helper()
	cfg.Dir = dir
	cfg.Interval = time.Hour
	sw := StartSnapshotBackup(context.Background(), l, cfg, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	t.Cleanup(sw.Stop)
	return sw
}

func listBackupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// readMetaLine parses the first JSONL line of a snapshot file.
func readMetaLine(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("meta line: %v", err)
	}
	return m
}

func TestSnapshotBackupFullThenIncremental(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	mustPostEntry(t, l, "e2")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 3, MaxRetries: 1,
	})

	// First tick is a full backup (every FullEvery-th starting at 1).
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backupOnce = (%q, %v), want (full, true)", kind, ok)
	}
	files := listBackupFiles(t, dir)
	if len(files) != 1 || !strings.HasPrefix(files[0], "snapshot-full-") {
		t.Fatalf("files = %v, want one snapshot-full-*", files)
	}
	meta := readMetaLine(t, filepath.Join(dir, files[0]))
	if meta["record"] != "meta" || int(meta["version"].(float64)) != 2 {
		t.Fatalf("full meta = %v, want record=meta version=2", meta)
	}

	// Second tick is incremental from version 2.
	mustPostEntry(t, l, "e3")
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backupOnce = (%q, %v), want (incremental, true)", kind, ok)
	}
	files = listBackupFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("files = %v, want full + incremental", files)
	}
	var incr string
	for _, f := range files {
		if strings.HasPrefix(f, "snapshot-incr-") {
			incr = f
		}
	}
	if incr == "" {
		t.Fatalf("no incremental file in %v", files)
	}
	meta = readMetaLine(t, filepath.Join(dir, incr))
	if meta["base_version"].(float64) != 2 || meta["version"].(float64) != 3 {
		t.Fatalf("incremental meta = %v, want base_version=2 version=3", meta)
	}
}

func TestSnapshotBackupRetention(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 2, KeepIncremental: 1, FullEvery: 2, MaxRetries: 1,
	})
	// Ticks: full, incr, full, incr.
	for i := 0; i < 4; i++ {
		mustPostEntry(t, l, fmt.Sprintf("tick-%d", i))
		if _, ok := sw.backupOnce(); !ok {
			t.Fatalf("tick %d failed", i)
		}
	}
	var fulls, incrs int
	for _, f := range listBackupFiles(t, dir) {
		switch {
		case strings.HasPrefix(f, "snapshot-full-"):
			fulls++
		case strings.HasPrefix(f, "snapshot-incr-"):
			incrs++
		}
	}
	if fulls != 2 || incrs != 1 {
		t.Fatalf("retention: fulls=%d incrs=%d, want 2/1", fulls, incrs)
	}
}

func TestSnapshotBackupAtomicWrite(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{MaxRetries: 1})
	if _, ok := sw.backupOnce(); !ok {
		t.Fatal("backupOnce failed")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".snapshot-tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSnapshotBackupFailureIsCounted(t *testing.T) {
	l := New()
	// A regular file where the directory should be: CreateTemp fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotKind string
	var gotOk = true
	sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{
		Dir: blocker, Interval: time.Hour, MaxRetries: 1,
	}, func(kind string, ok bool) {
		gotKind, gotOk = kind, ok
	})
	if sw == nil {
		t.Fatal("nil worker")
	}
	defer sw.Stop()
	kind, ok := sw.backupOnce()
	if ok {
		t.Fatal("backupOnce succeeded against a file-as-dir")
	}
	if kind != "full" {
		t.Fatalf("kind = %q, want full (first tick)", kind)
	}
	// Drive the callback path the same way the goroutine does.
	sw.onBackup(kind, ok)
	if gotKind != "full" || gotOk {
		t.Fatalf("onBackup = (%q, %v), want (full, false)", gotKind, gotOk)
	}
}

func TestSnapshotBackupAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	before, _, _ := l.AuditStats()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{MaxRetries: 1})
	if _, ok := sw.backupOnce(); !ok {
		t.Fatal("backupOnce failed")
	}
	// Flush the async writer before asserting.
	time.Sleep(500 * time.Millisecond)
	after, _, _ := l.AuditStats()
	if after <= before {
		t.Fatalf("audit events: before=%d after=%d, want an increase", before, after)
	}
	// The backup event is in the audit files with op snapshot_backup.
	found := false
	entries, _ := os.ReadDir(al.Dir())
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(al.Dir(), e.Name()))
		if strings.Contains(string(data), `"op":"snapshot_backup"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no snapshot_backup op in audit log files")
	}
}

func TestSnapshotBackupDisabled(t *testing.T) {
	l := New()
	if sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{Dir: ""}, nil); sw != nil {
		t.Fatal("empty Dir should disable the worker")
	}
	if sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{Dir: t.TempDir()}, nil); sw != nil {
		t.Fatal("zero Interval should disable the worker")
	}
}

func TestSnapshotBackupFilenameOrdering(t *testing.T) {
	a := snapshotBackupFilename("full", 0, 7, time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC))
	b := snapshotBackupFilename("full", 0, 8, time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC))
	if !(a < b) {
		t.Fatalf("filenames not chronological: %q >= %q", a, b)
	}
	c := snapshotBackupFilename("incremental", 7, 9, time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(c, "snapshot-incr-") || !strings.HasSuffix(c, ".jsonl") {
		t.Fatalf("bad incremental filename: %q", c)
	}
}

func TestSnapshotBackupDefaults(t *testing.T) {
	cfg := SnapshotBackupConfig{}.withDefaults()
	if cfg.KeepFull != DefaultSnapshotBackupKeepFull ||
		cfg.KeepIncremental != DefaultSnapshotBackupKeepIncremental ||
		cfg.FullEvery != DefaultSnapshotBackupFullEvery ||
		cfg.MaxRetries != DefaultSnapshotBackupMaxRetries {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}
