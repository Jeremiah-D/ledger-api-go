package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// FX forward settlement grace period runtime revision (LG-347).
//
// A booked forward is a bilateral rate agreement with a value date
// (SettleAt). Settlement may land a little late — an operational
// delay, a bank holiday, a counterparty that is slow to confirm —
// and the ledger tolerates that: the contract may still settle for
// ForwardSettlementGracePeriod (24h) past SettleAt, and only past
// that grace is it considered lapsed (cancelled, reason "expired").
// The grace is a construction-time constant (forward.go): when a
// settlement system fault or a bank holiday strands in-flight
// forwards, the operator's only way to widen the window — saving
// contracts that would otherwise lapse at SettleAt + 24h and have
// to be re-booked at a new market price, with all economic terms
// changed — or to tighten it — clearing stale contracts faster —
// was to change the code and redeploy. This module is the
// orthogonal runtime path: an operator revises the live grace in
// place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along LG-312
// (the hold horizon cap: the same duration-knob, (error) audit
// shape) and LG-344 (the check order — validate, compare, swap,
// then audit):
//
//   - The grace is a live field on the Ledger, guarded by l.mu —
//     the same lock that guards the forwards map and the
//     forwardRollover config, so no second lock is introduced and
//     no lock order is created. Every lapse decision point
//     snapshots it once under the lock it already holds:
//     expireForwardsLocked (the sweep), forwardBookReportLocked
//     (the Reconcile forward_book section, read lock suffices),
//     maybeRollForwardLocked (the roll window's anchor
//     lastSettleable = SettleAt + grace), and ClosePeriodPreflight
//     (the booked-forwards blocker scan, read lock). The settle
//     path reads it through the lazy sweep it runs first.
//   - The revision is future-only and applies to lapse decisions
//     not yet made: widening the grace re-admits a booked contract
//     that no sweep, settle, or read has yet touched — lazy expiry
//     recomputes on every call, so a revision that lands before
//     the next decision rescues the contract. A contract already
//     persisted as cancelled/expired is a terminal state and is
//     never revived: the revision changes no persisted state. New
//     bookings are unaffected (the grace only acts at the
//     settle/lapse decision points, never in the booking terms).
//   - Validation is fail-closed with no silent normalization: the
//     new grace must be positive, else
//     ErrInvalidForwardSettlementGracePeriod and the live grace
//     stays exactly as it was (the returned value is the live
//     grace in that case too). Zero is rejected, not a "close the
//     window" state: a zero grace would lapse a contract the
//     instant SettleAt arrives, while settlement itself requires
//     now >= SettleAt — the contract could never settle at all,
//     a semantic trap, not a meaningful "off" state. There is no
//     upper bound: an over-long grace delays the cleanup of stale
//     contracts and drags the book's exposure out, and the
//     operator owns that trade-off (documented in the README,
//     along LG-312/LG-344).
//   - A revision identical to the live grace is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - The amendment is structural config, not ledger state: it
//     bumps neither the version nor the audit chain, writes no
//     journal entry, and emits no outbox event. It lands one
//     fx_forward_grace_period_retuned audit event
//     (previous_grace_period / current_grace_period, as duration
//     strings, along LG-312's hold_max_duration_amended shape).
//     The Ctx variant attributes the operator (LG-73); with no
//     operator in ctx the actor defaults to DefaultAuditActor
//     ("system").
//   - Snapshots do not carry the grace: ImportSnapshot builds
//     through New, which always seeds the construction-time
//     default 24h, so a restart (or a disaster-recovery restore)
//     restores 24h while the revision events themselves stay in
//     the audit trail for traceability.
//   - The getter shares its name with the package-level constant
//     ForwardSettlementGracePeriod (forward.go): the constant is
//     the default value (and the seed), the method
//     Ledger.ForwardSettlementGracePeriod() reads the live value.
//     They live in different namespaces (package vs. method set),
//     so they coexist; unqualified ForwardSettlementGracePeriod
//     inside the package still means the constant (the default /
//     seed), and the live value is always read through the method
//     or the l.forwardGracePeriod field — never mixed.
//
// Orthogonality: LG-110 owns the forward mechanism itself (this
// module is its missing grace-revision entry point); LG-117 owns
// forward auto-rollover (the roll window's anchor moves with the
// grace — lastSettleable = SettleAt + grace — but RollWindow and
// MaxRolls are untouched); LG-45 owns forward pricing provenance
// (the locked rate and snapshot version are untouched); LG-312
// owns the hold horizon cap (the same duration-knob shape, a
// different object); LG-335 owns the webhook endpoint signing-key
// grace period (the same word "grace", a different object —
// retired-key verification, not forward settlement); LG-287 owns
// the rate-override TTL (a per-quote parameter, not a
// ledger-level grace); LG-304 owns the webhook endpoint delivery
// policy (outbound delivery, not forward settlement). There is no
// HTTP endpoint for the revision (along LG-241/242 and the
// forward_roll.go family: forward config is in-process), no new
// env knob, and no per-contract grace (a per-contract grace is a
// booking term, a separate item if operators ever want it).

var (
	// ErrInvalidForwardSettlementGracePeriod is returned by
	// SetForwardSettlementGracePeriod when the requested grace is
	// not positive. The live grace is left untouched (the returned
	// value is the live grace in that case).
	ErrInvalidForwardSettlementGracePeriod = errors.New("ledger: forward settlement grace period must be positive")
)

// SetForwardSettlementGracePeriod revises the live FX forward
// settlement grace period in place and returns the pre-revision
// grace (for the audit trail). A widened grace re-admits booked
// contracts that no sweep, settle, or read has yet lapsed — lazy
// expiry recomputes on every call — while a contract already
// persisted as cancelled/expired is never revived. A tightened
// grace lapses contracts sooner, on the next decision that touches
// them. A non-positive grace is rejected with
// ErrInvalidForwardSettlementGracePeriod and the live grace is left
// untouched (the returned value is the live grace in that case
// too). Revising to the current grace is a no-op with no audit
// event. The revision is audited as
// fx_forward_grace_period_retuned. A restart restores the 24h
// default.
func (l *Ledger) SetForwardSettlementGracePeriod(d time.Duration) (time.Duration, error) {
	return l.SetForwardSettlementGracePeriodCtx(context.Background(), d)
}

// SetForwardSettlementGracePeriodCtx is
// SetForwardSettlementGracePeriod with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetForwardSettlementGracePeriodCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	// Check order, along LG-344: validate first, then compare,
	// then swap, then audit — all under l.mu, which also guards
	// the field, so the decision and the swap are atomic with
	// respect to every lapse reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.forwardGracePeriod
	if d <= 0 {
		return previous, fmt.Errorf("%w: got %s", ErrInvalidForwardSettlementGracePeriod, d)
	}
	if d == previous {
		return previous, nil
	}
	l.forwardGracePeriod = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_forward_grace_period_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_grace_period": previous.String(),
			"current_grace_period":  d.String(),
		},
	})
	return previous, nil
}

// ForwardSettlementGracePeriod reports the live FX forward
// settlement grace period: how long after SettleAt a booked
// forward may still settle before it is considered lapsed. The
// name is shared with the package-level constant (forward.go),
// which is the construction-time default (24h) and the seed —
// the method reads the live value.
func (l *Ledger) ForwardSettlementGracePeriod() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.forwardGracePeriod
}
