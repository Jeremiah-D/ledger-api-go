package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// retuneTestLedger builds a ledger with an audit log in a fresh temp
// dir, returning the ledger, the log, and the dir (for file assertions).
func retuneTestLedger(t *testing.T, opts ...AuditLogOption) (*Ledger, *AuditLog, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir, opts...)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return New(WithAuditLog(al)), al, dir
}

// todayAuditDay is the UTC day string audit files are named under.
func todayAuditDay() string { return time.Now().UTC().Format("2006-01-02") }

// rotatedAuditFiles returns the day's rotated (sequence >= 001) audit
// files, plain or gzipped.
func rotatedAuditFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "audit-"+todayAuditDay()+"-*.jsonl*"))
	if err != nil {
		t.Fatalf("Glob rotated files: %v", err)
	}
	return matches
}

// waitAuditWritten polls until the log has flushed at least n events
// (the background writer is async, so a test that must order a
// revision after earlier events have hit disk waits on the written
// counter instead of sleeping).
func waitAuditWritten(t *testing.T, al *AuditLog, n uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if w, _ := al.Stats(); w >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w, _ := al.Stats(); w < n {
		t.Fatalf("audit log wrote %d events, want at least %d within 10s", w, n)
	}
}

// TestSetAuditLogMaxBytesTightenRotatesOnNextWrite pins the live
// maxBytes semantics: tightening the cap does not force an immediate
// rotation, but the next written event sees curSize >= new cap and
// rotates to a fresh sequence file.
func TestSetAuditLogMaxBytesTightenRotatesOnNextWrite(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	// One event on top of the construction-time signing-disabled alert,
	// so the base file holds more than the tightened cap.
	al.Log(AuditEvent{Op: "post", Actor: "TestRetune", TraceID: "retune-1",
		Details: map[string]any{"amount_cents": 100}})
	if err := l.SetAuditLogMaxBytes(64); err != nil {
		t.Fatalf("SetAuditLogMaxBytes(64): %v", err)
	}
	if got, ok := l.AuditLogMaxBytes(); !ok || got != 64 {
		t.Fatalf("AuditLogMaxBytes() = %d, %v; want 64, true", got, ok)
	}
	// The next write — not the setter itself — must rotate.
	al.Log(AuditEvent{Op: "post", Actor: "TestRetune", TraceID: "retune-2",
		Details: map[string]any{"amount_cents": 200}})
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// At least one rotation must have happened: under the pre-revision
	// 100 MiB cap no rotation was possible, so any rotated file proves
	// the tightened cap took effect on a write after the revision.
	// (With a 64-byte cap every post-revision write rotates — one
	// event alone exceeds the cap — so the exact count is
	// event-count-dependent and only the lower bound is pinned.)
	if rotated := rotatedAuditFiles(t, dir); len(rotated) < 1 {
		t.Fatalf("rotated files = %v, want at least 1 after tightening the cap", rotated)
	}
	// Nothing was lost across the rotations: alert + 2 posts +
	// amendment, all readable, none corrupt.
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (alert + 2 posts + amendment)", len(events))
	}
	byOp := auditEventsByOp(t, dir)
	amended := byOp["audit_log_max_bytes_amended"]
	if len(amended) != 1 {
		t.Fatalf("audit_log_max_bytes_amended events = %d, want 1", len(amended))
	}
	if amended[0].Details["previous_max_bytes"] != float64(DefaultAuditMaxBytes) {
		t.Errorf("previous_max_bytes = %v, want %d", amended[0].Details["previous_max_bytes"], DefaultAuditMaxBytes)
	}
	if amended[0].Details["max_bytes"] != float64(64) {
		t.Errorf("max_bytes = %v, want 64", amended[0].Details["max_bytes"])
	}
}

// TestSetAuditLogMaxBytesLoosenKeepsCurrentFile pins the other side of
// the live cap: after loosening, further writes keep appending to the
// current file up to the new cap instead of rotating again.
func TestSetAuditLogMaxBytesLoosenKeepsCurrentFile(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogMaxBytes(64))
	// Two events under the tiny cap force rotations on the way: the
	// construction alert lands in the base file and each following
	// event sees curSize >= 64 and rotates.
	al.Log(AuditEvent{Op: "post", Actor: "TestRetune", TraceID: "retune-1",
		Details: map[string]any{"amount_cents": 100}})
	al.Log(AuditEvent{Op: "post", Actor: "TestRetune", TraceID: "retune-2",
		Details: map[string]any{"amount_cents": 200}})
	// Drain the queue first: the writer is async, and loosening before
	// it has flushed the pre-loosen events would let them land under
	// the new cap instead of rotating.
	waitAuditWritten(t, al, 3)
	rotatedBefore := len(rotatedAuditFiles(t, dir))
	if rotatedBefore < 1 {
		t.Fatalf("rotated files = %d before loosening, want at least 1", rotatedBefore)
	}
	if err := l.SetAuditLogMaxBytes(DefaultAuditMaxBytes); err != nil {
		t.Fatalf("SetAuditLogMaxBytes(default): %v", err)
	}
	for i := 0; i < 3; i++ {
		al.Log(AuditEvent{Op: "post", Actor: "TestRetune", TraceID: fmt.Sprintf("retune-loose-%d", i),
			Details: map[string]any{"amount_cents": 100 + i}})
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// No new rotation after loosening: the three later writes plus the
	// amendment event all appended to the current file.
	if rotated := rotatedAuditFiles(t, dir); len(rotated) != rotatedBefore {
		t.Fatalf("rotated files = %d after loosening, want %d (no new rotation)", len(rotated), rotatedBefore)
	}
	if got, ok := l.AuditLogMaxBytes(); !ok || got != DefaultAuditMaxBytes {
		t.Fatalf("AuditLogMaxBytes() = %d, %v; want %d, true", got, ok, DefaultAuditMaxBytes)
	}
	// Nothing was lost: alert + 2 posts + amendment + 3 posts.
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 7 {
		t.Fatalf("events = %d, want 7", len(events))
	}
}

// TestSetAuditLogMaxBytesRejectsNonPositive pins the runtime gate: at
// runtime there is no default to fall back to, so a non-positive cap
// is an error and the old cap is left untouched.
func TestSetAuditLogMaxBytesRejectsNonPositive(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	defer al.Close()
	for _, n := range []int64{0, -1, -1 << 40} {
		if err := l.SetAuditLogMaxBytes(n); !errors.Is(err, ErrInvalidAuditLogMaxBytes) {
			t.Errorf("SetAuditLogMaxBytes(%d) = %v, want ErrInvalidAuditLogMaxBytes", n, err)
		}
	}
	if got, ok := l.AuditLogMaxBytes(); !ok || got != DefaultAuditMaxBytes {
		t.Errorf("AuditLogMaxBytes() = %d, %v; want %d, true (unchanged)", got, ok, DefaultAuditMaxBytes)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if byOp := auditEventsByOp(t, dir); len(byOp["audit_log_max_bytes_amended"]) != 0 {
		t.Errorf("rejected revisions wrote %d amendment events, want 0", len(byOp["audit_log_max_bytes_amended"]))
	}
}

// TestSetAuditLogRetentionDaysSweepsExpiredFiles pins the live
// retention semantics: tightening the window runs a sweep synchronously
// inside the setter, deleting whole files older than the new cutoff,
// and the deletion event carries the new window.
func TestSetAuditLogRetentionDaysSweepsExpiredFiles(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	oldPlain := makeOldAuditFile(t, dir, 10, false)
	oldGz := makeOldAuditFile(t, dir, 12, true)
	if err := l.SetAuditLogRetentionDays(7); err != nil {
		t.Fatalf("SetAuditLogRetentionDays(7): %v", err)
	}
	for _, p := range []string{oldPlain, oldGz} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expired file %s survived the setter sweep (err %v)", p, err)
		}
	}
	if got, ok := l.AuditLogRetentionDays(); !ok || got != 7 {
		t.Fatalf("AuditLogRetentionDays() = %d, %v; want 7, true", got, ok)
	}
	if d, ok := l.AuditRetentionDeletions(); !ok || d != 2 {
		t.Fatalf("AuditRetentionDeletions() = %d, %v; want 2, true", d, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ev := findRetentionEvent(t, dir)
	if ev == nil {
		t.Fatal("no audit_retention_deleted event after the setter sweep")
	}
	if ev.Details["retention_days"] != float64(7) {
		t.Errorf("retention event retention_days = %v, want 7 (the new window)", ev.Details["retention_days"])
	}
	deleted, _ := ev.Details["deleted_files"].([]any)
	if len(deleted) != 2 {
		t.Errorf("retention event deleted_files = %v, want 2 entries", ev.Details["deleted_files"])
	}
	byOp := auditEventsByOp(t, dir)
	amended := byOp["audit_log_retention_days_amended"]
	if len(amended) != 1 {
		t.Fatalf("audit_log_retention_days_amended events = %d, want 1", len(amended))
	}
	if amended[0].Details["previous_retention_days"] != float64(0) {
		t.Errorf("previous_retention_days = %v, want 0", amended[0].Details["previous_retention_days"])
	}
	if amended[0].Details["retention_days"] != float64(7) {
		t.Errorf("retention_days = %v, want 7", amended[0].Details["retention_days"])
	}
}

// TestSetAuditLogRetentionDaysRejectsNegative pins the runtime gate:
// a negative window is an error and the old window is left untouched.
func TestSetAuditLogRetentionDaysRejectsNegative(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogRetentionDays(30))
	defer al.Close()
	for _, n := range []int{-1, -30} {
		if err := l.SetAuditLogRetentionDays(n); !errors.Is(err, ErrInvalidAuditLogRetentionDays) {
			t.Errorf("SetAuditLogRetentionDays(%d) = %v, want ErrInvalidAuditLogRetentionDays", n, err)
		}
	}
	if got, ok := l.AuditLogRetentionDays(); !ok || got != 30 {
		t.Errorf("AuditLogRetentionDays() = %d, %v; want 30, true (unchanged)", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if byOp := auditEventsByOp(t, dir); len(byOp["audit_log_retention_days_amended"]) != 0 {
		t.Errorf("rejected revisions wrote %d amendment events, want 0", len(byOp["audit_log_retention_days_amended"]))
	}
}

// TestSetAuditLogRetentionDaysZeroRestoresUnlimited pins that revising
// back to 0 restores unlimited retention: the setter's synchronous
// sweep then deletes nothing.
func TestSetAuditLogRetentionDaysZeroRestoresUnlimited(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogRetentionDays(7))
	old := makeOldAuditFile(t, dir, 10, false)
	if err := l.SetAuditLogRetentionDays(0); err != nil {
		t.Fatalf("SetAuditLogRetentionDays(0): %v", err)
	}
	if got, ok := l.AuditLogRetentionDays(); !ok || got != 0 {
		t.Fatalf("AuditLogRetentionDays() = %d, %v; want 0, true", got, ok)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("old file %s was deleted after restoring unlimited retention: %v", old, err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ev := findRetentionEvent(t, dir); ev != nil {
		t.Errorf("unexpected audit_retention_deleted event after restoring unlimited retention: %+v", ev)
	}
}

// TestSetAuditLogWithoutAuditLog pins the not-configured error: a
// ledger with no audit log attached rejects both revisions, and both
// getters report disabled — the mutation form of AuditStats'
// enabled=false.
func TestSetAuditLogWithoutAuditLog(t *testing.T) {
	l := New()
	if err := l.SetAuditLogMaxBytes(1024); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Errorf("SetAuditLogMaxBytes without log = %v, want ErrAuditLogNotConfigured", err)
	}
	if err := l.SetAuditLogMaxBytesCtx(context.Background(), 1024); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Errorf("SetAuditLogMaxBytesCtx without log = %v, want ErrAuditLogNotConfigured", err)
	}
	if err := l.SetAuditLogRetentionDays(7); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Errorf("SetAuditLogRetentionDays without log = %v, want ErrAuditLogNotConfigured", err)
	}
	if err := l.SetAuditLogRetentionDaysCtx(context.Background(), 7); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Errorf("SetAuditLogRetentionDaysCtx without log = %v, want ErrAuditLogNotConfigured", err)
	}
	if got, ok := l.AuditLogMaxBytes(); ok || got != 0 {
		t.Errorf("AuditLogMaxBytes() = %d, %v; want 0, false", got, ok)
	}
	if got, ok := l.AuditLogRetentionDays(); ok || got != 0 {
		t.Errorf("AuditLogRetentionDays() = %d, %v; want 0, false", got, ok)
	}
}

// TestSetAuditLogSameValueNoOp pins that revising to the current value
// is a silent no-op: no audit event, and — for retention — no sweep,
// so an expired file that a real revision would delete survives.
func TestSetAuditLogSameValueNoOp(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogRetentionDays(7))
	old := makeOldAuditFile(t, dir, 10, false)
	if err := l.SetAuditLogRetentionDays(7); err != nil {
		t.Fatalf("SetAuditLogRetentionDays(7) same-value: %v", err)
	}
	if err := l.SetAuditLogMaxBytes(DefaultAuditMaxBytes); err != nil {
		t.Fatalf("SetAuditLogMaxBytes(default) same-value: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("old file %s was deleted by a same-value (no-op) call: %v", old, err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if n := len(byOp["audit_log_retention_days_amended"]); n != 0 {
		t.Errorf("same-value retention revision wrote %d amendment events, want 0", n)
	}
	if n := len(byOp["audit_log_max_bytes_amended"]); n != 0 {
		t.Errorf("same-value maxBytes revision wrote %d amendment events, want 0", n)
	}
	if ev := findRetentionEvent(t, dir); ev != nil {
		t.Errorf("same-value retention revision ran a sweep: %+v", ev)
	}
}

// TestSetAuditLogCtxActorAttribution pins LG-73 attribution on the Ctx
// variants: the operator identity replaces the entrypoint name, while
// the plain setters keep it.
func TestSetAuditLogCtxActorAttribution(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := l.SetAuditLogMaxBytesCtx(ctx, 1024*1024); err != nil {
		t.Fatalf("SetAuditLogMaxBytesCtx: %v", err)
	}
	if err := l.SetAuditLogRetentionDaysCtx(ctx, 30); err != nil {
		t.Fatalf("SetAuditLogRetentionDaysCtx: %v", err)
	}
	if err := l.SetAuditLogMaxBytes(2048 * 1024); err != nil {
		t.Fatalf("SetAuditLogMaxBytes: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	maxAmended := byOp["audit_log_max_bytes_amended"]
	if len(maxAmended) != 2 {
		t.Fatalf("audit_log_max_bytes_amended events = %d, want 2", len(maxAmended))
	}
	if maxAmended[0].Actor != "ops-alan" {
		t.Errorf("Ctx amendment actor = %q, want ops-alan", maxAmended[0].Actor)
	}
	if maxAmended[1].Actor != "SetAuditLogMaxBytes" {
		t.Errorf("plain amendment actor = %q, want SetAuditLogMaxBytes", maxAmended[1].Actor)
	}
	retAmended := byOp["audit_log_retention_days_amended"]
	if len(retAmended) != 1 {
		t.Fatalf("audit_log_retention_days_amended events = %d, want 1", len(retAmended))
	}
	if retAmended[0].Actor != "ops-alan" {
		t.Errorf("Ctx retention amendment actor = %q, want ops-alan", retAmended[0].Actor)
	}
}

// TestSetAuditLogConcurrentAppendNoRace hammers the live readers and
// writers together: concurrent appends (the background writer reads
// the cap on every write) plus concurrent cap/window revisions must
// not race and must leave the log readable.
func TestSetAuditLogConcurrentAppendNoRace(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				al.Log(AuditEvent{Op: "post", Actor: "TestConcurrent",
					TraceID: fmt.Sprintf("c-%d-%d", n, j),
					Details: map[string]any{"n": j}})
			}
		}(n)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			_ = l.SetAuditLogMaxBytes(int64(1024 + i))
			_ = l.SetAuditLogRetentionDays(i % 3)
		}
	}()
	wg.Wait()
	if got, ok := l.AuditLogMaxBytes(); !ok || got < 1024 || got > 1024+24 {
		t.Fatalf("AuditLogMaxBytes() = %d, %v; want within [1024, 1048], true", got, ok)
	}
	if got, ok := l.AuditLogRetentionDays(); !ok || got < 0 || got > 2 {
		t.Fatalf("AuditLogRetentionDays() = %d, %v; want within [0, 2], true", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	// 1 construction alert + 200 concurrent posts + up to 50 amendment
	// events; the bound is loose because a dropped event is legal for
	// the async log, but a torn or lost run would show up far below it.
	if len(events) < 200 {
		t.Fatalf("read back %d events, want at least 200", len(events))
	}
}
