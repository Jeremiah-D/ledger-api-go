package ledger

import (
	"context"
	"errors"
	"fmt"
)

// FX limit-order price amendment (LG-236).
//
// PlaceLimitOrder (LG-191) parks a price-triggered transfer
// instruction; ExtendLimitOrderExpiry (LG-234) moves that
// instruction's deadline. This module is the orthogonal in-place
// price path: an operator revises the trigger price before the
// order fills, instead of cancelling and placing a fresh one.
// Amending in place matters because a fresh order is a new
// instruction — it joins the back of the book, loses its
// accumulated fills and fill count, and re-runs placement
// validation — while an amendment keeps the same order, the same
// fills, and one audit trail from placement to fill, cancel, or
// expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a pending order may be amended. An unknown ID fails
//     with ErrLimitOrderNotFound. A filled, cancelled, or expired
//     order fails with ErrLimitOrderNotPending — the state
//     machine moves one way, and a departed order has no trigger
//     left to move. (A pending order whose Expiry already passed
//     but whose sweep has not run yet is still amendable, the
//     LG-234 stale-deadline rule: limit orders carry no lazy
//     expiry predicate, so the order is still pending, and
//     amending it revises a live instruction rather than
//     resurrecting a departed one.)
//   - The new limit must be a positive ratio: limitNum > 0 and
//     limitDen > 0, or the amendment fails with
//     ErrLimitOrderPriceAmendNotPositive and the original limit
//     is untouched. The ratio is stored exactly as given — no
//     reduction, no float — so the trigger comparison stays
//     exact big.Rat arithmetic.
//   - The amendment touches the limit price and nothing else:
//     the currency pair, the side, the amount, the filled amount,
//     the fill count, the state, and the expiry are unchanged,
//     the ledger version is not bumped, and no outbox event
//     fires. The amendment lands one audit event
//     (fx_limit_order_price_amended, with the previous and new
//     limits) so the "who moved the trigger, and from what to
//     what" trail is tamper-evident.
//   - MatchLimitOrders reads the live limit through limitRat()
//     (see fx_limit_order.go), so no sweep change is needed: an
//     amended order triggers at its new price on the next sweep,
//     and the snapshot round-trip of the book carries the
//     amended ratio verbatim.
//
// Orthogonality: PlaceLimitOrder owns the initial instruction
// (and its price), ExtendLimitOrderExpiry owns the deadline,
// CancelLimitOrder owns manual departure, MatchLimitOrders owns
// fills and auto-expiry, and this module owns only the
// pre-fill price revision. It is orthogonal to the mandate
// pre-authorization amount amendment (LG-235), which revises a
// mandate-limit reservation amount rather than an order-book
// trigger price.

// FX limit-order price amendment errors.
var (
	// ErrLimitOrderPriceAmendNotPositive is returned by
	// AmendLimitOrderPrice when the new limit is not a positive
	// ratio: limitNum and limitDen must both exceed zero.
	ErrLimitOrderPriceAmendNotPositive = errors.New("ledger: FX limit order amended price must be a positive ratio")
)

// AmendLimitOrderPrice moves a pending limit order's trigger price
// to limitNum/limitDen and returns the updated order. The currency
// pair, side, amount, filled amount, fill count, state, and expiry
// are unchanged, and a failed amendment leaves the original limit
// exactly as it was. Amending is off-journal — it does not bump
// the ledger version — but it is audited as
// fx_limit_order_price_amended.
func (l *Ledger) AmendLimitOrderPrice(id string, limitNum, limitDen int64) (LimitOrder, error) {
	return l.AmendLimitOrderPriceCtx(context.Background(), id, limitNum, limitDen)
}

// AmendLimitOrderPriceCtx is AmendLimitOrderPrice with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendLimitOrderPriceCtx(ctx context.Context, id string, limitNum, limitDen int64) (LimitOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return LimitOrder{}, fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	if limitNum <= 0 || limitDen <= 0 {
		return LimitOrder{}, fmt.Errorf("%w: got %d/%d", ErrLimitOrderPriceAmendNotPositive, limitNum, limitDen)
	}
	previousNum, previousDen := o.LimitNum, o.LimitDen
	o.LimitNum, o.LimitDen = limitNum, limitDen
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "fx_limit_order_price_amended",
		Actor:    "AmendLimitOrderPrice",
		TraceID:  id,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":          id,
			"previous_limit_num": previousNum,
			"previous_limit_den": previousDen,
			"limit_num":         limitNum,
			"limit_den":         limitDen,
		},
	})
	return *o, nil
}
