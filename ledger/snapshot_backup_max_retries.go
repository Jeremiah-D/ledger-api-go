package ledger

import (
	"context"
	"errors"
)

// Snapshot-backup export retry budget runtime revision (LG-344).
//
// StartSnapshotBackup fixes the backup worker's export retry budget —
// a failed export is retried up to MaxRetries times with a 100ms
// backoff before the tick counts as failed — for the life of the
// worker: snapshotBackupConfig() (main.go) has no env knob for it at
// all, so the startup value is always the default 3, and backupOnce()
// judged each tick's retries from that startup value with no revision
// entry point (SetMaxRetries / SetSnapshotBackupMaxRetries were
// whole-repo zero hits). The only way to widen the budget from 3 to 8
// (the disk is flapping and the operator wants one tick to ride out
// several more transient failures instead of dropping a backup) or
// tighten it to 0 (exports keep failing on a full disk and the
// operator wants fail-fast: one attempt, no wasted backoff sleeps or
// repeated full-export read locks) was to change the code and
// redeploy. This module is the orthogonal runtime path: an operator
// revises the live worker's retry budget in place, instead of
// restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-333's
// SnapshotBackup.SetFullEvery (the same worker, a different knob) and
// LG-323's FXRateWatch.SetWarningThreshold (a threshold read live at
// each decision, no ticker rebuild):
//
//   - The revision is in place: the worker keeps its goroutine, its
//     rotation counter (backups / lastVersion), its ticks counter, and
//     its interval, retention, cadence, and encryption configuration
//     — none of them resets. Only the retry budget moves. No restart,
//     no Stop + Start, no retune channel: the budget is a threshold
//     the next backup reads live at its retry loop's entry, not a
//     time cadence, so there is no ticker to rebuild.
//   - The revision is future-only: a backup already in progress runs
//     to completion on the budget it started with (backupOnce reads
//     the live value once into a local at the loop's entry, so a
//     revision mid-tick never rewrites an in-flight budget), and the
//     new budget governs from the next backup.
//   - Validation is fail-closed with no silent normalization: the new
//     budget must be >= 0, else ErrInvalidSnapshotBackupMaxRetries
//     and the live budget stays exactly as it was. Zero is legal
//     here — unlike the startup-time "non-positive falls back to the
//     default 3" rule and unlike FullEvery's "0 is rejected": zero
//     means fail-fast, a single attempt with no backoff sleep, so a
//     runtime revision must not silently normalize it away (along
//     LG-314). There is no upper bound: an oversized budget lengthens
//     a failing tick and holds the export read lock longer, and the
//     operator owns that trade-off (documented in the README).
//   - A revision identical to the live budget is a no-op: it returns
//     the live value with a nil error and writes no audit event, so
//     a retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     snapshot_backup_max_retries_retuned audit event (previous /
//     current budget as plain integers — purely numeric, no backup
//     content or key material). The Ctx variant attributes the
//     operator (LG-73). Lock order: the setter releases
//     maxRetriesMu, then takes l.mu.RLock to emit — maxRetriesMu,
//     sw.mu, and l.mu are never held nested, so the lock order is
//     acyclic. backupOnce takes maxRetriesMu.RLock only briefly
//     inside sw.mu at the retry loop's entry and releases it before
//     its audit section takes l.mu.RLock. In particular the setter
//     never calls backupOnce and never takes sw.mu while holding
//     maxRetriesMu: backupOnce holds sw.mu for a whole export
//     (including its 100ms x MaxRetries backoff, atomic write,
//     retention pruning, and audit), so a setter that took sw.mu
//     would stall behind a backup in flight. It does not reuse
//     intervalMu either: that lock guards the backup-interval
//     cadence and its retune channel, a different knob.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-333 family): backup tuning is
//     in-process structural config. main.go stores the started
//     worker handle on srv.snapshotBackup so existing operator paths
//     can call it.
//   - Snapshots do not carry the retry budget: the budget comes only
//     from the startup-time default 3, so a restart restores 3 while
//     the revision events themselves stay in the audit trail for
//     traceability. Persisting a revision across restarts would need
//     an env knob to be introduced, a later round.
//   - The budget is NOT the backup interval, NOT the retention
//     policy, NOT the full/incremental cadence, and NOT the
//     encryption config: it only decides how many times a failed
//     export is retried before the tick counts as failed. Interval
//     still decides how often a backup is taken, KeepFull /
//     KeepIncremental still decide how many files survive pruning,
//     FullEvery still decides whether a future backup is full or
//     incremental, and the LG-77 key ring still decides whether
//     files are sealed — none of them participates in the revision.
//     The ticks counter and ledger_snapshot_backups_total /
//     ledger_snapshot_backups_failed_total keep accumulating across
//     a revision.
//   - A worker that was never started (dir unset or interval
//     invalid, so snapshotBackupConfig() returned false and
//     StartSnapshotBackup was never called) has no handle to revise:
//     the revision applies only to a running worker.
//
// Orthogonality: LG-39 owns the backup rotation mechanism itself
// (this module is its missing runtime revision entry point for the
// export retry budget), LG-329 owns the same worker's backup interval
// (how often a backup is taken; this module owns how long a failed
// one is retried — the LG-329 draft explicitly left MaxRetries
// read-only from cfg, and this module is that exclusion's
// fulfillment), LG-332 owns the same worker's retention depths (how
// many files survive pruning; this module owns the export retry
// budget — the LG-332 draft explicitly excluded MaxRetries and
// deferred it here), LG-333 owns the same worker's full/incremental
// alternation cadence (which backup is full; this module owns the
// export retry budget — the LG-333 module comment states
// "MaxRetries still decides the export retry backoff", and this
// module is that gap's fulfillment), LG-337 owns the same worker's
// encryption fail-closed posture (whether a backup lands sealed;
// this module owns the export retry budget — the LG-337 draft
// excluded MaxRetries), LG-77 owns at-rest encryption of backup
// files (whether a backup lands sealed; this module owns the retry
// budget), LG-78 owns anchoring a full snapshot's balance Merkle
// root into the audit chain (evidence anchoring; this module owns
// the evidence-production resilience, and a revision changes no
// backup file's merkle_root), LG-70 owns GET /snapshots/diff (a
// read-only comparison over the backup directory; it reads
// srv.snapshotBackupDir directly with no worker-handle dependency,
// so a retry-budget revision does not touch its 404 fail-closed
// semantics), LG-24/LG-28 own full/incremental export itself (the
// export semantics; this module owns what happens after an export
// fails), LG-127 owns mandate debit retry (a different object: debit
// retries vs. snapshot export retries — the shared field name is
// coincidence), LG-310/LG-311 own the payment remediation queue
// capacity/TTL (a different component), and LG-330/LG-331 own the
// balance-history worker knobs (a different worker).

var (
	// ErrInvalidSnapshotBackupMaxRetries is returned by
	// SnapshotBackup.SetMaxRetries when the requested budget is
	// negative. The live budget is left untouched.
	ErrInvalidSnapshotBackupMaxRetries = errors.New("ledger: snapshot backup: invalid max retries: max retries must be non-negative")
)

// SetMaxRetries revises the worker's export retry budget in place and
// reports the pre-revision budget (for the audit trail). The revision
// is future-only: a backup already in progress runs to completion on
// the budget it started with, and only the next backup's retry loop
// uses the new budget. A revision identical to the live budget is a
// no-op that returns the live value with a nil error and no side
// effect. A negative budget returns
// ErrInvalidSnapshotBackupMaxRetries with the live budget untouched
// (the returned value is the live budget in that case too). Zero is
// legal: it means fail-fast, a single export attempt with no backoff
// sleep.
func (sw *SnapshotBackup) SetMaxRetries(n int) (int, error) {
	return sw.SetMaxRetriesCtx(context.Background(), n)
}

// SetMaxRetriesCtx is SetMaxRetries with per-call audit actor attribution
// (LG-73).
func (sw *SnapshotBackup) SetMaxRetriesCtx(ctx context.Context, n int) (int, error) {
	sw.maxRetriesMu.Lock()
	previous := sw.maxRetries
	if n < 0 {
		sw.maxRetriesMu.Unlock()
		return previous, ErrInvalidSnapshotBackupMaxRetries
	}
	if n == previous {
		sw.maxRetriesMu.Unlock()
		return previous, nil
	}
	sw.maxRetries = n
	sw.maxRetriesMu.Unlock()

	// Audit after the swap, under l.mu.RLock like LG-329 and the
	// LG-316..LG-333 retunes: maxRetriesMu and l.mu are never held
	// nested, so the lock order is acyclic. emitAuditCtx is a no-op
	// when no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two integer budgets: no backup content, token, key, or other
	// secret material ever rides in a retune event.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_max_retries_retuned",
		Actor:         "SetMaxRetries",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_max_retries": previous,
			"current_max_retries":  n,
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// MaxRetries reports the worker's live export retry budget: a failed
// export is retried up to this many times before the tick counts as
// failed. Zero means fail-fast, a single attempt.
func (sw *SnapshotBackup) MaxRetries() int {
	sw.maxRetriesMu.RLock()
	defer sw.maxRetriesMu.RUnlock()
	return sw.maxRetries
}
