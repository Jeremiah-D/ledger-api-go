package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate validity-end amendment (LG-248).
//
// CreateMandate (LG-125) fixes a mandate's validity window at creation;
// SuspendMandate and CancelMandate own the mandate's lifecycle, and
// AmendMandateMaxAmount (LG-243) and AmendMandateFrequency (LG-246)
// own the cap and the cadence. This module is the orthogonal in-place
// validity-end path: an operator extends how long a live authorization
// may be pulled under instead of cancelling it and issuing a new one.
// Amending in place matters because a cancel-and-replace is a new
// mandate — a fresh ID that breaks the mandate_of link story and the
// debit history — while an amendment keeps the same mandate, the same
// authorization, and one audit trail from creation to every debit
// under it.
//
// Semantics, deliberately fail-closed throughout, along the
// SuspendMandate state gates (LG-243) and the frequency amendment
// (LG-246):
//
//   - Only an active mandate may be amended. An unknown ID fails
//     with ErrMandateNotFound; a suspended, cancelled, or
//     already-marked-expired mandate fails with ErrMandateNotActive;
//     a mandate whose ValidUntil has passed — swept to expired or
//     merely lapsed while nobody was watching — fails with
//     ErrMandateExpired, the same lazy-expiry reading Debit uses.
//     An expired mandate is never resurrected by an amendment: the
//     operator creates a new mandate instead.
//   - The new ValidUntil must be strictly later than the current
//     one: this is an extension only, never a shortening. An equal,
//     earlier, or zero ValidUntil fails with ErrMandateInvalidWindow,
//     the same gate CreateMandate applies to the original window, so
//     an amendment can never install a window the mandate could not
//     have been created with. A failed amendment leaves the original
//     validity end exactly as it was. In particular there is no
//     same-value no-op here, unlike the term-deposit rate amendment
//     (LG-247): re-stating the current end is a refused shortening
//     attempt, not a redundant reprice — "extend" with nothing to
//     extend is a caller error, not a silent success.
//   - The amendment touches the validity end and nothing else: the
//     payer, payee, currency, per-debit cap, frequency, ValidFrom,
//     creation instant, and status are unchanged, and past debits
//     are untouched — the window is a gate on future pulls, not a
//     rewrite of settled ones. There is no window state to migrate:
//     DebitWithMandate reads the live ValidUntil on every pull (the
//     lazy-expiry predicate and the booking-timestamp gate both key
//     on it), so the new end takes effect from the very next debit
//     with no debit-path change. The extension cuts one way only:
//     a debit after the old end but before the new end, which the old
//     window would have rejected as expired, now pulls; a debit
//     already rejected under the old window stays rejected — the
//     rejection booked nothing, so the caller simply retries it
//     under the extended window.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the mandate was already
//     reported when created. The amendment lands one audit event
//     (mandate_valid_until_amended, with the mandate ID, the
//     previous and new validity ends, the currency, and the cap) so
//     the "who extended the authorization, and from when to when"
//     trail is tamper-evident. Mandates are snapshot records, so the
//     amended validity end rides the mandate record through
//     snapshots exactly like the original — no snapshot change is
//     needed.
//
// Orthogonality: CreateMandate owns the initial window,
// SuspendMandate and CancelMandate own the lifecycle,
// ExpireMandates owns the expiry sweep, DebitWithMandate owns the
// pulls, AmendMandateMaxAmount (LG-243) owns the per-debit cap,
// AmendMandateFrequency (LG-246) owns the pull cadence, and this
// module owns only the validity-end extension.

// AmendMandateValidUntil extends an active mandate's ValidUntil to
// newValidUntil in place. The new end must be strictly later than the
// current one; a failed amendment leaves the mandate exactly as it
// was. Amending is off-journal — it does not bump the ledger version
// — but it is audited as mandate_valid_until_amended.
func (l *Ledger) AmendMandateValidUntil(mandateID string, newValidUntil time.Time) (Mandate, error) {
	return l.AmendMandateValidUntilCtx(context.Background(), mandateID, newValidUntil)
}

// AmendMandateValidUntilCtx is AmendMandateValidUntil with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendMandateValidUntilCtx(ctx context.Context, mandateID string, newValidUntil time.Time) (Mandate, error) {
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
	if !newValidUntil.After(m.ValidUntil) {
		return Mandate{}, fmt.Errorf("%w: new valid_until %s is not after current %s", ErrMandateInvalidWindow, newValidUntil.UTC().Format(time.RFC3339), m.ValidUntil.UTC().Format(time.RFC3339))
	}
	previous := m.ValidUntil
	m.ValidUntil = newValidUntil
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_valid_until_amended",
		Actor:         "AmendMandateValidUntil",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":           mandateID,
			"previous_valid_until": previous.UTC().Format(time.RFC3339),
			"valid_until":          newValidUntil.UTC().Format(time.RFC3339),
			"currency":             m.Currency,
			"max_amount_cents":     m.MaxAmountCents,
		},
	})
	return m, nil
}
