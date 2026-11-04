package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Audit-sink signing-secret rotation (LG-313).
//
// StartAuditSink fixes the sink's HMAC signing secret for the life of
// the forwarder: SinkConfig.Secret is read once at construction and the
// sink signs every delivery with it. A leaked or aging key is a real
// security event, and the only path to a new key was Stop +
// StartAuditSink — a teardown, not a revision: Stop waits for the run
// goroutine to exit, a restart re-reads the cursor file (no progress
// is lost, but the stream stalls for the gap), a delivery in flight is
// cut short by the done channel, and a failed restart (outbox
// directory unavailable, corrupt cursor) stops forwarding entirely.
// This module is the orthogonal runtime path: an operator rotates the
// signing secret in place, instead of rebuilding the sink.
//
// Semantics, deliberately fail-closed throughout, along LG-306's
// delivery-policy revision (the same component, the same shape):
//
//   - The rotation is in place: the cursor, the queue order, the
//     counters, and the delivery policy stay exactly as they were.
//     Only the signing secret moves.
//   - The sink is the signer, not the verifier — each delivery carries
//     a single X-Ledger-Signature, so rotation is an atomic value
//     swap, NOT a retired-key table (unlike LG-300's anchor key and
//     LG-302's TSA trust root, which need retirement tables because a
//     verifier must still accept old signatures). There is no
//     verifier side here and no dual signing.
//   - Future-only is guaranteed by the existing snapshot: deliver
//     snapshots the secret under cfgMu.RLock at entry and computes the
//     signature once per delivery, and the retry loop reuses the same
//     signature — so a delivery in flight (including all its retries)
//     keeps the old-key signature, and the rotation governs only the
//     next delivery (along LG-288 / LG-301 / LG-304 / LG-306).
//   - The setter follows the LG-306 lock order: it swaps s.cfg.Secret
//     under cfgMu.Lock (bytes.Clone on the input, so the caller cannot
//     later mutate the live key — the snapshot hands deliver the live
//     slice, and deliver only reads it), releases cfgMu, then takes
//     l.mu.RLock to emit the audit event. cfgMu and l.mu are never
//     held nested, so the lock order is acyclic.
//   - Validation reuses the construction-time gate: an empty or nil
//     secret is ErrInvalidSinkSecret and the live secret stays exactly
//     as it was (SinkConfig.Validate refuses an empty secret —
//     "unsigned webhooks can be forged" — and a runtime revision must
//     never silently accept one). There is no minimum length: the
//     construction-time gate has none either.
//   - A revision byte-identical to the live secret is a no-op: it
//     returns nil and writes no audit event, so a retried or
//     duplicated rotation cannot spam the audit trail.
//   - The amendment is key management, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event — so the sink never forwards
//     its own revision event and there is no self-loop. It lands one
//     sink_secret_rotated audit event whose details carry only the
//     previous and current SHA-256 hex prefixes (first 12
//     characters, for correlation) — never key material: an HMAC
//     shared secret is not public material, unlike LG-302's public
//     certificate fingerprints, and along LG-305 key material never
//     enters the audit trail. The Ctx variant attributes the operator
//     (LG-73).
//   - Operator ordering must be: pre-provision the new secret at the
//     downstream (SIEM) first, then call RotateSinkSecret. Deliveries
//     after the rotation are signed with the new key immediately; a
//     downstream that has not pre-provisioned it fails verification,
//     and those events exhaust their normal retries into the dead
//     letter (audit_sink_dead_letter, cursor advancing as usual, not
//     blocking). Recovery is the downstream accepting the key and the
//     existing LG-67 dead-letter replay path — this module does not
//     own dead-letter replay.
//   - There is no HTTP endpoint for the rotation (along LG-241/242 and
//     LG-306): signing-key rotation is in-process structural config.
//   - Snapshots do not carry the secret: a restart restores the
//     LEDGER_AUDIT_SINK_SECRET env value, so the operator must update
//     the env var / secret store alongside the rotation for it to
//     persist; the rotation event itself stays in the audit trail for
//     traceability (along LG-305).
//   - This module deliberately adds no Ledger-level setter: the Ledger
//     never holds the sink (main.go's srv.auditSink is the sole
//     holder), so callers holding the *AuditSink rotate it directly —
//     the one deliberate departure from LG-300–305's Ledger delegate
//     shape, for a reason verified in the code (along LG-306).
//   - Explicitly out of scope: URL (endpoint identity — changing the
//     address is Stop + StartAuditSink), CursorFile (fixed at start),
//     PollInterval (the run loop's idle poll rhythm, not a signature),
//     and the four delivery knobs (LG-306 already owns them).
//
// Orthogonality: LG-67 owns the sink mechanism itself (polling,
// cursor, signing, dead-lettering — configurable only at start; this
// module is its signing key's runtime entry point), LG-306 owns the
// four delivery knobs (its entry explicitly deferred the secret to a
// later round — this module is that round), LG-305 owns audit-line
// signing-key rotation (audit-log rows; this module is the outbound
// webhook signature — a different component, a different key), LG-300
// owns the anchor Ed25519 signing key (the verifier side needs a
// retired-public-key history; this module is the signer side, atomic
// swap, no retirement table), LG-302 owns the TSA trust root (the
// verifier side's certificate; this module is the signer side's shared
// secret), webhook.go RotateKey owns the outbound webhook
// dispatcher's per-endpoint key ring (a multi-endpoint registry with
// key IDs and grace; this module is a single sink with no key ID and
// no grace), and LG-130/LG-308 own API keys (inbound auth, different
// credentials).

var (
	// ErrInvalidSinkSecret is returned by AuditSink.RotateSinkSecret
	// when the new secret is empty or nil. The live secret is left
	// untouched.
	ErrInvalidSinkSecret = errors.New("ledger: audit sink: invalid sink secret: secret must not be empty")
)

// sinkSecretFingerprintPrefixLen is the number of hex characters of a
// secret's SHA-256 kept in the rotation audit event for correlation —
// enough to match a rotation to a downstream key, never enough to
// reconstruct or verify against key material.
const sinkSecretFingerprintPrefixLen = 12

// sinkSecretSHA256Hex returns the SHA-256 hex of a sink signing
// secret — the fingerprint algorithm along LG-302's
// tsaTrustCertFingerprint (sha256.Sum256).
func sinkSecretSHA256Hex(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])
}

// sinkSecretFingerprintPrefix returns the first
// sinkSecretFingerprintPrefixLen hex characters of a secret's SHA-256,
// the only form in which a secret's identity ever enters the audit
// trail.
func sinkSecretFingerprintPrefix(secret []byte) string {
	return sinkSecretSHA256Hex(secret)[:sinkSecretFingerprintPrefixLen]
}

// RotateSinkSecret rotates the sink's HMAC signing secret in place.
// The cursor, the queue order, the counters, and the delivery policy
// are untouched; a delivery already in flight keeps the signature it
// computed at entry, and only the next delivery is signed with the new
// secret. The input is cloned, so the caller cannot later mutate the
// live key. An empty or nil secret is ErrInvalidSinkSecret with the
// live secret untouched; a secret byte-identical to the live one is a
// no-op that writes no audit event. Pre-provision the new secret at
// the downstream before calling: deliveries after the rotation are
// signed with the new key immediately.
func (s *AuditSink) RotateSinkSecret(newSecret []byte) error {
	return s.RotateSinkSecretCtx(context.Background(), newSecret)
}

// RotateSinkSecretCtx is RotateSinkSecret with per-call audit actor
// attribution (LG-73).
func (s *AuditSink) RotateSinkSecretCtx(ctx context.Context, newSecret []byte) error {
	s.cfgMu.Lock()
	previous := s.cfg.Secret
	if len(newSecret) == 0 {
		s.cfgMu.Unlock()
		return ErrInvalidSinkSecret
	}
	if bytes.Equal(newSecret, previous) {
		s.cfgMu.Unlock()
		return nil
	}
	previousPrefix := sinkSecretFingerprintPrefix(previous)
	s.cfg.Secret = bytes.Clone(newSecret)
	currentPrefix := sinkSecretFingerprintPrefix(s.cfg.Secret)
	s.cfgMu.Unlock()

	// Audit after the swap, under l.mu.RLock like deadLetter and
	// LG-306: cfgMu and l.mu are never held nested, so the lock order
	// is acyclic. emitAudit is a no-op when no audit log is attached,
	// and the event goes only to the audit log — never to the outbox,
	// so the sink cannot forward its own revision event.
	s.l.mu.RLock()
	s.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sink_secret_rotated",
		Actor:         "RotateSinkSecret",
		VersionBefore: s.l.version,
		VersionAfter:  s.l.version,
		Details: map[string]any{
			"previous_secret_sha256_prefix": previousPrefix,
			"secret_sha256_prefix":          currentPrefix,
		},
	})
	s.l.mu.RUnlock()
	return nil
}

// SinkSecretFingerprint returns the SHA-256 hex of the live sink
// signing secret. It is in-process only: the fingerprint is never
// written to disk, to the audit log, or to the outbox — only the
// 12-character prefix ever enters the audit trail, in the
// sink_secret_rotated event. It never equals the secret itself.
func (s *AuditSink) SinkSecretFingerprint() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return sinkSecretSHA256Hex(s.cfg.Secret)
}
