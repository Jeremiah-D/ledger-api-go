package ledger

import (
	"errors"
	"time"
)

// Pre-authorization expiry extension (LG-233).
//
// AuthorizePreAuth (LG-192) stamps a time-bound reservation on a
// mandate's pull limit; this module is the orthogonal pre-expiry
// path: an operator extends that reservation's deadline before it
// lapses, instead of letting the pre-authorization expire and
// re-authorizing. Extending in place matters because a
// re-authorization is a new reservation — it re-runs the mandate
// gates and the frequency-window check, so it can fail where the
// original authorization succeeded (the window may now be occupied
// by a posted debit), and it breaks the audit continuity from the
// original authorization — while an extension keeps the same
// reservation, the same window occupancy, and one audit trail from
// authorization to capture, release, or expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active, unexpired pre-authorization may be extended.
//     An unknown pre-authorization, or one under a different mandate
//     than the route's, fails with ErrPreAuthNotFound (the
//     CapturePreAuth/ReleasePreAuth lookup convention). A captured
//     or released pre-authorization fails with ErrPreAuthNotActive
//     — a terminal reservation has no deadline left to move — and
//     an expired one (stored expired, or active but past ExpiresAt
//     under the lazy expiry predicate) fails with ErrPreAuthExpired.
//     Unlike the freeze extensions (LG-229/LG-230), a lapsed-but-
//     unswept pre-authorization is NOT still live: expiry is lazy
//     by predicate, so a pre-authorization past ExpiresAt already
//     reads as expired everywhere and reserves nothing — there is
//     no live stop to lengthen, only a dead reservation to
//     re-authorize.
//   - The new deadline must be strictly later than the current one
//     and in the future: extensions only lengthen, never shorten.
//     An equal, earlier, or past new deadline fails with
//     ErrPreAuthExpiryExtensionNotLater and the original deadline
//     is untouched, so a mistaken call cannot silently cut an
//     authorization short.
//   - The extension touches the deadline and nothing else: the
//     reserved amount, the currency, the mandate link, and the
//     status are unchanged, the ledger version is not bumped,
//     and no outbox event fires (the reservation was already
//     reported when authorized). The extension lands one audit
//     event (preauth_expiry_extended, with the previous and new
//     deadlines) so the "who moved the expiry, and from what to
//     what" trail is tamper-evident.
//   - The lazy expiry predicate and the ExpirePreAuthsAt sweep
//     read the live deadline (see mandate_preauth.go), so no sweep
//     change is needed: an extended pre-authorization survives
//     sweeps up to its new deadline and expires after it.
//     Pre-authorizations stay out of snapshots (the LG-192
//     convention): an extension, like the authorization itself,
//     is in-memory operational state and never round-trips.
//
// Orthogonality: AuthorizePreAuth owns the initial reservation
// (and its expiry), CapturePreAuth owns settlement, ReleasePreAuth
// owns dropping the reservation, ExpirePreAuths owns the sweep,
// and this module owns only the pre-expiry lengthening. It is
// orthogonal to the freeze expiry extensions (LG-229/LG-230),
// which move whole-account stops and partial-freeze reservations
// rather than mandate-limit reservations.

// Pre-authorization expiry extension errors.
var (
	// ErrPreAuthExpiryExtensionNotLater is returned by
	// ExtendPreAuthExpiry when the new deadline is not strictly
	// later than the current expiry, or is not in the future.
	ErrPreAuthExpiryExtensionNotLater = errors.New("ledger: pre-authorization expiry extension must be later than current expiry and in the future")
)

// ExtendPreAuthExpiry moves an active, unexpired pre-authorization's
// ExpiresAt to newExpiresAt and returns the updated pre-authorization.
// The reserved amount, currency, mandate link, and status are
// unchanged, and a failed extension leaves the original deadline
// exactly as it was. Extending is off-journal — it does not bump the
// ledger version — but it is audited as preauth_expiry_extended.
func (l *Ledger) ExtendPreAuthExpiry(mandateID, id string, newExpiresAt time.Time) (PreAuth, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.preauths[id]
	if !ok || p.MandateID != mandateID {
		return PreAuth{}, ErrPreAuthNotFound
	}
	if p.Status == PreAuthStatusCaptured || p.Status == PreAuthStatusReleased {
		return PreAuth{}, ErrPreAuthNotActive
	}
	now := time.Now()
	if p.Status == PreAuthStatusExpired || preauthExpiredLocked(p, now) {
		return PreAuth{}, ErrPreAuthExpired
	}
	if !newExpiresAt.After(now) || !newExpiresAt.After(p.ExpiresAt) {
		return PreAuth{}, ErrPreAuthExpiryExtensionNotLater
	}
	previous := p.ExpiresAt
	p.ExpiresAt = newExpiresAt
	l.preauths[id] = p
	m := l.mandates[p.MandateID]
	l.emitAudit(AuditEvent{
		Op:            "preauth_expiry_extended",
		Actor:         "ExtendPreAuthExpiry",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":          p.MandateID,
			"amount_cents":        p.AmountCents,
			"currency":            p.Currency,
			"previous_expires_at": previous.UTC().Format(time.RFC3339),
			"expires_at":          newExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return p, nil
}
