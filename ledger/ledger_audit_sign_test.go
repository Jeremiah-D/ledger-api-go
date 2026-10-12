package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSigningKeys returns a two-key rotation set: k1 active, k2 retired an
// hour ago. Key material is fixed (not random) so the tests are
// deterministic; these keys never leave the test process.
func testSigningKeys(t *testing.T) []AuditSigningKey {
	t.Helper()
	raw := "k1:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff," +
		"k2:ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100@" +
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	keys, err := ParseAuditSigningKeys(raw)
	if err != nil {
		t.Fatalf("ParseAuditSigningKeys: %v", err)
	}
	return keys
}

func mustSignedAuditLog(t *testing.T, keys []AuditSigningKey, grace time.Duration) *AuditLog {
	t.Helper()
	al, err := NewAuditLog(t.TempDir(), WithAuditLogSigning(keys, grace))
	if err != nil {
		t.Fatalf("NewAuditLog signed: %v", err)
	}
	return al
}

// readRawAuditLines returns the raw JSONL lines of the day's audit file.
func readRawAuditLines(t *testing.T, dir string, day string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "audit-"+day+"*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob audit files: %v %v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

func TestAuditLogSignsLines(t *testing.T) {
	keys := testSigningKeys(t)
	al := mustSignedAuditLog(t, keys, time.Hour)
	defer al.Close()
	if !al.Signed() {
		t.Fatal("Signed() = false, want true")
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	day := time.Now().UTC().Format("2006-01-02")
	rep, err := VerifyAuditSignatures(al.Dir(), keys, time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("report not ok: %+v", rep.BadLines)
	}
	if rep.SignedLines == 0 || rep.UnsignedLines != 0 {
		t.Fatalf("signed=%d unsigned=%d, want signed>0 unsigned=0", rep.SignedLines, rep.UnsignedLines)
	}
	// Every raw line carries a sig header naming the active key.
	for _, ln := range readRawAuditLines(t, al.Dir(), day) {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		sig, ok := m["sig"].(map[string]any)
		if !ok {
			t.Fatalf("line missing sig: %s", ln)
		}
		if sig["kid"] != "k1" {
			t.Fatalf("sig kid = %v, want k1 (the active key)", sig["kid"])
		}
		mac, _ := sig["mac"].(string)
		if len(mac) != 64 {
			t.Fatalf("sig mac length = %d, want 64 hex chars", len(mac))
		}
	}
}

func TestAuditSignaturesDetectTampering(t *testing.T) {
	keys := testSigningKeys(t)
	al := mustSignedAuditLog(t, keys, time.Hour)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 200}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	day := time.Now().UTC().Format("2006-01-02")
	lines := readRawAuditLines(t, al.Dir(), day)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	// Rewrite the second line's amount in place, keeping the signature.
	tampered := strings.Replace(lines[1], `"amount_cents":200`, `"amount_cents":99999`, 1)
	if tampered == lines[1] {
		t.Fatal("test setup: amount not found in line")
	}
	matches, _ := filepath.Glob(filepath.Join(al.Dir(), "audit-"+day+"*.jsonl"))
	if err := os.WriteFile(matches[0], []byte(lines[0]+"\n"+tampered+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	rep, err := VerifyAuditSignatures(al.Dir(), keys, time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if rep.Ok {
		t.Fatal("report ok after tampering, want failure")
	}
	if len(rep.BadLines) != 1 {
		t.Fatalf("bad lines = %+v, want exactly 1", rep.BadLines)
	}
	bad := rep.BadLines[0]
	if bad.Reason != "bad_signature" {
		t.Fatalf("reason = %q, want bad_signature", bad.Reason)
	}
	if bad.Line != 2 || bad.TraceID != "e2" {
		t.Fatalf("bad line = %+v, want line 2 trace e2", bad)
	}
	if !strings.HasSuffix(bad.File, ".jsonl") {
		t.Fatalf("file = %q, want the day's jsonl", bad.File)
	}
}

func TestAuditSignaturesUnknownKey(t *testing.T) {
	keys := testSigningKeys(t)
	al := mustSignedAuditLog(t, keys, time.Hour)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	other, err := ParseAuditSigningKeys("zz:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("parse other keys: %v", err)
	}
	rep, err := VerifyAuditSignatures(al.Dir(), other, time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if rep.Ok || len(rep.BadLines) == 0 || rep.BadLines[0].Reason != "unknown_key" {
		t.Fatalf("want unknown_key failures, got %+v", rep.BadLines)
	}
}

func TestAuditSignaturesRotation(t *testing.T) {
	now := time.Now().UTC()
	k1hex := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	k2hex := hex.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
	retiredAt := now.Add(-2 * time.Hour).Format(time.RFC3339)

	// Phase 1: sign with k1 (active).
	k1only, err := ParseAuditSigningKeys("k1:" + k1hex)
	if err != nil {
		t.Fatalf("parse k1: %v", err)
	}
	dir := t.TempDir()
	al1, err := NewAuditLog(dir, WithAuditLogSigning(k1only, 24*time.Hour))
	if err != nil {
		t.Fatalf("NewAuditLog phase 1: %v", err)
	}
	l1 := New(WithAuditLog(al1))
	if _, _, err := l1.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post e1: %v", err)
	}
	if err := al1.Close(); err != nil {
		t.Fatalf("Close phase 1: %v", err)
	}

	// Phase 2: k1 retired, k2 active — new lines sign with k2.
	both, err := ParseAuditSigningKeys("k1:" + k1hex + "@" + retiredAt + ",k2:" + k2hex)
	if err != nil {
		t.Fatalf("parse both: %v", err)
	}
	al2, err := NewAuditLog(dir, WithAuditLogSigning(both, 24*time.Hour))
	if err != nil {
		t.Fatalf("NewAuditLog phase 2: %v", err)
	}
	l2 := New(WithAuditLog(al2))
	if _, _, err := l2.Post(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 200}); err != nil {
		t.Fatalf("Post e2: %v", err)
	}
	if err := al2.Close(); err != nil {
		t.Fatalf("Close phase 2: %v", err)
	}

	// Full rotation set verifies everything: k1's old lines are inside
	// the 24h grace window, k2's new lines verify as primary.
	rep, err := VerifyAuditSignatures(dir, both, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("rotation verify failed: %+v", rep.BadLines)
	}
	if rep.SignedLines != 2 {
		t.Fatalf("signed = %d, want 2", rep.SignedLines)
	}

	// With a zero grace window the k1-signed line (written after its
	// retirement timestamp) is rejected as key_retired.
	rep2, err := VerifyAuditSignatures(dir, both, 0)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures zero grace: %v", err)
	}
	if rep2.Ok {
		t.Fatal("zero grace: want key_retired failure, got ok")
	}
	found := false
	for _, b := range rep2.BadLines {
		if b.Reason == "key_retired" && b.TraceID == "e1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("zero grace: want key_retired on e1, got %+v", rep2.BadLines)
	}
}

func TestAuditLogUnsignedModeAlert(t *testing.T) {
	al := mustAuditLog(t) // no signing keys: degraded unsigned mode
	defer al.Close()
	if al.Signed() {
		t.Fatal("Signed() = true without keys, want false")
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	day := time.Now().UTC().Format("2006-01-02")
	events, corrupt, err := ReadAuditLog(al.Dir(), day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (alert + post)", len(events))
	}
	alert := events[0]
	if alert.Op != "audit_signing_disabled" || alert.Actor != "NewAuditLog" {
		t.Fatalf("first event = %+v, want the audit_signing_disabled alert", alert)
	}
	if alert.Sig != nil {
		t.Fatal("alert carries a sig in unsigned mode, want nil")
	}

	// The offline verifier counts unsigned lines instead of failing them.
	keys := testSigningKeys(t)
	rep, err := VerifyAuditSignatures(al.Dir(), keys, time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("unsigned log should verify ok, got %+v", rep.BadLines)
	}
	if rep.UnsignedLines != 2 || rep.SignedLines != 0 {
		t.Fatalf("signed=%d unsigned=%d, want 0/2", rep.SignedLines, rep.UnsignedLines)
	}
}

func TestVerifyAuditSignaturesCorruptLine(t *testing.T) {
	keys := testSigningKeys(t)
	al := mustSignedAuditLog(t, keys, time.Hour)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	matches, _ := filepath.Glob(filepath.Join(al.Dir(), "audit-"+day+"*.jsonl"))
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Append a torn line: it must be reported with its line number, not
	// silently skipped.
	raw = append(raw, []byte("{not json\n")...)
	if err := os.WriteFile(matches[0], raw, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	rep, err := VerifyAuditSignatures(al.Dir(), keys, time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if rep.Ok {
		t.Fatal("want corrupt failure, got ok")
	}
	if len(rep.BadLines) != 1 || rep.BadLines[0].Reason != "corrupt" || rep.BadLines[0].Line != 2 {
		t.Fatalf("bad lines = %+v, want one corrupt at line 2", rep.BadLines)
	}
}

func TestParseAuditSigningKeys(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	raw := "k1:001122,k2:aabbcc@" + now.Format(time.RFC3339)
	keys, err := ParseAuditSigningKeys(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "k1" || keys[1].ID != "k2" {
		t.Fatalf("keys = %+v", keys)
	}
	if !keys[0].RetiredAt.IsZero() {
		t.Fatal("k1 should be active")
	}
	if !keys[1].RetiredAt.Equal(now) {
		t.Fatalf("k2 retiredAt = %v, want %v", keys[1].RetiredAt, now)
	}
	if string(keys[0].Key) != "\x00\x11\x22" {
		t.Fatalf("k1 key bytes = %x", keys[0].Key)
	}

	for _, bad := range []string{
		"",
		"   ",
		"nocolon",
		":001122",       // empty id
		"k1:zzzz",       // not hex
		"k1:",           // empty key
		"k1:001122,k1:3344", // duplicate id
		"k1:001122@not-a-time",
	} {
		if _, err := ParseAuditSigningKeys(bad); err == nil {
			t.Fatalf("ParseAuditSigningKeys(%q): want error, got nil", bad)
		}
	}
}

func TestAuditSignerPrimarySelection(t *testing.T) {
	mk := func(id string, retired bool) AuditSigningKey {
		k := AuditSigningKey{ID: id, Key: []byte("0123456789abcdef")}
		if retired {
			k.RetiredAt = time.Now().Add(-time.Hour)
		}
		return k
	}
	// First active key signs, even when listed after a retired one.
	s, err := newAuditSigner([]AuditSigningKey{mk("old", true), mk("new", false)}, time.Hour)
	if err != nil {
		t.Fatalf("newAuditSigner: %v", err)
	}
	if sig := s.sign([]byte("line")); sig.KeyID != "new" {
		t.Fatalf("signed with %q, want new", sig.KeyID)
	}
	// All retired: last one listed signs rather than failing open.
	s2, err := newAuditSigner([]AuditSigningKey{mk("old1", true), mk("old2", true)}, time.Hour)
	if err != nil {
		t.Fatalf("newAuditSigner all-retired: %v", err)
	}
	if sig := s2.sign([]byte("line")); sig.KeyID != "old2" {
		t.Fatalf("signed with %q, want old2", sig.KeyID)
	}
	if _, err := newAuditSigner(nil, time.Hour); err == nil {
		t.Fatal("empty keys: want error")
	}
	if _, err := newAuditSigner([]AuditSigningKey{mk("k", false)}, -time.Hour); err == nil {
		t.Fatal("negative grace: want error")
	}
}
