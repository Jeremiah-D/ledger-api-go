package ledger

import (
	"strings"
	"testing"
	"time"
)

// LG-152: read-only cross-namespace idempotency key query.

func postKeyedEntry(t *testing.T, l *Ledger, id, key string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  "cash",
		CreditAccount: "revenue",
		AmountCents:   10000,
		Currency:      "USD",
		IdempotencyKey: key,
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
}

func TestListIdempotencyKeysNamespaces(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	postKeyedEntry(t, l, "e1", "key-entry-1")
	postKeyedEntry(t, l, "e2", "other-entry")

	if _, err := l.PostTransfer(Transfer{
		ID:             "t1",
		From:           "cash",
		To:             "rent",
		AmountCents:    500,
		Currency:       "USD",
		IdempotencyKey: "key-transfer-1",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, _, err := l.Hold(Hold{
		ID:             "h1",
		Account:        "cash",
		AmountCents:    300,
		Currency:       "USD",
		ExpiresAt:      time.Now().UTC().Add(time.Hour),
		IdempotencyKey: "key-hold-1",
	}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.PostSweep(Sweep{
		ID:             "s1",
		From:           []AccountID{"cash"},
		To:             "vault",
		IdempotencyKey: "key-sweep-1",
	}); err != nil {
		t.Fatalf("PostSweep: %v", err)
	}

	infos := l.ListIdempotencyKeys("", "", 0)
	byKey := map[string]IdempotencyKeyInfo{}
	for _, in := range infos {
		byKey[in.Key] = in
	}
	for key, want := range map[string]struct{ ns, maps string }{
		"key-entry-1":    {"entries", "e1"},
		"key-transfer-1": {"transfers", "t1"},
		"key-hold-1":     {"holds", "h1"},
		"key-sweep-1":    {"sweeps", "s1"},
	} {
		got, ok := byKey[key]
		if !ok {
			t.Fatalf("key %q missing from listing", key)
		}
		if got.Namespace != want.ns {
			t.Fatalf("key %q namespace = %s, want %s", key, got.Namespace, want.ns)
		}
		if got.MapsTo != want.maps {
			t.Fatalf("key %q maps_to = %s, want %s", key, got.MapsTo, want.maps)
		}
		if got.ExpiresAt == nil {
			t.Fatalf("key %q expires_at = nil, want TTL expiry", key)
		}
	}

	// Sorted by (namespace, key).
	for i := 1; i < len(infos); i++ {
		a, b := infos[i-1], infos[i]
		if a.Namespace > b.Namespace || (a.Namespace == b.Namespace && a.Key > b.Key) {
			t.Fatalf("unsorted: %+v before %+v", a, b)
		}
	}
}

func TestListIdempotencyKeysFilters(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	postKeyedEntry(t, l, "e1", "alpha-1")
	postKeyedEntry(t, l, "e2", "alpha-2")
	postKeyedEntry(t, l, "e3", "beta-1")

	if got := l.ListIdempotencyKeys("alpha", "", 0); len(got) != 2 {
		t.Fatalf("prefix filter = %d, want 2", len(got))
	}
	if got := l.ListIdempotencyKeys("", "entries", 0); len(got) != 3 {
		t.Fatalf("namespace filter = %d, want 3", len(got))
	}
	if got := l.ListIdempotencyKeys("", "transfers", 0); len(got) != 0 {
		t.Fatalf("empty namespace = %d, want 0", len(got))
	}
	if got := l.ListIdempotencyKeys("", "nope", 0); len(got) != 0 {
		t.Fatalf("unknown namespace = %d, want 0", len(got))
	}
	if got := l.ListIdempotencyKeys("", "", 2); len(got) != 2 {
		t.Fatalf("limit = %d, want 2", len(got))
	}
}

func TestListIdempotencyKeysNoTTL(t *testing.T) {
	l := New() // no TTL configured
	postKeyedEntry(t, l, "e1", "key-1")
	infos := l.ListIdempotencyKeys("", "", 0)
	if len(infos) != 1 {
		t.Fatalf("infos = %d, want 1", len(infos))
	}
	if infos[0].ExpiresAt != nil {
		t.Fatalf("expires_at = %v, want nil without TTL", infos[0].ExpiresAt)
	}
}

func TestListIdempotencyKeysExposesNoPayloads(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	postKeyedEntry(t, l, "e1", "key-1")
	for _, in := range l.ListIdempotencyKeys("", "", 0) {
		if strings.Contains(in.MapsTo, "100") || strings.Contains(in.MapsTo, "cash") {
			t.Fatalf("maps_to leaks payload data: %q", in.MapsTo)
		}
	}
}

func TestIdempotencyNamespaces(t *testing.T) {
	names := IdempotencyNamespaces()
	if len(names) == 0 {
		t.Fatalf("no namespaces")
	}
	if !IsIdempotencyNamespace("entries") || IsIdempotencyNamespace("nope") {
		t.Fatalf("IsIdempotencyNamespace wrong")
	}
	// Mutating the returned slice must not affect the registry.
	names[0] = "corrupt"
	if !IsIdempotencyNamespace("entries") {
		t.Fatalf("namespaces slice not defensively copied")
	}
}
