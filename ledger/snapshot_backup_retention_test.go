package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Snapshot-backup retention depths runtime revision tests (LG-332).
// They reuse mustAuditLog from ledger_audit_test.go,
// mustPostEntry / testBackupWorker / listBackupFiles from
// snapshot_backup_test.go, startRetuneTestSnapshotBackup from
// snapshot_backup_interval_test.go, and waitFor from
// ledger_hold_sweep_test.go.

// retunedSnapshotBackupRetentionEvents stops the worker, closes the
// audit log (which drains the async writer), and returns every
// snapshot_backup_keep_full_retuned and
// snapshot_backup_keep_incremental_retuned event recorded in it. The
// ledger must not be used after this call.
func retunedSnapshotBackupRetentionEvents(t *testing.T, sw *SnapshotBackup, al *AuditLog) []AuditEvent {
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
		if ev.Op == "snapshot_backup_keep_full_retuned" || ev.Op == "snapshot_backup_keep_incremental_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// countBackupFiles counts backup files of one kind ("full"/"incr"
// filename prefix) in dir.
func countBackupFiles(t *testing.T, dir, kind string) int {
	t.Helper()
	n := 0
	for _, f := range listBackupFiles(t, dir) {
		if kind == "full" && strings.HasPrefix(f, "snapshot-full-") {
			n++
		}
		if kind == "incr" && strings.HasPrefix(f, "snapshot-incr-") {
			n++
		}
	}
	return n
}

// driveBackups posts one entry and runs one backup per call, so each
// backup covers a new version and lands under a distinct filename.
// It returns the kinds in order.
func driveBackups(t *testing.T, l *Ledger, sw *SnapshotBackup, n int) []string {
	t.Helper()
	kinds := make([]string, 0, n)
	for i := 0; i < n; i++ {
		mustPostEntry(t, l, fmt.Sprintf("ret-%d-%d", n, i))
		kind, ok := sw.backupOnce()
		if !ok {
			t.Fatalf("backupOnce %d: not ok", i)
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

func TestSetSnapshotBackupRetentionInvalidRejectedAndKeepsLive(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	for _, n := range []int{0, -1, -100} {
		prev, err := sw.SetKeepFull(n)
		if !errors.Is(err, ErrInvalidSnapshotBackupKeepFull) {
			t.Fatalf("SetKeepFull(%d): err = %v, want ErrInvalidSnapshotBackupKeepFull", n, err)
		}
		if prev != 100 {
			t.Fatalf("SetKeepFull(%d) returned %d, want the live 100", n, prev)
		}
		if got := sw.KeepFull(); got != 100 {
			t.Fatalf("KeepFull() = %d after rejected %d, want still 100", got, n)
		}
		prev, err = sw.SetKeepIncremental(n)
		if !errors.Is(err, ErrInvalidSnapshotBackupKeepIncremental) {
			t.Fatalf("SetKeepIncremental(%d): err = %v, want ErrInvalidSnapshotBackupKeepIncremental", n, err)
		}
		if prev != 100 {
			t.Fatalf("SetKeepIncremental(%d) returned %d, want the live 100", n, prev)
		}
		if got := sw.KeepIncremental(); got != 100 {
			t.Fatalf("KeepIncremental() = %d after rejected %d, want still 100", got, n)
		}
	}
	// The Ctx variants reject the same way.
	if prev, err := sw.SetKeepFullCtx(context.Background(), 0); !errors.Is(err, ErrInvalidSnapshotBackupKeepFull) || prev != 100 {
		t.Fatalf("SetKeepFullCtx(0) = (%d, %v), want (100, ErrInvalidSnapshotBackupKeepFull)", prev, err)
	}
	if prev, err := sw.SetKeepIncrementalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidSnapshotBackupKeepIncremental) || prev != 100 {
		t.Fatalf("SetKeepIncrementalCtx(0) = (%d, %v), want (100, ErrInvalidSnapshotBackupKeepIncremental)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time normalize
	// rule: zero kept would make the next backup's pruning delete
	// every file of that kind, and a runtime revision never
	// silently normalizes.
	if events := retunedSnapshotBackupRetentionEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetSnapshotBackupRetentionSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	if prev, err := sw.SetKeepFull(100); err != nil || prev != 100 {
		t.Fatalf("same-value SetKeepFull(100) = (%d, %v), want (100, nil)", prev, err)
	}
	if prev, err := sw.SetKeepIncremental(100); err != nil || prev != 100 {
		t.Fatalf("same-value SetKeepIncremental(100) = (%d, %v), want (100, nil)", prev, err)
	}
	// One real revision per knob, then the same values again: only
	// the real ones audit.
	if _, err := sw.SetKeepFull(7); err != nil {
		t.Fatalf("SetKeepFull(7): %v", err)
	}
	if _, err := sw.SetKeepIncremental(24); err != nil {
		t.Fatalf("SetKeepIncremental(24): %v", err)
	}
	if prev, err := sw.SetKeepFull(7); err != nil || prev != 7 {
		t.Fatalf("same-value SetKeepFull(7) = (%d, %v), want (7, nil)", prev, err)
	}
	if prev, err := sw.SetKeepIncremental(24); err != nil || prev != 24 {
		t.Fatalf("same-value SetKeepIncremental(24) = (%d, %v), want (24, nil)", prev, err)
	}
	if events := retunedSnapshotBackupRetentionEvents(t, sw, al); len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2 (one per knob) after the real revisions plus the no-ops", len(events))
	}
}

func TestSetSnapshotBackupRetentionReturnsPreviousValue(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	prev, err := sw.SetKeepFull(7)
	if err != nil {
		t.Fatalf("SetKeepFull: %v", err)
	}
	if prev != 100 {
		t.Fatalf("returned previous keep-full = %d, want 100", prev)
	}
	if got := sw.KeepFull(); got != 7 {
		t.Fatalf("KeepFull() = %d after the revision, want 7", got)
	}
	prev, err = sw.SetKeepIncremental(24)
	if err != nil {
		t.Fatalf("SetKeepIncremental: %v", err)
	}
	if prev != 100 {
		t.Fatalf("returned previous keep-incremental = %d, want 100", prev)
	}
	if got := sw.KeepIncremental(); got != 24 {
		t.Fatalf("KeepIncremental() = %d after the revision, want 24", got)
	}
}

func TestSetSnapshotBackupKeepFullShortenPrunesNextFullBackup(t *testing.T) {
	l := New()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 1, MaxRetries: 1,
	})

	// Five full backups land (FullEvery=1, one post per backup so
	// each lands under a distinct filename).
	kinds := driveBackups(t, l, sw, 5)
	for i, k := range kinds {
		if k != "full" {
			t.Fatalf("backup %d kind = %q, want full", i, k)
		}
	}
	if n := len(listBackupFiles(t, dir)); n != 5 {
		t.Fatalf("files = %d before the revision, want 5 full backups", n)
	}

	// Tighten to 2: the next full backup's regular pruning keeps
	// only the two newest.
	if _, err := sw.SetKeepFull(2); err != nil {
		t.Fatalf("SetKeepFull(2): %v", err)
	}
	mustPostEntry(t, l, "ret-shorten-full")
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("next backupOnce = (%q, %v), want (full, true)", kind, ok)
	}
	if n := countBackupFiles(t, dir, "full"); n != 2 {
		t.Fatalf("full files = %d after the tightened backup, want 2 (files = %v)", n, listBackupFiles(t, dir))
	}
}

func TestSetSnapshotBackupKeepIncrementalShortenPrunesNextIncrBackup(t *testing.T) {
	l := New()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 100, MaxRetries: 1,
	})

	// One full + four incrementals land.
	kinds := driveBackups(t, l, sw, 5)
	if kinds[0] != "full" {
		t.Fatalf("backup 0 kind = %q, want full", kinds[0])
	}
	for i, k := range kinds[1:] {
		if k != "incremental" {
			t.Fatalf("backup %d kind = %q, want incremental", i+1, k)
		}
	}
	if n := countBackupFiles(t, dir, "incr"); n != 4 {
		t.Fatalf("incremental files = %d before the revision, want 4", n)
	}

	// Tighten the incremental depth to 2: the next incremental
	// backup's pruning keeps only the two newest incrementals,
	// and the full file is untouched.
	if _, err := sw.SetKeepIncremental(2); err != nil {
		t.Fatalf("SetKeepIncremental(2): %v", err)
	}
	mustPostEntry(t, l, "ret-shorten-incr")
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("next backupOnce = (%q, %v), want (incremental, true)", kind, ok)
	}
	if n := countBackupFiles(t, dir, "incr"); n != 2 {
		t.Fatalf("incremental files = %d after the tightened backup, want 2 (files = %v)", n, listBackupFiles(t, dir))
	}
	if n := countBackupFiles(t, dir, "full"); n != 1 {
		t.Fatalf("full files = %d after the tightened incremental backup, want still 1", n)
	}
}

func TestSetSnapshotBackupKeepFullLengthenKeepsOldFiles(t *testing.T) {
	l := New()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 2, KeepIncremental: 100, FullEvery: 1, MaxRetries: 1,
	})

	// Five full backups under a depth of 2: only the two newest
	// survive each pruning.
	driveBackups(t, l, sw, 5)
	if n := countBackupFiles(t, dir, "full"); n != 2 {
		t.Fatalf("full files = %d under the tight depth, want 2", n)
	}

	// Loosen to 10: the next three backups accumulate instead of
	// pruning — the files the old depth would have deleted survive.
	if _, err := sw.SetKeepFull(10); err != nil {
		t.Fatalf("SetKeepFull(10): %v", err)
	}
	driveBackups(t, l, sw, 3)
	if n := countBackupFiles(t, dir, "full"); n != 5 {
		t.Fatalf("full files = %d after the loosened backups, want 5 (2 kept + 3 new)", n)
	}
}

func TestSetSnapshotBackupRetentionRevisionDeletesNoFiles(t *testing.T) {
	l := New()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 1, MaxRetries: 1,
	})

	driveBackups(t, l, sw, 3)
	before := listBackupFiles(t, dir)
	if len(before) != 3 {
		t.Fatalf("files = %d before the revision, want 3", len(before))
	}

	// Tighten both depths drastically — but no backup runs on the
	// 1h cadence, so the directory must be unchanged: the revision
	// itself never sweeps.
	if _, err := sw.SetKeepFull(1); err != nil {
		t.Fatalf("SetKeepFull(1): %v", err)
	}
	if _, err := sw.SetKeepIncremental(1); err != nil {
		t.Fatalf("SetKeepIncremental(1): %v", err)
	}
	after := listBackupFiles(t, dir)
	if len(after) != 3 {
		t.Fatalf("files = %v after the revision with no backup, want the same 3 files %v", after, before)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("files changed after the revision: before %v, after %v", before, after)
		}
	}
}

func TestSetSnapshotBackupRetentionInFlightBackupNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	// A fuller journal makes the export take a real (if short) while,
	// so the revisions below land while the backup is in flight.
	for i := 0; i < 200; i++ {
		mustPostEntry(t, l, fmt.Sprintf("inflight-ret-%d", i))
	}
	dir := t.TempDir()
	sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{
		Dir: dir, Interval: time.Hour, FullEvery: 2, MaxRetries: 1,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()

	// Drive one backup in a goroutine and revise both depths from
	// this goroutine at the same time: the revisions must not cut
	// the in-flight backup short, and they never take sw.mu, so
	// they cannot stall behind it either.
	type result struct {
		kind string
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		kind, ok := sw.backupOnce()
		done <- result{kind, ok}
	}()
	if _, err := sw.SetKeepFull(3); err != nil {
		t.Fatalf("SetKeepFull during an in-flight backup: %v", err)
	}
	if _, err := sw.SetKeepIncremental(5); err != nil {
		t.Fatalf("SetKeepIncremental during an in-flight backup: %v", err)
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
	// The rotation counter is continuous: the next backup is the
	// incremental second of the FullEvery=2 alternation, not a
	// restarted full.
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("next backupOnce = (%q, %v), want (incremental, true)", kind, ok)
	}
	if sw.backups != 2 {
		t.Fatalf("backups = %d, want 2 (in-flight + next, counter continuous)", sw.backups)
	}
	if got := sw.KeepFull(); got != 3 {
		t.Fatalf("KeepFull() = %d after the revision, want 3", got)
	}
	if got := sw.KeepIncremental(); got != 5 {
		t.Fatalf("KeepIncremental() = %d after the revision, want 5", got)
	}
}

func TestSetSnapshotBackupRetentionFullEveryRotationNotReset(t *testing.T) {
	l := New()
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 3, MaxRetries: 1,
	})

	// FullEvery=3: full, incremental, incremental.
	kinds := driveBackups(t, l, sw, 3)
	if want := []string{"full", "incremental", "incremental"}; fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}

	// Revise both depths: the alternation must continue, not
	// restart — the fourth backup is the next full of the cycle.
	if _, err := sw.SetKeepFull(7); err != nil {
		t.Fatalf("SetKeepFull: %v", err)
	}
	if _, err := sw.SetKeepIncremental(24); err != nil {
		t.Fatalf("SetKeepIncremental: %v", err)
	}
	mustPostEntry(t, l, "ret-rotation")
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("fourth backupOnce = (%q, %v), want (full, true)", kind, ok)
	}
	if sw.backups != 4 {
		t.Fatalf("backups = %d, want 4 (rotation counter continuous)", sw.backups)
	}
	if n := len(listBackupFiles(t, dir)); n != 4 {
		t.Fatalf("files = %d, want 4 (one per backup, nothing pruned at depth 7/24)", n)
	}
}

func TestSetSnapshotBackupRetentionConcurrentRevisionAndBackupsRaceFree(t *testing.T) {
	_, sw, al, dir, cancel := startRetuneTestSnapshotBackup(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the worker
	// ticks underneath, plus direct tick() calls from another
	// goroutine: -race must stay silent, and the worker must keep
	// backing up on some positive depths.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				n := 1 + g*10 + i%5
				_, _ = sw.SetKeepFull(n)
				_, _ = sw.SetKeepIncremental(n)
				_ = sw.KeepFull()
				_ = sw.KeepIncremental()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			sw.tick()
		}
	}()
	wg.Wait()
	if got := sw.KeepFull(); got <= 0 {
		t.Fatalf("KeepFull() = %d after concurrent revisions, want a positive live value", got)
	}
	if got := sw.KeepIncremental(); got <= 0 {
		t.Fatalf("KeepIncremental() = %d after concurrent revisions, want a positive live value", got)
	}
	// The revision storm keeps the worker ticking on the 10ms
	// cadence; once it ends a tick lands within one interval.
	waitFor(t, 3*time.Second, "a tick after the concurrent revisions", func() bool {
		return sw.ticks.Load() >= 1
	})
	if n := len(listBackupFiles(t, dir)); n < 1 {
		t.Fatalf("files = %d after the concurrent revisions, want at least 1 (one backup landed)", n)
	}
}

func TestSetSnapshotBackupRetentionCtxActorAttribution(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetKeepFullCtx(ctx, 7); err != nil {
		t.Fatalf("SetKeepFullCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetKeepIncremental(24); err != nil {
		t.Fatalf("SetKeepIncremental: %v", err)
	}
	events := retunedSnapshotBackupRetentionEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Op != "snapshot_backup_keep_full_retuned" || events[0].Actor != "ops-alan" {
		t.Fatalf("first event = (%q, %q), want (snapshot_backup_keep_full_retuned, ops-alan)", events[0].Op, events[0].Actor)
	}
	if got := events[0].Details["previous_keep_full"]; got != float64(100) {
		t.Fatalf("first previous_keep_full = %v, want 100", got)
	}
	if got := events[0].Details["current_keep_full"]; got != float64(7) {
		t.Fatalf("first current_keep_full = %v, want 7", got)
	}
	if events[1].Op != "snapshot_backup_keep_incremental_retuned" || events[1].Actor != "SetKeepIncremental" {
		t.Fatalf("second event = (%q, %q), want (snapshot_backup_keep_incremental_retuned, SetKeepIncremental)", events[1].Op, events[1].Actor)
	}
	if got := events[1].Details["previous_keep_incremental"]; got != float64(100) {
		t.Fatalf("second previous_keep_incremental = %v, want 100", got)
	}
	if got := events[1].Details["current_keep_incremental"]; got != float64(24) {
		t.Fatalf("second current_keep_incremental = %v, want 24", got)
	}
}

func TestSetSnapshotBackupRetentionAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	if prev, err := sw.SetKeepFull(7); err != nil || prev != 100 {
		t.Fatalf("SetKeepFull(7) = (%d, %v), want (100, nil)", prev, err)
	}
	if prev, err := sw.SetKeepIncremental(24); err != nil || prev != 100 {
		t.Fatalf("SetKeepIncremental(24) = (%d, %v), want (100, nil)", prev, err)
	}
	events := retunedSnapshotBackupRetentionEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	full := events[0]
	if full.Op != "snapshot_backup_keep_full_retuned" || full.Actor != "SetKeepFull" {
		t.Errorf("full event = (%q, %q), want (snapshot_backup_keep_full_retuned, SetKeepFull)", full.Op, full.Actor)
	}
	if full.VersionBefore != full.VersionAfter {
		t.Errorf("full event version moved %d -> %d, want no bump", full.VersionBefore, full.VersionAfter)
	}
	if got := full.Details["previous_keep_full"]; got != float64(100) {
		t.Errorf("previous_keep_full = %v, want 100", got)
	}
	if got := full.Details["current_keep_full"]; got != float64(7) {
		t.Errorf("current_keep_full = %v, want 7", got)
	}
	if len(full.Details) != 2 {
		t.Errorf("full details = %v, want exactly previous/current keep full", full.Details)
	}
	incr := events[1]
	if incr.Op != "snapshot_backup_keep_incremental_retuned" || incr.Actor != "SetKeepIncremental" {
		t.Errorf("incremental event = (%q, %q), want (snapshot_backup_keep_incremental_retuned, SetKeepIncremental)", incr.Op, incr.Actor)
	}
	if incr.VersionBefore != incr.VersionAfter {
		t.Errorf("incremental event version moved %d -> %d, want no bump", incr.VersionBefore, incr.VersionAfter)
	}
	if got := incr.Details["previous_keep_incremental"]; got != float64(100) {
		t.Errorf("previous_keep_incremental = %v, want 100", got)
	}
	if got := incr.Details["current_keep_incremental"]; got != float64(24) {
		t.Errorf("current_keep_incremental = %v, want 24", got)
	}
	if len(incr.Details) != 2 {
		t.Errorf("incremental details = %v, want exactly previous/current keep incremental", incr.Details)
	}
	// No backup content, path, or key material rides in either
	// event: only the two integer depths per event.
	for _, ev := range events {
		blob, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		for _, bad := range []string{"snapshot-", "LEDSNAPENC1", "key_id", "merkle_root", ".jsonl"} {
			if strings.Contains(string(blob), bad) {
				t.Errorf("audit event %q contains %q — backup content must never ride in a retune event", ev.Op, bad)
			}
		}
	}
}

func TestSetSnapshotBackupRetentionNoRestartContinuousBackups(t *testing.T) {
	_, sw, al, dir, cancel := startRetuneTestSnapshotBackup(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the worker back up on the original depths first.
	waitFor(t, 3*time.Second, "first backups", func() bool { return sw.ticks.Load() >= 2 })
	ticksBefore := sw.ticks.Load()
	filesBefore := len(listBackupFiles(t, dir))

	// Revise both depths without restarting: the same goroutine
	// keeps backing up (ticks only grow). (File count is a lower
	// bound, not an exact count: filenames are second-granular and
	// the version does not move without posts, so fast ticks on an
	// unchanged ledger overwrite the same file instead of adding
	// one.)
	if _, err := sw.SetKeepFull(7); err != nil {
		t.Fatalf("SetKeepFull: %v", err)
	}
	if _, err := sw.SetKeepIncremental(24); err != nil {
		t.Fatalf("SetKeepIncremental: %v", err)
	}
	waitFor(t, 3*time.Second, "backups after the revision", func() bool {
		return sw.ticks.Load() >= ticksBefore+2
	})
	if got := sw.ticks.Load(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if n := len(listBackupFiles(t, dir)); n < filesBefore {
		t.Fatalf("files = %d after the revision, want at least %d (backups continue, none lost)", n, filesBefore)
	}
	if got := sw.KeepFull(); got != 7 {
		t.Fatalf("KeepFull() = %d after the revision, want 7", got)
	}
	if got := sw.KeepIncremental(); got != 24 {
		t.Fatalf("KeepIncremental() = %d after the revision, want 24", got)
	}
}

func TestSetSnapshotBackupRetentionGetterFollowsLiveValue(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	if got := sw.KeepFull(); got != 100 {
		t.Fatalf("KeepFull() = %d at start, want 100", got)
	}
	if got := sw.KeepIncremental(); got != 100 {
		t.Fatalf("KeepIncremental() = %d at start, want 100", got)
	}
	if prev, err := sw.SetKeepFull(7); err != nil || prev != 100 {
		t.Fatalf("SetKeepFull(7) = (%d, %v), want (100, nil)", prev, err)
	}
	if got := sw.KeepFull(); got != 7 {
		t.Fatalf("KeepFull() = %d after tightening, want 7", got)
	}
	if prev, err := sw.SetKeepIncremental(24); err != nil || prev != 100 {
		t.Fatalf("SetKeepIncremental(24) = (%d, %v), want (100, nil)", prev, err)
	}
	if got := sw.KeepIncremental(); got != 24 {
		t.Fatalf("KeepIncremental() = %d after tightening, want 24", got)
	}
	// A rejected revision leaves the getters in place.
	if _, err := sw.SetKeepFull(0); !errors.Is(err, ErrInvalidSnapshotBackupKeepFull) {
		t.Fatalf("SetKeepFull(0): err = %v, want ErrInvalidSnapshotBackupKeepFull", err)
	}
	if got := sw.KeepFull(); got != 7 {
		t.Fatalf("KeepFull() = %d after rejected revision, want still 7", got)
	}
	if _, err := sw.SetKeepIncremental(-1); !errors.Is(err, ErrInvalidSnapshotBackupKeepIncremental) {
		t.Fatalf("SetKeepIncremental(-1): err = %v, want ErrInvalidSnapshotBackupKeepIncremental", err)
	}
	if got := sw.KeepIncremental(); got != 24 {
		t.Fatalf("KeepIncremental() = %d after rejected revision, want still 24", got)
	}
}
