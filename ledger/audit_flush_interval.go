package ledger

import (
	"context"
	"errors"
	"time"
)

// Audit-log fsync interval revision (LG-342).
//
// The background writer fsyncs the audit file on a ticker — today a
// `time.Second` literal in run (audit.go) — plus once every 128 written
// events, whichever comes first. The interval is the durability window:
// how much written-but-unsynced trail a crash can lose. An operator
// watching for a crash risk wants to tighten it toward 100ms to shrink
// the loss window; an operator watching a disk under fsync pressure
// wants to loosen it toward 5s-10s to cut fsyncs. Today both can only
// change code and restart — and a restart tears the writer down and
// rebuilds the chain head.
//
// This module is the orthogonal runtime path: an operator revises the
// interval in place, instead of rebuilding the ledger and replaying
// its journal. The semantics, deliberately fail-closed throughout,
// follow the LG-301/LG-309 structural revisions:
//
//   - The ticker belongs to the writer, so unlike maxBytes it cannot
//     be swapped by writing a field: the background writer owns the
//     ticker in run, and the revision travels to it as a
//     flushIntervalRequest. The writer applies it at a select point
//     where it holds no received event: it syncs once at the old
//     cadence boundary, then stops the old ticker and arms a new one
//     at the new interval (retune-drain, along LG-328). A revision
//     therefore never swallows a due sync, and the new cadence
//     governs from the next tick.
//   - The every-128-events sync is a second, independent trigger
//     (audit.go write): it is untouched by this module — a long
//     interval never delays the write path, only the time-based
//     sync, and a burst of 128 events still syncs mid-interval.
//   - Validation reuses the construction-time gate, without the
//     "keep the default" fallback — at runtime there is no default
//     to fall back to. The interval must be a positive duration
//     (WithAuditLogFlushInterval ignores non-positive values at
//     construction; here they are ErrInvalidAuditLogFlushInterval
//     and the live interval is left untouched). No upper bound is
//     enforced: the documentation states plainly that a longer
//     interval widens the crash-loss window, and the operator owns
//     that trade-off.
//   - A ledger with no audit log attached (WithAuditLog was never
//     used) rejects the revision with ErrAuditLogNotConfigured —
//     the mutation form of AuditStats' enabled=false.
//   - Revising to the current interval is a no-op: it returns the
//     live value and writes no audit event, so a retried or
//     duplicated revision cannot spam the audit trail.
//   - The amendment is log infrastructure tuning, not ledger state:
//     it moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires.
//     It lands one audit_log_flush_interval_amended event
//     (previous_flush_interval_ms / flush_interval_ms, whole
//     milliseconds). The Ctx variant attributes the operator
//     (LG-73). The setters return the previous interval, so a
//     caller can log or restore it.
//   - Snapshots carry no interval (AuditLog lives outside the ledger
//     snapshot): a restart restores the construction-time / env
//     value, while the amendment events themselves stay in the audit
//     trail for traceability.
//
// Lock order: the Ledger-level setters hold l.mu, then the AuditLog
// takes flushMu (serializing setters with Close, along LG-309) and
// cfgMu. chainMu is never nested under either: the amendment event
// is emitted only after setFlushInterval has returned and released
// both, exactly as LG-301/LG-309 emit theirs.
//
// Orthogonality: WithAuditLogFlushInterval owns construction-time
// install, LG-301 owns the rotation cap and retention window, LG-309
// owns the async queue capacity, LG-314 owns the outbox forwarder
// cadence (a different component), LG-65 owns per-line HMAC signing,
// LG-32 owns hash-chain sealing, and this module owns only the
// runtime revision of the fsync interval. There is no HTTP endpoint
// for the revision (per LG-241/242): audit-log tuning is in-process
// structural config.

var (
	// ErrInvalidAuditLogFlushInterval is returned by
	// SetAuditLogFlushInterval when the requested interval is not a
	// positive duration.
	ErrInvalidAuditLogFlushInterval = errors.New("ledger: audit log flush interval must be a positive duration")
)

// flushIntervalRequest is one pending fsync-interval revision for the
// background writer (LG-342). done is closed once the ticker is
// rebuilt at the new interval; the writer syncs once at the old
// cadence boundary first, so no due sync is swallowed.
type flushIntervalRequest struct {
	d    time.Duration
	done chan struct{}
}

// FlushInterval reports the current fsync cadence, read live under
// cfgMu. It is the runtime counterpart of the construction-time
// WithAuditLogFlushInterval value (default DefaultAuditFlushInterval).
func (a *AuditLog) FlushInterval() time.Duration {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.flushInterval
}

// setFlushInterval installs a new fsync cadence and reports the
// previous value and whether it changed. Callers must validate d > 0
// first; the swap itself is unconditional once past the same-value
// check, so a rejected value leaves the live interval untouched. The
// writer owns the ticker rebuild: a live log takes the request path
// (flushReq, applied at a select point), a closed log takes the
// inline path (Close holds flushMu, so a closed log seen here means
// the writer has exited and there is no ticker left to rebuild).
func (a *AuditLog) setFlushInterval(d time.Duration) (previous time.Duration, changed bool) {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	a.cfgMu.RLock()
	previous = a.flushInterval
	closed := a.closed.Load()
	a.cfgMu.RUnlock()
	if previous == d {
		return previous, false
	}
	if closed {
		a.cfgMu.Lock()
		a.flushInterval = d
		a.cfgMu.Unlock()
		return previous, true
	}
	req := flushIntervalRequest{d: d, done: make(chan struct{})}
	a.flushReq <- req
	<-req.done
	return previous, true
}

// SetAuditLogFlushInterval revises the audit log's fsync interval in
// place and returns the previous interval. The new interval takes
// effect from the next tick: the writer syncs once at the old cadence
// boundary, then rebuilds its ticker — a revision never swallows a
// due sync, and the every-128-events sync is untouched. A
// non-positive interval is rejected with
// ErrInvalidAuditLogFlushInterval and the old interval is left
// untouched; a ledger with no audit log attached returns
// ErrAuditLogNotConfigured. Revising to the current interval is a
// no-op (it returns the live value) with no audit event. The revision
// is audited as audit_log_flush_interval_amended.
func (l *Ledger) SetAuditLogFlushInterval(d time.Duration) (time.Duration, error) {
	return l.SetAuditLogFlushIntervalCtx(context.Background(), d)
}

// SetAuditLogFlushIntervalCtx is SetAuditLogFlushInterval with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetAuditLogFlushIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d <= 0 {
		return 0, ErrInvalidAuditLogFlushInterval
	}
	if l.audit == nil {
		return 0, ErrAuditLogNotConfigured
	}
	previous, changed := l.audit.setFlushInterval(d)
	if !changed {
		return previous, nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_log_flush_interval_amended",
		Actor:         "SetAuditLogFlushInterval",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_flush_interval_ms": previous.Milliseconds(),
			"flush_interval_ms":          d.Milliseconds(),
		},
	})
	return previous, nil
}

// AuditLogFlushInterval reports the audit log's current fsync cadence.
// Enabled is false when no audit log is attached (WithAuditLog was
// never used): in that case the interval is zero and meaningless.
// Follows the AuditStats enabled shape.
func (l *Ledger) AuditLogFlushInterval() (flushInterval time.Duration, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.FlushInterval(), true
}
