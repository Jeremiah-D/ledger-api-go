package ledger

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testTSA is an in-process RFC 3161 timestamp authority: it parses real
// TimeStampReqs and returns really-signed TimeStampResps (RSA,
// SHA-256), so the client/verify paths below exercise the true wire
// format rather than a mock.
type testTSA struct {
	key      *rsa.PrivateKey
	cert     *x509.Certificate
	srv      *httptest.Server
	requests atomic.Int64
	reject   atomic.Int64 // nonzero => answer with this PKIStatus instead of granting
	serial   atomic.Int64
	handleErr atomic.Value // string: handler-side parse failure, checked after the test
}

func newTestTSA(t *testing.T) *testTSA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0x545341),
		Subject:               pkix.Name{CommonName: "ledger-test-tsa"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(2 * time.Hour),
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
	ts := &testTSA{key: key, cert: cert}
	ts.srv = httptest.NewServer(http.HandlerFunc(ts.handle))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *testTSA) handle(w http.ResponseWriter, r *http.Request) {
	ts.requests.Add(1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		ts.handleErr.Store("read: " + err.Error())
		return
	}
	if s := ts.reject.Load(); s != 0 {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.Write(derSeq(derSeq(derInt64(s))))
		return
	}
	digest, nonce, err := parseTestTSARequest(body)
	if err != nil {
		ts.handleErr.Store("parse request: " + err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/timestamp-reply")
	w.Write(ts.buildResponse(digest, nonce))
}

// parseTestTSARequest extracts the imprint digest and nonce from a
// TimeStampReq using the production DER reader.
func parseTestTSARequest(body []byte) ([32]byte, *big.Int, error) {
	var digest [32]byte
	root, _, err := parseDER(body)
	if err != nil {
		return digest, nil, err
	}
	kids, err := root.children()
	if err != nil || len(kids) < 3 {
		return digest, nil, fmt.Errorf("bad TimeStampReq")
	}
	impKids, err := kids[1].children()
	if err != nil || len(impKids) != 2 {
		return digest, nil, fmt.Errorf("bad messageImprint")
	}
	if len(impKids[1].content) != 32 {
		return digest, nil, fmt.Errorf("bad digest length")
	}
	copy(digest[:], impKids[1].content)
	nonce, err := derBigInt(kids[2])
	if err != nil {
		return digest, nil, err
	}
	return digest, nonce, nil
}

// buildResponse assembles a granted TimeStampResp with a real CMS
// signature, using the production DER writer.
func (ts *testTSA) buildResponse(digest [32]byte, nonce *big.Int) []byte {
	serial := big.NewInt(ts.serial.Add(1))
	imprint := derSeq(derAlgorithmIdentifier(oidSHA256), derOctet(digest[:]))
	tstInfo := derSeq(
		derInt64(1),
		derOID("1.2.3.4.5.6"), // test policy OID
		imprint,
		derInt(serial),
		derGeneralizedTime(time.Now()),
		derInt(nonce),
	)
	tstHash := sha256.Sum256(tstInfo)
	attrCT := derSeq(derOID(oidContentTypeAttr), derSet(derOID(oidTSTInfo)))
	attrMD := derSeq(derOID(oidMessageDigestAttr), derSet(derOctet(tstHash[:])))
	attrs := [][]byte{attrCT, attrMD}
	sort.Slice(attrs, func(i, j int) bool { return bytes.Compare(attrs[i], attrs[j]) < 0 })
	// [0] IMPLICIT SET OF Attribute: implicit tagging *replaces* the SET
	// tag, so the content is the concatenated attributes directly — no
	// inner SET wrapper. The signature covers these exact bytes.
	signedAttrs := derTLV(0xa0, bytes.Join(attrs, nil))
	h := sha256.Sum256(signedAttrs)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ts.key, crypto.SHA256, h[:])
	if err != nil {
		panic(err)
	}
	signerInfo := derSeq(
		derInt64(1),
		derSeq(ts.cert.RawIssuer, derInt(ts.cert.SerialNumber)),
		derAlgorithmIdentifier(oidSHA256),
		signedAttrs,
		derSeq(derOID(oidSHA256WithRSA), derNull()),
		derOctet(sig),
	)
	signedData := derSeq(
		derInt64(1),
		derSet(derAlgorithmIdentifier(oidSHA256)),
		derSeq(derOID(oidTSTInfo), derContext(0, derOctet(tstInfo))),
		derContext(0, ts.cert.Raw), // certificates [0] IMPLICIT
		derSet(signerInfo),
	)
	contentInfo := derSeq(derOID(oidSignedData), derContext(0, signedData))
	return derSeq(derSeq(derInt64(0)), contentInfo) // status granted
}

func (ts *testTSA) checkNoHandleError(t *testing.T) {
	t.Helper()
	if v := ts.handleErr.Load(); v != nil {
		t.Fatalf("test TSA handler error: %v", v)
	}
}

// newTSATestLedger builds a ledger with an audit log (durable home of
// tsa_anchors.jsonl), a TSA URL, and the pinned trust anchor.
func newTSATestLedger(t *testing.T, ts *testTSA) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	l := New(WithAuditLog(al), WithTSAURL(ts.srv.URL), WithTSATrustCert(ts.cert))
	return l, dir
}

func TestAnchorTimestampHappyPath(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsa", 3)

	a, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatalf("AnchorTimestamp: %v", err)
	}
	if a.Seq != 3 {
		t.Fatalf("anchor seq = %d, want 3", a.Seq)
	}
	if a.TSAURL != ts.srv.URL {
		t.Fatalf("anchor tsa_url = %q", a.TSAURL)
	}
	if a.GenTime.IsZero() || a.Token == "" || a.Serial == "" {
		t.Fatalf("anchor missing token metadata: %+v", a)
	}
	head, _ := l.ChainHead()
	if a.HeadHash != head {
		t.Fatalf("anchor head %s != chain head %s", a.HeadHash, head)
	}
	ts.checkNoHandleError(t)

	// The token verifies against the pinned trust anchor.
	raw, err := base64.StdEncoding.DecodeString(a.Token)
	if err != nil {
		t.Fatal(err)
	}
	var want [32]byte
	hb, _ := hex.DecodeString(head)
	copy(want[:], hb)
	vt, err := VerifyTimestampToken(raw, want, ts.cert)
	if err != nil {
		t.Fatalf("VerifyTimestampToken: %v", err)
	}
	if !vt.Trusted {
		t.Fatal("expected Trusted=true with the pinned trust anchor")
	}
	if vt.GenTime.IsZero() {
		t.Fatal("missing genTime")
	}

	// The journal file exists and the offline verifier accepts it.
	if _, err := os.Stat(filepath.Join(dir, tsaAnchorsFileName)); err != nil {
		t.Fatalf("anchor journal missing: %v", err)
	}
	n, err := VerifyTimestampAnchors(dir, ts.cert)
	if err != nil {
		t.Fatalf("VerifyTimestampAnchors: %v", err)
	}
	if n != 1 {
		t.Fatalf("verified %d anchors, want 1", n)
	}

	// Live status: configured, trusted, verified.
	st := l.TimestampAnchorStatus()
	if !st.Configured || st.Anchors != 1 || !st.Trusted {
		t.Fatalf("bad status: %+v", st)
	}
	if st.Verified == nil || !*st.Verified {
		t.Fatalf("status not verified: %+v", st)
	}
}

func TestAnchorTimestampIdempotentForUnchangedHead(t *testing.T) {
	ts := newTestTSA(t)
	l, _ := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsaidem", 2)

	a1, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	reqs := ts.requests.Load()
	a2, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Token != a2.Token {
		t.Fatal("idempotent anchor returned a different token")
	}
	if ts.requests.Load() != reqs {
		t.Fatal("unchanged head caused a second TSA round-trip")
	}

	// A new entry moves the head: the next anchor really re-anchors.
	postAnchorEntries(t, l, "tsaidem2", 1)
	a3, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if a3.Seq != 3 || a3.Token == a1.Token {
		t.Fatalf("new head did not re-anchor: %+v", a3)
	}
	if ts.requests.Load() != reqs+1 {
		t.Fatal("expected exactly one more TSA round-trip")
	}
}

func TestVerifyTimestampTokenRejects(t *testing.T) {
	ts := newTestTSA(t)
	l, _ := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsarej", 1)
	a, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(a.Token)
	var want [32]byte
	hb, _ := hex.DecodeString(a.HeadHash)
	copy(want[:], hb)

	// Wrong digest.
	var wrong [32]byte
	wrong[0] = 0xff
	if _, err := VerifyTimestampToken(raw, wrong, ts.cert); err == nil {
		t.Fatal("wrong digest accepted")
	}

	// Tampered token bytes.
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-10] ^= 0x01
	if _, err := VerifyTimestampToken(tampered, want, ts.cert); err == nil {
		t.Fatal("tampered token accepted")
	}

	// Truncated token.
	if _, err := VerifyTimestampToken(raw[:40], want, ts.cert); err == nil {
		t.Fatal("truncated token accepted")
	}

	// Wrong trust anchor: a different CA must not pin.
	other := newTestTSA(t)
	if _, err := VerifyTimestampToken(raw, want, other.cert); err == nil {
		t.Fatal("token pinned to the wrong trust anchor")
	}

	// No trust anchor: signature still verifies against the embedded
	// certificate, but the result is honestly reported untrusted.
	vt, err := VerifyTimestampToken(raw, want, nil)
	if err != nil {
		t.Fatalf("embedded-cert verification failed: %v", err)
	}
	if vt.Trusted {
		t.Fatal("unpinned verification reported Trusted=true")
	}
}

func TestRFC3161ClientRejectsTSARejection(t *testing.T) {
	ts := newTestTSA(t)
	ts.reject.Store(2) // PKIStatus rejection
	_, err := (&RFC3161Client{URL: ts.srv.URL}).Timestamp(context.Background(), [32]byte{1})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

func TestRFC3161ClientDetectsImprintMismatch(t *testing.T) {
	// A TSA that answers for a *different* digest than requested must be
	// rejected by the client (it would be a useless anchor).
	ts := newTestTSA(t)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var other [32]byte
		other[0] = 0x42
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.Write(ts.buildResponse(other, big.NewInt(7)))
	}))
	defer evil.Close()
	_, err := (&RFC3161Client{URL: evil.URL}).Timestamp(context.Background(), [32]byte{9})
	if err == nil || !strings.Contains(err.Error(), "imprint") {
		t.Fatalf("expected imprint-mismatch error, got %v", err)
	}
}

func TestVerifyTimestampAnchorsDetectsTamperedJournal(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsatamper", 2)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored token in the journal.
	path := filepath.Join(dir, tsaAnchorsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 journal line, got %d", len(lines))
	}
	corrupt := lines[0][:len(lines[0])-20] + "AAAAAAAAAAAAAAAAAAAA\"" + "}"
	if err := os.WriteFile(path, []byte(corrupt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTimestampAnchors(dir, ts.cert); err == nil {
		t.Fatal("tampered journal line accepted")
	} else if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error should name the line, got: %v", err)
	}
}

func TestAnchorTimestampRequiresAuditDir(t *testing.T) {
	ts := newTestTSA(t)
	l := New(WithTSAURL(ts.srv.URL)) // no audit log => no durable home
	postAnchorEntries(t, l, "tsanodir", 1)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err == nil {
		t.Fatal("expected fail-closed without an audit directory")
	}
}

func TestAnchorTimestampRequiresAuthority(t *testing.T) {
	ts := newTestTSA(t)
	l, _ := newTSATestLedger(t, ts)
	if _, err := l.AnchorTimestamp(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil authority")
	}
}

// TestTimestampAnchoringWorkerDegradesLoudly: the worker anchors on its
// cadence; when the TSA dies the worker records failures and a
// tsa_anchor_failed audit event — and posting keeps working.
func TestTimestampAnchoringWorkerDegradesLoudly(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsaworker", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var anchored atomic.Int64
	w := StartTimestampAnchoring(ctx, l, 20*time.Millisecond, &RFC3161Client{URL: ts.srv.URL}, func(TimestampAnchor) {
		anchored.Add(1)
	})
	if w == nil {
		t.Fatal("worker not started")
	}
	defer w.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for w.AnchoredTotal() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.AnchoredTotal() == 0 {
		t.Fatal("worker never anchored")
	}
	if anchored.Load() == 0 {
		t.Fatal("onAnchor hook never fired")
	}
	// Idempotency: a quiet ledger does not spam the TSA.
	reqs := ts.requests.Load()
	time.Sleep(80 * time.Millisecond)
	if ts.requests.Load() != reqs {
		t.Fatal("worker re-anchored an unchanged head")
	}

	// Kill the TSA: the next anchor of a *moved* head must fail. Failures
	// accumulate, the audit event fires, and posting is unaffected.
	ts.srv.Close()
	postAnchorEntries(t, l, "tsaworker2", 1) // posting never blocks on the TSA
	deadline = time.Now().Add(3 * time.Second)
	for w.FailuresTotal() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.FailuresTotal() == 0 {
		t.Fatal("no anchor failures recorded after TSA death")
	}

	found := false
	day := time.Now().UTC().Format("2006-01-02")
	deadline = time.Now().Add(3 * time.Second)
	for !found && time.Now().Before(deadline) {
		events, _, err := ReadAuditLog(dir, day)
		if err != nil {
			t.Fatalf("ReadAuditLog: %v", err)
		}
		for _, e := range events {
			if e.Op == "tsa_anchor_failed" {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("tsa_anchor_failed audit event not found")
	}
	if w.Ticks() == 0 {
		t.Fatal("worker recorded no ticks")
	}
}

func TestStartTimestampAnchoringDisabled(t *testing.T) {
	ts := newTestTSA(t)
	l, _ := newTSATestLedger(t, ts)
	if w := StartTimestampAnchoring(context.Background(), l, 0, &RFC3161Client{URL: ts.srv.URL}, nil); w != nil {
		t.Fatal("zero interval should disable the worker")
	}
	if w := StartTimestampAnchoring(context.Background(), l, time.Second, nil, nil); w != nil {
		t.Fatal("nil authority should disable the worker")
	}
}

// TestTimestampAnchorSurvivesChainRewriteDetection: after anchoring,
// rewriting the journal (simulated by anchoring, then checking the
// status against a ledger whose chain no longer matches) is detected.
// Here we simulate the rewrite by hand-editing the in-memory chain
// through a fresh ledger that replays the same journal file — the
// honest simulation is simpler: verifyTimestampAnchorsLocked fails
// when the live chain diverges from the anchored digest.
func TestTimestampAnchorStatusDetectsChainDivergence(t *testing.T) {
	ts := newTestTSA(t)
	l, _ := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsadiv", 2)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatal(err)
	}
	st := l.TimestampAnchorStatus()
	if st.Verified == nil || !*st.Verified {
		t.Fatalf("expected verified before divergence: %+v", st)
	}
	// Simulate a wholesale rewrite: swap the head link's hash in memory
	// (the on-disk journal is untouched — the token still attests the
	// old head).
	l.mu.Lock()
	l.chain[len(l.chain)-1].hash[0] ^= 0x01
	l.mu.Unlock()
	st = l.TimestampAnchorStatus()
	if st.Verified == nil || *st.Verified {
		t.Fatalf("rewritten chain reported verified: %+v", st)
	}
	if !strings.Contains(st.Error, "rewritten") {
		t.Fatalf("error should name the rewrite, got: %v", st.Error)
	}
}
