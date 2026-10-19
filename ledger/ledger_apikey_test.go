package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateAPIKeyRoundTrip(t *testing.T) {
	l := New()
	k, secret, err := l.CreateAPIKey("reconciler", []string{ScopeReadLedger, ScopeWriteEntries}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !strings.HasPrefix(secret, apiKeySecretPrefix) {
		t.Fatalf("secret missing prefix: %q", secret)
	}
	if k.KeyPrefix == "" || !strings.HasPrefix(secret, apiKeySecretPrefix+k.KeyPrefix) {
		t.Fatalf("key prefix %q not a prefix of the secret", k.KeyPrefix)
	}
	got, err := l.AuthenticateAPIKey(secret)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey: %v", err)
	}
	if got.ID != k.ID || got.Name != "reconciler" {
		t.Fatalf("authenticated wrong key: %+v", got)
	}
	// The hash must never leave the package: public copies are zeroed.
	var zero [32]byte
	if got.KeyHash != zero {
		t.Fatal("authenticated copy leaks the key hash")
	}
}

func TestCreateAPIKeyValidation(t *testing.T) {
	l := New()
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name       string
		keyName    string
		scopes     []string
		expires    time.Time
		rate       int64
		wantErr    error
	}{
		{"bad scope", "a", []string{"root:everything"}, future, 0, ErrInvalidAPIKeyScope},
		{"dup scope", "a", []string{ScopeReadLedger, ScopeReadLedger}, future, 0, ErrInvalidAPIKeyScope},
		{"no scopes", "a", nil, future, 0, ErrInvalidAPIKeyScope},
		{"empty name", "", []string{ScopeReadLedger}, future, 0, ErrInvalidAPIKeyName},
		{"past expiry", "a", []string{ScopeReadLedger}, time.Now().Add(-time.Hour), 0, ErrInvalidAPIKeyExpiry},
		{"negative rate", "a", []string{ScopeReadLedger}, future, -1, ErrInvalidAPIKeyRateLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := l.CreateAPIKey(c.keyName, c.scopes, c.expires, c.rate)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestAuthenticateAPIKeyFailures(t *testing.T) {
	l := New()
	k, secret, err := l.CreateAPIKey("x", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.AuthenticateAPIKey("bogus"); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Fatalf("bogus secret: got %v", err)
	}
	if _, err := l.AuthenticateAPIKey("lak_" + strings.Repeat("0", 64)); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Fatalf("unknown well-formed secret: got %v", err)
	}
	if _, err := l.RevokeAPIKey(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AuthenticateAPIKey(secret); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Fatalf("revoked key: got %v", err)
	}
}

func TestAPIKeyExpiry(t *testing.T) {
	l := New()
	_, secret, err := l.CreateAPIKey("short", []string{ScopeReadLedger}, time.Now().Add(50*time.Millisecond), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.AuthenticateAPIKey(secret); err != nil {
		t.Fatalf("fresh key should authenticate: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := l.AuthenticateAPIKey(secret); !errors.Is(err, ErrAPIKeyExpired) {
		t.Fatalf("expired key: got %v", err)
	}
}

func TestRotateAPIKeyDualRun(t *testing.T) {
	l := New()
	old, oldSecret, err := l.CreateAPIKey("worker", []string{ScopeWriteTransfer}, time.Now().Add(2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	nk, newSecret, err := l.RotateAPIKey(old.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if nk.ID == old.ID {
		t.Fatal("rotation must issue a new key identity")
	}
	if nk.RotatedFrom != old.ID {
		t.Fatalf("RotatedFrom = %q, want %q", nk.RotatedFrom, old.ID)
	}
	// Dual-run: both secrets authenticate during the grace window.
	if _, err := l.AuthenticateAPIKey(oldSecret); err != nil {
		t.Fatalf("old secret should work during grace: %v", err)
	}
	if _, err := l.AuthenticateAPIKey(newSecret); err != nil {
		t.Fatalf("new secret should work: %v", err)
	}
	// Scopes and quota carry over.
	if !nk.HasScope(ScopeWriteTransfer) {
		t.Fatal("rotated key lost scopes")
	}
}

func TestRotateAPIKeyZeroGrace(t *testing.T) {
	l := New()
	old, oldSecret, err := l.CreateAPIKey("worker", []string{ScopeReadLedger}, time.Now().Add(2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.RotateAPIKey(old.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AuthenticateAPIKey(oldSecret); !errors.Is(err, ErrAPIKeyExpired) {
		t.Fatalf("zero-grace rotation must retire the old secret at once: %v", err)
	}
}

func TestRotateAPIKeyUnknown(t *testing.T) {
	l := New()
	if _, _, err := l.RotateAPIKey("key_nope", time.Hour); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestRevokeAPIKeyIdempotent(t *testing.T) {
	l := New()
	k, _, err := l.CreateAPIKey("x", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := l.RevokeAPIKey(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.RevokeAPIKey(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RevokedAt.Equal(second.RevokedAt) {
		t.Fatal("revoke must be idempotent")
	}
	if _, err := l.RevokeAPIKey("key_nope"); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestAPIKeyScopeImplication(t *testing.T) {
	l := New()
	k, _, err := l.CreateAPIKey("root", []string{ScopeAdminKeys}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ScopeReadLedger, ScopeWriteEntries, ScopeWriteTransfer, ScopeAdminAudit, ScopeAdminKeys} {
		if !k.HasScope(s) {
			t.Fatalf("admin:keys should imply %s", s)
		}
	}
	w, _, err := l.CreateAPIKey("writer", []string{ScopeWriteEntries}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasScope(ScopeReadLedger) {
		t.Fatal("write:entries must not imply read:ledger")
	}
}

func TestConsumeAPIKeyQuota(t *testing.T) {
	l := New()
	k, _, err := l.CreateAPIKey("limited", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !l.ConsumeAPIKeyQuota(k.ID) || !l.ConsumeAPIKeyQuota(k.ID) {
		t.Fatal("first two consumes within quota should pass")
	}
	if l.ConsumeAPIKeyQuota(k.ID) {
		t.Fatal("third consume should be denied")
	}
	// Unknown keys and unlimited keys always pass.
	if !l.ConsumeAPIKeyQuota("key_nope") {
		t.Fatal("unknown key should not be quota-blocked")
	}
	u, _, err := l.CreateAPIKey("unlimited", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if !l.ConsumeAPIKeyQuota(u.ID) {
			t.Fatal("unlimited key must always pass")
		}
	}
}

func TestListAPIKeysNoSecrets(t *testing.T) {
	l := New()
	if _, _, err := l.CreateAPIKey("a", []string{ScopeReadLedger}, time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.CreateAPIKey("b", []string{ScopeAdminKeys}, time.Now().Add(time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	keys := l.ListAPIKeys()
	if len(keys) != 2 {
		t.Fatalf("got %d keys", len(keys))
	}
	var zero [32]byte
	for _, k := range keys {
		if k.KeyHash != zero {
			t.Fatalf("listing leaks hash for %s", k.ID)
		}
	}
	if _, ok := l.GetAPIKey(keys[0].ID); !ok {
		t.Fatal("GetAPIKey should find the key")
	}
	if _, ok := l.GetAPIKey("key_nope"); ok {
		t.Fatal("GetAPIKey should miss unknown keys")
	}
}

func TestAPIKeySecretUniqueness(t *testing.T) {
	l := New()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		_, secret, err := l.CreateAPIKey("k", []string{ScopeReadLedger}, time.Time{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if seen[secret] {
			t.Fatal("duplicate secret issued")
		}
		seen[secret] = true
	}
}

func TestAPIKeySnapshotRoundTrip(t *testing.T) {
	src := New()
	_, s1, err := src.CreateAPIKey("keep", []string{ScopeReadLedger, ScopeWriteEntries}, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := src.CreateAPIKey("dead", []string{ScopeAdminAudit}, time.Now().Add(2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.RevokeAPIKey(k2.ID); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Live keys keep authenticating after the restore (hashes travel);
	// revoked keys stay revoked.
	if _, err := restored.AuthenticateAPIKey(s1); err != nil {
		t.Fatalf("restored key should authenticate: %v", err)
	}
	keys := restored.ListAPIKeys()
	if len(keys) != 2 {
		t.Fatalf("got %d keys after restore", len(keys))
	}
	for _, k := range keys {
		if k.ID == k2.ID && k.RevokedAt.IsZero() {
			t.Fatal("revocation did not survive the restore")
		}
	}
	// Corrupt rows fail the import instead of installing broken creds.
	var bad bytes.Buffer
	if err := src.ExportSnapshot(&bad); err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(bad.String(), `"scopes":["read:ledger","write:entries"]`, `"scopes":["root:everything"]`, 1)
	if _, err := ImportSnapshot(strings.NewReader(corrupt)); err == nil {
		t.Fatal("import with invalid scope should fail")
	}
}
