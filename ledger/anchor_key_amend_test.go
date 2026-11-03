package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Anchor signing-key runtime rotation (LG-300): the setter's
// fail-closed gates, the no-op, the retired-public-key table, restart
// recovery, concurrent rotation, operator attribution, and the audit
// trail's key-material boundary.

// anchorKeyIDOf returns the KeyID (first 8 public-key bytes, hex) of
// a private key.
func anchorKeyIDOf(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	pub := key.Public().(ed25519.PublicKey)
	return hex.EncodeToString(pub[:8])
}

// anchorKeyRotatedAuditEvents returns the anchor_key_rotated events in
// order, closing the audit log first (like the other amend tests).
func anchorKeyRotatedAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "anchor_key_rotated" {
			out = append(out, e)
		}
	}
	return out
}

// newAnchorKeyTestLedger builds a ledger with an audit log and a fresh
// random anchor key, returning the audit log too so tests can read the
// audit trail. The caller must not use the audit log after
// anchorKeyRotatedAuditEvents closes it.
func newAnchorKeyTestLedger(t *testing.T) (*Ledger, ed25519.PrivateKey, string, *AuditLog) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() }) // idempotent: safe after providerAuditEvents closes it
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al), WithAnchorKey(key))
	return l, key, dir, al
}

func TestSetAnchorKeyRejectsInvalidKey(t *testing.T) {
	l, key1, dir, _ := newAnchorKeyTestLedger(t)
	seedChain(t, l, 2)
	cp1, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	oldKeyID := cp1.KeyID

	corrupt := []byte(key1)
	corrupt[63] ^= 0xff // break the public half
	cases := map[string]ed25519.PrivateKey{
		"short key":       make([]byte, 32),
		"empty key":       nil,
		"corrupt pubhalf": corrupt,
	}
	for name, bad := range cases {
		if err := l.SetAnchorKey(bad); !errors.Is(err, ErrInvalidAnchorKey) {
			t.Errorf("SetAnchorKey(%s) = %v, want ErrInvalidAnchorKey", name, err)
		}
	}
	// The old key is untouched: anchoring still signs under it, the
	// live KeyID is unchanged, and no retired key was persisted.
	cp2, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() after rejected rotations = %v", err)
	}
	if cp2 != cp1 {
		t.Fatalf("Anchor() after rejected rotations = %+v, want unchanged %+v", cp2, cp1)
	}
	if st := l.AnchorStatus(); st.KeyID != oldKeyID {
		t.Fatalf("AnchorStatus().KeyID = %q, want %q (old key)", st.KeyID, oldKeyID)
	}
	if _, err := os.Stat(filepath.Join(dir, retiredAnchorKeysFileName)); !os.IsNotExist(err) {
		t.Fatalf("anchor_retired_keys.jsonl exists after rejected rotations (err=%v)", err)
	}
}

func TestSetAnchorKeySameKeyIsNoOp(t *testing.T) {
	l, key, dir, al := newAnchorKeyTestLedger(t)
	seedChain(t, l, 2)
	cp1, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	v := l.Version()

	if err := l.SetAnchorKey(key); err != nil {
		t.Fatalf("SetAnchorKey(same key) = %v, want nil", err)
	}
	// Re-installing the same key changes nothing: same live KeyID, no
	// new checkpoint, no version bump, no retired record, no audit
	// event.
	if st := l.AnchorStatus(); st.KeyID != cp1.KeyID {
		t.Fatalf("AnchorStatus().KeyID = %q, want %q", st.KeyID, cp1.KeyID)
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (no bump on no-op)", got, v)
	}
	cp2, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() after no-op = %v", err)
	}
	if cp2 != cp1 {
		t.Fatalf("Anchor() after no-op = %+v, want idempotent %+v", cp2, cp1)
	}
	if _, err := os.Stat(filepath.Join(dir, retiredAnchorKeysFileName)); !os.IsNotExist(err) {
		t.Fatalf("anchor_retired_keys.jsonl exists after no-op (err=%v)", err)
	}
	if events := anchorKeyRotatedAuditEvents(t, al); len(events) != 0 {
		t.Fatalf("anchor_key_rotated events = %d, want 0 (no-op writes no audit)", len(events))
	}
}

func TestSetAnchorKeyRotatesSigning(t *testing.T) {
	l, key1, _, _ := newAnchorKeyTestLedger(t)
	seedChain(t, l, 2)
	cp1, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := l.Version()

	if err := l.SetAnchorKey(key2); err != nil {
		t.Fatalf("SetAnchorKey = %v", err)
	}
	// The live KeyID follows the new key, with no version bump.
	st := l.AnchorStatus()
	if st.KeyID != anchorKeyIDOf(t, key2) {
		t.Fatalf("AnchorStatus().KeyID = %q, want new key %q", st.KeyID, anchorKeyIDOf(t, key2))
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (rotation bumps no version)", got, v)
	}
	// The same head under the new KeyID appends a fresh checkpoint:
	// the idempotency comparison includes the KeyID.
	cp2, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() after rotation = %v", err)
	}
	if cp2.Seq != cp1.Seq || cp2.HeadHash != cp1.HeadHash {
		t.Fatalf("anchor after rotation = %+v, want same seq/head as %+v", cp2, cp1)
	}
	if cp2.KeyID != anchorKeyIDOf(t, key2) {
		t.Fatalf("cp2.KeyID = %q, want new key %q", cp2.KeyID, anchorKeyIDOf(t, key2))
	}
	if cp2.Signature == cp1.Signature {
		t.Fatal("anchor after rotation reuses the old signature")
	}
	pub2 := key2.Public().(ed25519.PublicKey)
	var hh [32]byte
	hb, _ := hex.DecodeString(cp2.HeadHash)
	copy(hh[:], hb)
	sig, _ := hex.DecodeString(cp2.Signature)
	if !ed25519.Verify(pub2, anchorMessage(cp2.Seq, hh, cp2.AnchoredAt), sig) {
		t.Fatal("anchor after rotation does not verify under the new key")
	}
	cps := l.AnchorCheckpoints()
	if len(cps) != 2 || cps[0].KeyID != anchorKeyIDOf(t, key1) || cps[1].KeyID != anchorKeyIDOf(t, key2) {
		t.Fatalf("checkpoints = %v, want [old new] KeyIDs", cps)
	}
}

func TestSetAnchorKeyOldCheckpointsStillVerify(t *testing.T) {
	l, key1, _, _ := newAnchorKeyTestLedger(t)
	seedChain(t, l, 2)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetAnchorKey(key2); err != nil {
		t.Fatalf("SetAnchorKey = %v", err)
	}
	// The old checkpoint verifies against the retired public key even
	// before any new anchor is taken.
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err != nil {
		t.Fatalf("verifyAnchorsLocked after rotation = %v", err)
	}
	if st := l.AnchorStatus(); !st.Verified || st.Error != "" {
		t.Fatalf("AnchorStatus() = %+v, want verified across rotation", st)
	}
	rep := l.Reconcile(time.Now())
	if !rep.AuditChain.AnchorOK || rep.AuditChain.AnchorError != "" {
		t.Fatalf("Reconcile anchor health = %+v, want ok", rep.AuditChain)
	}
	// After a new anchor under the new key, both checkpoints verify,
	// the old one under its retired public key.
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() after rotation = %v", err)
	}
	cps := l.AnchorCheckpoints()
	if len(cps) != 2 || cps[0].KeyID != anchorKeyIDOf(t, key1) || cps[1].KeyID != anchorKeyIDOf(t, key2) {
		t.Fatalf("checkpoints = %v, want [old new] KeyIDs", cps)
	}
	if st := l.AnchorStatus(); !st.Verified || st.Checkpoints != 2 {
		t.Fatalf("AnchorStatus() = %+v, want 2 checkpoints verified", st)
	}
	// Fail-closed is preserved: a checkpoint naming an unknown KeyID
	// still fails, in the pre-rotation error shape.
	l.anchor.mu.Lock()
	cp := l.anchor.checkpoints[0]
	cp.KeyID = "deadbeefdeadbeef"
	l.anchor.checkpoints[0] = cp
	l.anchor.mu.Unlock()
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err == nil || !strings.Contains(err.Error(), "is not this ledger's anchor key") {
		t.Fatalf("verifyAnchorsLocked(unknown KeyID) = %v, want fail-closed key_id error", err)
	}
}

func TestSetAnchorKeyRetiredKeysSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, key1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al), WithAnchorKey(key1))
	seedChain(t, l, 2)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	if err := l.SetAnchorKey(key2); err != nil {
		t.Fatalf("SetAnchorKey = %v", err)
	}
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() after rotation = %v", err)
	}
	al.Close()

	// The retired public key is on disk: the old KeyID, the full
	// public key, a retirement timestamp — and no private material.
	raw, err := os.ReadFile(filepath.Join(dir, retiredAnchorKeysFileName))
	if err != nil {
		t.Fatalf("read %s: %v", retiredAnchorKeysFileName, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s has %d lines, want 1", retiredAnchorKeysFileName, len(lines))
	}
	var rk retiredAnchorKey
	if err := json.Unmarshal([]byte(lines[0]), &rk); err != nil {
		t.Fatalf("retired line unmarshal: %v", err)
	}
	if rk.KeyID != anchorKeyIDOf(t, key1) {
		t.Fatalf("retired KeyID = %q, want %q", rk.KeyID, anchorKeyIDOf(t, key1))
	}
	if rk.PublicKeyHex != hex.EncodeToString(key1.Public().(ed25519.PublicKey)) {
		t.Fatal("retired public key does not match the old key")
	}
	if rk.RetiredAt.IsZero() {
		t.Fatal("retired_at is zero")
	}

	// A fresh ledger on the same directory, live on the new key,
	// recovers the retired table and still verifies the old
	// checkpoint.
	al2, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer al2.Close()
	l2 := New(WithAuditLog(al2), WithAnchorKey(key2))
	seedChain(t, l2, 2) // rebuild the same chain prefix
	st := l2.AnchorStatus()
	if st.Checkpoints != 2 || !st.Verified || st.Error != "" {
		t.Fatalf("reloaded AnchorStatus() = %+v, want 2 checkpoints verified", st)
	}
	if st.KeyID != anchorKeyIDOf(t, key2) {
		t.Fatalf("reloaded AnchorStatus().KeyID = %q, want live key %q", st.KeyID, anchorKeyIDOf(t, key2))
	}
	l2.mu.RLock()
	err = l2.verifyAnchorsLocked()
	l2.mu.RUnlock()
	if err != nil {
		t.Fatalf("reloaded verifyAnchorsLocked = %v", err)
	}
}

func TestSetAnchorKeyConcurrentAnchor(t *testing.T) {
	l, key1, _, _ := newAnchorKeyTestLedger(t)
	seedChain(t, l, 4)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, key3, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		anchorKeyIDOf(t, key1): true,
		anchorKeyIDOf(t, key2): true,
		anchorKeyIDOf(t, key3): true,
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	rotate := func(key ed25519.PrivateKey) {
		defer wg.Done()
		if err := l.SetAnchorKey(key); err != nil {
			errs <- err
		}
	}
	anchor := func() {
		defer wg.Done()
		if _, err := l.Anchor(); err != nil {
			errs <- err
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go anchor()
		wg.Add(1)
		go rotate(key2)
		wg.Add(1)
		go rotate(key3)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Anchor/SetAnchorKey = %v", err)
	}
	// Every checkpoint names a key that was live or retired, so the
	// whole history still verifies; the live KeyID is one of the
	// rotation targets.
	for _, cp := range l.AnchorCheckpoints() {
		if !allowed[cp.KeyID] {
			t.Fatalf("checkpoint KeyID %q not in {key1,key2,key3}", cp.KeyID)
		}
	}
	st := l.AnchorStatus()
	if !st.Verified || st.Error != "" {
		t.Fatalf("AnchorStatus() after concurrent rotation = %+v, want verified", st)
	}
	if !allowed[st.KeyID] || st.KeyID == anchorKeyIDOf(t, key1) {
		t.Fatalf("AnchorStatus().KeyID = %q, want key2 or key3", st.KeyID)
	}
}

func TestSetAnchorKeyCtxActor(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, key1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al), WithAnchorKey(key1))
	ctx := ContextWithActor(context.Background(), "ops-ana")
	if err := l.SetAnchorKeyCtx(ctx, key2); err != nil {
		t.Fatalf("SetAnchorKeyCtx = %v", err)
	}
	events := anchorKeyRotatedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("anchor_key_rotated events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "ops-ana" {
		t.Fatalf("audit Actor = %q, want %q (LG-73 operator)", ev.Actor, "ops-ana")
	}
	if ev.Details["previous_key_id"] != anchorKeyIDOf(t, key1) {
		t.Fatalf("previous_key_id = %v, want %q", ev.Details["previous_key_id"], anchorKeyIDOf(t, key1))
	}
	if ev.Details["key_id"] != anchorKeyIDOf(t, key2) {
		t.Fatalf("key_id = %v, want %q", ev.Details["key_id"], anchorKeyIDOf(t, key2))
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("audit versions = %d->%d, want equal (no version bump)", ev.VersionBefore, ev.VersionAfter)
	}

	// The plain variant attributes the entrypoint name.
	dir2 := t.TempDir()
	al2, err := NewAuditLog(dir2)
	if err != nil {
		t.Fatal(err)
	}
	l2 := New(WithAuditLog(al2), WithAnchorKey(key1))
	if err := l2.SetAnchorKey(key2); err != nil {
		t.Fatalf("SetAnchorKey = %v", err)
	}
	events2 := anchorKeyRotatedAuditEvents(t, al2)
	if len(events2) != 1 || events2[0].Actor != "SetAnchorKey" {
		t.Fatalf("plain-variant audit events = %+v, want one with Actor SetAnchorKey", events2)
	}
}

func TestSetAnchorKeyAuditHasNoKeyMaterial(t *testing.T) {
	l, key1, dir, al := newAnchorKeyTestLedger(t)
	_, key2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetAnchorKey(key2); err != nil {
		t.Fatalf("SetAnchorKey = %v", err)
	}
	events := anchorKeyRotatedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("anchor_key_rotated events = %d, want 1", len(events))
	}
	ev := events[0]
	// The audit detail carries exactly the two KeyIDs — nothing else,
	// and in particular no private key material and no public key.
	if len(ev.Details) != 2 {
		t.Fatalf("audit Details keys = %v, want exactly [previous_key_id key_id]", ev.Details)
	}
	for _, k := range []string{"previous_key_id", "key_id"} {
		v, ok := ev.Details[k].(string)
		if !ok || len(v) != 16 {
			t.Fatalf("audit detail %q = %v, want a 16-char KeyID", k, ev.Details[k])
		}
	}
	raw, err := json.Marshal(ev.Details)
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{
		"old private key": hex.EncodeToString(key1),
		"new private key": hex.EncodeToString(key2),
		"old public key":  hex.EncodeToString(key1.Public().(ed25519.PublicKey)),
		"new public key":  hex.EncodeToString(key2.Public().(ed25519.PublicKey)),
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("audit detail contains the %s", name)
		}
	}
	// The boundary is the audit log, not the retired-keys file: the
	// old public key belongs there by design (it is what lets old
	// checkpoints verify), and the private key belongs nowhere.
	retired, err := os.ReadFile(filepath.Join(dir, retiredAnchorKeysFileName))
	if err != nil {
		t.Fatalf("read %s: %v", retiredAnchorKeysFileName, err)
	}
	if !strings.Contains(string(retired), hex.EncodeToString(key1.Public().(ed25519.PublicKey))) {
		t.Fatal("retired file lacks the old public key")
	}
	if strings.Contains(string(retired), hex.EncodeToString(key1)) {
		t.Fatal("retired file contains private key material")
	}
}

func TestSetAnchorKeyOnUnconfiguredLedger(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	l := New(WithAuditLog(al)) // no anchor key
	if _, err := l.Anchor(); err != ErrAnchorKeyNotConfigured {
		t.Fatalf("Anchor() without key = %v, want ErrAnchorKeyNotConfigured", err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetAnchorKey(key); err != nil {
		t.Fatalf("SetAnchorKey on unconfigured ledger = %v", err)
	}
	seedChain(t, l, 2)
	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() after installing key = %v", err)
	}
	if cp.KeyID != anchorKeyIDOf(t, key) {
		t.Fatalf("cp.KeyID = %q, want %q", cp.KeyID, anchorKeyIDOf(t, key))
	}
	if st := l.AnchorStatus(); !st.Configured || !st.Verified {
		t.Fatalf("AnchorStatus() = %+v, want configured and verified", st)
	}
	// No previous key, so nothing was retired.
	if _, err := os.Stat(filepath.Join(dir, retiredAnchorKeysFileName)); !os.IsNotExist(err) {
		t.Fatalf("anchor_retired_keys.jsonl exists after first install (err=%v)", err)
	}
}
