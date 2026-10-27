package ledger

import (
	"context"
	"errors"
	"time"
)

// Whole-account timed-freeze extension (LG-229).
//
// FreezeUntil (LG-58) schedules a whole-account freeze to lift
// automatically at a deadline; this module is the orthogonal
// pre-expiry path: an operator extends that deadline before it
// lapses, instead of letting the freeze release and re-freezing.
// Extending in place matters because a re-freeze would be a new
// stop — a gap in which the account is unfrozen, plus a fresh
// freeze audit trail that hides the continuity — while an
// extension keeps the same freeze, the same stop, and one audit
// trail from freeze to release.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a timed freeze may be extended. An account that is not
//     frozen, or frozen manually (Freeze with no deadline —
//     including a timed freeze later re-frozen manually, which
//     converts it to a manual freeze by deleting the deadline),
//     fails with ErrFreezeUntilNoTimedFreeze and nothing
//     changes: a manual freeze has no deadline to move, and
//     inventing one would silently convert a never-expiring stop
//     into an expiring one.
//   - The new deadline must be strictly later than the current
//     one and in the future: extensions only lengthen, never
//     shorten. An equal, earlier, or past new deadline fails
//     with ErrFreezeUntilExtensionNotLater and the original
//     deadline is untouched, so a mistaken call cannot silently
//     cut a risk stop short.
//   - A deadline that has passed but whose release has not run
//     yet (no write-path check, no sweeper tick, no
//     ExpireFrozenAccounts call) is still a live stop — the
//     account is still frozen — so extending it to a future
//     time lengthens a live stop rather than resurrecting a
//     released one. An already-released freeze has no deadline
//     entry and fails with ErrFreezeUntilNoTimedFreeze.
//   - The extension touches the deadline and nothing else: the
//     frozen state itself is unchanged, the ledger version is
//     not bumped, and no outbox event fires (the account was
//     already reported frozen when the freeze was applied).
//     The extension lands one audit event
//     (freeze_until_extended, with the previous and new
//     deadlines) so the "who moved the release, and from what
//     to what" trail is tamper-evident.
//   - The lazy release and the sweeper read the live deadline
//     (see freeze.go), so no sweep change is needed: an
//     extended freeze survives sweeps up to its new deadline
//     and releases after it, and the snapshot round-trip of
//     the deadline (FrozenAutoUnfreeze rows) carries the
//     extended instant verbatim.
//
// Orthogonality: FreezeUntil owns the initial deadline (and the
// manual-freeze conversion), ExpireFrozenAccounts and the
// sweeper own the release, and this module owns only the
// pre-release lengthening. It is orthogonal to the amount-level
// partial-freeze expiry extension (LG-230), which moves a
// reservation deadline rather than a whole-account stop.

// Timed-freeze extension errors.
var (
	// ErrFreezeUntilNoTimedFreeze is returned by
	// ExtendFreezeUntil when the account has no timed freeze
	// to extend: it is not frozen, or it is frozen manually
	// with no scheduled automatic-unfreeze deadline.
	ErrFreezeUntilNoTimedFreeze = errors.New("ledger: account has no timed freeze to extend")
	// ErrFreezeUntilExtensionNotLater is returned by
	// ExtendFreezeUntil when the new deadline is not strictly
	// later than the current deadline, or is not in the future.
	ErrFreezeUntilExtensionNotLater = errors.New("ledger: freeze auto-unfreeze extension must be later than current deadline and in the future")
)

// ExtendFreezeUntil moves an account's timed-freeze automatic-
// unfreeze deadline to newUntil. The new deadline must be
// strictly later than the current one and in the future; the
// frozen state itself is unchanged, and a failed extension
// leaves the original deadline exactly as it was. Extending is
// off-journal — it does not bump the ledger version — but it is
// audited as freeze_until_extended.
func (l *Ledger) ExtendFreezeUntil(a AccountID, newUntil time.Time) error {
	return l.ExtendFreezeUntilCtx(context.Background(), a, newUntil)
}

// ExtendFreezeUntilCtx is ExtendFreezeUntil with per-call audit
// actor attribution (LG-73).
func (l *Ledger) ExtendFreezeUntilCtx(ctx context.Context, a AccountID, newUntil time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.frozen[a] {
		return ErrFreezeUntilNoTimedFreeze
	}
	current, ok := l.frozenUntil[a]
	if !ok {
		return ErrFreezeUntilNoTimedFreeze
	}
	now := time.Now()
	if !newUntil.After(now) || !newUntil.After(current) {
		return ErrFreezeUntilExtensionNotLater
	}
	l.frozenUntil[a] = newUntil
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "freeze_until_extended",
		Actor:         "ExtendFreezeUntil",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"previous_auto_unfreeze_at": current.UTC().Format(time.RFC3339),
			"auto_unfreeze_at":          newUntil.UTC().Format(time.RFC3339),
		},
	})
	return nil
}
