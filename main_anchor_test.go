package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newAnchorHTTPServer builds a server whose ledger has an audit log (the
// durable home of checkpoints.jsonl) and a fresh random anchor key,
// seeded with 2 journal entries.
func newAnchorHTTPServer(t *testing.T) (*httptest.Server, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithAnchorKey(key))
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e := ledger.JournalEntry{
			ID:             "e-anchor-http-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "key-anchor-http-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv, pub
}

func TestHandleAnchorCheckpoint(t *testing.T) {
	srv, _ := newAnchorHTTPServer(t)

	code, body := postJSON(t, srv.URL+"/entries/anchor", "")
	if code != http.StatusOK {
		t.Fatalf("anchor status = %d body=%v, want 200", code, body)
	}
	if body["seq"] != float64(2) {
		t.Errorf("seq = %v, want 2", body["seq"])
	}
	if head, _ := body["head_hash"].(string); len(head) != 64 {
		t.Errorf("head_hash = %q, want 64 hex chars", head)
	}
	if sig, _ := body["signature"].(string); len(sig) != 128 {
		t.Errorf("signature = %q, want 128 hex chars", sig)
	}
	// Re-anchoring the unchanged head is idempotent: same checkpoint back.
	code, body2 := postJSON(t, srv.URL+"/entries/anchor", "")
	if code != http.StatusOK {
		t.Fatalf("re-anchor status = %d, want 200", code)
	}
	if body2["signature"] != body["signature"] || body2["seq"] != body["seq"] {
		t.Errorf("re-anchor = %v, want identical checkpoint %v", body2, body)
	}
}

func TestHandleAnchorCheckpointNotConfigured(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	code, body := postJSON(t, srv.URL+"/entries/anchor", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("anchor status = %d, want 500", code)
	}
	if _, ok := body["error"].(string); !ok {
		t.Errorf("anchor body = %v, want error field", body)
	}
}

func TestVerifyEntriesIncludesAnchorStatus(t *testing.T) {
	srv, _ := newAnchorHTTPServer(t)

	// Before any anchor: configured, zero checkpoints, verified.
	code, body := getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	anchor, ok := body["anchor"].(map[string]any)
	if !ok {
		t.Fatalf("verify body has no anchor section: %v", body)
	}
	if anchor["configured"] != true || anchor["checkpoints"] != float64(0) || anchor["verified"] != true {
		t.Errorf("anchor status = %v, want configured/0/verified", anchor)
	}

	// After anchoring: one checkpoint, still verified.
	if code, _ := postJSON(t, srv.URL+"/entries/anchor", ""); code != http.StatusOK {
		t.Fatalf("anchor status = %d, want 200", code)
	}
	_, body = getJSON(t, srv.URL+"/entries/verify")
	anchor, _ = body["anchor"].(map[string]any)
	if anchor["checkpoints"] != float64(1) || anchor["verified"] != true {
		t.Errorf("anchor status after anchor = %v, want 1/verified", anchor)
	}
	latest, ok := anchor["latest"].(map[string]any)
	if !ok || latest["seq"] != float64(2) {
		t.Errorf("anchor.latest = %v, want seq 2", anchor["latest"])
	}
}

func TestVerifyEntriesAnchorStatusUnconfigured(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	code, body := getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	anchor, ok := body["anchor"].(map[string]any)
	if !ok {
		t.Fatalf("verify body has no anchor section: %v", body)
	}
	if anchor["configured"] != false || anchor["verified"] != true {
		t.Errorf("anchor status = %v, want unconfigured/verified", anchor)
	}
}

func TestMetricsIncludesAnchorsTotal(t *testing.T) {
	srv, _ := newAnchorHTTPServer(t)
	if code, _ := postJSON(t, srv.URL+"/entries/anchor", ""); code != http.StatusOK {
		t.Fatalf("anchor status = %d, want 200", code)
	}
	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, body)
	v, ok := m["ledger_anchors_total"]
	if !ok {
		t.Fatal("ledger_anchors_total missing from exposition")
	}
	if v != 1 {
		t.Errorf("ledger_anchors_total = %d, want 1", v)
	}
	// The anchor section of /entries/verify names this ledger's key.
	_, vbody := getJSON(t, srv.URL+"/entries/verify")
	anchor, _ := vbody["anchor"].(map[string]any)
	if kid, _ := anchor["key_id"].(string); len(kid) != 16 {
		t.Errorf("anchor.key_id = %q, want 16 hex chars", kid)
	}
}
