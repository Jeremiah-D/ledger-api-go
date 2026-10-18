package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// FX forward auto-rollover (LG-117).
//
// A booked forward is a bilateral rate agreement with a settlement
// date. When the date arrives the desk either settles at the locked
// rate or — when the commercial relationship continues — rolls the
// obligation into a new agreement at the current market price, today by
// hand before expiry. Auto-rollover makes that roll the default for
// opted-in ledgers: SettleForward, called while a booked contract sits
// inside its roll window (RollWindow before the last settleable
// instant, SettleAt + the settlement grace period), books a
// replacement contract at the live market price and lapses the
// original, instead of settling.
//
// The roll is idempotent: the replacement contract's ID derives
// deterministically from the original ID and the roll sequence
// ("roll/<id>/<n>"), so a retried settlement replays to the same
// replacement contract instead of booking a second one. A rolled
// settlement reports ErrForwardRolled naming the new contract: the
// obligation was extended, not settled — the operator settles the
// replacement when its value date arrives.
//
// The replacement inherits the original's commercial dimensions: the
// counterparties (From/To accounts), the notional, the opt-in notional
// cap, and the dealing side. What changes is the price — the current
// market dealing price on the contract's side, priced like a fresh
// booking — and the pinned market-table version (the LG-45 convention:
// the new contract pins the current fxSnapshotVersion, not the
// original's). Its value date pushes forward by the original tenor
// (SettleAt - BookedAt): a 30-day forward rolled on day 29 settles 30
// days after the original value date.
//
// MaxRolls caps how many times one contract may roll: a desk that
// wants a standing hedge re-books deliberately, not through an
// infinite auto-roll. When the cap is reached the roll is skipped and
// the contract settles normally at its locked rate — the settlement
// promise outranks the roll preference.
//
// A roll needs a live market price: without a rate for the pair the
// roll cannot be priced and fails closed (the wrapped FX error), the
// contract stays booked, and the operator retries after populating the
// rate table or settles the original manually.

// ErrInvalidForwardRollover is returned by EnableForwardRollover when
// the roll window is not positive or maxRolls is below 1. Both are
// configuration errors: fail-fast at setup, like every other
// risk-control threshold in this package.
var ErrInvalidForwardRollover = errors.New("ledger: forward rollover config must have a positive roll window and at least one max roll")

// ErrForwardRolled is returned by SettleForward when auto-rollover
// replaced the contract instead of settling it: the message names the
// replacement contract. The original contract is lapsed (cancelled,
// reason "expired") and the fx_forward_rolled audit event binds the
// two. It is a 409-class error: the settlement did not happen, but the
// request was processed — settle the new contract when its value date
// arrives.
var ErrForwardRolled = errors.New("ledger: FX forward was rolled over to a new contract instead of settling")

// ForwardRolloverConfig is the opt-in auto-rollover configuration for
// FX forward contracts: SettleForward rolls a booked contract whose
// last settleable instant (SettleAt + the settlement grace period) is
// within RollWindow into a replacement contract at the current market
// price, instead of settling it. MaxRolls caps the rolls per contract
// (see the package comment).
type ForwardRolloverConfig struct {
	// RollWindow is how close to the last settleable instant a booked
	// contract must be to roll, e.g. 48h. Must be positive.
	RollWindow time.Duration
	// MaxRolls is the per-contract roll budget: a contract whose Rolls
	// already reached it settles normally instead of rolling. Must be
	// at least 1.
	MaxRolls int
}

// EnableForwardRollover opts the ledger into FX forward auto-rollover
// (see the package comment). A non-positive roll window or a maxRolls
// below 1 is rejected with ErrInvalidForwardRollover. Structural
// config, like the rate table: it does not bump the journal version.
func (l *Ledger) EnableForwardRollover(rollWindow time.Duration, maxRolls int) error {
	if rollWindow <= 0 || maxRolls < 1 {
		return ErrInvalidForwardRollover
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forwardRollover = &ForwardRolloverConfig{RollWindow: rollWindow, MaxRolls: maxRolls}
	return nil
}

// DisableForwardRollover turns FX forward auto-rollover off: booked
// contracts settle normally again. Structural config — it does not
// bump the journal version — and it leaves already-rolled contracts
// untouched.
func (l *Ledger) DisableForwardRollover() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forwardRollover = nil
}

// ForwardRolloverConfig reports the auto-rollover config and whether
// rollover is enabled.
func (l *Ledger) ForwardRolloverConfig() (ForwardRolloverConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.forwardRollover == nil {
		return ForwardRolloverConfig{}, false
	}
	return *l.forwardRollover, true
}

// rollForwardKey derives the replacement contract's ID from the
// original forward ID and the roll sequence number: deterministic, so
// a retried roll replays to the same contract (idempotent).
func rollForwardKey(id string, n int) string {
	return fmt.Sprintf("roll/%s/%d", id, n)
}

// rolledReplayLocked reports whether id was rolled into a replacement
// contract: the replacement's ID derives deterministically from id and
// the roll sequence (see rollForwardKey), and its RolledFromID names
// the original, so a genuine expiry can never match. Used by
// settleForwardLocked to make the roll idempotent: a retried
// settlement after a roll replays to the same replacement instead of
// failing with ErrForwardExpired. Callers must hold l.mu.
func (l *Ledger) rolledReplayLocked(id string) (*FXForward, bool) {
	c, ok := l.forwards[id]
	if !ok || c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		return nil, false
	}
	key := rollForwardKey(id, c.Rolls+1)
	rolled, ok := l.forwards[key]
	if !ok || rolled.RolledFromID != id {
		return nil, false
	}
	return rolled.copy(), true
}

// maybeRollForwardLocked applies the opt-in auto-rollover to a booked
// contract: when the contract's last settleable instant is inside the
// roll window and the roll budget is not exhausted, it books the
// replacement at the current market price and lapses the original.
// Returns the (possibly new) contract, whether a roll happened, and
// any error. When the roll budget is exhausted the contract is
// returned unchanged — the settlement promise outranks the roll
// preference — so the caller settles the original at its locked rate.
// Callers must hold the write lock.
func (l *Ledger) maybeRollForwardLocked(c *FXForward, now time.Time) (*FXForward, bool, error) {
	cfg := l.forwardRollover
	if cfg == nil || c.Rolls >= cfg.MaxRolls {
		return c, false, nil
	}
	lastSettleable := c.SettleAt.Add(ForwardSettlementGracePeriod)
	remaining := lastSettleable.Sub(now)
	if remaining <= 0 || remaining > cfg.RollWindow {
		return c, false, nil
	}
	newC, err := l.rollForwardLocked(c, now)
	if err != nil {
		return c, false, err
	}
	return newC, true, nil
}

// rollForwardLocked books the replacement contract for c at the current
// market dealing price and lapses c (cancelled, reason "expired").
// Idempotent: the replacement ID derives from c's ID and the roll
// sequence, so a replay returns the existing replacement without
// booking again. The replacement inherits the counterparties, the
// notional, the opt-in notional cap, and the dealing side; the price is
// the live market price and the pinned market-table version is the
// current one (the LG-45 convention); the value date pushes forward by
// the original tenor. Emits the fx_forward_rolled audit event binding
// the original to its replacement. Callers must hold the write lock.
func (l *Ledger) rollForwardLocked(c *FXForward, now time.Time) (*FXForward, error) {
	n := c.Rolls + 1
	key := rollForwardKey(c.ID, n)
	if existing, ok := l.forwards[key]; ok {
		// Replay: the roll already happened — return the
		// replacement without booking or auditing again.
		return existing.copy(), nil
	}
	// The roll re-prices at the live market price on the contract's
	// dealing side: a roll is a new agreement at today's price, not a
	// repricing of the old one. Without a rate for the pair the roll
	// cannot be priced — fail closed, the contract stays booked.
	eff, err := l.effectiveFXRateLocked(fxPair{from: c.FromCurrency, to: c.ToCurrency}, c.FXSide, now)
	if err != nil {
		return nil, fmt.Errorf("ledger: cannot roll forward %q without a live %s->%s rate: %w",
			c.ID, c.FromCurrency, c.ToCurrency, err)
	}
	tenor := c.SettleAt.Sub(c.BookedAt)
	if tenor <= 0 {
		return nil, fmt.Errorf("%w: forward %q has a non-positive tenor", ErrForwardInvalid, c.ID)
	}
	if _, err := l.bookForwardLocked(ForwardBooking{
		ID:               key,
		From:             c.FromAccount,
		To:               c.ToAccount,
		FromCurrency:     c.FromCurrency,
		ToCurrency:       c.ToCurrency,
		Rate:             big.NewRat(eff.Num, eff.Den),
		NotionalCents:    c.NotionalCents,
		NotionalCapCents: c.NotionalCapCents,
		SettleAt:         c.SettleAt.Add(tenor),
		FXSide:           c.FXSide,
	}, now); err != nil {
		return nil, err
	}
	// Lineage on the stored contract: bookForwardLocked validated and
	// stored it, the roll sequence is this module's bookkeeping.
	stored := l.forwards[key]
	stored.Rolls = n
	stored.RolledFromID = c.ID
	// The original lapses: expiry IS cancellation (see forward.go),
	// and a roll is the operator's standing instruction to replace the
	// obligation rather than let it settle — the terminal state is
	// "cancelled"/"expired", bound to the replacement by the audit
	// event below.
	c.Status = ForwardCancelled
	c.CancelReason = ForwardCancelExpired
	l.emitAudit(AuditEvent{
		Op:            "fx_forward_rolled",
		Actor:         "SettleForward",
		TraceID:       c.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{c.FromAccount, c.ToAccount},
		Details: map[string]any{
			"forward_id":                c.ID,
			"new_forward_id":            key,
			"rolls":                     n,
			"from_currency":             c.FromCurrency,
			"to_currency":               c.ToCurrency,
			"old_locked_rate":           fmt.Sprintf("%d/%d", c.RateNum, c.RateDen),
			"new_locked_rate":           fmt.Sprintf("%d/%d", stored.RateNum, stored.RateDen),
			"old_rate_snapshot_version": c.RateSnapshotVersion,
			"new_rate_snapshot_version": stored.RateSnapshotVersion,
			"old_settle_at":             c.SettleAt.UTC().Format(time.RFC3339),
			"new_settle_at":             stored.SettleAt.UTC().Format(time.RFC3339),
			"fx_side":                   c.FXSide,
		},
	})
	return stored.copy(), nil
}
