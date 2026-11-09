package ledger

import (
	"context"
	"fmt"
	"time"
)

// Mandate validity-start amendment (LG-365).
//
// CreateMandate (LG-125) fixes a mandate's validity window at
// creation; SuspendMandate and CancelMandate own the mandate's
// lifecycle, and AmendMandateMaxAmount (LG-243),
// AmendMandateFrequency (LG-246), AmendMandateValidUntil (LG-248),
// and AmendMandatePayee (LG-340) own the cap, the cadence, the
// validity end, and the payee. ValidFrom is the last window edge
// with no revision entry — LG-248's text names it explicitly as a
// field the end amendment leaves untouched. This module is the
// orthogonal in-place validity-start path: an operator moves a
// live authorization's effective start — a customer who wants the
// first pull next week instead of next month, or a start pushed
// out while onboarding finishes — instead of cancelling the
// mandate and issuing a new one. Amending in place matters because
// a cancel-and-replace is a new mandate — a fresh ID that breaks
// the mandate_of link story and the debit history — while an
// amendment keeps the same mandate, the same authorization, and
// one audit trail from creation to every debit under it.
//
// Semantics, deliberately fail-closed throughout, along the
// SuspendMandate state gates (LG-243) and the validity-end
// amendment (LG-248):
//
//   - Only an active mandate with zero posted debits may be
//     amended. An unknown ID fails with ErrMandateNotFound; a
//     mandate whose ValidUntil has passed — swept to expired or
//     merely lapsed while nobody was watching — fails with
//     ErrMandateExpired, the same lazy-expiry reading Debit uses;
//     a suspended, cancelled, or already-marked-expired mandate
//     fails with ErrMandateNotActive. An expired mandate is never
//     resurrected by an amendment: the operator creates a new
//     mandate instead. A mandate that already has a posted debit
//     fails with ErrMandateHasDebits: with zero debits no debit
//     has been journaled under the old window, so moving the start
//     restates nothing — no journaled row references the old
//     window. Once a debit has posted, its journaled row fixes the
//     window that debit was pulled under, and moving the start
//     would restate the authorization history; that is out of scope
//     for this amendment. (The backlog item names no separate error
//     for the debited case; ErrMandateHasDebits is the new
//     "this revision is not allowed for this mandate" error, and
//     the wrap message names the posted-debit count, mirroring how
//     LG-286 names the posted-period count on its installment
//     gate.)
//   - The new ValidFrom must be strictly before the live
//     ValidUntil, reusing the creation window gate
//     (validateMandateFields): a new start at or after the live end
//     fails with ErrMandateInvalidWindow, so an amendment can never
//     install a window the mandate could not have been created
//     with. A zero ValidFrom passes the same gate — zero is before
//     any live end — and means effective since the beginning of
//     time, exactly as at creation. A failed amendment leaves the
//     original validity start exactly as it was.
//   - The amendment touches the validity start and nothing else:
//     the payer, payee, currency, per-debit cap, frequency,
//     ValidUntil, creation instant, and status are unchanged, and
//     past debits are untouched — the window is a gate on future
//     pulls, not a rewrite of settled ones. There is no window
//     state to migrate: DebitWithMandate reads the live ValidFrom
//     on every pull (the booking-timestamp gate keys on it), so the
//     new start takes effect from the very next debit with no
//     debit-path change. The revision cuts both ways, and only
//     forward: a debit before the old start but after the new one,
//     which the old window would have rejected as not yet active,
//     now pulls; a debit already rejected under the old window
//     stays rejected — the rejection booked nothing, so the caller
//     simply retries it under the new window.
//   - Amending to the current ValidFrom is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other mandate transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the mandate was already
//     reported when created. The amendment lands one audit event
//     (mandate_valid_from_amended, with the mandate ID, the
//     previous and new validity starts, the currency, and the cap)
//     so the "who moved the authorization start, and from when to
//     when" trail is tamper-evident. Mandates are snapshot records,
//     so the amended validity start rides the mandate record
//     through snapshots exactly like the original — no snapshot
//     change is needed.
//
// Orthogonality: CreateMandate owns the initial window,
// SuspendMandate and CancelMandate own the lifecycle,
// ExpireMandates owns the expiry sweep, DebitWithMandate owns the
// pulls, AmendMandateMaxAmount (LG-243) owns the per-debit cap,
// AmendMandateFrequency (LG-246) owns the pull cadence,
// AmendMandateValidUntil (LG-248) owns the validity end,
// AmendMandatePayee (LG-340) owns the payee, and this module owns
// only the validity-start revision.

// AmendMandateValidFrom revises an active mandate's ValidFrom to
// newValidFrom in place. The mandate must have zero posted debits
// and the new start must be strictly before the live ValidUntil
// (a zero start means effective since the beginning of time); a
// failed amendment leaves the mandate exactly as it was. Amending
// is off-journal — it does not bump the ledger version — but it is
// audited as mandate_valid_from_amended.
func (l *Ledger) AmendMandateValidFrom(mandateID string, newValidFrom time.Time) (Mandate, error) {
	return l.AmendMandateValidFromCtx(context.Background(), mandateID, newValidFrom)
}

// AmendMandateValidFromCtx is AmendMandateValidFrom with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendMandateValidFromCtx(ctx context.Context, mandateID string, newValidFrom time.Time) (Mandate, error) {
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
	if n := len(l.mandateDebits[mandateID]); n != 0 {
		return Mandate{}, fmt.Errorf("%w: mandate has %d posted debits", ErrMandateHasDebits, n)
	}
	if !newValidFrom.Before(m.ValidUntil) {
		return Mandate{}, fmt.Errorf("%w: new valid_from %s is not before current valid_until %s", ErrMandateInvalidWindow, newValidFrom.UTC().Format(time.RFC3339), m.ValidUntil.UTC().Format(time.RFC3339))
	}
	if newValidFrom.Equal(m.ValidFrom) {
		return m, nil
	}
	previous := m.ValidFrom
	m.ValidFrom = newValidFrom
	l.mandates[mandateID] = m
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "mandate_valid_from_amended",
		Actor:         "AmendMandateValidFrom",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":          mandateID,
			"previous_valid_from": previous.UTC().Format(time.RFC3339),
			"valid_from":          newValidFrom.UTC().Format(time.RFC3339),
			"currency":            m.Currency,
			"max_amount_cents":    m.MaxAmountCents,
		},
	})
	return m, nil
}
