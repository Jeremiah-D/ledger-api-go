package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// AuditChainExportFormat is the manifest format identifier. It is bumped
// when the manifest schema changes in a way a verifier must know about.
const AuditChainExportFormat = "audit-chain-export/1"

// AuditChainExportOptions configures ExportAuditChain.
type AuditChainExportOptions struct {
	// FromDay and ToDay bound the exported audit days (UTC,
	// "2006-01-02", inclusive). Empty means unbounded on that side.
	FromDay string
	ToDay   string
	// ExportDir is the destination directory for the read-only snapshot.
	// It is created when missing. Required; it must not be the audit
	// directory itself — the export never writes into the source.
	ExportDir string
	// SigningKeys HMAC-signs the manifest following the audit_sign.go
	// convention (LG-65, see SignAuditManifest). Empty runs unsigned:
	// the manifest records Signed:false honestly instead of failing.
	SigningKeys []AuditSigningKey
	// ErasureVersion/ErasureCount annotate the manifest with the PII
	// erasure state (see pii.go, LG-131): downstream consumers can tell
	// whether their copy predates an erasure. Zero values are omitted
	// from the manifest, so exports from ledgers without erasures look
	// exactly as before.
	ErasureVersion uint64
	ErasureCount   int
}

// AuditChainExportFile describes one audit file copied into the export
// snapshot. Name is the base filename; SHA256 is the hex SHA-256 of the
// copied bytes, so a verifier can confirm the snapshot byte-for-byte.
type AuditChainExportFile struct {
	Name   string `json:"name"`
	Day    string `json:"day"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// AuditChainExportReport is the VerifyAuditLog summary recomputed over
// the exported copies. A day range that starts after the log's first day
// cannot link the genesis marker, so the report is honestly not-ok with
// a prev_mismatch first break on the earliest copied entry — that is the
// tamper-evident signal, not an export failure; the export still
// completes and the manifest records it.
type AuditChainExportReport struct {
	Ok             bool   `json:"ok"`
	CheckedEntries int    `json:"checked_entries"`
	SkippedLines   int    `json:"skipped_lines"`
	FilesChecked   int    `json:"files_checked"`
	Head           string `json:"head"`
}

// AuditChainExportManifest is the deterministic manifest.json written to
// the export dir. Deterministic means: two exports of the same source
// files with the same keys produce byte-identical manifests except
// ExportedAt — the signature covers the canonical JSON with the
// signature field absent and ExportedAt blanked, so the attestation
// timestamp stays informational and does not perturb the MAC. A
// verifier strips "signature", blanks "exported_at", re-marshals, and
// recomputes HMAC-SHA256(key, kid || 0x00 || bytes) — the same
// convention as per-line audit signing (LG-65).
type AuditChainExportManifest struct {
	Format     string                `json:"format"`
	ExportedAt string                `json:"exported_at"` // UTC RFC3339Nano
	FromDay    string                `json:"from_day,omitempty"`
	ToDay      string                `json:"to_day,omitempty"`
	Files      []AuditChainExportFile `json:"files"`
	Report     AuditChainExportReport `json:"report"`
	// ErasureVersion/ErasureCount annotate the PII erasure state at
	// export time (see pii.go, LG-131): a consumer holding an older
	// manifest can see that erasures happened since. Both are covered
	// by the manifest signature like every other field.
	ErasureVersion uint64 `json:"erasure_version,omitempty"`
	ErasureCount   int    `json:"erasure_count,omitempty"`
	Signed     bool                `json:"signed"`
	Signature  *AuditSig            `json:"signature,omitempty"`
}

// manifestSignBytes returns the canonical bytes the manifest signature
// is computed over: the manifest's JSON with the signature field absent
// and the informational ExportedAt blanked. See AuditChainExportManifest.
func manifestSignBytes(m AuditChainExportManifest) ([]byte, error) {
	m.Signature = nil
	m.ExportedAt = ""
	return json.Marshal(m)
}

// AuditExportInputError is returned by ExportAuditChain for invalid
// options: a malformed day, FromDay after ToDay, a missing export dir, or
// the export dir being the audit dir itself. HTTP callers map it to 400.
type AuditExportInputError struct {
	msg string
}

func (e *AuditExportInputError) Error() string { return "ledger: audit export: " + e.msg }

// exportSource is one audit file selected for the snapshot.
type exportSource struct {
	path string
	base string
	day  string
	size int64
}

// ExportAuditChain copies the sealed audit files in dir for the
// FromDay/ToDay range into opts.ExportDir as a read-only snapshot and
// writes a deterministic, HMAC-signed manifest.json there. The source
// audit files are only ever read, never touched or mutated.
//
// The snapshot is forensic: every file is copied byte-for-byte, hashed
// with SHA-256, and chmod'd 0444; the manifest then replays VerifyAuditLog
// over the copies and records the seal-chain head and the verification
// summary. A tampered source file exports faithfully and the manifest
// reports the break — the export never hides what the chain says.
//
// The caller owns the destination lifetime: pass a fresh directory (the
// HTTP endpoint uses a temp dir and serves the manifest from it; a CLI
// passes its own out dir). Re-exporting into a non-empty dir overwrites
// same-named files.
func ExportAuditChain(dir string, opts AuditChainExportOptions) (AuditChainExportManifest, error) {
	var m AuditChainExportManifest
	if opts.FromDay != "" {
		if _, err := time.Parse("2006-01-02", opts.FromDay); err != nil {
			return m, &AuditExportInputError{msg: fmt.Sprintf("bad from_day %q: want YYYY-MM-DD", opts.FromDay)}
		}
	}
	if opts.ToDay != "" {
		if _, err := time.Parse("2006-01-02", opts.ToDay); err != nil {
			return m, &AuditExportInputError{msg: fmt.Sprintf("bad to_day %q: want YYYY-MM-DD", opts.ToDay)}
		}
	}
	if opts.FromDay != "" && opts.ToDay != "" && opts.FromDay > opts.ToDay {
		return m, &AuditExportInputError{msg: fmt.Sprintf("from_day %q after to_day %q", opts.FromDay, opts.ToDay)}
	}
	if opts.ExportDir == "" {
		return m, &AuditExportInputError{msg: "export dir is required"}
	}
	if err := os.MkdirAll(opts.ExportDir, 0o755); err != nil {
		return m, fmt.Errorf("ledger: audit export: mkdir %s: %w", opts.ExportDir, err)
	}
	if sameDir(dir, opts.ExportDir) {
		return m, &AuditExportInputError{msg: "export dir must not be the audit dir"}
	}

	srcs, err := listAuditExportSources(dir, opts.FromDay, opts.ToDay)
	if err != nil {
		return m, err
	}
	files := make([]AuditChainExportFile, 0, len(srcs))
	for _, s := range srcs {
		sum, size, err := copyExportFile(s.path, filepath.Join(opts.ExportDir, s.base))
		if err != nil {
			return m, err
		}
		files = append(files, AuditChainExportFile{
			Name: s.base, Day: s.day, SHA256: sum, Size: size,
		})
	}

	// Verify the seal chain over the exported copies, not the source:
	// the manifest attests to what is in the snapshot.
	rep, err := VerifyAuditLog(opts.ExportDir)
	if err != nil {
		return m, fmt.Errorf("ledger: audit export: verify snapshot: %w", err)
	}

	m = AuditChainExportManifest{
		Format:         AuditChainExportFormat,
		ExportedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		FromDay:        opts.FromDay,
		ToDay:          opts.ToDay,
		Files:          files,
		ErasureVersion: opts.ErasureVersion,
		ErasureCount:   opts.ErasureCount,
		Report: AuditChainExportReport{
			Ok:             rep.Ok,
			CheckedEntries: rep.CheckedEntries,
			SkippedLines:   rep.SkippedLines,
			FilesChecked:   rep.FilesChecked,
			Head:           rep.Head,
		},
		Signed: len(opts.SigningKeys) > 0,
	}
	if m.Signed {
		canon, err := manifestSignBytes(m)
		if err != nil {
			return AuditChainExportManifest{}, fmt.Errorf("ledger: audit export: marshal manifest: %w", err)
		}
		sig, err := SignAuditManifest(canon, opts.SigningKeys)
		if err != nil {
			return AuditChainExportManifest{}, fmt.Errorf("ledger: audit export: sign manifest: %w", err)
		}
		m.Signature = &sig
	}
	final, err := json.Marshal(m)
	if err != nil {
		return AuditChainExportManifest{}, fmt.Errorf("ledger: audit export: marshal manifest: %w", err)
	}
	manifestPath := filepath.Join(opts.ExportDir, "manifest.json")
	// Remove-then-write keeps re-exports idempotent even though the
	// snapshot files are read-only.
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return AuditChainExportManifest{}, fmt.Errorf("ledger: audit export: replace manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, final, 0o444); err != nil {
		return AuditChainExportManifest{}, fmt.Errorf("ledger: audit export: write manifest: %w", err)
	}
	return m, nil
}

// listAuditExportSources returns the audit files in dir within the
// [from, to] day range (empty bound = unbounded), oldest first. A source
// file that vanished mid-export (a rotation completing under us) fails
// the export: a snapshot must be complete, and a retry on a quiescent
// log succeeds.
func listAuditExportSources(dir, from, to string) ([]exportSource, error) {
	files, err := listAuditFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []exportSource
	for _, p := range files {
		day, _, ok := parseAuditFilename(filepath.Base(p))
		if !ok {
			continue
		}
		if from != "" && day < from {
			continue
		}
		if to != "" && day > to {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("ledger: audit export: stat %s: %w", p, err)
		}
		out = append(out, exportSource{path: p, base: filepath.Base(p), day: day, size: st.Size()})
	}
	return out, nil
}

// copyExportFile copies src to dst byte-for-byte, returns the hex
// SHA-256 of the copied bytes and the size. The destination lands
// read-only (0444): the export is a snapshot, not a working copy. An
// existing destination is replaced so re-exports are idempotent.
func copyExportFile(src, dst string) (sum string, size int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("ledger: audit export: open %s: %w", src, err)
	}
	defer in.Close()
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return "", 0, fmt.Errorf("ledger: audit export: replace %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return "", 0, fmt.Errorf("ledger: audit export: create %s: %w", dst, err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, fmt.Errorf("ledger: audit export: copy %s: %w", src, copyErr)
	}
	if syncErr != nil {
		return "", 0, fmt.Errorf("ledger: audit export: sync %s: %w", dst, syncErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("ledger: audit export: close %s: %w", dst, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// sameDir reports whether a and b resolve to the same directory. Used to
// keep the export from ever writing into the audit source.
func sameDir(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return aa == bb
}

// IsAuditExportInputError reports whether err is an
// *AuditExportInputError (invalid export options). HTTP callers map it to
// 400; anything else from ExportAuditChain is a 500.
func IsAuditExportInputError(err error) bool {
	var ierr *AuditExportInputError
	return errors.As(err, &ierr)
}
