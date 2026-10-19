package ledger

import (
	"bufio"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Chain segment archives (LG-136): cold storage for the audit chain's
// head-pruned prefix. A busy ledger's chain grows one link per Post
// forever; keeping every link in memory is fine for the books but not
// for very long histories. ExportChainSegment writes a seq range to a
// gzip JSONL archive plus a JSON manifest, VerifyChainWithArchives
// re-verifies archives plus the live hot chain as one continuous chain,
// and PruneChainPrefix drops the archived prefix from memory. The
// journal entries themselves are never deleted — balances, statements,
// and reconciliation keep working; only the in-memory links go, and the
// archive carries everything needed to re-verify them later.

// ChainSegmentFormat is the manifest format marker for chain segment
// archives.
const ChainSegmentFormat = "chain-segment/1"

// archivedChainLink is one chain link as stored in a segment archive:
// one JSON object per line, hashes hex-encoded (32-byte hashes are not
// JSON-friendly raw).
type archivedChainLink struct {
	Seq      uint64 `json:"seq"`
	EntryID  string `json:"entry_id"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// ChainSegmentManifest describes one exported segment archive. FromSeq
// and ToSeq are 1-based chain seqs (the ledger version at Post time).
// An operator archives [from_seq..to_seq]; the manifest pins exactly
// what was exported so a later verification can check the archive was
// not swapped or truncated.
type ChainSegmentManifest struct {
	Format string `json:"format"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	// LinkCount is the number of links in the archive; it must equal
	// ToSeq-FromSeq+1.
	LinkCount uint64 `json:"link_count"`
	// SegmentHeadPrevHash is the first link's PrevHash — the 32-zero-byte
	// genesis marker when FromSeq is 1, otherwise the hash of the
	// previous (already archived or still hot) link.
	SegmentHeadPrevHash string `json:"segment_head_prev_hash"`
	// SegmentTailHash is the last link's hash. An external LG-44 anchor
	// checkpoint can witness this hash to attest the segment existed at
	// anchor time: the checkpoint signs (seq, headHash), and
	// segment_tail_hash is exactly the head hash of the chain the
	// segment covers — anchor the segment tail like anchoring a chain
	// head (see anchor.go).
	SegmentTailHash string `json:"segment_tail_hash"`
	// ExportChainHead pins the live chain head at export time: the
	// archive was taken while the chain head was this (seq, hash) with
	// this many hot links. Informational — later Posts move the head
	// without invalidating the archive.
	ExportChainHeadSeq   uint64 `json:"export_chain_head_seq"`
	ExportChainHeadHash  string `json:"export_chain_head_hash"`
	ExportChainHeadLinks uint64 `json:"export_chain_head_links"`
	ExportedAt time.Time `json:"exported_at"`
}

// chainArchiveRecord remembers one export the ledger performed, in
// memory, so PruneChainPrefix can demand that the prefix it is about to
// drop is backed by a verifiable archive. The record is not persisted:
// after a restart the operator re-exports (or keeps the archive files
// and re-verifies them explicitly) before pruning.
type chainArchiveRecord struct {
	fromSeq      uint64
	toSeq        uint64
	archivePath  string
	manifestPath string
}

// ExportChainSegment writes the chain links with seq in
// [fromSeq..toSeq] to dir as a gzip JSONL archive plus a JSON manifest:
// chain-segment-<from>-<to>.jsonl.gz and
// chain-segment-<from>-<to>.manifest.json. The snapshot is taken under
// the read lock and the files are written after the lock is released,
// so a concurrent Post is blocked only for the slice copy, not for the
// (slow) compression and disk write. The export is read-only: the hot
// chain is untouched.
//
// fromSeq must be >= 1 and inside the hot chain, fromSeq <= toSeq, and
// toSeq <= the current head seq. Exporting a range whose prefix was
// already pruned is an error — the cold data lives in the archives, not
// in memory; archive it once and prune, in that order.
func (l *Ledger) ExportChainSegment(fromSeq, toSeq uint64, dir string) (archivePath, manifestPath string, err error) {
	if fromSeq < 1 {
		return "", "", fmt.Errorf("ledger: export chain segment: fromSeq %d < 1", fromSeq)
	}
	if fromSeq > toSeq {
		return "", "", fmt.Errorf("ledger: export chain segment: fromSeq %d > toSeq %d", fromSeq, toSeq)
	}

	l.mu.RLock()
	// Copy the segment under the read lock; everything slow happens
	// after the unlock.
	var seg []chainLink
	var headSeq uint64
	var headHash [32]byte
	var hotLinks uint64
	if n := len(l.chain); n > 0 {
		first, last := l.chain[0].seq, l.chain[n-1].seq
		if fromSeq < first {
			l.mu.RUnlock()
			return "", "", fmt.Errorf("ledger: export chain segment: seq %d already pruned from the hot chain (starts at %d)", fromSeq, first)
		}
		if toSeq > last {
			l.mu.RUnlock()
			return "", "", fmt.Errorf("ledger: export chain segment: toSeq %d beyond the chain head %d", toSeq, last)
		}
		seg = append([]chainLink(nil), l.chain[fromSeq-first:toSeq-first+1]...)
		headSeq, headHash, hotLinks = last, l.chain[n-1].hash, uint64(n)
	} else {
		l.mu.RUnlock()
		return "", "", fmt.Errorf("ledger: export chain segment: chain is empty, nothing to export")
	}
	l.mu.RUnlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("ledger: export chain segment: %w", err)
	}
	base := fmt.Sprintf("chain-segment-%d-%d", fromSeq, toSeq)
	archivePath = filepath.Join(dir, base+".jsonl.gz")
	manifestPath = filepath.Join(dir, base+".manifest.json")

	if err := writeChainSegmentArchive(archivePath, seg); err != nil {
		return "", "", err
	}
	manifest := ChainSegmentManifest{
		Format:               ChainSegmentFormat,
		FromSeq:              fromSeq,
		ToSeq:                toSeq,
		LinkCount:            uint64(len(seg)),
		SegmentHeadPrevHash:  hex.EncodeToString(seg[0].prevHash[:]),
		SegmentTailHash:      hex.EncodeToString(seg[len(seg)-1].hash[:]),
		ExportChainHeadSeq:   headSeq,
		ExportChainHeadHash:  hex.EncodeToString(headHash[:]),
		ExportChainHeadLinks: hotLinks,
		ExportedAt:           time.Now().UTC(),
	}
	if err := writeChainSegmentManifest(manifestPath, manifest); err != nil {
		_ = os.Remove(archivePath)
		return "", "", err
	}

	// Remember the export so PruneChainPrefix can require the dropped
	// prefix to be backed by exactly this archive.
	l.mu.Lock()
	l.chainArchives = append(l.chainArchives, chainArchiveRecord{
		fromSeq:      fromSeq,
		toSeq:        toSeq,
		archivePath:  archivePath,
		manifestPath: manifestPath,
	})
	l.mu.Unlock()
	return archivePath, manifestPath, nil
}

// writeChainSegmentArchive writes the links as gzip JSONL, one link per
// line, and fsyncs before returning: a segment archive is a compliance
// artifact, and a torn tail would fail verification later. On error the
// partial file is removed.
func writeChainSegmentArchive(path string, seg []chainLink) error {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	gz := gzip.NewWriter(fh)
	w := bufio.NewWriter(gz)
	for _, link := range seg {
		line, err := json.Marshal(archivedChainLink{
			Seq:      link.seq,
			EntryID:  link.entryID,
			PrevHash: hex.EncodeToString(link.prevHash[:]),
			Hash:     hex.EncodeToString(link.hash[:]),
		})
		if err != nil {
			gz.Close()
			fh.Close()
			cleanup()
			return fmt.Errorf("ledger: export chain segment: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			gz.Close()
			fh.Close()
			cleanup()
			return fmt.Errorf("ledger: export chain segment: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		gz.Close()
		fh.Close()
		cleanup()
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	if err := gz.Close(); err != nil {
		fh.Close()
		cleanup()
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		cleanup()
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	if err := fh.Close(); err != nil {
		cleanup()
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	return nil
}

// writeChainSegmentManifest writes the manifest as indented JSON and
// fsyncs: operators read these by hand during audits.
func writeChainSegmentManifest(path string, m ChainSegmentManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	fh, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	defer fh.Close()
	if err := fh.Sync(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("ledger: export chain segment: %w", err)
	}
	return nil
}

// VerifyChainWithArchives re-verifies the audit chain when its cold
// prefix lives in segment archives: each archive is checked internally
// (seq continuity, prevHash continuity, and every link's hash recomputed
// from the in-memory journal — the journal entries are kept even after
// PruneChainPrefix), archives are stitched to each other by
// segment_head_prev_hash == previous segment_tail_hash (the first
// archive must start at the genesis when its FromSeq is 1), and the
// live hot chain is stitched onto the last archive's tail. With no
// archives it degrades to the ordinary VerifyChain genesis check. An
// empty hot chain verifies too, when the archives cover the whole
// history.
func (l *Ledger) VerifyChainWithArchives(archivePaths ...string) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.verifyChainWithArchivesLocked(archivePaths)
}

// verifyChainWithArchivesLocked implements VerifyChainWithArchives.
// Callers must hold l.mu (read or write).
func (l *Ledger) verifyChainWithArchivesLocked(archivePaths []string) error {
	if len(archivePaths) == 0 {
		// No archives: the hot chain must stand on its own from the
		// genesis, exactly like VerifyChain. Note the entry-link chain
		// (links.go) is covered here too, via verifyChainLocked.
		return l.verifyChainLocked()
	}

	archives, err := loadChainSegments(archivePaths)
	if err != nil {
		return err
	}
	// Stitch archives oldest-first; the path order given by the caller
	// is not trusted.
	sort.Slice(archives, func(i, j int) bool { return archives[i].manifest.FromSeq < archives[j].manifest.FromSeq })
	var prevTail [32]byte
	for i, a := range archives {
		if err := a.checkInternal(l); err != nil {
			return err
		}
		if i == 0 {
			if a.manifest.FromSeq != 1 {
				return fmt.Errorf("ledger: chain archive %s starts at seq %d: cannot anchor to the genesis without its predecessor archives",
					a.archivePath, a.manifest.FromSeq)
			}
		} else if a.headPrev != prevTail {
			return fmt.Errorf("ledger: chain archive %s does not extend the previous archive (spliced archive set)", a.archivePath)
		}
		prevTail = a.tail
	}

	// Stitch the hot chain onto the last archive's tail. Hot links the
	// archives already cover (seq <= last ToSeq) are skipped — the
	// archive, already re-verified against the journal above, is
	// authoritative for them, so verifying works both before pruning
	// (export taken, hot chain still whole) and after. The first hot
	// link beyond the archived range must extend the archive tail; an
	// empty remainder is fine when the archives cover the whole
	// history.
	if len(l.chain) > 0 {
		lastTo := archives[len(archives)-1].manifest.ToSeq
		k := 0
		for k < len(l.chain) && l.chain[k].seq <= lastTo {
			k++
		}
		var prev [32]byte
		if k < len(l.chain) {
			// The hot chain is contiguous by construction (Post
			// appends sequentially, prune drops a prefix), so the
			// first uncovered link must follow the archive tail
			// exactly — no gaps, no overlap beyond the skip.
			if l.chain[k].seq != lastTo+1 {
				return fmt.Errorf("ledger: audit chain link %q (seq %d) does not follow the last archive (ends at seq %d)",
					l.chain[k].entryID, l.chain[k].seq, lastTo)
			}
			prev = prevTail
			for i := k; i < len(l.chain); i++ {
				link := l.chain[i]
				if want := l.chain[k].seq + uint64(i-k); link.seq != want {
					return fmt.Errorf("ledger: audit chain link %d carries seq %d, want %d (reordered chain)",
						i, link.seq, want)
				}
				if link.prevHash != prev {
					return fmt.Errorf("ledger: audit chain link %q does not extend the archive tail (spliced hot chain)",
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
		}
	}
	return nil
}

// chainSegment is one loaded archive: its manifest plus the parsed
// links, with the head-prev and tail hashes decoded for stitching.
type chainSegment struct {
	archivePath string
	manifest    ChainSegmentManifest
	links       []archivedChainLink
	headPrev    [32]byte
	tail        [32]byte
}

// loadChainSegments reads each archive and its sibling manifest
// (chain-segment-<from>-<to>.manifest.json next to the .jsonl.gz) and
// checks the manifest against the archive it describes: the format
// marker, the seq range, the link count, and the head/tail hashes.
func loadChainSegments(paths []string) ([]chainSegment, error) {
	out := make([]chainSegment, 0, len(paths))
	for _, path := range paths {
		manifestPath := strings.TrimSuffix(path, ".jsonl.gz") + ".manifest.json"
		mdata, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("ledger: chain archive %s: read manifest: %w", path, err)
		}
		var m ChainSegmentManifest
		if err := json.Unmarshal(mdata, &m); err != nil {
			return nil, fmt.Errorf("ledger: chain archive %s: bad manifest: %w", path, err)
		}
		if m.Format != ChainSegmentFormat {
			return nil, fmt.Errorf("ledger: chain archive %s: manifest format %q, want %q", path, m.Format, ChainSegmentFormat)
		}
		links, err := readChainSegmentLinks(path)
		if err != nil {
			return nil, err
		}
		if uint64(len(links)) != m.LinkCount || m.LinkCount != m.ToSeq-m.FromSeq+1 {
			return nil, fmt.Errorf("ledger: chain archive %s: manifest covers [%d..%d] (%d links) but the archive holds %d links",
				path, m.FromSeq, m.ToSeq, m.LinkCount, len(links))
		}
		headPrev, err := decodeHashField(m.SegmentHeadPrevHash)
		if err != nil {
			return nil, fmt.Errorf("ledger: chain archive %s: manifest segment_head_prev_hash: %v", path, err)
		}
		tail, err := decodeHashField(m.SegmentTailHash)
		if err != nil {
			return nil, fmt.Errorf("ledger: chain archive %s: manifest segment_tail_hash: %v", path, err)
		}
		if len(links) > 0 {
			if links[0].Seq != m.FromSeq || links[len(links)-1].Seq != m.ToSeq {
				return nil, fmt.Errorf("ledger: chain archive %s: links span [%d..%d], manifest says [%d..%d]",
					path, links[0].Seq, links[len(links)-1].Seq, m.FromSeq, m.ToSeq)
			}
			firstPrev, err := decodeHashField(links[0].PrevHash)
			if err != nil || firstPrev != headPrev {
				return nil, fmt.Errorf("ledger: chain archive %s: first link's prev_hash does not match the manifest", path)
			}
			lastHash, err := decodeHashField(links[len(links)-1].Hash)
			if err != nil || lastHash != tail {
				return nil, fmt.Errorf("ledger: chain archive %s: last link's hash does not match the manifest", path)
			}
		}
		out = append(out, chainSegment{archivePath: path, manifest: m, links: links, headPrev: headPrev, tail: tail})
	}
	return out, nil
}

// decodeHashField decodes a 64-hex-char hash field into 32 bytes.
func decodeHashField(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("want 64 hex chars, got %q", s)
	}
	copy(out[:], b)
	return out, nil
}

// readChainSegmentLinks gunzips the archive and parses one JSON link
// per line.
func readChainSegmentLinks(path string) ([]archivedChainLink, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: chain archive %s: %w", path, err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return nil, fmt.Errorf("ledger: chain archive %s: gunzip: %w", path, err)
	}
	defer gz.Close()
	var links []archivedChainLink
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var link archivedChainLink
		if err := json.Unmarshal(line, &link); err != nil {
			return nil, fmt.Errorf("ledger: chain archive %s: line %d: %w", path, lineNo, err)
		}
		links = append(links, link)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ledger: chain archive %s: scan: %w", path, err)
	}
	return links, nil
}

// checkInternal verifies one archive's internal continuity: seqs are
// contiguous over the manifest's range, every link's prevHash extends
// the previous link's hash, and every link's hash recomputes from the
// in-memory journal entry. Callers must hold l.mu.
func (a *chainSegment) checkInternal(l *Ledger) error {
	var prev [32]byte
	copy(prev[:], a.headPrev[:])
	for i, link := range a.links {
		if want := a.manifest.FromSeq + uint64(i); link.Seq != want {
			return fmt.Errorf("ledger: chain archive %s: link %d carries seq %d, want %d (reordered archive)",
				a.archivePath, i, link.Seq, want)
		}
		prevHash, err := decodeHashField(link.PrevHash)
		if err != nil || prevHash != prev {
			return fmt.Errorf("ledger: chain archive %s: link seq %d does not extend the previous link (spliced archive)",
				a.archivePath, link.Seq)
		}
		hash, err := decodeHashField(link.Hash)
		if err != nil {
			return fmt.Errorf("ledger: chain archive %s: link seq %d: bad hash field: %v", a.archivePath, link.Seq, err)
		}
		e, ok := l.entries[link.EntryID]
		if !ok {
			return fmt.Errorf("ledger: chain archive %s: references missing journal entry %q", a.archivePath, link.EntryID)
		}
		if got := hashChainLink(prev, e); got != hash {
			return fmt.Errorf("ledger: chain archive %s: journal entry %q was modified after posting (hash mismatch)",
				a.archivePath, link.EntryID)
		}
		prev = hash
	}
	return nil
}

// PruneChainPrefix drops the in-memory chain links with seq <= toSeq,
// keeping the journal entries: balances, statements, and
// reconciliation are unaffected. The prefix must be backed by archives
// exported from this process (see ExportChainSegment): the archives are
// re-verified against the live chain before anything is dropped, so a
// corrupt or missing archive refuses the prune instead of destroying
// the only verifiable copy. After pruning, VerifyChain honestly fails —
// the hot chain no longer starts at the genesis — and
// VerifyChainWithArchives is the way to verify.
func (l *Ledger) PruneChainPrefix(toSeq uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.chain) == 0 {
		return fmt.Errorf("ledger: prune chain prefix: chain is empty, nothing to prune")
	}
	first, last := l.chain[0].seq, l.chain[len(l.chain)-1].seq
	if toSeq < first {
		return fmt.Errorf("ledger: prune chain prefix: seq %d is not in the hot chain [%d..%d]", toSeq, first, last)
	}
	if toSeq > last {
		return fmt.Errorf("ledger: prune chain prefix: toSeq %d beyond the chain head %d", toSeq, last)
	}

	// Collect the archive records covering [1..toSeq], oldest first.
	// Only archives exported by this process count: an operator-hand
	// archive could cover the range without the ledger having verified
	// it at export time.
	recs := append([]chainArchiveRecord(nil), l.chainArchives...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].fromSeq < recs[j].fromSeq })
	var paths []string
	next := uint64(1)
	for _, r := range recs {
		if r.fromSeq > next {
			break // gap: the covered prefix ends here
		}
		if r.toSeq >= next {
			paths = append(paths, r.archivePath)
			next = r.toSeq + 1
		}
	}
	if next <= toSeq {
		return fmt.Errorf("ledger: prune chain prefix: no verified archive covers the prefix [1..%d] (covered up to %d); export the range first",
			toSeq, next-1)
	}
	if err := l.verifyChainWithArchivesLocked(paths); err != nil {
		return fmt.Errorf("ledger: prune chain prefix: archive verification failed, refusing to drop the prefix: %w", err)
	}

	l.chain = append([]chainLink(nil), l.chain[toSeq-first+1:]...)
	if toSeq > l.chainPrunedTo {
		l.chainPrunedTo = toSeq
	}
	return nil
}

// ChainArchives reports the segment archives this process exported
// (from/to seq ranges), oldest first. In-memory only; see
// chainArchiveRecord.
func (l *Ledger) ChainArchives() [][2]uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([][2]uint64, 0, len(l.chainArchives))
	for _, r := range l.chainArchives {
		out = append(out, [2]uint64{r.fromSeq, r.toSeq})
	}
	return out
}
