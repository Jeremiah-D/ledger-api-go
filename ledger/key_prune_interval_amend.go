package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Lazy key-prune interval runtime revision (LG-349).
//
// defaultKeyPruneInterval (ledger.go) fixes the minimum time
// between two lazy idempotency-key sweeps at construction —
// 1m, with no env knob, no option, and no setter — so the
// throttling valve on the Post path's amortized pruning was
// frozen for the life of the process. This module is the
// orthogonal runtime path: an operator revises the live interval
// in place, instead of changing code and redeploying.
//
// Semantics, deliberately fail-closed throughout, along LG-242
// (the same field family, the same l.mu guard, the same audit
// shape):
//
//   - The interval is a live field on the Ledger, guarded by
//     l.mu — the same lock that guards the four lazy readers,
//     so no second lock is introduced and no lock order is
//     created. The setter only swaps the field; every reader
//     already reads it live under l.mu, so no read path needed
//     a code change — the revision is clean exactly because the
//     readers were already live. The four readers share the one
//     valve: maybePruneIdempotencyKeys (ledger.go, the
//     idempotency-key table), maybePruneMonthlySpendLocked
//     (budget.go, the monthly spend buckets),
//     maybePruneLargeTxReportsLocked (ctr.go, the large-tx
//     daily reports), and maybePruneDailyOutflowLocked
//     (daily_limit.go, the daily outflow buckets). One value
//     paces all four kinds of dead-data sweeping.
//   - The revision is future-only: the new interval governs the
//     next lazy check on each path — the next Post's key sweep,
//     the next budget / large-tx / daily-limit path's bucket
//     sweep. Tightening the interval prunes sooner (if
//     lastPrune is already older than the new interval, the
//     next Post triggers a sweep at once); widening it defers
//     the next sweep. The revision itself evicts nothing and
//     resets no last-prune timestamp — it moves only the
//     interval, never the clock.
//   - Validation is fail-closed with no silent normalization:
//     the new interval must be positive, else
//     ErrInvalidKeyPruneInterval and the live interval stays
//     exactly as it was (the returned value is the live
//     interval in that case too). Zero is rejected, not a
//     "disable the throttle" state: a zero interval cancels
//     the throttle gate, so every Post would full-scan the key
//     table (the exact anti-pattern the ledger.go docs name) —
//     not a meaningful "off" state (unlike LG-242's TTL, where
//     non-positive disables expiry: the TTL governs whether
//     keys die, the interval governs how often dead keys are
//     swept, and switching the sweep throttle off has no
//     "disable sweeping" semantics). There is no upper bound:
//     an over-long interval lets expired keys and dead buckets
//     linger in memory longer, with a bigger single sweep, and
//     the operator owns that trade-off (documented in the
//     README, along LG-312/LG-347).
//   - A revision identical to the live interval is a no-op: it
//     returns the live value with a nil error and writes no
//     audit event, so a retried or duplicated revision cannot
//     spam the audit trail.
//   - The amendment is structural config, not ledger state: it
//     bumps neither the version nor the audit chain, writes no
//     journal entry, and emits no outbox event. It lands one
//     key_prune_interval_retuned audit event
//     (previous_prune_interval / current_prune_interval, as
//     duration strings, along LG-312/LG-347). The Ctx variant
//     attributes the operator (LG-73); with no operator in ctx
//     the actor defaults to DefaultAuditActor ("system").
//   - Snapshots do not carry the interval (snapshot.go has no
//     pruneInterval): a restart restores the 1m default, while
//     the revision events themselves stay in the audit trail
//     for traceability. A dry-run twin clones the live value
//     (dryrun.go), so a twin opened after a revision carries
//     the revised interval and an in-flight twin keeps the
//     value it cloned with.
//   - The TTL itself is untouched (LG-242 owns it): revising
//     the interval never changes when a key dies, only how
//     often dead keys are swept. An operator who wants an
//     immediate sweep still calls ExpireIdempotencyKeys, which
//     is never throttled.
//
// Orthogonality: LG-242 owns the idempotency-key TTL itself (a
// key is dead after the TTL; this module owns how often dead
// keys are swept — LG-242's own module comment says "the lazy
// sweep stays interval-guarded, as before", and this item is
// the interval it left guarded); LG-04 owns the construction-
// time TTL install (the initial value, not the runtime revision
// of the interval); LG-312 owns the hold horizon cap (the same
// duration-knob family, a different object); LG-310/LG-311
// own the payment-repair queue capacity/TTL (the repair queue,
// not the lazy prune throttle); LG-316..LG-322 own seven
// background sweeper scan intervals (background ticker-driven
// workers — this item is the Post-path lazy amortized prune,
// a different trigger mechanism: a background sweep vs. the
// now.Sub(lastPrune) gate on every Post); LG-329/330/331 own
// background worker intervals (worker tickers, not the lazy
// gate); LG-344 owns snapshot-backup export retries (the same
// "interval/throttle" family, a different component). There is
// no HTTP endpoint for the revision (along LG-241/242), no new
// env knob, no per-namespace prune interval (one global valve
// for all four sweeps), and no background sweep goroutine (the
// lazy amortization mechanism is unchanged).

var (
	// ErrInvalidKeyPruneInterval is returned by
	// SetKeyPruneInterval when the requested interval is not
	// positive. The live interval is left untouched (the
	// returned value is the live interval in that case).
	ErrInvalidKeyPruneInterval = errors.New("ledger: key prune interval must be positive")
)

// SetKeyPruneInterval revises the live lazy key-prune interval in
// place and returns the pre-revision interval (for the audit
// trail). The new interval governs the next lazy sweep on every
// path that shares the valve — the idempotency-key table and the
// monthly-spend, large-tx-report, and daily-outflow buckets —
// from the next Post (or budget / large-tx / daily-limit path)
// onward. Tightening prunes sooner, widening defers the next
// sweep; the revision itself evicts nothing and resets no
// last-prune timestamp. A non-positive interval is rejected with
// ErrInvalidKeyPruneInterval and the live interval is left
// untouched (the returned value is the live interval in that
// case too). Revising to the current interval is a no-op with
// no audit event. The revision is audited as
// key_prune_interval_retuned. A restart restores the 1m
// default.
func (l *Ledger) SetKeyPruneInterval(d time.Duration) (time.Duration, error) {
	return l.SetKeyPruneIntervalCtx(context.Background(), d)
}

// SetKeyPruneIntervalCtx is SetKeyPruneInterval with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetKeyPruneIntervalCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	// Check order, along LG-344: validate first, then compare,
	// then swap, then audit — all under l.mu, which also guards
	// the field, so the decision and the swap are atomic with
	// respect to every lazy reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.pruneInterval
	if d <= 0 {
		return previous, fmt.Errorf("%w: got %s", ErrInvalidKeyPruneInterval, d)
	}
	if d == previous {
		return previous, nil
	}
	l.pruneInterval = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "key_prune_interval_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_prune_interval": previous.String(),
			"current_prune_interval":  d.String(),
		},
	})
	return previous, nil
}

// KeyPruneInterval reports the live lazy key-prune interval: the
// minimum time between two lazy idempotency-key sweeps (and the
// monthly-spend, large-tx-report, and daily-outflow bucket
// sweeps that share the valve). The construction-time default is
// 1m (defaultKeyPruneInterval); a restart restores it.
func (l *Ledger) KeyPruneInterval() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pruneInterval
}
