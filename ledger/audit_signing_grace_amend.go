package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Audit-log signing grace window runtime revision (LG-348).
//
// WithAuditLogSigning (audit.go) and LEDGER_AUDIT_SIGNING_GRACE
// (main.go) configure the retired-key verification window only at
// construction — the grace was parsed once at startup and sealed
// into the signer, which is immutable after construction
// (audit_sign.go), so tightening the window after a key leak (make
// rows signed by the stolen retired key fail verification sooner,
// shrinking the exposure) or widening it when clock skew or a slow
// rotation makes legitimate old rows fail as key_retired both
// needed a restart: a gap in which the old window governs, plus a
// fresh process identity that breaks the audit story. This module
// is the orthogonal runtime path: an operator revises the live
// grace in place, instead of rebuilding the ledger.
//
// Semantics, deliberately fail-closed throughout, along the LG-305
// (same signMu domain, same lock order) and LG-347 (the same
// duration-knob, (previous, error) audit shape) revisions:
//
//   - The AuditLog side owns the swap under signMu: setSigningGrace
//     updates a.signingGrace and, when a key set is installed,
//     immediately rebuilds the live signer from the same key set
//     with the new grace (newAuditSigner(a.signingKeys, d)), so
//     the next verification judges against the new window. A
//     ledger without keys only stores the grace — the next
//     rotation (LG-305, "an unsigned ledger rotates into
//     signing") installs a signer built with the revised grace.
//     The rebuild cannot fail after validation (the key set is
//     non-empty, the grace is positive), but the setter still
//     fails closed and installs nothing if it ever did.
//   - The background writer snapshots its signer at flush time
//     (audit.go run: snapshotSigner), so a row already being
//     flushed keeps the signer it started with — the revision is
//     future-only. The primary key never changes and new rows
//     keep the same kid, so signing itself is untouched: the
//     grace only governs verification of retired-key rows.
//   - Validation is fail-closed with no silent normalization:
//     the new grace must be positive, else
//     ErrInvalidAuditSigningGrace and the live grace stays
//     exactly as it was (the returned value is the live grace in
//     that case too). Zero is rejected, not a "close the window"
//     state: a zero grace makes any row signed by a retired key
//     fail as key_retired the instant RetiredAt arrives
//     (verifyReason, audit_sign.go), silently voiding all
//     history verification for retired keys — a semantic trap,
//     not a meaningful "off" state (unlike the FX watch's 0 =
//     silence-a-class-of-alerts). There is no upper bound: an
//     over-long grace keeps a stolen retired key verifiable for
//     longer, and the operator owns that trade-off (documented
//     in the README).
//   - A revision identical to the live grace is a no-op: it
//     returns the live value with a nil error and writes no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - A ledger with no audit log attached (WithAuditLog was never
//     used) rejects the revision with ErrAuditLogNotConfigured
//     (along LG-301) — there is no live value to return, so the
//     returned grace is 0.
//   - The amendment is structural config, not ledger state: it
//     bumps neither the version nor the audit chain, writes no
//     journal entry, and emits no outbox event. It lands one
//     audit_signing_grace_retuned audit event (previous_grace /
//     current_grace, as duration strings, along LG-312/LG-347).
//     The Ctx variant attributes the operator (LG-73); with no
//     operator in ctx the actor defaults to DefaultAuditActor
//     ("system").
//   - Snapshots do not carry the grace (AuditLog lives outside
//     the ledger snapshot): a restart restores the env/default
//     value, while the revision events themselves stay in the
//     audit trail for traceability.
//   - Honest boundaries, two of them: offline
//     VerifyAuditSignatures(dir, keys, grace) takes an explicit
//     grace argument — a live revision changes no offline call's
//     argument, so an operator verifying old rows offline must
//     still pass the grace they mean. SignAuditManifest builds
//     its signer straight from DefaultAuditSigningGrace — export
//     manifest signatures still use the 24h constant and are
//     unaffected by this item (the LG-305 twin boundary: this
//     item governs only the row-level signing signer). GET
//     /audit/verify is hash-chain verification and is
//     unaffected.
//
// Orthogonality: LG-65 owns the signing mechanism itself (this
// module is its runtime entry point for the grace); LG-305 owns
// signing-key rotation (it swaps the key set, this module moves
// only the verification window — the same signer, different
// knobs; LG-305's own comment says "rotation rebuilds the signer
// with this same grace", and this item is what makes that grace
// a revisable live value); LG-301 owns maxBytes/retentionDays
// (which explicitly named signing as an independent concern —
// this module is that concern); LG-342 owns the writer fsync
// interval (writer durability, same AuditLog, different knob);
// LG-309 owns the audit async queue capacity (writer queue, not
// the signing window); LG-128 owns export manifest signing
// (explicit key set, constant grace, unaffected); LG-314 owns
// the audit sink poll interval (background forwarder, not
// row-level signing); LG-67 owns the audit sink's webhook HMAC
// key (a different key set, a different component). There is no
// HTTP endpoint for the revision (along LG-241/242), no new env
// knob, and the grace never enters snapshots.

var (
	// ErrInvalidAuditSigningGrace is returned by
	// SetAuditSigningGrace when the requested grace is not
	// positive. The live grace is left untouched (the returned
	// value is the live grace in that case).
	ErrInvalidAuditSigningGrace = errors.New("ledger: audit signing grace must be positive")
)

// SigningGrace reports the live retired-key verification window,
// read under signMu.RLock.
func (a *AuditLog) SigningGrace() time.Duration {
	a.signMu.RLock()
	defer a.signMu.RUnlock()
	return a.signingGrace
}

// setSigningGrace installs a new retired-key verification window
// and reports the previous value and whether it changed. Callers
// must validate d > 0 first. When a key set is installed the live
// signer is rebuilt immediately from the same keys with the new
// grace, so the next verification judges against the new window;
// an unsigned log only stores the grace for the next rotation.
// The swap is unconditional under signMu, so the background
// writer (which snapshots the signer per flush) sees the new
// signer on its next line.
func (a *AuditLog) setSigningGrace(d time.Duration) (previous time.Duration, changed bool, err error) {
	a.signMu.Lock()
	defer a.signMu.Unlock()
	previous = a.signingGrace
	if previous == d {
		return previous, false, nil
	}
	if len(a.signingKeys) > 0 {
		signer, err := newAuditSigner(a.signingKeys, d)
		if err != nil {
			// Unreachable after validation (the key set is
			// non-empty, the grace is positive), but fail
			// closed: never install a signer the constructor
			// would reject.
			return previous, false, err
		}
		a.signer = signer
	}
	a.signingGrace = d
	return previous, true, nil
}

// SetAuditSigningGrace revises the live audit-log signing grace
// window in place and returns the pre-revision grace (for the
// audit trail). A tightened grace makes rows signed by a retired
// key fail verification sooner; a widened grace re-admits rows
// that failed as key_retired under the old window. Signing itself
// is untouched (the primary key and new rows' kid never change).
// A non-positive grace is rejected with
// ErrInvalidAuditSigningGrace and the live grace is left
// untouched (the returned value is the live grace in that case
// too). Revising to the current grace is a no-op with no audit
// event. A ledger with no audit log attached returns
// ErrAuditLogNotConfigured (and 0). The revision is audited as
// audit_signing_grace_retuned. A restart restores the env/default
// value.
func (l *Ledger) SetAuditSigningGrace(d time.Duration) (time.Duration, error) {
	return l.SetAuditSigningGraceCtx(context.Background(), d)
}

// SetAuditSigningGraceCtx is SetAuditSigningGrace with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetAuditSigningGraceCtx(ctx context.Context, d time.Duration) (time.Duration, error) {
	// Check order, along LG-344/LG-346: no audit log first, then
	// validate, then compare, then swap, then audit — all under
	// l.mu, with the AuditLog-side swap under signMu, so the
	// lock order l.mu -> a.signMu (established by LG-305) holds.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.audit == nil {
		return 0, ErrAuditLogNotConfigured
	}
	previous := l.audit.SigningGrace()
	if d <= 0 {
		return previous, fmt.Errorf("%w: got %s", ErrInvalidAuditSigningGrace, d)
	}
	if d == previous {
		return previous, nil
	}
	prev, changed, err := l.audit.setSigningGrace(d)
	if err != nil {
		return previous, err
	}
	if !changed {
		return prev, nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_signing_grace_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_grace": previous.String(),
			"current_grace":  d.String(),
		},
	})
	return previous, nil
}

// AuditSigningGrace reports the live audit-log signing grace
// window: how long after a key's retirement its signatures still
// verify. Enabled is false when no audit log is attached
// (WithAuditLog was never used); an attached log always reports
// a positive grace (the default DefaultAuditSigningGrace when
// never revised). Follows the AuditStats enabled shape.
func (l *Ledger) AuditSigningGrace() (grace time.Duration, enabled bool) {
	if l.audit == nil {
		return 0, false
	}
	return l.audit.SigningGrace(), true
}
