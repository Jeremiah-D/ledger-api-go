package ledger

import (
	"context"
	"errors"
	"time"
)

// Snapshot-backup interval runtime revision (LG-329).
//
// StartSnapshotBackup fixes the backup worker's cadence — how often a
// disaster-recovery snapshot is exported to the backup directory —
// for the life of the worker: snapshotBackupConfig() (main.go) reads
// LEDGER_SNAPSHOT_BACKUP_INTERVAL once at startup, and run() built
// its single ticker from that value with no revision entry point
// (SetSnapshotBackupInterval was a whole-repo zero hit). The only way
// to tighten the cadence from 1h to 5m (a large posting or suspicious
// activity just landed and the operator wants disaster-recovery
// snapshots on disk sooner, shortening the RPO) or loosen it back to
// save export I/O and audit noise while the disk is hot was to change
// the env var and restart the process. This module is the orthogonal
// runtime path: an operator revises the live worker's backup interval
// in place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-324's
// FXRateWatch.SetInterval (the same retune shape, a different worker)
// and the LG-316 ReportSweeper pattern:
//
//   - The revision is in place: the worker keeps its goroutine, its
//     rotation counter (backups / lastVersion), its ticks counter,
//     and its retention and encryption configuration — none of them
//     resets. Only the backup cadence moves. No restart, no Stop +
//     Start. The next backup's kind still follows the FullEvery
//     rotation from the uninterrupted counter, so tightening the
//     cadence never restarts the full/incremental alternation.
//   - The revision is future-only: the run loop selects between
//     ticks, so a backup already in progress runs to completion on
//     the old cadence (a triggered backup is never cut short, its
//     file still lands, the rotation counter still advances), and
//     the new interval governs from the next tick. The loop
//     rebuilds its ticker inside run(): the old ticker is stopped
//     before the new one is built, so two tickers never run in
//     parallel, and a tick that fired while the revision was in
//     flight is run before the rebuild, so a revision cannot
//     swallow a due backup.
//   - Validation is fail-closed with no silent normalization: the
//     new interval must be > 0, else ErrInvalidSnapshotBackupInterval
//     and the live interval stays exactly as it was. Zero is
//     rejected too — unlike the startup-time "non-positive disables
//     the worker" rule: zero here would stop the whole worker, and
//     a runtime revision never silently disables it (along LG-314 /
//     LG-316 / LG-324).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     snapshot_backup_interval_retuned audit event (previous /
//     current backup interval as millisecond integers — purely
//     numeric, no token or key material). The Ctx variant attributes
//     the operator (LG-73). Lock order: the setter releases
//     intervalMu, then takes l.mu.RLock to emit — intervalMu, sw.mu,
//     and l.mu are never held nested, so the lock order is acyclic.
//     run() reads the interval under intervalMu.RLock when it
//     rebuilds the ticker, so the two paths never nest either. In
//     particular the setter never calls backupOnce and never takes
//     sw.mu while holding intervalMu: backupOnce holds sw.mu for a
//     whole export (including its 100ms x MaxRetries backoff, atomic
//     write, retention pruning, and audit), so a setter that took
//     sw.mu would stall behind a backup in flight.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-328 family): backup tuning is
//     in-process structural config. main.go stores the started
//     worker handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the backup interval: the interval comes
//     only from the startup-time LEDGER_SNAPSHOT_BACKUP_INTERVAL
//     parse, so a restart restores the env value while the revision
//     events themselves stay in the audit trail for traceability.
//     Persisting a revision across restarts would need the env var
//     to be re-read, a later round.
//   - The interval is NOT the retention policy and NOT the
//     encryption config: it only decides how often a backup is
//     taken. KeepFull / KeepIncremental still decide how many files
//     survive pruning, FullEvery still decides the full/incremental
//     alternation, MaxRetries still decides the export retry
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
// backup cadence), LG-77 owns at-rest encryption of backup files
// (whether a backup lands sealed; this module owns how often one
// lands — the encryption config does not participate in the
// revision), LG-78 owns anchoring a full snapshot's balance Merkle
// root into the audit chain (evidence anchoring; this module owns
// the evidence-production cadence, and a revision changes no
// backup file's merkle_root), LG-70 owns GET /snapshots/diff (a
// read-only comparison over the backup directory; it reads
// srv.snapshotBackupDir directly with no worker-handle dependency,
// so a cadence revision does not touch its 404 fail-closed
// semantics), LG-24/LG-28 own full/incremental export itself (the
// export semantics; this module owns the trigger cadence),
// LG-316..LG-322 own the seven sweeper poll intervals (the same
// revision pattern, different workers), LG-323/LG-324/LG-325/LG-326
// own the FX watch knobs (the same family, different workers),
// LG-327 owns the health-watch scan interval and LG-328 owns the
// TSA anchor interval (the same family, different workers — the
// LG-328 draft explicitly deferred this item as the first item of
// the next round, and this module is that fulfillment), and LG-314
// owns the audit-sink idle poll interval (the same (previous,
// error) return and audit shape, a different component).

var (
	// ErrInvalidSnapshotBackupInterval is returned by
	// SnapshotBackup.SetInterval when the requested interval is not
	// positive. The live interval is left untouched.
	ErrInvalidSnapshotBackupInterval = errors.New("ledger: snapshot backup: invalid backup interval: backup interval must be positive")
)

// SetInterval revises the worker's backup interval in place and
// reports the pre-revision interval (for the audit trail). The
// revision is future-only: a backup already in progress runs to
// completion, and only the next tick uses the new interval; a tick
// that fired while the revision was in flight is run before the
// ticker is rebuilt, so it is never swallowed. A revision identical
// to the live interval is a no-op that returns the live value with a
// nil error and no side effect. A non-positive interval returns
// ErrInvalidSnapshotBackupInterval with the live interval untouched
// (the returned value is the live interval in that case too).
func (sw *SnapshotBackup) SetInterval(d time.Duration) (time.Duration, error) {
	return sw.SetIntervalCtx(context.Background(), d)
}

// SetIntervalCtx is SetInterval with per-call audit actor attribution
// (LG-73).
func (sw *SnapshotBackup) SetIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	sw.intervalMu.Lock()
	previous := sw.interval
	if d <= 0 {
		sw.intervalMu.Unlock()
		return previous, ErrInvalidSnapshotBackupInterval
	}
	if d == previous {
		sw.intervalMu.Unlock()
		return previous, nil
	}
	sw.interval = d
	sw.intervalMu.Unlock()

	// Wake the run loop so it rebuilds its ticker on the new
	// interval. The loop re-reads the live value under
	// intervalMu.RLock, so a newer revision supersedes this one
	// even if its wakeup was coalesced into this one; the send is
	// non-blocking so a revision never stalls on a loop that is
	// mid-backup.
	select {
	case sw.retune <- struct{}{}:
	default:
	}

	// Audit after the swap, under l.mu.RLock like LG-314 and the
	// LG-316..LG-328 retunes: intervalMu and l.mu are never held
	// nested, so the lock order is acyclic. emitAuditCtx is a no-op
	// when no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two numeric millisecond intervals: no token, key, or other
	// secret material ever rides in a retune event.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_interval_retuned",
		Actor:         "SetInterval",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_backup_interval_ms": previous.Milliseconds(),
			"current_backup_interval_ms":  d.Milliseconds(),
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// Interval reports the worker's live backup interval.
func (sw *SnapshotBackup) Interval() time.Duration {
	sw.intervalMu.RLock()
	defer sw.intervalMu.RUnlock()
	return sw.interval
}
