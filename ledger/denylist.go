package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrAccountDenied is returned by Post, PostTransfer, and Capture when a
// leg of the posting names an account on the sanctions/denylist (see
// AddDeniedAccount and AddDeniedPrefix). A denied account is a
// compliance stop: no new money may move through it in either direction.
//
// Ordering inside the write paths is deliberate and differs from the
// frozen gate on purpose: the denylist screen runs immediately after
// field validation, BEFORE the idempotency replay check. A sanctions
// designation is a legal obligation that applies to replays too — a
// replay books nothing new, but the operator must still see the attempt
// rejected and audited rather than silently succeeding. Hit requests
// leave no idempotency trace: the replay index is only consulted after
// the screen passes, so a denied request can never plant a key.
//
// Reads are unaffected by design: Balance, Snapshot, TrialBalance,
// ListEntries, the audit chain, and Reconcile keep working on denied
// accounts, so compliance and reconciliation tooling can keep watching
// an account while it is stopped. Every rejection emits a
// "denylist_rejected" structured audit event — the compliance trail
// must show the attempt, not just the absence of a posting.
var ErrAccountDenied = errors.New("ledger: account is on the sanctions/denylist")

// ErrInvalidDenylistEntry is returned by the denylist management calls
// when the entry is malformed: an empty account ID, or an empty prefix
// (which would match every account and is almost certainly an operator
// mistake).
var ErrInvalidDenylistEntry = errors.New("ledger: denylist entry must name a non-empty account or prefix")

// DenylistEntry is one sanctions/denylist row: either an exact account ID
// or a prefix pattern. A prefix entry matches every account whose ID
// starts with the prefix — the classic shape is a sanctioned-entity
// namespace like "ofac-sdn-" covering all of that entity's accounts.
// Exact matches win over prefix matches, and among prefixes the longest
// one wins, so attribution names the most specific rule that fired.
type DenylistEntry struct {
	// Value is the account ID (exact entry) or the prefix (pattern
	// entry) that matched.
	Value string `json:"value"`
	// Prefix is true for pattern entries, false for exact account IDs.
	Prefix bool `json:"prefix"`
	// AddedAt is when the entry was installed, UTC.
	AddedAt time.Time `json:"added_at"`
	// AddedBy is the operator actor that installed the entry (see
	// ActorFromContext), empty when installed without attribution.
	AddedBy string `json:"added_by,omitempty"`
}

// Label renders the entry the way rejection errors and audit events name
// it: the bare value for exact entries, "<prefix>*" for patterns.
func (e DenylistEntry) Label() string {
	if e.Prefix {
		return e.Value + "*"
	}
	return e.Value
}

// DenylistSpec is one row of a batch denylist import (see ImportDenylist):
// exactly one of Account or Prefix must be non-empty.
type DenylistSpec struct {
	Account string `json:"account,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
}

// AddDeniedAccount puts an exact account ID on the denylist. Adding an
// already-listed account is idempotent (the original row is kept).
// Structural config, like Freeze: it does not bump the ledger version
// and it survives disaster-recovery snapshots — a restore that dropped
// the list would silently lift sanctions screening.
func (l *Ledger) AddDeniedAccount(a AccountID) DenylistEntry {
	return l.AddDeniedAccountCtx(context.Background(), a)
}

// AddDeniedAccountCtx is AddDeniedAccount with per-call audit actor
// attribution (LG-73): the audit event records the operator carried by
// ctx when set, and the entrypoint name otherwise.
func (l *Ledger) AddDeniedAccountCtx(ctx context.Context, a AccountID) DenylistEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := DenylistEntry{Value: string(a), AddedAt: time.Now().UTC(), AddedBy: ActorFromContext(ctx)}
	if existing, ok := l.deniedExact[a]; ok {
		return existing
	}
	l.deniedExact[a] = entry
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "denylist_added",
		Actor:         "AddDeniedAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details:       map[string]any{"entry": entry.Label()},
	})
	return entry
}

// AddDeniedPrefix puts a prefix pattern on the denylist: every account
// whose ID starts with prefix is denied. An empty prefix is rejected
// with ErrInvalidDenylistEntry — it would deny the whole ledger.
// Idempotent like AddDeniedAccount.
func (l *Ledger) AddDeniedPrefix(prefix string) (DenylistEntry, error) {
	return l.AddDeniedPrefixCtx(context.Background(), prefix)
}

// AddDeniedPrefixCtx is AddDeniedPrefix with per-call audit actor
// attribution.
func (l *Ledger) AddDeniedPrefixCtx(ctx context.Context, prefix string) (DenylistEntry, error) {
	if prefix == "" {
		return DenylistEntry{}, ErrInvalidDenylistEntry
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := DenylistEntry{Value: prefix, Prefix: true, AddedAt: time.Now().UTC(), AddedBy: ActorFromContext(ctx)}
	if existing, ok := l.deniedPrefixes[prefix]; ok {
		return existing, nil
	}
	l.deniedPrefixes[prefix] = entry
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "denylist_added",
		Actor:         "AddDeniedPrefix",
		TraceID:       prefix,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"entry": entry.Label()},
	})
	return entry, nil
}

// RemoveDeniedAccount deletes an exact account ID from the denylist. It
// reports whether a row was actually removed; removing a non-listed
// account is a no-op.
func (l *Ledger) RemoveDeniedAccount(a AccountID) bool {
	return l.RemoveDeniedAccountCtx(context.Background(), a)
}

// RemoveDeniedAccountCtx is RemoveDeniedAccount with per-call audit actor
// attribution.
func (l *Ledger) RemoveDeniedAccountCtx(ctx context.Context, a AccountID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.deniedExact[a]
	if !ok {
		return false
	}
	delete(l.deniedExact, a)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "denylist_removed",
		Actor:         "RemoveDeniedAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details:       map[string]any{"entry": entry.Label()},
	})
	return true
}

// RemoveDeniedPrefix deletes a prefix pattern from the denylist. It
// reports whether a row was actually removed.
func (l *Ledger) RemoveDeniedPrefix(prefix string) bool {
	return l.RemoveDeniedPrefixCtx(context.Background(), prefix)
}

// RemoveDeniedPrefixCtx is RemoveDeniedPrefix with per-call audit actor
// attribution.
func (l *Ledger) RemoveDeniedPrefixCtx(ctx context.Context, prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.deniedPrefixes[prefix]
	if !ok {
		return false
	}
	delete(l.deniedPrefixes, prefix)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "denylist_removed",
		Actor:         "RemoveDeniedPrefix",
		TraceID:       prefix,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"entry": entry.Label()},
	})
	return true
}

// Denylist returns every denylist entry, exact entries first then prefix
// patterns, each group sorted by value. Deterministic for operators and
// tests.
func (l *Ledger) Denylist() []DenylistEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.denylistLocked()
}

// denylistLocked lists the entries. Callers must hold l.mu (either lock
// suffices; nothing is mutated).
func (l *Ledger) denylistLocked() []DenylistEntry {
	out := make([]DenylistEntry, 0, len(l.deniedExact)+len(l.deniedPrefixes))
	for _, e := range l.deniedExact {
		out = append(out, e)
	}
	for _, e := range l.deniedPrefixes {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prefix != out[j].Prefix {
			return !out[i].Prefix
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// ImportDenylist installs a batch of denylist rows at once — the
// operator shape for loading a sanctions feed. The import is atomic:
// every spec is validated first, and a malformed spec aborts the whole
// import with ErrInvalidDenylistEntry before anything is installed.
// Rows that are already listed (same value and kind) are skipped and
// counted, not duplicated. One "denylist_imported" audit event covers
// the batch; per-row events would churn the compliance log on every
// feed refresh.
func (l *Ledger) ImportDenylist(specs []DenylistSpec) (added, skipped int, err error) {
	return l.ImportDenylistCtx(context.Background(), specs)
}

// ImportDenylistCtx is ImportDenylist with per-call audit actor
// attribution.
func (l *Ledger) ImportDenylistCtx(ctx context.Context, specs []DenylistSpec) (added, skipped int, err error) {
	for i, s := range specs {
		if (s.Account == "") == (s.Prefix == "") {
			return 0, 0, fmt.Errorf("%w: spec %d must set exactly one of account or prefix", ErrInvalidDenylistEntry, i)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().UTC()
	by := ActorFromContext(ctx)
	labels := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Account != "" {
			a := AccountID(s.Account)
			if _, ok := l.deniedExact[a]; ok {
				skipped++
				continue
			}
			l.deniedExact[a] = DenylistEntry{Value: s.Account, AddedAt: now, AddedBy: by}
			labels = append(labels, s.Account)
			added++
			continue
		}
		if _, ok := l.deniedPrefixes[s.Prefix]; ok {
			skipped++
			continue
		}
		l.deniedPrefixes[s.Prefix] = DenylistEntry{Value: s.Prefix, Prefix: true, AddedAt: now, AddedBy: by}
		labels = append(labels, s.Prefix+"*")
		added++
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "denylist_imported",
		Actor:         "ImportDenylist",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"added":   added,
			"skipped": skipped,
			"entries": labels,
		},
	})
	return added, skipped, nil
}

// DeniedBy reports the denylist entry that denies account a, if any:
// exact match first, then the longest matching prefix. The second return
// value is false when the account is not denied.
func (l *Ledger) DeniedBy(a AccountID) (DenylistEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, ok := l.denylistMatchLocked(a)
	return entry, ok
}

// denylistMatchLocked finds the entry denying a: the exact row when the
// account ID is listed, otherwise the longest listed prefix it starts
// with. Callers must hold l.mu (either lock suffices).
func (l *Ledger) denylistMatchLocked(a AccountID) (DenylistEntry, bool) {
	if e, ok := l.deniedExact[a]; ok {
		return e, true
	}
	var best DenylistEntry
	found := false
	for prefix, e := range l.deniedPrefixes {
		if strings.HasPrefix(string(a), prefix) {
			if !found || len(prefix) > len(best.Value) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// denylistRejectedLocked reports the first denied account among the
// given legs, in argument order, with the entry that denies it. Write
// paths screen every account money moves through before the idempotency
// replay check (see ErrAccountDenied): a hit rejects the whole posting
// and leaves no idempotency trace. Callers must hold l.mu.
func (l *Ledger) denylistRejectedLocked(accounts ...AccountID) (AccountID, DenylistEntry, bool) {
	for _, a := range accounts {
		if e, ok := l.denylistMatchLocked(a); ok {
			return a, e, true
		}
	}
	return "", DenylistEntry{}, false
}

// deniedRejectionLocked builds the ErrAccountDenied error for a screened
// account and emits the "denylist_rejected" audit event the compliance
// trail requires. extraAccounts names the posting's other legs for the
// audit event; the denied account is listed once even when it also
// appears in extraAccounts. Callers must hold the write lock; the audit
// event records the write path's actor name (Post, PostTransfer, Capture
// — rejections are system decisions, not operator actions, so the
// x-ledger-actor attribution does not apply).
func (l *Ledger) deniedRejectionLocked(actor, traceID string, acct AccountID, entry DenylistEntry, extraAccounts []AccountID) error {
	accounts := []AccountID{acct}
	for _, a := range extraAccounts {
		if a != acct {
			accounts = append(accounts, a)
		}
	}
	l.emitAudit(AuditEvent{
		Op:            "denylist_rejected",
		Actor:         actor,
		TraceID:       traceID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      accounts,
		Details: map[string]any{
			"denied_account": string(acct),
			"matched_entry":  entry.Label(),
		},
	})
	return fmt.Errorf("%w: account %q matched denylist entry %q", ErrAccountDenied, acct, entry.Label())
}
