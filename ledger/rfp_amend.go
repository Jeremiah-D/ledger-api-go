// Request-for-payment amendment (LG-363).
//
// CreateRfP (LG-171) fixes an RfP's terms — amount, currency, expiry —
// at issue time, and the record stays issued until the debtor accepts,
// rejects, or it expires. A creditor who mistyped the amount or the
// deadline had no way to correct it short of rejecting the request and
// issuing a fresh one: a new ID, a new audit trail, and the rejected
// request left on the books as noise. This module is the revision path:
// an operator corrects an issued request's terms in place, before any
// acceptance has been given and before any money has moved.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an issued request may be amended. An unknown ID fails with
//     ErrRfPNotFound. An issued request past its expiry is lazily
//     expired first — persisted as Expired with an rfp_expired audit
//     event, exactly like AcceptRfP/RejectRfP — and fails with
//     ErrRfPExpired. Any other status (paid, rejected, expired) fails
//     with ErrRfPNotIssued: once the request has been answered, the
//     terms the debtor answered to are the terms that stand.
//   - The merged candidate reuses the CreateRfP field checks exactly
//     (validateRfPFields): the amount must be positive, else
//     ErrRfPNonPositiveAmount; the currency is normalized (empty
//     becomes the default currency; anything but 3 uppercase letters
//     is rejected with ErrInvalidCurrency — lowercase is not folded,
//     exactly like at creation); the expiry must be in the future,
//     else ErrRfPExpiryNotFuture. A failed validation leaves the live
//     record untouched.
//   - The amendment touches AmountCents, Currency, and ExpiresAt and
//     nothing else: ID, Creditor, Debtor, CreatedAt, Status,
//     TransferID, and the paid receipt are unchanged — the parties are
//     the request's identity, not its terms. Amending to the current
//     terms is a no-op: the record is untouched and no audit event
//     fires, so a retried or redundant revision does not pollute the
//     audit trail with a change that changed nothing.
//   - Like issuing, the amendment is structural: it bumps neither the
//     ledger version nor the audit chain, and books no journal rows —
//     the RfP is off-journal until the debtor accepts. The amendment
//     lands one audit event (rfp_amended, with the previous and new
//     terms) so the "who changed the amount, and from what to what"
//     trail is tamper-evident. RfPs stay out of snapshots (see rfp.go),
//     so an amendment, like the request itself, is in-memory
//     operational state and never round-trips.
//
// Orthogonality: AmendPayoutAmount owns the payout instruction's
// amount (a different object — the payout instruction, not the payment
// request); the schedule amendments own recurring-plan terms; and
// Create/Accept/Reject/Expire own the RfP lifecycle itself — this module
// owns only the issued request's terms.
package ledger

import (
	"context"
	"time"
)

// RfPAmendment is the patch for AmendRfP: nil fields leave the term
// untouched. Creditor and Debtor are not amendable — the parties are the
// request's identity, and changing them would make it a different
// request, not a corrected one.
type RfPAmendment struct {
	AmountCents *int64
	Currency    *string
	ExpiresAt   *time.Time
}

// AmendRfP revises an issued request-for-payment's terms in place. An
// unknown ID fails with ErrRfPNotFound; a lazily expired request fails
// with ErrRfPExpired (and is persisted as expired); a non-issued
// request fails with ErrRfPNotIssued; an invalid merged candidate fails
// with the CreateRfP field error and leaves the live record untouched.
// Only AmountCents, Currency, and ExpiresAt change; amending to the
// current terms is a no-op. Amending is off-journal — it does not bump
// the ledger version — but it is audited as rfp_amended with the
// previous and new terms.
func (l *Ledger) AmendRfP(ctx context.Context, id string, patch RfPAmendment, actor string) (RfP, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r, ok := l.rfps[id]
	if !ok {
		return RfP{}, ErrRfPNotFound
	}
	if rfpExpiredLocked(r, now) {
		r.Status = RfPStatusExpired
		l.rfps[id] = r
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "rfp_expired",
			Actor:         "AmendRfP",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Creditor, r.Debtor},
		})
		return RfP{}, ErrRfPExpired
	}
	if r.Status != RfPStatusIssued {
		return RfP{}, ErrRfPNotIssued
	}
	candidate := r
	if patch.AmountCents != nil {
		candidate.AmountCents = *patch.AmountCents
	}
	if patch.Currency != nil {
		candidate.Currency = *patch.Currency
	}
	if patch.ExpiresAt != nil {
		candidate.ExpiresAt = *patch.ExpiresAt
	}
	validated, err := validateRfPFields(candidate, now)
	if err != nil {
		return RfP{}, err
	}
	candidate = validated
	if candidate.AmountCents == r.AmountCents && candidate.Currency == r.Currency && candidate.ExpiresAt.Equal(r.ExpiresAt) {
		return r, nil
	}
	if actor == "" {
		actor = "AmendRfP"
	}
	previous := r
	l.rfps[id] = candidate
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "rfp_amended",
		Actor:         actor,
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details: map[string]any{
			"previous_amount_cents": previous.AmountCents,
			"amount_cents":          candidate.AmountCents,
			"previous_currency":     previous.Currency,
			"currency":              candidate.Currency,
			"previous_expires_at":   previous.ExpiresAt.UTC().Format(time.RFC3339),
			"expires_at":            candidate.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return candidate, nil
}
