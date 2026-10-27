package ledger

import (
	"errors"
	"fmt"
	"time"
)

// FX limit-order expiry extension (LG-234).
//
// PlaceLimitOrder (LG-191) parks a price-triggered transfer
// instruction with an optional auto-cancel deadline; this module is
// the orthogonal pre-expiry path: an operator extends that deadline
// before it lapses, instead of letting the order expire and placing
// a fresh one. Extending in place matters because a fresh order is
// a new instruction — it joins the back of the book, loses its
// accumulated fills and fill count, and re-runs placement validation
// — while an extension keeps the same order, the same fills, and
// one audit trail from placement to fill, cancel, or expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only a pending order that already carries an expiry may be
//     extended. An unknown ID fails with ErrLimitOrderNotFound. A
//     filled, cancelled, or expired order fails with
//     ErrLimitOrderNotPending — the state machine moves one way,
//     and a departed order has no deadline left to move. An order
//     with no expiry (zero Expiry = never expires, the LG-191
//     convention) fails with ErrLimitOrderExpiryNotSet, following
//     the LG-230 precedent: extending must never invent a deadline
//     on an order that was meant to rest until filled or manually
//     cancelled.
//   - The new deadline must be strictly later than the current one
//     and in the future: extensions only lengthen, never shorten.
//     An equal, earlier, or past new deadline fails with
//     ErrLimitOrderExpiryExtensionNotLater and the original
//     deadline is untouched, so a mistaken call cannot silently
//     cut a resting order short.
//   - A pending order whose Expiry already passed but whose sweep
//     has not run yet is still extendable: limit orders carry no
//     lazy expiry predicate — only the sweep retires them — so
//     the order is still pending, and extending it to a future
//     time lengthens a live instruction rather than resurrecting
//     a departed one (the LG-229/LG-230 stale-deadline rule).
//     Once the sweep marks it expired, extension is rejected
//     with ErrLimitOrderNotPending.
//   - The extension touches the deadline and nothing else: the
//     limit price, the amount, the filled amount, the fill count,
//     the side, and the state are unchanged, the ledger version
//     is not bumped, and no outbox event fires. The extension
//     lands one audit event (fx_limit_order_expiry_extended, with
//     the previous and new deadlines) so the "who moved the
//     deadline, and from what to what" trail is tamper-evident.
//   - MatchLimitOrders reads the live deadline (see
//     fx_limit_order.go), so no sweep change is needed: an
//     extended order survives sweeps up to its new deadline and
//     expires after it, and the snapshot round-trip of the book
//     carries the extended instant verbatim.
//
// Orthogonality: PlaceLimitOrder owns the initial instruction (and
// its expiry), CancelLimitOrder owns manual departure,
// MatchLimitOrders owns fills and auto-expiry, and this module owns
// only the pre-expiry lengthening. It is orthogonal to the
// mandate pre-authorization expiry extension (LG-233), which
// moves a mandate-limit reservation deadline rather than an order
// book deadline.

// FX limit-order expiry extension errors.
var (
	// ErrLimitOrderExpiryNotSet is returned by
	// ExtendLimitOrderExpiry when the order carries no expiry to
	// extend: the operator places an expiring order instead.
	ErrLimitOrderExpiryNotSet = errors.New("ledger: FX limit order has no expiry to extend")
	// ErrLimitOrderExpiryExtensionNotLater is returned by
	// ExtendLimitOrderExpiry when the new deadline is not strictly
	// later than the current expiry, or is not in the future.
	ErrLimitOrderExpiryExtensionNotLater = errors.New("ledger: FX limit order expiry extension must be later than current expiry and in the future")
)

// ExtendLimitOrderExpiry moves a pending limit order's Expiry to
// newExpiry and returns the updated order. The limit price, the
// amount, the filled amount, the fill count, the side, and the
// state are unchanged, and a failed extension leaves the original
// deadline exactly as it was. Extending is off-journal — it does
// not bump the ledger version — but it is audited as
// fx_limit_order_expiry_extended.
func (l *Ledger) ExtendLimitOrderExpiry(id string, newExpiry time.Time) (LimitOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return LimitOrder{}, fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	if o.Expiry.IsZero() {
		return LimitOrder{}, ErrLimitOrderExpiryNotSet
	}
	now := time.Now()
	if !newExpiry.After(now) || !newExpiry.After(o.Expiry) {
		return LimitOrder{}, ErrLimitOrderExpiryExtensionNotLater
	}
	previous := o.Expiry
	o.Expiry = newExpiry
	l.emitAudit(AuditEvent{
		Op:       "fx_limit_order_expiry_extended",
		Actor:    "ExtendLimitOrderExpiry",
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
