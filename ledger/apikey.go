package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// API key management for machine-to-machine access to the HTTP API
// (LG-130).
//
// The ledger historically had no authentication at all: operator
// endpoints relied on network posture plus the x-ledger-actor header,
// which is attribution, not authentication. API keys close that gap
// for automation (reconciliation jobs, payout workers) without
// disturbing the existing anonymous-operator flow: enforcement is
// opt-in (WithAPIKeyAuth), and the HTTP middleware is a pure
// pass-through until it is enabled.
//
// Design notes:
//   - The secret is shown exactly once, at creation/rotation time. At
//     rest only SHA-256(secret) is stored, so a database/snapshot leak
//     never yields a usable credential. Snapshot round-trips carry the
//     hash, never the secret.
//   - Secrets look like "lak_" + 64 hex chars (32 random bytes). The
//     first 8 hex chars after the prefix double as a human-readable
//     key prefix for log correlation without ever identifying the
//     credential.
//   - Scopes are coarse and explicit: read:ledger, write:entries,
//     write:transfer, admin:audit, admin:keys. admin:keys implies every
//     other scope (key management is the root of trust). The HTTP
//     middleware maps each route to its required scope; see main.go.
//   - RotateAPIKey issues a NEW key identity whose RotatedFrom points
//     at the old one; the old secret stays valid for the grace window
//     so rolling deployments never drop in flight. RevokeAPIKey is
//     immediate and idempotent.
//   - Rate quotas are an opt-in per-key per-minute token bucket: a
//     compromised key cannot be used to hammer the ledger at line
//     rate. Zero means unlimited (the default).
//   - Every lifecycle change (create/rotate/revoke) emits a structured
//     audit event with who/what/when (the LG-29 trail), so key
//     management itself is tamper-evident. The events carry key IDs
//     and scope lists, never secret material.
//   - Authentication does a linear scan with subtle.ConstantTimeCompare
//     per candidate: key counts are small (tens, not millions), and a
//     hash map keyed by hash would leak existence via timing anyway.
//
// All entry points are safe for concurrent use; mutations take the
// write lock, authentication takes the read lock.

// API key scopes.
const (
	// ScopeReadLedger permits read-only endpoints: balances, entries,
	// trial balances, statements, metrics.
	ScopeReadLedger = "read:ledger"
	// ScopeWriteEntries permits journal-writing endpoints: POST
	// /entries, /entries/batch, reversals.
	ScopeWriteEntries = "write:entries"
	// ScopeWriteTransfer permits transfer booking: POST /transfers and
	// its dry-run.
	ScopeWriteTransfer = "write:transfer"
	// ScopeAdminAudit permits operator endpoints: review
	// approve/reject, freeze, reconcile, audit export, PII erasure.
	ScopeAdminAudit = "admin:audit"
	// ScopeAdminKeys permits API key management itself, and implies
	// every other scope.
	ScopeAdminKeys = "admin:keys"
)

// apiKeyScopes is the closed set of valid scopes.
var apiKeyScopes = map[string]bool{
	ScopeReadLedger:    true,
	ScopeWriteEntries:  true,
	ScopeWriteTransfer: true,
	ScopeAdminAudit:    true,
	ScopeAdminKeys:     true,
}

// apiKeySecretPrefix prefixes every issued secret.
const apiKeySecretPrefix = "lak_"

// apiKeySecretBytes is the entropy per secret: 32 bytes, 256 bits.
const apiKeySecretBytes = 32

// Validation and state errors returned by the API key APIs.
var (
	// ErrInvalidAPIKeyScope is returned when a scope outside the closed
	// set is requested.
	ErrInvalidAPIKeyScope = errors.New("ledger: invalid API key scope")
	// ErrInvalidAPIKeyName is returned for an empty or overlong key
	// name.
	ErrInvalidAPIKeyName = errors.New("ledger: API key name must be 1-128 characters")
	// ErrInvalidAPIKeyExpiry is returned when the expiry is not in the
	// future.
	ErrInvalidAPIKeyExpiry = errors.New("ledger: API key expiry must be in the future")
	// ErrInvalidAPIKeyRateLimit is returned for a negative per-minute
	// rate quota.
	ErrInvalidAPIKeyRateLimit = errors.New("ledger: API key rate limit must not be negative")
	// ErrAPIKeyNotFound is returned for an unknown key ID.
	ErrAPIKeyNotFound = errors.New("ledger: API key not found")
	// ErrAPIKeyExpired is returned when authenticating with an expired
	// key.
	ErrAPIKeyExpired = errors.New("ledger: API key expired")
	// ErrAPIKeyRevoked is returned when authenticating with a revoked
	// key.
	ErrAPIKeyRevoked = errors.New("ledger: API key revoked")
	// ErrAPIKeyInvalid is returned when no stored key matches the
	// presented secret.
	ErrAPIKeyInvalid = errors.New("ledger: invalid API key")
)

// IsValidAPIKeyScope reports whether s is one of the closed scope set.
func IsValidAPIKeyScope(s string) bool { return apiKeyScopes[s] }

// APIKey is one issued credential. KeyHash is never exposed outside the
// package: List/Get return copies with the hash zeroed, and snapshots
// carry the hex form only for re-import matching.
type APIKey struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	KeyHash         [32]byte  `json:"-"`
	KeyPrefix       string    `json:"key_prefix"`
	Scopes          []string  `json:"scopes"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	RevokedAt       time.Time `json:"revoked_at,omitempty"`
	RotatedFrom     string    `json:"rotated_from,omitempty"`
	RateLimitPerMin int64     `json:"rate_limit_per_min,omitempty"`

	// quotaTokens/quotaRefill are the per-minute token bucket state
	// (ConsumeAPIKeyQuota); guarded by l.mu like every other field.
	quotaTokens float64
	quotaRefill time.Time
}

// HasScope reports whether the key grants the required scope.
// admin:keys implies everything.
func (k *APIKey) HasScope(required string) bool {
	for _, s := range k.Scopes {
		if s == ScopeAdminKeys || s == required {
			return true
		}
	}
	return false
}

// active reports whether the key may authenticate at now: neither
// revoked nor expired.
func (k *APIKey) active(now time.Time) bool {
	if !k.RevokedAt.IsZero() && !now.Before(k.RevokedAt) {
		return false
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return false
	}
	return true
}

// publicCopy returns the key with secret material stripped for API
// responses and listings.
func (k *APIKey) publicCopy() *APIKey {
	c := *k
	c.KeyHash = [32]byte{}
	return &c
}

// WithAPIKeyAuth enables API key enforcement for the HTTP layer (the
// middleware is a pass-through without it). Keys can be managed via
// the ledger API regardless; this flag only flips enforcement on.
func WithAPIKeyAuth() Option {
	return func(l *Ledger) { l.apiKeyAuth = true }
}

// APIKeyAuthEnabled reports whether the HTTP layer must enforce API
// key authentication.
func (l *Ledger) APIKeyAuthEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.apiKeyAuth
}

// APIKeyCount returns the number of issued keys (revoked included).
func (l *Ledger) APIKeyCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.apiKeys)
}

func validateAPIKeyParams(name string, scopes []string, expiresAt time.Time, rateLimitPerMin int64, now time.Time) error {
	if len(name) == 0 || len(name) > 128 {
		return ErrInvalidAPIKeyName
	}
	if len(scopes) == 0 {
		return ErrInvalidAPIKeyScope
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !IsValidAPIKeyScope(s) {
			return fmt.Errorf("%w: %q", ErrInvalidAPIKeyScope, s)
		}
		if seen[s] {
			return fmt.Errorf("%w: duplicate %q", ErrInvalidAPIKeyScope, s)
		}
		seen[s] = true
	}
	if !expiresAt.IsZero() && !expiresAt.After(now) {
		return ErrInvalidAPIKeyExpiry
	}
	if rateLimitPerMin < 0 {
		return ErrInvalidAPIKeyRateLimit
	}
	return nil
}

func newAPIKeySecret() (secret string, hash [32]byte, err error) {
	raw := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", [32]byte{}, err
	}
	secret = apiKeySecretPrefix + hex.EncodeToString(raw)
	return secret, sha256.Sum256([]byte(secret)), nil
}

// CreateAPIKey issues a new API key. The returned secret is shown
// exactly once — the caller must deliver it to the key owner now,
// because the ledger stores only its SHA-256 hash and can never
// reproduce it. The audit event records who/what/when (actor, key ID,
// scopes), never secret material.
func (l *Ledger) CreateAPIKey(name string, scopes []string, expiresAt time.Time, rateLimitPerMin int64) (*APIKey, string, error) {
	return l.CreateAPIKeyCtx(context.Background(), name, scopes, expiresAt, rateLimitPerMin)
}

// CreateAPIKeyCtx is CreateAPIKey with actor attribution from ctx.
func (l *Ledger) CreateAPIKeyCtx(ctx context.Context, name string, scopes []string, expiresAt time.Time, rateLimitPerMin int64) (*APIKey, string, error) {
	now := time.Now().UTC()
	if err := validateAPIKeyParams(name, scopes, expiresAt, rateLimitPerMin, now); err != nil {
		return nil, "", err
	}
	secret, hash, err := newAPIKeySecret()
	if err != nil {
		return nil, "", err
	}
	sorted := append([]string(nil), scopes...)
	sort.Strings(sorted)
	k := &APIKey{
		ID:              "key_" + hex.EncodeToString(hash[:8]),
		Name:            name,
		KeyHash:         hash,
		KeyPrefix:       secret[len(apiKeySecretPrefix) : len(apiKeySecretPrefix)+8],
		Scopes:          sorted,
		CreatedAt:       now,
		ExpiresAt:       expiresAt.UTC(),
		RateLimitPerMin: rateLimitPerMin,
		quotaTokens:     float64(rateLimitPerMin),
		quotaRefill:     now,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apiKeys[k.ID] = k
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "api_key_created",
		Actor:   "CreateAPIKey",
		Details: map[string]any{"key_id": k.ID, "name": name, "scopes": sorted, "expires_at": expiresAt.UTC().Format(time.RFC3339)},
	})
	return k.publicCopy(), secret, nil
}

// RotateAPIKey issues a fresh secret for the same logical identity and
// retires the old key after the grace window (dual-run): automation
// can roll to the new secret without dropping in flight. The old key
// keeps its original expiry if that comes first. Both the retirement
// and the issuance are audit-logged.
func (l *Ledger) RotateAPIKey(id string, grace time.Duration) (*APIKey, string, error) {
	return l.RotateAPIKeyCtx(context.Background(), id, grace)
}

// RotateAPIKeyCtx is RotateAPIKey with actor attribution from ctx.
func (l *Ledger) RotateAPIKeyCtx(ctx context.Context, id string, grace time.Duration) (*APIKey, string, error) {
	if grace < 0 {
		grace = 0
	}
	now := time.Now().UTC()
	secret, hash, err := newAPIKeySecret()
	if err != nil {
		return nil, "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	old, ok := l.apiKeys[id]
	if !ok {
		return nil, "", ErrAPIKeyNotFound
	}
	if !old.RevokedAt.IsZero() {
		return nil, "", ErrAPIKeyRevoked
	}
	// Retire the old key at the end of the grace window (or its own
	// expiry, whichever comes first): dual-run, never a gap, never an
	// overlap past the window. The new key inherits the old key's
	// ORIGINAL expiry horizon — capping it to the grace window would
	// kill the fresh secret at once on zero-grace rotations.
	origExpiry := old.ExpiresAt
	retireAt := now.Add(grace)
	if !origExpiry.IsZero() && origExpiry.Before(retireAt) {
		retireAt = origExpiry
	}
	old.ExpiresAt = retireAt
	sorted := append([]string(nil), old.Scopes...)
	nk := &APIKey{
		ID:              "key_" + hex.EncodeToString(hash[:8]),
		Name:            old.Name,
		KeyHash:         hash,
		KeyPrefix:       secret[len(apiKeySecretPrefix) : len(apiKeySecretPrefix)+8],
		Scopes:          sorted,
		CreatedAt:       now,
		ExpiresAt:       origExpiry,
		RotatedFrom:     old.ID,
		RateLimitPerMin: old.RateLimitPerMin,
		quotaTokens:     float64(old.RateLimitPerMin),
		quotaRefill:     now,
	}
	l.apiKeys[nk.ID] = nk
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "api_key_rotated",
		Actor:   "RotateAPIKey",
		Details: map[string]any{"key_id": nk.ID, "rotated_from": old.ID, "grace_seconds": int64(grace / time.Second)},
	})
	return nk.publicCopy(), secret, nil
}

// RevokeAPIKey retires a key immediately: in-flight requests holding
// the secret fail closed on their next authenticate. Idempotent —
// revoking twice returns the original record.
func (l *Ledger) RevokeAPIKey(id string) (*APIKey, error) {
	return l.RevokeAPIKeyCtx(context.Background(), id)
}

// RevokeAPIKeyCtx is RevokeAPIKey with actor attribution from ctx.
func (l *Ledger) RevokeAPIKeyCtx(ctx context.Context, id string) (*APIKey, error) {
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	k, ok := l.apiKeys[id]
	if !ok {
		return nil, ErrAPIKeyNotFound
	}
	if k.RevokedAt.IsZero() {
		k.RevokedAt = now
	}
	out := k.publicCopy()
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "api_key_revoked",
		Actor:   "RevokeAPIKey",
		Details: map[string]any{"key_id": k.ID, "name": k.Name},
	})
	return out, nil
}

// AuthenticateAPIKey verifies a presented secret against the stored
// hashes with constant-time comparison. It returns a public copy —
// the hash never leaves the package.
func (l *Ledger) AuthenticateAPIKey(secret string) (*APIKey, error) {
	if !strings.HasPrefix(secret, apiKeySecretPrefix) {
		return nil, ErrAPIKeyInvalid
	}
	digest := sha256.Sum256([]byte(secret))
	now := time.Now().UTC()
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, k := range l.apiKeys {
		if subtle.ConstantTimeCompare(k.KeyHash[:], digest[:]) != 1 {
			continue
		}
		if !k.RevokedAt.IsZero() {
			return nil, ErrAPIKeyRevoked
		}
		if !k.active(now) {
			return nil, ErrAPIKeyExpired
		}
		return k.publicCopy(), nil
	}
	return nil, ErrAPIKeyInvalid
}

// ConsumeAPIKeyQuota takes one request from the key's per-minute
// bucket. Keys without a quota (RateLimitPerMin == 0) always pass.
// The bucket refills lazily on wall-clock minutes.
func (l *Ledger) ConsumeAPIKeyQuota(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	k, ok := l.apiKeys[id]
	if !ok || k.RateLimitPerMin <= 0 {
		return true
	}
	now := time.Now().UTC()
	if elapsed := now.Sub(k.quotaRefill); elapsed > 0 {
		k.quotaTokens += elapsed.Minutes() * float64(k.RateLimitPerMin)
		if k.quotaTokens > float64(k.RateLimitPerMin) {
			k.quotaTokens = float64(k.RateLimitPerMin)
		}
		k.quotaRefill = now
	}
	if k.quotaTokens < 1 {
		return false
	}
	k.quotaTokens--
	return true
}

// GetAPIKey returns the public record for a key ID.
func (l *Ledger) GetAPIKey(id string) (*APIKey, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	k, ok := l.apiKeys[id]
	if !ok {
		return nil, false
	}
	return k.publicCopy(), true
}

// ListAPIKeys returns every key's public record, sorted by creation
// time. Secret material is never included.
func (l *Ledger) ListAPIKeys() []*APIKey {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*APIKey, 0, len(l.apiKeys))
	for _, k := range l.apiKeys {
		out = append(out, k.publicCopy())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// apiKeySnapshot is the serializable form of an API key for snapshot
// round-trips: the hash travels (authentication keeps working after a
// restore), the secret never does (it was shown once and is
// unrecoverable by design).
type apiKeySnapshot struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	KeyHashHex      string   `json:"key_hash"`
	KeyPrefix       string   `json:"key_prefix"`
	Scopes          []string `json:"scopes"`
	CreatedAt       string   `json:"created_at"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	RevokedAt       string   `json:"revoked_at,omitempty"`
	RotatedFrom     string   `json:"rotated_from,omitempty"`
	RateLimitPerMin int64    `json:"rate_limit_per_min,omitempty"`
}

// apiKeysSnapshotLocked exports every key for the snapshot config
// line. Callers must hold l.mu (either lock suffices).
func (l *Ledger) apiKeysSnapshotLocked() []apiKeySnapshot {
	out := make([]apiKeySnapshot, 0, len(l.apiKeys))
	for _, k := range l.apiKeys {
		s := apiKeySnapshot{
			ID:              k.ID,
			Name:            k.Name,
			KeyHashHex:      hex.EncodeToString(k.KeyHash[:]),
			KeyPrefix:       k.KeyPrefix,
			Scopes:          append([]string(nil), k.Scopes...),
			CreatedAt:       k.CreatedAt.UTC().Format(time.RFC3339Nano),
			RotatedFrom:     k.RotatedFrom,
			RateLimitPerMin: k.RateLimitPerMin,
		}
		if !k.ExpiresAt.IsZero() {
			s.ExpiresAt = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		if !k.RevokedAt.IsZero() {
			s.RevokedAt = k.RevokedAt.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// restoreAPIKeysLocked validates and installs API keys from a
// snapshot. A snapshot row that could never have been issued (bad
// scope, duplicate ID, malformed hash) fails the import instead of
// silently installing a broken credential. Callers must hold the write
// lock.
func (l *Ledger) restoreAPIKeysLocked(rows []apiKeySnapshot) error {
	seen := make(map[string]bool, len(rows))
	for _, s := range rows {
		if s.ID == "" || seen[s.ID] {
			return fmt.Errorf("duplicate or empty API key id %q", s.ID)
		}
		seen[s.ID] = true
		if len(s.Scopes) == 0 {
			return fmt.Errorf("API key %q has no scopes", s.ID)
		}
		for _, sc := range s.Scopes {
			if !IsValidAPIKeyScope(sc) {
				return fmt.Errorf("API key %q has invalid scope %q", s.ID, sc)
			}
		}
		raw, err := hex.DecodeString(s.KeyHashHex)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("API key %q has malformed key hash", s.ID)
		}
		var hash [32]byte
		copy(hash[:], raw)
		createdAt, err := time.Parse(time.RFC3339Nano, s.CreatedAt)
		if err != nil {
			return fmt.Errorf("API key %q has bad created_at: %w", s.ID, err)
		}
		k := &APIKey{
			ID:              s.ID,
			Name:            s.Name,
			KeyHash:         hash,
			KeyPrefix:       s.KeyPrefix,
			Scopes:          append([]string(nil), s.Scopes...),
			CreatedAt:       createdAt.UTC(),
			RotatedFrom:     s.RotatedFrom,
			RateLimitPerMin: s.RateLimitPerMin,
			quotaTokens:     float64(s.RateLimitPerMin),
			quotaRefill:     time.Now().UTC(),
		}
		if s.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339Nano, s.ExpiresAt)
			if err != nil {
				return fmt.Errorf("API key %q has bad expires_at: %w", s.ID, err)
			}
			k.ExpiresAt = t.UTC()
		}
		if s.RevokedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, s.RevokedAt)
			if err != nil {
				return fmt.Errorf("API key %q has bad revoked_at: %w", s.ID, err)
			}
			k.RevokedAt = t.UTC()
		}
		l.apiKeys[k.ID] = k
	}
	return nil
}
