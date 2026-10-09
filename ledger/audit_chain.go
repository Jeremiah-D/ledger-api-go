package ledger

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AuditGenesisPrevHash is the PrevHash carried by the first sealed
// audit entry: a fixed all-zero 64-hex marker. It makes the chain's
// start explicit and unambiguous in verification output.
const AuditGenesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// auditCanonical is the fixed-field-order serialization an audit entry's
// seal is computed over. It carries every AuditEvent field except Hash
// (the seal itself) — PrevHash is folded in separately, per the seal
// definition below. encoding/json marshals struct fields in declaration
// order and sorts map keys, so the bytes are deterministic for a given
// event; the verifier rebuilds this exact shape from the parsed line.
type auditCanonical struct {
	TS            string         `json:"ts"`
	Op            string         `json:"op"`
	Actor         string         `json:"actor"`
	TraceID       string         `json:"trace_id"`
	VersionBefore uint64         `json:"version_before"`
	VersionAfter  uint64         `json:"version_after"`
	EntryIDs      []string       `json:"entry_ids,omitempty"`
	Accounts      []string       `json:"accounts,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
}

// sealAuditEvent computes the hex SHA-256 seal of a sealed event:
// SHA-256(canonical(ev) || prevHash), standard library only. The
// timestamp is normalized to UTC RFC3339Nano so the emit-time and
// verify-time serializations agree regardless of the process timezone
// the event was created in. ev.PrevHash must already be assigned.
func sealAuditEvent(ev AuditEvent) string {
	accts := make([]string, len(ev.Accounts))
	for i, a := range ev.Accounts {
		accts[i] = string(a)
	}
	canon, err := json.Marshal(auditCanonical{
		TS:            ev.Timestamp.UTC().Format(time.RFC3339Nano),
		Op:            ev.Op,
		Actor:         ev.Actor,
		TraceID:       ev.TraceID,
		VersionBefore: ev.VersionBefore,
		VersionAfter:  ev.VersionAfter,
		EntryIDs:      ev.EntryIDs,
		Accounts:      accts,
		Details:       ev.Details,
	})
	if err != nil {
		// The struct is fully JSON-marshalable by construction
		// (map values came from JSON-native Go values); this is
		// unreachable in practice. An empty canonical form still
		// seals deterministically rather than panicking.
		canon = []byte("{}")
	}
	h := sha256.New()
	h.Write(canon)
	h.Write([]byte(ev.PrevHash))
	return hex.EncodeToString(h.Sum(nil))
}

// AuditChainBreak pinpoints the first place a hash-chain verification
// found the audit log broken. EntryID is the entry's trace_id.
// Reason is one of:
//   - "missing_hash": the entry carries no seal (rewritten by a tool
//     that stripped the chain fields, or a pre-chain-format line).
//   - "prev_mismatch": the entry's PrevHash does not match the previous
//     entry's seal — an entry was deleted, or files were reordered.
//   - "hash_mismatch": the entry links correctly but its content no
//     longer matches its seal — the entry was edited in place.
type AuditChainBreak struct {
	File         string `json:"file"`
	Line         int    `json:"line"`
	EntryID      string `json:"entry_id"`
	ExpectedPrev string `json:"expected_prev"`
	ActualPrev   string `json:"actual_prev"`
	Reason       string `json:"reason"`
}

// AuditVerifyReport is the result of VerifyAuditLog. Ok is true only
// when every parsed entry's seal recomputed cleanly. CheckedEntries
// counts verified entries (up to the first break); SkippedLines counts
// corrupt (unparseable) lines, which are disclosed but are not breaks —
// a torn write can only ever corrupt its own line. FilesChecked counts
// the audit files walked, oldest first, including .gz archives. Head is
// the last good seal (the genesis marker when nothing was checked).
type AuditVerifyReport struct {
	Ok             bool             `json:"ok"`
	CheckedEntries int              `json:"checked_entries"`
	SkippedLines   int              `json:"skipped_lines"`
	FilesChecked   int              `json:"files_checked"`
	Head           string           `json:"head"`
	FirstBreak     *AuditChainBreak `json:"first_break"`
}

// VerifyAuditLog replays the audit log's hash chain over every audit
// file in dir — all days, oldest first, plain .jsonl and .gz archives —
// and reports whether it is intact. Each entry's PrevHash must equal the
// previous entry's seal and its Hash must recompute from the canonical
// serialization; the first entry must link the genesis marker.
//
// Verification stops at the first break: everything after a broken link
// would report cascading false breaks, so CheckedEntries and Head
// describe the verified prefix. The chain spans rotations by design (a
// new file's first entry links the previous file's last seal), so a
// rotation boundary is verified like any other link.
func VerifyAuditLog(dir string) (AuditVerifyReport, error) {
	rep := AuditVerifyReport{Head: AuditGenesisPrevHash}
	files, err := listAuditFiles(dir)
	if err != nil {
		return rep, err
	}
	expected := AuditGenesisPrevHash
	for _, path := range files {
		brk, checked, skipped, head, read, verr := verifyAuditFile(path, expected)
		if verr != nil {
			return rep, verr
		}
		if !read {
			continue // rotation completed mid-verify; nothing to check
		}
		rep.FilesChecked++
		rep.CheckedEntries += checked
		rep.SkippedLines += skipped
		expected = head
		if brk != nil {
			rep.FirstBreak = brk
			rep.Head = expected
			rep.Ok = false
			return rep, nil
		}
	}
	rep.Head = expected
	rep.Ok = true
	return rep, nil
}

// verifyAuditFile checks one audit file's entries against the chain,
// continuing from expected (the previous file's last seal, or the
// genesis marker). It returns the first break, if any, plus the counts,
// the file's last good seal, and whether the file was actually read
// (false when a rotation completed mid-verify and neither copy was
// visible). Lines are decoded with UseNumber so numeric Details
// round-trip through the canonical form byte-identical to the emit-time
// serialization.
func verifyAuditFile(path, expected string) (brk *AuditChainBreak, checked, skipped int, head string, read bool, err error) {
	head = expected
	fh, rdr, err := openAuditReader(path)
	if err != nil {
		return nil, 0, 0, expected, false, err
	}
	if fh == nil {
		return nil, 0, 0, expected, false, nil
	}
	read = true
	defer fh.Close()
	if c, ok := rdr.(io.Closer); ok {
		defer c.Close()
	}
	sc := bufio.NewScanner(rdr)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev AuditEvent
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			skipped++
			continue
		}
		if ev.Hash == "" {
			return &AuditChainBreak{
				File: filepath.Base(path), Line: lineNo, EntryID: ev.TraceID,
				ExpectedPrev: expected, ActualPrev: ev.PrevHash,
				Reason: "missing_hash",
			}, checked, skipped, head, true, nil
		}
		if ev.PrevHash != expected {
			return &AuditChainBreak{
				File: filepath.Base(path), Line: lineNo, EntryID: ev.TraceID,
				ExpectedPrev: expected, ActualPrev: ev.PrevHash,
				Reason: "prev_mismatch",
			}, checked, skipped, head, true, nil
		}
		if sealAuditEvent(ev) != ev.Hash {
			return &AuditChainBreak{
				File: filepath.Base(path), Line: lineNo, EntryID: ev.TraceID,
				ExpectedPrev: expected, ActualPrev: ev.PrevHash,
				Reason: "hash_mismatch",
			}, checked, skipped, head, true, nil
		}
		expected = ev.Hash
		head = ev.Hash
		checked++
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, expected, true, fmt.Errorf("ledger: audit verify scan %s: %w", path, err)
	}
	return nil, checked, skipped, head, true, nil
}

// openAuditReader opens path for verification, transparently
// decompressing .gz archives. It mirrors readAuditFile's rotation
// fallback: if the file vanished between listing and open (a rotation
// completed mid-verify), the gzip twin — the same events — is read
// instead; when neither copy is visible the file is skipped.
func openAuditReader(path string) (fh *os.File, rdr io.Reader, err error) {
	fh, err = os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("ledger: audit verify open %s: %w", path, err)
		}
		twin := path + ".gz"
		if strings.HasSuffix(path, ".gz") {
			twin = strings.TrimSuffix(path, ".gz")
		}
		fh2, err2 := os.Open(twin)
		if err2 != nil {
			return nil, nil, nil // skipped: rotation mid-flight
		}
		fh = fh2
		path = twin
	}
	rdr = io.Reader(fh)
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(fh)
		if err != nil {
			fh.Close()
			return nil, nil, fmt.Errorf("ledger: audit verify gunzip %s: %w", path, err)
		}
		rdr = gz
	}
	return fh, rdr, nil
}

// auditFileRef identifies one audit file for ordering: its UTC day and
// size-rotation sequence.
type auditFileRef struct {
	path string
	day  string
	seq  int
}

// listAuditFiles returns every audit file in dir — all days, plain and
// gzipped — ordered oldest-first by (day, sequence), deduplicated so a
// rotation caught mid-flight (both .jsonl and .jsonl.gz present, with
// identical events) is read once, preferring the plain file.
func listAuditFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl*"))
	if err != nil {
		return nil, fmt.Errorf("ledger: audit verify glob: %w", err)
	}
	var refs []auditFileRef
	for _, m := range matches {
		if !strings.HasSuffix(m, ".jsonl") && !strings.HasSuffix(m, ".jsonl.gz") {
			continue
		}
		day, seq, ok := parseAuditFilename(filepath.Base(m))
		if !ok {
			continue
		}
		refs = append(refs, auditFileRef{path: m, day: day, seq: seq})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].day != refs[j].day {
			return refs[i].day < refs[j].day
		}
		if refs[i].seq != refs[j].seq {
			return refs[i].seq < refs[j].seq
		}
		// Same logical file, both copies present: the plain file
		// sorts first so dedupe keeps it.
		return !strings.HasSuffix(refs[i].path, ".gz") && strings.HasSuffix(refs[j].path, ".gz")
	})
	seen := make(map[string]bool)
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		base := strings.TrimSuffix(r.path, ".gz")
		if seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, r.path)
	}
	return out, nil
}

// parseAuditFilename splits "audit-2006-01-02.jsonl",
// "audit-2006-01-02-001.jsonl" (and .gz variants) into day and sequence.
func parseAuditFilename(name string) (day string, seq int, ok bool) {
	name = strings.TrimSuffix(name, ".gz")
	name = strings.TrimSuffix(name, ".jsonl")
	rest, found := strings.CutPrefix(name, "audit-")
	if !found {
		return "", 0, false
	}
	if len(rest) < 10 {
		return "", 0, false
	}
	day, tail := rest[:10], rest[10:]
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", 0, false
	}
	switch tail {
	case "":
		return day, 0, true
	default:
		n, err := strconv.Atoi(strings.TrimPrefix(tail, "-"))
		if err != nil || !strings.HasPrefix(tail, "-") {
			return "", 0, false
		}
		return day, n, true
	}
}

// recoverAuditChainHead returns the seal of the newest sealed audit
// entry in dir, so a restarted process continues the hash chain instead
// of breaking it. The scan is bounded: at most 64 files, newest first,
// plain files by a 64 KiB tail read and .gz archives by a streaming
// scan that keeps only the last sealed entry. Empty means no sealed
// entry exists and the chain starts at the genesis marker.
func recoverAuditChainHead(dir string) string {
	files, err := listAuditFiles(dir)
	if err != nil {
		return ""
	}
	const maxFiles = 64
	for i := len(files) - 1; i >= 0 && len(files)-1-i < maxFiles; i-- {
		if h := lastSealedHash(files[i]); h != "" {
			return h
		}
	}
	return ""
}

// lastSealedHash returns the Hash of the last sealed (non-empty Hash)
// entry in path, or "" when the file holds none.
func lastSealedHash(path string) string {
	if strings.HasSuffix(path, ".gz") {
		return lastSealedHashGzip(path)
	}
	return lastSealedHashPlain(path)
}

// lastSealedHashPlain tail-reads at most the last 64 KiB of a plain
// audit file and returns the last sealed entry's hash. Audit lines are
// short (a few hundred bytes), so the tail window always covers the
// file's last entries; a partial first line in the window is simply
// unparseable and skipped.
func lastSealedHashPlain(path string) string {
	const tailSize = 64 * 1024
	fh, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - tailSize
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := fh.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev AuditEvent
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			continue
		}
		if ev.Hash != "" {
			return ev.Hash
		}
	}
	return ""
}

// lastSealedHashGzip streams a gzipped archive keeping only the last
// sealed entry's hash: O(file) time, O(1) memory.
func lastSealedHashGzip(path string) string {
	fh, rdr, err := openAuditReader(path)
	if err != nil || fh == nil {
		if fh != nil {
			fh.Close()
		}
		return ""
	}
	defer fh.Close()
	if c, ok := rdr.(io.Closer); ok {
		defer c.Close()
	}
	var last string
	sc := bufio.NewScanner(rdr)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev AuditEvent
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil {
			continue
		}
		if ev.Hash != "" {
			last = ev.Hash
		}
	}
	return last
}
