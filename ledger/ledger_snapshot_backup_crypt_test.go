package ledger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey1Hex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
const testKey2Hex = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

func mustParseKeys(t *testing.T, raw string) BackupEncryptionConfig {
	t.Helper()
	cfg, err := ParseBackupEncryptionKeys(raw)
	if err != nil {
		t.Fatalf("ParseBackupEncryptionKeys(%q): %v", raw, err)
	}
	return cfg
}

func TestParseBackupEncryptionKeys(t *testing.T) {
	// Bare hex: single key, stable derived id.
	cfg := mustParseKeys(t, testKey1Hex)
	if cfg.ActiveKeyID == "" {
		t.Fatal("bare hex: empty active key id")
	}
	if len(cfg.Keys) != 1 {
		t.Fatalf("bare hex: %d keys, want 1", len(cfg.Keys))
	}
	// Derived id is stable across parses.
	cfg2 := mustParseKeys(t, testKey1Hex)
	if cfg.ActiveKeyID != cfg2.ActiveKeyID {
		t.Fatalf("derived id unstable: %q vs %q", cfg.ActiveKeyID, cfg2.ActiveKeyID)
	}

	// Ring: first entry active, rest retired.
	cfg = mustParseKeys(t, "new:"+testKey1Hex+",old:"+testKey2Hex)
	if cfg.ActiveKeyID != "new" {
		t.Fatalf("active = %q, want new", cfg.ActiveKeyID)
	}
	if len(cfg.Keys) != 2 {
		t.Fatalf("ring: %d keys, want 2", len(cfg.Keys))
	}
	if _, ok := cfg.ActiveKey(); !ok {
		t.Fatal("ActiveKey() not found")
	}

	// Empty parses to unconfigured, no error.
	cfg = mustParseKeys(t, "")
	if cfg.ActiveKeyID != "" || len(cfg.Keys) != 0 {
		t.Fatalf("empty: got %+v, want zero config", cfg)
	}

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"bad hex", "zzzz"},
		{"short key", "001122"},
		{"odd hex", "0"},
		{"duplicate kid", "a:" + testKey1Hex + ",a:" + testKey2Hex},
		{"bad kid chars", "a/b:" + testKey1Hex},
		{"empty entry", testKey1Hex + ",,"},
		{"empty hex", "a:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseBackupEncryptionKeys(tc.raw); !errors.Is(err, ErrBackupKeyInvalid) {
				t.Fatalf("ParseBackupEncryptionKeys(%q) = %v, want ErrBackupKeyInvalid", tc.raw, err)
			}
		})
	}
}

func TestBackupEncryptDecryptRoundTrip(t *testing.T) {
	cfg := mustParseKeys(t, "k1:"+testKey1Hex)
	plaintext := []byte("{\"record\":\"meta\",\"version\":42}\n{\"record\":\"entry\"}\n")

	sealed, err := EncryptSnapshotBackup(plaintext, cfg)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !HasBackupEncryption(sealed) {
		t.Fatal("sealed backup missing magic")
	}
	if bytes.HasPrefix(sealed, plaintext) {
		t.Fatal("sealed backup starts with plaintext")
	}

	opened, encrypted, err := DecryptSnapshotBackup(sealed, cfg.Keys)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !encrypted {
		t.Fatal("encrypted=false for sealed backup")
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatal("round-trip bytes differ")
	}

	// Header names the active key id.
	lines := strings.SplitN(string(sealed), "\n", 3)
	if !strings.Contains(lines[1], `"kid":"k1"`) {
		t.Fatalf("header = %q, want kid k1", lines[1])
	}

	// Nonces are random: two seals of the same plaintext differ.
	sealed2, err := EncryptSnapshotBackup(plaintext, cfg)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(sealed, sealed2) {
		t.Fatal("two seals identical: nonce not random")
	}
}

func TestBackupDecryptTamperFailsLoud(t *testing.T) {
	cfg := mustParseKeys(t, "k1:"+testKey1Hex)
	sealed, err := EncryptSnapshotBackup([]byte("secret books"), cfg)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a ciphertext byte: GCM authentication fails loudly.
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, _, err := DecryptSnapshotBackup(tampered, cfg.Keys); !errors.Is(err, ErrBackupDecryptFailed) {
		t.Fatalf("tampered ciphertext: err = %v, want ErrBackupDecryptFailed", err)
	}

	// Unknown key id: distinct error so the operator knows to add the
	// retired key to the ring.
	lines := strings.SplitN(string(sealed), "\n", 3)
	swapped := lines[0] + "\n" + strings.Replace(lines[1], `"kid":"k1"`, `"kid":"nope"`, 1) + "\n" + lines[2]
	if _, _, err := DecryptSnapshotBackup([]byte(swapped), cfg.Keys); !errors.Is(err, ErrBackupUnknownKeyID) {
		t.Fatalf("unknown kid: err = %v, want ErrBackupUnknownKeyID", err)
	}

	// Truncated envelope.
	if _, _, err := DecryptSnapshotBackup([]byte(backupCryptMagic), cfg.Keys); !errors.Is(err, ErrBackupEnvelopeInvalid) {
		t.Fatalf("truncated: err = %v, want ErrBackupEnvelopeInvalid", err)
	}

	// Decrypting with the wrong key fails authentication, not a
	// half-decrypted import.
	wrong := mustParseKeys(t, "k1:"+testKey2Hex)
	if _, _, err := DecryptSnapshotBackup(sealed, wrong.Keys); !errors.Is(err, ErrBackupDecryptFailed) {
		t.Fatalf("wrong key: err = %v, want ErrBackupDecryptFailed", err)
	}
}

func TestBackupDecryptPlaintextPassthrough(t *testing.T) {
	plain := []byte("{\"record\":\"meta\"}\n")
	out, encrypted, err := DecryptSnapshotBackup(plain, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if encrypted {
		t.Fatal("encrypted=true for plaintext")
	}
	if !bytes.Equal(out, plain) {
		t.Fatal("plaintext not passed through unchanged")
	}
}

func TestBackupKeyRotation(t *testing.T) {
	old := mustParseKeys(t, "old:"+testKey2Hex)
	sealed, err := EncryptSnapshotBackup([]byte("books v1"), old)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Rotate: new key active, old key retired (decrypt-only). The old
	// backup still opens.
	rotated := mustParseKeys(t, "new:"+testKey1Hex+",old:"+testKey2Hex)
	opened, _, err := DecryptSnapshotBackup(sealed, rotated.Keys)
	if err != nil {
		t.Fatalf("Decrypt after rotation: %v", err)
	}
	if string(opened) != "books v1" {
		t.Fatalf("opened = %q", opened)
	}

	// New backups seal under the new key id.
	sealed2, err := EncryptSnapshotBackup([]byte("books v2"), rotated)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.Contains(string(sealed2), `"kid":"new"`) {
		t.Fatal("post-rotation backup not sealed under new key")
	}
}

func TestBackupWorkerEncryptsFiles(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	mustPostEntry(t, l, "e2")

	cfg := mustParseKeys(t, "k1:"+testKey1Hex)
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 3, MaxRetries: 1,
		Encryption: cfg,
	})
	kind, ok := sw.backupOnce()
	if !ok || kind != "full" {
		t.Fatalf("backupOnce = (%s, %v), want (full, true)", kind, ok)
	}
	names := listBackupFiles(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want 1", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !HasBackupEncryption(data) {
		t.Fatal("backup file on disk is not encrypted")
	}

	// The file restores through the decrypt path.
	restored, err := OpenSnapshotBackup(filepath.Join(dir, names[0]), cfg.Keys)
	if err != nil {
		t.Fatalf("OpenSnapshotBackup: %v", err)
	}
	l2, err := ImportSnapshot(bytes.NewReader(restored))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.BalanceIn("alice", "USD"); got != 200 {
		t.Fatalf("restored alice balance = %d, want 200", got)
	}

	// Audit carries the encryption facts.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Op == "snapshot_backup" {
			found = true
			if e.Details["encrypted"] != true || e.Details["key_id"] != "k1" {
				t.Errorf("snapshot_backup details = %+v, want encrypted=true key_id=k1", e.Details)
			}
		}
	}
	if !found {
		t.Fatal("no snapshot_backup audit event")
	}
}

func TestBackupWorkerEncryptedIncrementalChain(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	cfg := mustParseKeys(t, "k1:"+testKey1Hex)
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 2, MaxRetries: 1,
		Encryption: cfg,
	})
	// full, incr, full, incr: versions 1..4 with an encrypted chain.
	for i := 2; i <= 4; i++ {
		mustPostEntry(t, l, "e"+string(rune('0'+i)))
		if _, ok := sw.backupOnce(); !ok {
			t.Fatalf("backupOnce %d failed", i)
		}
	}
	for _, n := range listBackupFiles(t, dir) {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !HasBackupEncryption(data) {
			t.Fatalf("%s is not encrypted", n)
		}
	}
	loaded, err := LoadSnapshotAtVersionWithKeys(dir, 4, cfg.Keys)
	if err != nil {
		t.Fatalf("LoadSnapshotAtVersionWithKeys: %v", err)
	}
	if got := loaded.BalanceIn("alice", "USD"); got != 400 {
		t.Fatalf("loaded alice balance = %d, want 400", got)
	}
	if loaded.Version() != 4 {
		t.Fatalf("loaded version = %d, want 4", loaded.Version())
	}
}

func TestBackupWorkerFailOpenWarnsOnce(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 3, MaxRetries: 1,
	})
	// Two ticks: plaintext both times, one warning.
	for i := 0; i < 2; i++ {
		if _, ok := sw.backupOnce(); !ok {
			t.Fatalf("backupOnce %d failed", i)
		}
		mustPostEntry(t, l, "warn"+string(rune('a'+i)))
	}
	for _, n := range listBackupFiles(t, dir) {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if HasBackupEncryption(data) {
			t.Fatalf("%s unexpectedly encrypted", n)
		}
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var warnings int
	for _, e := range events {
		if e.Op == "snapshot_backup_unencrypted" {
			warnings++
			if !strings.Contains(e.Details["reason"].(string), "fail-open") {
				t.Errorf("warning reason = %v, want fail-open honesty", e.Details["reason"])
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("snapshot_backup_unencrypted events = %d, want exactly 1", warnings)
	}
}

func TestBackupWorkerFailClosed(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustPostEntry(t, l, "e1")
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 3, MaxRetries: 1,
		Encryption: BackupEncryptionConfig{FailClosed: true},
	})
	kind, ok := sw.backupOnce()
	if ok {
		t.Fatal("fail-closed tick succeeded without a key")
	}
	if kind != "full" {
		t.Fatalf("kind = %q, want full", kind)
	}
	if names := listBackupFiles(t, dir); len(names) != 0 {
		t.Fatalf("files = %v, want none (refused, not plaintext)", names)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var refusals int
	for _, e := range events {
		if e.Op == "snapshot_backup_encryption_missing" {
			refusals++
		}
		if e.Op == "snapshot_backup" {
			t.Error("snapshot_backup event emitted for a refused tick")
		}
	}
	if refusals != 1 {
		t.Fatalf("snapshot_backup_encryption_missing events = %d, want 1", refusals)
	}
}

func TestLoadSnapshotRejectsTamperedEncryptedBackup(t *testing.T) {
	l := New()
	mustPostEntry(t, l, "e1")
	cfg := mustParseKeys(t, "k1:"+testKey1Hex)
	dir := t.TempDir()
	sw := testBackupWorker(t, l, dir, SnapshotBackupConfig{
		KeepFull: 5, KeepIncremental: 5, FullEvery: 3, MaxRetries: 1,
		Encryption: cfg,
	})
	if _, ok := sw.backupOnce(); !ok {
		t.Fatal("backupOnce failed")
	}
	names := listBackupFiles(t, dir)
	path := filepath.Join(dir, names[0])
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = LoadSnapshotAtVersionWithKeys(dir, 1, cfg.Keys)
	if !errors.Is(err, ErrBackupDecryptFailed) {
		t.Fatalf("load tampered backup: err = %v, want ErrBackupDecryptFailed", err)
	}
}
