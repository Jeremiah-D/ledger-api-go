package ledger

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Read-only idempotency key registry query (LG-152).
//
// Every journal-writing operation keeps its own idempotency namespace
// (entries, transfers, holds, captures, sweeps, batches, ...), and an
// operator chasing a "why did my retry return a duplicate?" report
// currently has to guess which namespace a key landed in. This module
// exposes one read-only cross-namespace listing: the key, which
// namespace registered it, what it maps to (an operation ID — never a
// payload or an amount), and when it expires under the idempotency TTL.
//
// The listing is operator tooling, not a replay path: it never returns
// the underlying records, so business payloads (amounts, accounts, memo
// text) and the API keys registry stay out of reach. Namespaces whose
// entries are in-memory-only (netting, installments, clearing) are still
// listed while they live — they expire with the TTL like the rest.

// IdempotencyKeyInfo is one registered idempotency key.
type IdempotencyKeyInfo struct {
	// Namespace is the owning operation family, e.g. "entries" or
	// "transfers".
	Namespace string `json:"namespace"`
	// Key is the registered idempotency key.
	Key string `json:"key"`
	// MapsTo is what the key resolves to: the operation ID it
	// registered (an entry ID, hold ID, sweep ID, ...). It never
	// carries a payload or an amount.
	MapsTo string `json:"maps_to"`
	// ExpiresAt is when the key lapses under the ledger's idempotency
	// TTL; nil when keys never expire (TTL disabled) or the namespace
	// does not expire keys.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// idempotencyNamespaceNames enumerates every idempotency key namespace
// in a stable order. Keep this list in sync with the registries on the
// Ledger struct.
var idempotencyNamespaceNames = []string{
	"entries",
	"transfers",
	"holds",
	"captures",
	"partial-captures",
	"term-deposits",
	"payouts",
	"sweeps",
	"batches",
	"allocations",
	"netting",
	"installments",
	"clearing",
	"merges",
	"splits",
	"schedules",
	"disputes",
}

// IdempotencyNamespaces returns the names of every idempotency key
// namespace the ledger tracks.
func IdempotencyNamespaces() []string {
	return slices.Clone(idempotencyNamespaceNames)
}

// IsIdempotencyNamespace reports whether ns names a known idempotency
// key namespace.
func IsIdempotencyNamespace(ns string) bool {
	return slices.Contains(idempotencyNamespaceNames, ns)
}

// ListIdempotencyKeys returns the registered idempotency keys across
// every namespace, sorted by (namespace, key). prefix filters keys by
// prefix (empty matches all); namespace restricts to one namespace
// (empty matches all, unknown matches none). limit bounds the result: a
// non-positive limit defaults to 100 and the bound is capped at 1000,
// so a full scan of a long-lived ledger cannot OOM the operator's
// browser tab.
//
// The listing holds a single read lock: it reflects a consistent
// instant of the registries, and journal writers are never blocked.
func (l *Ledger) ListIdempotencyKeys(prefix, namespace string, limit int) []IdempotencyKeyInfo {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	// One pass over the journal resolves transfer keys to their
	// registration time; transferKeys only stores entry IDs.
	entryCreated := make(map[string]time.Time, len(l.entries))
	for id, e := range l.entries {
		if e.CreatedAt.IsZero() {
			continue
		}
		if _, ok := entryCreated[id]; !ok {
			entryCreated[id] = e.CreatedAt
		}
	}

	var out []IdempotencyKeyInfo
	add := func(ns, key, mapsTo string, createdAt time.Time) {
		if namespace != "" && ns != namespace {
			return
		}
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return
		}
		info := IdempotencyKeyInfo{Namespace: ns, Key: key, MapsTo: mapsTo}
		if l.idempotencyTTL > 0 && !createdAt.IsZero() {
			exp := createdAt.Add(l.idempotencyTTL)
			info.ExpiresAt = &exp
		}
		out = append(out, info)
	}

	for key, e := range l.byKey {
		add("entries", key, e.ID, e.CreatedAt)
	}
	for key, ids := range l.transferKeys {
		var created time.Time
		for _, id := range ids {
			if t, ok := entryCreated[id]; ok {
				created = t
				break
			}
		}
		add("transfers", key, strings.Join(ids, ","), created)
	}
	for key, holdID := range l.holdKeys {
		var created time.Time
		if h, ok := l.holds[holdID]; ok {
			created = h.CreatedAt
		}
		add("holds", key, holdID, created)
	}
	for key, r := range l.captureKeys {
		add("captures", key, r.CaptureID, r.Entry.CreatedAt)
	}
	for key, r := range l.partialCaptureKeys {
		add("partial-captures", key, r.CaptureID, r.Entry.CreatedAt)
	}
	for key, depositID := range l.termDepositKeys {
		var created time.Time
		if d, ok := l.termDeposits[depositID]; ok {
			created = d.OpenedAt
		}
		add("term-deposits", key, depositID, created)
	}
	for key, payoutID := range l.payoutKeys {
		var created time.Time
		if p, ok := l.payouts[payoutID]; ok {
			created = p.CreatedAt
		}
		add("payouts", key, payoutID, created)
	}
	for key, r := range l.sweepKeys {
		add("sweeps", key, r.sweepID, r.createdAt)
	}
	for key, r := range l.batchKeys {
		add("batches", key, r.batchID, r.createdAt)
	}
	for key, r := range l.batchTransferKeys {
		add("transfer-batches", key, r.batchID, r.createdAt)
	}
	for key, r := range l.allocKeys {
		add("allocations", key, r.ruleID+"/"+r.month, r.createdAt)
	}
	for key, r := range l.nettingKeys {
		add("netting", key, r.sessionID, r.createdAt)
	}
	for key, r := range l.installmentKeys {
		add("installments", key, r.planID, r.createdAt)
	}
	for key, r := range l.clearingKeys {
		add("clearing", key, r.batchID, r.createdAt)
	}
	for key, mergeID := range l.mergeKeys {
		var created time.Time
		if m, ok := l.merges[mergeID]; ok {
			created = m.createdAt
		}
		add("merges", key, mergeID, created)
	}
	for key, splitID := range l.splitKeys {
		var created time.Time
		if s, ok := l.splits[splitID]; ok {
			created = s.createdAt
		}
		add("splits", key, splitID, created)
	}
	for key, scheduleID := range l.scheduleKeys {
		var created time.Time
		if s, ok := l.schedules[scheduleID]; ok {
			created = s.CreatedAt
		}
		add("schedules", key, scheduleID, created)
	}
	for key, disputeID := range l.disputeKeys {
		var created time.Time
		if d, ok := l.disputes[disputeID]; ok {
			created = d.CreatedAt
		}
		add("disputes", key, disputeID, created)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
