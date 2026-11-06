package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Snapshot-backup encryption fail-closed posture runtime revision
// (LG-337): SetEncryptFailClosed revises the live no-key posture in
// place — toward fail-closed stops plaintext backups from the next
// tick, toward fail-open restores the backup flow — with no restart
// and no key rotation. Tests reuse mustAuditLog from
// ledger_audit_test.go, mustPostEntry / testBackupWorker /
// listBackupFiles from snapshot_backup_test.go, mustParseKeys /
// testKey1Hex from ledger_snapshot_backup_crypt_test.go, and
// auditEventsByOp from ledger_actor_test.go.

// failClosedTestWorker starts a backup worker with a long ticker
// interval so the goroutine never fires during the test; tests drive
// backupOnce directly for determinism.
func failClosedTestWorker(t *testing.T, failClosed bool) (*Ledger, *SnapshotBackup, *AuditLog, string) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 100, MaxRetries: 1,
		Encryption: BackupEncryptionConfig{FailClosed: failClosed},
	})
	return l, sw, al, dir
}

// failClosedTestWorkerWithKeys is failClosedTestWorker with an
// encryption key configured, so the sealing path is live.
func failClosedTestWorkerWithKeys(t *testing.T, failClosed bool) (*Ledger, *SnapshotBackup, *AuditLog, string, BackupEncryptionConfig) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	enc := mustParseKeys(t, testKey1Hex)
	enc.FailClosed = failClosed
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 100, MaxRetries: 1,
		Encryption: enc,
	})
	return l, sw, al, dir, enc
}

// readFailClosedAuditEvents stops the worker, closes the audit log
// (which drains the async writer), and returns every audit event
// recorded in it, grouped by op. The ledger must not be used after
// this call.
func readFailClosedAuditEvents(t *testing.T, sw *SnapshotBackup, al *AuditLog) map[string][]AuditEvent {
	t.Helper()
	sw.Stop()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	return auditEventsByOp(t, al.Dir())
}

// auditDetailBool reads one boolean audit detail.
func auditDetailBool(t *testing.T, ev AuditEvent, key string) bool {
	t.Helper()
	v, ok := ev.Details[key]
	if !ok {
		t.Fatalf("audit event %q has no detail %q (details: %v)", ev.Op, key, ev.Details)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("audit detail %q = %T %v, want a JSON bool", key, v, v)
	}
	return b
}

func TestSetEncryptFailClosedFalseToTrueReturnsPreviousAndAudits(t *testing.T) {
	_, sw, al, _ := failClosedTestWorker(t, false)
	mustPostEntry(t, sw.l, "fc-1")
	prev, err := sw.SetEncryptFailClosed(true)
	if err != nil || prev != false {
		t.Fatalf("SetEncryptFailClosed(true) = (%v, %v), want (false, nil)", prev, err)
	}
	if got := sw.EncryptFailClosed(); got != true {
		t.Fatalf("EncryptFailClosed() = %v after the revision, want true", got)
	}
	events := readFailClosedAuditEvents(t, sw, al)["snapshot_backup_encrypt_fail_closed_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetEncryptFailClosed" {
		t.Fatalf("event Actor = %q, want SetEncryptFailClosed", ev.Actor)
	}
	if got := auditDetailBool(t, ev, "previous_fail_closed"); got != false {
		t.Fatalf("previous_fail_closed = %v, want false", got)
	}
	if got := auditDetailBool(t, ev, "current_fail_closed"); got != true {
		t.Fatalf("current_fail_closed = %v, want true", got)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetEncryptFailClosedTrueToFalseReturnsPrevious(t *testing.T) {
	_, sw, al, _ := failClosedTestWorker(t, true)
	if got := sw.EncryptFailClosed(); got != true {
		t.Fatalf("EncryptFailClosed() on a fail-closed worker = %v, want true", got)
	}
	prev, err := sw.SetEncryptFailClosed(false)
	if err != nil || prev != true {
		t.Fatalf("SetEncryptFailClosed(false) = (%v, %v), want (true, nil)", prev, err)
	}
	if got := sw.EncryptFailClosed(); got != false {
		t.Fatalf("EncryptFailClosed() = %v after the revision, want false", got)
	}
	events := readFailClosedAuditEvents(t, sw, al)["snapshot_backup_encrypt_fail_closed_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if got := auditDetailBool(t, events[0], "previous_fail_closed"); got != true {
		t.Fatalf("previous_fail_closed = %v, want true", got)
	}
	if got := auditDetailBool(t, events[0], "current_fail_closed"); got != false {
		t.Fatalf("current_fail_closed = %v, want false", got)
	}
}

func TestSetEncryptFailClosedSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, _ := failClosedTestWorker(t, false)
	prev, err := sw.SetEncryptFailClosed(false)
	if err != nil || prev != false {
		t.Fatalf("SetEncryptFailClosed(false) = (%v, %v), want (false, nil)", prev, err)
	}
	events := readFailClosedAuditEvents(t, sw, al)["snapshot_backup_encrypt_fail_closed_retuned"]
	if len(events) != 0 {
		t.Fatalf("retuned events = %d for a same-value revision, want 0", len(events))
	}
}

func TestSetEncryptFailClosedToClosedNextTickFailsWithoutKey(t *testing.T) {
	_, sw, al, dir := failClosedTestWorker(t, false)
	mustPostEntry(t, sw.l, "fc-closed-1")
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true): %v", err)
	}
	kind, ok := sw.backupOnce()
	if ok {
		t.Fatal("fail-closed tick succeeded without a key")
	}
	if kind != "full" {
		t.Fatalf("kind = %q, want full", kind)
	}
	if names := listBackupFiles(t, dir); len(names) != 0 {
		t.Fatalf("files = %v, want none (refused, not plaintext)", names)
	}
	events := readFailClosedAuditEvents(t, sw, al)
	if refusals := len(events["snapshot_backup_encryption_missing"]); refusals != 1 {
		t.Fatalf("snapshot_backup_encryption_missing events = %d, want exactly 1", refusals)
	}
	if len(events["snapshot_backup"]) != 0 {
		t.Fatal("snapshot_backup event emitted for a refused tick")
	}
}

func TestSetEncryptFailClosedBackToOpenNextTickWritesPlaintext(t *testing.T) {
	_, sw, al, dir := failClosedTestWorker(t, false)
	mustPostEntry(t, sw.l, "fc-open-1")
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true): %v", err)
	}
	if _, ok := sw.backupOnce(); ok {
		t.Fatal("fail-closed tick succeeded without a key")
	}
	if _, err := sw.SetEncryptFailClosed(false); err != nil {
		t.Fatalf("SetEncryptFailClosed(false): %v", err)
	}
	// The refused fail-closed tick already consumed the full rotation
	// slot (backupOnce increments the counter up front), so this
	// tick is the incremental second of the alternation.
	kind, ok := sw.backupOnce()
	if !ok || kind != "incremental" {
		t.Fatalf("fail-open tick = (%q, %v), want (incremental, true)", kind, ok)
	}
	files := listBackupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v, want one plaintext backup", files)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, encrypted, err := DecryptSnapshotBackup(data, nil); err != nil || encrypted {
		t.Fatalf("backup file encrypted = %v, err = %v; want plaintext", encrypted, err)
	}
	events := readFailClosedAuditEvents(t, sw, al)
	if warnings := len(events["snapshot_backup_unencrypted"]); warnings != 1 {
		t.Fatalf("snapshot_backup_unencrypted events = %d, want exactly 1", warnings)
	}
}

func TestSetEncryptFailClosedWarningsRearmPerPosture(t *testing.T) {
	_, sw, al, _ := failClosedTestWorker(t, false)
	mustPostEntry(t, sw.l, "fc-rearm-1")
	// Fail-open tick 1: plaintext + the one-time warning.
	if _, ok := sw.backupOnce(); !ok {
		t.Fatal("fail-open tick 1 failed")
	}
	// Flip to fail-closed: the refusal warning is re-armed.
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true): %v", err)
	}
	if _, ok := sw.backupOnce(); ok {
		t.Fatal("fail-closed tick succeeded without a key")
	}
	// Flip back to fail-open: the plaintext warning is re-armed, so
	// the next plaintext tick warns again instead of staying silent
	// on a warning the operator already saw under the old posture.
	if _, err := sw.SetEncryptFailClosed(false); err != nil {
		t.Fatalf("SetEncryptFailClosed(false): %v", err)
	}
	if _, ok := sw.backupOnce(); !ok {
		t.Fatal("fail-open tick 2 failed")
	}
	// Flip to fail-closed once more: the refusal warning re-arms
	// the other way too.
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true) again: %v", err)
	}
	if _, ok := sw.backupOnce(); ok {
		t.Fatal("second fail-closed tick succeeded without a key")
	}
	events := readFailClosedAuditEvents(t, sw, al)
	if warnings := len(events["snapshot_backup_unencrypted"]); warnings != 2 {
		t.Fatalf("snapshot_backup_unencrypted events = %d, want 2 (one per fail-open posture entry)", warnings)
	}
	if refusals := len(events["snapshot_backup_encryption_missing"]); refusals != 2 {
		t.Fatalf("snapshot_backup_encryption_missing events = %d, want 2 (one per fail-closed posture entry)", refusals)
	}
}

func TestSetEncryptFailClosedWithKeySealingUnaffected(t *testing.T) {
	_, sw, al, dir, enc := failClosedTestWorkerWithKeys(t, false)
	mustPostEntry(t, sw.l, "fc-key-1")
	// Flip both ways with a key configured: the sealing path never
	// consults the posture, so every tick still lands encrypted
	// under the same active key id.
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true): %v", err)
	}
	if kind, ok := sw.backupOnce(); !ok || kind != "full" {
		t.Fatalf("tick under fail-closed with a key = (%q, %v), want (full, true)", kind, ok)
	}
	if _, err := sw.SetEncryptFailClosed(false); err != nil {
		t.Fatalf("SetEncryptFailClosed(false): %v", err)
	}
	if kind, ok := sw.backupOnce(); !ok || kind != "incremental" {
		t.Fatalf("tick under fail-open with a key = (%q, %v), want (incremental, true)", kind, ok)
	}
	files := listBackupFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("files = %v, want 2 encrypted backups", files)
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if _, encrypted, err := DecryptSnapshotBackup(data, enc.Keys); err != nil || !encrypted {
			t.Fatalf("backup %s encrypted = %v, err = %v; want sealed", name, encrypted, err)
		}
	}
	events := readFailClosedAuditEvents(t, sw, al)
	for _, ev := range events["snapshot_backup"] {
		if ev.Details["key_id"] != enc.ActiveKeyID {
			t.Fatalf("backup key_id = %v, want %s", ev.Details["key_id"], enc.ActiveKeyID)
		}
	}
}

func TestSetEncryptFailClosedInFlightBackupNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	// A fuller journal makes the export take a real (if short) while,
	// so the revision below lands while the backup is in flight.
	for i := 0; i < 200; i++ {
		mustPostEntry(t, l, fmt.Sprintf("fc-inflight-%d", i))
	}
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 100, KeepIncremental: 100, FullEvery: 2, MaxRetries: 1,
		Encryption: BackupEncryptionConfig{FailClosed: true},
	})
	// Drive one backup in a goroutine and flip the posture from this
	// goroutine at the same time: the revision must not cut the
	// in-flight backup short, and it never takes sw.mu, so it cannot
	// stall behind it either. The flip lands during the export, so
	// the tick's encryption branch reads the live fail-open value
	// and the file still lands — plaintext — instead of refusing.
	type result struct {
		kind string
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		kind, ok := sw.backupOnce()
		done <- result{kind, ok}
	}()
	if _, err := sw.SetEncryptFailClosed(false); err != nil {
		t.Fatalf("SetEncryptFailClosed during an in-flight backup: %v", err)
	}
	select {
	case r := <-done:
		if !r.ok || r.kind != "full" {
			t.Fatalf("in-flight backupOnce = (%q, %v), want (full, true)", r.kind, r.ok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight backup never finished")
	}
	// Its file still landed, plaintext under the live posture.
	files := listBackupFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v, want one snapshot-full-* from the in-flight backup", files)
	}
	if HasBackupEncryption(mustReadFile(t, filepath.Join(dir, files[0]))) {
		t.Fatalf("in-flight file %s is encrypted, want plaintext under the flipped posture", files[0])
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
	if got := sw.EncryptFailClosed(); got != false {
		t.Fatalf("EncryptFailClosed() = %v after the revision, want false", got)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

func TestSetEncryptFailClosedCtxRecordsOperator(t *testing.T) {
	_, sw, al, _ := failClosedTestWorker(t, false)
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetEncryptFailClosedCtx(ctx, true); err != nil {
		t.Fatalf("SetEncryptFailClosedCtx: %v", err)
	}
	events := readFailClosedAuditEvents(t, sw, al)["snapshot_backup_encrypt_fail_closed_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("event Actor = %q, want ops-alan", events[0].Actor)
	}
}

func TestSetEncryptFailClosedConcurrentFlipAndTicksRaceFree(t *testing.T) {
	_, sw, _, _ := failClosedTestWorker(t, false)
	mustPostEntry(t, sw.l, "fc-race-1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := sw.SetEncryptFailClosed((n+j)%2 == 0); err != nil {
					t.Errorf("SetEncryptFailClosed: %v", err)
					return
				}
				_ = sw.EncryptFailClosed()
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				// A tick under either posture is fine: fail-open
				// writes plaintext, fail-closed refuses — both are
				// legal outcomes of the live posture.
				sw.backupOnce()
			}
		}()
	}
	wg.Wait()
}

func TestSetEncryptFailClosedOtherKnobsUntouched(t *testing.T) {
	_, sw, _, _ := failClosedTestWorker(t, false)
	intervalBefore := sw.Interval()
	keepFullBefore := sw.KeepFull()
	keepIncrBefore := sw.KeepIncremental()
	fullEveryBefore := sw.FullEvery()
	backupsBefore := sw.backups
	if _, err := sw.SetEncryptFailClosed(true); err != nil {
		t.Fatalf("SetEncryptFailClosed(true): %v", err)
	}
	if _, err := sw.SetEncryptFailClosed(false); err != nil {
		t.Fatalf("SetEncryptFailClosed(false): %v", err)
	}
	if got := sw.Interval(); got != intervalBefore {
		t.Fatalf("Interval() moved %v -> %v across the flips, want unchanged", intervalBefore, got)
	}
	if got := sw.KeepFull(); got != keepFullBefore {
		t.Fatalf("KeepFull() moved %d -> %d across the flips, want unchanged", keepFullBefore, got)
	}
	if got := sw.KeepIncremental(); got != keepIncrBefore {
		t.Fatalf("KeepIncremental() moved %d -> %d across the flips, want unchanged", keepIncrBefore, got)
	}
	if got := sw.FullEvery(); got != fullEveryBefore {
		t.Fatalf("FullEvery() moved %d -> %d across the flips, want unchanged", fullEveryBefore, got)
	}
	if sw.backups != backupsBefore {
		t.Fatalf("backups moved %d -> %d across the flips, want unchanged", backupsBefore, sw.backups)
	}
}

func TestSetEncryptFailClosedRestartRestoresEnvValue(t *testing.T) {
	// A fresh worker seeds its live posture from the startup-time
	// config — a restart restores the env value, not the revision.
	_, swOpen, _, _ := failClosedTestWorker(t, false)
	if got := swOpen.EncryptFailClosed(); got != false {
		t.Fatalf("EncryptFailClosed() on a fail-open worker = %v, want false", got)
	}
	_, swClosed, _, _ := failClosedTestWorker(t, true)
	if got := swClosed.EncryptFailClosed(); got != true {
		t.Fatalf("EncryptFailClosed() on a fail-closed worker = %v, want true", got)
	}
}
