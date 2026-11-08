package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Dispute-hold TTL runtime revision (LG-358).
//
// The freeze hold a raised dispute places on the payee (see
// dispute.go) expires after 120 days (defaultDisputeHoldTTL): the
// classic chargeback filing window. The TTL was a package constant
// with no live field and no setter on the Ledger: an operator who
// wanted to tighten the freeze to 60 days under a card-scheme rule
// change — or loosen it to 180 days for a complex dispute — could
// only change the constant and rebuild, losing all live state.
// This module is the runtime path: an operator revises the live TTL
// in place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along the LG-357
// representment-cap and LG-312 max-hold-duration revisions:
//
//   - The revision is future-only: it governs the next dispute
//     raised. Already-raised holds keep the ExpiresAt they were
//     created with — tightening never shortens a live freeze and
//     loosening never extends one. RaiseDispute reads the live
//     value under the write lock it already holds; the setter
//     holds the write lock, along the representmentWindow
//     morphology.
//   - Validation is fail-closed with no silent normalization: the
//     new TTL must be positive, else ErrInvalidDisputeHoldTTL and
//     the live TTL stays exactly as it was. Zero is rejected — a
//     zero TTL would make a new freeze born expired — and
//     negatives are rejected. Zero has no "off" meaning here:
//     unlike SetMaxHoldDuration, where 0 disables the cap, a
//     dispute freeze always needs a horizon, along
//     SetRepresentmentWindow's non-positive rejection.
//   - A revision identical to the live TTL is a no-op: it returns
//     the live value with a nil error and writes no audit event,
//     so a retried or duplicated revision cannot spam the audit
//     trail.
//   - The amendment is structural config, not ledger state: it
//     bumps neither the version nor the audit chain, writes no
//     journal entry, and emits no outbox event. It lands one
//     dispute_hold_ttl_retuned audit event
//     (previous_dispute_hold_ttl / dispute_hold_ttl, as duration
//     strings, following the LG-312 hold_max_duration_amended
//     detail shape). The Ctx variant attributes the operator
//     (LG-73).
//   - The LG-312 maximum-hold-duration cap exemption is unchanged:
//     dispute holds still travel through holdLockedUncapped, so a
//     cap shorter than the TTL never makes dispute raising fail.
//     The cap and the TTL are orthogonal knobs — the cap bounds
//     operator-chosen authorization horizons, the TTL bounds the
//     risk-control freeze.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-326 family): dispute governance
//     tuning is in-process structural config.
//   - Snapshots do not carry the TTL: a restore starts again at the
//     default 120 days, while the revision events themselves stay
//     in the audit trail for traceability.
//
// Orthogonality: LG-312 owns the generic maximum hold duration
// (this module is the dispute freeze's own TTL, not the admission
// cap), LG-357 owns the representment evidence-package cap (the
// payee's filing, this module the freeze horizon), and LG-179 owns
// the representment mechanism itself.

var (
	// ErrInvalidDisputeHoldTTL is returned by SetDisputeHoldTTL
	// when the requested TTL is not positive. The live TTL is left
	// untouched.
	ErrInvalidDisputeHoldTTL = errors.New("ledger: dispute hold TTL must be positive")
)

// SetDisputeHoldTTL revises the live dispute-hold TTL in place and
// reports the pre-revision TTL (for the audit trail). A shorter TTL
// tightens the freeze horizon for disputes raised after the
// revision; a longer one loosens it. Already-raised holds are
// unaffected. A non-positive TTL returns ErrInvalidDisputeHoldTTL
// with the live TTL untouched (the returned value is the live TTL
// in that case too). A revision identical to the live TTL is a
// no-op that returns the live value with a nil error and no side
// effect.
func (l *Ledger) SetDisputeHoldTTL(d time.Duration) (time.Duration, error) {
	return l.SetDisputeHoldTTLCtx(context.Background(), d)
}

// SetDisputeHoldTTLCtx is SetDisputeHoldTTL with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetDisputeHoldTTLCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.disputeHoldTTL
	if d <= 0 {
		return previous, fmt.Errorf("%w: %s", ErrInvalidDisputeHoldTTL, d)
	}
	if d == previous {
		return previous, nil
	}
	l.disputeHoldTTL = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "dispute_hold_ttl_retuned",
		Actor:         "SetDisputeHoldTTL",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_dispute_hold_ttl": previous.String(),
			"dispute_hold_ttl":          d.String(),
		},
	})
	return previous, nil
}

// DisputeHoldTTL reports the live dispute-hold TTL: the freeze hold
// a newly raised dispute places expires this long after the raise.
func (l *Ledger) DisputeHoldTTL() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.disputeHoldTTL
}
