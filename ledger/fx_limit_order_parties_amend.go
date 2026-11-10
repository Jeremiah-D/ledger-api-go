package ledger

import (
	"context"
	"errors"
	"fmt"
)

// FX limit-order parties amendment (LG-373).
//
// PlaceLimitOrder (LG-191) parks a price-triggered transfer
// instruction; AmendLimitOrderAmount (LG-237) revises that
// instruction's size, AmendLimitOrderPrice (LG-236) its trigger
// price, AmendLimitOrderExpiry (LG-234) its deadline. This module
// is the orthogonal in-place settlement-legs path: an operator
// revises the order's payer and payee before it fills, instead of
// cancelling and placing a fresh one. Amending in place matters
// because a fresh order is a new instruction — it joins the back
// of the book, loses its accumulated fills and fill count, and
// re-runs placement validation — while an amendment keeps the same
// order, the same fills, and one audit trail from placement to
// fill, cancel, or expiry. The settlement account is the one
// core field a limit order had no revision entry for: a
// mis-keyed account (treasury cut-over, payroll-account change)
// could only be fixed by cancel-and-replace, which forfeits the
// order's queue position and its filled history.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a pending order may be amended. An unknown ID fails
//     with ErrLimitOrderNotFound. A filled, cancelled, or
//     expired order fails with ErrLimitOrderNotPending — the
//     state machine moves one way, and a departed order has no
//     legs left to move. (A pending order whose Expiry already
//     passed but whose sweep has not run yet is still
//     amendable, the LG-234 stale-deadline rule: limit orders
//     carry no lazy expiry predicate, so the order is still
//     pending, and amending it revises a live instruction
//     rather than resurrecting a departed one.)
//   - The new parties reuse PlaceLimitOrder's own validation:
//     both accounts non-empty and different. An empty party or
//     equal parties fails with ErrLimitOrderPartiesAmendInvalid
//     and the original legs are untouched. Amending to the same
//     pair is a no-op: the original order is returned with no
//     audit event.
//   - The amendment touches the parties and nothing else: the
//     currency pair, side, limit price, amount, filled amount,
//     fill count, state, and expiry are unchanged, the ledger
//     version is not bumped, and no outbox event fires. The
//     amendment lands one audit event
//     (fx_limit_order_parties_amended, with the previous and new
//     parties) so the "who re-routed the order, and from what
//     to what" trail is tamper-evident.
//   - Settled fills are facts and are not rewritten: a fill
//     already posted keeps its old legs, and only the remaining
//     amount settles through the new accounts. The amendment
//     itself runs no funds, freeze, or overdraft check — the
//     fill-time gates stay authoritative, exactly as the amount
//     amendment runs no funds check. MatchLimitOrders reads the
//     live legs through fillLimitOrderLocked (see
//     fx_limit_order.go), so no sweep change is needed: the
//     next sweep's fill debits the new payer's available
//     source-currency balance and credits the new payee, and
//     the snapshot round-trip of the book carries the amended
//     parties verbatim.
//
// Orthogonality: PlaceLimitOrder owns the initial instruction
// (and its legs), AmendLimitOrderPrice owns the trigger price,
// AmendLimitOrderAmount owns the size, AmendLimitOrderExpiry
// owns the deadline, CancelLimitOrder owns manual departure,
// MatchLimitOrders owns fills and auto-expiry, and this module
// owns only the settlement-legs revision. It is orthogonal to
// the scheduled-transfer parties amendment (LG-364), which
// revises a standing transfer plan's legs rather than a
// resting order-book instruction, and to the direct-debit payer
// amendment (LG-366), which revises a mandate's payer rather
// than an order's settlement legs. A request-for-payment's
// parties are identity, not settlement legs, and stay
// unamendable (LG-363).

// FX limit-order parties amendment errors.
var (
	// ErrLimitOrderPartiesAmendInvalid is returned by
	// AmendLimitOrderParties when the new parties are empty or
	// equal: the amendment must never park an order with no
	// payer, no payee, or a self-transfer.
	ErrLimitOrderPartiesAmendInvalid = errors.New("ledger: FX limit order amended parties must be non-empty and differ")
)

// AmendLimitOrderParties moves a pending limit order's payer and
// payee to newFrom/newTo and returns the updated order. The
// currency pair, side, limit price, amount, filled amount, fill
// count, state, and expiry are unchanged, and a failed amendment
// leaves the original legs exactly as they were. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as fx_limit_order_parties_amended.
func (l *Ledger) AmendLimitOrderParties(id string, newFrom, newTo AccountID) (LimitOrder, error) {
	return l.AmendLimitOrderPartiesCtx(context.Background(), id, newFrom, newTo)
}

// AmendLimitOrderPartiesCtx is AmendLimitOrderParties with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendLimitOrderPartiesCtx(ctx context.Context, id string, newFrom, newTo AccountID) (LimitOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return LimitOrder{}, fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	if newFrom == "" || newTo == "" || newFrom == newTo {
		return LimitOrder{}, fmt.Errorf("%w: got %q -> %q", ErrLimitOrderPartiesAmendInvalid, newFrom, newTo)
	}
	if newFrom == o.FromAccount && newTo == o.ToAccount {
		return *o, nil
	}
	previousFrom, previousTo := o.FromAccount, o.ToAccount
	o.FromAccount, o.ToAccount = newFrom, newTo
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "fx_limit_order_parties_amended",
		Actor:    "AmendLimitOrderParties",
		TraceID:  id,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":              id,
			"previous_from_account": previousFrom,
			"from_account":          newFrom,
			"previous_to_account":   previousTo,
			"to_account":            newTo,
		},
	})
	return *o, nil
}
