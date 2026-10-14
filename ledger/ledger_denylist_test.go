package ledger

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDenylistExactBlocksPost(t *testing.T) {
	l := New()
	l.AddDeniedAccount("sanctioned")
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "sanctioned", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("Post debit through denied account: err=%v, want ErrAccountDenied", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "sanctioned", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("Post credit through denied account: err=%v, want ErrAccountDenied", err)
	}
	// Rejected posts record nothing: no journal row, no version bump.
	if l.Version() != 0 {
		t.Fatalf("version=%d after denied posts, want 0", l.Version())
	}
	if _, ok := l.entries["e1"]; ok {
		t.Fatal("denied entry e1 was journaled")
	}
	// Clean accounts keep posting.
	if _, _, err := l.Post(JournalEntry{ID: "e3", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatalf("Post on clean accounts: %v", err)
	}
}

func TestDenylistPrefixBlocksPost(t *testing.T) {
	l := New()
	if _, err := l.AddDeniedPrefix("ofac-sdn-"); err != nil {
		t.Fatalf("AddDeniedPrefix: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "ofac-sdn-123", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("Post through prefixed account: err=%v, want ErrAccountDenied", err)
	}
	// A near-miss that does not start with the prefix is unaffected.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "xofac-sdn-123", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatalf("Post on non-matching account: %v", err)
	}
	// Empty prefix would deny the whole ledger: rejected.
	if _, err := l.AddDeniedPrefix(""); !errors.Is(err, ErrInvalidDenylistEntry) {
		t.Fatalf("AddDeniedPrefix(\"\"): err=%v, want ErrInvalidDenylistEntry", err)
	}
}

func TestDenylistScreenRunsBeforeIdempotencyReplay(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("setup Post: %v", err)
	}
	// The debit account lands on the denylist after the original posting.
	l.AddDeniedAccount("cash")
	// Replaying the original key is denied, not a duplicate replay: the
	// screen runs before the replay check, and the denied request leaves
	// no idempotency trace of its own.
	if _, dup, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, IdempotencyKey: "k1"}); !errors.Is(err, ErrAccountDenied) || dup {
		t.Fatalf("replay after denylist: dup=%v err=%v, want denied", dup, err)
	}
	// A fresh key on the denied account is denied and plants no key.
	if _, _, err := l.Post(JournalEntry{ID: "e3", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, IdempotencyKey: "k2"}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("fresh key on denied account: err=%v, want ErrAccountDenied", err)
	}
	if _, ok := l.GetByIdempotencyKey("k2"); ok {
		t.Fatal("denied request planted an idempotency key")
	}
	// The original entry is still replayable by key lookup (reads are
	// unaffected by the denylist).
	if _, ok := l.GetByIdempotencyKey("k1"); !ok {
		t.Fatal("original idempotency key lost")
	}
}

func TestDenylistBlocksTransfer(t *testing.T) {
	l := New()
	l.AddDeniedAccount("bad-payer")
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "bad-payer", To: "merchant", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("transfer from denied payer: err=%v, want ErrAccountDenied", err)
	}
	l.AddDeniedAccount("bad-payee")
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "customer", To: "bad-payee", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("transfer to denied payee: err=%v, want ErrAccountDenied", err)
	}
	// Screening runs before the idempotency replay check for transfers too.
	if _, err := l.PostTransfer(Transfer{ID: "t3", From: "customer", To: "merchant", AmountCents: 100, IdempotencyKey: "tk1"}); err != nil {
		t.Fatalf("setup transfer: %v", err)
	}
	l.AddDeniedAccount("customer")
	if _, err := l.PostTransfer(Transfer{ID: "t4", From: "customer", To: "merchant", AmountCents: 100, IdempotencyKey: "tk1"}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("transfer replay after denylist: err=%v, want ErrAccountDenied", err)
	}
	if l.Version() != 1 {
		t.Fatalf("version=%d, want 1 (only the setup transfer booked)", l.Version())
	}
}

func TestDenylistBlocksCapture(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("setup hold: %v", err)
	}
	// Denying the hold's account blocks the capture (payer leg).
	l.AddDeniedAccount("card")
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 1000}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("capture from denied hold account: err=%v, want ErrAccountDenied", err)
	}
	// Denying the capture target blocks it too (payee leg).
	l2 := New()
	fundAccount(t, l2, "card", 10000, "")
	if _, _, err := l2.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("setup hold: %v", err)
	}
	l2.AddDeniedAccount("merchant")
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 1000}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("capture to denied target: err=%v, want ErrAccountDenied", err)
	}
	// The hold is untouched by the denied capture: still active.
	h := l2.holds["h1"]
	if h.Status != HoldStatusActive {
		t.Fatalf("hold status=%q after denied capture, want active", h.Status)
	}
	// Screening runs before the capture replay check: settle a capture,
	// deny the target, then replay the capture key — denied, not a
	// duplicate receipt.
	l3 := New()
	fundAccount(t, l3, "card", 10000, "")
	if _, _, err := l3.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("setup hold: %v", err)
	}
	if _, err := l3.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 1000, IdempotencyKey: "ck1"}); err != nil {
		t.Fatalf("setup capture: %v", err)
	}
	l3.AddDeniedAccount("merchant")
	if _, err := l3.Capture(Capture{ID: "c2", HoldID: "h1", To: "merchant", AmountCents: 500, IdempotencyKey: "ck1"}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("capture replay after denylist: err=%v, want ErrAccountDenied", err)
	}
}

func TestDenylistManagement(t *testing.T) {
	l := New()
	// Exact add is idempotent and keeps the original row.
	first := l.AddDeniedAccount("a1")
	second := l.AddDeniedAccount("a1")
	if !first.AddedAt.Equal(second.AddedAt) {
		t.Fatal("re-adding an account changed its row")
	}
	if _, err := l.AddDeniedPrefix("p-"); err != nil {
		t.Fatalf("AddDeniedPrefix: %v", err)
	}
	got := l.Denylist()
	if len(got) != 2 {
		t.Fatalf("Denylist()=%d entries, want 2", len(got))
	}
	// Exact entries sort before prefix patterns.
	if got[0].Prefix || got[0].Value != "a1" || !got[1].Prefix || got[1].Value != "p-" {
		t.Fatalf("Denylist() order=%v, want exact first then prefix", got)
	}
	// DeniedBy: exact wins over prefix; longest prefix wins.
	l.AddDeniedPrefix("p-lo")
	entry, ok := l.DeniedBy("p-long-1")
	if !ok || !entry.Prefix || entry.Value != "p-lo" {
		t.Fatalf("DeniedBy(p-long-1)=%v,%v, want longest prefix p-lo", entry, ok)
	}
	entry, ok = l.DeniedBy("a1")
	if !ok || entry.Prefix || entry.Value != "a1" {
		t.Fatalf("DeniedBy(a1)=%v,%v, want exact a1", entry, ok)
	}
	if _, ok := l.DeniedBy("clean"); ok {
		t.Fatal("DeniedBy(clean) matched")
	}
	// Removal.
	if !l.RemoveDeniedAccount("a1") {
		t.Fatal("RemoveDeniedAccount(a1)=false, want true")
	}
	if l.RemoveDeniedAccount("a1") {
		t.Fatal("second RemoveDeniedAccount(a1)=true, want false")
	}
	if !l.RemoveDeniedPrefix("p-") {
		t.Fatal("RemoveDeniedPrefix(p-)=false, want true")
	}
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a1", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatalf("Post after removal: %v", err)
	}
}

func TestDenylistImport(t *testing.T) {
	l := New()
	added, skipped, err := l.ImportDenylist([]DenylistSpec{
		{Account: "a1"},
		{Prefix: "sdn-"},
		{Account: "a1"}, // duplicate: skipped
		{Prefix: "sdn-"}, // duplicate: skipped
	})
	if err != nil || added != 2 || skipped != 2 {
		t.Fatalf("ImportDenylist: added=%d skipped=%d err=%v, want 2/2/nil", added, skipped, err)
	}
	// A malformed spec aborts the whole import atomically: nothing is
	// installed.
	if _, _, err := l.ImportDenylist([]DenylistSpec{{Account: "a2"}, {}}); !errors.Is(err, ErrInvalidDenylistEntry) {
		t.Fatalf("bad import: err=%v, want ErrInvalidDenylistEntry", err)
	}
	if _, ok := l.DeniedBy("a2"); ok {
		t.Fatal("atomic import installed a2 despite the bad spec")
	}
	// Both kinds of rows screen.
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a1", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("imported exact row did not screen: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "sdn-x", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("imported prefix row did not screen: %v", err)
	}
}

func TestDenylistRejectionAudited(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	l.AddDeniedAccount("bad")
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "bad", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("Post: err=%v, want ErrAccountDenied", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	rejected := byOp["denylist_rejected"]
	if len(rejected) != 1 {
		t.Fatalf("denylist_rejected events = %d, want 1", len(rejected))
	}
	ev := rejected[0]
	if ev.Actor != "Post" {
		t.Fatalf("actor=%q, want Post", ev.Actor)
	}
	found := false
	for _, a := range ev.Accounts {
		if a == "bad" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit accounts=%v, want to include the denied account", ev.Accounts)
	}
	if ev.Details["denied_account"] != "bad" || ev.Details["matched_entry"] != "bad" {
		t.Fatalf("audit details=%v, want denied_account and matched_entry", ev.Details)
	}
}

func TestDenylistSnapshotRoundTrip(t *testing.T) {
	l := New()
	l.AddDeniedAccount("bad-exact")
	if _, err := l.AddDeniedPrefix("bad-"); err != nil {
		t.Fatalf("AddDeniedPrefix: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if _, ok := restored.DeniedBy("bad-exact"); !ok {
		t.Fatal("restored ledger lost the exact denylist row")
	}
	if _, ok := restored.DeniedBy("bad-prefix-1"); !ok {
		t.Fatal("restored ledger lost the prefix denylist row")
	}
	if _, _, err := restored.Post(JournalEntry{ID: "e1", DebitAccount: "bad-exact", CreditAccount: "equity", AmountCents: 100}); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("restored ledger did not screen: %v", err)
	}
	if len(restored.Denylist()) != 2 {
		t.Fatalf("restored Denylist()=%d entries, want 2", len(restored.Denylist()))
	}
}

func TestDenylistConcurrentPosts(t *testing.T) {
	l := New()
	l.AddDeniedAccount("bad")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := strings.Join([]string{"e", string(rune('0' + i)), "-", string(rune('0' + j))}, "")
				l.Post(JournalEntry{ID: id, DebitAccount: "bad", CreditAccount: "equity", AmountCents: 1})
				l.Denylist()
			}
		}(i)
	}
	wg.Wait()
	if l.Version() != 0 {
		t.Fatalf("version=%d, want 0: every post hit the denylist", l.Version())
	}
}
