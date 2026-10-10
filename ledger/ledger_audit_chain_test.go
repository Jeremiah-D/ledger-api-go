package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sealTestEvent builds a deterministic audit event for chain tests.
func sealTestEvent(op, trace string, version uint64) AuditEvent {
	return AuditEvent{
		Timestamp:     time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC),
		Op:            op,
		Actor:         "Test",
		TraceID:       trace,
		VersionBefore: version,
		VersionAfter:  version + 1,
		EntryIDs:      []string{trace + "/USD"},
		Accounts:      []AccountID{"cash", "rev"},
		Details:       map[string]any{"amount_cents": 100, "note": "chain-test"},
	}
}

// logTestEvents seals n events (op-i/trace-i) through the log. The caller
// must Close the log afterwards.
func logTestEvents(t *testing.T, al *AuditLog, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !al.Log(sealTestEvent("post", fmt.Sprintf("trace-%c", 'a'+i), uint64(i))) {
			t.Fatalf("Log event %d: dropped", i)
		}
	}
}

// auditDayFile returns the day's plain audit file path (the writer's
// current file before any rotation).
func auditDayFile(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no audit file in %s", dir)
	}
	return matches[0]
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer fh.Close()
	var lines []string
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAuditChainSealLinksEntries(t *testing.T) {
	al := mustAuditLog(t)
	logTestEvents(t, al, 3)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The writer buckets files by the event's own timestamp day, and the
	// sealTestEvent fixture pins 2026-10-09 — read that day, not "today",
	// so the test is day-boundary independent.
	day := sealTestEvent("post", "trace-a", 0).Timestamp.UTC().Format("2006-01-02")
	events, corrupt, err := ReadAuditLog(al.Dir(), day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 || len(events) != 3 {
		t.Fatalf("events=%d corrupt=%d, want 3/0", len(events), corrupt)
	}
	// Genesis links the all-zero marker; every later entry links the
	// previous entry's seal.
	if events[0].PrevHash != AuditGenesisPrevHash {
		t.Errorf("event[0].PrevHash = %q, want genesis marker", events[0].PrevHash)
	}
	for i, ev := range events {
		if len(ev.Hash) != 64 {
			t.Errorf("event[%d].Hash = %q, want 64 hex chars", i, ev.Hash)
		}
		if got := sealAuditEvent(ev); got != ev.Hash {
			t.Errorf("event[%d]: recomputed seal %q != stored %q", i, got, ev.Hash)
		}
		if i > 0 && ev.PrevHash != events[i-1].Hash {
			t.Errorf("event[%d].PrevHash does not match event[%d].Hash", i, i-1)
		}
	}

	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("VerifyAuditLog: ok=%v break=%+v, want clean", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != 3 || rep.SkippedLines != 0 || rep.FilesChecked != 1 {
		t.Errorf("report = %+v, want checked=3 skipped=0 files=1", rep)
	}
	if rep.Head != events[2].Hash {
		t.Errorf("head = %q, want last entry seal %q", rep.Head, events[2].Hash)
	}
}

func TestAuditChainVerifyDetectsTamperedContent(t *testing.T) {
	al := mustAuditLog(t)
	logTestEvents(t, al, 5)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := auditDayFile(t, al.Dir())
	lines := readLines(t, path)
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want 5", len(lines))
	}

	// Tamper with the third entry's content, keeping its seal fields:
	// the link is intact but the content no longer matches the seal.
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["details"].(map[string]any)["amount_cents"] = float64(999999)
	raw, _ := json.Marshal(m)
	lines[2] = string(raw)
	writeLines(t, path, lines)

	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if rep.Ok || rep.FirstBreak == nil {
		t.Fatalf("VerifyAuditLog: ok=%v, want a reported break", rep.Ok)
	}
	brk := rep.FirstBreak
	if brk.Reason != "hash_mismatch" {
		t.Errorf("reason = %q, want hash_mismatch", brk.Reason)
	}
	if brk.Line != 3 {
		t.Errorf("line = %d, want 3 (exact tampered line)", brk.Line)
	}
	if brk.EntryID != "trace-c" {
		t.Errorf("entry_id = %q, want trace-c", brk.EntryID)
	}
	if brk.ExpectedPrev != brk.ActualPrev {
		t.Errorf("expected_prev != actual_prev: link should be intact, content tampered")
	}
	if !strings.HasSuffix(brk.File, ".jsonl") {
		t.Errorf("file = %q, want the day's .jsonl", brk.File)
	}
	if rep.CheckedEntries != 2 {
		t.Errorf("checked_entries = %d, want 2 (verified prefix before the break)", rep.CheckedEntries)
	}
}

func TestAuditChainVerifyDetectsDeletedEntry(t *testing.T) {
	al := mustAuditLog(t)
	logTestEvents(t, al, 5)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := auditDayFile(t, al.Dir())
	lines := readLines(t, path)

	// Delete the middle entry (line 3). The entry after it still points
	// at the deleted entry's seal.
	origThird := lines[2]
	var deleted, next, prev map[string]any
	if err := json.Unmarshal([]byte(origThird), &deleted); err != nil {
		t.Fatalf("unmarshal deleted: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[3]), &next); err != nil {
		t.Fatalf("unmarshal next: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &prev); err != nil {
		t.Fatalf("unmarshal prev: %v", err)
	}
	kept := append(append([]string{}, lines[:2]...), lines[3:]...)
	writeLines(t, path, kept)

	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if rep.Ok || rep.FirstBreak == nil {
		t.Fatalf("VerifyAuditLog: ok=%v, want a reported break", rep.Ok)
	}
	brk := rep.FirstBreak
	if brk.Reason != "prev_mismatch" {
		t.Errorf("reason = %q, want prev_mismatch", brk.Reason)
	}
	if brk.Line != 3 {
		t.Errorf("line = %d, want 3 (first entry after the deletion)", brk.Line)
	}
	// The surviving entry dangles at the deleted entry's seal, while the
	// verifier expected the last intact entry's seal.
	if brk.ActualPrev != next["prev_hash"] || brk.ActualPrev != deleted["hash"] {
		t.Errorf("actual_prev = %q, want the deleted entry's seal %q", brk.ActualPrev, deleted["hash"])
	}
	if brk.ExpectedPrev != prev["hash"] {
		t.Errorf("expected_prev = %q, want the last intact entry's seal %q", brk.ExpectedPrev, prev["hash"])
	}
}

func TestAuditChainSurvivesRotation(t *testing.T) {
	// Tiny size cap: every couple of events forces a size rotation, and
	// Close waits for the background gzips to land.
	al := mustAuditLog(t, WithAuditLogMaxBytes(500))
	logTestEvents(t, al, 10)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gz, _ := filepath.Glob(filepath.Join(al.Dir(), "*.jsonl.gz"))
	if len(gz) == 0 {
		t.Fatal("want at least one gzipped rotated file")
	}
	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("VerifyAuditLog across rotations+archives: ok=%v break=%+v", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != 10 {
		t.Errorf("checked_entries = %d, want 10", rep.CheckedEntries)
	}
	if rep.FilesChecked < 2 {
		t.Errorf("files_checked = %d, want >= 2 (rotations happened)", rep.FilesChecked)
	}
	if rep.SkippedLines != 0 {
		t.Errorf("skipped_lines = %d, want 0", rep.SkippedLines)
	}
}

func TestAuditChainSkipsCorruptLines(t *testing.T) {
	al := mustAuditLog(t)
	logTestEvents(t, al, 3)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := auditDayFile(t, al.Dir())
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	// A torn/garbage line and a blank line: the garbage line is
	// disclosed as skipped, the blank line is silently ignored, and
	// neither breaks the chain.
	if _, err := fh.WriteString("this is not json\n\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	fh.Close()

	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("VerifyAuditLog: ok=false break=%+v, corrupt lines must not break the chain", rep.FirstBreak)
	}
	if rep.CheckedEntries != 3 {
		t.Errorf("checked_entries = %d, want 3", rep.CheckedEntries)
	}
	if rep.SkippedLines != 1 {
		t.Errorf("skipped_lines = %d, want 1 (the garbage line disclosed)", rep.SkippedLines)
	}
}

func TestAuditChainRestartRecoversHead(t *testing.T) {
	dir := t.TempDir()

	al1, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	logTestEvents(t, al1, 3)
	if err := al1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A new process on the same directory must continue the chain from
	// the previous run's tail instead of restarting at genesis.
	al2, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog (restart): %v", err)
	}
	logTestEvents(t, al2, 2)
	if err := al2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("VerifyAuditLog after restart: ok=%v break=%+v", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != 5 {
		t.Errorf("checked_entries = %d, want 5", rep.CheckedEntries)
	}
	if rep.FilesChecked < 2 {
		t.Errorf("files_checked = %d, want >= 2 (restart starts a fresh sequence file)", rep.FilesChecked)
	}
}

func TestAuditChainRestartRecoversHeadAcrossGzip(t *testing.T) {
	dir := t.TempDir()

	// Force the first run's file to be gzipped via a size rotation, so
	// the restart must recover the head from an archive.
	al1, err := NewAuditLog(dir, WithAuditLogMaxBytes(500))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	logTestEvents(t, al1, 4)
	if err := al1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	gz, _ := filepath.Glob(filepath.Join(dir, "*.jsonl.gz"))
	if len(gz) == 0 {
		t.Fatal("want a gzipped archive from the first run")
	}

	al2, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog (restart): %v", err)
	}
	logTestEvents(t, al2, 2)
	if err := al2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("VerifyAuditLog after restart across gzip: ok=%v break=%+v", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != 6 {
		t.Errorf("checked_entries = %d, want 6", rep.CheckedEntries)
	}
}

func TestAuditChainConcurrentSeal(t *testing.T) {
	al := mustAuditLog(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ev := sealTestEvent("post", fmt.Sprintf("trace-g%d-%d", g, i), uint64(g*50+i))
				if !al.Log(ev) {
					t.Errorf("Log dropped under concurrency")
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rep, err := VerifyAuditLog(al.Dir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil {
		t.Fatalf("VerifyAuditLog after concurrent sealing: ok=%v break=%+v", rep.Ok, rep.FirstBreak)
	}
	if rep.CheckedEntries != 400 {
		t.Errorf("checked_entries = %d, want 400 (every sealed event exactly once)", rep.CheckedEntries)
	}
}

func TestVerifyAuditLogEmptyDir(t *testing.T) {
	rep, err := VerifyAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.CheckedEntries != 0 || rep.FilesChecked != 0 {
		t.Errorf("empty dir report = %+v, want clean and empty", rep)
	}
	if rep.Head != AuditGenesisPrevHash {
		t.Errorf("head = %q, want genesis marker", rep.Head)
	}
}

func TestParseAuditFilename(t *testing.T) {
	for _, tc := range []struct {
		name string
		day  string
		seq  int
		ok   bool
	}{
		{"audit-2026-10-09.jsonl", "2026-10-09", 0, true},
		{"audit-2026-10-09-001.jsonl", "2026-10-09", 1, true},
		{"audit-2026-10-09-001.jsonl.gz", "2026-10-09", 1, true},
		{"audit-2026-10-09.jsonl.gz", "2026-10-09", 0, true},
		{"audit-2026-13-99.jsonl", "", 0, false},
		{"audit-notes.jsonl", "", 0, false},
		{"random.txt", "", 0, false},
	} {
		day, seq, ok := parseAuditFilename(tc.name)
		if day != tc.day || seq != tc.seq || ok != tc.ok {
			t.Errorf("parseAuditFilename(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tc.name, day, seq, ok, tc.day, tc.seq, tc.ok)
		}
	}
}

// TestSealIsDeterministicAcrossJSONRoundTrip guards the core invariant
// the verifier depends on: sealing the parsed form of a written line
// must reproduce the stored seal, including numeric Details values that
// cross the float64/UseNumber boundary.
func TestSealIsDeterministicAcrossJSONRoundTrip(t *testing.T) {
	ev := sealTestEvent("post", "trace-z", 7)
	ev.Details["big"] = int64(9007199254740993) // > 2^53: float64 would lose it
	ev.PrevHash = AuditGenesisPrevHash
	ev.Hash = sealAuditEvent(ev)

	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed AuditEvent
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := sealAuditEvent(parsed); got != ev.Hash {
		t.Errorf("round-trip seal mismatch: got %q want %q", got, ev.Hash)
	}
}
