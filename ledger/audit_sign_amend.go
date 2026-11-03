package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audit-log row-level HMAC signing key runtime rotation (LG-305).
//
// WithAuditLogSigning (audit.go) and LEDGER_AUDIT_SIGNING_KEYS
// (main.go) configure the signing key set only at construction — the
// set was parsed once at startup and the signer built from it is
// immutable (audit_sign.go), so rotating a leaked or ageing sealing
// key meant restarting the process: closing the old AuditLog and
// rebuilding with a new env key set, a gap in which the old keys
// govern plus a fresh process identity that breaks the audit story.
// This module is the orthogonal runtime path: an operator installs a
// new rotation set in place, with no downtime, instead of rebuilding
// the ledger.
//
// Semantics, deliberately fail-closed throughout, along the LG-301
// retune pattern (audit_log_retune.go):
//
//   - Race safety is the point. The background writer (audit.go run)
//     and Signed() used to read a.signer lock-free; both now snapshot
//     it under signMu.RLock (see snapshotSigner), and rotation swaps
//     it under signMu.Lock. A line already being flushed keeps the
//     signer snapshot it started with, so rotation only affects rows
//     flushed afterwards (future-only, along LG-288/LG-301). signMu is
//     its own mutex — not chainMu (hash-chain sealing) and not
//     LG-301's cfgMu (maxBytes/retentionDays) — because signing
//     rotation is an independent concern.
//   - Validate-then-install: the new set is fully validated before
//     anything is swapped, so a rejected rotation leaves the old key
//     set untouched and the runtime path never panics (unlike the
//     construction-time fail-fast). An empty set is rejected — closing
//     signing is a separate dangerous concern left to the restart
//     path. An all-retired set is rejected — newAuditSigner would
//     silently fall back to signing with the last listed retired key,
//     which at runtime is an operator error, not a config to honour.
//     Duplicate ids, empty ids and empty key material are rejected. A
//     key id that already exists in the live set with different key
//     bytes is rejected (kid confusion fails closed). All of these
//     return ErrInvalidAuditSigningKeys.
//   - Keys in the new set that already carry RetiredAt keep the passed
//     value (ParseAuditSigningKeys @retiredAt semantics). Old keys
//     absent from the new set are automatically appended as retired
//     keys with RetiredAt = rotation time (already-retired keys keep
//     their original RetiredAt) and carried into the new signer — so
//     offline VerifyAuditSignatures with the full set (new + retired)
//     still verifies all history rows: verifyReason only rejects rows
//     written after RetiredAt+grace, and old rows predate retirement.
//   - Rotating to a set whose effective installed set is identical to
//     the incumbent (same id, same key bytes, same retired state,
//     item by item, including the auto-appended retired keys) is a
//     no-op: it returns nil and writes no audit event, so a retried
//     or duplicated rotation cannot spam the audit trail.
//   - An unsigned ledger rotates into signing: rotation installs keys,
//     Signed() flips true, subsequent rows are signed, and old
//     unsigned rows are still only counted, not failed, by
//     VerifyAuditSignatures. The rotation event itself is emitted
//     after installing the new keys, so it is signed by the new
//     primary.
//   - A ledger with no audit log attached (WithAuditLog was never
//     used) rejects rotation with ErrAuditLogNotConfigured (along
//     LG-301).
//   - The rotation is audited as audit_signing_keys_rotated carrying
//     previous_key_ids / new_key_ids / retired_key_ids — key ids only,
//     never key material. The Ctx variant attributes the operator
//     (LG-73) via l.emitAuditCtx. Lock order is l.mu -> a.signMu,
//     along SetAuditLogMaxBytesCtx: the Ledger setter holds l.mu,
//     calls the AuditLog-side setter, then emits; the writer takes
//     only signMu, never l.mu.
//   - Rotation affects only subsequent signing: no version bump, no
//     journal entry, no outbox event, no hash-chain touch (chain
//     sealing lives under chainMu in Log, independent of signing).
//     There is no HTTP endpoint for rotation (per LG-241/242).
//   - Snapshots carry no keys (AuditLog lives outside the ledger
//     snapshot): a restart restores the env values, while the
//     rotation event itself stays in the audit trail for traceability
//     — the operator must sync LEDGER_AUDIT_SIGNING_KEYS so the
//     rotation persists across restarts.
//   - Honest boundaries: offline VerifyAuditSignatures takes an
//     explicit key set — the operator must assemble the full set from
//     the three id groups in the rotation event; passing only the new
//     keys makes old rows report unknown_key. GET /audit/export's
//     manifest signature still reads the env keys (main.go) and is out
//     of scope; GET /audit/verify is hash-chain verification and is
//     unaffected.
//
// Orthogonality: WithAuditLogSigning owns construction-time install,
// LG-65 owns the signing mechanism itself (rotation-set semantics,
// retired+grace, kid header, offline verifier — only consumable at
// construction before this item, which is its runtime entrypoint),
// LG-32 owns hash-chain sealing, LG-301 owns maxBytes/retentionDays
// revision (which explicitly named signing as an independent concern —
// this module is that concern), LG-128 owns export manifest signing
// (explicit key set, unaffected), and LG-300/302 own persisted
// retirement material (public keys/certificates — key material here
// never lands on disk; retired keys live only in the in-memory
// signer).

var (
	// ErrInvalidAuditSigningKeys is returned by
	// RotateAuditSigningKeys when the requested rotation set fails
	// validation: empty set, all-retired set, duplicate id, empty id,
	// empty key material, or a key id that already exists in the live
	// set with different key bytes. The old key set is left untouched.
	ErrInvalidAuditSigningKeys = errors.New("ledger: invalid audit signing key set")
)

// validateAuditSigningKeys checks the input-only gates of a rotation
// set, without reference to the live set: non-empty, at least one
// active (non-retired) key, unique non-empty ids, non-empty key
// material. The live-set comparison (same id, different key bytes)
// happens in rotateSigningKeys under signMu.
func validateAuditSigningKeys(keys []AuditSigningKey) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: empty set (closing signing is restart-only)", ErrInvalidAuditSigningKeys)
	}
	seen := make(map[string]bool, len(keys))
	allRetired := true
	for _, k := range keys {
		if strings.TrimSpace(k.ID) == "" {
			return fmt.Errorf("%w: empty key id", ErrInvalidAuditSigningKeys)
		}
		if seen[k.ID] {
			return fmt.Errorf("%w: duplicate key id %q", ErrInvalidAuditSigningKeys, k.ID)
		}
		seen[k.ID] = true
		if len(k.Key) == 0 {
			return fmt.Errorf("%w: key %q has empty key material", ErrInvalidAuditSigningKeys, k.ID)
		}
		if k.RetiredAt.IsZero() {
			allRetired = false
		}
	}
	if allRetired {
		return fmt.Errorf("%w: all keys retired; at least one active key is required", ErrInvalidAuditSigningKeys)
	}
	return nil
}

// copyAuditSigningKeys deep-copies a rotation set (including key
// material) so the live signer can never alias the caller's slice.
func copyAuditSigningKeys(keys []AuditSigningKey) []AuditSigningKey {
	if keys == nil {
		return nil
	}
	out := make([]AuditSigningKey, len(keys))
	for i, k := range keys {
		out[i] = k
		if k.Key != nil {
			out[i].Key = append([]byte(nil), k.Key...)
		}
	}
	return out
}

// auditSigningKeysEqual reports whether two rotation sets are identical
// item by item: same order, same id, same key bytes, same retired
// state (RetiredAt equal). It is the no-op test for rotation.
func auditSigningKeysEqual(a, b []AuditSigningKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || !bytes.Equal(a[i].Key, b[i].Key) || !a[i].RetiredAt.Equal(b[i].RetiredAt) {
			return false
		}
	}
	return true
}

// auditSigningKeyIDs returns the ids of keys in order, ids only —
// never key material.
func auditSigningKeyIDs(keys []AuditSigningKey) []string {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.ID)
	}
	return ids
}

// snapshotSigner returns the live signer under signMu.RLock. The
// background writer signs each flushed line with the snapshot it took,
// so a rotation mid-flush cannot change the key under a line —
// rotation only affects rows flushed afterwards.
func (a *AuditLog) snapshotSigner() *auditSigner {
	a.signMu.RLock()
	defer a.signMu.RUnlock()
	return a.signer
}

// rotateSigningKeys installs a new rotation set and reports the
// previous key ids, the new key ids, the key ids this rotation retired,
// and whether anything changed. Validation runs before the swap, so a
// rejected set leaves the live signer untouched; a no-op (effective
// installed set identical to the incumbent) reports changed=false and
// installs nothing. Old keys absent from the new set are appended as
// retired keys with RetiredAt = rotation time (already-retired keys
// keep their original RetiredAt), so history rows keep verifying
// against the full set.
func (a *AuditLog) rotateSigningKeys(keys []AuditSigningKey) (previous, newIDs, retired []string, changed bool, err error) {
	if err := validateAuditSigningKeys(keys); err != nil {
		return nil, nil, nil, false, err
	}
	now := time.Now().UTC()
	a.signMu.Lock()
	defer a.signMu.Unlock()
	previous = auditSigningKeyIDs(a.signingKeys)
	// Same id as the live set but different key bytes: kid confusion
	// fails closed — the operator must retire the old key under a new
	// id or pass the identical material.
	live := make(map[string]AuditSigningKey, len(a.signingKeys))
	for _, k := range a.signingKeys {
		live[k.ID] = k
	}
	for _, k := range keys {
		if cur, ok := live[k.ID]; ok && !bytes.Equal(cur.Key, k.Key) {
			return nil, nil, nil, false, fmt.Errorf("%w: key %q already exists with different key material", ErrInvalidAuditSigningKeys, k.ID)
		}
	}
	// Build the installed set: the new set as passed (keys already
	// carrying RetiredAt keep the passed value), then every old key
	// absent from the new set appended as retired.
	newIDs = auditSigningKeyIDs(keys)
	inNew := make(map[string]bool, len(keys))
	for _, k := range keys {
		inNew[k.ID] = true
	}
	installed := copyAuditSigningKeys(keys)
	for _, old := range a.signingKeys {
		if inNew[old.ID] {
			continue
		}
		c := old
		if c.RetiredAt.IsZero() {
			c.RetiredAt = now
		}
		if c.Key != nil {
			c.Key = append([]byte(nil), c.Key...)
		}
		installed = append(installed, c)
		retired = append(retired, old.ID)
	}
	if auditSigningKeysEqual(a.signingKeys, installed) {
		// No-op: the effective installed set is unchanged. Report
		// the groups for the caller's record, but install nothing
		// and let the Ledger side skip the audit event.
		return previous, newIDs, retired, false, nil
	}
	signer, err := newAuditSigner(installed, a.signingGrace)
	if err != nil {
		// Unreachable after validation (the set is non-empty with an
		// active key), but fail closed: never install a signer the
		// constructor would reject.
		return nil, nil, nil, false, err
	}
	a.signer = signer
	a.signingKeys = installed
	return previous, newIDs, retired, true, nil
}

// SigningKeyIDs reports the installed rotation set's key ids — primary
// first, then the remaining ids in installed order — ids only, never
// key material. An unsigned log reports an empty (non-nil) slice.
func (a *AuditLog) SigningKeyIDs() []string {
	a.signMu.RLock()
	defer a.signMu.RUnlock()
	// Primary first: the first non-retired key in installed order is
	// the one signing new lines; the rest follow in installed order
	// (active standbys, then retired keys).
	ids := make([]string, 0, len(a.signingKeys))
	primary := -1
	for i, k := range a.signingKeys {
		if k.RetiredAt.IsZero() {
			primary = i
			break
		}
	}
	if primary >= 0 {
		ids = append(ids, a.signingKeys[primary].ID)
	}
	for i, k := range a.signingKeys {
		if i != primary {
			ids = append(ids, k.ID)
		}
	}
	return ids
}

// RotateAuditSigningKeys installs a new audit-log signing rotation set
// at runtime, with no downtime: the first non-retired key of the new
// set signs subsequent lines, old keys absent from the new set are
// carried as retired keys so history rows keep verifying, and the
// rotation is audited as audit_signing_keys_rotated. An invalid set is
// rejected with ErrInvalidAuditSigningKeys and the old set is left
// untouched; rotating to the current set is a no-op with no audit
// event. A ledger with no audit log attached returns
// ErrAuditLogNotConfigured. The operator must sync
// LEDGER_AUDIT_SIGNING_KEYS for the rotation to survive a restart.
func (l *Ledger) RotateAuditSigningKeys(keys []AuditSigningKey) error {
	return l.RotateAuditSigningKeysCtx(context.Background(), keys)
}

// RotateAuditSigningKeysCtx is RotateAuditSigningKeys with per-call
// audit actor attribution (LG-73).
func (l *Ledger) RotateAuditSigningKeysCtx(ctx context.Context, keys []AuditSigningKey) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := validateAuditSigningKeys(keys); err != nil {
		return err
	}
	if l.audit == nil {
		return ErrAuditLogNotConfigured
	}
	previous, newIDs, retiredIDs, changed, err := l.audit.rotateSigningKeys(keys)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	// Emitted after installing the new keys, so the rotation event
	// itself is signed by the new primary.
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "audit_signing_keys_rotated",
		Actor:         "RotateAuditSigningKeys",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_key_ids": previous,
			"new_key_ids":      newIDs,
			"retired_key_ids":  retiredIDs,
		},
	})
	return nil
}

// AuditSigningKeyIDs reports the audit log's installed signing key ids
// — primary first, then the remaining ids in installed order — ids
// only, never key material. Enabled is false when no audit log is
// attached (WithAuditLog was never used); an attached but unsigned log
// reports an empty slice with enabled true. Follows the AuditStats
// enabled shape.
func (l *Ledger) AuditSigningKeyIDs() ([]string, bool) {
	if l.audit == nil {
		return nil, false
	}
	return l.audit.SigningKeyIDs(), true
}
