package ledger

import (
	"context"
	"errors"
	"time"
)

// Maximum hold duration (LG-312).
//
// Hold creation validates ExpiresAt is non-zero (hold.go) but never
// bounded it: a caller could lock an account's available funds for
// years with a single authorization, and the operator's only remedy
// was to Release each hold after the fact. During an incident — or
// under a malicious client — one over-long hold can strand funds
// indefinitely. This module is the orthogonal runtime path: an
// operator caps the hold horizon in place, instead of rebuilding
// the ledger.
//
// Semantics, deliberately fail-closed throughout, follow the LG-241
// / LG-242 / LG-309 / LG-310 / LG-311 structural revisions:
//
//   - The cap is a maximum *duration from now*, not an absolute
//     deadline: a hold is rejected when ExpiresAt is strictly after
//     now + max. A hold expiring exactly at now + max is admitted
//     (the boundary is inclusive, like the TTL-expiry boundary in
//     hold.go where now >= ExpiresAt is expired). A hold whose
//     expiry is already in the past is still accepted but born
//     expired — the cap only ever rejects over-long futures.
//   - Validation is strict on the setter: a negative duration is
//     ErrInvalidMaxHoldDuration and the live value stays untouched.
//     Zero disables the cap (the default: no cap), so there is no
//     "invalid zero" — zero is the off switch, mirroring the
//     construction-time convention that 0 means "not configured"
//     (idempotencyTTL in ledger.go).
//   - Enforcement runs in holdLocked, after basic validation and
//     after the idempotency replay check, before the
//     available-funds check — the same ordering principle as the
//     frozen check: a replay reserves nothing new, so it must not
//     fail on a cap that landed after the original hold was
//     placed. The check reads the same time.Now() the rest of
//     holdLocked uses, so the cap decision and the born-expired
//     decision can never disagree about "now".
//   - Future-only: the cap stamps no hold. Already-placed holds
//     keep the ExpiresAt they were created with — tightening never
//     expires a live hold early (its reservation stands until its
//     own expiry) and loosening never extends one. Only holds
//     created after the revision are judged against the new cap.
//   - The dispute freeze hold is exempt. RaiseDispute
//     (dispute.go) places its freeze through holdLockedUncapped:
//     its 120-day TTL (defaultDisputeHoldTTL) is a fixed
//     risk-control freeze, not an operator-chosen authorization
//     horizon — capping it would make dispute raising fail the
//     moment an operator set a cap shorter than 120 days, turning
//     a hold-length guard into a dispute-blocking bug. The
//     dispute lifecycle (resolve/expire) bounds the freeze
//     instead. Multisig fund-freeze holds (multisig.go) stay
//     capped: their expiry is operator-chosen via the op's
//     ExpiresAt/policy TTL, so the cap legitimately applies.
//   - Revising to the current cap is a no-op: it returns nil and
//     writes no audit event, so a retried or duplicated revision
//     cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it
//     moves no money, so it bumps neither the ledger version nor
//     the audit chain, and no journal entry or outbox event fires.
//     It lands one hold_max_duration_amended event
//     (previous_max_hold_duration / max_hold_duration, as
//     duration strings, following the LG-242
//     idempotency_ttl_amended detail shape). The Ctx variant
//     attributes the operator (LG-73).
//   - Rejections ride the existing ledger_hold_rejections_total
//     counter: the HTTP layer maps ErrHoldExceedsMaxDuration to
//     422 and increments HoldRejections alongside
//     ErrInsufficientAvailableFunds (main.go handleCreateHold),
//     so no new counter is needed — the counter already covers
//     422-class hold-domain rejections.
//   - Snapshots carry no hold cap (snapshot.go has no hold-cap
//     state): a disaster-recovery restore returns the default
//     (unlimited), and the amendment must be re-applied — the
//     amendment events themselves stay in the audit trail for
//     traceability.
//
// Orthogonality: LG-20 owns the hold mechanism itself, LG-310
// owns the repair queue capacity knob (same family, different
// module), LG-311 owns the repair record TTL knob, and LG-242
// owns the idempotency-key TTL. There is no environment variable
// or HTTP endpoint for the revision (per LG-241/242): hold-cap
// tuning is in-process structural config. ExtendHoldExpiry
// (hold_expiry_extend.go) is untouched: the cap governs hold
// creation only, so an extension can still move a deadline past
// the cap — extending an existing reservation is a separate
// decision from admitting a new one, and is left to a later item
// if operators want it bounded too.

var (
	// ErrInvalidMaxHoldDuration is returned by SetMaxHoldDuration
	// when the requested cap is negative: a negative horizon is
	// meaningless, and zero is the off switch, not an error.
	ErrInvalidMaxHoldDuration = errors.New("ledger: max hold duration must not be negative")
	// ErrHoldExceedsMaxDuration is returned by Hold when the
	// hold's ExpiresAt lies strictly after now + the live maximum
	// hold duration. The hold is rejected: nothing is reserved,
	// nothing is recorded, and the ledger version is untouched.
	ErrHoldExceedsMaxDuration = errors.New("ledger: hold expiry exceeds the maximum hold duration")
)

// SetMaxHoldDuration revises the maximum hold duration in place.
// A hold created after the revision is rejected with
// ErrHoldExceedsMaxDuration when its ExpiresAt is strictly after
// now + d. Already-placed holds are unaffected: tightening never
// expires a live hold early and loosening never extends one. A
// negative duration is rejected with ErrInvalidMaxHoldDuration and
// the live value is left untouched; zero disables the cap
// (unlimited, the default). Revising to the current cap is a no-op
// with no audit event. The revision is audited as
// hold_max_duration_amended.
func (l *Ledger) SetMaxHoldDuration(d time.Duration) error {
	return l.SetMaxHoldDurationCtx(context.Background(), d)
}

// SetMaxHoldDurationCtx is SetMaxHoldDuration with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetMaxHoldDurationCtx(ctx context.Context, d time.Duration) error {
	if d < 0 {
		return ErrInvalidMaxHoldDuration
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if d == l.maxHoldDuration {
		return nil
	}
	previous := l.maxHoldDuration
	l.maxHoldDuration = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "hold_max_duration_amended",
		Actor:         "SetMaxHoldDuration",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_hold_duration": previous.String(),
			"max_hold_duration":          d.String(),
		},
	})
	return nil
}

// MaxHoldDuration reports the live maximum hold duration: a hold
// created from now on is rejected when its ExpiresAt is strictly
// after now + this value. Zero means unlimited (the default).
func (l *Ledger) MaxHoldDuration() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.maxHoldDuration
}
