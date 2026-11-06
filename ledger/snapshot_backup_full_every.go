package ledger

import (
	"context"
	"errors"
)

// Snapshot-backup full/incremental alternation cadence runtime revision
// (LG-333).
//
// StartSnapshotBackup fixes the backup worker's full/incremental
// alternation — every FullEvery-th backup is a full snapshot, the rest
// are incremental deltas from the last successful backup's version —
// for the life of the worker: snapshotBackupConfig() (main.go) reads
// LEDGER_SNAPSHOT_BACKUP_FULL_EVERY once at startup, and backupOnce()
// judged each backup's kind from that startup value with no revision
// entry point (SetFullEvery / SetSnapshotBackupFullEvery were whole-repo
// zero hits). The only way to tighten the cadence from every 6th backup
// to every 2nd (suspicious activity just landed and the operator wants
// a sealed full snapshot on disk sooner, shortening the distance to the
// nearest full restore point) or loosen it back to every 24th (the disk
// is hot and the operator wants fewer expensive full exports, more cheap
// incremental links) was to change the env var and restart the process.
// This module is the orthogonal runtime path: an operator revises the
// live worker's alternation cadence in place, instead of restarting the
// server.
//
// Semantics, deliberately fail-closed throughout, along LG-329's
// SnapshotBackup.SetInterval (the same worker, a different knob) and
// LG-323's FXRateWatch.SetWarningThreshold (a threshold read live at
// each decision, no ticker rebuild):
//
//   - The revision is in place: the worker keeps its goroutine, its
//     rotation counter (backups / lastVersion), its ticks counter, and
//     its retention, retry, and encryption configuration — none of
//     them resets. Only the alternation cadence moves. No restart, no
//     Stop + Start, no retune channel: the cadence is a threshold the
//     next backup reads live at kind-selection time, not a time
//     cadence, so there is no ticker to rebuild.
//   - The revision is future-only: a backup already in progress runs
//     to completion on the cadence it started with (a triggered backup
//     is never cut short, its file still lands, the rotation counter
//     still advances), and the new cadence governs from the next
//     backup's kind selection.
//   - Phase semantics (answering the "revision changes phase" question
//     LG-332 (c) left open): the kind is a pure function of the
//     uninterrupted counter, (backups-1)%FullEvery == 0, so a revision
//     never resets backups / lastVersion — the next backup's kind is
//     the counter continued against the new modulus, with no
//     redesign. E.g. with backups=7 when FullEvery moves 6 -> 2, the
//     8th backup is (8-1)%2 == 1 (incremental) and the 9th is
//     (9-1)%2 == 0 (full). FullEvery=1 means every backup from the
//     next one on is full. Loosening the cadence only lengthens the
//     incremental chain, and an incremental snapshot is a delta from
//     the last successful backup's version (base=lastVersion), so the
//     chain stays valid.
//   - Validation is fail-closed with no silent normalization: the new
//     cadence must be > 0, else ErrInvalidSnapshotBackupFullEvery
//     and the live cadence stays exactly as it was. Zero is rejected
//     too — unlike the startup-time "non-positive falls back to the
//     default 6" rule: zero here would divide by zero at the kind
//     check, so a runtime revision must fail closed (along LG-314).
//     "Every backup full" is expressed with the legal value 1; there
//     is no special zero mode.
//   - A revision identical to the live cadence is a no-op: it returns
//     the live value with a nil error and writes no audit event, so
//     a retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     snapshot_backup_full_every_retuned audit event (previous /
//     current cadence as plain integers — purely numeric, no backup
//     content or key material). The Ctx variant attributes the
//     operator (LG-73). Lock order: the setter releases fullEveryMu,
//     then takes l.mu.RLock to emit — fullEveryMu, sw.mu, and l.mu
//     are never held nested, so the lock order is acyclic. backupOnce
//     takes fullEveryMu.RLock only briefly inside sw.mu at
//     kind-selection time and releases it before its audit section
//     takes l.mu.RLock. In particular the setter never calls
//     backupOnce and never takes sw.mu while holding fullEveryMu:
//     backupOnce holds sw.mu for a whole export (including its 100ms
//     x MaxRetries backoff, atomic write, retention pruning, and
//     audit), so a setter that took sw.mu would stall behind a backup
//     in flight. It does not reuse intervalMu either: that lock
//     guards the backup-interval cadence and its retune channel, a
//     different knob.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-332 family): backup tuning is
//     in-process structural config. main.go stores the started
//     worker handle on srv.snapshotBackup so existing operator paths
//     can call it.
//   - Snapshots do not carry the alternation cadence: the cadence
//     comes only from the startup-time LEDGER_SNAPSHOT_BACKUP_FULL_EVERY
//     parse, so a restart restores the env value while the revision
//     events themselves stay in the audit trail for traceability.
//     Persisting a revision across restarts would need the env var to
//     be re-read, a later round.
//   - The cadence is NOT the retention policy, NOT the backup
//     interval, NOT the export retry count, and NOT the encryption
//     config: it only decides whether a future backup is full or
//     incremental. KeepFull / KeepIncremental still decide how many
//     files survive pruning, Interval still decides how often a
//     backup is taken, MaxRetries still decides the export retry
//     backoff, and the LG-77 key ring still decides whether files
//     are sealed — none of them participates in the revision. The
//     ticks counter and ledger_snapshot_backups_total /
//     ledger_snapshot_backups_failed_total keep accumulating across
//     a revision.
//   - A worker that was never started (dir unset or interval
//     invalid, so snapshotBackupConfig() returned false and
//     StartSnapshotBackup was never called) has no handle to revise:
//     the revision applies only to a running worker.
//
// Orthogonality: LG-39 owns the backup rotation mechanism itself
// (this module is its missing runtime revision entry point for the
// alternation cadence), LG-329 owns the same worker's backup interval
// (how often a backup is taken; this module owns how often one of
// them is full — the LG-329 draft explicitly excluded FullEvery from
// interval revision, and this module is that exclusion's fulfillment),
// LG-332 owns the same worker's retention depths (how many files
// survive pruning; this module owns how often a backup is full — the
// LG-332 draft explicitly excluded FullEvery and deferred it here),
// LG-77 owns at-rest encryption of backup files (whether a backup
// lands sealed; this module owns how often one is full — the
// encryption config does not participate in the revision), LG-78 owns
// anchoring a full snapshot's balance Merkle root into the audit chain
// (evidence anchoring; this module owns the evidence-production
// rhythm, and a revision changes no backup file's merkle_root), LG-70
// owns GET /snapshots/diff (a read-only comparison over the backup
// directory; it reads srv.snapshotBackupDir directly with no
// worker-handle dependency, so a cadence revision does not touch its
// 404 fail-closed semantics), LG-24/LG-28 own full/incremental export
// itself (the export semantics; this module owns which one is
// triggered), LG-331 owns the balance-history retention window (a
// different worker), LG-316..LG-322 own the seven sweeper poll
// intervals (the same revision pattern, different workers), and
// LG-323..LG-326 own the FX watch knobs (the same family, different
// workers).

var (
	// ErrInvalidSnapshotBackupFullEvery is returned by
	// SnapshotBackup.SetFullEvery when the requested cadence is not
	// positive. The live cadence is left untouched.
	ErrInvalidSnapshotBackupFullEvery = errors.New("ledger: snapshot backup: invalid full-every cadence: full-every cadence must be positive")
)

// SetFullEvery revises the worker's full/incremental alternation
// cadence in place and reports the pre-revision cadence (for the audit
// trail). The revision is future-only: a backup already in progress
// runs to completion, and only the next backup's kind selection uses
// the new cadence. The rotation counter is never reset: the next
// backup's kind is the uninterrupted counter against the new modulus.
// A revision identical to the live cadence is a no-op that returns the
// live value with a nil error and no side effect. A non-positive
// cadence returns ErrInvalidSnapshotBackupFullEvery with the live
// cadence untouched (the returned value is the live cadence in that
// case too).
func (sw *SnapshotBackup) SetFullEvery(n int) (int, error) {
	return sw.SetFullEveryCtx(context.Background(), n)
}

// SetFullEveryCtx is SetFullEvery with per-call audit actor attribution
// (LG-73).
func (sw *SnapshotBackup) SetFullEveryCtx(ctx context.Context, n int) (int, error) {
	sw.fullEveryMu.Lock()
	previous := sw.fullEvery
	if n <= 0 {
		sw.fullEveryMu.Unlock()
		return previous, ErrInvalidSnapshotBackupFullEvery
	}
	if n == previous {
		sw.fullEveryMu.Unlock()
		return previous, nil
	}
	sw.fullEvery = n
	sw.fullEveryMu.Unlock()

	// Audit after the swap, under l.mu.RLock like LG-329 and the
	// LG-316..LG-332 retunes: fullEveryMu and l.mu are never held
	// nested, so the lock order is acyclic. emitAuditCtx is a no-op
	// when no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two integer cadences: no backup content, token, key, or other
	// secret material ever rides in a retune event.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_full_every_retuned",
		Actor:         "SetFullEvery",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_full_every": previous,
			"current_full_every":  n,
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// FullEvery reports the worker's live full/incremental alternation
// cadence: every FullEvery-th backup is a full snapshot.
func (sw *SnapshotBackup) FullEvery() int {
	sw.fullEveryMu.RLock()
	defer sw.fullEveryMu.RUnlock()
	return sw.fullEvery
}
