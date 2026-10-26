package ledger

import (
	"errors"
	"time"
)

// Hold expiry extension (LG-228).
//
// ExpireHolds/ExpireHoldsAt already sweep holds whose expiry has
// passed; this module is the orthogonal pre-expiry path: an operator
// extends an active hold's deadline before it lapses, instead of
// letting it expire and re-placing it. Extending in place matters
// because a re-placed hold would be a new reservation — a new ID,
// a new idempotency story, and a gap in which the funds are
// unreserved — while an extension keeps the same hold, the same
// reservation, and the same audit trail.
//
// Semantics, mirroring the Capture state checks:
//
//   - Only an active hold may be extended. An unknown ID fails
//     with ErrHoldNotFound; a captured or released hold fails with
//     ErrHoldNotActive; a hold whose expiry has already passed —
//     swept to expired or merely lapsed while nobody was watching —
//     fails with ErrHoldExpired, the same lazy-expiry reading
//     Capture uses. An expired hold is never resurrected by an
//     extension: the operator places a new hold instead.
//   - The new expiry must be strictly later than the current one:
//     extensions only lengthen, never shorten. An equal, earlier,
//     or past new expiry fails with ErrInvalidHoldExpiryExtension
//     and the original expiry is untouched — fail-closed, so a
//     mistaken call cannot silently cut a reservation short.
//   - The extension touches the expiry and nothing else: amount,
//     currency, and the partial-capture position (CapturedCents,
//     see CapturePartial) are unchanged, and a partially captured
//     hold stays active with its remainder still reserving funds.
//     The extension does not reset capture state in either
//     direction — it neither settles nor releases anything.
//   - The extension lands one audit event (hold_expiry_extended,
//     with the hold ID, the previous expiry, and the new expiry)
//     so the "who moved the deadline, and from what to what" trail
//     is tamper-evident. Like Hold and Release, it bumps neither
//     the ledger version nor the audit chain: holds are off-journal
//     reservations, and moving their deadline moves no money.
//   - Orthogonality: ExpireHolds/ExpireHoldsAt own the expiry
//     sweep (an extended hold simply survives sweeps up to its new
//     expiry and lapses after it), and CapturePartial owns the
//     capture position (an extension never touches it).

// Hold expiry extension errors.
var (
	// ErrInvalidHoldExpiryExtension is returned by
	// ExtendHoldExpiry when the new expiry is not strictly later
	// than the hold's current expiry.
	ErrInvalidHoldExpiryExtension = errors.New("ledger: hold expiry extension must be later than current expiry")
)

// ExtendHoldExpiry moves an active hold's expiry to newExpiresAt.
// The new expiry must be strictly later than the current one; the
// amount, currency, and capture position are untouched, and a
// failed extension leaves the hold exactly as it was. Extending is
// off-journal — it does not bump the ledger version — but it is
// audited as hold_expiry_extended.
func (l *Ledger) ExtendHoldExpiry(holdID string, newExpiresAt time.Time) (Hold, error) {
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
	if !newExpiresAt.After(h.ExpiresAt) {
		return Hold{}, ErrInvalidHoldExpiryExtension
	}
	previous := h.ExpiresAt
	h.ExpiresAt = newExpiresAt
	l.holds[holdID] = h
	l.emitAudit(AuditEvent{
		Op:            "hold_expiry_extended",
		Actor:         "ExtendHoldExpiry",
		TraceID:       h.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"hold_id":             h.ID,
			"amount_cents":        h.AmountCents,
			"currency":            h.Currency,
			"previous_expires_at": previous.UTC().Format(time.RFC3339),
			"expires_at":          newExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return h, nil
}
