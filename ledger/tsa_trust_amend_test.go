package ledger

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TSA timestamp trust-certificate runtime rotation (LG-302): the
// setter's fail-closed gates, the no-op, the retired-certificate
// table, restart recovery, concurrent rotation, operator attribution,
// and the audit trail's certificate-PEM boundary.

// newTSATrustTestLedger builds a ledger with an audit log, a TSA URL,
// and the pinned trust anchor, returning the audit log too so tests
// can read the audit trail. The caller must not use the audit log
// after tsaTrustRotatedAuditEvents closes it.
func newTSATrustTestLedger(t *testing.T, ts *testTSA) (*Ledger, string, *AuditLog) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	l := New(WithAuditLog(al), WithTSAURL(ts.srv.URL), WithTSATrustCert(ts.cert))
	return l, dir, al
}

// tsaTrustRotatedAuditEvents returns the tsa_trust_cert_rotated events
// in order, closing the audit log first (like the other amend tests).
func tsaTrustRotatedAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "tsa_trust_cert_rotated" {
			out = append(out, e)
		}
	}
	return out
}

// expiredTestCert builds a self-signed certificate whose NotAfter is
// already past, for the install-time expiry gate.
func expiredTestCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0x45585049),
		Subject:               pkix.Name{CommonName: "ledger-test-tsa-expired"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// anchorWith posts n entries under prefix and anchors the head with
// the given test TSA's authority.
func anchorWith(t *testing.T, l *Ledger, ts *testTSA, prefix string, n int) TimestampAnchor {
	t.Helper()
	postAnchorEntries(t, l, prefix, n)
	a, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatalf("AnchorTimestamp(%s): %v", ts.srv.URL, err)
	}
	return a
}

func TestSetTSATrustCertRejectsNilAndExpired(t *testing.T) {
	ts := newTestTSA(t)
	l, dir, _ := newTSATrustTestLedger(t, ts)
	anchorWith(t, l, ts, "tsa", 3)
	wantFP, ok := l.TSATrustCertFingerprint()
	if !ok {
		t.Fatal("TSATrustCertFingerprint() = false, want the pinned cert")
	}
	st := l.TimestampAnchorStatus()
	if st.Verified == nil || !*st.Verified {
		t.Fatalf("status before rejected rotations = %+v, want verified", st)
	}

	if err := l.SetTSATrustCert(nil); !errors.Is(err, ErrInvalidTSATrustCert) {
		t.Fatalf("SetTSATrustCert(nil) = %v, want ErrInvalidTSATrustCert", err)
	}
	if err := l.SetTSATrustCert(expiredTestCert(t)); !errors.Is(err, ErrInvalidTSATrustCert) {
		t.Fatalf("SetTSATrustCert(expired) = %v, want ErrInvalidTSATrustCert", err)
	}
	// The original certificate is untouched: the fingerprint is
	// unchanged, the status still verifies, and no retired
	// certificate was persisted.
	if got, ok := l.TSATrustCertFingerprint(); !ok || got != wantFP {
		t.Fatalf("TSATrustCertFingerprint() after rejected rotations = %q, want %q", got, wantFP)
	}
	st = l.TimestampAnchorStatus()
	if st.Verified == nil || !*st.Verified {
		t.Fatalf("status after rejected rotations = %+v, want verified", st)
	}
	if _, err := os.Stat(filepath.Join(dir, retiredTSATrustCertsFileName)); !os.IsNotExist(err) {
		t.Fatalf("tsa_retired_certs.jsonl exists after rejected rotations (err=%v)", err)
	}
}

func TestSetTSATrustCertSameCertIsNoOp(t *testing.T) {
	ts := newTestTSA(t)
	l, dir, al := newTSATrustTestLedger(t, ts)
	anchorWith(t, l, ts, "tsa", 3)
	v := l.Version()

	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert(same cert) = %v, want nil", err)
	}
	// Re-installing the same certificate changes nothing: same live
	// fingerprint, no version bump, no retired record, no audit event.
	wantFP := tsaTrustCertFingerprint(ts.cert)
	if got, ok := l.TSATrustCertFingerprint(); !ok || got != wantFP {
		t.Fatalf("TSATrustCertFingerprint() = %q, want %q", got, wantFP)
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (no bump on no-op)", got, v)
	}
	if _, err := os.Stat(filepath.Join(dir, retiredTSATrustCertsFileName)); !os.IsNotExist(err) {
		t.Fatalf("tsa_retired_certs.jsonl exists after no-op (err=%v)", err)
	}
	if events := tsaTrustRotatedAuditEvents(t, al); len(events) != 0 {
		t.Fatalf("tsa_trust_cert_rotated events = %d, want 0 (no-op writes no audit)", len(events))
	}
}

func TestSetTSATrustCertRotatesAndOldAnchorsStillVerify(t *testing.T) {
	ts1 := newTestTSA(t)
	ts2 := newTestTSA(t)
	l, dir, al := newTSATrustTestLedger(t, ts1)
	anchorWith(t, l, ts1, "tsa", 3)
	v := l.Version()

	if err := l.SetTSATrustCert(ts2.cert); err != nil {
		t.Fatalf("SetTSATrustCert = %v", err)
	}
	// The live fingerprint follows the new certificate, with no
	// version bump.
	if got, ok := l.TSATrustCertFingerprint(); !ok || got != tsaTrustCertFingerprint(ts2.cert) {
		t.Fatalf("TSATrustCertFingerprint() = %q, want new cert %q", got, tsaTrustCertFingerprint(ts2.cert))
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (rotation bumps no version)", got, v)
	}
	// The old anchor verifies against the retired certificate even
	// before any new anchor is taken.
	st := l.TimestampAnchorStatus()
	if !st.Trusted || st.Verified == nil || !*st.Verified || st.Error != "" {
		t.Fatalf("TimestampAnchorStatus() after rotation = %+v, want trusted+verified", st)
	}
	rep := l.Reconcile(time.Now())
	if !rep.AuditChain.TSAAnchorOK || rep.AuditChain.TSAAnchorError != "" {
		t.Fatalf("Reconcile TSA anchor health = %+v, want ok", rep.AuditChain)
	}
	// A new anchor under the new certificate verifies too, against
	// the live certificate.
	anchorWith(t, l, ts2, "tsa2", 1)
	st = l.TimestampAnchorStatus()
	if st.Anchors != 2 || st.Verified == nil || !*st.Verified {
		t.Fatalf("TimestampAnchorStatus() after new anchor = %+v, want 2 anchors verified", st)
	}

	// The retired certificate is on disk: subject, serial,
	// fingerprint, PEM, and a retirement timestamp — and the PEM
	// parses back to the old certificate.
	raw, err := os.ReadFile(filepath.Join(dir, retiredTSATrustCertsFileName))
	if err != nil {
		t.Fatalf("read %s: %v", retiredTSATrustCertsFileName, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s has %d lines, want 1", retiredTSATrustCertsFileName, len(lines))
	}
	var rec retiredTSATrustCert
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("retired line unmarshal: %v", err)
	}
	if rec.Subject != ts1.cert.Subject.String() {
		t.Fatalf("retired subject = %q, want %q", rec.Subject, ts1.cert.Subject.String())
	}
	if rec.SerialHex != tsaTrustCertSerialHex(ts1.cert) {
		t.Fatalf("retired serial_hex = %q, want %q", rec.SerialHex, tsaTrustCertSerialHex(ts1.cert))
	}
	if rec.Fingerprint != tsaTrustCertFingerprint(ts1.cert) {
		t.Fatalf("retired fingerprint = %q, want %q", rec.Fingerprint, tsaTrustCertFingerprint(ts1.cert))
	}
	if rec.RetiredAt.IsZero() {
		t.Fatal("retired_at is zero")
	}
	if _, err := ParseTSATrustCert([]byte(rec.CertPEM)); err != nil {
		t.Fatalf("retired cert_pem does not parse: %v", err)
	}

	// The audit event carries fingerprints and subjects only — never
	// the certificate PEM.
	events := tsaTrustRotatedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("tsa_trust_cert_rotated events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Details["previous_fingerprint"] != tsaTrustCertFingerprint(ts1.cert) {
		t.Fatalf("audit previous_fingerprint = %v, want %q", ev.Details["previous_fingerprint"], tsaTrustCertFingerprint(ts1.cert))
	}
	if ev.Details["fingerprint"] != tsaTrustCertFingerprint(ts2.cert) {
		t.Fatalf("audit fingerprint = %v, want %q", ev.Details["fingerprint"], tsaTrustCertFingerprint(ts2.cert))
	}
	if ev.Details["previous_subject"] != ts1.cert.Subject.String() || ev.Details["subject"] != ts2.cert.Subject.String() {
		t.Fatalf("audit subjects = %v, want old/new subjects", ev.Details)
	}
	detailsJSON, err := json.Marshal(ev.Details)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(detailsJSON), "BEGIN CERTIFICATE") {
		t.Fatalf("audit details contain certificate PEM: %s", detailsJSON)
	}
	ts1.checkNoHandleError(t)
	ts2.checkNoHandleError(t)
}

func TestSetTSATrustCertRetiredCertsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := newTestTSA(t)
	ts2 := newTestTSA(t)
	l := New(WithAuditLog(al), WithTSAURL(ts1.srv.URL), WithTSATrustCert(ts1.cert))
	anchorWith(t, l, ts1, "tsa", 3)
	if err := l.SetTSATrustCert(ts2.cert); err != nil {
		t.Fatalf("SetTSATrustCert = %v", err)
	}
	anchorWith(t, l, ts2, "tsa2", 1)
	al.Close()

	// A fresh ledger on the same directory, live on the new
	// certificate, recovers the retired table and still verifies the
	// old anchor.
	al2, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer al2.Close()
	l2 := New(WithAuditLog(al2), WithTSAURL(ts2.srv.URL), WithTSATrustCert(ts2.cert))
	postAnchorEntries(t, l2, "tsa", 3)  // rebuild the same chain prefix
	postAnchorEntries(t, l2, "tsa2", 1) // the anchor heads must match
	st := l2.TimestampAnchorStatus()
	if st.Anchors != 2 || !st.Trusted || st.Verified == nil || !*st.Verified || st.Error != "" {
		t.Fatalf("reloaded TimestampAnchorStatus() = %+v, want 2 anchors trusted+verified", st)
	}
	if got, ok := l2.TSATrustCertFingerprint(); !ok || got != tsaTrustCertFingerprint(ts2.cert) {
		t.Fatalf("reloaded TSATrustCertFingerprint() = %q, want live cert %q", got, tsaTrustCertFingerprint(ts2.cert))
	}
	l2.mu.RLock()
	err = l2.verifyTimestampAnchorsLocked()
	l2.mu.RUnlock()
	if err != nil {
		t.Fatalf("reloaded verifyTimestampAnchorsLocked = %v", err)
	}
	ts1.checkNoHandleError(t)
	ts2.checkNoHandleError(t)
}

func TestSetTSATrustCertUnknownSignerStillFailClosed(t *testing.T) {
	ts1 := newTestTSA(t)
	ts3 := newTestTSA(t)
	l, _, _ := newTSATrustTestLedger(t, ts1)
	// Anchor with a third TSA the ledger never trusted: the token is
	// genuine, but its signer is not the configured trust anchor.
	anchorWith(t, l, ts3, "tsa", 3)
	st := l.TimestampAnchorStatus()
	if st.Verified == nil || *st.Verified {
		t.Fatalf("TimestampAnchorStatus() = %+v, want verified=false for unknown signer", st)
	}
	if !strings.Contains(st.Error, "not the configured trust anchor") {
		t.Fatalf("status error = %q, want fail-closed trust-anchor error", st.Error)
	}
	rep := l.Reconcile(time.Now())
	if rep.AuditChain.TSAAnchorOK || !strings.Contains(rep.AuditChain.TSAAnchorError, "not the configured trust anchor") {
		t.Fatalf("Reconcile TSA anchor health = %+v, want failure", rep.AuditChain)
	}
	ts1.checkNoHandleError(t)
	ts3.checkNoHandleError(t)
}

func TestSetTSATrustCertConcurrentAnchor(t *testing.T) {
	ts1 := newTestTSA(t)
	ts2 := newTestTSA(t)
	ts3 := newTestTSA(t)
	l, _, _ := newTSATrustTestLedger(t, ts1)
	postAnchorEntries(t, l, "tsa", 4)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts1.srv.URL}); err != nil {
		t.Fatalf("AnchorTimestamp = %v", err)
	}
	allowed := map[string]bool{
		tsaTrustCertFingerprint(ts1.cert): true,
		tsaTrustCertFingerprint(ts2.cert): true,
		tsaTrustCertFingerprint(ts3.cert): true,
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	rotate := func(cert *x509.Certificate) {
		defer wg.Done()
		if err := l.SetTSATrustCert(cert); err != nil {
			errs <- err
		}
	}
	anchor := func() {
		defer wg.Done()
		// Same authority (and head) for every concurrent anchor, so
		// the idempotent path returns the one stored anchor instead
		// of appending same-seq duplicates.
		if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts1.srv.URL}); err != nil {
			errs <- err
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go anchor()
		wg.Add(1)
		go rotate(ts2.cert)
		wg.Add(1)
		go rotate(ts3.cert)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AnchorTimestamp/SetTSATrustCert = %v", err)
	}
	// The one stored anchor was signed under ts1, which is live or
	// retired after the rotations, so the whole history still
	// verifies; the live fingerprint is one of the rotation targets.
	st := l.TimestampAnchorStatus()
	if st.Anchors != 1 || st.Verified == nil || !*st.Verified || st.Error != "" {
		t.Fatalf("TimestampAnchorStatus() after concurrent rotation = %+v, want 1 anchor verified", st)
	}
	got, ok := l.TSATrustCertFingerprint()
	if !ok || !allowed[got] {
		t.Fatalf("TSATrustCertFingerprint() = %q, want one of the rotation targets", got)
	}
	ts1.checkNoHandleError(t)
	ts2.checkNoHandleError(t)
	ts3.checkNoHandleError(t)
}

func TestSetTSATrustCertCtxOperatorAttribution(t *testing.T) {
	ts1 := newTestTSA(t)
	ts2 := newTestTSA(t)
	l, _, al := newTSATrustTestLedger(t, ts1)
	anchorWith(t, l, ts1, "tsa", 3)

	ctx := ContextWithActor(context.Background(), "op-rotation")
	if err := l.SetTSATrustCertCtx(ctx, ts2.cert); err != nil {
		t.Fatalf("SetTSATrustCertCtx = %v", err)
	}
	events := tsaTrustRotatedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("tsa_trust_cert_rotated events = %d, want 1", len(events))
	}
	if events[0].Actor != "op-rotation" {
		t.Fatalf("audit Actor = %q, want %q", events[0].Actor, "op-rotation")
	}
	ts1.checkNoHandleError(t)
	ts2.checkNoHandleError(t)
}

func TestSetTSATrustCertInstallsOnUnconfiguredLedger(t *testing.T) {
	ts := newTestTSA(t)
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	l := New(WithAuditLog(al), WithTSAURL(ts.srv.URL))
	// No trust root yet: anchoring still works, but the status is
	// honestly untrusted and unverified.
	if _, ok := l.TSATrustCertFingerprint(); ok {
		t.Fatal("TSATrustCertFingerprint() = true on an unconfigured ledger, want false")
	}
	anchorWith(t, l, ts, "tsa", 3)
	st := l.TimestampAnchorStatus()
	if st.Trusted || st.Verified != nil {
		t.Fatalf("status before install = %+v, want trusted=false, verified=nil", st)
	}
	// Rotating onto an unconfigured ledger installs the trust root.
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert = %v", err)
	}
	if got, ok := l.TSATrustCertFingerprint(); !ok || got != tsaTrustCertFingerprint(ts.cert) {
		t.Fatalf("TSATrustCertFingerprint() = %q, want %q", got, tsaTrustCertFingerprint(ts.cert))
	}
	st = l.TimestampAnchorStatus()
	if !st.Trusted || st.Verified == nil || !*st.Verified {
		t.Fatalf("status after install = %+v, want trusted+verified", st)
	}
	ts.checkNoHandleError(t)
}

func TestTSATrustCertFingerprintMatchesRaw(t *testing.T) {
	ts := newTestTSA(t)
	l, _, _ := newTSATrustTestLedger(t, ts)
	sum := sha256.Sum256(ts.cert.Raw)
	want := hex.EncodeToString(sum[:])
	if got, ok := l.TSATrustCertFingerprint(); !ok || got != want {
		t.Fatalf("TSATrustCertFingerprint() = %q, want SHA-256 hex of cert.Raw %q", got, want)
	}
}
