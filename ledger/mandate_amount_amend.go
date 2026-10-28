package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate per-debit cap amendment (LG-243).
//
// CreateMandate (LG-125) fixes a mandate's per-debit cap at creation;
// SuspendMandate and CancelMandate own the mandate's lifecycle. This
// module is the orthogonal in-place cap path: an operator revises the
// cap on a live authorization instead of cancelling it and issuing a
// new one. Amending in place matters because a cancel-and-replace is
// a new mandate — a fresh ID that breaks the mandate_of link story
// and the debit history — while an amendment keeps the same mandate,
// the same authorization, and one audit trail from creation to every
// debit under it.
//
// Semantics, deliberately fail-closed throughout, along the
// SuspendMandate state gates:
//
//   - Only an active mandate may be amended. An unknown ID fails
//     with ErrMandateNotFound; a suspended, cancelled, or
//     already-marked-expired mandate fails with ErrMandateNotActive;
//     a mandate whose ValidUntil has passed — swept to expired or
//     merely lapsed while nobody was watching — fails with
//     ErrMandateExpired, the same lazy-expiry reading Debit uses.
//     An expired mandate is never resurrected by an amendment: the
//     operator creates a new mandate instead.
//   - The new cap must be positive: zero or negative fails with
//     ErrMandateNonPositiveMaxAmount, the same gate CreateMandate
//     applies to the original cap, so an amendment can never
//     install a cap the mandate could not have been created with.
//     A failed amendment leaves the original cap exactly as it was.
//   - The amendment touches the cap and nothing else: the payer,
//     payee, currency, frequency, validity window, creation instant,
//     and status are unchanged, and past debits are untouched —
//     the cap is a gate on future pulls, not a rewrite of settled
//     ones. DebitWithMandate reads the live MaxAmountCents on every
//     pull (see debitMandateCoreLockedExcluding), so the new cap
//     takes effect from the very next debit with no debit-path
//     change: a debit that was under the old cap stays journaled
//     even if it exceeds the new one, and a debit above the old
//     cap becomes pullable the moment the cap is raised past it.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the mandate was
//     already reported when created (see the pre-authorization
//     amount amendment, LG-235, for the same convention). The
//     amendment lands one audit event
//     (mandate_max_amount_amended, with the mandate ID, the
//     previous and new caps, the currency, and the frequency) so
//     the "who moved the cap, and from what to what" trail is
//     tamper-evident. Mandates are snapshot records, so the
//     amended cap rides the mandate record through snapshots
//     exactly like the original — no snapshot change is needed.
//
// Orthogonality: CreateMandate owns the initial cap, SuspendMandate
// and CancelMandate own the lifecycle, ExpireMandates owns the
// expiry sweep, DebitWithMandate owns the pulls, and this module
// owns only the per-debit cap revision.

// AmendMandateMaxAmount sets an active mandate's per-debit cap to
// newMaxCents in place. The new cap must be positive; a failed
// amendment leaves the mandate exactly as it was. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as mandate_max_amount_amended.
func (l *Ledger) AmendMandateMaxAmount(mandateID string, newMaxCents int64) (Mandate, error) {
	return l.AmendMandateMaxAmountCtx(context.Background(), mandateID, newMaxCents)
}

// AmendMandateMaxAmountCtx is AmendMandateMaxAmount with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendMandateMaxAmountCtx(ctx context.Context, mandateID string, newMaxCents int64) (Mandate, error) {
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
	if newMaxCents <= 0 {
		return Mandate{}, ErrMandateNonPositiveMaxAmount
	}
	previous := m.MaxAmountCents
	m.MaxAmountCents = newMaxCents
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_max_amount_amended",
		Actor:         "AmendMandateMaxAmount",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":                mandateID,
			"previous_max_amount_cents": previous,
			"max_amount_cents":          newMaxCents,
			"currency":                  m.Currency,
			"frequency":                 m.Frequency,
		},
	})
	return m, nil
}
