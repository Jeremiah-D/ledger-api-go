package ledger

import (
	"context"
	"time"
)

// Review expiry amendment (LG-288).
//
// WithReviewExpiry installs the pending-review lifetime at
// construction; this module is the orthogonal runtime path: an
// operator revises the lifetime in place, instead of rebuilding
// the ledger and replaying its journal. Revising in place matters
// because a rebuild is a new ledger — a gap in which the old
// lifetime (or no lifetime) governs reviews, plus a fresh identity
// that breaks the audit story — while an amendment keeps the same
// ledger, the same journal, and the same audit trail from old
// lifetime to new.
//
// Semantics, deliberately minimal throughout:
//
//   - The new lifetime takes effect immediately for every later
//     review, because review creation stamps ExpiresAt from the
//     live field (review.go: `if l.reviewExpiry > 0` at creation
//     time), never a cached copy. No creation, sweep, or replay
//     path needed a code change — the revision is clean exactly
//     because the reader was already live.
//   - Already-pending reviews keep the ExpiresAt stamped at their
//     creation: the amendment touches only the lifetime field,
//     never any review record, so a review created under a longer
//     lifetime is not re-dated when the lifetime is shortened, and
//     lengthening it does not rescue a review already lapsed.
//   - A non-positive lifetime disables auto-expiry, exactly as at
//     construction: creation leaves ExpiresAt zero and reviews
//     wait indefinitely. There is no invalid lifetime — every
//     duration installs — so the setter returns nothing and
//     cannot fail.
//   - Revising to the current lifetime is a no-op: no state
//     change, no audit event.
//   - The amendment is structural, like SetDailyLimit: it moves
//     no money, so it bumps neither the ledger version nor the
//     audit chain, but it lands one review_expiry_amended audit
//     event carrying the previous and new lifetimes. The amended
//     lifetime rides the config line (review_expiry) through
//     snapshots exactly like a construction-time lifetime — no
//     snapshot change is needed.
//
// Orthogonality: WithReviewExpiry owns the construction-time
// lifetime, ExpireReviews owns the explicit sweep, the background
// ReviewSweeper owns amortized expiry, and this module owns only
// the runtime revision of the lifetime itself.

// SetReviewExpiry replaces the pending-review lifetime in place.
// The new lifetime governs every review created after the call;
// already-pending reviews keep the ExpiresAt stamped at their
// creation. A non-positive lifetime disables auto-expiry.
// Revising to the current lifetime is a no-op. The revision is
// audited as review_expiry_amended.
func (l *Ledger) SetReviewExpiry(d time.Duration) {
	l.SetReviewExpiryCtx(context.Background(), d)
}

// SetReviewExpiryCtx is SetReviewExpiry with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetReviewExpiryCtx(ctx context.Context, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.reviewExpiry
	if previous == d {
		return
	}
	l.reviewExpiry = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "review_expiry_amended",
		Actor:         "SetReviewExpiry",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_ttl": previous.String(),
			"ttl":          d.String(),
		},
	})
}
