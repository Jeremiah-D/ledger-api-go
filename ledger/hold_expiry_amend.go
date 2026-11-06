package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Hold expiry amendment (LG-339).
//
// ExtendHoldExpiry (LG-228) lengthens a hold's deadline in place;
// this module is the orthogonal absolute-set path: an operator sets
// a hold's deadline to an exact instant — shortening it, lengthening
// it, or re-stating it — instead of releasing the hold and placing a
// new one. Amending in place matters because a release-and-replace
// is two reservations — a gap in which the funds are available, plus
// a fresh hold identity that breaks the capture story — while an
// amendment keeps the same hold, the same reservation, and the same
// audit trail from hold to capture.
//
// Semantics, deliberately fail-closed throughout, along the
// ExtendHoldExpiry / AmendHoldAmount state gates:
//
//   - Only an active, unexpired hold may be amended. An unknown
//     ID fails with ErrHoldNotFound; a captured or released hold
//     fails with ErrHoldNotActive; a hold whose expiry has
//     already passed — swept to expired or merely lapsed while
//     nobody was watching — fails with ErrHoldExpired, the same
//     lazy-expiry reading Capture uses. An expired hold is
//     never resurrected by an amendment: the operator places a
//     new hold instead.
//   - The new expiry must be in the future and within the live
//     maximum-hold-duration cap: a past or present instant fails
//     with ErrHoldExpiryAmendInvalid, and an instant strictly
//     after now + the live cap fails with
//     ErrHoldExceedsMaxDuration — the same cap Hold applies to a
//     new hold (see holdLockedCapped, LG-312), so an amendment
//     can never install a deadline the hold could not have been
//     created with. Either failure leaves the original expiry
//     exactly as it was. Unlike ExtendHoldExpiry, the new
//     deadline need not be later than the current one —
//     shortening is the point.
//   - The amendment touches the expiry and nothing else: the
//     account, currency, amount, idempotency key, creation
//     instant, and captured position are unchanged, and a
//     partially captured hold stays active with its remainder
//     still reserving funds. The amendment lands one audit event
//     (hold_expiry_amended, with the hold ID, the previous and
//     new expiries, the amount, and the currency) so the "who
//     moved the deadline, and from what to what" trail is
//     tamper-evident. Like Hold and Release, it bumps neither
//     the ledger version nor the audit chain: holds are
//     off-journal reservations, and moving their deadline moves
//     no money. The amended expiry rides the hold record through
//     snapshots exactly like the original — holds are snapshot
//     records, so no snapshot change is needed either. The
//     ExpireHolds sweep reads the live ExpiresAt on every tick,
//     so a shortened hold expires sooner and a lengthened one
//     survives past its old deadline with no sweep change.
//
// Orthogonality: Hold owns the initial reservation, Capture
// and CapturePartial own settlement, Release owns dropping a
// hold, ExpireHolds owns the expiry sweep, ExtendHoldExpiry
// (LG-228) owns the lengthen-only pre-expiry path,
// AmendHoldAmount (LG-240) owns the amount, and this module
// owns only the absolute deadline revision.

// Hold expiry amendment errors.
var (
	// ErrHoldExpiryAmendInvalid is returned by AmendHoldExpiry
	// when the new expiry is not in the future: a past deadline
	// would expire the hold on the next sweep, and an amendment
	// must never silently kill a live reservation.
	ErrHoldExpiryAmendInvalid = errors.New("ledger: hold amended expiry must be in the future")
)

// AmendHoldExpiry sets an active hold's ExpiresAt to newExpiresAt
// in place. The new expiry must be in the future and within the
// live maximum-hold-duration cap; a failed amendment leaves the
// hold exactly as it was. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// hold_expiry_amended.
func (l *Ledger) AmendHoldExpiry(holdID string, newExpiresAt time.Time) (Hold, error) {
	return l.AmendHoldExpiryCtx(context.Background(), holdID, newExpiresAt)
}

// AmendHoldExpiryCtx is AmendHoldExpiry with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendHoldExpiryCtx(ctx context.Context, holdID string, newExpiresAt time.Time) (Hold, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	h, ok := l.holds[holdID]
	if !ok {
		return Hold{}, ErrHoldNotFound
	}
	if h.Status == HoldStatusCaptured || h.Status == HoldStatusReleased {
		return Hold{}, ErrHoldNotActive
	}
	if h.Status == HoldStatusExpired {
		return Hold{}, ErrHoldExpired
	}
	now := time.Now()
	if !now.Before(h.ExpiresAt) {
		return Hold{}, ErrHoldExpired
	}
	if !newExpiresAt.After(now) {
		return Hold{}, fmt.Errorf("%w: got %v", ErrHoldExpiryAmendInvalid, newExpiresAt.UTC().Format(time.RFC3339))
	}
	// The live maximum-hold-duration cap (LG-312), reused from
	// hold creation: a deadline the hold could not have been
	// created with cannot be amended onto it either. The
	// boundary is inclusive, exactly like creation — an expiry
	// exactly at now + the cap is admitted.
	if l.maxHoldDuration > 0 && newExpiresAt.After(now.Add(l.maxHoldDuration)) {
		return Hold{}, ErrHoldExceedsMaxDuration
	}
	previous := h.ExpiresAt
	h.ExpiresAt = newExpiresAt
	l.holds[holdID] = h
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "hold_expiry_amended",
		Actor:         "AmendHoldExpiry",
		TraceID:       h.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"hold_id":             h.ID,
			"amount_cents":        h.AmountCents,
			"currency":            h.Currency,
			"previous_expires_at": previous.UTC().Format(time.RFC3339),
			"expires_at":          newExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return h, nil
}
