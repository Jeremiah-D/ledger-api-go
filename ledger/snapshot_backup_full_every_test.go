package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Snapshot-backup full/incremental alternation cadence runtime revision
// tests (LG-333). They reuse mustAuditLog from ledger_audit_test.go,
// mustPostEntry / testBackupWorker / listBackupFiles from
// snapshot_backup_test.go, and waitFor from ledger_hold_sweep_test.go.

// startFullEveryTestSnapshotBackup starts a backup worker with a long
// retention window, so no file is ever pruned and every tick leaves one
// more file in dir for the test to count.
func startFullEveryTestSnapshotBackup(t *testing.T, fullEvery int) (*Ledger, *SnapshotBackup, *AuditLog, string, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour,
		KeepFull: 100, KeepIncremental: 100, FullEvery: fullEvery, MaxRetries: 1,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	return l, sw, al, dir, cancel
}

// retunedSnapshotBackupFullEveryEvents stops the worker, closes the
// audit log (which drains the async writer), and returns every
// snapshot_backup_full_every_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedSnapshotBackupFullEveryEvents(t *testing.T, sw *SnapshotBackup, al *AuditLog) []AuditEvent {
	t.Helper()
	sw.Stop()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "snapshot_backup_full_every_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// countBackupFilesByKind splits a directory listing into full and
// incremental backup file counts.
func countBackupFilesByKind(files []string) (full, incr int) {
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, "snapshot-full-"):
			full++
		case strings.HasPrefix(f, "snapshot-incr-"):
			incr++
		}
	}
	return full, incr
}

func TestSetSnapshotBackupFullEveryInvalidRejectedAndKeepsLive(t *testing.T) {
	_, sw, al, _, cancel := startFullEveryTestSnapshotBackup(t, 6)
	defer cancel()

	for _, n := range []int{0, -1, -6} {
		prev, err := sw.SetFullEvery(n)
		if !errors.Is(err, ErrInvalidSnapshotBackupFullEvery) {
			t.Fatalf("SetFullEvery(%d): err = %v, want ErrInvalidSnapshotBackupFullEvery", n, err)
		}
		if prev != 6 {
			t.Fatalf("SetFullEvery(%d) returned %d, want the live 6", n, prev)
		}
		if got := sw.FullEvery(); got != 6 {
			t.Fatalf("FullEvery() = %d after rejected %d, want still 6", got, n)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := sw.SetFullEveryCtx(context.Background(), 0); !errors.Is(err, ErrInvalidSnapshotBackupFullEvery) || prev != 6 {
		t.Fatalf("SetFullEveryCtx(0) = (%d, %v), want (6, ErrInvalidSnapshotBackupFullEvery)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time fallback rule:
	// zero would divide by zero at the kind check, and a runtime
	// revision never silently normalizes (along LG-314).
	if events := retunedSnapshotBackupFullEveryEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetSnapshotBackupFullEverySameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, _, cancel := startFullEveryTestSnapshotBackup(t, 6)
	defer cancel()

	prev, err := sw.SetFullEvery(6)
	if err != nil {
		t.Fatalf("same-value SetFullEvery: %v", err)
	}
	if prev != 6 {
		t.Fatalf("same-value revision returned %d, want the live 6", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := sw.SetFullEvery(2); err != nil {
		t.Fatalf("SetFullEvery(2): %v", err)
	}
	if prev, err := sw.SetFullEvery(2); err != nil || prev != 2 {
		t.Fatalf("same-value SetFullEvery(2) = (%d, %v), want (2, nil)", prev, err)
	}
	if events := retunedSnapshotBackupFullEveryEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 (only the real revision audits)", len(events))
	}
}

func TestSetSnapshotBackupFullEveryReturnsPreviousAndGetterFollowsLive(t *testing.T) {
	_, sw, al, _, cancel := startFullEveryTestSnapshotBackup(t, 6)
	defer cancel()

	for _, tc := range []struct {
		n, wantPrev int
	}{
		{2, 6},
		{24, 2},
		{1, 24},
	} {
		prev, err := sw.SetFullEvery(tc.n)
		if err != nil {
			t.Fatalf("SetFullEvery(%d): %v", tc.n, err)
		}
		if prev != tc.wantPrev {
			t.Fatalf("SetFullEvery(%d) returned %d, want previous %d", tc.n, prev, tc.wantPrev)
		}
		if got := sw.FullEvery(); got != tc.n {
			t.Fatalf("FullEvery() = %d after SetFullEvery(%d), want %d", got, tc.n, tc.n)
		}
	}
	if events := retunedSnapshotBackupFullEveryEvents(t, sw, al); len(events) != 3 {
		t.Fatalf("retuned events = %d, want 3", len(events))
	}
}

func TestSetSnapshotBackupFullEveryOneMeansEveryBackupFull(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{FullEvery: 6, MaxRetries: 1})

	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 1 = (%q, %v), want (full, true)", kind, ok)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 2 = (%q, %v), want (incremental, true)", kind, ok)
	}
	if _, err := sw.SetFullEvery(1); err != nil {
		t.Fatalf("SetFullEvery(1): %v", err)
	}
	// From the next backup on, every backup is full.
	for i := 3; i <= 5; i++ {
		kind, ok := sw.backupOnce()
		if !ok || kind != "full" {
			t.Fatalf("backup %d = (%q, %v), want (full, true) with FullEvery=1", i, kind, ok)
		}
	}
	if sw.backups != 5 {
		t.Fatalf("backups = %d, want 5", sw.backups)
	}
}

func TestSetSnapshotBackupFullEveryWidenKeepsCounterPhase(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{FullEvery: 6, MaxRetries: 1})

	// FullEvery=6 alternation over the first 7 backups: full,
	// five incrementals, full.
	want := []string{"full", "incremental", "incremental", "incremental", "incremental", "incremental", "full"}
	for i, w := range want {
		kind, ok := sw.backupOnce()
		if !ok || kind != w {
			t.Fatalf("backup %d = (%q, %v), want (%q, true)", i+1, kind, ok, w)
		}
	}
	// Widen 6 -> 2 at backups=7: the counter is NOT reset, so the
	// next backup's kind is the uninterrupted counter against the
	// new modulus — backup 8 is (8-1)%2 == 1 (incremental) and
	// backup 9 is (9-1)%2 == 0 (full). A reset would have restarted
	// the sequence at full.
	prev, err := sw.SetFullEvery(2)
	if err != nil {
		t.Fatalf("SetFullEvery(2): %v", err)
	}
	if prev != 6 {
		t.Fatalf("SetFullEvery(2) returned %d, want previous 6", prev)
	}
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 8 = (%q, %v), want (incremental, true) — phase must continue, not restart", kind, ok)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 9 = (%q, %v), want (full, true)", kind, ok)
	}
	if sw.backups != 9 {
		t.Fatalf("backups = %d, want 9", sw.backups)
	}
}

func TestSetSnapshotBackupFullEveryTightenSequenceFollowsNewModulus(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{FullEvery: 6, MaxRetries: 1})

	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 1 = (%q, %v), want (full, true)", kind, ok)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 2 = (%q, %v), want (incremental, true)", kind, ok)
	}
	// Tighten 6 -> 3 at backups=2: backup 3 is (3-1)%3 == 2
	// (incremental), backup 4 is (4-1)%3 == 0 (full), then two
	// incrementals, then full again at backup 7.
	if _, err := sw.SetFullEvery(3); err != nil {
		t.Fatalf("SetFullEvery(3): %v", err)
	}
	want := []string{"incremental", "full", "incremental", "incremental", "full"}
	for i, w := range want {
		kind, ok := sw.backupOnce()
		if !ok || kind != w {
			t.Fatalf("backup %d = (%q, %v), want (%q, true)", i+3, kind, ok, w)
		}
	}
	if sw.backups != 7 {
		t.Fatalf("backups = %d, want 7", sw.backups)
	}
}

func TestSetSnapshotBackupFullEveryInFlightBackupNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	// A fuller journal makes the export take a real (if short) while,
	// so the revision below lands while the backup is in flight.
	for i := 0; i < 200; i++ {
		mustPostEntry(t, l, fmt.Sprintf("inflight-%d", i))
	}
	dir := t.TempDir()
	sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour, FullEvery: 2, MaxRetries: 1,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()

	// Drive one backup in a goroutine and revise the cadence from
	// this goroutine at the same time: the revision must not cut
	// the in-flight backup short, and it never takes sw.mu, so it
	// cannot stall behind it either.
	type result struct {
		kind string
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		kind, ok := sw.backupOnce()
		done <- result{kind, ok}
	}()
	if _, err := sw.SetFullEvery(3); err != nil {
		t.Fatalf("SetFullEvery during an in-flight backup: %v", err)
	}
	select {
	case r := <-done:
		if !r.ok || r.kind != "full" {
			t.Fatalf("in-flight backupOnce = (%q, %v), want (full, true)", r.kind, r.ok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight backup never finished")
	}
	// Its file still landed.
	files := listBackupFiles(t, dir)
	if len(files) != 1 || !strings.HasPrefix(files[0], "snapshot-full-") {
		t.Fatalf("files = %v, want one snapshot-full-* from the in-flight backup", files)
	}
	// The rotation counter is continuous and the new cadence governs
	// the next backup: backup 2 is (2-1)%3 == 1 (incremental), not
	// a restarted full.
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("next backupOnce = (%q, %v), want (incremental, true)", kind, ok)
	}
	if sw.backups != 2 {
		t.Fatalf("backups = %d, want 2 (in-flight + next, counter continuous)", sw.backups)
	}
	if got := sw.FullEvery(); got != 3 {
		t.Fatalf("FullEvery() = %d after the revision, want 3", got)
	}
}

func TestSetSnapshotBackupFullEveryDoesNotChangeRetention(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 1, KeepIncremental: 1, FullEvery: 2, MaxRetries: 1,
	})

	// FullEvery=2 with KeepFull=1 / KeepIncremental=1: full, incr,
	// full — the second full prunes the first, so exactly one of
	// each kind survives.
	for i, w := range []string{"full", "incremental", "full"} {
		kind, ok := sw.backupOnce()
		if !ok || kind != w {
			t.Fatalf("backup %d = (%q, %v), want (%q, true)", i+1, kind, ok, w)
		}
	}
	full, incr := countBackupFilesByKind(listBackupFiles(t, dir))
	if full != 1 || incr != 1 {
		t.Fatalf("files = %d full + %d incr, want 1 + 1", full, incr)
	}
	// Revise the cadence: the retention depths do not participate.
	if _, err := sw.SetFullEvery(4); err != nil {
		t.Fatalf("SetFullEvery(4): %v", err)
	}
	if got := sw.KeepFull(); got != 1 {
		t.Fatalf("KeepFull() = %d after the revision, want still 1", got)
	}
	if got := sw.KeepIncremental(); got != 1 {
		t.Fatalf("KeepIncremental() = %d after the revision, want still 1", got)
	}
	// Backup 4 is (4-1)%4 == 3 (incremental) and backup 5 is
	// (5-1)%4 == 0 (full); pruning still keeps exactly one of
	// each kind.
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 4 = (%q, %v), want (incremental, true)", kind, ok)
	}
	full, incr = countBackupFilesByKind(listBackupFiles(t, dir))
	if full != 1 || incr != 1 {
		t.Fatalf("files after backup 4 = %d full + %d incr, want 1 + 1", full, incr)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 5 = (%q, %v), want (full, true)", kind, ok)
	}
	full, incr = countBackupFilesByKind(listBackupFiles(t, dir))
	if full != 1 || incr != 1 {
		t.Fatalf("files after backup 5 = %d full + %d incr, want 1 + 1", full, incr)
	}
}

func TestSetSnapshotBackupFullEveryDoesNotChangeIncrementalBase(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{FullEvery: 6, MaxRetries: 1})

	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 1 = (%q, %v), want (full, true)", kind, ok)
	}
	if sw.lastVersion != 1 {
		t.Fatalf("lastVersion = %d after backup 1, want 1", sw.lastVersion)
	}
	// One more posting, then revise: the incremental chain's base
	// must still be the last successful backup's version.
	mustPostEntry(t, l, "e2")
	if _, err := sw.SetFullEvery(2); err != nil {
		t.Fatalf("SetFullEvery(2): %v", err)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 2 = (%q, %v), want (incremental, true)", kind, ok)
	}
	files := listBackupFiles(t, dir)
	var incr string
	for _, f := range files {
		if strings.HasPrefix(f, "snapshot-incr-") {
			incr = f
		}
	}
	// Base v00000000000000000001 (lastVersion before backup 2),
	// version v00000000000000000002 (after posting e2).
	if !strings.HasPrefix(incr, "snapshot-incr-v00000000000000000001-v00000000000000000002-") {
		t.Fatalf("incremental file = %q, want base v1 -> v2 (lastVersion continuous across the revision)", incr)
	}
	if sw.lastVersion != 2 {
		t.Fatalf("lastVersion = %d after backup 2, want 2", sw.lastVersion)
	}
}

func TestSetSnapshotBackupFullEveryConcurrentRevisionAndBackupRaceFree(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: 10 * time.Millisecond,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 6, MaxRetries: 1,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the worker
	// ticks underneath, plus direct backupOnce calls from another
	// goroutine: -race must stay silent, and the worker must keep
	// backing up on some positive cadence.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				n := 1 + (g*2+i)%8
				_, _ = sw.SetFullEvery(n)
				_ = sw.FullEvery()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _ = sw.backupOnce()
		}
	}()
	wg.Wait()
	if got := sw.FullEvery(); got < 1 || got > 8 {
		t.Fatalf("FullEvery() = %d after concurrent revisions, want a live value in 1..8", got)
	}
	waitFor(t, 3*time.Second, "a tick after the concurrent revisions", func() bool {
		return sw.ticks.Load() >= 1
	})
	waitForBackupFiles(t, dir, 1, 3*time.Second)
}

func TestSetSnapshotBackupFullEveryCtxActorAttribution(t *testing.T) {
	_, sw, al, _, cancel := startFullEveryTestSnapshotBackup(t, 6)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetFullEveryCtx(ctx, 2); err != nil {
		t.Fatalf("SetFullEveryCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetFullEvery(4); err != nil {
		t.Fatalf("SetFullEvery: %v", err)
	}
	events := retunedSnapshotBackupFullEveryEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_full_every"]; got != float64(6) {
		t.Fatalf("first previous_full_every = %v, want 6", got)
	}
	if got := events[0].Details["current_full_every"]; got != float64(2) {
		t.Fatalf("first current_full_every = %v, want 2", got)
	}
	if events[1].Actor != "SetFullEvery" {
		t.Fatalf("second event Actor = %q, want SetFullEvery", events[1].Actor)
	}
	if got := events[1].Details["previous_full_every"]; got != float64(2) {
		t.Fatalf("second previous_full_every = %v, want 2", got)
	}
	if got := events[1].Details["current_full_every"]; got != float64(4) {
		t.Fatalf("second current_full_every = %v, want 4", got)
	}
}

func TestSetSnapshotBackupFullEveryAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, _, cancel := startFullEveryTestSnapshotBackup(t, 6)
	defer cancel()

	prev, err := sw.SetFullEvery(2)
	if err != nil {
		t.Fatalf("SetFullEvery: %v", err)
	}
	if prev != 6 {
		t.Fatalf("SetFullEvery(2) returned %d, want previous 6", prev)
	}
	events := retunedSnapshotBackupFullEveryEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if got := ev.Details["previous_full_every"]; got != float64(6) {
		t.Fatalf("previous_full_every = %v, want 6", got)
	}
	if got := ev.Details["current_full_every"]; got != float64(2) {
		t.Fatalf("current_full_every = %v, want 2", got)
	}
	// The retune event carries only the two integer cadences: no
	// backup content, file path, or key material.
	if len(ev.Details) != 2 {
		t.Fatalf("details keys = %v, want exactly previous_full_every + current_full_every", ev.Details)
	}
}
