package ledger

import (
	"context"
	"sort"
	"time"
)

// API key attribute runtime revision (LG-308).
//
// CreateAPIKey mints a key, RotateAPIKey swaps its secret, and
// RevokeAPIKey retires it — but none of them can change a live key's
// name, scopes, expiry, or per-minute rate quota. Until now the only
// way to tighten a scope, extend an expiry, or lower a rate limit was
// revoke + recreate: a new secret the caller must swap in, and the old
// key's quota history and audit chain break with it. This module is
// the orthogonal runtime path: an operator revises any subset of a
// live key's attributes in place, keeping its identity, its secret,
// and its history.
//
// Semantics, deliberately fail-closed throughout, along the LG-304 /
// LG-305 / LG-306 / LG-307 structural revisions:
//
//   - The revision is in place: the key keeps its ID, its secret
//     (KeyHash/KeyPrefix are never re-issued — the secret is
//     write-only and a revision must never re-issue one), its
//     CreatedAt, and its quota usage history. Only the patched fields
//     move. The revision governs only future authentications and
//     quota consumes: a request already in flight is never
//     re-authorized.
//   - ID, CreatedAt, KeyHash, KeyPrefix, and the secret are not
//     revisable: there is no field for them on the patch. Changing
//     the secret is RotateAPIKey; changing the identity is
//     revoke + create.
//   - Validation is merge-then-install: the requested partial patch
//     is merged over the live key (nil fields keep the live value)
//     and the merged key reuses validateAPIKeyParams' full gates —
//     name 1–128, scopes non-empty with every scope valid and no
//     duplicates, expiry zero (never expires) or in the future,
//     rate limit >= 0. An illegal merged key returns the
//     corresponding ErrInvalidAPIKey* sentinel and the live key
//     stays exactly as it was.
//   - A non-nil zero ExpiresAt clears the expiry: the key never
//     expires. (A nil ExpiresAt keeps the live expiry; there is no
//     other way to express "clear" — zero is the only value that
//     cannot be a live expiry, since validateAPIKeyParams rejects
//     past expiries.)
//   - An all-nil patch, and a revision identical to the live key on
//     every field, are no-ops: they return the live key with a nil
//     error and write no audit event, so a retried or duplicated
//     revision cannot spam the audit trail.
//   - An unknown key ID returns ErrAPIKeyNotFound; a revoked key
//     returns ErrAPIKeyRevoked (along RotateAPIKey). Note the merged
//     expiry is validated against now, so a key whose expiry has
//     already passed can only be revised together with a new future
//     expiry (or a cleared one) — a name-only revision of an expired
//     key is rejected with ErrInvalidAPIKeyExpiry rather than
//     silently re-validating a dead credential.
//   - Quota history is preserved across a rate-limit revision:
//     quotaTokens is kept as-is, clamped down to the new cap when it
//     exceeds it, and quotaRefill is untouched. Tightening the limit
//     therefore takes effect immediately (no surplus tokens above
//     the new cap survive), while raising it grants no free top-up —
//     the key keeps exactly the tokens it had earned and refills at
//     the new rate from the next ConsumeAPIKeyQuota call, per that
//     method's lazy wall-clock refill. A limit revised to 0 is
//     unlimited, as before.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     api_key_revised audit event (key_id and previous_*/current_*
//     per changed field). The Ctx variant attributes the operator
//     (LG-73). The event carries key IDs and scope lists, never
//     secret material — along CreateAPIKey.
//   - Locking is the ledger write lock for the whole lookup, merge,
//     validate, install, and audit — the same lock
//     ConsumeAPIKeyQuota takes — so a revision racing a quota
//     consume can neither tear the bucket nor lose a consume: the
//     consume sees the old limit or the new limit, never a
//     half-revised one.
//   - There is no HTTP endpoint for the revision (along LG-241/242):
//     key tuning is in-process structural config.
//   - Snapshots already carry API keys (snapshot.go): a revised key
//     round-trips with its revised attributes, and the old secret
//     keeps authenticating after a restore (the hash travels, the
//     secret never does).
//
// Orthogonality: CreateAPIKey issues, RotateAPIKey swaps the secret,
// RevokeAPIKey retires — this module revises attributes. LG-127 owns
// the direct-debit retry strategy, LG-306 the audit sink delivery
// policy; neither touches key attributes.

/*
	Note: the file ledger/apikey.go owns the key lifecycle; this file
	owns only the revision entry points, so the two stay independently
	reviewable.
*/

// APIKeyRevision is a partial revision of one issued API key. A nil
// field keeps the live value; a non-nil field replaces it. All fields
// nil is a no-op. A non-nil zero ExpiresAt clears the expiry (the key
// never expires). ID, CreatedAt, KeyHash, KeyPrefix, and the secret
// are deliberately absent: they are not revisable.
type APIKeyRevision struct {
	// Name replaces the key's display name (1–128 characters).
	Name *string
	// Scopes replaces the scope list wholesale (non-empty, every
	// scope valid, no duplicates). Narrowing a scope rejects
	// further calls under the removed scope at once.
	Scopes *[]string
	// ExpiresAt replaces the expiry. A non-nil zero value clears
	// it (never expires); a non-nil past value is rejected.
	ExpiresAt *time.Time
	// RateLimitPerMin replaces the per-minute quota (0 = unlimited).
	// Quota usage history is preserved; see the package doc.
	RateLimitPerMin *int64
}

// mergeAPIKeyRevision overlays a partial revision on the live key:
// nil fields keep the live value. The Scopes list is deep-copied and
// sorted (along CreateAPIKey) so the installed key never aliases the
// caller's slice.
func mergeAPIKeyRevision(live APIKey, patch APIKeyRevision) APIKey {
	merged := live
	if patch.Name != nil {
		merged.Name = *patch.Name
	}
	if patch.Scopes != nil {
		merged.Scopes = append([]string(nil), (*patch.Scopes)...)
		sort.Strings(merged.Scopes)
	}
	if patch.ExpiresAt != nil {
		merged.ExpiresAt = patch.ExpiresAt.UTC()
	}
	if patch.RateLimitPerMin != nil {
		merged.RateLimitPerMin = *patch.RateLimitPerMin
	}
	return merged
}

// apiKeyRevisionEqual reports whether two keys agree on every
// revisable field. ID, CreatedAt, KeyHash, KeyPrefix, and the secret
// are never revised; quota bucket state is deliberately excluded —
// a rate change that leaves the limit equal is a no-op even if the
// bucket has since been drawn on.
func apiKeyRevisionEqual(a, b APIKey) bool {
	return a.Name == b.Name &&
		equalStringSlices(a.Scopes, b.Scopes) &&
		a.ExpiresAt.Equal(b.ExpiresAt) &&
		a.RateLimitPerMin == b.RateLimitPerMin
}

// equalStringSlices reports whether two string lists hold the same
// elements in the same order.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// formatAPIKeyExpiry renders an expiry for the audit trail: RFC3339,
// or "" when the key never expires.
func formatAPIKeyExpiry(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ReviseAPIKey revises one issued API key's attributes in place and
// returns the revised key. See ReviseAPIKeyCtx.
func (l *Ledger) ReviseAPIKey(id string, patch APIKeyRevision) (*APIKey, error) {
	return l.ReviseAPIKeyCtx(context.Background(), id, patch)
}

// ReviseAPIKeyCtx is ReviseAPIKey with per-call audit actor
// attribution (LG-73). The patch merges over the live key and the
// merged key is re-validated with validateAPIKeyParams' full gates;
// an illegal merge returns the corresponding ErrInvalidAPIKey*
// sentinel with the live key untouched. An unknown ID returns
// ErrAPIKeyNotFound; a revoked key returns ErrAPIKeyRevoked. An
// all-nil patch, and a revision identical to the live key, are no-ops
// that return the live key with a nil error and no audit event. A
// real revision installs the merged attributes in place — identity,
// secret, and quota usage history are preserved — and emits one
// api_key_revised audit event (key_id, previous_*/current_* per
// changed field). The revision bumps neither the version nor the
// audit chain, writes no journal entry, and emits no outbox event.
func (l *Ledger) ReviseAPIKeyCtx(ctx context.Context, id string, patch APIKeyRevision) (*APIKey, error) {
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	k, ok := l.apiKeys[id]
	if !ok {
		return nil, ErrAPIKeyNotFound
	}
	if !k.RevokedAt.IsZero() {
		return nil, ErrAPIKeyRevoked
	}
	previous := *k
	merged := mergeAPIKeyRevision(previous, patch)
	// Quota bucket state travels with the merged key untouched:
	// mergeAPIKeyRevision copies the struct, so quotaTokens and
	// quotaRefill survive the revision as-is.
	if err := validateAPIKeyParams(merged.Name, merged.Scopes, merged.ExpiresAt, merged.RateLimitPerMin, now); err != nil {
		return nil, err
	}
	if apiKeyRevisionEqual(previous, merged) {
		return k.publicCopy(), nil
	}
	// Preserve quota usage history across a rate-limit change: keep
	// the tokens the key has earned, clamped down to the new cap so
	// a tightened limit bites immediately, and leave quotaRefill
	// untouched so a raised limit grants no free top-up — the bucket
	// refills at the new rate from the next ConsumeAPIKeyQuota call.
	if merged.RateLimitPerMin != previous.RateLimitPerMin && merged.quotaTokens > float64(merged.RateLimitPerMin) {
		merged.quotaTokens = float64(merged.RateLimitPerMin)
	}
	*k = merged
	details := map[string]any{"key_id": k.ID}
	if merged.Name != previous.Name {
		details["previous_name"] = previous.Name
		details["current_name"] = merged.Name
	}
	if !equalStringSlices(previous.Scopes, merged.Scopes) {
		details["previous_scopes"] = append([]string(nil), previous.Scopes...)
		details["current_scopes"] = append([]string(nil), merged.Scopes...)
	}
	if !previous.ExpiresAt.Equal(merged.ExpiresAt) {
		details["previous_expires_at"] = formatAPIKeyExpiry(previous.ExpiresAt)
		details["current_expires_at"] = formatAPIKeyExpiry(merged.ExpiresAt)
	}
	if merged.RateLimitPerMin != previous.RateLimitPerMin {
		details["previous_rate_limit_per_min"] = previous.RateLimitPerMin
		details["current_rate_limit_per_min"] = merged.RateLimitPerMin
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "api_key_revised",
		Actor:         "ReviseAPIKey",
		TraceID:       k.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return k.publicCopy(), nil
}
