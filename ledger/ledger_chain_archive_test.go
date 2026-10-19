package ledger

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// postArchiveEntry posts one journal entry with a caller-chosen ID (the
// shared seedChain helper uses fixed IDs, so it cannot post twice on the
// same ledger).
func postArchiveEntry(t *testing.T, l *Ledger, id string, amount int64) {
	t.Helper()
	e := chainEntry(id, amount, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post(%s) = dup=%v err=%v", id, dup, err)
	}
}

// rewriteChainArchiveLinks replaces the archive's link lines with links
// (used to simulate tampering).
func rewriteChainArchiveLinks(t *testing.T, path string, links []archivedChainLink) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	gz := gzip.NewWriter(fh)
	for _, link := range links {
		line, err := json.Marshal(link)
		if err != nil {
			t.Fatalf("marshal link: %v", err)
		}
		if _, err := gz.Write(append(line, '\n')); err != nil {
			t.Fatalf("write link: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := fh.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

func TestExportChainSegmentRoundTrip(t *testing.T) {
	l := New()
	seedChain(t, l, 5)
	dir := t.TempDir()

	archivePath, manifestPath, err := l.ExportChainSegment(1, 5, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if base := filepath.Base(archivePath); base != "chain-segment-1-5.jsonl.gz" {
		t.Errorf("archive name = %q, want chain-segment-1-5.jsonl.gz", base)
	}
	if base := filepath.Base(manifestPath); base != "chain-segment-1-5.manifest.json" {
		t.Errorf("manifest name = %q, want chain-segment-1-5.manifest.json", base)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m ChainSegmentManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Format != ChainSegmentFormat {
		t.Errorf("format = %q, want %q", m.Format, ChainSegmentFormat)
	}
	if m.FromSeq != 1 || m.ToSeq != 5 || m.LinkCount != 5 {
		t.Errorf("range = [%d..%d] count=%d, want [1..5] count=5", m.FromSeq, m.ToSeq, m.LinkCount)
	}
	if want := strings.Repeat("0", 64); m.SegmentHeadPrevHash != want {
		t.Errorf("segment_head_prev_hash = %q, want genesis zeros", m.SegmentHeadPrevHash)
	}
	head, _ := l.ChainHead()
	if m.SegmentTailHash != head {
		t.Errorf("segment_tail_hash = %q, want chain head %q", m.SegmentTailHash, head)
	}
	if m.ExportChainHeadSeq != 5 || m.ExportChainHeadLinks != 5 || m.ExportChainHeadHash != head {
		t.Errorf("export head = seq %d hash %q links %d, want seq 5 hash %q links 5",
			m.ExportChainHeadSeq, m.ExportChainHeadHash, m.ExportChainHeadLinks, head)
	}
	if m.ExportedAt.IsZero() {
		t.Error("exported_at is zero")
	}

	// The archive holds 5 links, seqs 1..5.
	links, err := readChainSegmentLinks(archivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(links) != 5 {
		t.Fatalf("archive holds %d links, want 5", len(links))
	}
	for i, link := range links {
		if link.Seq != uint64(i+1) {
			t.Errorf("link %d seq = %d, want %d", i, link.Seq, i+1)
		}
		if len(link.Hash) != 64 || len(link.PrevHash) != 64 {
			t.Errorf("link %d hashes are not 64 hex chars", i)
		}
	}

	// Prune the archived prefix, then the archive plus the (now empty)
	// hot chain must verify as one chain.
	if err := l.PruneChainPrefix(5); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	if err := l.VerifyChainWithArchives(archivePath); err != nil {
		t.Fatalf("VerifyChainWithArchives = %v", err)
	}
	if _, n := l.ChainHead(); n != 0 {
		t.Errorf("hot chain has %d links after full prune, want 0", n)
	}
	// The journal entries survive the prune: the books are intact.
	if len(l.entries) != 5 {
		t.Errorf("journal has %d entries after prune, want 5", len(l.entries))
	}
}

func TestVerifyChainWithArchivesBeforePrune(t *testing.T) {
	// Exporting does not touch the hot chain: the archive plus the
	// still-whole hot chain must verify, with the covered links skipped
	// (the archive is authoritative for them).
	l := New()
	seedChain(t, l, 5)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(1, 5, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.VerifyChainWithArchives(archivePath); err != nil {
		t.Fatalf("VerifyChainWithArchives before prune = %v", err)
	}
}

func TestExportChainSegmentPartialAndContinue(t *testing.T) {
	l := New()
	seedChain(t, l, 5)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.PruneChainPrefix(3); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	// The remaining hot links stitch onto the archive tail.
	if err := l.VerifyChainWithArchives(archivePath); err != nil {
		t.Fatalf("VerifyChainWithArchives = %v", err)
	}
	// New posts keep working on the pruned chain and verify too.
	postArchiveEntry(t, l, "e-arch-6", 200)
	postArchiveEntry(t, l, "e-arch-7", 300)
	if err := l.VerifyChainWithArchives(archivePath); err != nil {
		t.Fatalf("VerifyChainWithArchives after new posts = %v", err)
	}
	if _, n := l.ChainHead(); n != 4 {
		t.Errorf("hot chain has %d links, want 4 (2 old + 2 new)", n)
	}
}

func TestChainArchiveTamperDetection(t *testing.T) {
	l := New()
	seedChain(t, l, 3)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}

	// Tamper 1: rewrite a middle link's hash in the archive.
	links, err := readChainSegmentLinks(archivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	bad := links[1].Hash
	if bad[0] == 'a' {
		links[1].Hash = "b" + bad[1:]
	} else {
		links[1].Hash = "a" + bad[1:]
	}
	rewriteChainArchiveLinks(t, archivePath, links)
	if err := l.VerifyChainWithArchives(archivePath); err == nil {
		t.Fatal("VerifyChainWithArchives accepted a tampered link hash")
	} else if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tamper error = %q, want a hash-mismatch report", err)
	}
}

func TestChainArchiveManifestMismatch(t *testing.T) {
	l := New()
	seedChain(t, l, 3)
	dir := t.TempDir()
	archivePath, manifestPath, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	// Corrupt the manifest's tail hash: the manifest must not disagree
	// with the archive it describes.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	raw["segment_tail_hash"] = strings.Repeat("f", 64)
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := l.VerifyChainWithArchives(archivePath); err == nil {
		t.Fatal("VerifyChainWithArchives accepted a manifest that disagrees with its archive")
	}
}

func TestChainArchiveSpliceDetection(t *testing.T) {
	l := New()
	seedChain(t, l, 5)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.PruneChainPrefix(3); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	// Splice the hot chain: the first hot link no longer extends the
	// archive tail.
	l.mu.Lock()
	l.chain[0].prevHash[0] ^= 0xff
	l.mu.Unlock()
	if err := l.VerifyChainWithArchives(archivePath); err == nil {
		t.Fatal("VerifyChainWithArchives accepted a spliced hot chain")
	} else if !strings.Contains(err.Error(), "spliced") {
		t.Fatalf("splice error = %q, want a splice report", err)
	}
}

func TestPruneChainPrefixHonestVerifyChain(t *testing.T) {
	l := New()
	seedChain(t, l, 3)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.PruneChainPrefix(3); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	// VerifyChain must fail honestly: it cannot see the archived
	// prefix, and must say so instead of passing.
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed on a pruned chain, want an honest failure")
	} else if !strings.Contains(err.Error(), "VerifyChainWithArchives") {
		t.Fatalf("VerifyChain error = %q, want guidance toward VerifyChainWithArchives", err)
	}
	// The archive path still verifies.
	if err := l.VerifyChainWithArchives(archivePath); err != nil {
		t.Fatalf("VerifyChainWithArchives = %v", err)
	}
}

func TestPruneChainPrefixRequiresArchive(t *testing.T) {
	l := New()
	seedChain(t, l, 4)
	dir := t.TempDir()

	// No archive at all: the prune is refused.
	if err := l.PruneChainPrefix(2); err == nil {
		t.Fatal("PruneChainPrefix without an archive succeeded")
	}

	// Archive covers [1..2] but the prune wants [1..3]: refused.
	if _, _, err := l.ExportChainSegment(1, 2, dir); err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.PruneChainPrefix(3); err == nil {
		t.Fatal("PruneChainPrefix beyond the archived range succeeded")
	}
	// The archived prefix itself prunes fine.
	if err := l.PruneChainPrefix(2); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
}

func TestExportChainSegmentValidation(t *testing.T) {
	dir := t.TempDir()
	l := New()
	if _, _, err := l.ExportChainSegment(1, 1, dir); err == nil {
		t.Error("ExportChainSegment on an empty chain succeeded")
	}
	seedChain(t, l, 3)
	for _, tc := range []struct {
		name string
		from uint64
		to   uint64
	}{
		{"from zero", 0, 2},
		{"from > to", 3, 2},
		{"to beyond head", 1, 4},
		{"from beyond head", 5, 6},
	} {
		if _, _, err := l.ExportChainSegment(tc.from, tc.to, dir); err == nil {
			t.Errorf("ExportChainSegment(%s) succeeded, want an error", tc.name)
		}
	}
	// A single-link export works, and a pruned-away range cannot be
	// re-exported from memory.
	if _, _, err := l.ExportChainSegment(2, 2, dir); err != nil {
		t.Fatalf("ExportChainSegment(2,2) = %v", err)
	}
	if _, _, err := l.ExportChainSegment(1, 2, dir); err != nil {
		t.Fatalf("ExportChainSegment(1,2) = %v", err)
	}
	if err := l.PruneChainPrefix(2); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	if _, _, err := l.ExportChainSegment(1, 2, dir); err == nil {
		t.Error("ExportChainSegment of a pruned range succeeded")
	}
}

func TestVerifyChainWithArchivesNonGenesisStart(t *testing.T) {
	// An archive that does not start at seq 1 cannot anchor to the
	// genesis: verification must say so instead of passing.
	l := New()
	seedChain(t, l, 5)
	dir := t.TempDir()
	archivePath, _, err := l.ExportChainSegment(2, 5, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment = %v", err)
	}
	if err := l.VerifyChainWithArchives(archivePath); err == nil {
		t.Fatal("VerifyChainWithArchives accepted an archive that does not start at the genesis")
	} else if !strings.Contains(err.Error(), "genesis") {
		t.Fatalf("error = %q, want a genesis-anchoring report", err)
	}
}

func TestVerifyChainWithArchivesMultipleSegments(t *testing.T) {
	l := New()
	seedChain(t, l, 6)
	dir := t.TempDir()
	a1, _, err := l.ExportChainSegment(1, 3, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment(1,3) = %v", err)
	}
	a2, _, err := l.ExportChainSegment(4, 6, dir)
	if err != nil {
		t.Fatalf("ExportChainSegment(4,6) = %v", err)
	}
	if err := l.PruneChainPrefix(6); err != nil {
		t.Fatalf("PruneChainPrefix = %v", err)
	}
	// Both orders verify: the archives are stitched oldest-first.
	for _, paths := range [][]string{{a1, a2}, {a2, a1}} {
		if err := l.VerifyChainWithArchives(paths...); err != nil {
			t.Fatalf("VerifyChainWithArchives(%v) = %v", paths, err)
		}
	}
	if got := l.ChainArchives(); len(got) != 2 {
		t.Errorf("ChainArchives = %v, want 2 records", got)
	}
}

func TestVerifyChainWithArchivesNoArchivesDegrades(t *testing.T) {
	// No archives: ordinary genesis verification, intact and broken.
	l := New()
	seedChain(t, l, 3)
	if err := l.VerifyChainWithArchives(); err != nil {
		t.Fatalf("VerifyChainWithArchives() = %v", err)
	}
	l.mu.Lock()
	l.chain[1].prevHash[0] ^= 0xff
	l.mu.Unlock()
	if err := l.VerifyChainWithArchives(); err == nil {
		t.Fatal("VerifyChainWithArchives() accepted a broken chain")
	}
}
