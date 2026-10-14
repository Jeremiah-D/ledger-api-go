package ledger

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTestLeaves returns a shuffled leaf set so tests can prove the
// tree is order-independent.
func buildTestLeaves() []MerkleLeaf {
	return []MerkleLeaf{
		{Account: "carol", Currency: "USD", BalanceCents: 300},
		{Account: "alice", Currency: "USD", BalanceCents: 100},
		{Account: "bob", Currency: "EUR", BalanceCents: -50},
		{Account: "alice", Currency: "EUR", BalanceCents: 200},
		{Account: "dave", Currency: "USD", BalanceCents: 0},
	}
}

func mustProve(t *testing.T, tree *MerkleTree, i int) MerkleProof {
	t.Helper()
	p, err := tree.Prove(i)
	if err != nil {
		t.Fatalf("Prove(%d): %v", i, err)
	}
	return p
}

// The root is deterministic: insertion order does not matter, and the
// leaves are canonicalized to sorted (account, currency) order.
func TestMerkleTreeDeterministic(t *testing.T) {
	a := BuildMerkleTree(buildTestLeaves())
	b := BuildMerkleTree([]MerkleLeaf{
		{Account: "alice", Currency: "USD", BalanceCents: 100},
		{Account: "alice", Currency: "EUR", BalanceCents: 200},
		{Account: "bob", Currency: "EUR", BalanceCents: -50},
		{Account: "carol", Currency: "USD", BalanceCents: 300},
		{Account: "dave", Currency: "USD", BalanceCents: 0},
	})
	if a.Root() != b.Root() {
		t.Fatal("same leaves in different order produced different roots")
	}
	if got, want := a.Leaf(0), (MerkleLeaf{Account: "alice", Currency: "EUR", BalanceCents: 200}); got != want {
		t.Fatalf("Leaf(0) = %+v, want canonical first leaf %+v", got, want)
	}
	if got := a.RootHex(); len(got) != 64 {
		t.Fatalf("RootHex = %q, want 64 hex chars", got)
	}
	if idx := a.LeafIndex("bob", "EUR"); idx != 2 {
		t.Fatalf("LeafIndex(bob,EUR) = %d, want 2", idx)
	}
	if idx := a.LeafIndex("nobody", "USD"); idx != -1 {
		t.Fatalf("LeafIndex(nobody,USD) = %d, want -1", idx)
	}
}

// Every leaf of an odd-sized tree proves and verifies.
func TestMerkleProofRoundTripOddLeaves(t *testing.T) {
	tree := BuildMerkleTree(buildTestLeaves()) // 5 leaves: odd at two levels
	root := tree.Root()
	for i := 0; i < tree.LeafCount(); i++ {
		p := mustProve(t, tree, i)
		if p.Leaf != tree.Leaf(i) {
			t.Fatalf("proof leaf %d mismatch", i)
		}
		if !VerifyMerkleProof(root, p) {
			t.Fatalf("proof for leaf %d did not verify", i)
		}
	}
}

// Even leaf counts, a single leaf, and two leaves all verify.
func TestMerkleProofShapes(t *testing.T) {
	for _, n := range []int{1, 2, 4, 5} {
		leaves := buildTestLeaves()[:n]
		tree := BuildMerkleTree(leaves)
		root := tree.Root()
		for i := 0; i < n; i++ {
			if !VerifyMerkleProof(root, mustProve(t, tree, i)) {
				t.Fatalf("n=%d: proof for leaf %d did not verify", n, i)
			}
		}
	}
}

// The empty ledger commits to the genesis (all-zero) root.
func TestMerkleTreeEmpty(t *testing.T) {
	tree := BuildMerkleTree(nil)
	var zero [32]byte
	if tree.Root() != zero {
		t.Fatalf("empty tree root = %x, want 32 zero bytes", tree.Root())
	}
	if got := tree.RootHex(); got != strings.Repeat("0", 64) {
		t.Fatalf("empty tree RootHex = %q, want 64 zeros", got)
	}
	if _, err := tree.Prove(0); !errors.Is(err, ErrMerkleProofIndex) {
		t.Fatalf("Prove on empty tree = %v, want ErrMerkleProofIndex", err)
	}
}

// Tampering anywhere — the balance, a sibling hash, the leaf index, or
// the root — fails verification.
func TestMerkleProofTamperDetection(t *testing.T) {
	tree := BuildMerkleTree(buildTestLeaves())
	root := tree.Root()
	p := mustProve(t, tree, 1)

	// Rewritten balance: the classic snapshot-tamper scenario — the
	// auditor's leaf no longer hashes to the committed leaf.
	tampered := p
	tampered.Leaf.BalanceCents += 1
	if VerifyMerkleProof(root, tampered) {
		t.Fatal("proof with rewritten balance verified")
	}

	// Spliced sibling: a corrupted path cannot reach the root.
	spliced := p
	spliced.Siblings = append([][32]byte(nil), p.Siblings...)
	spliced.Siblings[0][0] ^= 0xff
	if VerifyMerkleProof(root, spliced) {
		t.Fatal("proof with corrupted sibling verified")
	}

	// Swapped leaf index: the fold order no longer matches the tree.
	moved := p
	moved.LeafIndex = 2
	if VerifyMerkleProof(root, moved) {
		t.Fatal("proof with wrong leaf index verified")
	}

	// Wrong root: a proof only means something against its anchored root.
	var other [32]byte
	other[0] = 1
	if VerifyMerkleProof(other, p) {
		t.Fatal("proof verified against a different root")
	}

	// A foreign leaf can never be smuggled in.
	foreign := p
	foreign.Leaf = MerkleLeaf{Account: "mallory", Currency: "USD", BalanceCents: 999999}
	if VerifyMerkleProof(root, foreign) {
		t.Fatal("proof for a foreign leaf verified")
	}
}

// The leaf encoding is length-prefixed: "a"+"bc" must not alias "ab"+"c".
func TestMerkleLeafEncodingUnambiguous(t *testing.T) {
	a := merkleLeafEncoding(MerkleLeaf{Account: "a", Currency: "bc", BalanceCents: 1})
	b := merkleLeafEncoding(MerkleLeaf{Account: "ab", Currency: "c", BalanceCents: 1})
	if bytes.Equal(a, b) {
		t.Fatal("ambiguous leaf encodings collide")
	}
}

// Domain separation: a leaf hash can never equal an inner hash of the
// same value, so a proof cannot be reinterpreted across tree shapes.
func TestMerkleDomainSeparation(t *testing.T) {
	leaf := MerkleLeaf{Account: "alice", Currency: "USD", BalanceCents: 100}
	lh := merkleLeafHash(leaf)
	inner := merkleParentHash(lh, lh)
	if lh == inner {
		t.Fatal("leaf hash equals inner hash — domain separation broken")
	}
}

// Snapshot round-trip: the exported header root matches the live root,
// the import succeeds, and the restored ledger's root agrees.
func TestSnapshotMerkleRootRoundTrip(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	mustPostEntry(t, l, "e2")

	rootHex, version := l.SnapshotMerkleRoot()
	if len(rootHex) != 64 {
		t.Fatalf("SnapshotMerkleRoot = %q, want 64 hex chars", rootHex)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	meta := readMetaLine(t, mustWriteTempSnapshot(t, buf.Bytes()))
	got, _ := meta["merkle_root"].(string)
	if got != rootHex {
		t.Fatalf("snapshot meta merkle_root = %q, want live root %q", got, rootHex)
	}

	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if r2, _ := restored.SnapshotMerkleRoot(); r2 != rootHex {
		t.Fatalf("restored root = %q, want %q", r2, rootHex)
	}
}

// A tampered merkle_root in the snapshot header rejects the import —
// this is the header-commitment half of the tamper protection (the
// journal-chain half is covered by the existing chain verification).
func TestImportSnapshotRejectsTamperedMerkleRoot(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	var meta map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("meta parse: %v", err)
	}
	meta["merkle_root"] = strings.Repeat("0", 64)
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("meta marshal: %v", err)
	}
	lines[0] = string(raw)
	tampered := strings.Join(lines, "\n") + "\n"

	if _, err := ImportSnapshot(strings.NewReader(tampered)); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("ImportSnapshot with tampered merkle_root = %v, want ErrSnapshotInvalid", err)
	}
}

// Pre-LG-78 snapshots carry no merkle_root: they still import (the
// root check is skipped), preserving backward compatibility.
func TestImportSnapshotLegacyWithoutMerkleRoot(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	var meta map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("meta parse: %v", err)
	}
	delete(meta, "merkle_root")
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("meta marshal: %v", err)
	}
	lines[0] = string(raw)
	legacy := strings.Join(lines, "\n") + "\n"

	restored, err := ImportSnapshot(strings.NewReader(legacy))
	if err != nil {
		t.Fatalf("ImportSnapshot of pre-LG-78 snapshot: %v", err)
	}
	rootHex, _ := restored.SnapshotMerkleRoot()
	if len(rootHex) != 64 {
		t.Fatalf("restored root = %q, want 64 hex chars", rootHex)
	}
}

// An empty ledger exports the genesis root and reimports cleanly.
func TestSnapshotMerkleRootEmptyLedger(t *testing.T) {
	l := New()
	rootHex, version := l.SnapshotMerkleRoot()
	if rootHex != strings.Repeat("0", 64) || version != 0 {
		t.Fatalf("empty root = %q v%d, want 64 zeros v0", rootHex, version)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if _, err := ImportSnapshot(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("ImportSnapshot of empty snapshot: %v", err)
	}
}

// MerkleProofFor: default currency, explicit currency, unknown account,
// and invalid currency.
func TestMerkleProofFor(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "alice", CreditAccount: "bob", AmountCents: 100, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "alice", CreditAccount: "bob", AmountCents: 200, Currency: "EUR"}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	// Default currency (empty string means USD).
	proof, rootHex, version, err := l.MerkleProofFor("alice", "")
	if err != nil {
		t.Fatalf("MerkleProofFor(alice, \"\"): %v", err)
	}
	if proof.Leaf.BalanceCents != 100 || proof.Leaf.Currency != "USD" {
		t.Fatalf("proof leaf = %+v, want alice/USD/100", proof.Leaf)
	}
	var root [32]byte
	raw, _ := hex.DecodeString(rootHex)
	copy(root[:], raw)
	if !VerifyMerkleProof(root, proof) {
		t.Fatal("proof from MerkleProofFor did not verify offline")
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}

	// Explicit currency: the EUR position is a separate leaf.
	peur, _, _, err := l.MerkleProofFor("alice", "EUR")
	if err != nil {
		t.Fatalf("MerkleProofFor(alice, EUR): %v", err)
	}
	if peur.Leaf.BalanceCents != 200 {
		t.Fatalf("EUR leaf balance = %d, want 200", peur.Leaf.BalanceCents)
	}
	if peur.LeafIndex == proof.LeafIndex {
		t.Fatal("USD and EUR leaves share an index")
	}

	// Unknown account: no leaf, no proof.
	if _, _, _, err := l.MerkleProofFor("mallory", "USD"); !errors.Is(err, ErrMerkleLeafNotFound) {
		t.Fatalf("MerkleProofFor(mallory) = %v, want ErrMerkleLeafNotFound", err)
	}
	// Valid account, unposted currency: also no leaf.
	if _, _, _, err := l.MerkleProofFor("alice", "CNY"); !errors.Is(err, ErrMerkleLeafNotFound) {
		t.Fatalf("MerkleProofFor(alice,CNY) = %v, want ErrMerkleLeafNotFound", err)
	}
	// Invalid currency code.
	if _, _, _, err := l.MerkleProofFor("alice", "usd"); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("MerkleProofFor(alice,usd) = %v, want ErrInvalidCurrency", err)
	}
}

// The backup worker's snapshot_backup audit event anchors the Merkle
// root with the snapshot version: the sealed audit trail then proves
// which root was committed for which version.
func TestSnapshotBackupAuditAnchorsMerkleRoot(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 10, MaxRetries: 1,
	})
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backupOnce = %q,%v, want full,true", kind, ok)
	}
	// Flush the async audit writer before asserting.
	time.Sleep(500 * time.Millisecond)

	wantRoot, _ := l.SnapshotMerkleRoot()
	found := false
	entries, _ := os.ReadDir(al.Dir())
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(al.Dir(), e.Name()))
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, `"op":"snapshot_backup"`) {
				continue
			}
			found = true
			if !strings.Contains(line, `"merkle_root":"`+wantRoot+`"`) {
				t.Fatalf("snapshot_backup audit event does not anchor the export root %q:\n%s", wantRoot, line)
			}
			if !strings.Contains(line, `"version_after":1`) {
				t.Fatalf("snapshot_backup audit event missing version anchoring:\n%s", line)
			}
		}
	}
	if !found {
		t.Fatal("no snapshot_backup op in audit log files")
	}
}

// mustWriteTempSnapshot writes snapshot bytes to a temp file and
// returns the path (readMetaLine in snapshot_backup_test.go takes a
// path).
func mustWriteTempSnapshot(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
