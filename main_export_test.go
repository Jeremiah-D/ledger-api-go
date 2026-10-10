package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Without an anchor key the server cannot sign: POST /reconcile/export
// fails closed with 500 (deployment misconfiguration), not a signed
// package.
func TestHandleReconcileExportNoKey(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/reconcile/export", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("body = %v, want an error object", body)
	}
}

// exportPackage mirrors the wire shape, keeping the report as raw bytes
// so the test verifies the signature against exactly the bytes the
// server sent — the true offline-verification path.
type exportPackage struct {
	Report    json.RawMessage `json:"report"`
	Signature string          `json:"signature"`
	KeyID     string          `json:"key_id"`
}

// With an anchor key, POST /reconcile/export returns a self-consistent
// signed package: {report, signature, key_id} whose signature verifies
// offline against the canonical report bytes with the public key.
func TestHandleReconcileExportSigned(t *testing.T) {
	srv, pub := newAnchorHTTPServer(t)

	resp, err := http.Post(srv.URL+"/reconcile/export", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /reconcile/export: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, raw)
	}
	var pkg exportPackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("decode export package: %v", err)
	}
	if len(pkg.Signature) != ed25519.SignatureSize*2 {
		t.Fatalf("signature = %q, want %d hex chars", pkg.Signature, ed25519.SignatureSize*2)
	}
	if pkg.KeyID != hex.EncodeToString(pub[:8]) {
		t.Fatalf("key_id = %q, want hex(pub[:8])", pkg.KeyID)
	}

	// Offline verification, reimplemented from the documented recipe: the
	// signed message is the domain prefix concatenated with the report
	// bytes exactly as received. No ledger code is involved.
	msg := append([]byte("LEDGER-RECONCILE-EXPORT/v1:"), pkg.Report...)
	sig, err := hex.DecodeString(pkg.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("export signature does not verify offline against the received report bytes")
	}

	// The same package also verifies through the ledger's own verifier,
	// which re-canonicalizes the report from its fields.
	var exp ledger.SignedReconcileExport
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("decode into SignedReconcileExport: %v", err)
	}
	if !exp.VerifySignature(pub) {
		t.Error("SignedReconcileExport.VerifySignature = false")
	}
	if exp.Report.Version != 2 {
		t.Errorf("report.version = %d, want 2", exp.Report.Version)
	}
}
