package ledger

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Signed reconcile export (LG-46): an end-of-day reconciliation report
// bundled with an Ed25519 signature so a third party can archive and
// re-verify it offline, without trusting the ledger's storage or calling
// it back. The signature reuses the LG-44 anchor key system
// (WithAnchorKey / LEDGER_ANCHOR_KEY): the same operator key that anchors
// audit-chain heads also attests reconcile reports, so key_id names one
// key for both attestation kinds and auditors learn a single key ID.
//
// Offline verification recipe (no ledger code needed, any language):
//  1. Take the `report` object exactly as returned (it is already the
//     canonical JSON the signature covers: see CanonicalJSON).
//  2. Recompute the canonical bytes: UTF-8 JSON of the report, object
//     keys in the field order shown, no HTML escaping of <>&, no
//     insignificant whitespace.
//  3. message = "LEDGER-RECONCILE-EXPORT/v1:" || canonical bytes.
//  4. Ed25519-verify `signature` (hex) against `message` with the public
//     key whose first 8 bytes hex-encode to `key_id`.
//  5. A verifying signature proves the key holder saw exactly this report
//     — including its generated_at, version, and audit-chain head — and
//     that no byte was altered afterwards.
//
// The signature covers the report bytes, not a hash of them: Ed25519
// hashes internally, and signing the canonical bytes directly keeps the
// verifier's job to "concatenate and verify" with no extra digest step
// to get wrong.

// SignedReconcileExport is the signed package POST /reconcile/export
// returns: the deterministic reconciliation report plus the Ed25519
// signature over its canonical bytes and the signing key's ID.
type SignedReconcileExport struct {
	// Report is the reconciliation report, serialized in canonical form
	// for signing (see CanonicalJSON).
	Report ReconciliationReport `json:"report"`
	// Signature is the hex Ed25519 signature over ExportSignatureMessage
	// of the canonical report bytes.
	Signature string `json:"signature"`
	// KeyID identifies the signing key: hex of the first 8 bytes of the
	// Ed25519 public key — the same KeyID the LG-44 anchor checkpoints
	// record, so one key ID covers both attestation kinds.
	KeyID string `json:"key_id"`
}

// exportDomain separates reconcile-export signatures from every other
// signature the anchor key might produce (notably the LG-44 anchor
// signatures over "LEDGER-ANCHOR/v1:"): a signature can never be
// transplanted into a different protocol, in either direction.
var exportDomain = []byte("LEDGER-RECONCILE-EXPORT/v1:")

// CanonicalJSON serializes the report into the deterministic byte form
// the export signature covers. The report struct tree contains no maps —
// every list is emitted in sorted or insertion order — so encoding/json
// with HTML escaping disabled is already canonical: same report in, same
// bytes out, on every run and every platform. (A map anywhere in this
// shape would silently break that promise; the export tests pin a golden
// vector to catch it.)
func (r ReconciliationReport) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("ledger: reconcile export canonicalize: %w", err)
	}
	// Encoder.Encode appends one trailing newline; the signed bytes are
	// the bare object — a verifier concatenating "report bytes" must not
	// have to guess about a trailing \n.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ExportSignatureMessage is the exact byte string the export signature
// covers: domain || canonical report JSON. Verifiers reimplementing this
// offline concatenate the same two parts.
func ExportSignatureMessage(canonicalReport []byte) []byte {
	msg := make([]byte, 0, len(exportDomain)+len(canonicalReport))
	msg = append(msg, exportDomain...)
	msg = append(msg, canonicalReport...)
	return msg
}

// VerifySignature checks the export's signature against the canonical
// bytes of its own report, using the caller's public key. It
// re-canonicalizes the report rather than trusting any cached bytes, so a
// report that was tampered with after signing fails here even if the
// signature field was left untouched.
func (e SignedReconcileExport) VerifySignature(pub ed25519.PublicKey) bool {
	if len(e.Signature) != ed25519.SignatureSize*2 || len(e.KeyID) != 16 {
		return false
	}
	if got := hex.EncodeToString(pub[:8]); got != e.KeyID {
		return false
	}
	canonical, err := e.Report.CanonicalJSON()
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(e.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, ExportSignatureMessage(canonical), sig)
}

// ExportSignedReconcile runs the same read-only scan as
// ReconcileWithOptions and returns the report bundled with the anchor
// key's Ed25519 signature over its canonical bytes. The export is
// read-only — it bumps no version and appends no chain link — and it
// emits a `reconcile_export` audit event naming the signing key, so the
// audit log shows who attested which ledger state and when.
//
// Fail-closed: without a configured anchor key (see WithAnchorKey /
// LEDGER_ANCHOR_KEY) it returns ErrAnchorKeyNotConfigured. Unlike
// Anchor, the export needs no audit directory: nothing is appended to a
// journal — the signed package itself is the artifact the operator
// archives.
func (l *Ledger) ExportSignedReconcile(now time.Time, opts ReconcileOptions) (SignedReconcileExport, error) {
	a := &l.anchor
	if len(a.key) == 0 {
		return SignedReconcileExport{}, ErrAnchorKeyNotConfigured
	}
	report, err := l.ReconcileWithOptions(now, opts)
	if err != nil {
		return SignedReconcileExport{}, err
	}
	canonical, err := report.CanonicalJSON()
	if err != nil {
		return SignedReconcileExport{}, err
	}
	sig := ed25519.Sign(a.key, ExportSignatureMessage(canonical))
	l.mu.RLock()
	l.emitAudit(AuditEvent{
		Op:            "reconcile_export",
		Actor:         "Reconcile",
		TraceID:       fmt.Sprintf("reconcile-export@%d", report.Version),
		VersionBefore: report.Version,
		VersionAfter:  report.Version,
		Details: map[string]any{
			"key_id":          a.keyID,
			"signature_bytes": len(sig),
			"fx_applied":      report.FXApplied,
		},
	})
	l.mu.RUnlock()
	return SignedReconcileExport{
		Report:    report,
		Signature: hex.EncodeToString(sig),
		KeyID:     a.keyID,
	}, nil
}
