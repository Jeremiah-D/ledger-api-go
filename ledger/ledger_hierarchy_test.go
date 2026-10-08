package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// postCredits posts one entry debiting `to` and crediting "bank", so `to`
// ends with a positive balance of amount cents in the given currency. Each
// entry gets a unique ID/key from the counter.
func postCredits(t *testing.T, l *Ledger, to AccountID, amount int64, currency string, n int) {
	t.Helper()
	e := JournalEntry{
		ID:            fmt.Sprintf("cr-%d", n),
		DebitAccount:  to,
		CreditAccount: "bank",
		AmountCents:   amount,
		Currency:      currency,
		CreatedAt:     time.Date(2026, 10, 7, 12, 0, 0, n, time.UTC),
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post(%s <- %d %s): %v", to, amount, currency, err)
	}
}

func TestSetParentAssignsAndClears(t *testing.T) {
	l := New()
	if err := l.SetParent("sub-1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	p, ok := l.Parent("sub-1")
	if !ok || p != "merchant" {
		t.Fatalf("Parent(sub-1) = %q, %v; want merchant, true", p, ok)
	}
	if _, ok := l.Parent("merchant"); ok {
		t.Fatalf("Parent(merchant) reported a parent for a hierarchy root")
	}
	// Clearing removes the link.
	if err := l.SetParent("sub-1", ""); err != nil {
		t.Fatalf("SetParent clear: %v", err)
	}
	if _, ok := l.Parent("sub-1"); ok {
		t.Fatalf("Parent(sub-1) still assigned after clear")
	}
}

func TestSetParentRejectsEmptyChild(t *testing.T) {
	l := New()
	if err := l.SetParent("", "merchant"); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("SetParent(\"\", ...): got %v, want ErrEmptyAccountID", err)
	}
}

func TestSetParentRejectsSelfParent(t *testing.T) {
	l := New()
	if err := l.SetParent("merchant", "merchant"); !errors.Is(err, ErrAccountSelfParent) {
		t.Fatalf("SetParent self: got %v, want ErrAccountSelfParent", err)
	}
	// The failed assignment left no trace.
	if _, ok := l.Parent("merchant"); ok {
		t.Fatalf("self-parent attempt left a parent link behind")
	}
}

func TestSetParentRejectsDirectCycle(t *testing.T) {
	l := New()
	if err := l.SetParent("a", "b"); err != nil {
		t.Fatalf("SetParent(a -> b): %v", err)
	}
	if err := l.SetParent("b", "a"); !errors.Is(err, ErrParentCycle) {
		t.Fatalf("SetParent(b -> a): got %v, want ErrParentCycle", err)
	}
	// The rejected link was not applied; the original stands.
	p, _ := l.Parent("a")
	if p != "b" {
		t.Fatalf("Parent(a) = %q after rejected cycle, want b", p)
	}
	if _, ok := l.Parent("b"); ok {
		t.Fatalf("Parent(b) set despite cycle rejection")
	}
}

func TestSetParentRejectsIndirectCycle(t *testing.T) {
	l := New()
	// a -> b -> c -> d: linking d -> a would close the loop, as would
	// c -> a and b -> a.
	for child, parent := range map[AccountID]AccountID{"a": "b", "b": "c", "c": "d"} {
		if err := l.SetParent(child, parent); err != nil {
			t.Fatalf("SetParent(%s -> %s): %v", child, parent, err)
		}
	}
	for _, child := range []AccountID{"d", "c", "b"} {
		if err := l.SetParent(child, "a"); !errors.Is(err, ErrParentCycle) {
			t.Fatalf("SetParent(%s -> a): got %v, want ErrParentCycle", child, err)
		}
	}
	// A diamond is fine: d -> b skips a level without closing a loop...
	// (d's ancestors are c, b is d's grandparent — linking d -> b would
	// make b both ancestor and parent, i.e. a cycle through the new
	// edge: d -> b -> c -> d). It must be rejected.
	if err := l.SetParent("d", "b"); !errors.Is(err, ErrParentCycle) {
		t.Fatalf("SetParent(d -> b): got %v, want ErrParentCycle", err)
	}
	// ...but a genuinely fresh root is fine.
	if err := l.SetParent("d", "root"); err != nil {
		t.Fatalf("SetParent(d -> root): %v", err)
	}
}

func TestSetParentDoesNotBumpVersion(t *testing.T) {
	l := New()
	postCredits(t, l, "merchant", 1000, "USD", 1)
	_, v0 := l.Snapshot("merchant")
	if err := l.SetParent("sub-1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := l.SetParent("sub-1", ""); err != nil {
		t.Fatalf("SetParent clear: %v", err)
	}
	if _, v := l.Snapshot("merchant"); v != v0 {
		t.Fatalf("version moved %d -> %d on hierarchy changes; hierarchy is structural, not bookkeeping", v0, v)
	}
}

func TestRollupAggregatesSubtree(t *testing.T) {
	l := New()
	// merchant
	// ├── sub-1 (+200)
	// └── sub-2 (+300)
	//     └── sub-2a (+400)
	// merchant itself +1000.
	for child, parent := range map[AccountID]AccountID{
		"sub-1": "merchant", "sub-2": "merchant", "sub-2a": "sub-2",
	} {
		if err := l.SetParent(child, parent); err != nil {
			t.Fatalf("SetParent: %v", err)
		}
	}
	postCredits(t, l, "merchant", 1000, "USD", 1)
	postCredits(t, l, "sub-1", 200, "USD", 2)
	postCredits(t, l, "sub-2", 300, "USD", 3)
	postCredits(t, l, "sub-2a", 400, "USD", 4)

	r := l.Rollup("merchant")
	if r.Account != "merchant" {
		t.Errorf("Account = %q, want merchant", r.Account)
	}
	if r.Parent != "" {
		t.Errorf("Parent = %q, want empty (hierarchy root)", r.Parent)
	}
	if r.DescendantCount != 3 {
		t.Errorf("DescendantCount = %d, want 3", r.DescendantCount)
	}
	wantAccounts := []AccountID{"merchant", "sub-1", "sub-2", "sub-2a"}
	if fmt.Sprint(r.Accounts) != fmt.Sprint(wantAccounts) {
		t.Errorf("Accounts = %v, want %v", r.Accounts, wantAccounts)
	}
	if len(r.ByCurrency) != 1 || r.ByCurrency[0].Currency != "USD" || r.ByCurrency[0].BalanceCents != 1900 {
		t.Errorf("ByCurrency = %+v, want [{USD 1900}]", r.ByCurrency)
	}

	// A mid-tree rollup covers only its own subtree.
	r2 := l.Rollup("sub-2")
	if r2.DescendantCount != 1 {
		t.Errorf("sub-2 DescendantCount = %d, want 1", r2.DescendantCount)
	}
	if r2.Parent != "merchant" {
		t.Errorf("sub-2 Parent = %q, want merchant", r2.Parent)
	}
	if len(r2.ByCurrency) != 1 || r2.ByCurrency[0].BalanceCents != 700 {
		t.Errorf("sub-2 ByCurrency = %+v, want [{USD 700}]", r2.ByCurrency)
	}

	// A leaf rolls up to just itself.
	r3 := l.Rollup("sub-2a")
	if r3.DescendantCount != 0 || len(r3.Accounts) != 1 {
		t.Errorf("leaf rollup = %+v, want single-account rollup", r3)
	}
	if len(r3.ByCurrency) != 1 || r3.ByCurrency[0].BalanceCents != 400 {
		t.Errorf("leaf ByCurrency = %+v, want [{USD 400}]", r3.ByCurrency)
	}
}

func TestRollupAggregatesPerCurrency(t *testing.T) {
	l := New()
	if err := l.SetParent("sub-1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	postCredits(t, l, "merchant", 1000, "USD", 1)
	postCredits(t, l, "sub-1", 500, "EUR", 2)
	postCredits(t, l, "sub-1", 250, "USD", 3)

	r := l.Rollup("merchant")
	if len(r.ByCurrency) != 2 {
		t.Fatalf("ByCurrency = %+v, want 2 currency rows", r.ByCurrency)
	}
	// Sorted by currency code; currencies never summed together.
	if r.ByCurrency[0].Currency != "EUR" || r.ByCurrency[0].BalanceCents != 500 {
		t.Errorf("row 0 = %+v, want {EUR 500}", r.ByCurrency[0])
	}
	if r.ByCurrency[1].Currency != "USD" || r.ByCurrency[1].BalanceCents != 1250 {
		t.Errorf("row 1 = %+v, want {USD 1250}", r.ByCurrency[1])
	}
}

func TestRollupUnknownAccountIsEmpty(t *testing.T) {
	l := New()
	r := l.Rollup("ghost")
	if r.Account != "ghost" || r.DescendantCount != 0 {
		t.Errorf("rollup = %+v, want account=ghost with no descendants", r)
	}
	if len(r.Accounts) != 1 || r.Accounts[0] != "ghost" {
		t.Errorf("Accounts = %v, want [ghost]", r.Accounts)
	}
	if len(r.ByCurrency) != 0 {
		t.Errorf("ByCurrency = %+v, want empty", r.ByCurrency)
	}
	if r.Frozen {
		t.Errorf("Frozen = true for an unknown account")
	}
}

func TestRollupReportsOwnFrozenFlag(t *testing.T) {
	l := New()
	if err := l.SetParent("sub-1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	l.Freeze("sub-1")
	r := l.Rollup("merchant")
	if r.Frozen {
		t.Errorf("merchant rollup Frozen = true, but only sub-1 is frozen")
	}
	r2 := l.Rollup("sub-1")
	if !r2.Frozen {
		t.Errorf("sub-1 rollup Frozen = false, want true")
	}
}

func TestRollupVersionMatchesSnapshot(t *testing.T) {
	l := New()
	if err := l.SetParent("sub-1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	postCredits(t, l, "sub-1", 100, "USD", 1)
	_, v := l.Snapshot("sub-1")
	if r := l.Rollup("merchant"); r.Version != v {
		t.Errorf("rollup version = %d, snapshot version = %d", r.Version, v)
	}
}

// TestRollupConcurrentWithPosts exercises the read-lock path: rollups run
// while writers post, under -race. A rollup may observe any consistent
// prefix of the postings, so the test only asserts the accounting
// equation holds on the observed total.
func TestRollupConcurrentWithPosts(t *testing.T) {
	l := New()
	const subs = 8
	for i := 0; i < subs; i++ {
		if err := l.SetParent(AccountID(fmt.Sprintf("sub-%d", i)), "merchant"); err != nil {
			t.Fatalf("SetParent: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < subs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acct := AccountID(fmt.Sprintf("sub-%d", i))
			for n := 0; n < 50; n++ {
				// t.Errorf (not Fatalf): FailNow must run on the test
				// goroutine; Errorf is safe from worker goroutines.
				e := JournalEntry{
					ID:            fmt.Sprintf("conc-%d-%d", i, n),
					DebitAccount:  acct,
					CreditAccount: "bank",
					AmountCents:   10,
					Currency:      "USD",
					CreatedAt:     time.Date(2026, 10, 7, 13, 0, 0, n, time.UTC),
				}
				if _, _, err := l.Post(e); err != nil {
					t.Errorf("concurrent Post: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := l.Rollup("merchant")
			if r.DescendantCount != subs {
				t.Errorf("DescendantCount = %d, want %d", r.DescendantCount, subs)
			}
			if len(r.ByCurrency) > 1 {
				t.Errorf("ByCurrency = %+v, want at most one USD row", r.ByCurrency)
			}
		}()
	}
	wg.Wait()
	r := l.Rollup("merchant")
	if len(r.ByCurrency) != 1 || r.ByCurrency[0].BalanceCents != subs*50*10 {
		t.Errorf("final rollup = %+v, want [{USD %d}]", r.ByCurrency, subs*50*10)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation after concurrent posts: %v", err)
	}
}
