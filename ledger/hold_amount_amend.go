package ledger

import (
	"context"
	"errors"
	"time"
)

// Hold amount amendment (LG-240).
//
// ExtendHoldExpiry (LG-228) moves a hold's deadline in place; this
// module is the orthogonal amount path: an operator resizes a hold's
// reservation in place, instead of releasing it and placing a new
// one. Amending in place matters because a release-and-replace is
// two reservations — a gap in which the funds are available, plus a
// fresh hold identity that breaks the capture story — while an
// amendment keeps the same hold, the same reservation, and the
// same audit trail from hold to capture.
//
// Semantics, deliberately fail-closed throughout, along the
// LG-228 state gates:
//
//   - Only an active, unexpired hold may be amended. An unknown
//     ID fails with ErrHoldNotFound; a captured or released hold
//     fails with ErrHoldNotActive; a hold whose expiry has
//     already passed — swept to expired or merely lapsed while
//     nobody was watching — fails with ErrHoldExpired, the same
//     lazy-expiry reading Capture uses. An expired hold is
//     never resurrected by an amendment: the operator places a
//     new hold instead.
//   - The new amount must be positive and must not undercut what
//     has already settled: a non-positive amount fails with
//     ErrHoldNonPositiveAmount, and an amount below
//     CapturedCents fails with ErrHoldAmountBelowCaptured —
//     the settled part is immutable, so an amendment can never
//     rewrite history that already journaled. Either failure
//     leaves the original amount exactly as it was.
//   - An increase must fit the funds not otherwise reserved:
//     the new remaining cents (new amount minus CapturedCents)
//     must not exceed the account's available funds computed
//     with this hold's current reservation replaced, not added
//     — the hold's old remaining cents are subtracted out
//     before the test, so the increase is judged on its delta,
//     never double-counted against itself. A failed increase
//     fails with ErrInsufficientAvailableFunds and the
//     original amount is untouched. A decrease is always safe
//     — it only ever frees funds — so it is never
//     coverage-gated, even if other reservations have grown
//     since the hold was placed.
//   - The amendment touches the amount and nothing else: the
//     account, currency, expiry, idempotency key, creation
//     instant, and captured position are unchanged, and a
//     partially captured hold stays active with its remainder
//     still reserving funds. CapturePartial afterwards settles
//     against the new remainder automatically (remaining =
//     new amount minus CapturedCents) — no capture-path change
//     is needed, because every capture bound already reads
//     heldRemainingLocked. The amendment lands one audit event
//     (hold_amount_amended, with the hold ID, the previous and
//     new amounts, and the captured position) so the "who
//     resized the reservation, and from what to what" trail is
//     tamper-evident. Like Hold and Release, it bumps neither
//     the ledger version nor the audit chain: holds are
//     off-journal reservations, and resizing one moves no
//     money. The amended amount rides the hold record through
//     snapshots exactly like the original — holds are snapshot
//     records, so no snapshot change is needed either.
//
// Orthogonality: Hold owns the initial reservation, Capture
// and CapturePartial own settlement, Release owns dropping a
// hold, ExpireHolds owns the expiry sweep, ExtendHoldExpiry
// (LG-228) owns the deadline, and this module owns only the
// absolute amount revision.

// Hold amount amendment errors.
var (
	// ErrHoldAmountBelowCaptured is returned by AmendHoldAmount
	// when the new amount is below the hold's already-settled
	// CapturedCents: settled cents are immutable.
	ErrHoldAmountBelowCaptured = errors.New("ledger: hold amended amount must not be below captured amount")
)

// AmendHoldAmount sets an active hold's amount to
// newAmountCents in place. The new amount must be positive and
// at least the hold's CapturedCents; an increase must fit the
// account's available funds with this hold's current reservation
// replaced, not added. A failed amendment leaves the hold
// exactly as it was. Amending is off-journal — it does not bump
// the ledger version — but it is audited as hold_amount_amended.
func (l *Ledger) AmendHoldAmount(holdID string, newAmountCents int64) (Hold, error) {
	return l.AmendHoldAmountCtx(context.Background(), holdID, newAmountCents)
}

// AmendHoldAmountCtx is AmendHoldAmount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendHoldAmountCtx(ctx context.Context, holdID string, newAmountCents int64) (Hold, error) {
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
	if newAmountCents <= 0 {
		return Hold{}, ErrHoldNonPositiveAmount
	}
	if newAmountCents < h.CapturedCents {
		return Hold{}, ErrHoldAmountBelowCaptured
	}
	if newAmountCents > h.AmountCents {
		// Coverage gates increases only: the new remaining
		// cents must fit the funds available with this hold's
		// current reservation replaced — its old remaining
		// cents are subtracted out first, so the increase is
		// never double-counted against itself.
		oldRemaining := heldRemainingLocked(h)
		newRemaining := newAmountCents - h.CapturedCents
		key := accountCurrency{account: h.Account, currency: h.Currency}
		availableExcludingThis := l.balances[key] -
			(l.heldLocked(h.Account, h.Currency, now) - oldRemaining) -
			l.reviewHeldLocked(h.Account, h.Currency, now) -
			l.termLockedLocked(h.Account, h.Currency, now) -
			l.partialFrozenLocked(h.Account, h.Currency)
		if newRemaining > availableExcludingThis {
			return Hold{}, ErrInsufficientAvailableFunds
		}
	}
	previous := h.AmountCents
	h.AmountCents = newAmountCents
	l.holds[holdID] = h
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "hold_amount_amended",
		Actor:         "AmendHoldAmount",
		TraceID:       h.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"hold_id":               h.ID,
			"previous_amount_cents": previous,
			"amount_cents":          newAmountCents,
			"captured_cents":        h.CapturedCents,
			"currency":              h.Currency,
		},
	})
	return h, nil
}
