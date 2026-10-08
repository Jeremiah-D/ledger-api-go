package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// chainLink is one link in the ledger's tamper-evident audit chain. Every
// successfully posted entry appends exactly one link, in Post order, so the
// chain is an append-only record of the journal: links are never modified
// or removed.
//
// Hash is SHA-256 over (prevHash || canonical(entry)), where canonical is a
// length-prefixed encoding of the entry's fields; PrevHash is the previous
// link's Hash (32 zero bytes for the first link, the "genesis"). Rewriting
// any journaled field, deleting a link, or reordering/splicing the chain
// breaks the PrevHash/Hash continuity, and VerifyChain detects it by
// recomputation.
type chainLink struct {
	seq      uint64 // 1-based position in the chain == ledger version at Post time
	entryID  string
	prevHash [32]byte
	hash     [32]byte
}

// hashChainLink computes SHA-256(prevHash || canonical(entry)). Strings are
// length-prefixed so adjacent fields cannot be confused ("a"+"bc" vs
// "ab"+"c"); integers are fixed-width big-endian; CreatedAt uses UnixNano
// so the encoding is independent of time.Location. All inputs come from the
// standard library — no third-party crypto is involved.
func hashChainLink(prevHash [32]byte, e JournalEntry) [32]byte {
	h := sha256.New()
	h.Write(prevHash[:])
	writeString := func(s string) {
		var lb [8]byte
		binary.BigEndian.PutUint64(lb[:], uint64(len(s)))
		h.Write(lb[:])
		h.Write([]byte(s))
	}
	writeInt := func(n int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		h.Write(b[:])
	}
	writeString(e.ID)
	writeString(string(e.DebitAccount))
	writeString(string(e.CreditAccount))
	writeInt(e.AmountCents)
	writeString(e.Currency)
	writeString(e.IdempotencyKey)
	writeInt(e.CreatedAt.UnixNano())
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// appendChainLink records e as a new link at the head of the audit chain.
// The entry must already carry its final CreatedAt. Callers must hold l.mu.
func (l *Ledger) appendChainLink(e JournalEntry) {
	var prevHash [32]byte
	if n := len(l.chain); n > 0 {
		prevHash = l.chain[n-1].hash
	}
	l.chain = append(l.chain, chainLink{
		seq:      l.version, // version was just bumped for this Post
		entryID:  e.ID,
		prevHash: prevHash,
		hash:     hashChainLink(prevHash, e),
	})
}

// ChainHead returns the head of the audit chain: the hex-encoded SHA-256 of
// the newest link and the number of links. An empty ledger reports the
// 32-zero-byte genesis hash and 0 links. The head moves if and only if a
// new entry was posted, so operators can poll it as a cheap journal-change
// detector; GET /entries/verify recomputes the whole chain for a full
// integrity check.
func (l *Ledger) ChainHead() (head string, links uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n := len(l.chain); n > 0 {
		return hex.EncodeToString(l.chain[n-1].hash[:]), uint64(n)
	}
	var genesis [32]byte
	return hex.EncodeToString(genesis[:]), 0
}

// VerifyChain recomputes the audit chain from the genesis and reports the
// first inconsistency it finds: a link whose PrevHash does not extend the
// previous link's Hash (a splice), a journal entry missing from the ledger,
// or an entry whose fields no longer hash to the recorded link (a rewrite
// after posting). It returns nil when the chain is intact. The check is
// read-only; it holds the ledger's read lock so a concurrent Post cannot
// race it, at the cost of blocking writers for the duration of the scan.
func (l *Ledger) VerifyChain() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.verifyChainLocked()
}

// verifyChainLocked recomputes the audit chain from the genesis and reports
// the first inconsistency it finds (see VerifyChain). Callers must hold
// l.mu; the read lock suffices because the check mutates nothing. The
// reconciliation scan calls this directly so the chain check runs under the
// same read lock as the rest of the report, keeping the snapshot consistent.
func (l *Ledger) verifyChainLocked() error {
	var prev [32]byte
	for i, link := range l.chain {
		if want := uint64(i + 1); link.seq != want {
			return fmt.Errorf("ledger: audit chain link %d carries seq %d, want %d (reordered chain)",
				i, link.seq, want)
		}
		if link.prevHash != prev {
			return fmt.Errorf("ledger: audit chain link %q does not extend the previous link (spliced chain)",
				link.entryID)
		}
		e, ok := l.entries[link.entryID]
		if !ok {
			return fmt.Errorf("ledger: audit chain references missing journal entry %q", link.entryID)
		}
		if got := hashChainLink(prev, e); got != link.hash {
			return fmt.Errorf("ledger: journal entry %q was modified after posting (hash mismatch)", link.entryID)
		}
		prev = link.hash
	}
	return nil
}
