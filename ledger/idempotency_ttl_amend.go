package ledger

import (
	"context"
	"time"
)

// Idempotency TTL amendment (LG-242).
//
// WithIdempotencyTTL (LG-04) installs the key-retention TTL at
// construction; this module is the orthogonal runtime path: an
// operator revises the TTL in place, instead of rebuilding the
// ledger and replaying its journal. Revising in place matters
// because a rebuild is a new ledger — a gap in which the old TTL
// (or no TTL) governs retries, plus a fresh identity that breaks
// the audit story — while an amendment keeps the same ledger, the
// same journal, and the same audit trail from old TTL to new.
//
// Semantics, deliberately minimal throughout:
//
//   - The new TTL takes effect immediately for every later
//     expiry decision, because both expiry paths read the live
//     field, never a cached copy: ExpireIdempotencyKeys sweeps
//     against l.idempotencyTTL at call time, and Post's lazy
//     sweep (maybePruneIdempotencyKeys, pre-insert) prunes
//     against the same live field. No sweep, prune, or replay
//     path needed a code change — the revision is clean exactly
//     because the readers were already live. (The lazy sweep
//     stays interval-guarded, as before: an operator who needs
//     an immediate sweep calls ExpireIdempotencyKeys, which is
//     never throttled.)
//   - A non-positive TTL disables expiry, exactly as at
//     construction (LG-04): ExpireIdempotencyKeys becomes a
//     no-op returning 0, and the lazy sweep stops pruning. There
//     is no invalid TTL — every duration installs — so the
//     setter returns nothing and cannot fail.
//   - Evicted keys are never resurrected: the amendment touches
//     only the TTL field, never the byKey index, so a key swept
//     under an old (shorter) TTL stays gone when the TTL is
//     lengthened or disabled, and reposting it still books a
//     brand-new entry. Lengthening a TTL only protects keys
//     that have not yet been swept.
//   - The amendment is structural, like SetDailyLimit: it moves
//     no money, so it bumps neither the ledger version nor the
//     audit chain, but it lands one idempotency_ttl_amended
//     audit event carrying the previous and new TTLs. The
//     amended TTL rides the config line (idempotency_ttl)
//     through snapshots exactly like a construction-time TTL —
//     no snapshot change is needed.
//
// Orthogonality: WithIdempotencyTTL owns the construction-time
// TTL, ExpireIdempotencyKeys owns the explicit sweep, Post's
// lazy sweep owns amortized pruning, and this module owns only
// the runtime revision of the TTL itself.

// SetIdempotencyTTL replaces the idempotency-key retention TTL in
// place. The new TTL governs every later ExpireIdempotencyKeys
// sweep and Post lazy sweep. A non-positive TTL disables expiry
// (LG-04 semantics). Evicted keys are not restored. The revision
// is audited as idempotency_ttl_amended.
func (l *Ledger) SetIdempotencyTTL(ttl time.Duration) {
	l.SetIdempotencyTTLCtx(context.Background(), ttl)
}

// SetIdempotencyTTLCtx is SetIdempotencyTTL with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetIdempotencyTTLCtx(ctx context.Context, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.idempotencyTTL
	l.idempotencyTTL = ttl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "idempotency_ttl_amended",
		Actor:         "SetIdempotencyTTL",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ttl": previous.String(),
			"ttl":          ttl.String(),
		},
	})
}
