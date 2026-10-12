package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Multi-tenant ledger isolation (LG-60).
//
// A TenantStore is a registry of tenant-scoped ledgers: every tenant gets
// its own *Ledger, so accounts, journal entries, idempotency keys, the
// audit hash chain, freezes, limits, holds, and every other piece of
// ledger state are isolated by construction — there is no shared map to
// leak across tenants. A reference that exists only in another tenant
// (an account ID, an entry ID, an idempotency key) simply does not exist
// in this tenant's ledger, so cross-tenant references behave as
// not-found, never as a leak.
//
// Tenants are provisioned lazily: the first request that names a tenant
// creates its ledger on the spot, built with the same Option set the
// store was constructed with (so TTLs, fee policies, review thresholds,
// and the shared audit-log/outbox sinks behave identically for every
// tenant). Process-level sinks stay shared on purpose: the structured
// audit log and the domain-event outbox are single compliance streams,
// and every event they carry is stamped with its tenant (see
// Ledger.tenantID, AuditEvent.Tenant, OutboxEvent.Tenant), so one
// tamper-evident log still covers the whole process.
//
// Snapshots and restores are per tenant (see Snapshot/Restore): a
// tenant's disaster-recovery snapshot round-trips independently of every
// other tenant's. Reconcile runs against one tenant's ledger, so its
// report is per tenant too.
//
// The store is safe for concurrent use.

// TenantID identifies a tenant namespace. The zero value ("") is the
// default tenant: requests that name no tenant (no /tenants/{id} path
// prefix and no x-tenant header) resolve to it, which keeps a
// single-tenant deployment working exactly as before.
type TenantID string

// DefaultTenantID is the tenant every request resolves to when it carries
// no tenant routing information. Backward compatibility lives here: a
// deployment that never names a tenant only ever touches this one.
const DefaultTenantID TenantID = ""

// Tenant ID validation errors, returned by ValidateTenantID.
var (
	ErrTenantIDEmpty       = errors.New("ledger: tenant ID must not be empty")
	ErrTenantIDTooLong     = errors.New("ledger: tenant ID must be at most 64 characters")
	ErrTenantIDInvalidChar = errors.New("ledger: tenant ID may only contain letters, digits, '.', '_', and '-'")
)

// maxTenantIDLen caps a tenant ID at 64 characters: long enough for a
// merchant slug plus an environment suffix, short enough to keep URLs,
// log lines, and file names readable.
const maxTenantIDLen = 64

// ValidateTenantID rejects empty, overlong, or oddly-punctuated tenant
// IDs. The charset deliberately excludes '/' (it would collide with the
// /tenants/{id} path prefix) and anything that needs URL-escaping, so a
// validated ID round-trips through URLs, headers, and JSON unchanged.
func ValidateTenantID(id string) error {
	if id == "" {
		return ErrTenantIDEmpty
	}
	if len(id) > maxTenantIDLen {
		return ErrTenantIDTooLong
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("%w: %q", ErrTenantIDInvalidChar, r)
	}
	return nil
}

// TenantStore is the process-wide registry of tenant ledgers. See the
// package doc above for the isolation model.
type TenantStore struct {
	mu      sync.RWMutex
	tenants map[TenantID]*Ledger
	// opts is the Option set every lazily-provisioned tenant ledger is
	// built with: the same configuration the default ledger started
	// with, so tenants behave identically unless configured otherwise
	// per tenant afterwards. Shared sinks (audit log, outbox) are
	// captured here once and reused by every tenant.
	opts []Option
}

// NewTenantStore returns an empty tenant registry. Tenant ledgers created
// lazily through For are built with New(opts...); pass the same options
// the default ledger was built with so every tenant starts from the same
// configuration.
func NewTenantStore(opts ...Option) *TenantStore {
	return &TenantStore{
		tenants: make(map[TenantID]*Ledger),
		opts:    opts,
	}
}

// Adopt registers an already-built ledger as a tenant's ledger, stamping
// it with the tenant ID its audit and outbox events will carry. It is how
// the default (pre-existing, single-tenant) ledger joins the store: the
// ledger keeps its identity and its history, and starts resolving as the
// default tenant. Adopting over an existing tenant replaces it — the
// caller owns that decision (Restore uses it deliberately).
func (ts *TenantStore) Adopt(id TenantID, l *Ledger) {
	if l == nil {
		panic("ledger: TenantStore.Adopt called with nil ledger")
	}
	if id != DefaultTenantID {
		if err := ValidateTenantID(string(id)); err != nil {
			panic(fmt.Sprintf("ledger: TenantStore.Adopt: %v", err))
		}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	l.tenantID = id
	ts.tenants[id] = l
}

// For returns the tenant's ledger, provisioning it lazily on first use:
// the first call for an ID builds a fresh ledger with the store's option
// set and registers it, so every later call (and every concurrent caller)
// gets the same instance. An invalid tenant ID panics — fail-fast on a
// programmer error, mirroring the option constructors; the HTTP layer
// validates IDs and answers 400 before For is ever reached.
func (ts *TenantStore) For(id TenantID) *Ledger {
	if id != DefaultTenantID {
		if err := ValidateTenantID(string(id)); err != nil {
			panic(fmt.Sprintf("ledger: TenantStore.For: %v", err))
		}
	}
	ts.mu.RLock()
	l, ok := ts.tenants[id]
	ts.mu.RUnlock()
	if ok {
		return l
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if l, ok := ts.tenants[id]; ok {
		return l
	}
	l = New(ts.opts...)
	l.tenantID = id
	ts.tenants[id] = l
	return l
}

// IDs returns every provisioned tenant ID, sorted, including the default
// tenant when it has been adopted or provisioned. Read-only.
func (ts *TenantStore) IDs() []TenantID {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]TenantID, 0, len(ts.tenants))
	for id := range ts.tenants {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Count returns how many tenant ledgers are provisioned. Read-only.
func (ts *TenantStore) Count() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.tenants)
}

// Ledgers returns a copy of the tenant registry, for process-wide
// aggregation (metrics scrapes sum per-ledger counters across tenants).
// Mutating the returned map does not affect the store.
func (ts *TenantStore) Ledgers() map[TenantID]*Ledger {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make(map[TenantID]*Ledger, len(ts.tenants))
	for id, l := range ts.tenants {
		out[id] = l
	}
	return out
}

// Snapshot exports the tenant's disaster-recovery snapshot: the full
// journal, balances, chain, and operational config of that tenant alone.
// It round-trips through Restore independently of every other tenant.
func (ts *TenantStore) Snapshot(id TenantID) ([]byte, error) {
	var buf bytes.Buffer
	if err := ts.For(id).ExportSnapshot(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Restore replaces the tenant's ledger with the given snapshot: the
// snapshot is imported, the store's option set is re-applied (so the
// restored ledger keeps the shared audit-log/outbox sinks and the
// configured TTLs and policies), and the result is adopted as the
// tenant's ledger. Other tenants are untouched.
func (ts *TenantStore) Restore(id TenantID, snap []byte) error {
	l, err := ImportSnapshot(bytes.NewReader(snap))
	if err != nil {
		return err
	}
	for _, opt := range ts.opts {
		opt(l)
	}
	ts.Adopt(id, l)
	return nil
}
