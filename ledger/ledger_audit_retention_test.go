package ledger

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeOldAuditFile seeds dir with a genuine sealed audit event under an
// old day's filename: a throwaway log in an isolated temp dir produces
// real chained content, which is then moved to audit-<day>.jsonl(.gz) in
// dir. daysAgo=0 seeds today's file.
func makeOldAuditFile(t *testing.T, dir string, daysAgo int, gz bool) string {
	t.Helper()
	tmp := t.TempDir()
	al, err := NewAuditLog(tmp)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	al.Log(AuditEvent{Op: "post", Actor: "TestRetention", TraceID: "ret-seed",
		Details: map[string]any{"amount_cents": 100}})
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "audit-*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("seeded files = %v, %v; want exactly 1", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	day := time.Now().UTC().AddDate(0, 0, -daysAgo).Format("2006-01-02")
	dst := filepath.Join(dir, "audit-"+day+".jsonl")
	if gz {
		fh, err := os.Create(dst + ".gz")
		if err != nil {
			t.Fatalf("create gz: %v", err)
		}
		w := gzip.NewWriter(fh)
		if _, err := w.Write(data); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		if err := fh.Close(); err != nil {
			t.Fatalf("gz file close: %v", err)
		}
		return dst + ".gz"
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	return dst
}

// retentionEvent returns the first audit_retention_deleted event in
// dir's files for today, or nil.
func findRetentionEvent(t *testing.T, dir string) *AuditEvent {
	t.Helper()
	today := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(dir, today)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	for i := range events {
		if events[i].Op == "audit_retention_deleted" {
			return &events[i]
		}
	}
	return nil
}

func TestParseAuditRetentionDays(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"30", 30, false},
		{" 7 ", 7, false},
		{"abc", 0, true},
		{"1.5", 0, true},
		{"-1", 0, true},
		{"-30", 0, true},
	} {
		got, err := ParseAuditRetentionDays(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAuditRetentionDays(%q) = %d, want error", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseAuditRetentionDays(%q) = %d, %v; want %d, nil", tc.raw, got, err, tc.want)
		}
	}
}

func TestNewAuditLogRejectsNegativeRetention(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewAuditLog(dir, WithAuditLogRetentionDays(-1)); err == nil {
		t.Fatal("NewAuditLog with negative retention succeeded, want an error")
	}
	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(0))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	if got := al.RetentionDays(); got != 0 {
		t.Errorf("RetentionDays = %d, want 0", got)
	}
}

func TestAuditRetentionDeletesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	oldPlain := makeOldAuditFile(t, dir, 40, false)
	oldGz := makeOldAuditFile(t, dir, 45, true)
	recent := makeOldAuditFile(t, dir, 10, false)
	todayFile := makeOldAuditFile(t, dir, 0, false)
	// A file whose day cannot be parsed is never a deletion candidate.
	undated := filepath.Join(dir, "audit-notes.jsonl")
	if err := os.WriteFile(undated, []byte("not an audit file\n"), 0o644); err != nil {
		t.Fatalf("write undated file: %v", err)
	}

	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(30))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	deleted, err := al.ApplyAuditRetention(time.Now())
	if err != nil {
		t.Fatalf("ApplyAuditRetention = %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 files", deleted)
	}
	// The counter ticks once per deleted file.
	if got := al.RetentionDeletions(); got != 2 {
		t.Errorf("RetentionDeletions = %d, want 2", got)
	}
	// Flush the deletion event before reading the trail.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, p := range []string{oldPlain, oldGz} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after retention", p)
		}
	}
	for _, p := range []string{recent, todayFile, undated} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing after retention: %v", p, err)
		}
	}

	ev := findRetentionEvent(t, dir)
	if ev == nil {
		t.Fatal("no audit_retention_deleted event in the trail")
	}
	files, _ := ev.Details["deleted_files"].([]any)
	if len(files) != 2 {
		t.Errorf("deleted_files = %v, want 2 entries", files)
	}
	wantCutoff := time.Now().UTC().Truncate(24 * time.Hour).AddDate(0, 0, -30).Format("2006-01-02")
	if ev.Details["cutoff_day"] != wantCutoff {
		t.Errorf("cutoff_day = %v, want %q", ev.Details["cutoff_day"], wantCutoff)
	}
	if ev.Details["retention_days"] != float64(30) {
		t.Errorf("retention_days = %v, want 30", ev.Details["retention_days"])
	}
	// The cut is auditable: every deleted file's last seal is recorded.
	seals, _ := ev.Details["last_seals"].(map[string]any)
	if len(seals) != 2 {
		t.Errorf("last_seals = %v, want 2 entries", seals)
	}
	for _, s := range seals {
		if sh, ok := s.(string); !ok || len(sh) != 64 {
			t.Errorf("last_seals entry = %v, want a 64-hex seal", s)
		}
	}
}

func TestAuditRetentionCutoffBoundary(t *testing.T) {
	dir := t.TempDir()
	atCutoff := makeOldAuditFile(t, dir, 30, false) // day == cutoff: kept
	pastCutoff := makeOldAuditFile(t, dir, 31, false)

	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(30))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	deleted, err := al.ApplyAuditRetention(time.Now())
	if err != nil {
		t.Fatalf("ApplyAuditRetention = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != filepath.Base(pastCutoff) {
		t.Errorf("deleted = %v, want only %q", deleted, filepath.Base(pastCutoff))
	}
	if _, err := os.Stat(atCutoff); err != nil {
		t.Errorf("cutoff-day file was deleted: %v", err)
	}
}

func TestAuditRetentionUnlimitedKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	old := makeOldAuditFile(t, dir, 400, false)

	al, err := NewAuditLog(dir) // no retention: unlimited
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	deleted, err := al.ApplyAuditRetention(time.Now())
	if err != nil {
		t.Fatalf("ApplyAuditRetention = %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted = %v with unlimited retention, want none", deleted)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("old file deleted with unlimited retention: %v", err)
	}
	if got := al.RetentionDeletions(); got != 0 {
		t.Errorf("RetentionDeletions = %d, want 0", got)
	}
	// No sweep ran, so no deletion event is recorded either.
	if ev := findRetentionEvent(t, dir); ev != nil {
		t.Error("audit_retention_deleted event recorded with unlimited retention")
	}
}

func TestAuditRetentionEventFailureAborts(t *testing.T) {
	dir := t.TempDir()
	old := makeOldAuditFile(t, dir, 40, false)

	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(30))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	// A closed log cannot enqueue the deletion event: the sweep must
	// abort and delete nothing.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deleted, err := al.ApplyAuditRetention(time.Now())
	if err == nil {
		t.Fatal("ApplyAuditRetention on a closed log succeeded, want an error")
	}
	if len(deleted) != 0 {
		t.Errorf("deleted = %v despite the failure, want none", deleted)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("expired file was deleted despite the event failure: %v", err)
	}
	if got := al.RetentionDeletions(); got != 0 {
		t.Errorf("RetentionDeletions = %d after a failed sweep, want 0", got)
	}
}

func TestAuditRetentionIdempotent(t *testing.T) {
	dir := t.TempDir()
	makeOldAuditFile(t, dir, 40, false)

	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(30))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	first, err := al.ApplyAuditRetention(time.Now())
	if err != nil || len(first) != 1 {
		t.Fatalf("first sweep = %v, %v; want 1 file", first, err)
	}
	second, err := al.ApplyAuditRetention(time.Now())
	if err != nil {
		t.Fatalf("second sweep = %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep deleted %v, want none (idempotent)", second)
	}
	if got := al.RetentionDeletions(); got != 1 {
		t.Errorf("RetentionDeletions = %d, want 1", got)
	}
}

func TestLedgerAuditRetentionDeletions(t *testing.T) {
	// The ledger exposes the sweep count for the /metrics scrape.
	dir := t.TempDir()
	makeOldAuditFile(t, dir, 40, false)

	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(30))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	l := New(WithAuditLog(al))
	if _, err := al.ApplyAuditRetention(time.Now()); err != nil {
		t.Fatalf("ApplyAuditRetention = %v", err)
	}
	n, ok := l.AuditRetentionDeletions()
	if !ok {
		t.Fatal("AuditRetentionDeletions: log not attached")
	}
	if n != 1 {
		t.Errorf("AuditRetentionDeletions = %d, want 1", n)
	}
	// No log attached: not enabled.
	bare := New()
	if _, ok := bare.AuditRetentionDeletions(); ok {
		t.Error("AuditRetentionDeletions enabled on a ledger without an audit log")
	}
}

func TestAuditRetentionSkipsUnparseableNames(t *testing.T) {
	// Belt-and-braces: even if listAuditFiles ever returned a name
	// parseAuditFilename rejects, the sweep must not delete it. (It
	// cannot today — listAuditFiles filters — so this guards the
	// invariant from the other side: an undated file survives a sweep
	// with an aggressive retention.)
	dir := t.TempDir()
	weird := filepath.Join(dir, "audit-9999-99-99.jsonl")
	if err := os.WriteFile(weird, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	al, err := NewAuditLog(dir, WithAuditLogRetentionDays(1))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	if _, err := al.ApplyAuditRetention(time.Now()); err != nil {
		t.Fatalf("ApplyAuditRetention = %v", err)
	}
	if _, err := os.Stat(weird); err != nil {
		t.Errorf("unparseable file was deleted: %v", err)
	}
}
