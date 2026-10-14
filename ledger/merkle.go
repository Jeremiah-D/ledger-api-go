package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// Snapshot balance Merkle inclusion proofs (LG-78).
//
// A disaster-recovery snapshot (see snapshot.go) anchors the ledger's
// balances, not just its journal: at export time the ledger builds a
// Merkle tree over every (account, currency) -> balance leaf and writes
// the root into the snapshot header (the meta line's "merkle_root"
// field). An auditor holding only the exported snapshot file can then
// prove that a specific balance was committed at export time — without
// trusting the ledger's storage — by replaying the leaf hashes up to
// the anchored root. A rewritten balance in a tampered snapshot either
// breaks the audit-chain verification (the journal no longer folds to
// the exported balances) or the root recorded in the snapshot's own
// header, and ImportSnapshot rejects both.
//
// Canonical construction (standard library only; this is the scheme
// every verifier must implement, online and offline):
//
//	leaf encoding : u64be(len(account)) || account ||
//	                u64be(len(currency)) || currency || i64be(balance_cents)
//	                (big-endian fixed width; length prefixes keep
//	                "a"+"bc" from aliasing "ab"+"c")
//	leaf hash     : SHA-256(0x00 || leaf encoding)
//	inner hash    : SHA-256(0x01 || left || right)
//	leaf order    : sorted by (account, currency), byte-wise
//	odd level     : the last node is duplicated to pair with itself
//	                (an odd leaf proves against its own hash as sibling;
//	                the pairing rule is unambiguous and documented here
//	                rather than left to implementation folklore)
//	empty ledger  : the root is 32 zero bytes (the "genesis" root), so
//	                an empty snapshot still commits to "no balances"
//
// The 0x00/0x01 domain separators are second-preimage protection: a
// leaf hash can never be mistaken for an inner hash of the same value,
// so a proof cannot be reinterpreted as a proof for a different tree
// shape. The leaf sort order is part of the canonical form — two
// builders over the same balances produce byte-identical trees, and
// therefore identical roots, on every machine.
//
// All leaves come from the ledger's balances index: every
// (account, currency) key the ledger tracks, including zero balances
// (an account that netted to zero still has a position in the tree).
// The tree is a commitment to the full balance sheet, not just the
// non-zero accounts.

// MerkleLeaf is one balance-sheet leaf: the net balance of one account
// in one currency, in cents.
type MerkleLeaf struct {
	Account      AccountID
	Currency     string
	BalanceCents int64
}

// merkleLeafEncoding is the canonical byte form a leaf is hashed from.
// See the package comment for the field layout.
func merkleLeafEncoding(leaf MerkleLeaf) []byte {
	acc := string(leaf.Account)
	out := make([]byte, 0, 8+len(acc)+8+len(leaf.Currency)+8)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(len(acc)))
	out = append(out, b[:]...)
	out = append(out, acc...)
	binary.BigEndian.PutUint64(b[:], uint64(len(leaf.Currency)))
	out = append(out, b[:]...)
	out = append(out, leaf.Currency...)
	binary.BigEndian.PutUint64(b[:], uint64(leaf.BalanceCents))
	out = append(out, b[:]...)
	return out
}

// MerkleLeafEncoding is the canonical byte form a leaf is hashed from,
// exported so offline verifiers (and the HTTP layer) can recompute the
// leaf hash without reimplementing the field layout. See the package
// comment for the layout.
func MerkleLeafEncoding(leaf MerkleLeaf) []byte {
	return merkleLeafEncoding(leaf)
}

// merkleLeafHash hashes one leaf: SHA-256(0x00 || leaf encoding).
func merkleLeafHash(leaf MerkleLeaf) [32]byte {
	enc := merkleLeafEncoding(leaf)
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(enc)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// merkleParentHash hashes one inner node: SHA-256(0x01 || left || right).
func merkleParentHash(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// MerkleTree is a deterministic binary Merkle tree over balance-sheet
// leaves (see the package comment for the canonical construction).
// The zero value is not usable; build one with BuildMerkleTree.
type MerkleTree struct {
	leaves     []MerkleLeaf
	leafHashes [][32]byte
	// levels[0] is the leaf level, levels[n-1] is the single root level.
	levels [][][32]byte
}

// BuildMerkleTree builds the canonical Merkle tree over leaves. The
// input is copied and sorted by (account, currency), so insertion order
// does not affect the root; two trees over the same balances agree
// byte-for-byte. An empty leaf set yields the genesis root (32 zero
// bytes).
func BuildMerkleTree(leaves []MerkleLeaf) *MerkleTree {
	cp := make([]MerkleLeaf, len(leaves))
	copy(cp, leaves)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Account != cp[j].Account {
			return cp[i].Account < cp[j].Account
		}
		return cp[i].Currency < cp[j].Currency
	})
	t := &MerkleTree{leaves: cp}
	t.leafHashes = make([][32]byte, len(cp))
	for i, leaf := range cp {
		t.leafHashes[i] = merkleLeafHash(leaf)
	}
	if len(t.leafHashes) == 0 {
		var genesis [32]byte
		t.levels = [][][32]byte{{genesis}}
		t.leafHashes = [][32]byte{genesis}
		return t
	}
	level := t.leafHashes
	t.levels = append(t.levels, level)
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			left := level[i]
			right := left // odd last node pairs with itself
			if i+1 < len(level) {
				right = level[i+1]
			}
			next = append(next, merkleParentHash(left, right))
		}
		t.levels = append(t.levels, next)
		level = next
	}
	return t
}

// Root returns the tree's root hash: the commitment to the whole
// balance sheet. For an empty tree it is the 32-zero-byte genesis root.
func (t *MerkleTree) Root() [32]byte {
	return t.levels[len(t.levels)-1][0]
}

// RootHex returns the root as lowercase hex.
func (t *MerkleTree) RootHex() string {
	r := t.Root()
	return hex.EncodeToString(r[:])
}

// LeafCount returns the number of leaves in the tree.
func (t *MerkleTree) LeafCount() int {
	return len(t.leaves)
}

// Leaf returns the canonical leaf at index i (sorted (account,
// currency) order).
func (t *MerkleTree) Leaf(i int) MerkleLeaf {
	return t.leaves[i]
}

// LeafIndex returns the canonical index of the (account, currency)
// leaf, or -1 when the tree has no such leaf.
func (t *MerkleTree) LeafIndex(account AccountID, currency string) int {
	for i, leaf := range t.leaves {
		if leaf.Account == account && leaf.Currency == currency {
			return i
		}
	}
	return -1
}

// MerkleProof is an inclusion proof for one leaf: the sibling hash at
// every level from the leaf level up to (but excluding) the root, plus
// the leaf's canonical index. A verifier recomputes the leaf hash and
// folds the siblings up, halving the index each level: even index
// means the running hash sits on the left, odd means on the right.
type MerkleProof struct {
	Leaf      MerkleLeaf
	LeafIndex int
	Siblings  [][32]byte
}

// ErrMerkleProofIndex is returned by Prove when the leaf index is out
// of range.
var ErrMerkleProofIndex = errors.New("ledger: merkle proof leaf index out of range")

// Prove builds the inclusion proof for the leaf at canonical index i.
// The proof is self-contained: it carries the leaf (so an offline
// verifier needs no other source of truth for the claimed balance)
// and the sibling path up to the root.
func (t *MerkleTree) Prove(i int) (MerkleProof, error) {
	if i < 0 || i >= len(t.leaves) {
		return MerkleProof{}, fmt.Errorf("%w: %d (tree has %d leaves)", ErrMerkleProofIndex, i, len(t.leaves))
	}
	proof := MerkleProof{Leaf: t.leaves[i], LeafIndex: i}
	idx := i
	for level := 0; level < len(t.levels)-1; level++ {
		nodes := t.levels[level]
		var sibling [32]byte
		if idx%2 == 0 {
			if idx+1 < len(nodes) {
				sibling = nodes[idx+1]
			} else {
				sibling = nodes[idx] // odd last node pairs with itself
			}
		} else {
			sibling = nodes[idx-1]
		}
		proof.Siblings = append(proof.Siblings, sibling)
		idx /= 2
	}
	return proof, nil
}

// VerifyMerkleProof is the offline verifier: it recomputes the leaf
// hash from the proof's leaf and folds the sibling path up to a root,
// reporting whether it equals root. It is a pure function of its
// arguments — no ledger, no network, no trust in the prover beyond the
// anchored root. A rewritten balance, a spliced sibling, a swapped
// leaf index, or a wrong root all return false.
func VerifyMerkleProof(root [32]byte, proof MerkleProof) bool {
	if proof.LeafIndex < 0 {
		return false
	}
	// A single-leaf tree carries no siblings: the proof is the leaf
	// itself hashing to the root. Anything else with an empty path
	// cannot prove inclusion, and the recompute below rejects it.
	cur := merkleLeafHash(proof.Leaf)
	idx := proof.LeafIndex
	for _, sibling := range proof.Siblings {
		if idx < 0 {
			return false
		}
		if idx%2 == 0 {
			cur = merkleParentHash(cur, sibling)
		} else {
			cur = merkleParentHash(sibling, cur)
		}
		idx /= 2
	}
	return cur == root
}

// ErrMerkleLeafNotFound is returned when an account/currency has no
// leaf in the current balance tree (it never posted in that currency,
// so there is no committed balance to prove).
var ErrMerkleLeafNotFound = errors.New("ledger: account has no balance leaf in the current merkle tree")

// merkleLeavesLocked collects the canonical leaf list from the
// balances index: one leaf per (account, currency) key the ledger
// tracks, zero balances included. Callers must hold l.mu; the read
// lock suffices because the index is only read.
func (l *Ledger) merkleLeavesLocked() []MerkleLeaf {
	leaves := make([]MerkleLeaf, 0, len(l.balances))
	for k, balance := range l.balances {
		leaves = append(leaves, MerkleLeaf{
			Account:      k.account,
			Currency:     k.currency,
			BalanceCents: balance,
		})
	}
	return leaves
}

// merkleTreeLocked builds the balance Merkle tree over the current
// balances. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) merkleTreeLocked() *MerkleTree {
	return BuildMerkleTree(l.merkleLeavesLocked())
}

// SnapshotMerkleRoot returns the hex balance-Merkle root for the
// current ledger state together with the ledger version it was
// computed at. Both are read under one read lock, so the pair is
// consistent: the root commits to exactly the balances at that
// version.
func (l *Ledger) SnapshotMerkleRoot() (rootHex string, version uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.merkleTreeLocked().RootHex(), l.version
}

// MerkleProofFor returns the inclusion proof for one account's balance
// in one currency, the current root, and the ledger version — all read
// under one read lock so the proof is consistent with the root it
// verifies against. An empty currency means the default currency; any
// other code must be a valid ISO 4217 code (see normalizeCurrency).
// Accounts that never posted in the currency have no leaf and get
// ErrMerkleLeafNotFound: there is no committed balance to prove.
func (l *Ledger) MerkleProofFor(account AccountID, currency string) (proof MerkleProof, rootHex string, version uint64, err error) {
	if currency == "" {
		currency = DefaultCurrency
	} else if _, cerr := normalizeCurrency(currency); cerr != nil {
		return MerkleProof{}, "", 0, cerr
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	tree := l.merkleTreeLocked()
	idx := tree.LeafIndex(account, currency)
	if idx < 0 {
		return MerkleProof{}, "", 0, fmt.Errorf("%w: account %q currency %q", ErrMerkleLeafNotFound, account, currency)
	}
	proof, err = tree.Prove(idx)
	if err != nil {
		return MerkleProof{}, "", 0, err
	}
	return proof, tree.RootHex(), l.version, nil
}
