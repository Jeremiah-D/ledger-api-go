package ledger

import (
	"context"
	"errors"
	"time"
)

// Audit-log rotation cap and retention window amendment (LG-301).
//
// WithAuditLogMaxBytes (audit.go) and WithAuditLogRetentionDays
// (audit_retention.go) configure the audit log only at construction —
// the per-file size cap and the retention window were frozen for the
// life of the process, so tightening retention for a compliance change
// or loosening the rotation cap for a busy day both needed a restart.
// This module is the orthogonal runtime path: an operator revises
// either value in place, instead of rebuilding the ledger and
// replaying its journal. Revising in place matters because a restart
// is a gap in which the old cap/window governs, plus a fresh process
// identity that breaks the audit story — while an amendment keeps the
// same ledger, the same journal, and the same audit trail from old
// values to new.
//
// Semantics, deliberately fail-closed throughout, along the LG-292 /
// LG-296 / LG-299 structural revisions:
//
//   - Both values already had live readers, so no read path needed
//     changing — only the missing mutex. The background writer checks
//     the cap before every write (audit.go run: curSize >= MaxBytes)
//     and ApplyAuditRetention computes its cutoff from the live
//     window, so a revision takes effect as soon as it is installed.
//     What was missing was synchronization: maxBytes and retentionDays
//     had no mutex at all (chainMu only serializes hash-chain
//     sealing). AuditLog.cfgMu (a new sync.RWMutex, never chainMu)
//     guards both fields; the writer and the sweep read under RLock,
//     the setters write under Lock.
//   - Validation reuses the construction-time gates, without the
//     "keep the default" fallback — at runtime there is no default to
//     fall back to. maxBytes must be a positive number of bytes
//     (WithAuditLogMaxBytes ignores non-positive values at
//     construction; here they are ErrInvalidAuditLogMaxBytes and the
//     old cap is left untouched). retentionDays must be >= 0, with 0
//     meaning unlimited (NewAuditLog rejects negatives; here they are
//     ErrInvalidAuditLogRetentionDays and the old window is left
//     untouched).
//   - A ledger with no audit log attached (WithAuditLog was never
//     used) rejects both revisions with ErrAuditLogNotConfigured —
//     the mutation form of AuditStats' enabled=false.
//   - Revising to the current value is a no-op: it returns nil and
//     writes no audit event, so a retried or duplicated revision
//     cannot spam the audit trail. For retention the no-op also
//     skips the sweep — a same-value call must not delete anything.
//   - The live semantics differ per value, deliberately. A maxBytes
//     revision takes effect on the next write: a current file that
//     already exceeds the new (tighter) cap is not force-rotated on
//     the spot; the next write sees curSize >= new cap and rotates
//     then. A retentionDays revision runs ApplyAuditRetention
//     synchronously inside the setter, because the sweep otherwise
//     runs only once at startup (main.go) and a revision without a
//     sweep would take effect only at the next restart — no live
//     effect at all. A sweep failure returns the error while the new
//     window stays installed (the revision itself is recorded first,
//     so the trail shows the operator's intent) and no file is
//     deleted; the deletion event still carries the new window, and
//     deletions still count into AuditRetentionDeletions /
//     ledger_audit_retention_deletions_total.
//   - The amendment is log infrastructure tuning, not ledger state:
//     it moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires. It
//     lands one audit_log_max_bytes_amended event (previous_max_bytes
//     / max_bytes) or audit_log_retention_days_amended event
//     (previous_retention_days / retention_days). The Ctx variants
//     attribute the operator (LG-73).
//   - Snapshots carry neither value (AuditLog lives outside the
//     ledger snapshot): a restart restores the construction-time /
//     env values, while the amendment events themselves stay in the
//     audit trail for traceability.
//   - Explicitly out of scope: QueueSize (the channel capacity is
//     fixed at construction, audit.go) and signing (key rotation is
//     its own concern, audit_sign.go). Neither is revisable here, so
//     this module must not grow a setter for them.
//
// Orthogonality: WithAuditLogMaxBytes / WithAuditLogRetentionDays own
// construction-time install, LG-137 owns the retention sweep itself,
// LG-65 owns per-line HMAC signing, LG-32 owns hash-chain sealing,
// and this module owns only the runtime revision of the rotation cap
// and the retention window. There is no HTTP endpoint for the revision
// (per LG-241/242): audit-log tuning is in-process structural config.

var (
	// ErrInvalidAuditLogMaxBytes is returned by SetAuditLogMaxBytes
	// when the requested cap is not a positive number of bytes.
	ErrInvalidAuditLogMaxBytes = errors.New("ledger: audit log max bytes must be a positive number of bytes")

	// ErrInvalidAuditLogRetentionDays is returned by
	// SetAuditLogRetentionDays when the requested window is negative;
	// 0 means unlimited retention.
	ErrInvalidAuditLogRetentionDays = errors.New("ledger: audit log retention days must be 0 (unlimited) or a positive number of days")

	// ErrAuditLogNotConfigured is returned by the audit-log setters
	// when the ledger has no audit log attached (WithAuditLog was
	// never used).
	ErrAuditLogNotConfigured = errors.New("ledger: audit log is not configured on this ledger")
)

// MaxBytes reports the current per-file size cap in bytes, read live
// under cfgMu. It is the runtime counterpart of the construction-time
// WithAuditLogMaxBytes value (default DefaultAuditMaxBytes).
func (a *AuditLog) MaxBytes() int64 {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.maxBytes
}

// setMaxBytes installs a new per-file size cap and reports the previous
// value and whether it changed. Callers must validate n > 0 first; the
// swap itself is unconditional under cfgMu, so the background writer
// (which reads the cap live on every write) sees the new value on its
// next event.
func (a *AuditLog) setMaxBytes(n int64) (previous int64, changed bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	previous = a.maxBytes
	if previous == n {
		return previous, false
	}
	a.maxBytes = n
	return previous, true
}

// setRetentionDays installs a new retention window in whole days and
// reports the previous value and whether it changed. Callers must
// validate n >= 0 first; 0 means unlimited retention.
func (a *AuditLog) setRetentionDays(n int) (previous int, changed bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	previous = a.retentionDays
	if previous == n {
		return previous, false
	}
	a.retentionDays = n
	return previous, true
}

// SetAuditLogMaxBytes revises the audit log's per-file size cap in
// place. The new cap takes effect on the next written event: a current
// file that already exceeds a tightened cap is not force-rotated — the
// next write sees curSize >= new cap and rotates then. A non-positive
// cap is rejected with ErrInvalidAuditLogMaxBytes and the old cap is
// left untouched; a ledger with no audit log attached returns
// ErrAuditLogNotConfigured. Revising to the current cap is a no-op
// with no audit event. The revision is audited as
// audit_log_max_bytes_amended.
func (l *Ledger) SetAuditLogMaxBytes(n int64) error {
	return l.SetAuditLogMaxBytesCtx(context.Background(), n)
}

// SetAuditLogMaxBytesCtx is SetAuditLogMaxBytes with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetAuditLogMaxBytesCtx(ctx context.Context, n int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 {
		return ErrInvalidAuditLogMaxBytes
	}
	if l.audit == nil {
		return ErrAuditLogNotConfigured
	}
	previous, changed := l.audit.setMaxBytes(n)
	if !changed {
		return nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_log_max_bytes_amended",
		Actor:         "SetAuditLogMaxBytes",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_bytes": previous,
			"max_bytes":          n,
		},
	})
	return nil
}

// SetAuditLogRetentionDays revises the audit log's retention window in
// place and runs a retention sweep synchronously under the new window,
// so the revision has a live effect instead of waiting for the next
// restart (the sweep otherwise runs only once at startup). A negative
// window is rejected with ErrInvalidAuditLogRetentionDays and the old
// window is left untouched; a ledger with no audit log attached returns
// ErrAuditLogNotConfigured. Revising to the current window is a no-op
// with no audit event and no sweep. If the sweep fails the error is
// returned, the new window stays installed, and no file is deleted;
// the revision itself is recorded before the sweep runs, so the trail
// shows the operator's intent. The revision is audited as
// audit_log_retention_days_amended, and any sweep deletion is recorded
// as audit_retention_deleted carrying the new window.
func (l *Ledger) SetAuditLogRetentionDays(n int) error {
	return l.SetAuditLogRetentionDaysCtx(context.Background(), n)
}

// SetAuditLogRetentionDaysCtx is SetAuditLogRetentionDays with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetAuditLogRetentionDaysCtx(ctx context.Context, n int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n < 0 {
		return ErrInvalidAuditLogRetentionDays
	}
	if l.audit == nil {
		return ErrAuditLogNotConfigured
	}
	previous, changed := l.audit.setRetentionDays(n)
	if !changed {
		return nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_log_retention_days_amended",
		Actor:         "SetAuditLogRetentionDays",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_retention_days": previous,
			"retention_days":          n,
		},
	})
	// Synchronous sweep under the new window: without it the revision
	// would not take effect until the next restart. A failure leaves
	// the new window installed and deletes nothing — the caller sees
	// the error and can retry the sweep, while the amendment event
	// above already records the change.
	if _, err := l.audit.ApplyAuditRetention(time.Now()); err != nil {
		return err
	}
	return nil
}

// AuditLogMaxBytes reports the audit log's current per-file size cap in
// bytes. Enabled is false when no audit log is attached (WithAuditLog
// was never used): in that case the cap is zero and meaningless.
// Follows the AuditStats enabled shape.
func (l *Ledger) AuditLogMaxBytes() (maxBytes int64, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.MaxBytes(), true
}

// AuditLogRetentionDays reports the audit log's current retention
// window in whole days, or 0 for unlimited retention. Enabled is false
// when no audit log is attached. Follows the AuditStats enabled shape.
func (l *Ledger) AuditLogRetentionDays() (days int, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.RetentionDays(), true
}
