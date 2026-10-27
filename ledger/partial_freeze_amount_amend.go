package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Partial-freeze amount amendment (LG-238).
//
// FreezeAmount (LG-224) reserves an amount of an account's
// available funds; UnfreezeAmount releases part of it.
// This module is the orthogonal absolute-revision path: an
// operator sets the frozen amount to a new absolute value in
// place, instead of releasing and re-freezing. Amending in
// place matters because a release-and-refreeze is two
// reservations — a gap in which the funds are available, plus
// a fresh freeze audit trail that hides the continuity —
// while an amendment keeps the same reservation, the same
// audit trail from freeze to release, and no window in which
// the funds are unreserved.
//
// Why an absolute revision, not a delta: FreezeAmount is
// cumulative (partialFrozen[key] += amount, see
// partial_freeze.go), so a delta revision would need the
// caller to compute the difference and would race the
// current total; an absolute value states the intended end
// state directly, and the module enforces it. This was the
// open question left by the 143rd backlog round, verified
// against the cumulative FreezeAmount body before this
// module was specified: because FreezeAmount only ever
// adds, an absolute amendment is not a duplicate of it.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an (account, currency) that already carries a
//     partial freeze may be amended. A pair with no freeze
//     record fails with ErrPartialFreezeNotFound —
//     amending must never invent a reservation on an account
//     that was never frozen (the operator freezes first
//     with FreezeAmount). A zero frozen amount is no record:
//     the map never stores zero (a full release deletes the
//     row), so "no row" and "nothing frozen" are the same
//     state, and both reject.
//   - The new amount must be non-negative. A negative amount
//     fails with ErrPartialFreezeAmountNegative and the
//     original amount is untouched. Zero is legal and clears
//     the record: amending to zero is a full release by
//     another name, and it deletes the row exactly like a
//     full UnfreezeAmount does.
//   - An increase must fit the funds not otherwise
//     reserved: the new absolute amount must not exceed the
//     available-before-partial figure (balance minus holds,
//     review freezes, and term principals — see
//     availableBeforePartialLocked), or the amendment fails
//     with ErrPartialFreezeExceedsAvailable and the original
//     amount is untouched, along the FreezeAmount coverage
//     gate. A decrease is always safe — it only ever frees
//     funds — so it is never coverage-gated, even if other
//     reservations have grown since the freeze was set: a
//     decrease that the coverage gate would reject is still
//     a release, and releases are never blocked.
//   - A pair that already carries an automatic-release
//     deadline (LG-225) refuses amendment with
//     ErrPartialFreezeExpirySet, along the LG-225 gate: the
//     deadline releases the whole frozen amount at its
//     instant, so revising the amount under it would make
//     the release amount ambiguous — the same reason
//     FreezeAmount refuses new funds under a deadline. The
//     operator clears the deadline (ClearPartialFreezeExpiry)
//     or lets it release, then amends.
//   - The amendment touches the frozen amount and nothing
//     else: the account, the currency, and (where it could
//     exist, it cannot — see above) the deadline are
//     unchanged. The amendment lands one audit event
//     (partial_freeze_amount_amended, with the previous and
//     new amounts) so the "who resized the reservation, and
//     from what to what" trail is tamper-evident. Like
//     every other freeze operation it bumps no ledger
//     version and appends no chain link.
//   - The posting gate and Available read the live amount:
//     Available deducts partialFrozen as its own term (see
//     partial_freeze.go) and the overdraft-protected posting
//     floor reads the same map (see overdraft.go), so no
//     gate change is needed — an amended freeze binds
//     postings at its new amount immediately, and the
//     snapshot round-trip of the row carries the amended
//     amount verbatim.
//
// Orthogonality: FreezeAmount owns the initial reservation
// (and its cumulative adds), UnfreezeAmount owns partial and
// full release, SetPartialFreezeExpiry owns the deadline,
// ExtendPartialFreezeExpiry owns lengthening it,
// ClearPartialFreezeExpiry owns removing it,
// ExpirePartialFreezes owns the release, and this module
// owns only the absolute amount revision. It is orthogonal
// to the FX limit-order amount amendment (LG-237), which
// revises a resting order's size rather than a risk-control
// reservation, and to the mandate pre-authorization amount
// amendment (LG-235), which revises a mandate-limit
// reservation rather than a freeze.

// Partial-freeze amount amendment errors.
var (
	// ErrPartialFreezeNotFound is returned by
	// AmendPartialFreezeAmount when the (account, currency)
	// carries no partial freeze to amend: the operator freezes
	// first with FreezeAmount.
	ErrPartialFreezeNotFound = errors.New("ledger: no partial freeze to amend")
	// ErrPartialFreezeAmountNegative is returned by
	// AmendPartialFreezeAmount when the new amount is
	// negative: zero is legal (it clears the record), a
	// negative amount is not an amount at all.
	ErrPartialFreezeAmountNegative = errors.New("ledger: partial freeze amended amount must not be negative")
)

// AmendPartialFreezeAmount sets the account's partially frozen
// amount in the given currency to newAmountCents (an empty
// currency means the default currency). A zero amount clears
// the record. A failed amendment leaves the original amount
// exactly as it was. Amending moves no money: it bumps no
// ledger version, but it is audited as
// partial_freeze_amount_amended.
func (l *Ledger) AmendPartialFreezeAmount(a AccountID, currency string, newAmountCents int64) error {
	return l.AmendPartialFreezeAmountCtx(context.Background(), a, currency, newAmountCents)
}

// AmendPartialFreezeAmountCtx is AmendPartialFreezeAmount with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendPartialFreezeAmountCtx(ctx context.Context, a AccountID, currency string, newAmountCents int64) error {
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
	current := l.partialFrozenLocked(a, cur)
	if current == 0 {
		return ErrPartialFreezeNotFound
	}
	if _, ok := l.partialFreezeExpiry[key]; ok {
		return ErrPartialFreezeExpirySet
	}
	if newAmountCents < 0 {
		return fmt.Errorf("%w: got %d", ErrPartialFreezeAmountNegative, newAmountCents)
	}
	// Coverage gates increases only: a decrease only ever frees
	// funds, so it is never blocked, even if other reservations
	// have grown since the freeze was set.
	if newAmountCents > current {
		now := time.Now()
		availBeforePartial := l.availableBeforePartialLocked(a, cur, now)
		if newAmountCents > availBeforePartial {
			return ErrPartialFreezeExceedsAvailable
		}
	}
	if newAmountCents == 0 {
		delete(l.partialFrozen, key)
	} else {
		l.partialFrozen[key] = newAmountCents
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze_amount_amended",
		Actor:         "AmendPartialFreezeAmount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"currency":              cur,
			"previous_amount_cents": current,
			"amount_cents":          newAmountCents,
		},
	})
	return nil
}
