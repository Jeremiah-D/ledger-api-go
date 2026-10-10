package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAnchorTestLedger builds a ledger with an audit log (the durable home
// of checkpoints.jsonl) and a fresh random anchor key.
func newAnchorTestLedger(t *testing.T) (*Ledger, ed25519.PrivateKey, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	l := New(WithAuditLog(al), WithAnchorKey(key))
	return l, key, dir
}

// postAnchorEntries posts n entries with unique IDs under prefix (seedChain
// uses fixed IDs, so it cannot run twice on one ledger).
func postAnchorEntries(t *testing.T, l *Ledger, prefix string, n int) {
	t.Helper()
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		e := JournalEntry{
			ID:             fmt.Sprintf("%s-%d", prefix, i),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "anchor-" + fmt.Sprintf("%s-%d", prefix, i),
			CreatedAt:      base.Add(time.Duration(i) * time.Second),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
}

func TestParseAnchorKeySeed(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	key, err := ParseAnchorKey(hex.EncodeToString(seed))
	if err != nil {
		t.Fatalf("ParseAnchorKey(seed) = %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("len(key) = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
	if want := ed25519.NewKeyFromSeed(seed); string(key) != string(want) {
		t.Fatal("seed-derived key mismatch")
	}
}

func TestParseAnchorKeyPrivate(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAnchorKey(hex.EncodeToString([]byte(key)))
	if err != nil {
		t.Fatalf("ParseAnchorKey(private) = %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("private key round-trip mismatch")
	}
}

func TestParseAnchorKeyInvalid(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(key)
	corrupt[63] ^= 0xff // break the public half
	// Explicit cases, one assertion each — no table-driven cleverness
	// around what must fail.
	if _, err := ParseAnchorKey("not-hex"); err == nil {
		t.Error("ParseAnchorKey(not-hex) = nil, want error")
	}
	if _, err := ParseAnchorKey(hex.EncodeToString(make([]byte, 16))); err == nil {
		t.Error("ParseAnchorKey(16 bytes) = nil, want error")
	}
	if _, err := ParseAnchorKey(hex.EncodeToString(make([]byte, 48))); err == nil {
		t.Error("ParseAnchorKey(48 bytes) = nil, want error")
	}
	if _, err := ParseAnchorKey(hex.EncodeToString(corrupt)); err == nil {
		t.Error("ParseAnchorKey(mismatched public half) = nil, want error")
	}
	// Whitespace-padded valid seed parses (trimmed).
	if _, err := ParseAnchorKey("  " + hex.EncodeToString(make([]byte, 32)) + "\n"); err != nil {
		t.Errorf("ParseAnchorKey(padded seed) = %v, want nil", err)
	}
}

func TestAnchorRequiresKey(t *testing.T) {
	l := New()
	if _, err := l.Anchor(); err != ErrAnchorKeyNotConfigured {
		t.Fatalf("Anchor() without key = %v, want ErrAnchorKeyNotConfigured", err)
	}
}

func TestAnchorRequiresAuditDir(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAnchorKey(key)) // no audit log → no durable home
	if _, err := l.Anchor(); err == nil || !strings.Contains(err.Error(), "audit directory") {
		t.Fatalf("Anchor() without audit dir = %v, want audit-directory error", err)
	}
}

func TestAnchorRoundTrip(t *testing.T) {
	l, key, dir := newAnchorTestLedger(t)
	seedChain(t, l, 2)

	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	head, links := l.ChainHead()
	if cp.Seq != links || cp.Seq != 2 {
		t.Fatalf("cp.Seq = %d, want 2 (links=%d)", cp.Seq, links)
	}
	if cp.HeadHash != head {
		t.Fatalf("cp.HeadHash != chain head")
	}
	if cp.AnchoredAt.IsZero() {
		t.Fatal("cp.AnchoredAt is zero")
	}
	pub := key.Public().(ed25519.PublicKey)
	if want := hex.EncodeToString(pub[:8]); cp.KeyID != want {
		t.Fatalf("cp.KeyID = %q, want %q", cp.KeyID, want)
	}
	// The signature verifies over the canonical message.
	var hh [32]byte
	hb, _ := hex.DecodeString(cp.HeadHash)
	copy(hh[:], hb)
	sig, _ := hex.DecodeString(cp.Signature)
	if !ed25519.Verify(pub, anchorMessage(cp.Seq, hh, cp.AnchoredAt), sig) {
		t.Fatal("anchor signature does not verify")
	}
	// The checkpoint journal has exactly one line.
	raw, err := os.ReadFile(filepath.Join(dir, checkpointsFileName))
	if err != nil {
		t.Fatalf("read checkpoints.jsonl: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 1 {
		t.Fatalf("checkpoints.jsonl has %d lines, want 1", lines)
	}

	// A second anchor after another post: seq increases, history kept.
	postAnchorEntries(t, l, "a2", 1)
	cp2, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() #2 = %v", err)
	}
	if cp2.Seq != 3 {
		t.Fatalf("cp2.Seq = %d, want 3", cp2.Seq)
	}
	cps := l.AnchorCheckpoints()
	if len(cps) != 2 || cps[0].Seq != 2 || cps[1].Seq != 3 {
		t.Fatalf("AnchorCheckpoints() seqs = %v, want [2 3]", cps)
	}
	st := l.AnchorStatus()
	if !st.Configured || st.Checkpoints != 2 || !st.Verified || st.Error != "" {
		t.Fatalf("AnchorStatus() = %+v, want configured/2/verified", st)
	}
	if st.Latest == nil || st.Latest.Seq != 3 {
		t.Fatalf("AnchorStatus().Latest = %+v, want seq 3", st.Latest)
	}
}

func TestAnchorEmptyChain(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() on empty ledger = %v", err)
	}
	if cp.Seq != 0 {
		t.Fatalf("cp.Seq = %d, want 0", cp.Seq)
	}
	if want := strings.Repeat("0", 64); cp.HeadHash != want {
		t.Fatalf("cp.HeadHash = %q, want genesis zeros", cp.HeadHash)
	}
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err != nil {
		t.Fatalf("verifyAnchorsLocked = %v", err)
	}
}

func TestAnchorReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al), WithAnchorKey(key))
	seedChain(t, l, 2)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	al.Close()

	// A fresh ledger on the same directory resumes the anchor history.
	al2, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer al2.Close()
	l2 := New(WithAuditLog(al2), WithAnchorKey(key))
	seedChain(t, l2, 2) // rebuild the same chain prefix
	st := l2.AnchorStatus()
	if st.Checkpoints != 1 || !st.Verified {
		t.Fatalf("reloaded AnchorStatus() = %+v, want 1 checkpoint verified", st)
	}
}

func TestAnchorSkipsCorruptLines(t *testing.T) {
	l, _, dir := newAnchorTestLedger(t)
	seedChain(t, l, 1)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	// Append garbage after the valid checkpoint.
	fh, err := os.OpenFile(filepath.Join(dir, checkpointsFileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("this is not json\n"); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	// A fresh ledger loads the valid checkpoint and counts the bad line.
	dir2 := dir // same dir
	al, err := NewAuditLog(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l2 := New(WithAuditLog(al), WithAnchorKey(key))
	seedChain(t, l2, 1)
	st := l2.AnchorStatus()
	if st.Checkpoints != 1 {
		t.Fatalf("Checkpoints = %d, want 1 (valid line kept)", st.Checkpoints)
	}
	if st.SkippedLines != 1 {
		t.Fatalf("SkippedLines = %d, want 1", st.SkippedLines)
	}
}

func TestAnchorNonMonotonicSeqFails(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	seedChain(t, l, 2)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	// White-box: duplicate the checkpoint — the journal is append-only,
	// so a duplicated line is the realistic corruption.
	l.anchor.mu.Lock()
	l.anchor.checkpoints = append(l.anchor.checkpoints, l.anchor.checkpoints[0])
	l.anchor.mu.Unlock()
	l.mu.RLock()
	err := l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err == nil || !strings.Contains(err.Error(), "not greater than previous") {
		t.Fatalf("verifyAnchorsLocked() = %v, want non-monotonic error", err)
	}
	if st := l.AnchorStatus(); st.Verified || st.Error == "" {
		t.Fatalf("AnchorStatus() = %+v, want verified=false with error", st)
	}
}

func TestAnchorHeadMismatchFails(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	seedChain(t, l, 2)
	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	// White-box: rewrite the anchored head hash — the journal-rewritten-
	// after-anchoring scenario.
	l.anchor.mu.Lock()
	cp.HeadHash = strings.Repeat("1", 64)
	l.anchor.checkpoints[0] = cp
	l.anchor.mu.Unlock()
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err == nil || !strings.Contains(err.Error(), "head_hash does not match") {
		t.Fatalf("verifyAnchorsLocked() = %v, want head-hash mismatch error", err)
	}
}

func TestAnchorBadSignatureFails(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	seedChain(t, l, 1)
	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	// White-box: forge the signature — the shape stays valid so the
	// failure is the signature check, not the shape check.
	l.anchor.mu.Lock()
	cp.Signature = strings.Repeat("2", 128)
	l.anchor.checkpoints[0] = cp
	l.anchor.mu.Unlock()
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err == nil || !strings.Contains(err.Error(), "signature invalid") {
		t.Fatalf("verifyAnchorsLocked() = %v, want signature error", err)
	}
}

func TestAnchorSeqBeyondChainFails(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	// Anchor the empty chain (seq 0), then post: the checkpoint stays at
	// 0, which is still consistent. Instead, white-box a future seq.
	cp, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	_ = cp
	l.anchor.mu.Lock()
	future := l.anchor.checkpoints[0]
	future.Seq = 99
	future.HeadHash = strings.Repeat("3", 64)
	future.Signature = strings.Repeat("4", 128)
	l.anchor.checkpoints[0] = future
	l.anchor.mu.Unlock()
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err == nil || !strings.Contains(err.Error(), "beyond chain length") {
		t.Fatalf("verifyAnchorsLocked() = %v, want beyond-chain error", err)
	}
}

func TestReconcileReportsAnchorHealth(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	rep := l.Reconcile(time.Now())
	if rep.AuditChain.AnchorCheckpoints != 0 || !rep.AuditChain.AnchorOK {
		t.Fatalf("empty anchor health = %+v, want 0 checkpoints ok", rep.AuditChain)
	}
	seedChain(t, l, 2)
	if _, err := l.Anchor(); err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	rep = l.Reconcile(time.Now())
	if rep.AuditChain.AnchorCheckpoints != 1 || !rep.AuditChain.AnchorOK || rep.AuditChain.AnchorError != "" {
		t.Fatalf("anchor health = %+v, want 1 checkpoint ok", rep.AuditChain)
	}
	// Break the checkpoint history: the report must surface it.
	l.anchor.mu.Lock()
	l.anchor.checkpoints = append(l.anchor.checkpoints, l.anchor.checkpoints[0])
	l.anchor.mu.Unlock()
	rep = l.Reconcile(time.Now())
	if rep.AuditChain.AnchorOK || rep.AuditChain.AnchorError == "" {
		t.Fatalf("broken anchor health = %+v, want ok=false with error", rep.AuditChain)
	}
}

func TestAnchorConcurrent(t *testing.T) {
	l, _, dir := newAnchorTestLedger(t)
	seedChain(t, l, 4)
	done := make(chan AnchorCheckpoint, 8)
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		go func() {
			cp, err := l.Anchor()
			if err != nil {
				errs <- err
				return
			}
			done <- cp
		}()
	}
	var first AnchorCheckpoint
	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent Anchor() = %v", err)
		case cp := <-done:
			if i == 0 {
				first = cp
			} else if cp != first {
				t.Fatalf("concurrent anchors diverged: %+v vs %+v", first, cp)
			}
		}
	}
	// Same head anchored concurrently collapses to one checkpoint (the
	// idempotency rule): the journal holds a single line.
	if n := len(l.AnchorCheckpoints()); n != 1 {
		t.Fatalf("checkpoints = %d, want 1 (same-head dedup)", n)
	}
	raw, err := os.ReadFile(filepath.Join(dir, checkpointsFileName))
	if err != nil {
		t.Fatalf("read checkpoints.jsonl: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 1 {
		t.Fatalf("checkpoints.jsonl has %d lines, want 1", lines)
	}
	l.mu.RLock()
	err = l.verifyAnchorsLocked()
	l.mu.RUnlock()
	if err != nil {
		t.Fatalf("verifyAnchorsLocked after concurrent anchors = %v", err)
	}
}

func TestAnchorIdempotentSameHead(t *testing.T) {
	l, _, _ := newAnchorTestLedger(t)
	seedChain(t, l, 2)
	cp1, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() = %v", err)
	}
	// Anchoring the unchanged head again returns the existing checkpoint
	// instead of appending a duplicate.
	cp2, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() #2 = %v", err)
	}
	if cp2 != cp1 {
		t.Fatalf("re-anchor of unchanged head = %+v, want %+v", cp2, cp1)
	}
	if n := len(l.AnchorCheckpoints()); n != 1 {
		t.Fatalf("checkpoints = %d, want 1", n)
	}
	// A new post moves the head: the next anchor is a fresh checkpoint.
	postAnchorEntries(t, l, "b2", 1)
	cp3, err := l.Anchor()
	if err != nil {
		t.Fatalf("Anchor() #3 = %v", err)
	}
	if cp3.Seq != 3 || cp3 == cp1 {
		t.Fatalf("anchor after new post = %+v, want fresh seq-3 checkpoint", cp3)
	}
}
