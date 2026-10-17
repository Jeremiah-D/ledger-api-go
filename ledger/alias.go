package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Account aliases (LG-31).
//
// An alias is a human-readable, ledger-unique label for an account —
// "ops_cash", "stripe.payouts", "acme-corp" — so operators, dashboards,
// and payment ops tooling can address accounts without pasting opaque
// account IDs. One alias names exactly one account, and one account
// carries at most one alias: the mapping is bijective, which keeps
// resolution unambiguous in both directions.
//
// Aliases are classification, not bookkeeping — binding, rebinding, or
// clearing an alias is structural, like SetAccountTags or Freeze: it
// does not bump the journal version, so a labeling change never looks
// like money movement to version-based change detectors. Every change
// is recorded in the structured audit log (LG-29) as "alias_set",
// "alias_changed", or "alias_removed", with the account, the old alias,
// and the new alias — aliases are operator-chosen labels and carry no
// sensitive information. Aliases round-trip through the
// disaster-recovery snapshot config record (see snapshot.go), like the
// account tags.
//
// Alias syntax reuses the tag-segment charset: 1-64 UTF-8 characters of
// [A-Za-z0-9._-] (see checkTagSegment in tags.go). The charset keeps
// aliases safe in URL path segments and JSON without escaping rules,
// which matters because the HTTP layer resolves aliases from the
// /accounts/{id}/ path parameter.
//
// Resolution (see ResolveAccount): a registered alias wins over a
// literal account ID — an alias that happens to equal an account ID
// resolves through the alias map first. Anything not registered as an
// alias is used as the account ID verbatim, so unknown accounts keep
// their historical behavior (Balance and friends report zero).
//
// Self-binding (alias == account ID) is allowed: it resolves identically
// to the plain ID either way, so the binding is a documented no-op in
// effect, recorded as a normal "alias_set". Binding an alias that is
// already bound to a *different* account is rejected with
// ErrAliasConflict — the caller must clear or rebind the alias on its
// owning account first.

// Validation errors for account aliases.
var (
	// ErrEmptyAliasAccount is returned by SetAlias when the account ID
	// is empty.
	ErrEmptyAliasAccount = errors.New("ledger: alias account must not be empty")
	// ErrInvalidAlias is returned by SetAlias when the alias is
	// non-empty but malformed: it must be 1-64 UTF-8 characters of
	// [A-Za-z0-9._-]. An empty alias is not invalid — it is the clear
	// signal (see ClearAlias).
	ErrInvalidAlias = errors.New("ledger: alias must be 1-64 characters of [A-Za-z0-9._-]")
	// ErrAliasConflict is returned by SetAlias when the alias is
	// already bound to a different account. Aliases are
	// ledger-unique: the caller must rebind or clear the alias on its
	// owning account before it can point anywhere else.
	ErrAliasConflict = errors.New("ledger: alias already bound to another account")
	// ErrAliasNotSet is returned by alias lookups that require a bound
	// alias when the account carries none.
	ErrAliasNotSet = errors.New("ledger: account has no alias")
)

// maxAliasLen caps aliases at 64 UTF-8 characters, the same bound as
// the tag segments (see tags.go): long enough for "acme_corp_operating",
// short enough to keep the alias index compact and URL-safe.
const maxAliasLen = 64

// AccountAlias is one row of the account-alias mapping: the alias bound
// to an account. It is the unit the snapshot config record serializes
// (see snapshot.go).
type AccountAlias struct {
	Account AccountID `json:"account"`
	Alias   string    `json:"alias"`
}

// checkAlias validates a non-empty alias: 1-64 UTF-8 characters of
// [A-Za-z0-9._-], via the shared tag-segment validator. The empty alias
// is the clear signal and never reaches this function.
func checkAlias(alias string) error {
	return checkTagSegment(alias, ErrInvalidAlias)
}

// SetAlias binds alias to account, replacing any alias the account
// already had (the old alias is unbound atomically). An empty alias
// clears the account's alias (see ClearAlias).
//
// The alias must be unique ledger-wide: binding an alias that already
// names a different account fails with ErrAliasConflict and changes
// nothing. Setting the account's current alias again is a no-op — it
// returns nil without writing an audit event, so idempotent retries
// stay silent in the compliance trail.
//
// Alias changes are structural: they do not bump the journal version
// and they survive disaster-recovery snapshots. The operation is
// audited as "alias_set" (new binding), "alias_changed" (rebind, with
// old_alias and new_alias), or "alias_removed" (clear, with old_alias),
// attributed to the "SetAlias" actor (or the context operator via
// SetAliasCtx).
func (l *Ledger) SetAlias(account AccountID, alias string) error {
	return l.SetAliasCtx(context.Background(), account, alias)
}

// SetAliasCtx is SetAlias with per-call audit actor attribution (LG-73):
// the audit event records the operator carried by ctx when set, and the
// entrypoint name otherwise.
func (l *Ledger) SetAliasCtx(ctx context.Context, account AccountID, alias string) error {
	if account == "" {
		return ErrEmptyAliasAccount
	}
	if alias != "" {
		if err := checkAlias(alias); err != nil {
			return err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if alias != "" {
		if owner, ok := l.aliases[alias]; ok && owner != account {
			return fmt.Errorf("%w: %q is bound to account %q", ErrAliasConflict, alias, owner)
		}
	}
	old, had := l.accountAliases[account]
	if had && old == alias {
		// Idempotent replay of the current binding: nothing changed,
		// so no audit event — the trail records changes, not retries.
		return nil
	}
	if had {
		delete(l.aliases, old)
	}
	if alias != "" {
		l.aliases[alias] = account
		l.accountAliases[account] = alias
	} else {
		delete(l.accountAliases, account)
	}

	op := "alias_set"
	details := map[string]any{"account": string(account)}
	switch {
	case had && alias == "":
		op = "alias_removed"
		details["old_alias"] = old
	case had:
		op = "alias_changed"
		details["old_alias"] = old
		details["new_alias"] = alias
	default:
		details["new_alias"] = alias
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            op,
		Actor:         "SetAlias",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details:       details,
	})
	return nil
}

// ClearAlias removes the account's alias, if any. It is SetAlias with
// an empty alias: clearing an account that has no alias is a silent
// no-op (nil, no audit event), so teardown paths stay idempotent.
func (l *Ledger) ClearAlias(account AccountID) error {
	return l.SetAlias(account, "")
}

// AliasOf reports the alias bound to account. The second return value
// is false when the account carries no alias — either never aliased or
// cleared.
func (l *Ledger) AliasOf(account AccountID) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.accountAliases[account]
	return a, ok
}

// AliasAccount resolves an alias to the account it names. The second
// return value is false when the alias is not bound to any account.
func (l *Ledger) AliasAccount(alias string) (AccountID, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.aliases[alias]
	return a, ok
}

// ResolveAccount maps an account reference to its canonical account
// ID: a registered alias resolves to the account it names; anything
// else is used as the account ID verbatim. The second return value is
// the account's current alias (empty when the account has none) — the
// HTTP layer echoes it on balance/snapshot/trial-balance responses so
// reporting consumers can label rows without a second lookup.
//
// Unknown references are usable account IDs by design: accounts are
// created by posting, not registered, so Balance and friends report
// zero for them (see Balance). Resolution never fails.
func (l *Ledger) ResolveAccount(ref string) (AccountID, string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if a, ok := l.aliases[ref]; ok {
		return a, ref
	}
	a := AccountID(ref)
	return a, l.accountAliases[a]
}

// accountAliasesLocked lists every account's alias, sorted by account.
// Callers must hold l.mu; the read lock suffices. It feeds the
// disaster-recovery snapshot config record (see snapshot.go).
func (l *Ledger) accountAliasesLocked() []AccountAlias {
	out := make([]AccountAlias, 0, len(l.accountAliases))
	for a, alias := range l.accountAliases {
		out = append(out, AccountAlias{Account: a, Alias: alias})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}
