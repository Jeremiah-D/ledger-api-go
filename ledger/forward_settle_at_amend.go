package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FX forward value-date amendment (LG-374).
//
// BookForward (LG-110) nails the value date at booking: SettleAt must
// be in the future, and once booked the only paths that move it are
// RollForward (forward_roll.go:186 — the original lapses and a
// replacement is re-priced at the day's market rate, its value date
// shifted by one tenor: a new agreement, not an amendment) or cancel
// and re-book (which drops the locked rate, and the locked rate is
// the whole point of a forward). A treasurer who needs to move the
// value date — a payment day slips or comes forward, a value date
// was keyed wrong — has no in-place revision today. This module is
// that revision: the absolute-set path for a forward's SettleAt,
// the forward analogue of AmendPreAuthExpiry (LG-370, an absolute
// expiry reset) and AmendLimitOrderExpiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - The lazy expiry runs first, along cancelForwardLocked
//     (forward.go:615-655): an unknown ID fails with
//     ErrForwardNotFound; a settled or cancelled contract fails
//     with ErrForwardNotBooked; a lapsed contract — one the sweep
//     just persisted as cancelled/expired, or one a previous sweep
//     already lapsed — fails with ErrForwardExpired. The operator
//     cannot amend what already lapsed, and a lapsed contract is
//     never revived by moving its value date.
//   - The new value date must be in the future, reusing the
//     BookForward gate (forward.go:389): a zero, past, or current
//     instant fails with the new sentinel
//     ErrForwardSettleAtAmendInvalid and the live value date is
//     untouched. An amendment must never silently kill a resting
//     contract by dating it into the past.
//   - Amending to the current value date is a no-op: the recorded
//     contract is returned unchanged and no audit event fires.
//     The no-op check runs before the future gate, so the no-op
//     rule is unconditional — a same-value amendment is not an
//     amendment at all.
//   - The amendment touches the value date and nothing else: the
//     locked rate (RateNum/RateDen), the counterparties, the
//     currency pair, the notional and cap, the FXSide, and
//     BookedAt/BookedVersion are unchanged — a value-date revision
//     never re-prices. The ledger version is not bumped and no
//     outbox event fires: amending is off-journal, but a real
//     revision is audited as fx_forward_settle_at_amended (with
//     the previous and new value dates, RFC3339 UTC).
//   - Every live reader takes the amended value date: the
//     rollover window anchors on SettleAt + the live grace
//     (maybeRollForwardLocked, forward_roll.go:154-172), so a
//     revision moves the roll window with it, with no roll change;
//     Reconcile's forward_book section reads through
//     forwardEffectiveStatus (forward.go:700-714); and the snapshot
//     round trip (fxForwardsSnapshotLocked, forward.go:822) carries
//     the amended instant verbatim.
//
// Orthogonality: LG-110 owns booking the forward itself (and its
// value date), LG-347 owns the settlement grace-period config (the
// grace, this module the contract's value date), forward_roll owns
// rollover into a replacement (a new agreement at market price,
// this module the same agreement at the same price on a new date),
// LG-370 owns the pre-authorization expiry absolute reset (the
// same pattern, a different record), and AmendLimitOrderExpiry
// owns the limit-order deadline absolute reset (the same pattern,
// a different record).

// FX forward value-date amendment errors.
var (
	// ErrForwardSettleAtAmendInvalid is returned by
	// AmendForwardSettleAt when the new value date is not in the
	// future: a past value date would lapse the contract on the
	// next sweep, and an amendment must never silently kill a
	// resting contract.
	ErrForwardSettleAtAmendInvalid = errors.New("ledger: FX forward amended settle_at must be in the future")
)

// AmendForwardSettleAt moves a booked forward's SettleAt to
// newSettleAt in place and returns the updated contract. The
// locked rate, counterparties, currency pair, notional and cap,
// FXSide, and booking provenance are unchanged, and a failed
// amendment leaves the original value date exactly as it was.
// Amending to the current value date is a no-op. Amending is
// off-journal — it does not bump the ledger version — but a real
// revision is audited as fx_forward_settle_at_amended.
func (l *Ledger) AmendForwardSettleAt(id string, newSettleAt time.Time) (*FXForward, error) {
	return l.AmendForwardSettleAtCtx(context.Background(), id, newSettleAt)
}

// AmendForwardSettleAtCtx is AmendForwardSettleAt with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendForwardSettleAtCtx(ctx context.Context, id string, newSettleAt time.Time) (*FXForward, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Lazy expiry first, along cancelForwardLocked: a lapsed
	// contract reports expired instead of amending.
	l.expireForwardsLocked(now)
	c, ok := l.forwards[id]
	if !ok {
		return nil, ErrForwardNotFound
	}
	if c.Status != ForwardBooked {
		// A contract the lazy sweep just lapsed (or one a previous
		// sweep lapsed) reports expired, not merely "not booked".
		if c.CancelReason == ForwardCancelExpired {
			return nil, fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, id, c.SettleAt.UTC().Format(time.RFC3339))
		}
		return nil, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, id, c.Status)
	}
	if newSettleAt.Equal(c.SettleAt) {
		return c.copy(), nil
	}
	if newSettleAt.IsZero() || !now.Before(newSettleAt) {
		return nil, fmt.Errorf("%w: got %v", ErrForwardSettleAtAmendInvalid, newSettleAt.UTC().Format(time.RFC3339))
	}
	previous := c.SettleAt
	c.SettleAt = newSettleAt
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_forward_settle_at_amended",
		Actor:         "AmendForwardSettleAt",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{c.FromAccount, c.ToAccount},
		Details: map[string]any{
			"forward_id":         id,
			"previous_settle_at": previous.UTC().Format(time.RFC3339),
			"settle_at":          newSettleAt.UTC().Format(time.RFC3339),
		},
	})
	return c.copy(), nil
}
