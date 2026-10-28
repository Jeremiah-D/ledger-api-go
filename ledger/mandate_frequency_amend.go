package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate frequency amendment (LG-246).
//
// CreateMandate (LG-125) fixes a mandate's frequency at creation;
// SuspendMandate and CancelMandate own the mandate's lifecycle.
// This module is the orthogonal in-place frequency path: an
// operator revises how often a live authorization may be pulled
// instead of cancelling it and issuing a new one. Amending in
// place matters because a cancel-and-replace is a new mandate — a
// fresh ID that breaks the mandate_of link story and the debit
// history — while an amendment keeps the same mandate, the same
// authorization, and one audit trail from creation to every debit
// under it.
//
// Semantics, deliberately fail-closed throughout, along the
// SuspendMandate state gates (LG-243):
//
//   - Only an active mandate may be amended. An unknown ID fails
//     with ErrMandateNotFound; a suspended, cancelled, or
//     already-marked-expired mandate fails with ErrMandateNotActive;
//     a mandate whose ValidUntil has passed — swept to expired or
//     merely lapsed while nobody was watching — fails with
//     ErrMandateExpired, the same lazy-expiry reading Debit uses.
//     An expired mandate is never resurrected by an amendment: the
//     operator creates a new mandate instead.
//   - The new frequency must be one of per-day, per-week, or
//     per-month, else ErrInvalidMandateFrequency, the same gate
//     CreateMandate applies to the original frequency, so an
//     amendment can never install a frequency the mandate could
//     not have been created with. A failed amendment leaves the
//     original frequency exactly as it was.
//   - The amendment touches the frequency and nothing else: the
//     payer, payee, currency, per-debit cap, validity window,
//     creation instant, and status are unchanged, and past debits
//     are untouched — the frequency is a gate on future pulls,
//     not a rewrite of settled ones. There is no window state to
//     migrate: the frequency window is derived from each debit's
//     booking timestamp at gate time (see mandateWindow), so
//     DebitWithMandate reads the live Frequency on every pull and
//     the new frequency takes effect from the very next debit
//     with no debit-path change. The recomputation cuts both
//     ways, and operators should expect it: tightening —
//     per-day to per-month, say — widens the window, so a debit
//     earlier in the new window that the old frequency would
//     have let pass now blocks the next pull; loosening —
//     per-month to per-day — narrows it, so a debit earlier this
//     month but on a different day no longer blocks a pull today.
//     A debit already journaled under the old frequency stays
//     journaled even if the new window would have blocked it.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the mandate was
//     already reported when created (see the per-debit cap
//     amendment, LG-243, for the same convention). The amendment
//     lands one audit event (mandate_frequency_amended, with the
//     mandate ID, the previous and new frequencies, the currency,
//     and the cap) so the "who moved the cadence, and from what
//     to what" trail is tamper-evident. Mandates are snapshot
//     records, so the amended frequency rides the mandate record
//     through snapshots exactly like the original — no snapshot
//     change is needed.
//
// Orthogonality: CreateMandate owns the initial frequency,
// SuspendMandate and CancelMandate own the lifecycle,
// ExpireMandates owns the expiry sweep, DebitWithMandate owns the
// pulls, AmendMandateMaxAmount (LG-243) owns the per-debit cap,
// and this module owns only the pull-frequency revision.

// AmendMandateFrequency sets an active mandate's frequency to
// newFrequency in place. The new frequency must be per-day,
// per-week, or per-month; a failed amendment leaves the mandate
// exactly as it was. Amending is off-journal — it does not bump
// the ledger version — but it is audited as
// mandate_frequency_amended.
func (l *Ledger) AmendMandateFrequency(mandateID string, newFrequency string) (Mandate, error) {
	return l.AmendMandateFrequencyCtx(context.Background(), mandateID, newFrequency)
}

// AmendMandateFrequencyCtx is AmendMandateFrequency with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendMandateFrequencyCtx(ctx context.Context, mandateID string, newFrequency string) (Mandate, error) {
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
	if !validMandateFrequency(newFrequency) {
		return Mandate{}, fmt.Errorf("%w: %q", ErrInvalidMandateFrequency, newFrequency)
	}
	previous := m.Frequency
	m.Frequency = newFrequency
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_frequency_amended",
		Actor:         "AmendMandateFrequency",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":         mandateID,
			"previous_frequency": previous,
			"frequency":          newFrequency,
			"currency":           m.Currency,
			"max_amount_cents":   m.MaxAmountCents,
		},
	})
	return m, nil
}
