package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
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

// writeHashString writes s length-prefixed (8-byte big-endian length)
// into h, so adjacent hashed fields cannot be confused ("a"+"bc" vs
// "ab"+"c"). Shared by the entry chain (see hashChainLink) and the
// entry-link chain (see hashEntryLinkChainLink, links.go): the two
// chains use one canonical field encoding.
func writeHashString(h hash.Hash, s string) {
	var lb [8]byte
	binary.BigEndian.PutUint64(lb[:], uint64(len(s)))
	h.Write(lb[:])
	h.Write([]byte(s))
}

// writeHashInt writes n fixed-width big-endian into h. Shared like
// writeHashString.
func writeHashInt(h hash.Hash, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	h.Write(b[:])
}

// hashChainLink computes SHA-256(prevHash || canonical(entry)). Strings are
// length-prefixed so adjacent fields cannot be confused ("a"+"bc" vs
// "ab"+"c"); integers are fixed-width big-endian; CreatedAt uses UnixNano
// so the encoding is independent of time.Location. The memo is covered
// too: rewriting a journaled business note breaks the chain exactly like
// rewriting an amount (see LG-38). The external reference is covered
// like the memo (see LG-57): rewriting a journaled correlation
// identifier breaks the chain exactly like rewriting an amount. The
// attachments are covered like the memo (see LG-83): rewriting a
// journaled evidence reference breaks the chain exactly like rewriting
// an amount — the attachment count is hashed first so an empty list and
// a missing list hash identically, then each URI length-prefixed. The
// metadata is covered like the memo (see LG-112): keys in sorted order,
// then each key and value length-prefixed; entries without metadata skip
// the section entirely so their encoding stays byte-identical to the
// pre-metadata chain. The pending-review marker is covered
// as a single fixed byte: flipping a frozen row to effective (or vice
// versa) outside the approve path breaks the chain exactly like rewriting
// an amount (see LG-42). All inputs come from the standard library — no
// third-party crypto is involved.
func hashChainLink(prevHash [32]byte, e JournalEntry) [32]byte {
	h := sha256.New()
	h.Write(prevHash[:])
	writeString := func(s string) { writeHashString(h, s) }
	writeInt := func(n int64) { writeHashInt(h, n) }
	writeString(e.ID)
	writeString(string(e.DebitAccount))
	writeString(string(e.CreditAccount))
	writeInt(e.AmountCents)
	writeString(e.Currency)
	writeString(e.IdempotencyKey)
	writeString(e.Memo)
	writeString(e.ExternalRef)
	// The purpose code is covered like the metadata (see LG-112):
	// rewriting a journaled business purpose breaks the chain exactly
	// like rewriting an amount. Entries without a code skip the
	// section entirely, so their encoding stays byte-identical to the
	// pre-purpose-code chain.
	if e.PurposeCode != "" {
		writeString(e.PurposeCode)
	}
	writeInt(int64(len(e.Attachments)))
	for _, a := range e.Attachments {
		writeString(a)
	}
	// The metadata is covered like the memo (see LG-112): rewriting a
	// journaled reconciliation tag breaks the chain exactly like
	// rewriting an amount. Keys are hashed in sorted order so map
	// iteration order cannot change the digest. Entries without metadata
	// hash exactly as before — the section is skipped entirely, so the
	// encoding stays byte-identical to the pre-metadata chain.
	if len(e.Metadata) > 0 {
		keys := make([]string, 0, len(e.Metadata))
		for k := range e.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeInt(int64(len(keys)))
		for _, k := range keys {
			writeString(k)
			writeString(e.Metadata[k])
		}
	}
	if e.PendingReview {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
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
	// A fully pruned hot chain is empty, but the ledger still has
	// history (see chainPrunedTo): passing here would pretend the
	// chain is whole. Fail honestly and point at the archive path.
	if len(l.chain) == 0 && l.chainPrunedTo > 0 {
		return fmt.Errorf("ledger: audit chain is empty: the prefix up to seq %d was pruned to a segment archive; use VerifyChainWithArchives to verify the archived prefix",
			l.chainPrunedTo)
	}
	var prev [32]byte
	for i, link := range l.chain {
		if want := uint64(i + 1); link.seq != want {
			if i == 0 && link.seq > 1 {
				// The hot chain no longer starts at the genesis: its
				// cold prefix was pruned to a segment archive (see
				// ExportChainSegment / PruneChainPrefix) — or the
				// chain was tampered with. VerifyChain cannot check
				// the missing prefix, so it fails honestly instead of
				// pretending the chain is whole.
				return fmt.Errorf("ledger: audit chain starts at seq %d, not the genesis: if the prefix was pruned to a segment archive, use VerifyChainWithArchives to verify the archived prefix plus the hot chain",
					link.seq)
			}
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
	// The entry-link chain (see links.go, LG-123) verifies like the
	// entry chain: the declaration order, the endpoints, the type, and
	// the declaration metadata of every declared link recompute to the
	// recorded chain, so rewriting a declared link breaks verification
	// exactly like rewriting a journaled entry. The link chain has its
	// own genesis — it is independent of the entry chain, whose
	// links == version invariant link declarations never disturb.
	if len(l.linkChain) != len(l.entryLinks) {
		return fmt.Errorf("ledger: entry-link chain has %d links for %d declared entry links",
			len(l.linkChain), len(l.entryLinks))
	}
	var lprev [32]byte
	for i, lc := range l.linkChain {
		if want := uint64(i + 1); lc.seq != want {
			return fmt.Errorf("ledger: entry-link chain link %d carries seq %d, want %d (reordered chain)",
				i, lc.seq, want)
		}
		if lc.prevHash != lprev {
			return fmt.Errorf("ledger: entry-link chain link %d does not extend the previous link (spliced chain)", i)
		}
		link := l.entryLinks[i]
		if got := hashEntryLinkChainLink(lprev, link); got != lc.hash {
			return fmt.Errorf("ledger: entry link %q -> %q (%s) was modified after declaration (hash mismatch)",
				link.FromEntryID, link.ToEntryID, link.LinkType)
		}
		lprev = lc.hash
	}
	return nil
}
