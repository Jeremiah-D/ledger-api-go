package ledger

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Anchor signing-key runtime rotation (LG-300).
//
// WithAnchorKey (anchor.go) installs the Ed25519 signing key at
// construction — validated fail-fast, panicking on a malformed key —
// and this module is the orthogonal runtime path: an operator rotates
// the key in place, instead of rebuilding the ledger and replaying its
// journal. Rotating in place matters because a rebuild is a new ledger
// — a gap in which the old key (or none) signs anchors, plus a fresh
// identity that breaks the audit story — while a rotation keeps the
// same ledger, the same journal, and the same audit trail from old key
// to new.
//
// The naive rotation breaks history: verifyAnchorsLocked used to
// require every checkpoint's KeyID to equal the live key, so swapping
// the key would fail every checkpoint signed before the rotation, and
// AnchorStatus and the Reconcile anchor health would report failure
// with it. This module closes that gap with a retired-public-key
// table:
//
//   - On rotation the old key's public key — public key only, never
//     private key material — is recorded in the retired table and
//     persisted to anchor_retired_keys.jsonl in the audit directory,
//     next to checkpoints.jsonl, before the live key is swapped. A
//     crash between the persist and the swap leaves only a harmless
//     orphan record: a retired key nobody signed under.
//   - verifyAnchorsLocked resolves each checkpoint's KeyID against the
//     live key first, then the retired table, and still fails closed
//     on an unknown KeyID, in the same error shape as before.
//   - The retired table loads lazily with the checkpoints
//     (ensureCheckpointsLoaded), so a restart recovers it from the
//     audit directory and old checkpoints keep verifying. A ledger
//     without an audit directory keeps the table in memory only —
//     consistent with checkpoints, which are not durable there
//     either.
//   - The key itself never leaves the process: it is not written to
//     snapshots, checkpoints, the retired-keys file, or the audit log
//     — only KeyIDs (public-key prefixes) and retired public keys are
//     recorded. Rotating back to a previously retired key is legal:
//     the live key wins the lookup, and the stale retired entry is
//     harmless.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Validation is strict, like construction but without the panic:
//     the key must be 64 bytes (ed25519.PrivateKeySize) and reuse
//     ParseAnchorKey's public-half/seed-half consistency check. An
//     illegal key is rejected with ErrInvalidAnchorKey and the old
//     key is left exactly as it was — the runtime path never panics,
//     unlike construction-time WithAnchorKey.
//   - Rotating to a key whose KeyID equals the live KeyID is a no-op:
//     it returns nil and writes no audit event, so a retried or
//     duplicated rotation cannot spam the audit trail.
//   - The rotation governs only later Anchor() calls: Anchor signs
//     with the live key, so the same head under a new KeyID appends
//     one fresh checkpoint (the idempotency comparison includes the
//     KeyID), while old checkpoints keep their old KeyID and keep
//     verifying against the retired public key. verifyAnchorsLocked
//     permits the same seq twice when — and only when — the KeyID
//     differs (a rotation re-attestation); a duplicated line under
//     the same KeyID still fails. AnchorStatus().KeyID reports the
//     live key, and Verified stays true across the rotation.
//   - Rotating an unconfigured ledger installs the key: Anchor()
//     becomes usable afterwards.
//   - The rotation is key management, not a ledger event: it bumps
//     neither the ledger version nor the audit chain, and no journal
//     entry or outbox event fires. It lands one anchor_key_rotated
//     audit event carrying previous_key_id and key_id — KeyIDs only,
//     never key material. The Ctx variant attributes the operator
//     (LG-73).
//
// Lock order: the setter holds l.mu, then anchorState.mu, swapping
// key, KeyID, and the retired table atomically under anchorState.mu
// (see the lock-order note on verifyAnchorsLocked). Anchor's empty-key
// check moved inside anchorState.mu for the same reason: reading the
// live key outside the mutex would race the rotation.
//
// Orthogonality: WithAnchorKey owns construction-time install, Anchor
// owns signing, verifyAnchorsLocked owns verification, LG-46 owns the
// reconcile-report signature export (which reuses this key system, so
// its key_id follows the new key after a rotation), and this module
// owns only the runtime rotation and the history it leaves behind.
// There is no HTTP endpoint for the rotation (per LG-241/242): the
// signing key is in-process operator config.

// ErrInvalidAnchorKey is returned by SetAnchorKey when the candidate
// key is not a well-formed 64-byte Ed25519 private key (wrong length,
// or a public half that does not match the seed half). The previous
// key is left untouched.
var ErrInvalidAnchorKey = errors.New("ledger: invalid anchor key")

// retiredAnchorKeysFileName is the retired-public-key journal's name
// inside the audit directory (LEDGER_AUDIT_DIR).
const retiredAnchorKeysFileName = "anchor_retired_keys.jsonl"

// retiredAnchorKey is one rotated-out anchor public key: the KeyID it
// signed under, the full public key (hex), and when it was retired.
// Private key material is never recorded here.
type retiredAnchorKey struct {
	// KeyID identifies the retired signing key: hex of the first 8
	// bytes of its Ed25519 public key — the same KeyID checkpoints
	// carry.
	KeyID string `json:"key_id"`
	// PublicKeyHex is the hex of the full 32-byte Ed25519 public key.
	PublicKeyHex string `json:"public_key_hex"`
	// RetiredAt is when the key was rotated out, UTC.
	RetiredAt time.Time `json:"retired_at"`
}

// loadRetiredAnchorKeys reads anchor_retired_keys.jsonl, skipping
// corrupt lines: a torn write can only ever corrupt its own line, and
// a partially written tail must not wedge the ledger at startup. A
// missing file is not an error — no key has been rotated yet. A
// skipped line means the checkpoint that key signed may stop
// verifying, which verifyAnchorsLocked surfaces fail-closed.
func loadRetiredAnchorKeys(path string) []retiredAnchorKey {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	var out []retiredAnchorKey
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rk retiredAnchorKey
		if err := json.Unmarshal(line, &rk); err != nil || !validRetiredAnchorKeyShape(rk) {
			continue
		}
		out = append(out, rk)
	}
	return out
}

// validRetiredAnchorKeyShape rejects retired-key records that cannot
// possibly verify a checkpoint: wrong-length IDs or keys, non-hex, or
// a zero timestamp.
func validRetiredAnchorKeyShape(rk retiredAnchorKey) bool {
	if len(rk.KeyID) != 16 || len(rk.PublicKeyHex) != 64 {
		return false
	}
	if _, err := hex.DecodeString(rk.KeyID); err != nil {
		return false
	}
	if _, err := hex.DecodeString(rk.PublicKeyHex); err != nil {
		return false
	}
	return !rk.RetiredAt.IsZero()
}

// appendRetiredKeyLine appends one retired public key to the journal
// and fsyncs before returning: the persist must land before the live
// key is swapped, so a crash can only ever leave a harmless orphan
// record, never a checkpoint whose key is lost.
func appendRetiredKeyLine(dir string, rk retiredAnchorKey) error {
	line, err := json.Marshal(rk)
	if err != nil {
		return fmt.Errorf("ledger: retired anchor key marshal: %w", err)
	}
	line = append(line, '\n')
	fh, err := os.OpenFile(filepath.Join(dir, retiredAnchorKeysFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: retired anchor key open: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(line); err != nil {
		return fmt.Errorf("ledger: retired anchor key write: %w", err)
	}
	if err := fh.Sync(); err != nil {
		return fmt.Errorf("ledger: retired anchor key sync: %w", err)
	}
	return nil
}

// anchorPublicKeyFor resolves a checkpoint's KeyID to the public key
// that must verify it: the live key first, then the retired table. A
// checkpoint naming an unknown KeyID resolves to nothing and its
// caller fails closed.
func anchorPublicKeyFor(liveKey ed25519.PrivateKey, liveKeyID string, retired []retiredAnchorKey, keyID string) (ed25519.PublicKey, bool) {
	if keyID == liveKeyID {
		return liveKey.Public().(ed25519.PublicKey), true
	}
	for _, rk := range retired {
		if rk.KeyID != keyID {
			continue
		}
		raw, err := hex.DecodeString(rk.PublicKeyHex)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, false
		}
		return ed25519.PublicKey(raw), true
	}
	return nil, false
}

// validateAnchorKeyForRotation checks a rotation candidate the way
// construction does, but returns an error instead of panicking: 64
// bytes, with the public half matching the seed half (reusing
// ParseAnchorKey). The derived public key is returned for the KeyID.
func validateAnchorKeyForRotation(key ed25519.PrivateKey) (ed25519.PublicKey, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: want a 64-byte ed25519 private key, got %d bytes", ErrInvalidAnchorKey, len(key))
	}
	if _, err := ParseAnchorKey(hex.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAnchorKey, err)
	}
	return key.Public().(ed25519.PublicKey), nil
}

// SetAnchorKey rotates the anchor signing key at runtime. Later
// Anchor() calls sign with the new key; checkpoints signed under the
// old key keep verifying against the retired public key, which is
// persisted to anchor_retired_keys.jsonl before the live key swaps.
// An illegal key is rejected with ErrInvalidAnchorKey and the old key
// is left untouched. Rotating to the live KeyID is a no-op with no
// audit event. Rotating an unconfigured ledger installs the key. The
// rotation bumps neither the ledger version nor the audit chain, and
// fires no journal entry or outbox event; it lands one
// anchor_key_rotated audit event carrying KeyIDs only.
func (l *Ledger) SetAnchorKey(key ed25519.PrivateKey) error {
	return l.SetAnchorKeyCtx(context.Background(), key)
}

// SetAnchorKeyCtx is SetAnchorKey with per-call audit actor attribution
// (LG-73).
func (l *Ledger) SetAnchorKeyCtx(ctx context.Context, key ed25519.PrivateKey) error {
	pub, err := validateAnchorKeyForRotation(key)
	if err != nil {
		return err
	}
	newKeyID := hex.EncodeToString(pub[:8])
	l.ensureCheckpointsLoaded()
	l.mu.Lock()
	defer l.mu.Unlock()
	a := &l.anchor
	a.mu.Lock()
	defer a.mu.Unlock()
	if newKeyID == a.keyID {
		return nil
	}
	previousKeyID := ""
	if len(a.key) > 0 {
		previousKeyID = a.keyID
		oldPub := a.key.Public().(ed25519.PublicKey)
		rk := retiredAnchorKey{
			KeyID:        a.keyID,
			PublicKeyHex: hex.EncodeToString(oldPub),
			RetiredAt:    time.Now().UTC(),
		}
		// Persist the retired public key before swapping the live
		// key: a crash in between leaves only a harmless orphan
		// record. A failed persist rejects the rotation and the old
		// key stays live. A ledger without an audit directory keeps
		// the retired key in memory only, like its checkpoints.
		if dir, ok := l.AuditDir(); ok {
			if err := appendRetiredKeyLine(dir, rk); err != nil {
				return err
			}
		}
		a.retired = append(a.retired, rk)
	}
	a.key = ed25519.PrivateKey(bytes.Clone(key))
	a.keyID = newKeyID
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "anchor_key_rotated",
		Actor:         "SetAnchorKey",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_key_id": previousKeyID,
			"key_id":          newKeyID,
		},
	})
	return nil
}
