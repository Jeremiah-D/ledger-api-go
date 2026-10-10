package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

// newExportTestLedger builds a keyed ledger seeded with two balanced
// entries at a fixed timestamp, so exports are byte-comparable.
func newExportTestLedger(t *testing.T) (*Ledger, ed25519.PublicKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	l := New(WithAnchorKey(key))
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e := JournalEntry{
			ID:             "e-export-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100 * int64(i+1),
			IdempotencyKey: "key-export-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	return l, pub
}

func TestExportSignedReconcileRoundTrip(t *testing.T) {
	l, pub := newExportTestLedger(t)
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	exp, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	if exp.KeyID != hex.EncodeToString(pub[:8]) {
		t.Errorf("KeyID = %q, want hex(pub[:8])", exp.KeyID)
	}
	if len(exp.Signature) != ed25519.SignatureSize*2 {
		t.Errorf("len(Signature) = %d, want %d hex chars", len(exp.Signature), ed25519.SignatureSize*2)
	}
	if !exp.VerifySignature(pub) {
		t.Fatal("VerifySignature = false on a fresh export")
	}
	// The exported report is the same scan the plain endpoint serves.
	if exp.Report.Version != 2 {
		t.Errorf("Report.Version = %d, want 2", exp.Report.Version)
	}
	if !exp.Report.AccountingEquationOK {
		t.Errorf("AccountingEquationOK = false: %s", exp.Report.AccountingError)
	}
}

func TestExportSignedReconcileDeterministic(t *testing.T) {
	l, _ := newExportTestLedger(t)
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	a, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	b, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	// Ed25519 is deterministic and the report is canonical: identical
	// input state produces identical signed bytes.
	if a.Signature != b.Signature {
		t.Error("two exports of the same state at the same time have different signatures")
	}
	ca, _ := a.Report.CanonicalJSON()
	cb, _ := b.Report.CanonicalJSON()
	if string(ca) != string(cb) {
		t.Error("canonical report bytes differ between identical exports")
	}
}

func TestExportSignedReconcileTamperFails(t *testing.T) {
	l, pub := newExportTestLedger(t)
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	exp, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	// Flip one amount byte after signing: the signature must not verify.
	exp.Report.TotalDebitsCents++
	if exp.VerifySignature(pub) {
		t.Error("VerifySignature = true after tampering with the report")
	}
	// A signature from a different key must not verify either.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	fresh, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	if fresh.VerifySignature(otherPub) {
		t.Error("VerifySignature = true with the wrong public key")
	}
}

func TestExportSignedReconcileNoKey(t *testing.T) {
	l := New()
	_, err := l.ExportSignedReconcile(time.Now(), ReconcileOptions{})
	if err != ErrAnchorKeyNotConfigured {
		t.Errorf("err = %v, want ErrAnchorKeyNotConfigured (fail-closed)", err)
	}
}

func TestExportSignedReconcileDomainSeparated(t *testing.T) {
	l, pub := newExportTestLedger(t)
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	exp, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	sig, err := hex.DecodeString(exp.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	canonical, err := exp.Report.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	// The same signature must NOT verify as an LG-44 anchor message over
	// the same bytes — cross-protocol transplant is impossible because the
	// domains differ.
	if ed25519.Verify(pub, append([]byte("LEDGER-ANCHOR/v1:"), canonical...), sig) {
		t.Error("export signature verifies under the anchor domain: domain separation broken")
	}
}

func TestExportSignedReconcileBaseCurrency(t *testing.T) {
	l, pub := newExportTestLedger(t)
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	exp, err := l.ExportSignedReconcile(now, ReconcileOptions{BaseCurrency: "EUR"})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	if !exp.Report.FXApplied {
		t.Error("FXApplied = false with base_currency set")
	}
	if !exp.VerifySignature(pub) {
		t.Error("VerifySignature = false on a base-currency export")
	}
	// An invalid base currency fails the scan before any signing.
	if _, err := l.ExportSignedReconcile(now, ReconcileOptions{BaseCurrency: "nope"}); err == nil {
		t.Error("expected an error for an invalid base currency, got nil")
	}
}

func TestExportSignedReconcileReadOnly(t *testing.T) {
	l, pub := newExportTestLedger(t)
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	before := l.Version()
	exp, err := l.ExportSignedReconcile(now, ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	if l.Version() != before {
		t.Errorf("Version = %d after export, want %d (export must not bump)", l.Version(), before)
	}
	if n := len(l.chain); uint64(n) != before {
		t.Errorf("chain links = %d after export, want %d (export appends nothing)", n, before)
	}
	if !exp.VerifySignature(pub) {
		t.Error("VerifySignature = false")
	}
}

func TestExportSignedReconcileEmitsAudit(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	l := New(WithAuditLog(al), WithAnchorKey(key))
	e := JournalEntry{
		ID: "e-export-audit", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 50, CreatedAt: time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	exp, err := l.ExportSignedReconcile(time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC), ReconcileOptions{})
	if err != nil {
		t.Fatalf("ExportSignedReconcile: %v", err)
	}
	// Wait for the async audit writer to flush the reconcile_export event.
	deadline := time.Now().Add(5 * time.Second)
	for {
		written, _, _ := l.AuditStats()
		if written >= 3 { // post + reconcile + reconcile_export
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit log written = %d, want >= 3 (post, reconcile, reconcile_export)", written)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exp.KeyID == "" {
		t.Error("KeyID empty")
	}
}
