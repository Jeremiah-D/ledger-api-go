package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate payer amendment (LG-366).
//
// CreateMandate (LG-125) fixes a mandate's payer at creation;
// AmendMandatePayee (LG-340) owns the other side of the pair, and
// its module text names the payer revision explicitly as the later
// item — this module is that item. An operator corrects the payer
// on a live authorization — a payer account entered wrong, a
// settlement account rotated — instead of cancelling the mandate
// and issuing a new one. Amending in place matters because a
// cancel-and-replace is a new mandate — a fresh ID that breaks the
// mandate_of link story and the debit history — while an amendment
// keeps the same mandate, the same authorization, and one audit
// trail from creation to every debit under it.
//
// Semantics, deliberately fail-closed throughout, along
// AmendMandatePayee verbatim — this is the same revision family,
// the other half of the pair:
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
//   - The new payer reuses the creation validation: an empty payer
//     fails with ErrEmptyMandatePayer, and a payer equal to the
//     mandate's payee fails with ErrMandateSameParty — an
//     amendment can never install a payer the mandate could not
//     have been created with. A failed amendment leaves the
//     original payer exactly as it was.
//   - Amending to the current payer is a no-op: it returns the
//     mandate unchanged and writes no audit event, like
//     AmendMandatePayee.
//   - The amendment touches the payer and nothing else: the
//     payee, currency, per-debit cap, frequency, validity
//     window, creation instant, and status are unchanged, and
//     past debits are untouched — the payer is a gate on future
//     pulls, not a rewrite of settled ones. DebitWithMandate
//     reads the live Payer on every pull (see
//     debitMandateCoreLockedExcluding), so the new payer governs
//     the very next debit with no debit-path change: a debit
//     already journaled from the old payer stays journaled, and
//     the next pull settles from the new payer.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the mandate was
//     already reported when created. The amendment lands one
//     audit event (mandate_payer_amended, with the mandate ID,
//     the previous and new payers, and the currency) so the "who
//     moved the payer, and from whom to whom" trail is
//     tamper-evident. Mandates are snapshot records, so the
//     amended payer rides the mandate record through snapshots
//     exactly like the original — no snapshot change is needed.
//
// Orthogonality: CreateMandate owns the initial payer,
// SuspendMandate and CancelMandate own the lifecycle,
// ExpireMandates owns the expiry sweep, DebitWithMandate owns
// the pulls, AmendMandatePayee (LG-340) owns the payee revision,
// AmendMandateMaxAmount (LG-243) owns the per-debit cap,
// AmendMandateFrequency (LG-246) owns the pull cadence,
// AmendMandateValidUntil (LG-248) owns the validity end, and
// this module owns only the payer revision.

// AmendMandatePayer sets an active mandate's Payer to newPayer in
// place. The new payer must be non-empty and must differ from the
// mandate's payee; amending to the current payer is a no-op, and a
// failed amendment leaves the mandate exactly as it was. Amending
// is off-journal — it does not bump the ledger version — but a
// real revision is audited as mandate_payer_amended.
func (l *Ledger) AmendMandatePayer(mandateID string, newPayer AccountID) (Mandate, error) {
	return l.AmendMandatePayerCtx(context.Background(), mandateID, newPayer)
}

// AmendMandatePayerCtx is AmendMandatePayer with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendMandatePayerCtx(ctx context.Context, mandateID string, newPayer AccountID) (Mandate, error) {
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
	if newPayer == "" {
		return Mandate{}, ErrEmptyMandatePayer
	}
	if newPayer == m.Payee {
		return Mandate{}, ErrMandateSameParty
	}
	if newPayer == m.Payer {
		return m, nil
	}
	previous := m.Payer
	m.Payer = newPayer
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_payer_amended",
		Actor:         "AmendMandatePayer",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":     mandateID,
			"previous_payer": previous,
			"payer":          newPayer,
			"currency":       m.Currency,
		},
	})
	return m, nil
}
