package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FX limit-order expiry amendment (LG-338).
//
// PlaceLimitOrder (LG-191) parks a price-triggered transfer
// instruction with an optional auto-cancel deadline;
// ExtendLimitOrderExpiry (LG-234) lengthens that deadline. This
// module is the orthogonal absolute-set path: an operator sets
// the deadline to an exact instant — shortening it, lengthening
// it, clearing it to never-expire, or setting one on an order
// that was placed without one — instead of cancelling and
// placing a fresh one. Amending in place matters because a fresh
// order is a new instruction — it joins the back of the book,
// loses its accumulated fills and fill count, and re-runs
// placement validation — while an amendment keeps the same
// order, the same fills, and one audit trail from placement to
// fill, cancel, or expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a pending order may be amended. An unknown ID fails
//     with ErrLimitOrderNotFound. A filled, cancelled, or
//     expired order fails with ErrLimitOrderNotPending — the
//     state machine moves one way, and a departed order has no
//     deadline left to move. (A pending order whose Expiry
//     already passed but whose sweep has not run yet is still
//     amendable, the LG-234 stale-deadline rule: limit orders
//     carry no lazy expiry predicate, so the order is still
//     pending, and amending it revises a live instruction
//     rather than resurrecting a departed one.)
//   - The new expiry must be in the future or zero: a zero
//     Expiry clears the deadline (the LG-191 convention — the
//     order never expires), and any non-zero deadline must be
//     strictly after now, or the amendment fails with
//     ErrLimitOrderExpiryAmendInvalid and the original deadline
//     is untouched. A past deadline would expire the order on
//     the next sweep, and an amendment must never silently kill
//     a resting order. Unlike ExtendLimitOrderExpiry, the new
//     deadline need not be later than the current one —
//     shortening is the point — and the order need not already
//     carry one: setting a first deadline on a never-expiring
//     order is legal, as is clearing an existing deadline to
//     zero.
//   - The amendment touches the deadline and nothing else: the
//     currency pair, the side, the limit price, the amount, the
//     filled amount, the fill count, and the state are unchanged,
//     the ledger version is not bumped, and no outbox event
//     fires. The amendment lands one audit event
//     (fx_limit_order_expiry_amended, with the previous and new
//     deadlines) so the "who moved the deadline, and from what
//     to what" trail is tamper-evident.
//   - MatchLimitOrders reads the live deadline (see
//     fx_limit_order.go), so no sweep change is needed: an
//     amended order expires at its new deadline on the next
//     sweep — a shortened order expires sooner, a lengthened
//     one survives past its old deadline, and a cleared one
//     never expires — and the snapshot round-trip of the book
//     carries the amended instant verbatim.
//
// Orthogonality: PlaceLimitOrder owns the initial instruction
// (and its deadline), ExtendLimitOrderExpiry owns the
// lengthen-only pre-expiry path, AmendLimitOrderPrice owns the
// trigger price, AmendLimitOrderAmount owns the size,
// CancelLimitOrder owns manual departure, MatchLimitOrders owns
// fills and auto-expiry, and this module owns only the absolute
// deadline revision. It is orthogonal to the mandate
// pre-authorization expiry amendment, which revises a
// mandate-limit reservation deadline rather than an order-book
// deadline.

// FX limit-order expiry amendment errors.
var (
	// ErrLimitOrderExpiryAmendInvalid is returned by
	// AmendLimitOrderExpiry when the new expiry is neither zero
	// nor in the future: a past deadline would expire the order
	// on the next sweep, and an amendment must never silently
	// kill a resting order.
	ErrLimitOrderExpiryAmendInvalid = errors.New("ledger: FX limit order amended expiry must be in the future or zero")
)

// AmendLimitOrderExpiry moves a pending limit order's Expiry to
// newExpiry and returns the updated order. A zero newExpiry
// clears the deadline (the order never expires). The currency
// pair, side, limit price, amount, filled amount, fill count,
// and state are unchanged, and a failed amendment leaves the
// original deadline exactly as it was. Amending is off-journal —
// it does not bump the ledger version — but it is audited as
// fx_limit_order_expiry_amended.
func (l *Ledger) AmendLimitOrderExpiry(id string, newExpiry time.Time) (LimitOrder, error) {
	return l.AmendLimitOrderExpiryCtx(context.Background(), id, newExpiry)
}

// AmendLimitOrderExpiryCtx is AmendLimitOrderExpiry with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendLimitOrderExpiryCtx(ctx context.Context, id string, newExpiry time.Time) (LimitOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return LimitOrder{}, fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	now := time.Now()
	if !newExpiry.IsZero() && !newExpiry.After(now) {
		return LimitOrder{}, fmt.Errorf("%w: got %v", ErrLimitOrderExpiryAmendInvalid, newExpiry)
	}
	previous := o.Expiry
	o.Expiry = newExpiry
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "fx_limit_order_expiry_amended",
		Actor:    "AmendLimitOrderExpiry",
		TraceID:  id,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":        id,
			"previous_expiry": previous.UTC().Format(time.RFC3339),
			"expiry":          newExpiry.UTC().Format(time.RFC3339),
		},
	})
	return *o, nil
}
