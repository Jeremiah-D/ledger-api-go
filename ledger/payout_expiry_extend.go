package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Payout instruction expiry extension (LG-267).
//
// ExpirePayouts (LG-213) kills an approved payout whose opt-in
// ExpiresAt has passed; this module is the orthogonal pre-expiry
// path: an operator extends that deadline before it lapses, instead
// of letting the instruction expire and drafting a fresh one.
// Extending in place matters because a fresh instruction is a new
// instruction — it re-runs the maker-checker approval and loses
// the audit continuity from draft to approval — while an extension
// keeps the same instruction, the same maker and approver, and one
// audit trail from draft to submit, settle, or expiry.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an approved instruction that already carries an expiry
//     may be extended. An unknown ID fails with ErrPayoutNotFound.
//     A draft, submitted, settled, failed, cancelled, returned,
//     or expired instruction fails with
//     ErrPayoutInvalidTransition — the state machine moves one
//     way, and a departed instruction has no deadline left to
//     move. An instruction with no expiry (zero ExpiresAt =
//     never expires, the LG-213 convention) fails with
//     ErrPayoutExpiryNotSet, following the LG-234 precedent:
//     extending must never invent a deadline on an instruction
//     that was meant to stay submittable until submitted or
//     manually cancelled.
//   - The new deadline must be strictly later than the current
//     one and in the future: extensions only lengthen, never
//     shorten. An equal, earlier, or past new deadline fails
//     with ErrPayoutExpiryExtensionNotLater and the original
//     deadline is untouched, so a mistaken call cannot silently
//     cut an approved instruction short.
//   - An approved instruction whose ExpiresAt already passed but
//     whose sweep has not run yet — and which no submit attempt
//     has lazily expired — is still extendable: the instruction
//     is still approved, and extending it to a future time
//     lengthens a live instruction rather than resurrecting a
//     departed one (the LG-229/LG-230 stale-deadline rule).
//     Once the sweep (or a submit attempt) marks it expired,
//     extension is rejected with ErrPayoutInvalidTransition.
//     Note the extension itself never lazily expires: unlike
//     SubmitPayout, it does not move money, so there is no
//     lapsed-approval risk to guard — the operator is explicitly
//     choosing to keep this approval alive.
//   - The extension touches the deadline and nothing else: the
//     amount, currency, payee, destination, maker, approver,
//     status, and journaled rows are unchanged, the ledger
//     version is not bumped, and no outbox event fires. The
//     extension lands one audit event
//     (payout_expiry_extended, with the previous and new
//     deadlines) so the "who moved the deadline, and from what
//     to what" trail is tamper-evident.
//   - ExpirePayoutsAt and the SubmitPayout lazy-expiry guard
//     read the live deadline (see payout_expiry.go), so no sweep
//     change is needed: an extended instruction survives sweeps
//     up to its new deadline and expires after it. Payout
//     instructions stay out of snapshots (see snapshot.go),
//     so an extension, like the instruction itself, is
//     in-memory operational state and never round-trips.
//
// Orthogonality: CreatePayout owns the initial instruction (and
// its expiry), ApprovePayout owns the approval, SubmitPayout owns
// the clearing earmark, ExpirePayouts owns the sweep, and this
// module owns only the pre-expiry lengthening. It is orthogonal
// to the payout expiry mechanism itself (LG-213) and to the FX
// limit-order expiry extension (LG-234), which moves an order
// book deadline rather than a payout instruction deadline.

// Payout instruction expiry extension errors.
var (
	// ErrPayoutExpiryNotSet is returned by ExtendPayoutExpiry
	// when the instruction carries no expiry to extend: the
	// operator approves an expiring instruction instead.
	ErrPayoutExpiryNotSet = errors.New("ledger: payout instruction has no expiry to extend")
	// ErrPayoutExpiryExtensionNotLater is returned by
	// ExtendPayoutExpiry when the new deadline is not strictly
	// later than the current expiry, or is not in the future.
	ErrPayoutExpiryExtensionNotLater = errors.New("ledger: payout instruction expiry extension must be later than current expiry and in the future")
)

// ExtendPayoutExpiry moves an approved payout instruction's
// ExpiresAt to newExpiry and returns the updated instruction. The
// amount, currency, payee, destination, maker, approver, status,
// and journaled rows are unchanged, and a failed extension leaves
// the original deadline exactly as it was. Extending is
// off-journal — it does not bump the ledger version — but it is
// audited as payout_expiry_extended.
func (l *Ledger) ExtendPayoutExpiry(id string, newExpiry time.Time) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, fmt.Errorf("%w: %q", ErrPayoutNotFound, id)
	}
	if p.Status != PayoutApproved {
		return PayoutInstruction{}, fmt.Errorf("%w: payout %q is %s", ErrPayoutInvalidTransition, id, p.Status)
	}
	if p.ExpiresAt.IsZero() {
		return PayoutInstruction{}, ErrPayoutExpiryNotSet
	}
	now := time.Now()
	if !newExpiry.After(now) || !newExpiry.After(p.ExpiresAt) {
		return PayoutInstruction{}, ErrPayoutExpiryExtensionNotLater
	}
	previous := p.ExpiresAt
	p.ExpiresAt = newExpiry
	l.payouts[id] = p
	l.emitAudit(AuditEvent{
		Op:            "payout_expiry_extended",
		Actor:         "ExtendPayoutExpiry",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"payout_id":           id,
			"previous_expires_at": previous.UTC().Format(time.RFC3339),
			"expires_at":          newExpiry.UTC().Format(time.RFC3339),
		},
	})
	return p, nil
}
