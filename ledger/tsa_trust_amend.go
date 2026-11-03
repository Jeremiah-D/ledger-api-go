package ledger

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TSA timestamp trust-certificate runtime rotation (LG-302).
//
// WithTSATrustCert (tsa.go) pins the TSA trust anchor at construction —
// the certificate every stored timestamp token must be signed by —
// and this module is the orthogonal runtime path: an operator rotates
// the trust certificate in place, instead of rebuilding the ledger
// and replaying its journal. Rotating in place matters because a TSA
// signing certificate has an expiry date (a real operations event):
// when the TSA renews its certificate, the trust anchor must follow,
// and a rebuild is a new ledger — a gap in which the old certificate
// (or none) pins verification, plus a fresh identity that breaks the
// audit story — while a rotation keeps the same ledger, the same
// journal, and the same audit trail from old certificate to new.
//
// The naive rotation breaks history: verifyTimestampAnchorsLocked used
// to take a single live trust certificate and require every stored
// anchor's token signer to equal it byte-for-byte, so swapping the
// trust anchor would fail every anchor countersigned before the
// rotation, and TimestampAnchorStatus and the Reconcile anchor health
// would report failure with it. This module closes that gap with a
// retired-trust-certificate table, the same shape as LG-300's retired
// anchor public keys:
//
//   - On rotation the old trust certificate — public certificate
//     only; a TSA certificate is public material, there is no private
//     key to protect — is recorded in the retired table and persisted
//     to tsa_retired_certs.jsonl in the audit directory, next to
//     tsa_anchors.jsonl, before the live certificate is swapped. A
//     crash between the persist and the swap leaves only a harmless
//     orphan record: a retired certificate nobody's tokens name.
//   - verifyTimestampAnchorsLocked resolves each anchor's token signer
//     by SHA-256 fingerprint of the signer certificate's Raw bytes
//     against the live certificate first, then the retired table, and
//     still fails closed on an unknown signer, in the same error shape
//     as before ("token signer is not the configured trust anchor").
//   - The retired table loads lazily with the anchor journal
//     (ensureTSAAnchorsLoaded), so a restart recovers it from the
//     audit directory and old anchors keep verifying. A ledger without
//     an audit directory keeps the table in memory only — consistent
//     with the anchor journal, which is not durable there either.
//   - The certificate itself never enters the audit log: the
//     tsa_trust_cert_rotated event carries subjects, serials, and
//     fingerprints only, never the certificate PEM. The PEM lives only
//     in tsa_retired_certs.jsonl, where verification needs it.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Validation is strict, like construction but without the panic:
//     a nil certificate is rejected with ErrInvalidTSATrustCert, and
//     so is a certificate past its NotAfter — installing an expired
//     trust root for new anchors is an operator error. The old
//     certificate is left exactly as it was. Retired certificates
//     keep today's validity-agnostic verification semantics: a token
//     that was valid when the TSA signed it stays trusted, so expiry
//     is checked only at installation, never at verification.
//   - Rotating to a certificate whose fingerprint equals the live
//     certificate's is a no-op: it returns nil and writes no audit
//     event, so a retried or duplicated rotation cannot spam the
//     audit trail.
//   - The rotation governs only later verification: old anchors keep
//     verifying against the retired certificate, TimestampAnchorStatus
//     ().Trusted and .Verified stay true across the rotation, and
//     Reconcile anchor health stays green. Rotating an unconfigured
//     ledger installs the trust root: the status becomes Trusted
//     afterwards.
//   - The rotation is trust-root governance, not a ledger event: it
//     bumps neither the ledger version nor the audit chain, and no
//     journal entry or outbox event fires. It lands one
//     tsa_trust_cert_rotated audit event carrying previous and current
//     subjects, serials, and fingerprints — fingerprints and subjects
//     only, never certificate PEM. The Ctx variant attributes the
//     operator (LG-73).
//
// Lock order: the setter holds l.mu, then tsa.mu, swapping the live
// certificate and the retired table atomically under tsa.mu (see the
// lock-order note on tsaAnchorState: l.mu -> tsa.mu).
// verifyTimestampAnchorsLocked snapshots the live certificate, the
// retired table, and the anchor list under tsa.mu at entry, so a
// rotation mid-verification cannot tear the lookup.
//
// Orthogonality: WithTSATrustCert owns construction-time install,
// LG-84 owns the anchoring mechanism itself, LG-100 owns the
// chain_anchored audit-chain evidence, LG-300 owns the operator's own
// Ed25519 anchor signing key (a different key system from this X.509
// trust root), and this module owns only the runtime rotation of the
// TSA trust certificate and the history it leaves behind. There is no
// HTTP endpoint for the rotation (per LG-241/242): the trust root is
// in-process operator config. SetTSAURL is a separate item (LG-303),
// as is TSA client reconnect/retry.

// ErrInvalidTSATrustCert is returned by SetTSATrustCert when the
// candidate certificate is nil or past its NotAfter. The previous
// trust certificate is left untouched.
var ErrInvalidTSATrustCert = errors.New("ledger: invalid TSA trust certificate")

// retiredTSATrustCertsFileName is the retired-trust-certificate
// journal's name inside the audit directory (LEDGER_AUDIT_DIR).
const retiredTSATrustCertsFileName = "tsa_retired_certs.jsonl"

// retiredTSATrustCert is one rotated-out TSA trust certificate: its
// subject, serial, and SHA-256 fingerprint (of cert.Raw) for audit and
// dedup, the full PEM so a restart can re-verify tokens signed under
// it, and when it was retired. A TSA certificate is public material —
// no private key exists to protect — but the PEM still stays out of
// the audit log (see the module doc comment).
type retiredTSATrustCert struct {
	// Subject is the certificate subject, as x509 prints it.
	Subject string `json:"subject"`
	// SerialHex is the hex of the certificate serial number.
	SerialHex string `json:"serial_hex"`
	// Fingerprint is the hex SHA-256 of the certificate's Raw DER.
	Fingerprint string `json:"sha256_fingerprint_hex"`
	// CertPEM is the PEM-encoded certificate.
	CertPEM string `json:"cert_pem"`
	// RetiredAt is when the certificate was rotated out, UTC.
	RetiredAt time.Time `json:"retired_at"`

	// cert is the parsed form of CertPEM, kept out of the JSON.
	cert *x509.Certificate `json:"-"`
}

// tsaTrustCertFingerprint returns the SHA-256 hex of a certificate's
// Raw DER — the identity verifyTimestampAnchorsLocked matches token
// signers against.
func tsaTrustCertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// tsaTrustCertSerialHex returns the hex of a certificate's serial
// number.
func tsaTrustCertSerialHex(cert *x509.Certificate) string {
	return hex.EncodeToString(cert.SerialNumber.Bytes())
}

// tsaTrustCertPEM returns the PEM encoding of a certificate.
func tsaTrustCertPEM(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// loadRetiredTSATrustCerts reads tsa_retired_certs.jsonl, skipping
// corrupt lines: a torn write can only ever corrupt its own line, and
// a partially written tail must not wedge the ledger at startup. A
// missing file is not an error — no certificate has been rotated yet.
// A skipped line means the tokens that certificate signed may stop
// verifying, which verifyTimestampAnchorsLocked surfaces fail-closed.
func loadRetiredTSATrustCerts(path string) []retiredTSATrustCert {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	var out []retiredTSATrustCert
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec retiredTSATrustCert
		if err := json.Unmarshal(line, &rec); err != nil || !validRetiredTSATrustCertShape(&rec) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// validRetiredTSATrustCertShape parses the record's PEM and checks the
// recorded fingerprint and serial against the parsed certificate: a
// record that cannot possibly verify a token is skipped instead of
// trusted. It fills rec.cert on success.
func validRetiredTSATrustCertShape(rec *retiredTSATrustCert) bool {
	if len(rec.Fingerprint) != 64 || len(rec.SerialHex) == 0 || rec.Subject == "" {
		return false
	}
	if _, err := hex.DecodeString(rec.Fingerprint); err != nil {
		return false
	}
	if _, err := hex.DecodeString(rec.SerialHex); err != nil {
		return false
	}
	if rec.RetiredAt.IsZero() {
		return false
	}
	block, _ := pem.Decode([]byte(rec.CertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if tsaTrustCertFingerprint(cert) != rec.Fingerprint {
		return false
	}
	if tsaTrustCertSerialHex(cert) != rec.SerialHex {
		return false
	}
	rec.cert = cert
	return true
}

// appendRetiredTSATrustCertLine appends one retired trust certificate
// to the journal and fsyncs before returning: the persist must land
// before the live certificate is swapped, so a crash can only ever
// leave a harmless orphan record, never an anchor whose trust root is
// lost.
func appendRetiredTSATrustCertLine(dir string, rec retiredTSATrustCert) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ledger: retired TSA trust certificate marshal: %w", err)
	}
	line = append(line, '\n')
	fh, err := os.OpenFile(filepath.Join(dir, retiredTSATrustCertsFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("ledger: retired TSA trust certificate open: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(line); err != nil {
		return fmt.Errorf("ledger: retired TSA trust certificate write: %w", err)
	}
	if err := fh.Sync(); err != nil {
		return fmt.Errorf("ledger: retired TSA trust certificate sync: %w", err)
	}
	return nil
}

// tsaTrustCertFor resolves a token signer's fingerprint to the
// certificate that must verify it: the live trust certificate first,
// then the retired table. A fingerprint naming no known certificate
// resolves to nothing and its caller fails closed.
func tsaTrustCertFor(fingerprint string, live *x509.Certificate, retired []retiredTSATrustCert) (*x509.Certificate, bool) {
	if live != nil && tsaTrustCertFingerprint(live) == fingerprint {
		return live, true
	}
	for i := range retired {
		if retired[i].Fingerprint != fingerprint {
			continue
		}
		if retired[i].cert == nil {
			return nil, false
		}
		return retired[i].cert, true
	}
	return nil, false
}

// SetTSATrustCert rotates the TSA timestamp trust certificate at
// runtime. Tokens countersigned under the old certificate keep
// verifying against the retired certificate, which is persisted to
// tsa_retired_certs.jsonl before the live certificate swaps. A nil
// certificate, or one past its NotAfter, is rejected with
// ErrInvalidTSATrustCert and the old certificate is left untouched.
// Rotating to the live certificate's fingerprint is a no-op with no
// audit event. Rotating an unconfigured ledger installs the trust
// root. The rotation bumps neither the ledger version nor the audit
// chain, and fires no journal entry or outbox event; it lands one
// tsa_trust_cert_rotated audit event carrying subjects, serials, and
// fingerprints only — never certificate PEM.
func (l *Ledger) SetTSATrustCert(cert *x509.Certificate) error {
	return l.SetTSATrustCertCtx(context.Background(), cert)
}

// SetTSATrustCertCtx is SetTSATrustCert with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetTSATrustCertCtx(ctx context.Context, cert *x509.Certificate) error {
	if cert == nil {
		return ErrInvalidTSATrustCert
	}
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("%w: certificate expired at %s", ErrInvalidTSATrustCert, cert.NotAfter.UTC().Format(time.RFC3339))
	}
	newFingerprint := tsaTrustCertFingerprint(cert)
	l.ensureTSAAnchorsLoaded()
	l.mu.Lock()
	defer l.mu.Unlock()
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trustCert != nil && tsaTrustCertFingerprint(t.trustCert) == newFingerprint {
		return nil
	}
	previousSubject, previousSerialHex, previousFingerprint := "", "", ""
	if t.trustCert != nil {
		old := t.trustCert
		previousSubject = old.Subject.String()
		previousSerialHex = tsaTrustCertSerialHex(old)
		previousFingerprint = tsaTrustCertFingerprint(old)
		rec := retiredTSATrustCert{
			Subject:     previousSubject,
			SerialHex:   previousSerialHex,
			Fingerprint: previousFingerprint,
			CertPEM:     tsaTrustCertPEM(old),
			RetiredAt:   time.Now().UTC(),
			cert:        old,
		}
		// Persist the retired certificate before swapping the live
		// one: a crash in between leaves only a harmless orphan
		// record. A failed persist rejects the rotation and the old
		// certificate stays live. A ledger without an audit directory
		// keeps the retired certificate in memory only, like its
		// anchor journal.
		if dir, ok := l.AuditDir(); ok {
			if err := appendRetiredTSATrustCertLine(dir, rec); err != nil {
				return err
			}
		}
		t.retiredTrustCerts = append(t.retiredTrustCerts, rec)
	}
	t.trustCert = cert
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "tsa_trust_cert_rotated",
		Actor:         "SetTSATrustCert",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_subject":       previousSubject,
			"previous_serial_hex":    previousSerialHex,
			"previous_fingerprint":   previousFingerprint,
			"subject":                cert.Subject.String(),
			"serial_hex":             tsaTrustCertSerialHex(cert),
			"fingerprint":            newFingerprint,
		},
	})
	return nil
}

// TSATrustCertFingerprint returns the SHA-256 hex of the live TSA
// trust certificate's Raw DER. It reports false when no trust
// certificate is configured.
func (l *Ledger) TSATrustCertFingerprint() (string, bool) {
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trustCert == nil {
		return "", false
	}
	return tsaTrustCertFingerprint(t.trustCert), true
}
