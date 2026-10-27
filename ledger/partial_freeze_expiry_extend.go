package ledger

import (
	"context"
	"errors"
	"time"
)

// Partial-freeze expiry extension (LG-230).
//
// SetPartialFreezeExpiry (LG-225) stamps an automatic-release
// deadline on an existing partial freeze; this module is the
// orthogonal pre-expiry path: an operator extends that deadline
// before it lapses, instead of letting the freeze release and
// re-freezing. Extending in place matters because a re-freeze
// would be a new reservation — a gap in which the funds are
// available, plus a fresh freeze audit trail that hides the
// continuity — while an extension keeps the same frozen amount,
// the same reservation, and one audit trail from freeze to
// release.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an (account, currency) that already carries an
//     expiry may be extended. A pair with no partial freeze
//     fails with ErrPartialFreezeNoFreeze, and a pair with a
//     freeze but no expiry fails with
//     ErrPartialFreezeExpiryNotSet (the operator sets one first
//     with SetPartialFreezeExpiry): extending must never
//     invent a deadline on a freeze that was meant to last
//     until explicitly released.
//   - The new deadline must be strictly later than the current
//     one and in the future: extensions only lengthen, never
//     shorten. An equal, earlier, or past new deadline fails
//     with ErrPartialFreezeExpiryExtensionNotLater and the
//     original deadline is untouched, so a mistaken call cannot
//     silently cut a reservation short.
//   - A deadline that has passed but whose sweep has not run
//     yet is still a live reservation — the expiry still blocks
//     FreezeAmount for the pair (LG-225 is fail-closed on a
//     stale deadline) — so extending it to a future time
//     lengthens a live reservation rather than resurrecting a
//     released one. An already-swept freeze has no expiry entry
//     and fails with ErrPartialFreezeExpiryNotSet.
//   - The extension touches the deadline and nothing else: the
//     frozen amount is unchanged, and the FreezeAmount gate
//     stays in place (the pair still carries an expiry, so new
//     freezes are still rejected until the new deadline
//     releases or is manually cleared). The extension lands one
//     audit event (partial_freeze_expiry_extended, with the
//     previous and new deadlines) so the "who moved the
//     release, and from what to what" trail is tamper-evident.
//     Like every other freeze operation it bumps no ledger
//     version and appends no chain link.
//   - ExpirePartialFreezes reads the live deadline, so no sweep
//     change is needed: an extended freeze survives sweeps up
//     to its new deadline and releases after it, and the
//     snapshot round-trip of the deadline (the
//     PartialFrozenBalance row's ExpiresAt) carries the
//     extended instant verbatim.
//
// Orthogonality: SetPartialFreezeExpiry owns the initial
// deadline, ClearPartialFreezeExpiry owns removing it,
// ExpirePartialFreezes owns the release, and this module owns
// only the pre-release lengthening. It is orthogonal to the
// whole-account timed-freeze extension (LG-229), which moves a
// stop deadline rather than a reservation deadline, and to the
// hold expiry extension (LG-228), whose holds capture while a
// partial-freeze expiry only ever releases.

// Partial-freeze expiry extension errors.
var (
	// ErrPartialFreezeExpiryNotSet is returned by
	// ExtendPartialFreezeExpiry when the (account, currency)
	// carries no partial-freeze expiry to extend: the operator
	// sets one first with SetPartialFreezeExpiry.
	ErrPartialFreezeExpiryNotSet = errors.New("ledger: no partial freeze expiry to extend")
	// ErrPartialFreezeExpiryExtensionNotLater is returned by
	// ExtendPartialFreezeExpiry when the new deadline is not
	// strictly later than the current expiry, or is not in the
	// future.
	ErrPartialFreezeExpiryExtensionNotLater = errors.New("ledger: partial freeze expiry extension must be later than current expiry and in the future")
)

// ExtendPartialFreezeExpiry moves the automatic-release deadline
// on the account's existing partial freeze in the default
// currency to newExpiresAt. See ExtendPartialFreezeExpiryIn for
// the currency-scoped form.
func (l *Ledger) ExtendPartialFreezeExpiry(a AccountID, newExpiresAt time.Time) error {
	return l.ExtendPartialFreezeExpiryCtx(context.Background(), a, newExpiresAt)
}

// ExtendPartialFreezeExpiryCtx is ExtendPartialFreezeExpiry with
// per-call audit actor attribution (LG-73).
func (l *Ledger) ExtendPartialFreezeExpiryCtx(ctx context.Context, a AccountID, newExpiresAt time.Time) error {
	return l.extendPartialFreezeExpiryInCtx(ctx, a, DefaultCurrency, newExpiresAt)
}

// ExtendPartialFreezeExpiryIn moves the automatic-release
// deadline on the account's existing partial freeze in the
// given currency to newExpiresAt; an empty currency means the
// default currency.
func (l *Ledger) ExtendPartialFreezeExpiryIn(a AccountID, currency string, newExpiresAt time.Time) error {
	return l.ExtendPartialFreezeExpiryInCtx(context.Background(), a, currency, newExpiresAt)
}

// ExtendPartialFreezeExpiryInCtx is
// ExtendPartialFreezeExpiryIn with per-call audit actor
// attribution (LG-73).
func (l *Ledger) ExtendPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string, newExpiresAt time.Time) error {
	return l.extendPartialFreezeExpiryInCtx(ctx, a, currency, newExpiresAt)
}

// extendPartialFreezeExpiryInCtx is the shared body of the
// ExtendPartialFreezeExpiry entry points: validation, then the
// existing-expiry check, then the commit. A failed extension
// mutates nothing — neither the frozen amount nor any previously
// set expiry moves. It does not bump the ledger version: an
// expiry moves no money. Callers must not hold l.mu; the write
// lock is taken here.
func (l *Ledger) extendPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string, newExpiresAt time.Time) error {
	if a == "" {
		return ErrPartialFreezeEmptyAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := accountCurrency{account: a, currency: cur}
	frozen := l.partialFrozenLocked(a, cur)
	if frozen == 0 {
		return ErrPartialFreezeNoFreeze
	}
	current, ok := l.partialFreezeExpiry[key]
	if !ok {
		return ErrPartialFreezeExpiryNotSet
	}
	now := time.Now()
	if !newExpiresAt.After(now) || !newExpiresAt.After(current) {
		return ErrPartialFreezeExpiryExtensionNotLater
	}
	l.partialFreezeExpiry[key] = newExpiresAt
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze_expiry_extended",
		Actor:         "ExtendPartialFreezeExpiry",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"currency":             cur,
			"frozen_cents":         frozen,
			"previous_expires_at":  current.UTC().Format(time.RFC3339),
			"expires_at":           newExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return nil
}
