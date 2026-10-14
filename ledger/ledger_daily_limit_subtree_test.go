package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func postSubtreeOutflow(t *testing.T, l *Ledger, id string, credit AccountID, amount int64, currency string, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  "merchant",
		CreditAccount: credit,
		AmountCents:   amount,
		Currency:      currency,
		CreatedAt:     at,
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
}

func mustLink(t *testing.T, l *Ledger, child, parent AccountID) {
	t.Helper()
	if err := l.SetParent(child, parent); err != nil {
		t.Fatalf("SetParent(%q, %q): %v", child, parent, err)
	}
}

// A parent's daily limit constrains its whole subtree: the child's
// outflow counts against the parent's budget, and so does the parent's
// own outflow.
func TestSubtreeLimitConstrainsChildPost(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")
	fundAccount(t, l, "parent", 100000, "")

	postSubtreeOutflow(t, l, "e1", "child", 600, "USD", now)
	// 600 + 500 = 1100 > 1000: the child's second posting breaches the
	// parent's subtree budget.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 500, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("child post over parent subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	// The parent's own outflow shares the same budget: 600 + 500 > 1000.
	if _, _, err := l.Post(JournalEntry{ID: "e3", DebitAccount: "merchant", CreditAccount: "parent", AmountCents: 500, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("parent post over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	// 600 + 400 = 1000 exactly: allowed.
	postSubtreeOutflow(t, l, "e4", "child", 400, "USD", now)
	if got := l.BalanceIn("child", "USD"); got != 99000 {
		t.Fatalf("child balance=%d, want 99000", got)
	}
}

// The constraint spans generations: a grandchild's outflow counts
// against the grandparent's limit.
func TestSubtreeLimitSpansGenerations(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "grandparent", "USD", 1000)
	mustLink(t, l, "parent", "grandparent")
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")

	postSubtreeOutflow(t, l, "e1", "child", 700, "USD", now)
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 400, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("grandchild post over grandparent limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// Limits are per currency: a USD subtree limit does not touch EUR
// outflow.
func TestSubtreeLimitIsPerCurrency(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "EUR")

	postSubtreeOutflow(t, l, "e1", "child", 5000, "EUR", now)
	postSubtreeOutflow(t, l, "e2", "child", 600, "USD", now)
	if _, _, err := l.Post(JournalEntry{ID: "e3", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 500, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("USD post over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// Sibling subtrees share the parent's budget.
func TestSubtreeLimitSiblingsShareBudget(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child-a", "parent")
	mustLink(t, l, "child-b", "parent")
	fundAccount(t, l, "child-a", 100000, "")
	fundAccount(t, l, "child-b", 100000, "")

	postSubtreeOutflow(t, l, "e1", "child-a", 600, "USD", now)
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "merchant", CreditAccount: "child-b", AmountCents: 500, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("sibling post over shared subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// The child's own limit and the parent's subtree limit apply
// independently: the tighter one wins per posting.
func TestSubtreeLimitOwnAndAncestorBothApply(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "child", "USD", 500)
	mustSetDailyLimit(t, l, "parent", "USD", 2000)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")

	// 600 breaches the child's own 500 limit (subtree used would be
	// 600 < 2000, so only the own limit fires).
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 600, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("post over own limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	postSubtreeOutflow(t, l, "e2", "child", 400, "USD", now)
}

// PostTransfer's payer counts against the parent's subtree budget.
func TestSubtreeLimitAppliesToTransfer(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "payer", "parent")
	seedBalance(t, l, "payer", 100000)

	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "merchant", AmountCents: 700, CreatedAt: now}); err != nil {
		t.Fatalf("transfer 700: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "payer", To: "merchant", AmountCents: 400, CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("transfer over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// A backdated posting counts toward its own UTC day's subtree budget,
// not today's.
func TestSubtreeLimitBackdatedUsesEntryDay(t *testing.T) {
	l := New()
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")

	postSubtreeOutflow(t, l, "e1", "child", 900, "USD", yesterday)
	// Today: 0 + 200 <= 1000 — yesterday's 900 does not count.
	postSubtreeOutflow(t, l, "e2", "child", 200, "USD", now)
	// Config validation also keys on today: 200 used, so a limit of 100
	// is rejected but 250 is fine.
	if err := l.SetDailyLimit("parent", "USD", 100); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("lowering limit below today's subtree usage: err=%v, want ErrDailyLimitExceeded", err)
	}
	mustSetDailyLimit(t, l, "parent", "USD", 250)
}

// Linking a subtree under a parent whose limit the subtree's
// already-booked outflow would breach is rejected, and the previous link
// is restored.
func TestSetParentRejectsBreachingLink(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 500)
	fundAccount(t, l, "child", 100000, "")
	postSubtreeOutflow(t, l, "e1", "child", 800, "USD", now)

	if err := l.SetParent("child", "parent"); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("SetParent into breached limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	if p, ok := l.Parent("child"); ok || p != "" {
		t.Fatalf("Parent(child)=%q,%v after rejected link, want no parent", p, ok)
	}
	// A limit that admits today's usage links fine.
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child", "parent")
	// And the subtree budget now constrains the child: 800 + 300 > 1000.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 300, Currency: "USD", CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("post after link over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// Linking under a grandparent whose limit would breach also fails, and a
// breaching deeper link rolls back without disturbing the existing one.
func TestSetParentRejectsBreachingGrandparentLink(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "grandparent", "USD", 500)
	mustLink(t, l, "parent", "grandparent")
	fundAccount(t, l, "child", 100000, "")
	postSubtreeOutflow(t, l, "e1", "child", 800, "USD", now)

	if err := l.SetParent("child", "parent"); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("SetParent under breaching grandparent: err=%v, want ErrDailyLimitExceeded", err)
	}
	if p, ok := l.Parent("child"); ok || p != "" {
		t.Fatalf("Parent(child)=%q,%v after rejected link, want no parent", p, ok)
	}
}

// Lowering a limit below the subtree's already-booked outflow for today
// is rejected; raising it, or setting it equal, is fine.
func TestSetDailyLimitRejectsBelowSubtreeUsed(t *testing.T) {
	l := New()
	now := time.Now()
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")
	postSubtreeOutflow(t, l, "e1", "child", 800, "USD", now)

	if err := l.SetDailyLimit("parent", "USD", 799); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("SetDailyLimit below subtree used: err=%v, want ErrDailyLimitExceeded", err)
	}
	// Equal is allowed: the subtree used exactly the budget.
	mustSetDailyLimit(t, l, "parent", "USD", 800)
	// Raising is allowed.
	mustSetDailyLimit(t, l, "parent", "USD", 5000)
	// A fresh account with no usage accepts any non-negative limit.
	mustSetDailyLimit(t, l, "fresh", "USD", 0)
}

// Clearing a parent link only shrinks subtrees: it never needs the
// usage check.
func TestSetParentClearNeedsNoValidation(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 100)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")
	postSubtreeOutflow(t, l, "e1", "child", 50, "USD", now)
	if err := l.SetParent("child", ""); err != nil {
		t.Fatalf("clearing parent: %v", err)
	}
	// After the unlink the parent's limit no longer constrains the child.
	postSubtreeOutflow(t, l, "e2", "child", 5000, "USD", now)
}

// A batch whose planned outflow would breach an ancestor's subtree
// budget is rejected atomically: nothing books.
func TestSubtreeLimitBatchSimulation(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")

	postSubtreeOutflow(t, l, "seed", "child", 700, "USD", now)
	b := Batch{ID: "b1", Entries: []JournalEntry{
		{ID: "b1e1", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 200, Currency: "USD", CreatedAt: now},
		{ID: "b1e2", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 200, Currency: "USD", CreatedAt: now},
	}}
	if _, err := l.PostBatch(b); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("batch over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	if l.Version() != 2 { // seed + funding only
		t.Fatalf("version=%d after rejected batch, want 2", l.Version())
	}
	// A batch that fits (700 + 200 <= 1000) commits.
	b2 := Batch{ID: "b2", Entries: []JournalEntry{
		{ID: "b2e1", DebitAccount: "merchant", CreditAccount: "child", AmountCents: 200, Currency: "USD", CreatedAt: now},
	}}
	if _, err := l.PostBatch(b2); err != nil {
		t.Fatalf("batch within subtree limit: %v", err)
	}
}

// Split transfers simulate the chunk sequence against the ancestor's
// subtree budget.
func TestSubtreeLimitSplitTransfer(t *testing.T) {
	l := New(WithMaxTransferChunkCents(400))
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "payer", "parent")
	seedBalance(t, l, "payer", 100000)

	if _, err := l.PostTransferSplit(Transfer{ID: "s1", From: "payer", To: "merchant", AmountCents: 900, CreatedAt: now}); err != nil {
		t.Fatalf("split transfer 900 within subtree limit: %v", err)
	}
	if _, err := l.PostTransferSplit(Transfer{ID: "s2", From: "payer", To: "merchant", AmountCents: 900, CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("split transfer over subtree limit: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// The reconcile report's subtree_limit_utilization section carries every
// configured limit with its subtree's booked outflow for the report day.
func TestSubtreeLimitUtilizationReport(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustSetDailyLimit(t, l, "loner", "EUR", 200)
	mustLink(t, l, "child", "parent")
	fundAccount(t, l, "child", 100000, "")

	postSubtreeOutflow(t, l, "e1", "child", 250, "USD", now)
	report := l.Reconcile(now)
	byKey := map[string]SubtreeLimitUsage{}
	for _, u := range report.SubtreeLimitUtilization {
		byKey[string(u.Account)+"/"+u.Currency] = u
	}
	if len(byKey) != 2 {
		t.Fatalf("subtree_limit_utilization rows=%d, want 2", len(byKey))
	}
	parent := byKey["parent/USD"]
	if parent.LimitCents != 1000 || parent.SubtreeUsedCents != 250 || parent.SubtreeAccounts != 2 || parent.Exceeded {
		t.Fatalf("parent/USD row=%+v, want limit=1000 used=250 accounts=2 exceeded=false", parent)
	}
	if parent.Utilization != 0.25 {
		t.Fatalf("parent/USD utilization=%v, want 0.25", parent.Utilization)
	}
	loner := byKey["loner/EUR"]
	if loner.SubtreeUsedCents != 0 || loner.SubtreeAccounts != 1 || loner.Utilization != 0 {
		t.Fatalf("loner/EUR row=%+v, want used=0 accounts=1 utilization=0", loner)
	}
}

// Concurrent postings from one subtree never breach the parent's budget
// (checked under -race).
func TestSubtreeLimitConcurrent(t *testing.T) {
	l := New()
	now := time.Now()
	mustSetDailyLimit(t, l, "parent", "USD", 1000)
	mustLink(t, l, "child-a", "parent")
	mustLink(t, l, "child-b", "parent")
	fundAccount(t, l, "child-a", 100000, "")
	fundAccount(t, l, "child-b", 100000, "")

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			credit := AccountID("child-a")
			if i%2 == 1 {
				credit = AccountID("child-b")
			}
			for j := 0; j < 25; j++ {
				id := strings.Join([]string{"cc", string(rune('a' + i)), "-", string(rune('a' + j%26)), string(rune('0' + j/26))}, "")
				_, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "merchant", CreditAccount: credit, AmountCents: 100, Currency: "USD", CreatedAt: now})
				if err == nil {
					mu.Lock()
					accepted++
					mu.Unlock()
				} else if !errors.Is(err, ErrDailyLimitExceeded) {
					t.Errorf("post: unexpected err %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	if accepted != 10 {
		t.Fatalf("accepted=%d postings, want exactly 10 (1000 budget / 100 each)", accepted)
	}
	used := l.subtreeDailyOutflowLocked("parent", "USD", utcDay(now))
	if used != 1000 {
		t.Fatalf("subtree used=%d, want 1000", used)
	}
}
