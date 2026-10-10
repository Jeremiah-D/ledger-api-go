package ledger

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AnchorCheckpoint is one external anchoring of the audit chain head
// (LG-44): the operator's Ed25519 signature over the chain's (seq,
// headHash) at anchor time, appended as one JSON line to
// checkpoints.jsonl in the audit directory. An anchor is a
// tamper-evident witness a third party can check without trusting the
// ledger's storage: the signature proves the key holder saw exactly this
// chain head at exactly this time, and VerifyAnchors replays every
// checkpoint against the live chain — a rewritten journal breaks the
// head-hash link, a reordered checkpoint file breaks seq monotonicity,
// and a forged checkpoint breaks the signature.
//
// The checkpoints file is append-only and independent of the journal
// snapshots: it survives snapshot export/import untouched, and a ledger
// restored from a snapshot re-verifies its on-disk checkpoints against
// the rebuilt chain.
type AnchorCheckpoint struct {
	// Seq is the chain length at anchor time (1-based link count; 0
	// anchors the genesis head of an empty ledger).
	Seq uint64 `json:"seq"`
	// HeadHash is the hex SHA-256 of the chain head link at Seq (the
	// 32-zero-byte genesis hash when Seq is 0).
	HeadHash string `json:"head_hash"`
	// AnchoredAt is when the signature was made, UTC.
	AnchoredAt time.Time `json:"anchored_at"`
	// KeyID identifies the signing key: hex of the first 8 bytes of the
	// Ed25519 public key.
	KeyID string `json:"key_id"`
	// Signature is the hex Ed25519 signature over anchorMessage.
	Signature string `json:"signature"`
}

// checkpointsFileName is the checkpoint journal's name inside the audit
// directory (LEDGER_AUDIT_DIR).
const checkpointsFileName = "checkpoints.jsonl"

// ErrAnchorKeyNotConfigured is returned by Anchor when no anchor key was
// configured (see WithAnchorKey / LEDGER_ANCHOR_KEY).
var ErrAnchorKeyNotConfigured = errors.New("ledger: anchor key not configured (see WithAnchorKey / LEDGER_ANCHOR_KEY)")

// anchorDomain separates anchor signatures from every other signature the
// key might produce, so a signature can never be transplanted into a
// different protocol.
var anchorDomain = []byte("LEDGER-ANCHOR/v1:")

// anchorMessage is the canonical byte form an anchor signs:
// domain || BE64(seq) || headHash || BE64(anchoredAt unixnano, UTC).
// Fixed-width fields, no JSON ambiguity; standard library only.
func anchorMessage(seq uint64, headHash [32]byte, anchoredAt time.Time) []byte {
	msg := make([]byte, 0, len(anchorDomain)+48)
	msg = append(msg, anchorDomain...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	msg = append(msg, b[:]...)
	msg = append(msg, headHash[:]...)
	binary.BigEndian.PutUint64(b[:], uint64(anchoredAt.UTC().UnixNano()))
	msg = append(msg, b[:]...)
	return msg
}

// ParseAnchorKey parses LEDGER_ANCHOR_KEY: hex of a 32-byte seed (the
// public key is derived) or hex of a 64-byte Ed25519 private key (the
// public half must match the seed half, otherwise the key is corrupt).
// Anything else is an error; main.go fails the startup fast on it.
func ParseAnchorKey(raw string) (ed25519.PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("ledger: anchor key: invalid hex: %w", err)
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		// Validate the public half against the seed half: PrivateKey.Public
		// just copies the stored public bytes, so compare against a
		// freshly derived public key instead.
		derived := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]).Public().(ed25519.PublicKey)
		if !bytes.Equal(derived, b[ed25519.SeedSize:]) {
			return nil, fmt.Errorf("ledger: anchor key: public half does not match the seed half (corrupt key)")
		}
		return ed25519.PrivateKey(bytes.Clone(b)), nil
	default:
		return nil, fmt.Errorf("ledger: anchor key: want 32-byte seed or 64-byte private key hex, got %d bytes", len(b))
	}
}

// WithAnchorKey configures the Ed25519 private key Anchor uses to sign
// audit-chain head checkpoints. The key never leaves the process: it is
// not written to snapshots, checkpoints, or the audit log — only the
// KeyID (public-key prefix) is recorded. Panics on a malformed key; that
// is a programming error, fail it fast.
func WithAnchorKey(key ed25519.PrivateKey) Option {
	if len(key) != ed25519.PrivateKeySize {
		panic("ledger: anchor key must be a 64-byte ed25519 private key")
	}
	return func(l *Ledger) {
		l.anchor.key = key
		pub := key.Public().(ed25519.PublicKey)
		l.anchor.keyID = hex.EncodeToString(pub[:8])
	}
}

// anchorState holds the external-anchoring state. checkpointMu guards the
// checkpoint list and the loaded flag; it never nests inside l.mu (see
// the lock-order note on verifyAnchorsLocked).
type anchorState struct {
	key      ed25519.PrivateKey
	keyID    string
	mu       sync.Mutex
	loaded   bool
	skipped  int
	checkpoints []AnchorCheckpoint
}

// ensureCheckpointsLoaded reads checkpoints.jsonl once, lazily, so a
// ledger opened on an existing audit directory resumes its anchor
// history. Takes only anchorState.mu.
func (l *Ledger) ensureCheckpointsLoaded() {
	a := &l.anchor
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return
	}
	a.loaded = true
	dir, ok := l.AuditDir()
	if !ok {
		return
	}
	cps, skipped := loadCheckpoints(filepath.Join(dir, checkpointsFileName))
	a.checkpoints = cps
	a.skipped = skipped
}

// loadCheckpoints reads a checkpoints file, skipping corrupt lines
// (counted): a torn write can only ever corrupt its own line, and a
// partially written tail must not wedge the ledger at startup. A missing
// file is not an error — no anchor has been taken yet.
func loadCheckpoints(path string) ([]AnchorCheckpoint, int) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer fh.Close()
	var out []AnchorCheckpoint
	skipped := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var cp AnchorCheckpoint
		if err := json.Unmarshal(line, &cp); err != nil || !validCheckpointShape(cp) {
			skipped++
			continue
		}
		out = append(out, cp)
	}
	return out, skipped
}

// validCheckpointShape rejects checkpoints that cannot possibly verify:
// wrong-length hashes, non-hex, or a zero timestamp.
func validCheckpointShape(cp AnchorCheckpoint) bool {
	if len(cp.HeadHash) != 64 || len(cp.Signature) != 128 || len(cp.KeyID) != 16 {
		return false
	}
	if _, err := hex.DecodeString(cp.HeadHash); err != nil {
		return false
	}
	if _, err := hex.DecodeString(cp.Signature); err != nil {
		return false
	}
	if _, err := hex.DecodeString(cp.KeyID); err != nil {
		return false
	}
	return !cp.AnchoredAt.IsZero()
}

// appendCheckpointLine appends one checkpoint to the journal and fsyncs
// before returning: an anchor is a compliance event, and the caller must
// be able to treat a returned checkpoint as durable.
func appendCheckpointLine(dir string, cp AnchorCheckpoint) error {
	line, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("ledger: anchor checkpoint marshal: %w", err)
	}
	line = append(line, '\n')
	fh, err := os.OpenFile(filepath.Join(dir, checkpointsFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: anchor checkpoint open: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(line); err != nil {
		return fmt.Errorf("ledger: anchor checkpoint write: %w", err)
	}
	if err := fh.Sync(); err != nil {
		return fmt.Errorf("ledger: anchor checkpoint sync: %w", err)
	}
	return nil
}

// Anchor signs the current audit-chain head with the configured Ed25519
// key and appends the checkpoint to checkpoints.jsonl in the audit
// directory (fsync'd before returning), then records it in memory. The
// snapshot is taken under the read lock, so a concurrent Post either
// lands fully before the anchor (and is covered) or fully after (and is
// covered by the next anchor) — an anchor always attests a real prefix
// of the chain, never a torn one.
//
// Anchor is idempotent for an unchanged head: when the latest checkpoint
// already attests this exact (seq, headHash) under this key, the existing
// checkpoint is returned and nothing is appended — a periodic anchor job
// on a quiet ledger does not litter the journal with duplicate
// attestations, and concurrent anchors of the same head collapse to one.
//
// Fail-closed: without a configured key Anchor returns
// ErrAnchorKeyNotConfigured; without an audit directory (no durable home
// for checkpoints.jsonl) it returns an error; a failed disk append
// returns an error and records nothing in memory.
func (l *Ledger) Anchor() (AnchorCheckpoint, error) {
	a := &l.anchor
	if len(a.key) == 0 {
		return AnchorCheckpoint{}, ErrAnchorKeyNotConfigured
	}
	dir, ok := l.AuditDir()
	if !ok {
		return AnchorCheckpoint{}, fmt.Errorf("ledger: anchor requires an audit directory (LEDGER_AUDIT_DIR) as the durable home of checkpoints.jsonl")
	}
	l.ensureCheckpointsLoaded()
	l.mu.RLock()
	seq := uint64(len(l.chain))
	var head [32]byte
	if seq > 0 {
		head = l.chain[seq-1].hash
	}
	l.mu.RUnlock()
	headHex := hex.EncodeToString(head[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.checkpoints); n > 0 {
		if last := a.checkpoints[n-1]; last.Seq == seq && last.HeadHash == headHex && last.KeyID == a.keyID {
			return last, nil
		}
	}
	at := time.Now().UTC()
	cp := AnchorCheckpoint{
		Seq:        seq,
		HeadHash:   headHex,
		AnchoredAt: at,
		KeyID:      a.keyID,
		Signature:  hex.EncodeToString(ed25519.Sign(a.key, anchorMessage(seq, head, at))),
	}
	if err := appendCheckpointLine(dir, cp); err != nil {
		return AnchorCheckpoint{}, err
	}
	a.checkpoints = append(a.checkpoints, cp)
	return cp, nil
}

// AnchorCheckpoints returns the loaded checkpoints, oldest first. The
// returned slice is a copy.
func (l *Ledger) AnchorCheckpoints() []AnchorCheckpoint {
	l.ensureCheckpointsLoaded()
	a := &l.anchor
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AnchorCheckpoint, len(a.checkpoints))
	copy(out, a.checkpoints)
	return out
}

// verifyAnchorsLocked replays every loaded checkpoint against the live
// chain: sequence numbers must strictly increase, each checkpoint's head
// hash must equal the chain link at its seq, and — when an anchor key is
// configured — each signature must verify and name this ledger's key.
// Callers must hold l.mu (read or write); the checkpoint list itself is
// only read here, and every mutation path takes anchorState.mu without
// holding l.mu, so the lock order stays l.mu -> anchorState.mu.
//
// Lock-order audit: Anchor takes l.mu (released), then anchorState.mu;
// AnchorStatus takes anchorState.mu (released), then l.mu; Reconcile
// takes l.mu, then anchorState.mu inside ensureCheckpointsLoaded and
// here. No path holds both in the opposite order.
func (l *Ledger) verifyAnchorsLocked() error {
	a := &l.anchor
	var prev uint64
	for i, cp := range a.checkpoints {
		if i > 0 && cp.Seq <= prev {
			return fmt.Errorf("ledger: anchor checkpoint %d: seq %d not greater than previous seq %d (checkpoint file reordered or duplicated)",
				i, cp.Seq, prev)
		}
		prev = cp.Seq
		var want [32]byte
		if cp.Seq > 0 {
			if cp.Seq > uint64(len(l.chain)) {
				return fmt.Errorf("ledger: anchor checkpoint %d: seq %d beyond chain length %d (anchored data lost — chain was truncated)",
					i, cp.Seq, len(l.chain))
			}
			want = l.chain[cp.Seq-1].hash
		}
		if got := hex.EncodeToString(want[:]); got != cp.HeadHash {
			return fmt.Errorf("ledger: anchor checkpoint %d: head_hash does not match chain link %d (journal rewritten after anchoring)",
				i, cp.Seq)
		}
		if len(a.key) > 0 {
			if cp.KeyID != a.keyID {
				return fmt.Errorf("ledger: anchor checkpoint %d: key_id %q is not this ledger's anchor key",
					i, cp.KeyID)
			}
			headBytes, _ := hex.DecodeString(cp.HeadHash) // shape-validated at load/Anchor time
			var hh [32]byte
			copy(hh[:], headBytes)
			sig, _ := hex.DecodeString(cp.Signature)
			if !ed25519.Verify(a.key.Public().(ed25519.PublicKey), anchorMessage(cp.Seq, hh, cp.AnchoredAt), sig) {
				return fmt.Errorf("ledger: anchor checkpoint %d: signature invalid (checkpoint forged or corrupted)",
					i)
			}
		}
	}
	return nil
}

// AnchorStatus is the operator-facing summary of external anchoring,
// served by GET /entries/verify and folded into the Reconcile report's
// audit-chain health.
type AnchorStatus struct {
	// Configured is true when an anchor key is configured.
	Configured bool `json:"configured"`
	// KeyID identifies the configured signing key, empty when unconfigured.
	KeyID string `json:"key_id,omitempty"`
	// Checkpoints is the number of loaded checkpoints.
	Checkpoints int `json:"checkpoints"`
	// Latest is the newest checkpoint, nil when none exists.
	Latest *AnchorCheckpoint `json:"latest,omitempty"`
	// Verified is true when every checkpoint's seq/head-hash/signature
	// chain checks out (vacuously true with zero checkpoints).
	Verified bool `json:"verified"`
	// Error describes the first verification failure, empty when Verified.
	Error string `json:"error,omitempty"`
	// SkippedLines counts corrupt checkpoints.jsonl lines skipped at
	// load; a torn write corrupts only its own line.
	SkippedLines int `json:"skipped_lines,omitempty"`
}

// AnchorStatus summarizes external anchoring: configuration, checkpoint
// count, the latest checkpoint, and whether the whole checkpoint history
// verifies against the live chain.
func (l *Ledger) AnchorStatus() AnchorStatus {
	l.ensureCheckpointsLoaded()
	a := &l.anchor
	a.mu.Lock()
	n := len(a.checkpoints)
	skipped := a.skipped
	var latest *AnchorCheckpoint
	if n > 0 {
		cp := a.checkpoints[n-1]
		latest = &cp
	}
	configured := len(a.key) > 0
	keyID := a.keyID
	a.mu.Unlock()
	st := AnchorStatus{
		Configured:   configured,
		Checkpoints:  n,
		Latest:       latest,
		SkippedLines: skipped,
	}
	if configured {
		st.KeyID = keyID
	}
	l.mu.RLock()
	err := l.verifyAnchorsLocked()
	l.mu.RUnlock()
	st.Verified = err == nil
	if err != nil {
		st.Error = err.Error()
	}
	return st
}
