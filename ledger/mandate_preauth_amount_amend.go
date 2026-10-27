package ledger

import (
	"context"
	"fmt"
	"time"
)

// Pre-authorization amount amendment (LG-235).
//
// AuthorizePreAuth (LG-192) reserves a slice of a mandate's pull
// limit; ExtendPreAuthExpiry (LG-233) moves that reservation's
// deadline. This module is the orthogonal in-place amount path:
// an operator revises the reserved amount before the reservation
// settles, instead of releasing and re-authorizing. Amending in
// place matters because a re-authorization is a new reservation —
// it re-runs the mandate gates and the frequency-window check, so
// it can fail where the original authorization succeeded (the
// window may now be occupied by a posted debit), and it breaks the
// audit continuity from the original authorization — while an
// amendment keeps the same reservation, the same window occupancy,
// and one audit trail from authorization to capture, release, or
// expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active, unexpired pre-authorization may be amended.
//     The lookup follows the LG-233 convention: an unknown
//     pre-authorization, or one under a different mandate than
//     the route's, fails with ErrPreAuthNotFound; a captured or
//     released pre-authorization fails with ErrPreAuthNotActive —
//     a terminal reservation has no amount left to revise — and
//     an expired one (stored expired, or active but past
//     ExpiresAt under the lazy expiry predicate) fails with
//     ErrPreAuthExpired. A lapsed-but-unswept pre-authorization
//     is NOT still live: expiry is lazy by predicate, so a
//     pre-authorization past ExpiresAt already reads as expired
//     everywhere and reserves nothing.
//   - The new amount must be positive and must fit the mandate's
//     pull cap: zero or negative fails with
//     ErrPreAuthNonPositiveAmount, and an amount above the
//     mandate's MaxAmountCents (when set) fails with
//     ErrMandateAmountExceeded — the same two gates
//     AuthorizePreAuth applies to the original reservation, so
//     an amendment can never smuggle in a reservation the
//     mandate would not have authorized in the first place.
//     A failed amendment leaves the original amount exactly as
//     it was.
//   - The amendment touches the amount and nothing else: the
//     currency, the expiry, the mandate link, the status, and
//     the idempotency key are unchanged, the ledger version is
//     not bumped, and no outbox event fires (the reservation
//     was already reported when authorized). The amendment
//     lands one audit event (preauth_amount_amended, with the
//     previous and new amounts) so the "who moved the amount,
//     and from what to what" trail is tamper-evident.
//   - CapturePreAuth settles the live amount (see
//     mandate_preauth.go), so no settlement change is needed:
//     an amended pre-authorization captures at its new amount.
//     Pre-authorizations stay out of snapshots (the LG-192
//     convention): an amendment, like the authorization itself,
//     is in-memory operational state and never round-trips.
//
// Orthogonality: AuthorizePreAuth owns the initial reservation
// (and its amount), ExtendPreAuthExpiry owns the deadline,
// CapturePreAuth owns settlement, ReleasePreAuth owns dropping
// the reservation, ExpirePreAuths owns the sweep, and this module
// owns only the pre-settlement amount revision. It is orthogonal
// to the FX limit-order price amendment (LG-236), which revises
// an order-book trigger price rather than a mandate-limit
// reservation.

// AmendPreAuthAmount moves an active, unexpired pre-authorization's
// AmountCents to newAmountCents and returns the updated
// pre-authorization. The currency, expiry, mandate link, status,
// and idempotency key are unchanged, and a failed amendment leaves
// the original amount exactly as it was. Amending is off-journal —
// it does not bump the ledger version — but it is audited as
// preauth_amount_amended.
func (l *Ledger) AmendPreAuthAmount(mandateID, id string, newAmountCents int64) (PreAuth, error) {
	return l.AmendPreAuthAmountCtx(context.Background(), mandateID, id, newAmountCents)
}

// AmendPreAuthAmountCtx is AmendPreAuthAmount with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendPreAuthAmountCtx(ctx context.Context, mandateID, id string, newAmountCents int64) (PreAuth, error) {
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
	if newAmountCents <= 0 {
		return PreAuth{}, ErrPreAuthNonPositiveAmount
	}
	m, mandateOK := l.mandates[p.MandateID]
	if mandateOK && m.MaxAmountCents > 0 && newAmountCents > m.MaxAmountCents {
		return PreAuth{}, fmt.Errorf("%w: %d > %d", ErrMandateAmountExceeded, newAmountCents, m.MaxAmountCents)
	}
	previous := p.AmountCents
	p.AmountCents = newAmountCents
	l.preauths[id] = p
	var accounts []AccountID
	if mandateOK {
		accounts = []AccountID{m.Payer, m.Payee}
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "preauth_amount_amended",
		Actor:         "AmendPreAuthAmount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"mandate_id":            p.MandateID,
			"previous_amount_cents": previous,
			"amount_cents":          newAmountCents,
			"currency":              p.Currency,
		},
	})
	return p, nil
}
