package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pre-authorization expiry amendment (LG-370).
//
// ExtendPreAuthExpiry (LG-233) lengthens a pre-authorization's
// deadline in place — strictly later, and only later; this module is
// the orthogonal absolute-set path: an operator sets a
// pre-authorization's deadline to an exact instant — shortening it,
// lengthening it, or re-stating it — instead of releasing the
// pre-authorization and authorizing a new one. Amending in place
// matters because a release-and-re-authorize is a new reservation —
// it re-runs the mandate gates and the frequency-window check, so it
// can fail where the original authorization succeeded (the window
// may now be occupied by a posted debit), and it breaks the audit
// continuity from the original authorization — while an amendment
// keeps the same reservation, the same window occupancy, and one
// audit trail from authorization to capture, release, or expiry.
//
// Semantics, deliberately fail-closed throughout, along
// AmendHoldExpiry (LG-339) against ExtendHoldExpiry (LG-228):
//
//   - Only an active, unexpired pre-authorization may be amended.
//     The lookup follows the LG-233 convention: an unknown
//     pre-authorization, or one under a different mandate than
//     the route's, fails with ErrPreAuthNotFound; a captured or
//     released pre-authorization fails with ErrPreAuthNotActive —
//     a terminal reservation has no deadline left to move — and
//     an expired one (stored expired, or active but past
//     ExpiresAt under the lazy expiry predicate) fails with
//     ErrPreAuthExpired. Unlike the freeze extensions
//     (LG-229/LG-230), a lapsed-but-unswept pre-authorization is
//     NOT still live: expiry is lazy by predicate, so a
//     pre-authorization past ExpiresAt already reads as expired
//     everywhere and reserves nothing — there is no live stop to
//     move, only a dead reservation to re-authorize. An expired
//     pre-authorization is never resurrected by an amendment.
//   - The new expiry must be in the future: a past or present
//     instant fails with ErrPreAuthExpiryAmendInvalid and the
//     original deadline is untouched, so a mistaken call cannot
//     silently kill a live reservation. Unlike
//     ExtendPreAuthExpiry, the new deadline need not be later
//     than the current one — shortening is the point. There is
//     no maximum-hold-duration cap to reuse here (pre-authori-
//     zations carry no LG-312-style cap), so the future gate is
//     the only bound.
//   - Amending to the current expiry is a no-op: it returns the
//     pre-authorization unchanged and writes no audit event,
//     like AmendPreAuthCurrency.
//   - The amendment touches the expiry and nothing else: the
//     amount, the currency, the mandate link, the status, and
//     the idempotency key are unchanged, the ledger version is
//     not bumped, and no outbox event fires (the reservation
//     was already reported when authorized). The amendment lands
//     one audit event (preauth_expiry_amended, with the previous
//     and new deadlines, the amount, and the currency) so the
//     "who moved the deadline, and from what to what" trail is
//     tamper-evident.
//   - The lazy expiry predicate and the ExpirePreAuthsAt sweep
//     read the live deadline (see mandate_preauth.go), so no
//     sweep change is needed: a shortened pre-authorization
//     expires sooner and a lengthened one survives past its old
//     deadline. Pre-authorizations stay out of snapshots (the
//     LG-192 convention): an amendment, like the authorization
//     itself, is in-memory operational state and never
//     round-trips.
//
// Orthogonality: AuthorizePreAuth owns the initial reservation
// (and its expiry), ExtendPreAuthExpiry (LG-233) owns the
// lengthen-only pre-expiry path, AmendPreAuthAmount (LG-235)
// owns the amount, AmendPreAuthCurrency (LG-368) owns the
// currency, CapturePreAuth owns settlement, ReleasePreAuth owns
// dropping the reservation, ExpirePreAuths owns the sweep, and
// this module owns only the absolute deadline revision. It is
// orthogonal to AmendHoldExpiry (LG-339), which revises a hold's
// deadline (a balance reservation, not a mandate-limit
// reservation).

// Pre-authorization expiry amendment errors.
var (
	// ErrPreAuthExpiryAmendInvalid is returned by
	// AmendPreAuthExpiry when the new expiry is not in the
	// future: a past deadline would expire the
	// pre-authorization on the next sweep, and an amendment
	// must never silently kill a live reservation.
	ErrPreAuthExpiryAmendInvalid = errors.New("ledger: pre-authorization amended expiry must be in the future")
)

// AmendPreAuthExpiry sets an active, unexpired pre-authorization's
// ExpiresAt to newExpiresAt in place. The new expiry must be in the
// future; amending to the current expiry is a no-op, and a failed
// amendment leaves the original deadline exactly as it was.
// Amending is off-journal — it does not bump the ledger version —
// but a real revision is audited as preauth_expiry_amended.
func (l *Ledger) AmendPreAuthExpiry(mandateID, id string, newExpiresAt time.Time) (PreAuth, error) {
	return l.AmendPreAuthExpiryCtx(context.Background(), mandateID, id, newExpiresAt)
}

// AmendPreAuthExpiryCtx is AmendPreAuthExpiry with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendPreAuthExpiryCtx(ctx context.Context, mandateID, id string, newExpiresAt time.Time) (PreAuth, error) {
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
	if !newExpiresAt.After(now) {
		return PreAuth{}, fmt.Errorf("%w: got %v", ErrPreAuthExpiryAmendInvalid, newExpiresAt.UTC().Format(time.RFC3339))
	}
	if newExpiresAt.Equal(p.ExpiresAt) {
		return p, nil
	}
	previous := p.ExpiresAt
	p.ExpiresAt = newExpiresAt
	l.preauths[id] = p
	var accounts []AccountID
	if m, mandateOK := l.mandates[p.MandateID]; mandateOK {
		accounts = []AccountID{m.Payer, m.Payee}
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "preauth_expiry_amended",
		Actor:         "AmendPreAuthExpiry",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"mandate_id":          p.MandateID,
			"previous_expires_at": previous.UTC().Format(time.RFC3339),
			"expires_at":          newExpiresAt.UTC().Format(time.RFC3339),
			"amount_cents":        p.AmountCents,
			"currency":            p.Currency,
		},
	})
	return p, nil
}
