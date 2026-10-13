package ledger

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	// ErrBackupKeyInvalid rejects a malformed LEDGER_BACKUP_ENCRYPTION_KEY
	// value: bad hex, wrong key length, bad key id, or a duplicate id.
	ErrBackupKeyInvalid = errors.New("ledger: invalid backup encryption key")
	// ErrBackupUnknownKeyID rejects an encrypted backup whose header
	// names a key id the configured ring does not have — the most
	// common shape of a rotation misconfiguration. It is distinct from
	// a decrypt failure: the operator knows exactly what to fix (add
	// the retired key to the ring).
	ErrBackupUnknownKeyID = errors.New("ledger: unknown backup encryption key id")
	// ErrBackupDecryptFailed rejects an encrypted backup whose
	// authentication tag does not verify: tampered, truncated, or cut
	// with the wrong key. GCM fails loudly — a bad backup never
	// imports half-decrypted garbage.
	ErrBackupDecryptFailed = errors.New("ledger: backup decrypt failed")
	// ErrBackupEnvelopeInvalid rejects a file that carries the
	// encryption magic but whose header is malformed.
	ErrBackupEnvelopeInvalid = errors.New("ledger: invalid backup encryption envelope")
)

// Encrypted snapshot backups (LG-77) wrap the LG-24/LG-28 JSONL export
// in an AES-256-GCM envelope (standard library only, the WR-11
// convention of encrypting payloads at rest with GCM + random nonce +
// loud tamper failure). The envelope is content-detected, so encrypted
// and plaintext backups coexist in one backup directory and the
// rotation pruning (which matches the .jsonl suffix) keeps working:
//
//	LEDSNAPENC1\n
//	{"v":1,"enc":"aes-256-gcm","kid":"<key id>","nonce":"<hex>"}\n
//	<raw GCM ciphertext>
//
// The header JSON is the GCM additional authenticated data: swapping a
// header between two files (e.g. renaming the key id) breaks
// authentication. The key id in the header is what makes rotation
// work: a retired key stays in the configured ring as decrypt-only,
// and old backups keep decrypting after the active key changes.

// backupCryptMagic is the first line of every encrypted backup.
const backupCryptMagic = "LEDSNAPENC1\n"

// backupCryptHeader is the authenticated envelope header (line 2).
type backupCryptHeader struct {
	V     int    `json:"v"`
	Enc   string `json:"enc"`
	KeyID string `json:"kid"`
	Nonce string `json:"nonce"`
}

// BackupEncryptionConfig is the parsed LEDGER_BACKUP_ENCRYPTION_KEY.
// The zero value is unconfigured: backups are plaintext unless
// FailClosed refuses the tick.
type BackupEncryptionConfig struct {
	// Keys maps key id to the 32-byte AES key. Empty means no key is
	// configured.
	Keys map[string][32]byte
	// ActiveKeyID is the id backups are encrypted with: the first
	// entry of the configured list. The remaining ids are retired
	// keys kept for decrypting older backups.
	ActiveKeyID string
	// FailClosed, when true and no active key is configured, makes
	// backup ticks fail instead of writing plaintext. The default
	// (false) is fail-open: plaintext backups plus a one-time
	// "snapshot_backup_unencrypted" audit warning that says so
	// honestly.
	FailClosed bool
}

// ActiveKey returns the encryption key, or false when none is
// configured.
func (c BackupEncryptionConfig) ActiveKey() ([32]byte, bool) {
	if c.ActiveKeyID == "" {
		return [32]byte{}, false
	}
	k, ok := c.Keys[c.ActiveKeyID]
	return k, ok
}

// ParseBackupEncryptionKeys parses LEDGER_BACKUP_ENCRYPTION_KEY:
//
//	<64 hex>                              single key, id derived
//	<kid>:<64 hex>[,<kid>:<64 hex>...]    key ring, first entry active
//
// The first entry is the active encryption key; the rest are retired
// (decrypt-only) keys, so rotating the key does not strand older
// backups. A bare hex key gets a stable derived id ("k-" + the first
// 12 hex chars of SHA-256(key)). An empty string parses to the zero
// config (unconfigured), nil error. Anything else malformed is
// ErrBackupKeyInvalid — fail-fast at startup, never a silent
// misconfiguration.
func ParseBackupEncryptionKeys(raw string) (BackupEncryptionConfig, error) {
	var cfg BackupEncryptionConfig
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg, nil
	}
	cfg.Keys = map[string][32]byte{}
	var order []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return cfg, fmt.Errorf("%w: empty key entry", ErrBackupKeyInvalid)
		}
		kid, hexKey := "", part
		if i := strings.Index(part, ":"); i >= 0 {
			kid, hexKey = part[:i], part[i+1:]
		}
		keyBytes, err := hex.DecodeString(hexKey)
		if err != nil || len(keyBytes) != 32 {
			return cfg, fmt.Errorf("%w: key must be 64 hex chars (32 bytes)", ErrBackupKeyInvalid)
		}
		if kid == "" {
			sum := sha256.Sum256(keyBytes)
			kid = "k-" + hex.EncodeToString(sum[:])[:12]
		}
		if !validBackupKeyID(kid) {
			return cfg, fmt.Errorf("%w: bad key id %q (1-32 chars of [A-Za-z0-9_-])", ErrBackupKeyInvalid, kid)
		}
		if _, dup := cfg.Keys[kid]; dup {
			return cfg, fmt.Errorf("%w: duplicate key id %q", ErrBackupKeyInvalid, kid)
		}
		var key [32]byte
		copy(key[:], keyBytes)
		cfg.Keys[kid] = key
		order = append(order, kid)
	}
	cfg.ActiveKeyID = order[0]
	return cfg, nil
}

func validBackupKeyID(kid string) bool {
	if len(kid) == 0 || len(kid) > 32 {
		return false
	}
	for _, c := range kid {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// EncryptSnapshotBackup seals plaintext (a full or incremental
// snapshot export) under the config's active key. It returns
// ErrBackupKeyInvalid when no active key is configured — callers that
// tolerate plaintext check ActiveKey first and skip encryption, so a
// missing key is never silently confused with an encryption failure.
func EncryptSnapshotBackup(plaintext []byte, cfg BackupEncryptionConfig) ([]byte, error) {
	key, ok := cfg.ActiveKey()
	if !ok {
		return nil, fmt.Errorf("%w: no active key configured", ErrBackupKeyInvalid)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("ledger: backup encrypt: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("ledger: backup encrypt: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("ledger: backup encrypt: %w", err)
	}
	header := backupCryptHeader{V: 1, Enc: "aes-256-gcm", KeyID: cfg.ActiveKeyID, Nonce: hex.EncodeToString(nonce)}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("ledger: backup encrypt: %w", err)
	}
	var out bytes.Buffer
	out.WriteString(backupCryptMagic)
	out.Write(headerJSON)
	out.WriteByte('\n')
	// The header is additional authenticated data: a header swapped
	// between files (or a renamed key id) fails authentication.
	out.Write(gcm.Seal(nil, nonce, plaintext, headerJSON))
	return out.Bytes(), nil
}

// HasBackupEncryption reports whether data carries the encrypted-backup
// envelope. Plaintext snapshot exports start with a JSON meta line
// ({"record":"meta",...}); the magic line can never collide with that
// because a JSON export never starts with "LEDSNAPENC1".
func HasBackupEncryption(data []byte) bool {
	return bytes.HasPrefix(data, []byte(backupCryptMagic))
}

// DecryptSnapshotBackup opens an encrypted backup envelope with any key
// of the ring whose id the header names. Plaintext input passes
// through unchanged (encrypted=false), so restore paths can call this
// unconditionally and stay backward compatible. Unknown key ids and
// authentication failures are loud, distinct errors: a backup that
// cannot be authenticated never imports.
func DecryptSnapshotBackup(data []byte, keys map[string][32]byte) (plaintext []byte, encrypted bool, err error) {
	if !HasBackupEncryption(data) {
		return data, false, nil
	}
	rest := data[len(backupCryptMagic):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return nil, true, fmt.Errorf("%w: truncated header", ErrBackupEnvelopeInvalid)
	}
	headerJSON := rest[:nl]
	ciphertext := rest[nl+1:]
	var header backupCryptHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrBackupEnvelopeInvalid, err)
	}
	if header.V != 1 || header.Enc != "aes-256-gcm" || header.KeyID == "" || header.Nonce == "" {
		return nil, true, fmt.Errorf("%w: unsupported header %+v", ErrBackupEnvelopeInvalid, header)
	}
	key, ok := keys[header.KeyID]
	if !ok {
		return nil, true, fmt.Errorf("%w: %q", ErrBackupUnknownKeyID, header.KeyID)
	}
	nonce, err := hex.DecodeString(header.Nonce)
	if err != nil {
		return nil, true, fmt.Errorf("%w: bad nonce: %v", ErrBackupEnvelopeInvalid, err)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, true, fmt.Errorf("ledger: backup decrypt: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, true, fmt.Errorf("ledger: backup decrypt: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, true, fmt.Errorf("%w: bad nonce length %d", ErrBackupEnvelopeInvalid, len(nonce))
	}
	plaintext, err = gcm.Open(nil, nonce, ciphertext, headerJSON)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrBackupDecryptFailed, err)
	}
	return plaintext, true, nil
}

// OpenSnapshotBackup reads a backup file and returns its plaintext
// snapshot bytes, decrypting when the file carries the encryption
// envelope. keys is the decryption ring (may be nil for a
// plaintext-only directory): an encrypted file with no matching key
// fails, and the import is refused — never a half-decrypted import.
func OpenSnapshotBackup(path string, keys map[string][32]byte) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open snapshot %q: %w", path, err)
	}
	plaintext, _, err := DecryptSnapshotBackup(data, keys)
	if err != nil {
		return nil, fmt.Errorf("ledger: open snapshot %q: %w", path, err)
	}
	return plaintext, nil
}
