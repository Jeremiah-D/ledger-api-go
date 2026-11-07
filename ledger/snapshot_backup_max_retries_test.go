package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Snapshot-backup export retry budget runtime revision tests (LG-344).
// They reuse mustAuditLog from ledger_audit_test.go, mustPostEntry /
// listBackupFiles / readMetaLine from snapshot_backup_test.go, and
// waitFor from ledger_hold_sweep_test.go.

// startMaxRetriesTestSnapshotBackup starts a backup worker with a long
// retention window, so no file is ever pruned, and a 1h ticker, so the
// goroutine never fires during the test; tests drive backupOnce / tick
// directly for determinism.
func startMaxRetriesTestSnapshotBackup(t *testing.T, maxRetries int) (*Ledger, *SnapshotBackup, *AuditLog, string, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 6, MaxRetries: maxRetries,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	t.Cleanup(sw.Stop)
	return l, sw, al, dir, cancel
}

// retunedSnapshotBackupMaxRetriesEvents stops the worker, closes the
// audit log (which drains the async writer), and returns every
// snapshot_backup_max_retries_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedSnapshotBackupMaxRetriesEvents(t *testing.T, sw *SnapshotBackup, al *AuditLog) []AuditEvent {
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
		if ev.Op == "snapshot_backup_max_retries_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// breakTestLedgerExport makes every export from l fail deterministically:
// the journal chain still references the posted entry, but the entry
// row itself is gone, so ExportSnapshot / ExportIncrementalSnapshot
// fail with "chain references missing entry" on the first attempt and
// on every retry. Same-package tests may touch l.entries directly.
func breakTestLedgerExport(t *testing.T, l *Ledger, id string) {
	t.Helper()
	mustPostEntry(t, l, id)
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
}

func TestSetSnapshotBackupMaxRetriesInvalidRejectedAndKeepsLive(t *testing.T) {
	_, sw, al, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()

	for _, n := range []int{-1, -3, -100} {
		prev, err := sw.SetMaxRetries(n)
		if !errors.Is(err, ErrInvalidSnapshotBackupMaxRetries) {
			t.Fatalf("SetMaxRetries(%d): err = %v, want ErrInvalidSnapshotBackupMaxRetries", n, err)
		}
		if prev != 3 {
			t.Fatalf("SetMaxRetries(%d) returned %d, want the live 3", n, prev)
		}
		if got := sw.MaxRetries(); got != 3 {
			t.Fatalf("MaxRetries() = %d after rejected %d, want still 3", got, n)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := sw.SetMaxRetriesCtx(context.Background(), -1); !errors.Is(err, ErrInvalidSnapshotBackupMaxRetries) || prev != 3 {
		t.Fatalf("SetMaxRetriesCtx(-1) = (%d, %v), want (3, ErrInvalidSnapshotBackupMaxRetries)", prev, err)
	}
	if events := retunedSnapshotBackupMaxRetriesEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetSnapshotBackupMaxRetriesZeroIsLegalSingleAttempt(t *testing.T) {
	l, sw, _, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()
	breakTestLedgerExport(t, l, "e1")

	prev, err := sw.SetMaxRetries(0)
	if err != nil {
		t.Fatalf("SetMaxRetries(0): %v, want nil — zero is the legal fail-fast budget", err)
	}
	if prev != 3 {
		t.Fatalf("SetMaxRetries(0) returned %d, want the previous 3", prev)
	}
	if got := sw.MaxRetries(); got != 0 {
		t.Fatalf("MaxRetries() = %d after SetMaxRetries(0), want 0", got)
	}
	// Zero means a single attempt with no backoff sleep: one failed
	// export, no 100ms sleeps, so the tick finishes fast.
	start := time.Now()
	kind, ok := sw.backupOnce()
	elapsed := time.Since(start)
	if ok {
		t.Fatal("backupOnce succeeded against a broken export")
	}
	if kind != "full" {
		t.Fatalf("kind = %q, want full (first tick)", kind)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("zero-budget tick took %v, want < 150ms (a single attempt, no backoff sleep)", elapsed)
	}
}

func TestSetSnapshotBackupMaxRetriesSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()

	prev, err := sw.SetMaxRetries(3)
	if err != nil {
		t.Fatalf("same-value SetMaxRetries: %v", err)
	}
	if prev != 3 {
		t.Fatalf("same-value revision returned %d, want the live 3", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := sw.SetMaxRetries(8); err != nil {
		t.Fatalf("SetMaxRetries(8): %v", err)
	}
	if _, err := sw.SetMaxRetries(8); err != nil {
		t.Fatalf("same-value SetMaxRetries(8): %v", err)
	}
	events := retunedSnapshotBackupMaxRetriesEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 (only the 3 -> 8 revision)", len(events))
	}
	if got := events[0].Details["previous_max_retries"]; got != float64(3) {
		t.Fatalf("previous_max_retries = %v, want 3", got)
	}
	if got := events[0].Details["current_max_retries"]; got != float64(8) {
		t.Fatalf("current_max_retries = %v, want 8", got)
	}
}

func TestSetSnapshotBackupMaxRetriesWideningLengthensFailedTick(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	breakTestLedgerExport(t, l, "e1")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	var lastOK bool
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 6, MaxRetries: 3,
	}, func(kind string, ok bool) {
		calls++
		lastOK = ok
	})
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()

	// Budget 3: 4 attempts, 3 x 100ms backoff sleeps, ~300ms. The
	// tick still counts exactly once, however many retries it ran.
	start := time.Now()
	sw.tick()
	elapsed3 := time.Since(start)
	if calls != 1 || lastOK {
		t.Fatalf("onBackup after budget-3 tick: calls=%d ok=%v, want (1, false)", calls, lastOK)
	}
	if elapsed3 < 200*time.Millisecond || elapsed3 > 600*time.Millisecond {
		t.Fatalf("budget-3 failed tick took %v, want ~300ms", elapsed3)
	}

	if _, err := sw.SetMaxRetries(8); err != nil {
		t.Fatalf("SetMaxRetries(8): %v", err)
	}
	calls = 0
	start = time.Now()
	sw.tick()
	elapsed8 := time.Since(start)
	if calls != 1 || lastOK {
		t.Fatalf("onBackup after budget-8 tick: calls=%d ok=%v, want (1, false)", calls, lastOK)
	}
	// Budget 8: 9 attempts, 8 x 100ms sleeps, ~800ms — about 500ms
	// longer than the budget-3 tick, and still exactly one failed
	// tick.
	growth := elapsed8 - elapsed3
	if growth < 400*time.Millisecond || growth > 700*time.Millisecond {
		t.Fatalf("failed-tick growth 3 -> 8 = %v (3: %v, 8: %v), want ~500ms", growth, elapsed3, elapsed8)
	}
}

func TestSetSnapshotBackupMaxRetriesTightenToZeroFailsFast(t *testing.T) {
	l, sw, _, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()
	breakTestLedgerExport(t, l, "e1")

	if _, err := sw.SetMaxRetries(0); err != nil {
		t.Fatalf("SetMaxRetries(0): %v", err)
	}
	start := time.Now()
	_, ok := sw.backupOnce()
	elapsed := time.Since(start)
	if ok {
		t.Fatal("backupOnce succeeded against a broken export")
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("zero-budget tick took %v, want < 150ms (fail-fast: one attempt, no backoff)", elapsed)
	}
}

func TestSetSnapshotBackupMaxRetriesInFlightBackupKeepsOldBudget(t *testing.T) {
	l, sw, _, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()
	breakTestLedgerExport(t, l, "e1")

	done := make(chan struct{})
	var ok bool
	start := time.Now()
	go func() {
		defer close(done)
		_, ok = sw.backupOnce()
	}()
	// The tick is in its first 100ms backoff sleep by now: the export
	// fails in under a millisecond, so the goroutine has taken the
	// old budget (3) into its local long before this revision lands.
	time.Sleep(50 * time.Millisecond)
	if _, err := sw.SetMaxRetries(8); err != nil {
		t.Fatalf("SetMaxRetries(8) mid-tick: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight backup did not finish")
	}
	elapsed := time.Since(start)
	if ok {
		t.Fatal("backupOnce succeeded against a broken export")
	}
	if got := sw.MaxRetries(); got != 8 {
		t.Fatalf("MaxRetries() = %d after the revision, want 8", got)
	}
	// Old budget 3: 4 attempts, 3 x 100ms sleeps, ~300ms. Had the
	// revision rewritten the in-flight budget to 8, the tick would
	// have taken ~800ms; had it collapsed to 0, ~0ms.
	if elapsed < 150*time.Millisecond || elapsed > 650*time.Millisecond {
		t.Fatalf("in-flight tick took %v, want ~300ms (the old budget of 3)", elapsed)
	}
}

func TestSetSnapshotBackupMaxRetriesRevisionDoesNotInterruptInFlightSuccess(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 6, MaxRetries: 3,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()

	// Park the export inside backupOnce: hold the ledger write lock
	// so ExportSnapshot blocks on l.mu.RLock after backupOnce has
	// taken sw.mu and read the live budget into its local.
	l.mu.Lock()
	done := make(chan struct{})
	var kind string
	var ok bool
	go func() {
		defer close(done)
		kind, ok = sw.backupOnce()
	}()
	// Let the goroutine enter backupOnce and park in the export.
	time.Sleep(50 * time.Millisecond)
	// Revise while the backup is in flight. The swap itself never
	// blocks (maxRetriesMu is free); the audit emit parks on l.mu
	// behind this test's write lock, so run it in its own goroutine.
	revDone := make(chan struct{})
	go func() {
		defer close(revDone)
		if _, err := sw.SetMaxRetries(8); err != nil {
			t.Errorf("SetMaxRetries(8) mid-backup: %v", err)
		}
	}()
	// The live value has moved even though the audit emit is still
	// parked: the setter swaps before it audits.
	waitFor(t, 3*time.Second, "the mid-backup revision to swap the live value", func() bool {
		return sw.MaxRetries() == 8
	})
	// Release the ledger: the parked export runs, the file lands,
	// and the parked audit emit completes.
	l.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight backup did not complete after the ledger lock was released")
	}
	select {
	case <-revDone:
	case <-time.After(5 * time.Second):
		t.Fatal("mid-backup revision did not complete after the ledger lock was released")
	}
	if !ok {
		t.Fatal("in-flight backup failed after a mid-backup revision")
	}
	if kind != "full" {
		t.Fatalf("kind = %q, want full (first tick)", kind)
	}
	files := listBackupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("backup files = %v, want exactly 1 full file", files)
	}
	if !strings.HasPrefix(files[0], "snapshot-full-") {
		t.Fatalf("first file = %q, want a full snapshot", files[0])
	}
	// The file anchors the same Merkle root an independent export
	// of the untouched ledger produces: the revision changed the
	// retry budget, not the backup content.
	meta := readMetaLine(t, filepath.Join(dir, files[0]))
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	line := strings.SplitN(buf.String(), "\n", 2)[0]
	var wantMeta map[string]any
	if err := json.Unmarshal([]byte(line), &wantMeta); err != nil {
		t.Fatalf("meta line: %v", err)
	}
	if meta["merkle_root"] != wantMeta["merkle_root"] {
		t.Fatalf("merkle_root = %v, want %v", meta["merkle_root"], wantMeta["merkle_root"])
	}
	// The rotation counter was not reset by the revision: the next
	// backup is incremental, not another full.
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("second backupOnce = (%q, %v), want (incremental, true)", kind, ok)
	}
	if files := listBackupFiles(t, dir); len(files) != 2 {
		t.Fatalf("backup files = %v, want 2 after the second backup", files)
	}
}

func TestSetSnapshotBackupMaxRetriesConcurrentRevisionsAndBackupsRaceFree(t *testing.T) {
	l, sw, _, dir, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()
	mustPostEntry(t, l, "e1")

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := sw.SetMaxRetries((g + i) % 9); err != nil {
					t.Errorf("SetMaxRetries: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			// One new entry per backup so the version advances and
			// every backup lands under a distinct filename (the
			// filename carries the version, not a sequence number).
			if _, _, err := l.Post(JournalEntry{
				ID: fmt.Sprintf("race-%d", i),
				DebitAccount: "alice", CreditAccount: "bob",
				AmountCents: 100, Currency: "USD",
			}); err != nil {
				t.Errorf("Post race-%d: %v", i, err)
				return
			}
			if _, ok := sw.backupOnce(); !ok {
				t.Errorf("backupOnce %d failed", i)
				return
			}
		}
	}()
	wg.Wait()
	if got := sw.MaxRetries(); got < 0 || got > 8 {
		t.Fatalf("MaxRetries() = %d after concurrent revisions, want a live value in 0..8", got)
	}
	if files := listBackupFiles(t, dir); len(files) != 20 {
		t.Fatalf("backup files = %d, want 20 (one per backupOnce)", len(files))
	}
}

func TestSetSnapshotBackupMaxRetriesCtxActorAttribution(t *testing.T) {
	_, sw, al, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetMaxRetriesCtx(ctx, 8); err != nil {
		t.Fatalf("SetMaxRetriesCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetMaxRetries(0); err != nil {
		t.Fatalf("SetMaxRetries: %v", err)
	}
	events := retunedSnapshotBackupMaxRetriesEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_max_retries"]; got != float64(3) {
		t.Fatalf("first previous_max_retries = %v, want 3", got)
	}
	if got := events[0].Details["current_max_retries"]; got != float64(8) {
		t.Fatalf("first current_max_retries = %v, want 8", got)
	}
	if events[1].Actor != "SetMaxRetries" {
		t.Fatalf("second event Actor = %q, want SetMaxRetries", events[1].Actor)
	}
	if got := events[1].Details["previous_max_retries"]; got != float64(8) {
		t.Fatalf("second previous_max_retries = %v, want 8", got)
	}
	if got := events[1].Details["current_max_retries"]; got != float64(0) {
		t.Fatalf("second current_max_retries = %v, want 0", got)
	}
}

func TestSetSnapshotBackupMaxRetriesAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()

	prev, err := sw.SetMaxRetries(8)
	if err != nil {
		t.Fatalf("SetMaxRetries(8): %v", err)
	}
	if prev != 3 {
		t.Fatalf("SetMaxRetries(8) returned %d, want the previous 3", prev)
	}
	events := retunedSnapshotBackupMaxRetriesEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	// The details carry exactly the two integer budgets: no backup
	// content, no key material, nothing else rides in a retune event.
	if len(events[0].Details) != 2 {
		t.Fatalf("event Details keys = %v, want exactly previous/current", events[0].Details)
	}
	if got := events[0].Details["previous_max_retries"]; got != float64(3) {
		t.Fatalf("previous_max_retries = %v, want 3", got)
	}
	if got := events[0].Details["current_max_retries"]; got != float64(8) {
		t.Fatalf("current_max_retries = %v, want 8", got)
	}
}

func TestSetSnapshotBackupMaxRetriesGetterFollowsLive(t *testing.T) {
	_, sw, _, _, cancel := startMaxRetriesTestSnapshotBackup(t, 3)
	defer cancel()

	if got := sw.MaxRetries(); got != 3 {
		t.Fatalf("MaxRetries() = %d at start, want the seeded 3", got)
	}
	prev, err := sw.SetMaxRetries(8)
	if err != nil || prev != 3 {
		t.Fatalf("SetMaxRetries(8) = (%d, %v), want (3, nil)", prev, err)
	}
	if got := sw.MaxRetries(); got != 8 {
		t.Fatalf("MaxRetries() = %d after SetMaxRetries(8), want 8", got)
	}
	prev, err = sw.SetMaxRetries(0)
	if err != nil || prev != 8 {
		t.Fatalf("SetMaxRetries(0) = (%d, %v), want (8, nil)", prev, err)
	}
	if got := sw.MaxRetries(); got != 0 {
		t.Fatalf("MaxRetries() = %d after SetMaxRetries(0), want 0", got)
	}
	prev, err = sw.SetMaxRetries(5)
	if err != nil || prev != 0 {
		t.Fatalf("SetMaxRetries(5) = (%d, %v), want (0, nil)", prev, err)
	}
	if got := sw.MaxRetries(); got != 5 {
		t.Fatalf("MaxRetries() = %d after SetMaxRetries(5), want 5", got)
	}
}
