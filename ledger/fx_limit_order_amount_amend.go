package ledger

import (
	"context"
	"errors"
	"fmt"
)

// FX limit-order amount amendment (LG-237).
//
// PlaceLimitOrder (LG-191) parks a price-triggered transfer
// instruction; AmendLimitOrderPrice (LG-236) revises that
// instruction's trigger price. This module is the orthogonal
// in-place size path: an operator revises the order's total
// size before it fills, instead of cancelling and placing a
// fresh one. Amending in place matters because a fresh order
// is a new instruction — it joins the back of the book, loses
// its accumulated fills and fill count, and re-runs placement
// validation — while an amendment keeps the same order, the
// same fills, and one audit trail from placement to fill,
// cancel, or expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a pending order may be amended. An unknown ID fails
//     with ErrLimitOrderNotFound. A filled, cancelled, or
//     expired order fails with ErrLimitOrderNotPending — the
//     state machine moves one way, and a departed order has no
//     size left to move. (A pending order whose Expiry already
//     passed but whose sweep has not run yet is still
//     amendable, the LG-234 stale-deadline rule: limit orders
//     carry no lazy expiry predicate, so the order is still
//     pending, and amending it revises a live instruction
//     rather than resurrecting a departed one.)
//   - The new amount must be positive and at least the amount
//     already filled: a zero or negative amount, or an amount
//     below FilledCents, fails with
//     ErrLimitOrderAmountAmendInvalid and the original amount
//     is untouched. An amount below the filled amount would
//     imply negative remaining size and would rewrite history
//     — the fills already settled are facts, not drafts — so
//     the floor is the filled amount, not zero. An amount
//     equal to the filled amount is legal: it closes the
//     remaining size to zero while leaving the state machine
//     alone (the state still moves only through a fill, a
//     cancel, or a sweep expiry, never through an amendment).
//   - The amendment touches the amount and nothing else: the
//     currency pair, the side, the limit price, the filled
//     amount, the fill count, the state, and the expiry are
//     unchanged, the ledger version is not bumped, and no
//     outbox event fires. The amendment lands one audit event
//     (fx_limit_order_amount_amended, with the previous and new
//     amounts) so the "who resized the order, and from what to
//     what" trail is tamper-evident.
//   - MatchLimitOrders reads the live remainder through
//     RemainingCents() (see fx_limit_order.go), so no sweep
//     change is needed: an amended order fills up to its new
//     remainder on the next sweep — a larger order keeps
//     filling past its old size, a smaller order stops at its
//     new size — and the snapshot round-trip of the book
//     carries the amended amount verbatim.
//
// Orthogonality: PlaceLimitOrder owns the initial instruction
// (and its size), AmendLimitOrderPrice owns the trigger price,
// ExtendLimitOrderExpiry owns the deadline, CancelLimitOrder
// owns manual departure, MatchLimitOrders owns fills and
// auto-expiry, and this module owns only the pre-fill size
// revision. It is orthogonal to the mandate pre-authorization
// amount amendment (LG-235), which revises a mandate-limit
// reservation amount rather than an order-book size, and to
// the partial-freeze amount amendment (LG-238), which revises
// a risk-control reservation rather than a resting order.

// FX limit-order amount amendment errors.
var (
	// ErrLimitOrderAmountAmendInvalid is returned by
	// AmendLimitOrderAmount when the new amount is not positive
	// or is below the amount already filled: the amendment must
	// never imply negative remaining size or rewrite settled
	// fills.
	ErrLimitOrderAmountAmendInvalid = errors.New("ledger: FX limit order amended amount must be positive and at least the filled amount")
)

// AmendLimitOrderAmount moves a pending limit order's total size
// to newAmountCents and returns the updated order. The currency
// pair, side, limit price, filled amount, fill count, state,
// and expiry are unchanged, and a failed amendment leaves the
// original amount exactly as it was. Amending is off-journal —
// it does not bump the ledger version — but it is audited as
// fx_limit_order_amount_amended.
func (l *Ledger) AmendLimitOrderAmount(id string, newAmountCents int64) (LimitOrder, error) {
	return l.AmendLimitOrderAmountCtx(context.Background(), id, newAmountCents)
}

// AmendLimitOrderAmountCtx is AmendLimitOrderAmount with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendLimitOrderAmountCtx(ctx context.Context, id string, newAmountCents int64) (LimitOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return LimitOrder{}, fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	if newAmountCents <= 0 || newAmountCents < o.FilledCents {
		return LimitOrder{}, fmt.Errorf("%w: got %d with %d already filled", ErrLimitOrderAmountAmendInvalid, newAmountCents, o.FilledCents)
	}
	previousAmount := o.AmountCents
	o.AmountCents = newAmountCents
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "fx_limit_order_amount_amended",
		Actor:    "AmendLimitOrderAmount",
		TraceID:  id,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":              id,
			"previous_amount_cents": previousAmount,
			"amount_cents":          newAmountCents,
			"filled_cents":          o.FilledCents,
		},
	})
	return *o, nil
}
