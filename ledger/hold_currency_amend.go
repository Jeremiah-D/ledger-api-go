package ledger

import (
	"context"
	"time"
)

// Hold currency amendment (LG-369).
//
// Hold (LG-192 era) pins a hold's currency at creation: the code
// normalizes via normalizeCurrency (hold.go:262-266) and the
// currency is then nailed down — the reservation is keyed to that
// currency everywhere it matters: heldLocked sums per (account,
// currency), captures settle in h.Currency (hold.go: the settlement
// entry is built from the live currency), and the budget and monthly
// spend checks are currency-keyed too. AmendHoldAmount (LG-240)
// and AmendHoldExpiry (LG-228) already cover the amount and the
// deadline, so the currency is the hold's last field with no
// revision entry. A wrongly-set currency — the settlement currency
// changed, an old currency retired — could today only be remedied
// by releasing the hold and placing a new one. This module is the
// orthogonal in-place currency path: an operator revises the
// reservation's currency instead of releasing and re-placing.
// Amending in place matters because a release-and-replace is two
// reservations — a gap in which the funds sit unreserved, plus a
// fresh hold identity that breaks the capture story — while an
// amendment keeps the same hold, the same reservation, and the
// same audit trail from hold to capture.
//
// Semantics, deliberately fail-closed throughout, along the
// LG-228 state gates and AmendHoldAmount:
//
//   - Only an active, unexpired hold may be amended. An unknown
//     ID fails with ErrHoldNotFound; a captured or released hold
//     fails with ErrHoldNotActive; a hold whose expiry has
//     already passed — swept to expired or merely lapsed while
//     nobody was watching — fails with ErrHoldExpired, the same
//     lazy-expiry reading Capture uses. An expired hold is
//     never resurrected by an amendment: the operator places a
//     new hold instead.
//   - The new currency reuses the creation gate
//     (normalizeCurrency): anything that is not a three-letter
//     uppercase ISO 4217 code fails with ErrInvalidCurrency.
//     Unlike creation, an empty code is rejected rather than
//     normalized to the default currency, along LG-367: an
//     amendment that names no currency is a malformed revision,
//     not a request for the default — silently defaulting a EUR
//     reservation to USD would be a destructive surprise. A
//     failed amendment leaves the original currency exactly as
//     it was.
//   - Amending to the current currency (after normalization) is
//     a no-op: it returns the hold unchanged and writes no
//     audit event, like AmendHoldAmount's same-value path. The
//     no-op is checked before the coverage gate: a hold whose
//     own currency's coverage has drifted since placement (a
//     posting moved the balance after the hold) must still
//     no-op cleanly — the gate judges a move, and a no-op moves
//     nothing.
//   - The remaining reservation (AmountCents minus
//     CapturedCents) must fit the new currency's available
//     funds: balance in the new currency minus that currency's
//     other reservations — active holds, pending-review freezes,
//     term-deposit locks, and partial freezes — using the same
//     formula AmendHoldAmount uses for an increase. The hold's
//     own reservation sits in the old currency, so it is
//     naturally absent from the new currency's heldLocked sum:
//     the check judges the move against the new currency alone,
//     and the old currency's reservation is released the moment
//     the field changes. A shortfall fails with
//     ErrInsufficientAvailableFunds and the original currency is
//     untouched. Settled cents are immutable history: a
//     partially captured hold keeps its CapturedCents in the old
//     currency — only the un-settled remainder moves — and the
//     remaining is what must fit.
//   - The amendment touches the currency and nothing else: the
//     account, amount, expiry, idempotency key, creation
//     instant, and captured position are unchanged. Capture and
//     CapturePartial afterwards settle against the new remainder
//     in the new currency automatically — every capture bound
//     already reads the live currency and heldRemainingLocked —
//     so no capture-path change is needed. The amendment lands
//     one audit event (hold_currency_amended, with the hold ID,
//     the previous and new currencies, the amount, the captured
//     position, and the account) so the "who moved the
//     reservation's currency, and from what to what" trail is
//     tamper-evident. Like Hold and Release, it bumps neither
//     the ledger version nor the audit chain: holds are
//     off-journal reservations, and moving one across currencies
//     moves no money. The amended currency rides the hold record
//     through snapshots exactly like the original — holds are
//     snapshot records, so no snapshot change is needed either.
//
// This is not FX conversion: no rate is applied, no amount is
// recomputed, and no posted entry is restated. The revision only
// changes which currency the remaining reservation is held in.
//
// Orthogonality: Hold owns the initial reservation (and its
// currency), Capture and CapturePartial own settlement, Release
// owns dropping a hold, ExpireHolds owns the expiry sweep,
// ExtendHoldExpiry (LG-228) and AmendHoldExpiry own the deadline,
// AmendHoldAmount (LG-240) owns the amount, and this module owns
// only the currency revision. It is orthogonal to
// AmendPreAuthCurrency (LG-368), which revises a mandate-limit
// reservation's currency (never touching balances), and to
// AmendMandateCurrency (LG-367), which revises the mandate's
// currency (the settlement currency a pre-authorization must
// match, not a hold's).

// AmendHoldCurrency sets an active hold's currency to newCurrency
// in place. The remaining reservation (amount minus captured
// cents) must fit the new currency's available funds; an empty or
// malformed code fails with ErrInvalidCurrency, and amending to the
// current currency is a no-op. A failed amendment leaves the hold
// exactly as it was. Amending is off-journal — it does not bump
// the ledger version — but a real revision is audited as
// hold_currency_amended.
func (l *Ledger) AmendHoldCurrency(holdID string, newCurrency string) (Hold, error) {
	return l.AmendHoldCurrencyCtx(context.Background(), holdID, newCurrency)
}

// AmendHoldCurrencyCtx is AmendHoldCurrency with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendHoldCurrencyCtx(ctx context.Context, holdID string, newCurrency string) (Hold, error) {
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
	if newCurrency == "" {
		return Hold{}, ErrInvalidCurrency
	}
	normalized, err := normalizeCurrency(newCurrency)
	if err != nil {
		return Hold{}, err
	}
	if normalized == h.Currency {
		return h, nil
	}
	// Coverage gates the move: the un-settled remainder must fit
	// the new currency's available funds. The hold's own
	// reservation sits in the old currency, so heldLocked in the
	// new currency already excludes it — the move is judged
	// against the new currency alone, never double-counted
	// against itself.
	remaining := heldRemainingLocked(h)
	key := accountCurrency{account: h.Account, currency: normalized}
	availableInNew := l.balances[key] -
		l.heldLocked(h.Account, normalized, now) -
		l.reviewHeldLocked(h.Account, normalized, now) -
		l.termLockedLocked(h.Account, normalized, now) -
		l.partialFrozenLocked(h.Account, normalized)
	if remaining > availableInNew {
		return Hold{}, ErrInsufficientAvailableFunds
	}
	previous := h.Currency
	h.Currency = normalized
	l.holds[holdID] = h
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "hold_currency_amended",
		Actor:         "AmendHoldCurrency",
		TraceID:       h.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"hold_id":           h.ID,
			"previous_currency": previous,
			"currency":          normalized,
			"amount_cents":      h.AmountCents,
			"captured_cents":    h.CapturedCents,
			"account":           string(h.Account),
		},
	})
	return h, nil
}
