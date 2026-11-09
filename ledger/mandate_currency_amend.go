package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate currency amendment (LG-367).
//
// CreateMandate (LG-125) fixes a mandate's currency at creation:
// the code normalizes via normalizeCurrency and is then pinned.
// DebitWithMandate validates every pull against the live currency
// (see debitWithMandateLocked: entry.Currency != m.Currency fails
// with ErrMandateCurrencyMismatch), so a wrongly-set currency —
// a merchant that changed its settlement currency, an old currency
// retired — could only be remedied by voiding the mandate and
// issuing a new one. This module is the orthogonal in-place
// currency path: an operator revises the currency on a live
// authorization instead of cancelling the mandate and issuing a
// new one. Amending in place matters because a cancel-and-replace
// is a new mandate — a fresh ID that breaks the mandate_of link
// story and the debit history — while an amendment keeps the same
// mandate, the same authorization, and one audit trail from
// creation to every debit under it.
//
// Semantics, deliberately fail-closed throughout, along
// AmendMandatePayee (LG-340) and the installment-plan currency
// amendment (AmendInstallmentPlanCurrency, LG-266):
//
//   - Only an active mandate may be amended. An unknown ID fails
//     with ErrMandateNotFound; a mandate whose ValidUntil has
//     passed — swept to expired or merely lapsed while nobody
//     was watching — fails with ErrMandateExpired, the same
//     lazy-expiry reading Debit uses; a suspended, cancelled,
//     or already-marked-expired mandate fails with
//     ErrMandateNotActive. An expired mandate is never
//     resurrected by an amendment: the operator creates a new
//     mandate instead.
//   - The new currency reuses the creation gate
//     (normalizeCurrency): anything that is not a three-letter
//     uppercase ISO 4217 code fails with ErrInvalidCurrency.
//     Unlike creation, an empty code is rejected rather than
//     normalized to the default currency: creation must give a
//     new mandate some currency, but an amendment that names no
//     currency is a malformed revision, not a request for the
//     default — silently defaulting a EUR mandate to USD would
//     be a destructive surprise. A failed amendment leaves the
//     original currency exactly as it was.
//   - Amending to the current currency (after normalization) is a
//     no-op: it returns the mandate unchanged and writes no
//     audit event, like AmendMandatePayee.
//   - The amendment touches the currency and nothing else: the
//     payer, payee, per-debit cap, frequency, validity window,
//     creation instant, and status are unchanged, and past debits
//     are untouched — entries carry their own Currency, so a
//     debit already journaled in the old currency stays booked
//     in it. DebitWithMandate reads the live Currency on every
//     pull, so the new currency governs the very next debit with
//     no debit-path change: a debit in the old currency fails
//     with ErrMandateCurrencyMismatch, a debit in the new
//     currency books normally.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the mandate was
//     already reported when created. The amendment lands one
//     audit event (mandate_currency_amended, with the mandate ID,
//     the previous and new currencies, and the payer and payee)
//     so the "who moved the settlement currency, and from what
//     to what" trail is tamper-evident. Mandates are snapshot
//     records, so the amended currency rides the mandate record
//     through snapshots exactly like the original — no snapshot
//     change is needed.
//
// This is not FX conversion: no rate is applied, no amount is
// recomputed, and no posted entry is restated. The revision only
// changes which currency future pulls must be booked in.
//
// Orthogonality: CreateMandate owns the initial currency,
// SuspendMandate and CancelMandate own the lifecycle,
// ExpireMandates owns the expiry sweep, DebitWithMandate owns
// the pulls, AmendMandatePayee (LG-340) owns the payee revision,
// AmendMandatePayer (LG-366) owns the payer revision,
// AmendMandateMaxAmount (LG-243) owns the per-debit cap,
// AmendMandateFrequency (LG-246) owns the pull cadence,
// AmendMandateValidUntil (LG-248) owns the validity end, and
// this module owns only the currency revision. PreAuth currency
// is a separate item: PreAuth.Currency is untouched by this
// amendment.

// AmendMandateCurrency sets an active mandate's Currency to
// newCurrency in place. The new currency must be a non-empty
// three-letter uppercase ISO 4217 code; amending to the current
// currency is a no-op, and a failed amendment leaves the mandate
// exactly as it was. Amending is off-journal — it does not bump
// the ledger version — but a real revision is audited as
// mandate_currency_amended.
func (l *Ledger) AmendMandateCurrency(mandateID string, newCurrency string) (Mandate, error) {
	return l.AmendMandateCurrencyCtx(context.Background(), mandateID, newCurrency)
}

// AmendMandateCurrencyCtx is AmendMandateCurrency with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendMandateCurrencyCtx(ctx context.Context, mandateID string, newCurrency string) (Mandate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	m, ok := l.mandates[mandateID]
	if !ok {
		return Mandate{}, ErrMandateNotFound
	}
	now := time.Now()
	if mandateExpiredLocked(m, now) {
		return Mandate{}, ErrMandateExpired
	}
	if m.Status != MandateStatusActive {
		return Mandate{}, fmt.Errorf("%w: status %q", ErrMandateNotActive, m.Status)
	}
	if newCurrency == "" {
		return Mandate{}, ErrInvalidCurrency
	}
	normalized, err := normalizeCurrency(newCurrency)
	if err != nil {
		return Mandate{}, err
	}
	if normalized == m.Currency {
		return m, nil
	}
	previous := m.Currency
	m.Currency = normalized
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_currency_amended",
		Actor:         "AmendMandateCurrency",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":        mandateID,
			"previous_currency": previous,
			"currency":          normalized,
			"payer":             m.Payer,
			"payee":             m.Payee,
		},
	})
	return m, nil
}
