package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Snapshot-backup interval runtime revision tests (LG-329).
// They reuse mustAuditLog from ledger_audit_test.go,
// mustPostEntry / testBackupWorker / listBackupFiles from
// snapshot_backup_test.go, and waitFor from
// ledger_hold_sweep_test.go.

// startRetuneTestSnapshotBackup starts a backup worker with a long
// retention window and a FullEvery far above the tick counts the
// tests reach, so no file is ever pruned and every tick leaves one
// more file in dir for the test to count.
func startRetuneTestSnapshotBackup(t *testing.T, interval time.Duration) (*Ledger, *SnapshotBackup, *AuditLog, string, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	sw := StartSnapshotBackup(ctx, l, SnapshotBackupConfig{
		Dir: dir, Interval: interval,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 100, MaxRetries: 1,
	}, nil)
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	return l, sw, al, dir, cancel
}

// retunedSnapshotBackupIntervalEvents stops the worker, closes the
// audit log (which drains the async writer), and returns every
// snapshot_backup_interval_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedSnapshotBackupIntervalEvents(t *testing.T, sw *SnapshotBackup, al *AuditLog) []AuditEvent {
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
		if ev.Op == "snapshot_backup_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// waitForBackupFiles polls the backup directory until at least n
// backup files are present, then returns the listing.
func waitForBackupFiles(t *testing.T, dir string, n int, timeout time.Duration) []string {
	t.Helper()
	var files []string
	waitFor(t, timeout, fmt.Sprintf("%d backup files", n), func() bool {
		files = listBackupFiles(t, dir)
		return len(files) >= n
	})
	return files
}

func TestSetSnapshotBackupIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := sw.SetInterval(d)
		if !errors.Is(err, ErrInvalidSnapshotBackupInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidSnapshotBackupInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := sw.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := sw.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidSnapshotBackupInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidSnapshotBackupInterval)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time disable rule:
	// zero would stop the whole worker, and a runtime revision
	// never silently disables it.
	if events := retunedSnapshotBackupIntervalEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetSnapshotBackupIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	prev, err := sw.SetInterval(time.Hour)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 1h", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := sw.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if prev, err := sw.SetInterval(30 * time.Minute); err != nil || prev != 30*time.Minute {
		t.Fatalf("same-value SetInterval(30m) = (%v, %v), want (30m, nil)", prev, err)
	}
	if events := retunedSnapshotBackupIntervalEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetSnapshotBackupIntervalReturnsPreviousValue(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	prev, err := sw.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := sw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 20ms", got)
	}
}

func TestSetSnapshotBackupIntervalNextTickUsesNewInterval(t *testing.T) {
	_, sw, al, dir, cancel := startRetuneTestSnapshotBackup(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence. (Ticks, not
	// files, are the clock here: backup filenames are only
	// second-granular and the version does not move without posts,
	// so fast ticks on an unchanged ledger overwrite the same file
	// instead of adding one.)
	waitFor(t, 3*time.Second, "first tick", func() bool { return sw.ticks.Load() >= 1 })

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := sw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := sw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitFor(t, 3*time.Second, "4 ticks", func() bool { return sw.ticks.Load() >= 4 })
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
	// And the accelerated ticks really land backup files.
	if files := listBackupFiles(t, dir); len(files) < 2 {
		t.Fatalf("files = %d after the accelerated ticks, want at least a full and an incremental", len(files))
	}
}

func TestSetSnapshotBackupIntervalInFlightBackupNotInterrupted(t *testing.T) {
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

	// Drive one backup in a goroutine and revise the interval from
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
	if _, err := sw.SetInterval(5 * time.Minute); err != nil {
		t.Fatalf("SetInterval during an in-flight backup: %v", err)
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
	if got := sw.Interval(); got != 5*time.Minute {
		t.Fatalf("Interval() = %v after the revision, want 5m", got)
	}
}

func TestSetSnapshotBackupIntervalDoesNotSwallowTriggeredTick(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()

	// The first onBackup call blocks the run loop inside the
	// callback, so the next ticker firing buffers in ticker.C
	// (cap 1) while the loop cannot consume it.
	release := make(chan struct{})
	var calls atomic.Uint64
	sw := StartSnapshotBackup(context.Background(), l, SnapshotBackupConfig{
		Dir: dir, Interval: 100 * time.Millisecond,
		KeepFull: 100, KeepIncremental: 100, FullEvery: 100, MaxRetries: 1,
	}, func(_ string, _ bool) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
	})
	if sw == nil {
		t.Fatal("StartSnapshotBackup returned nil for a valid config")
	}
	defer sw.Stop()

	// The first backup lands and the loop blocks in onBackup.
	waitForBackupFiles(t, dir, 1, 3*time.Second)
	// Let the next tick fire while the loop is still blocked: it
	// buffers in ticker.C, unconsumed. (250ms clears the 200ms
	// firing with margin for a slow scheduler.)
	time.Sleep(250 * time.Millisecond)

	// Revise while that tick is pending, then release the loop.
	if _, err := sw.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval with a tick pending: %v", err)
	}
	close(release)

	// The pending tick must still run exactly once — either
	// straight off the ticker channel or via the retune branch's
	// drain — and nothing further fires on the loosened 1h cadence.
	// (10s, not 3s: under a loaded -race full-suite run the worker
	// goroutine can be starved for seconds after the release, and
	// the tick still lands — a short timeout would report that
	// scheduling delay as a swallowed tick.)
	files := waitForBackupFiles(t, dir, 2, 10*time.Second)
	if len(files) != 2 {
		t.Fatalf("files = %d after the revision, want exactly 2 (first backup + drained tick)", len(files))
	}
	time.Sleep(300 * time.Millisecond)
	if files := listBackupFiles(t, dir); len(files) != 2 {
		t.Fatalf("files = %d after settling, want still 2 on the loosened 1h rhythm", len(files))
	}
}

func TestSetSnapshotBackupIntervalFullEveryRotationNotReset(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{FullEvery: 3, MaxRetries: 1})

	// FullEvery=3 alternation: full, incremental, incremental, full.
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 1 = (%q, %v), want (full, true)", kind, ok)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 2 = (%q, %v), want (incremental, true)", kind, ok)
	}
	if _, err := sw.SetInterval(5 * time.Minute); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	// The counter was not reset: backup 3 is still the incremental
	// third of the sequence (a reset would have restarted it full),
	// and backup 4 is the full that closes the cycle.
	kind, ok = sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("backup 3 = (%q, %v), want (incremental, true) — rotation must continue, not restart", kind, ok)
	}
	kind, ok = sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backup 4 = (%q, %v), want (full, true)", kind, ok)
	}
	if sw.backups != 4 {
		t.Fatalf("backups = %d, want 4", sw.backups)
	}
}

func TestSetSnapshotBackupIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, sw, al, dir, cancel := startRetuneTestSnapshotBackup(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the worker
	// ticks underneath, plus direct backupOnce calls from another
	// goroutine: -race must stay silent, and the worker must keep
	// backing up on some positive interval.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = sw.SetInterval(d)
				_ = sw.Interval()
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
	if got := sw.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	// The revision storm rebuilds the ticker on every revision, so
	// the countdown restarts while revisions are still landing and
	// no tick may have fired yet; once the storm ends the last
	// rebuild's ticker fires within one live interval.
	waitFor(t, 3*time.Second, "a tick after the concurrent revisions", func() bool {
		return sw.ticks.Load() >= 1
	})
	waitForBackupFiles(t, dir, 1, 3*time.Second)
}

func TestSetSnapshotBackupIntervalCtxActorAttribution(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetIntervalCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetInterval(2 * time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedSnapshotBackupIntervalEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_backup_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Fatalf("first previous_backup_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := events[0].Details["current_backup_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("first current_backup_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if events[1].Actor != "SetInterval" {
		t.Fatalf("second event Actor = %q, want SetInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_backup_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("second previous_backup_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if got := events[1].Details["current_backup_interval_ms"]; got != float64(2*time.Hour/time.Millisecond) {
		t.Fatalf("second current_backup_interval_ms = %v, want %v", got, float64(2*time.Hour/time.Millisecond))
	}
}

func TestSetSnapshotBackupIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()

	prev, err := sw.SetInterval(30 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("returned previous interval = %v, want 1h", prev)
	}
	events := retunedSnapshotBackupIntervalEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetInterval" {
		t.Errorf("Actor = %q, want SetInterval", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_backup_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Errorf("previous_backup_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := ev.Details["current_backup_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Errorf("current_backup_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	// Only the two numeric ms fields ride in the event: no token,
	// key, or other secret material, and no other keys at all.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current backup interval ms", ev.Details)
	}
	for k := range ev.Details {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "key") {
			t.Errorf("details key %q looks like secret material", k)
		}
	}
}

func TestSetSnapshotBackupIntervalNoRestartContinuousBackups(t *testing.T) {
	_, sw, al, dir, cancel := startRetuneTestSnapshotBackup(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the worker back up on the original interval first.
	waitFor(t, 3*time.Second, "first ticks", func() bool { return sw.ticks.Load() >= 2 })
	ticksBefore := sw.ticks.Load()
	filesBefore := len(listBackupFiles(t, dir))

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the backup count is continuous. (File
	// count is a lower bound, not an exact count: filenames are
	// second-granular and the version does not move without posts,
	// so fast ticks on an unchanged ledger overwrite the same file
	// instead of adding one.)
	if _, err := sw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitFor(t, 3*time.Second, "ticks after the revision", func() bool {
		return sw.ticks.Load() >= ticksBefore+2
	})
	if got := sw.ticks.Load(); got < ticksBefore+2 {
		t.Fatalf("ticks = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if files := listBackupFiles(t, dir); len(files) < filesBefore {
		t.Fatalf("files = %d after the revision, want at least %d (backups continue, none lost)", len(files), filesBefore)
	}
	if got := sw.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 10ms", got)
	}
}

func TestSetSnapshotBackupIntervalGetterFollowsLiveValue(t *testing.T) {
	_, sw, al, _, cancel := startRetuneTestSnapshotBackup(t, time.Hour)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	if got := sw.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v at start, want 1h", got)
	}
	if prev, err := sw.SetInterval(30 * time.Minute); err != nil || prev != time.Hour {
		t.Fatalf("SetInterval(30m) = (%v, %v), want (1h, nil)", prev, err)
	}
	if got := sw.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after tightening, want 30m", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := sw.SetInterval(0); !errors.Is(err, ErrInvalidSnapshotBackupInterval) {
		t.Fatalf("SetInterval(0): err = %v, want ErrInvalidSnapshotBackupInterval", err)
	}
	if got := sw.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after rejected revision, want still 30m", got)
	}
}
