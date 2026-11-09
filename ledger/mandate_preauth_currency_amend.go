package ledger

import (
	"context"
	"fmt"
	"time"
)

// Pre-authorization currency amendment (LG-368).
//
// AuthorizePreAuth (LG-192) pins a pre-authorization's currency at
// creation: the code normalizes via normalizeCurrency and the
// currency must equal the mandate's (mandate_preauth.go:311), so a
// reservation is born speaking its mandate's settlement currency.
// AmendMandateCurrency (LG-367) can move the mandate's currency
// afterwards — a merchant that changed its settlement currency, an
// old currency retired — and when it does, every pre-authorization
// authorized under the old currency is left behind: the reservation
// still names the old currency, and the next capture would fail the
// mandate-currency gate (debitMandateCoreLockedExcluding re-runs it
// at capture time). This module is the orthogonal in-place currency
// path for the reservation itself: an operator revises the
// pre-authorization's currency to the mandate's live currency
// instead of releasing and re-authorizing. Amending in place matters
// because a re-authorization is a new reservation — it re-runs the
// mandate gates and the frequency-window check, so it can fail where
// the original authorization succeeded (the window may now be
// occupied by a posted debit), and it breaks the audit continuity
// from the original authorization — while an amendment keeps the
// same reservation, the same window occupancy, and one audit trail
// from authorization to capture, release, or expiry.
//
// Semantics, deliberately fail-closed throughout, along
// AmendPreAuthAmount (LG-235) and AmendMandateCurrency (LG-367):
//
//   - Only an active, unexpired pre-authorization may be amended.
//     The lookup follows the LG-233 convention: an unknown
//     pre-authorization, or one under a different mandate than
//     the route's, fails with ErrPreAuthNotFound; a captured or
//     released pre-authorization fails with ErrPreAuthNotActive —
//     a terminal reservation has no currency left to revise — and
//     an expired one (stored expired, or active but past
//     ExpiresAt under the lazy expiry predicate) fails with
//     ErrPreAuthExpired.
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
//   - The new currency must equal the mandate's live currency,
//     the same gate AuthorizePreAuth applies at creation (the
//     mandate's currency is the settlement currency, and a
//     capture books the reservation's currency through the
//     mandate debit path). A mismatch fails with
//     ErrMandateCurrencyMismatch, so an amendment can never
//     install a currency the mandate would not authorize a
//     capture in. This is what makes the LG-367 flow work: the
//     mandate moves first, then each of its pre-authorizations
//     is brought in line with the mandate's new currency.
//   - Amending to the current currency (after normalization) is
//     a no-op: it returns the pre-authorization unchanged and
//     writes no audit event, like AmendMandateCurrency.
//   - The amendment touches the currency and nothing else: the
//     amount, the expiry, the mandate link, the status, and the
//     idempotency key are unchanged, the ledger version is not
//     bumped, and no outbox event fires (the reservation was
//     already reported when authorized). The amendment lands one
//     audit event (preauth_currency_amended, with the previous
//     and new currencies and the amount) so the "who moved the
//     settlement currency, and from what to what" trail is
//     tamper-evident. CapturePreAuth settles the live currency
//     (see mandate_preauth.go: the debit is built from
//     p.Currency), so no settlement change is needed: an amended
//     pre-authorization captures in its new currency.
//   - Pre-authorizations stay out of snapshots (the LG-192
//     convention, unlike holds and mandates): an amendment, like
//     the authorization itself, is in-memory operational state
//     and never round-trips — so there is no snapshot test here,
//     only the live-capture proof.
//
// This is not FX conversion: no rate is applied, no amount is
// recomputed, and no posted entry is restated. The revision only
// changes which currency the next capture must be booked in.
//
// Orthogonality: AuthorizePreAuth owns the initial reservation
// (and its currency), ExtendPreAuthExpiry owns the deadline,
// AmendPreAuthAmount (LG-235) owns the amount,
// CapturePreAuth owns settlement, ReleasePreAuth owns dropping
// the reservation, ExpirePreAuths owns the sweep, and this module
// owns only the pre-settlement currency revision. It is orthogonal
// to AmendMandateCurrency (LG-367), which revises the mandate's
// currency (the one this amendment must match), and to
// AmendHoldCurrency (LG-369), which revises a hold's currency
// (a balance reservation, not a mandate-limit reservation).

// AmendPreAuthCurrency moves an active, unexpired
// pre-authorization's Currency to newCurrency and returns the
// updated pre-authorization. The new currency must be a non-empty
// three-letter uppercase ISO 4217 code and must equal the mandate's
// live currency; amending to the current currency is a no-op, and a
// failed amendment leaves the original currency exactly as it was.
// Amending is off-journal — it does not bump the ledger version —
// but a real revision is audited as preauth_currency_amended.
func (l *Ledger) AmendPreAuthCurrency(mandateID, id string, newCurrency string) (PreAuth, error) {
	return l.AmendPreAuthCurrencyCtx(context.Background(), mandateID, id, newCurrency)
}

// AmendPreAuthCurrencyCtx is AmendPreAuthCurrency with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendPreAuthCurrencyCtx(ctx context.Context, mandateID, id string, newCurrency string) (PreAuth, error) {
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
	if newCurrency == "" {
		return PreAuth{}, ErrInvalidCurrency
	}
	normalized, err := normalizeCurrency(newCurrency)
	if err != nil {
		return PreAuth{}, err
	}
	if m, mandateOK := l.mandates[p.MandateID]; mandateOK && normalized != m.Currency {
		return PreAuth{}, fmt.Errorf("%w: %q != %q", ErrMandateCurrencyMismatch, normalized, m.Currency)
	}
	if normalized == p.Currency {
		return p, nil
	}
	previous := p.Currency
	p.Currency = normalized
	l.preauths[id] = p
	var accounts []AccountID
	if m, mandateOK := l.mandates[p.MandateID]; mandateOK {
		accounts = []AccountID{m.Payer, m.Payee}
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "preauth_currency_amended",
		Actor:         "AmendPreAuthCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"mandate_id":        p.MandateID,
			"previous_currency": previous,
			"currency":          normalized,
			"amount_cents":      p.AmountCents,
		},
	})
	return p, nil
}
