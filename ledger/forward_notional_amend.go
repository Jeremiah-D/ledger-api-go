package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FX forward notional amendment (LG-382).
//
// BookForward (LG-110) nails the notional at booking: NotionalCents
// is the source-currency amount to convert at settlement, with an
// opt-in upper bound in NotionalCapCents (0 = no cap, checked at
// booking in forward.go:383-387 and re-checked at settlement in
// forward.go:524). Today the only way to change the size of a booked
// agreement is cancel-and-re-book, which drops the locked rate and
// the rollover window. A treasurer who needs a bigger or smaller
// conversion — a payment grew, a hedge is trimmed — has no in-place
// revision. This module is that revision: the absolute-set path for
// a forward's NotionalCents, the forward analogue of
// AmendForwardSettleAt (LG-374, the same pattern, the contract's
// other field).
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
//     never revived by resizing it.
//   - The new notional must be positive, reusing the BookForward
//     gate (forward.go:380): zero or negative fails with the new
//     sentinel ErrForwardNotionalAmendInvalid and the live notional
//     is untouched. An amendment must never silently zero a resting
//     contract.
//   - The new notional must fit the opt-in cap: when
//     NotionalCapCents > 0 and the new value exceeds it, the
//     amendment fails with ErrForwardCapExceeded and the live
//     notional is untouched. The cap itself is never moved by this
//     revision — it was fixed at booking.
//   - Amending to the current notional is a no-op: the recorded
//     contract is returned unchanged and no audit event fires.
//     The no-op check runs before the validity gates, so the
//     no-op rule is unconditional — a same-value amendment is not
//     an amendment at all.
//   - The amendment touches the notional and nothing else: the
//     locked rate (RateNum/RateDen) is frozen for the contract's
//     life (forward.go:23-28) — resizing changes how much converts,
//     never the agreed price — and SettleAt, the counterparties,
//     the currency pair, the cap, the FXSide, and
//     BookedAt/BookedVersion are unchanged. The ledger version is
//     not bumped and no outbox event fires: amending is
//     off-journal, but a real revision is audited as
//     fx_forward_notional_amended (with the previous and new
//     notional, in cents).
//   - Every live reader takes the amended notional: settlement
//     converts the live NotionalCents at the locked rate
//     (forward.go:524-535), so the next sweep settles the new
//     size; already-settled fills keep the old size. The snapshot
//     round trip (fxForwardsSnapshotLocked, forward.go:822) carries
//     the amended notional verbatim.
//
// Orthogonality: LG-110 owns booking the forward itself (and its
// notional), LG-117 owns automatic rollover (it moves the value
// date, this module the size), LG-347 owns the settlement grace
// period (a time tolerance, this module an amount), LG-374 owns
// the value-date revision (SettleAt, this module NotionalCents —
// the other field of the same contract), and AmendForwardSettleAt
// is the pattern this module mirrors.

// FX forward notional amendment errors.
var (
	// ErrForwardNotionalAmendInvalid is returned by
	// AmendForwardNotional when the new notional is not positive:
	// a zero or negative notional would silently kill a resting
	// contract.
	ErrForwardNotionalAmendInvalid = errors.New("ledger: FX forward amended notional must be positive")
)

// AmendForwardNotional revises a booked forward's NotionalCents to
// newNotionalCents in place and returns the updated contract. The
// locked rate, value date, counterparties, currency pair, cap,
// FXSide, and booking provenance are unchanged, and a failed
// amendment leaves the original notional exactly as it was.
// Amending to the current notional is a no-op. Amending is
// off-journal — it does not bump the ledger version — but a real
// revision is audited as fx_forward_notional_amended.
func (l *Ledger) AmendForwardNotional(id string, newNotionalCents int64) (*FXForward, error) {
	return l.AmendForwardNotionalCtx(context.Background(), id, newNotionalCents)
}

// AmendForwardNotionalCtx is AmendForwardNotional with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendForwardNotionalCtx(ctx context.Context, id string, newNotionalCents int64) (*FXForward, error) {
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
	if newNotionalCents == c.NotionalCents {
		return c.copy(), nil
	}
	if newNotionalCents <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrForwardNotionalAmendInvalid, newNotionalCents)
	}
	if c.NotionalCapCents > 0 && newNotionalCents > c.NotionalCapCents {
		return nil, fmt.Errorf("%w: notional %d exceeds cap %d", ErrForwardCapExceeded, newNotionalCents, c.NotionalCapCents)
	}
	previous := c.NotionalCents
	c.NotionalCents = newNotionalCents
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_forward_notional_amended",
		Actor:         "AmendForwardNotional",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{c.FromAccount, c.ToAccount},
		Details: map[string]any{
			"forward_id":              id,
			"previous_notional_cents": previous,
			"notional_cents":          newNotionalCents,
		},
	})
	return c.copy(), nil
}
