package ledger

import (
	"context"
	"errors"
)

// Snapshot-backup retention depths runtime revision (LG-332).
//
// StartSnapshotBackup fixes the backup worker's retention depths —
// how many of the newest full and incremental snapshots survive the
// per-backup pruning — for the life of the worker:
// snapshotBackupConfig() (main.go) reads
// LEDGER_SNAPSHOT_BACKUP_KEEP_FULL (default 7) and
// LEDGER_SNAPSHOT_BACKUP_KEEP_INCR (default 24) once at startup, and
// backupOnce() pruned on those values with no revision entry point
// (SetSnapshotBackupKeepFull / SetSnapshotBackupKeepIncremental were
// whole-repo zero hits). The only way to tighten the full retention
// from 7 to 3 (the disaster-recovery disk is hot and old full
// snapshots have to go) or loosen the incremental retention from 24
// to 72 (suspicious activity just landed and the operator wants a
// denser incremental chain kept, lengthening the recoverable window)
// was to change the env vars and restart the process. This module is
// the orthogonal runtime path: an operator revises the live worker's
// retention depths in place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-323's
// FXRateWatch.SetWarningThreshold (a per-tick threshold knob, read
// live with no ticker rebuild) and LG-329's
// SnapshotBackup.SetInterval (the same worker):
//
//   - The revision is in place: the worker keeps its goroutine, its
//     rotation counter (backups / lastVersion), its ticks counter,
//     and its cadence, alternation, retry, and encryption
//     configuration — none of them resets. Only the retention depth
//     moves. No restart, no Stop + Start, no ticker rebuild. The
//     next backup's kind still follows the FullEvery rotation from
//     the uninterrupted counter, so tightening the retention never
//     restarts the full/incremental alternation.
//   - The revision is future-only: a backup already in progress runs
//     to completion on the old depths (its export lands, its file
//     is written, its rotation counter advances), and the new depths
//     govern from the next backup's pruning. The depths are
//     thresholds the backup reads live, not a cadence, so the
//     revision needs no retune wakeup — along LG-323, the new values
//     simply take effect at the next backup.
//   - The revision itself deletes no files. This answers the policy
//     question LG-329 (c) left open ("does shortening delete files
//     immediately"): shortening only takes effect through the next
//     backup's regular pruning (files past the new depth are deleted
//     by that backup, exactly as that depth's normal pruning
//     behaves), and there is no out-of-band sweep. Lengthening keeps
//     old files from the next backup on.
//   - Validation is fail-closed with no silent normalization: each
//     new depth must be > 0, else ErrInvalidSnapshotBackupKeepFull /
//     ErrInvalidSnapshotBackupKeepIncremental and the live depth
//     stays exactly as it was. Zero is rejected too — there is no
//     "0 = keep forever" mode, and the startup-time "non-positive
//     normalizes to the default" rule is never reused for a runtime
//     revision (along LG-314): zero kept would make the next
//     backup's pruning delete every file of that kind. A runtime
//     revision never silently normalizes.
//   - A revision identical to the live depth is a no-op: it returns
//     the live value with a nil error and writes no audit event, so
//     a retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     snapshot_backup_keep_full_retuned or
//     snapshot_backup_keep_incremental_retuned audit event
//     (previous / current depth as plain integers — purely numeric,
//     no backup content or key material). The Ctx variants
//     attribute the operator (LG-73). Lock order: the setter
//     releases retentionMu, then takes l.mu.RLock to emit —
//     retentionMu, sw.mu, and l.mu are never held nested, so the
//     lock order is acyclic: backupOnce holds sw.mu while it reads
//     the depths under retentionMu.RLock (sw.mu -> retentionMu) and
//     audits under l.mu.RLock (sw.mu -> l.mu), and the setter goes
//     retentionMu -> l.mu; all three paths flow toward l.mu, never
//     in a cycle. In particular the setter never calls backupOnce
//     and never takes sw.mu while holding retentionMu: backupOnce
//     holds sw.mu for a whole export (including its 100ms x
//     MaxRetries backoff, atomic write, retention pruning, and
//     audit), so a setter that took sw.mu would stall behind a
//     backup in flight.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-331 family): retention tuning is
//     in-process structural config. main.go stores the started
//     worker handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the retention depths: the depths come
//     only from the startup-time LEDGER_SNAPSHOT_BACKUP_KEEP_FULL /
//     LEDGER_SNAPSHOT_BACKUP_KEEP_INCR parse, so a restart restores
//     the env values while the revision events themselves stay in
//     the audit trail for traceability. Persisting a revision across
//     restarts would need the env vars to be re-read, a later round.
//   - The depths are NOT the backup cadence, the alternation, the
//     retry policy, or the encryption config: they only decide how
//     many files survive pruning. Interval still decides how often
//     a backup is taken, FullEvery still decides the
//     full/incremental alternation, MaxRetries still decides the
//     export retry backoff, and the LG-77 key ring still decides
//     whether files are sealed — none of them participates in the
//     revision. The ticks counter and ledger_snapshot_backups_total
//     / ledger_snapshot_backups_failed_total keep accumulating
//     across a revision.
//   - A worker that was never started (dir unset or interval
//     invalid, so snapshotBackupConfig() returned false and
//     StartSnapshotBackup was never called) has no handle to revise:
//     the revision applies only to a running worker.
//
// Orthogonality: LG-39 owns the backup rotation mechanism itself
// (this module is its missing runtime revision entry point for the
// retention depths), LG-329 owns the same worker's backup interval
// (how often a backup is taken; this module owns how many are kept
// — the LG-329 draft explicitly excluded the retention depths and
// deferred them to a separate item, and this module is that
// fulfillment), LG-77 owns at-rest encryption of backup files
// (whether a backup lands sealed; this module owns how many land —
// the encryption config does not participate in the revision),
// LG-78 owns anchoring a full snapshot's balance Merkle root into
// the audit chain (evidence anchoring; this module owns the evidence
// retention depth, and a revision changes no backup file's
// merkle_root), LG-70 owns GET /snapshots/diff (a read-only
// comparison over the backup directory; it reads srv.snapshotBackupDir
// directly with no worker-handle dependency, so a retention revision
// does not touch its 404 fail-closed semantics), LG-24/LG-28 own
// full/incremental export itself (the export semantics; this module
// owns the retention depths), LG-331 owns the balance-history
// retention window (the same revision pattern, a different worker —
// disaster-recovery files versus the observability time series),
// LG-316..LG-322 own the seven sweeper poll intervals (the same
// revision pattern, different workers), LG-323/324/325/326 own the
// FX watch knobs (the same family, different workers), and LG-314
// owns the audit-sink idle poll interval (the same (previous, error)
// return and audit shape, a different component).

var (
	// ErrInvalidSnapshotBackupKeepFull is returned by
	// SnapshotBackup.SetKeepFull when the requested full retention
	// depth is not positive. The live depth is left untouched.
	ErrInvalidSnapshotBackupKeepFull = errors.New("ledger: snapshot backup: invalid full retention depth: keep-full must be positive")
	// ErrInvalidSnapshotBackupKeepIncremental is returned by
	// SnapshotBackup.SetKeepIncremental when the requested
	// incremental retention depth is not positive. The live depth
	// is left untouched.
	ErrInvalidSnapshotBackupKeepIncremental = errors.New("ledger: snapshot backup: invalid incremental retention depth: keep-incremental must be positive")
)

// SetKeepFull revises the worker's full-backup retention depth in
// place and reports the pre-revision depth (for the audit trail).
// The revision is future-only: a backup already in progress runs to
// completion on the old depth, and only the next backup's pruning
// uses the new one. The revision itself deletes no files —
// tightening takes effect through the next backup's regular pruning.
// A revision identical to the live depth is a no-op that returns the
// live value with a nil error and no side effect. A non-positive
// depth returns ErrInvalidSnapshotBackupKeepFull with the live depth
// untouched (the returned value is the live depth in that case too).
func (sw *SnapshotBackup) SetKeepFull(n int) (int, error) {
	return sw.SetKeepFullCtx(context.Background(), n)
}

// SetKeepFullCtx is SetKeepFull with per-call audit actor attribution
// (LG-73).
func (sw *SnapshotBackup) SetKeepFullCtx(ctx context.Context, n int) (int, error) {
	sw.retentionMu.Lock()
	previous := sw.keepFull
	if n <= 0 {
		sw.retentionMu.Unlock()
		return previous, ErrInvalidSnapshotBackupKeepFull
	}
	if n == previous {
		sw.retentionMu.Unlock()
		return previous, nil
	}
	sw.keepFull = n
	sw.retentionMu.Unlock()

	// No retune wakeup: the depths are per-backup thresholds the
	// backup reads live, not a cadence — along LG-323, the new
	// values take effect at the next backup's pruning without
	// rebuilding anything. The revision itself deletes no files.

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-331 retunes: retentionMu and l.mu are never held
	// nested, so the lock order is acyclic. emitAuditCtx is a no-op
	// when no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two integer depths: no backup content, path, or key material
	// ever rides in a retune event.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_keep_full_retuned",
		Actor:         "SetKeepFull",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_keep_full": previous,
			"current_keep_full":  n,
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// SetKeepIncremental revises the worker's incremental-backup
// retention depth in place and reports the pre-revision depth (for
// the audit trail). The semantics mirror SetKeepFull: future-only,
// the revision itself deletes no files, same-value is a no-op, and a
// non-positive depth returns
// ErrInvalidSnapshotBackupKeepIncremental with the live depth
// untouched (the returned value is the live depth in that case too).
func (sw *SnapshotBackup) SetKeepIncremental(n int) (int, error) {
	return sw.SetKeepIncrementalCtx(context.Background(), n)
}

// SetKeepIncrementalCtx is SetKeepIncremental with per-call audit
// actor attribution (LG-73).
func (sw *SnapshotBackup) SetKeepIncrementalCtx(ctx context.Context, n int) (int, error) {
	sw.retentionMu.Lock()
	previous := sw.keepIncremental
	if n <= 0 {
		sw.retentionMu.Unlock()
		return previous, ErrInvalidSnapshotBackupKeepIncremental
	}
	if n == previous {
		sw.retentionMu.Unlock()
		return previous, nil
	}
	sw.keepIncremental = n
	sw.retentionMu.Unlock()

	// No retune wakeup: see SetKeepFullCtx.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_keep_incremental_retuned",
		Actor:         "SetKeepIncremental",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_keep_incremental": previous,
			"current_keep_incremental":  n,
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// KeepFull reports the worker's live full-backup retention depth.
// KeepIncremental reports the worker's live incremental-backup
// retention depth.
func (sw *SnapshotBackup) KeepFull() int {
	sw.retentionMu.RLock()
	defer sw.retentionMu.RUnlock()
	return sw.keepFull
}

func (sw *SnapshotBackup) KeepIncremental() int {
	sw.retentionMu.RLock()
	defer sw.retentionMu.RUnlock()
	return sw.keepIncremental
}
