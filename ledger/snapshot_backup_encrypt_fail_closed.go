package ledger

import "context"

// Snapshot-backup encryption fail-closed posture runtime revision
// (LG-337).
//
// StartSnapshotBackup fixes the backup worker's encryption posture —
// what a tick does when no encryption key is configured: fail-closed
// refuses the tick instead of writing a plaintext backup, fail-open
// writes plaintext with a one-time warning — for the life of the
// worker: snapshotBackupConfig() (main.go) reads
// LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED once at startup, and
// StartSnapshotBackup never seeded it as a live value, so the only
// live read was backupOnce's construction-time
// cfg.Encryption.FailClosed branch. The only way to stop plaintext
// backups the moment a missing or misconfigured key is discovered
// (a plaintext payment ledger landing on disk is a compliance event)
// or to temporarily restore fail-open when a key fault under
// fail-closed makes every tick fail and leaves no usable backup at
// all was to change the env var and restart the process — and the
// restart window itself writes more plaintext backups or fails more
// ticks. This module is the orthogonal runtime path: an operator
// revises the live worker's fail-closed posture in place, instead of
// restarting the server.
//
// Semantics, deliberately fail-closed throughout, along the LG-329 /
// LG-332 / LG-333 structural revisions:
//
//   - The revision is in place: the worker keeps its goroutine, its
//     rotation counter (backups / lastVersion), its ticks counter,
//     and its interval, retention, cadence, and encryption-key
//     configuration — none of them resets. Only the fail-closed
//     posture moves. No restart, no Stop + Start. The key ring
//     itself never participates: flipping the posture rotates no
//     key and changes no key id.
//   - The revision is future-only: the posture is read exactly once,
//     at the encryption branch inside backupOnce, so a backup
//     already past that branch runs to completion on the posture it
//     read (its file still lands under the old posture), and the new
//     posture governs from the next tick's branch. A backup still
//     exporting when the revision lands reads the live value at its
//     own branch — the revision never cuts an export short.
//   - Flipping re-arms the one-time warning for the new posture:
//     toward fail-closed resets warnedMissingKey, toward fail-open
//     resets warnedUnencrypted. The LG-77 one-time semantics are
//     per posture, not per process lifetime — otherwise the first
//     tick under a posture the worker already warned about once
//     would fail or write plaintext in silence. Each posture's first
//     tick still signals its consequence exactly once.
//   - A revision identical to the live posture is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - A bool has no illegal value, so the error is always nil — the
//     (previous, error) return is kept for consistency with the
//     retune family.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     snapshot_backup_encrypt_fail_closed_retuned audit event
//     (previous_fail_closed / current_fail_closed, two bools, no
//     key material). The Ctx variant attributes the operator
//     (LG-73). Lock order: the setter releases failClosedMu, then
//     takes l.mu.RLock to emit — failClosedMu, sw.mu, and l.mu are
//     never held nested, so the lock order is acyclic. In
//     particular the setter never takes sw.mu: backupOnce holds it
//     for a whole export (including its 100ms x MaxRetries backoff,
//     atomic write, retention pruning, and audit), so a setter that
//     took sw.mu would stall behind a backup in flight.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-329 / LG-332 / LG-333 family): backup tuning is
//     in-process structural config. main.go stores the started
//     worker handle on srv so existing operator paths can call it.
//   - Snapshots do not carry the posture: the posture comes only
//     from the startup-time LEDGER_SNAPSHOT_BACKUP_ENCRYPT_FAIL_CLOSED
//     parse, so a restart restores the env value while the revision
//     events themselves stay in the audit trail for traceability.
//   - A worker that was never started (dir unset or interval
//     invalid, so snapshotBackupConfig() returned false and
//     StartSnapshotBackup was never called) has no handle to revise:
//     the revision applies only to a running worker.
//   - With a key configured, flipping the posture changes nothing
//     about the sealing path: the branch is never reached, files
//     still land encrypted under the same active key id.
//
// Orthogonality: LG-77 owns backup encryption itself (this module
// is its missing runtime revision entry point for the no-key
// posture — the key ring does not move), LG-329 owns the backup
// interval, LG-332 owns the retention depths, LG-333 owns the
// full/incremental cadence (the same worker, different knobs —
// none of them touches the encryption posture), LG-78 owns
// anchoring a full snapshot's balance Merkle root into the audit
// chain (evidence anchoring; this module owns the evidence
// landing posture, and a revision changes no backup file's
// merkle_root), LG-39 owns the backup rotation mechanism, and
// LG-70 owns GET /snapshots/diff (a read-only comparison over the
// backup directory).
//
// Explicitly out of scope: rotating the backup encryption key
// (LG-329 (c) / LG-332 (c) already rejected a key-management
// mechanism — this module revises only the strategy bool and never
// touches key material), MaxRetries, the interval / retention /
// cadence knobs (LG-329 / LG-332 / LG-333), a new HTTP endpoint,
// per-backup overrides, persisting the posture across restarts, and
// stopping the worker through a revision (Stop owns that).

// SetEncryptFailClosed revises the worker's fail-closed posture in
// place and reports the pre-revision posture (for the audit trail).
// The revision is future-only: a backup already past the encryption
// branch runs to completion on the posture it read, and only the
// next tick's branch uses the new posture. Flipping re-arms the
// one-time warning for the new posture, so the first tick under it
// still signals its consequence once. A revision identical to the
// live posture is a no-op that returns the live value with a nil
// error and no side effect. The error is always nil: a bool has no
// illegal value, and the (previous, error) return is kept for
// consistency with the retune family.
func (sw *SnapshotBackup) SetEncryptFailClosed(v bool) (bool, error) {
	return sw.SetEncryptFailClosedCtx(context.Background(), v)
}

// SetEncryptFailClosedCtx is SetEncryptFailClosed with per-call audit
// actor attribution (LG-73).
func (sw *SnapshotBackup) SetEncryptFailClosedCtx(ctx context.Context, v bool) (bool, error) {
	sw.failClosedMu.Lock()
	previous := sw.failClosed
	if v == previous {
		sw.failClosedMu.Unlock()
		return previous, nil
	}
	sw.failClosed = v
	// Re-arm the one-time warning for the new posture (LG-77
	// one-time semantics are per posture): flipping toward
	// fail-closed re-arms the refusal warning, flipping toward
	// fail-open re-arms the plaintext warning.
	if v {
		sw.warnedMissingKey.Store(false)
	} else {
		sw.warnedUnencrypted.Store(false)
	}
	sw.failClosedMu.Unlock()

	// Audit after the swap, under l.mu.RLock like LG-329 and the
	// LG-332 / LG-333 retunes: failClosedMu and l.mu are never held
	// nested, so the lock order is acyclic. emitAuditCtx is a no-op
	// when no audit log is attached, and the event goes only to the
	// audit log — never to the outbox. The details carry only the
	// two posture bools: no key material ever rides in a retune
	// event.
	sw.l.mu.RLock()
	sw.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "snapshot_backup_encrypt_fail_closed_retuned",
		Actor:         "SetEncryptFailClosed",
		VersionBefore: sw.l.version,
		VersionAfter:  sw.l.version,
		Details: map[string]any{
			"previous_fail_closed": previous,
			"current_fail_closed":  v,
		},
	})
	sw.l.mu.RUnlock()
	return previous, nil
}

// EncryptFailClosed reports the worker's live fail-closed posture:
// true means a tick with no encryption key configured fails instead
// of writing a plaintext backup.
func (sw *SnapshotBackup) EncryptFailClosed() bool {
	sw.failClosedMu.RLock()
	defer sw.failClosedMu.RUnlock()
	return sw.failClosed
}
