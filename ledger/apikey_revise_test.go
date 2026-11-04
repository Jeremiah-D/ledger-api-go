package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ptrTime(v time.Time) *time.Time { return &v }
func ptrScopes(v []string) *[]string { return &v }

// revisedAPIKeyEvents reads the current day's audit log and returns
// the api_key_revised events in order.
func revisedAPIKeyEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "api_key_revised" {
			out = append(out, ev)
		}
	}
	return out
}

// mustCreateAPIKey is a test helper that fails on error.
func mustCreateAPIKey(t *testing.T, l *Ledger, name string, scopes []string, expires time.Time, rate int64) (*APIKey, string) {
	t.Helper()
	k, secret, err := l.CreateAPIKey(name, scopes, expires, rate)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return k, secret
}

// Shrinking a key's scopes takes effect on the very next
// authentication: the removed scope is rejected, the kept scope
// still passes.
func TestReviseAPIKeyScopesShrink(t *testing.T) {
	l := New()
	k, secret, err := l.CreateAPIKey("worker", []string{ScopeReadLedger, ScopeWriteEntries}, time.Now().Add(2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := l.ReviseAPIKey(k.ID, APIKeyRevision{
		Scopes: ptrScopes([]string{ScopeReadLedger}),
	})
	if err != nil {
		t.Fatalf("ReviseAPIKey: %v", err)
	}
	if len(revised.Scopes) != 1 || revised.Scopes[0] != ScopeReadLedger {
		t.Fatalf("revised scopes = %v, want [read:ledger]", revised.Scopes)
	}
	if revised.ID != k.ID {
		t.Fatalf("revision moved identity: %q -> %q", k.ID, revised.ID)
	}
	got, err := l.AuthenticateAPIKey(secret)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey after scope shrink: %v", err)
	}
	if !got.HasScope(ScopeReadLedger) {
		t.Fatal("kept scope read:ledger should still pass")
	}
	if got.HasScope(ScopeWriteEntries) {
		t.Fatal("removed scope write:entries should be rejected")
	}
}

// Tightening the rate limit applies the new quota at once (surplus
// tokens above the new cap do not survive); raising it grants no free
// top-up — the key keeps exactly the tokens it had earned.
func TestReviseAPIKeyRateLimitTightened(t *testing.T) {
	l := New()
	k, _, err := l.CreateAPIKey("limited", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if !l.ConsumeAPIKeyQuota(k.ID) {
			t.Fatalf("consume %d should pass under limit 10", i+1)
		}
	}
	if _, err := l.ReviseAPIKey(k.ID, APIKeyRevision{RateLimitPerMin: ptrInt64(2)}); err != nil {
		t.Fatalf("ReviseAPIKey: %v", err)
	}
	// 7 tokens remained under the old limit; the clamp leaves 2.
	if !l.ConsumeAPIKeyQuota(k.ID) || !l.ConsumeAPIKeyQuota(k.ID) {
		t.Fatal("first two consumes under the new limit 2 should pass")
	}
	if l.ConsumeAPIKeyQuota(k.ID) {
		t.Fatal("third consume should be denied under the new limit 2")
	}

	// Raising the limit does not top the bucket up.
	l2 := New()
	k2, _, err := l2.CreateAPIKey("raised", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !l2.ConsumeAPIKeyQuota(k2.ID) || !l2.ConsumeAPIKeyQuota(k2.ID) {
		t.Fatal("two consumes should drain limit 2")
	}
	if _, err := l2.ReviseAPIKey(k2.ID, APIKeyRevision{RateLimitPerMin: ptrInt64(10)}); err != nil {
		t.Fatalf("ReviseAPIKey: %v", err)
	}
	if l2.ConsumeAPIKeyQuota(k2.ID) {
		t.Fatal("raising the limit must not grant a free top-up: exhausted bucket stays exhausted")
	}
}

// Extending an expired key's expiry revives it; clearing the expiry
// (non-nil zero ExpiresAt) makes it never expire.
func TestReviseAPIKeyExpiryExtendedAndCleared(t *testing.T) {
	l := New()
	k, secret := mustCreateAPIKey(t, l, "short", []string{ScopeReadLedger}, time.Now().Add(50*time.Millisecond), 0)
	time.Sleep(80 * time.Millisecond)
	if _, err := l.AuthenticateAPIKey(secret); !errors.Is(err, ErrAPIKeyExpired) {
		t.Fatalf("expired key: got %v, want ErrAPIKeyExpired", err)
	}
	revised, err := l.ReviseAPIKey(k.ID, APIKeyRevision{ExpiresAt: ptrTime(time.Now().Add(2 * time.Hour))})
	if err != nil {
		t.Fatalf("ReviseAPIKey extend: %v", err)
	}
	if revised.ExpiresAt.IsZero() || time.Until(revised.ExpiresAt) < time.Hour {
		t.Fatalf("extended expiry = %v, want ~2h out", revised.ExpiresAt)
	}
	if _, err := l.AuthenticateAPIKey(secret); err != nil {
		t.Fatalf("revived key should authenticate: %v", err)
	}
	cleared, err := l.ReviseAPIKey(k.ID, APIKeyRevision{ExpiresAt: ptrTime(time.Time{})})
	if err != nil {
		t.Fatalf("ReviseAPIKey clear expiry: %v", err)
	}
	if !cleared.ExpiresAt.IsZero() {
		t.Fatalf("cleared expiry = %v, want zero (never expires)", cleared.ExpiresAt)
	}
	if _, err := l.AuthenticateAPIKey(secret); err != nil {
		t.Fatalf("cleared key should authenticate: %v", err)
	}
}

// An illegal merge is rejected with the corresponding
// ErrInvalidAPIKey* sentinel and the live key stays exactly as it was.
func TestReviseAPIKeyIllegalMergeRejected(t *testing.T) {
	l := New()
	expires := time.Now().Add(2 * time.Hour)
	k, _ := mustCreateAPIKey(t, l, "worker", []string{ScopeReadLedger, ScopeWriteEntries}, expires, 5)
	original, _ := l.GetAPIKey(k.ID)

	cases := []struct {
		name    string
		patch   APIKeyRevision
		wantErr error
	}{
		{"bad scope", APIKeyRevision{Scopes: ptrScopes([]string{"root:everything"})}, ErrInvalidAPIKeyScope},
		{"dup scope", APIKeyRevision{Scopes: ptrScopes([]string{ScopeReadLedger, ScopeReadLedger})}, ErrInvalidAPIKeyScope},
		{"no scopes", APIKeyRevision{Scopes: ptrScopes([]string{})}, ErrInvalidAPIKeyScope},
		{"empty name", APIKeyRevision{Name: ptrString("")}, ErrInvalidAPIKeyName},
		{"name too long", APIKeyRevision{Name: ptrString(strings.Repeat("x", 129))}, ErrInvalidAPIKeyName},
		{"past expiry", APIKeyRevision{ExpiresAt: ptrTime(time.Now().Add(-time.Hour))}, ErrInvalidAPIKeyExpiry},
		{"negative rate", APIKeyRevision{RateLimitPerMin: ptrInt64(-1)}, ErrInvalidAPIKeyRateLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.ReviseAPIKey(k.ID, c.patch); !errors.Is(err, c.wantErr) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
			got, ok := l.GetAPIKey(k.ID)
			if !ok {
				t.Fatal("key vanished after rejected revision")
			}
			if got.Name != original.Name || !equalStringSlices(got.Scopes, original.Scopes) ||
				!got.ExpiresAt.Equal(original.ExpiresAt) || got.RateLimitPerMin != original.RateLimitPerMin {
				t.Fatalf("key moved after rejected revision: %+v, want %+v", got, original)
			}
		})
	}

	// A key whose expiry has already passed can only be revised
	// together with a new future (or cleared) expiry: a name-only
	// revision is rejected rather than silently re-validating a dead
	// credential.
	l2 := New()
	k2, _ := mustCreateAPIKey(t, l2, "stale", []string{ScopeReadLedger}, time.Now().Add(50*time.Millisecond), 0)
	time.Sleep(80 * time.Millisecond)
	if _, err := l2.ReviseAPIKey(k2.ID, APIKeyRevision{Name: ptrString("renamed")}); !errors.Is(err, ErrInvalidAPIKeyExpiry) {
		t.Fatalf("name-only revision of expired key: got %v, want ErrInvalidAPIKeyExpiry", err)
	}
	if got, _ := l2.GetAPIKey(k2.ID); got.Name != "stale" {
		t.Fatalf("expired key renamed despite rejection: %q", got.Name)
	}
}

func TestReviseAPIKeyUnknown(t *testing.T) {
	l := New()
	if _, err := l.ReviseAPIKey("key_nope", APIKeyRevision{Name: ptrString("x")}); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("got %v, want ErrAPIKeyNotFound", err)
	}
	if _, err := l.ReviseAPIKeyCtx(context.Background(), "key_nope", APIKeyRevision{Name: ptrString("x")}); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("Ctx: got %v, want ErrAPIKeyNotFound", err)
	}
}

func TestReviseAPIKeyRevoked(t *testing.T) {
	l := New()
	k, _ := mustCreateAPIKey(t, l, "dead", []string{ScopeReadLedger}, time.Now().Add(time.Hour), 0)
	if _, err := l.RevokeAPIKey(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReviseAPIKey(k.ID, APIKeyRevision{Name: ptrString("x")}); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Fatalf("got %v, want ErrAPIKeyRevoked", err)
	}
	if _, err := l.ReviseAPIKeyCtx(context.Background(), k.ID, APIKeyRevision{Name: ptrString("x")}); !errors.Is(err, ErrAPIKeyRevoked) {
		t.Fatalf("Ctx: got %v, want ErrAPIKeyRevoked", err)
	}
}

// An all-nil patch, and a revision identical to the live key, are
// no-ops: they return the live key and write no audit event.
func TestReviseAPIKeyNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	expires := time.Now().Add(2 * time.Hour)
	k, _ := mustCreateAPIKey(t, l, "steady", []string{ScopeReadLedger}, expires, 5)
	before := l.Version()
	if _, err := l.ReviseAPIKey(k.ID, APIKeyRevision{}); err != nil {
		t.Fatalf("all-nil patch: %v", err)
	}
	same, err := l.ReviseAPIKey(k.ID, APIKeyRevision{
		Name:            ptrString("steady"),
		Scopes:          ptrScopes([]string{ScopeReadLedger}),
		ExpiresAt:       ptrTime(expires),
		RateLimitPerMin: ptrInt64(5),
	})
	if err != nil {
		t.Fatalf("same-value patch: %v", err)
	}
	if same.Name != "steady" || same.RateLimitPerMin != 5 {
		t.Fatalf("no-op returned wrong key: %+v", same)
	}
	if got := l.Version(); got != before {
		t.Fatalf("no-op bumped version: %d -> %d", before, got)
	}
	if events := revisedAPIKeyEvents(t, al); len(events) != 0 {
		t.Fatalf("api_key_revised events = %d, want 0", len(events))
	}
}

// A revision never re-issues the secret: the old secret keeps
// authenticating, the prefix is unchanged, and no new key identity
// appears.
func TestReviseAPIKeySecretUnchanged(t *testing.T) {
	l := New()
	k, secret := mustCreateAPIKey(t, l, "worker", []string{ScopeReadLedger}, time.Now().Add(2*time.Hour), 0)
	revised, err := l.ReviseAPIKey(k.ID, APIKeyRevision{
		Name:            ptrString("renamed"),
		Scopes:          ptrScopes([]string{ScopeReadLedger, ScopeWriteEntries}),
		RateLimitPerMin: ptrInt64(10),
	})
	if err != nil {
		t.Fatalf("ReviseAPIKey: %v", err)
	}
	if revised.ID != k.ID || revised.KeyPrefix != k.KeyPrefix {
		t.Fatalf("revision moved identity/prefix: %+v vs %+v", revised, k)
	}
	if got, err := l.AuthenticateAPIKey(secret); err != nil {
		t.Fatalf("old secret should still authenticate: %v", err)
	} else if got.ID != k.ID || got.Name != "renamed" {
		t.Fatalf("authenticated wrong key after revision: %+v", got)
	}
	if keys := l.ListAPIKeys(); len(keys) != 1 {
		t.Fatalf("keys after revision = %d, want 1 (no new identity)", len(keys))
	}
}

// The Ctx variant attributes the operator (LG-73); the plain variant
// keeps the entrypoint name. The event carries per-changed-field
// previous/current values and nothing else, and the revision does not
// bump the version.
func TestReviseAPIKeyCtxOperatorAttribution(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	expires := time.Now().Add(2 * time.Hour)
	k, _ := mustCreateAPIKey(t, l, "worker", []string{ScopeReadLedger, ScopeWriteEntries}, expires, 5)
	before := l.Version()
	ctx := ContextWithActor(context.Background(), "ops-ana")
	if _, err := l.ReviseAPIKeyCtx(ctx, k.ID, APIKeyRevision{RateLimitPerMin: ptrInt64(2)}); err != nil {
		t.Fatalf("ReviseCtx: %v", err)
	}
	if _, err := l.ReviseAPIKey(k.ID, APIKeyRevision{Name: ptrString("renamed")}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	events := revisedAPIKeyEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("api_key_revised events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-ana" {
		t.Errorf("Ctx revision actor = %q, want ops-ana", events[0].Actor)
	}
	if events[1].Actor != "ReviseAPIKey" {
		t.Errorf("plain revision actor = %q, want ReviseAPIKey", events[1].Actor)
	}
	// Audit details round-trip through JSON, so numbers decode as
	// float64.
	if events[0].Details["previous_rate_limit_per_min"] != float64(5) ||
		events[0].Details["current_rate_limit_per_min"] != float64(2) {
		t.Errorf("Ctx event rate details = %v, want previous 5 current 2", events[0].Details)
	}
	if _, ok := events[0].Details["previous_name"]; ok {
		t.Errorf("Ctx event should not carry name details, got %v", events[0].Details)
	}
	if events[1].Details["previous_name"] != "worker" || events[1].Details["current_name"] != "renamed" {
		t.Errorf("plain event name details = %v, want previous worker current renamed", events[1].Details)
	}
	if events[0].Details["key_id"] != k.ID {
		t.Errorf("event key_id = %v, want %s", events[0].Details["key_id"], k.ID)
	}
	if events[0].TraceID != k.ID {
		t.Errorf("event trace_id = %q, want the key ID", events[0].TraceID)
	}
	if events[0].VersionAfter != events[0].VersionBefore || events[0].VersionBefore != before {
		t.Errorf("revision bumped version: %d -> %d (was %d)", events[0].VersionBefore, events[0].VersionAfter, before)
	}
	// No secret material anywhere in the audit details.
	for _, ev := range events {
		if s := fmt.Sprintf("%v", ev.Details); strings.Contains(s, "lak_") {
			t.Errorf("audit event leaks secret material: %v", ev.Details)
		}
	}
}

// Revising the name is a plain attribute change — and the revision
// flows through a snapshot round-trip with the old secret still
// authenticating.
func TestReviseAPIKeyNameAndSnapshotRoundTrip(t *testing.T) {
	src := New()
	expires := time.Now().Add(2 * time.Hour)
	k, secret := mustCreateAPIKey(t, src, "old-name", []string{ScopeReadLedger, ScopeWriteEntries}, expires, 10)
	revised, err := src.ReviseAPIKey(k.ID, APIKeyRevision{
		Name:   ptrString("new-name"),
		Scopes: ptrScopes([]string{ScopeReadLedger}),
	})
	if err != nil {
		t.Fatalf("ReviseAPIKey: %v", err)
	}
	if revised.Name != "new-name" || len(revised.Scopes) != 1 || revised.Scopes[0] != ScopeReadLedger {
		t.Fatalf("revised key = %+v", revised)
	}
	if !revised.ExpiresAt.Equal(expires) || revised.RateLimitPerMin != 10 || !revised.CreatedAt.Equal(k.CreatedAt) {
		t.Fatalf("revision moved untouched fields: %+v vs %+v", revised, k)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, ok := restored.GetAPIKey(k.ID)
	if !ok {
		t.Fatal("revised key missing after restore")
	}
	if got.Name != "new-name" || len(got.Scopes) != 1 || got.Scopes[0] != ScopeReadLedger {
		t.Fatalf("restored key = %+v, want revised attributes", got)
	}
	if _, err := restored.AuthenticateAPIKey(secret); err != nil {
		t.Fatalf("old secret should authenticate after restore: %v", err)
	}
}
