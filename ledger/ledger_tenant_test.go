package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestValidateTenantID(t *testing.T) {
	valid := []string{"acme", "acme-corp", "acme_corp", "acme.corp", "ACME123", "a", "tenant-1.prod"}
	for _, id := range valid {
		if err := ValidateTenantID(id); err != nil {
			t.Errorf("ValidateTenantID(%q) = %v, want nil", id, err)
		}
	}
	invalid := map[string]string{
		"":          "empty",
		"has space": "space",
		"has/slash": "slash",
		"has@at":    "at",
		"has:colon": "colon",
		"café":      "non-ascii",
	}
	for id, why := range invalid {
		if err := ValidateTenantID(id); err == nil {
			t.Errorf("ValidateTenantID(%q) (%s) = nil, want error", id, why)
		}
	}
	// 64 chars is the cap; 65 is too long.
	long65 := ""
	for i := 0; i < 65; i++ {
		long65 += "a"
	}
	if err := ValidateTenantID(long65); err == nil {
		t.Error("ValidateTenantID(65 chars) = nil, want error")
	}
	id64 := long65[:64]
	if err := ValidateTenantID(id64); err != nil {
		t.Errorf("ValidateTenantID(64 chars) = %v, want nil", err)
	}
}

// mustTenantPost is a test helper: posts one entry, failing on error or
// unexpected duplicate.
func mustTenantPost(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", e.ID, dup, err)
	}
}

func TestTenantStoreIsolation(t *testing.T) {
	ts := NewTenantStore()
	a := ts.For("acme")
	b := ts.For("globex")

	// Same account IDs, same idempotency keys, same entry IDs — the
	// tenants must not see each other at all.
	mustTenantPost(t, a, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1000, Currency: "USD", IdempotencyKey: "order-1"})
	mustTenantPost(t, b, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 5000, Currency: "USD", IdempotencyKey: "order-1"})

	if got := a.BalanceIn("cash", "USD"); got != 1000 {
		t.Errorf("tenant acme cash = %d, want 1000", got)
	}
	if got := b.BalanceIn("cash", "USD"); got != 5000 {
		t.Errorf("tenant globex cash = %d, want 5000", got)
	}
	// Idempotency namespaces are per tenant: the same key books once in
	// each tenant, not once globally.
	if got := len(a.Entries()); got != 1 {
		t.Errorf("tenant acme entries = %d, want 1", got)
	}
	if got := len(b.Entries()); got != 1 {
		t.Errorf("tenant globex entries = %d, want 1", got)
	}
	// Journal versions and audit chains advance independently.
	if a.Version() != 1 || b.Version() != 1 {
		t.Errorf("versions = %d/%d, want 1/1", a.Version(), b.Version())
	}
	mustTenantPost(t, a, JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100, Currency: "USD"})
	if a.Version() != 2 || b.Version() != 1 {
		t.Errorf("versions after acme post = %d/%d, want 2/1", a.Version(), b.Version())
	}
	if err := a.VerifyChain(); err != nil {
		t.Errorf("acme VerifyChain: %v", err)
	}
	if err := b.VerifyChain(); err != nil {
		t.Errorf("globex VerifyChain: %v", err)
	}
}

func TestTenantStoreFreezeIsolation(t *testing.T) {
	ts := NewTenantStore()
	a := ts.For("acme")
	b := ts.For("globex")

	a.Freeze("cash")
	// The freeze stops acme's cash...
	if _, _, err := a.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != ErrAccountFrozen {
		t.Errorf("acme Post on frozen = %v, want ErrAccountFrozen", err)
	}
	// ...but globex's cash is untouched.
	mustTenantPost(t, b, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100})
	if got := b.Balance("cash"); got != 100 {
		t.Errorf("globex cash = %d, want 100", got)
	}
	if !a.IsFrozen("cash") || b.IsFrozen("cash") {
		t.Error("freeze leaked across tenants")
	}
}

func TestTenantStoreProvisioning(t *testing.T) {
	ts := NewTenantStore(WithIdempotencyTTL(0))
	if got := ts.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
	a1 := ts.For("acme")
	a2 := ts.For("acme")
	if a1 != a2 {
		t.Fatal("For returned different instances for the same tenant")
	}
	if got := ts.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	ts.For("globex")
	ts.For("acme-2")
	ids := ts.IDs()
	want := []TenantID{"acme", "acme-2", "globex"}
	if len(ids) != len(want) {
		t.Fatalf("IDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("IDs = %v, want %v", ids, want)
		}
	}
	// Concurrent provisioning of the same tenant yields one ledger.
	var wg sync.WaitGroup
	got := make([]*Ledger, 16)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = ts.For("race-tenant")
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatal("concurrent For returned different instances")
		}
	}
}

func TestTenantStoreAdopt(t *testing.T) {
	ts := NewTenantStore()
	l := New()
	mustTenantPost(t, l, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 777})
	ts.Adopt(DefaultTenantID, l)
	if ts.For(DefaultTenantID) != l {
		t.Fatal("Adopt: For(default) did not return the adopted ledger")
	}
	if got := ts.For(DefaultTenantID).Balance("cash"); got != 777 {
		t.Errorf("adopted ledger balance = %d, want 777", got)
	}
	if got := ts.Count(); got != 1 {
		t.Errorf("Count = %d, want 1", got)
	}
}

func TestTenantSnapshotRestore(t *testing.T) {
	ts := NewTenantStore()
	a := ts.For("acme")
	b := ts.For("globex")
	mustTenantPost(t, a, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1000, Currency: "USD"})
	mustTenantPost(t, b, JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 2000, Currency: "USD"})
	a.Freeze("frozen-acct")

	snap, err := ts.Snapshot("acme")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Mutate after the snapshot...
	mustTenantPost(t, a, JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 500, Currency: "USD"})
	// ...then restore: acme goes back, globex is untouched.
	if err := ts.Restore("acme", snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := ts.For("acme")
	if got := restored.BalanceIn("cash", "USD"); got != 1000 {
		t.Errorf("restored acme cash = %d, want 1000", got)
	}
	if !restored.IsFrozen("frozen-acct") {
		t.Error("restored acme lost its freeze")
	}
	if got := ts.For("globex").BalanceIn("cash", "USD"); got != 2000 {
		t.Errorf("globex cash after acme restore = %d, want 2000", got)
	}
	// The restored ledger still belongs to its tenant.
	if _, _, err := restored.Post(JournalEntry{ID: "e3", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 10}); err != nil {
		t.Fatalf("post-restore Post: %v", err)
	}
	if got := restored.Balance("cash"); got != 1010 {
		t.Errorf("restored acme cash after post = %d, want 1010", got)
	}
}

func TestTenantSnapshotRestoreCorrupt(t *testing.T) {
	ts := NewTenantStore()
	if err := ts.Restore("acme", []byte("not a snapshot")); err == nil {
		t.Fatal("Restore(corrupt) = nil, want error")
	}
}

// TestTenantAuditAttribution checks that events from different tenants
// land in the one shared audit log stamped with their tenant, while the
// default tenant's events carry no tenant field (backward compatible).
func TestTenantAuditAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	ts := NewTenantStore(WithAuditLog(al))
	ts.For("acme").Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100})
	ts.For(DefaultTenantID).Post(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100})

	// Drain: closing flushes the queue; then read the day file.
	al.Close()
	var tenants []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// Read every audit jsonl file and collect tenant fields of post ops.
	type auditLine struct {
		Op     string `json:"op"`
		Tenant string `json:"tenant"`
	}
	for _, de := range entries {
		name := de.Name()
		if len(name) < 6 || name[:6] != "audit-" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, line := range splitLines(raw) {
			var aln auditLine
			if err := json.Unmarshal(line, &aln); err != nil {
				continue
			}
			if aln.Op == "post" {
				tenants = append(tenants, aln.Tenant)
			}
		}
	}
	if len(tenants) != 2 {
		t.Fatalf("post events = %d, want 2", len(tenants))
	}
	seen := map[string]bool{}
	for _, tn := range tenants {
		seen[tn] = true
	}
	if !seen["acme"] || !seen[""] {
		t.Errorf("tenant attribution = %v, want [acme \"\"]", tenants)
	}
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}
