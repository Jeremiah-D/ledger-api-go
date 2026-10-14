package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// GET /accounts/{id}/merkle-proof returns a self-contained inclusion
// proof that verifies offline against the anchored root.
func TestHandleMerkleProof(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/cash/merkle-proof")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["account"] != "cash" || body["currency"] != "USD" {
		t.Fatalf("account/currency = %v/%v, want cash/USD", body["account"], body["currency"])
	}
	if body["balance_cents"] != float64(300) {
		t.Fatalf("balance_cents = %v, want 300", body["balance_cents"])
	}
	root, _ := body["merkle_root"].(string)
	if len(root) != 64 {
		t.Fatalf("merkle_root = %q, want 64 hex chars", root)
	}
	leafHex, _ := body["leaf"].(string)
	siblings, _ := body["siblings"].([]any)
	leafIdx, _ := body["leaf_index"].(float64)
	if leafHex == "" || siblings == nil {
		t.Fatalf("missing leaf/siblings in %v", body)
	}

	// Offline verification, independent of the server's own prover:
	// recompute the leaf hash from the published encoding and fold
	// the sibling path to the root.
	enc, err := hex.DecodeString(leafHex)
	if err != nil {
		t.Fatalf("leaf hex: %v", err)
	}
	cur := sha256.Sum256(append([]byte{0x00}, enc...))
	idx := int(leafIdx)
	for _, s := range siblings {
		sib, err := hex.DecodeString(s.(string))
		if err != nil {
			t.Fatalf("sibling hex: %v", err)
		}
		h := sha256.New()
		h.Write([]byte{0x01})
		if idx%2 == 0 {
			h.Write(cur[:])
			h.Write(sib)
		} else {
			h.Write(sib)
			h.Write(cur[:])
		}
		copy(cur[:], h.Sum(nil))
		idx /= 2
	}
	if hex.EncodeToString(cur[:]) != root {
		t.Fatal("offline recomputation did not reach the anchored root")
	}
}

// The same proof verifies through the ledger package's own offline
// verifier, for consumers that import the module.
func TestHandleMerkleProofPackageVerifier(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/equity/merkle-proof")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rootHex, _ := body["merkle_root"].(string)
	rootBytes, _ := hex.DecodeString(rootHex)
	var root [32]byte
	copy(root[:], rootBytes)
	leafIdx := int(body["leaf_index"].(float64))
	siblings := body["siblings"].([]any)
	sibHashes := make([][32]byte, len(siblings))
	for i, s := range siblings {
		b, _ := hex.DecodeString(s.(string))
		copy(sibHashes[i][:], b)
	}
	proof := ledger.MerkleProof{
		Leaf:      ledger.MerkleLeaf{Account: "equity", Currency: "USD", BalanceCents: int64(body["balance_cents"].(float64))},
		LeafIndex: leafIdx,
		Siblings:  sibHashes,
	}
	if !ledger.VerifyMerkleProof(root, proof) {
		t.Fatal("ledger.VerifyMerkleProof rejected the server's proof")
	}
}

// ?currency= selects the per-currency leaf; an account with no leaf in
// that currency 404s, and a bad code 400s.
func TestHandleMerkleProofCurrency(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/cash/merkle-proof?currency=EUR")
	if code != http.StatusNotFound {
		t.Fatalf("EUR status = %d, want 404 (no EUR leaf)", code)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("EUR body missing error: %v", body)
	}

	code, _ = getJSON(t, srv.URL+"/accounts/cash/merkle-proof?currency=usd")
	if code != http.StatusBadRequest {
		t.Fatalf("bad currency status = %d, want 400", code)
	}
}

// Unknown account: 404, and the request is not counted.
func TestHandleMerkleProofUnknownAccount(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/ghost/merkle-proof")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("body missing error: %v", body)
	}
}

// Successful proofs are counted in /metrics.
func TestHandleMerkleProofMetrics(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	if code, _ := getJSON(t, srv.URL+"/accounts/cash/merkle-proof"); code != http.StatusOK {
		t.Fatalf("proof status = %d, want 200", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_merkle_proof_queries_total 1\n") {
		t.Fatalf("/metrics missing the merkle proof counter:\n%s", sb.String())
	}
}
