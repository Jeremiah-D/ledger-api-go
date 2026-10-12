package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustPostClose(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: debit, CreditAccount: credit, AmountCents: cents, Currency: "USD"}); err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
}

// A closed account rejects every write path that would book a new entry
// through it; a replay of a key posted before the closure returns the
// original entry instead of failing.
func TestCloseAccountPostRejected(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 10000)
	if _, _, err := l.Post(JournalEntry{ID: "pre", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("Post(pre): %v", err)
	}
	ver := l.Version()

	closure, already, err := l.CloseAccount("cash", false)
	if err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if already || closure.Account != "cash" || closure.ClosedAt.IsZero() {
		t.Fatalf("CloseAccount = %+v already=%v, want fresh record", closure, already)
	}
	if got := l.Version(); got != ver {
		t.Fatalf("closing bumped the version: %d -> %d", ver, got)
	}

	// Debit leg and credit leg both reject.
	for _, e := range []JournalEntry{
		{ID: "p1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"},
		{ID: "p2", DebitAccount: "equity", CreditAccount: "cash", AmountCents: 100, Currency: "USD"},
	} {
		if _, _, err := l.Post(e); !errors.Is(err, ErrAccountClosed) {
			t.Fatalf("Post(%s) through closed account: got %v, want ErrAccountClosed", e.ID, err)
		}
	}
	// Rejected posts record nothing.
	if got := l.Version(); got != ver {
		t.Fatalf("rejected post bumped version: %d -> %d", ver, got)
	}

	// Idempotency replay from before the close still returns the
	// original entry: the replay books nothing new.
	orig, dup, err := l.Post(JournalEntry{ID: "replay", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD", IdempotencyKey: "k1"})
	if err != nil || !dup || orig.ID != "pre" {
		t.Fatalf("replay on closed account: orig=%v dup=%v err=%v, want original", orig.ID, dup, err)
	}
	if !l.IsClosed("cash") || l.IsClosed("equity") {
		t.Fatalf("IsClosed wrong: cash=%v equity=%v", l.IsClosed("cash"), l.IsClosed("equity"))
	}
}

// Closing is irreversible and idempotent: there is no unclose, and
// re-closing returns the original record instead of an error.
func TestCloseAccountIrreversibleAndIdempotent(t *testing.T) {
	l := New()
	c1, already1, err := l.CloseAccount("cash", false)
	if err != nil || already1 {
		t.Fatalf("first close: already=%v err=%v", already1, err)
	}
	c2, already2, err := l.CloseAccount("cash", false)
	if err != nil || !already2 {
		t.Fatalf("second close: already=%v err=%v, want already=true", already2, err)
	}
	if c2 != c1 {
		t.Fatalf("replay returned different record: %+v vs %+v", c2, c1)
	}
	if _, _, err := l.CloseAccount("", false); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("CloseAccount(empty): got %v, want ErrEmptyAccountID", err)
	}
}

// Every write path through a closed account must reject.
func TestCloseAccountAllWritePaths(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 200000)
	mustPostClose(t, l, "rev-me", "cash", "equity", 5000)

	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}

	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "cash", To: "rent", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostTransfer from closed: got %v, want ErrAccountClosed", err)
	}
	// A fee leg naming a closed fee account also rejects.
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "equity", To: "rent", AmountCents: 100, Currency: "USD", FeeCents: 10, FeeAccount: "cash"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostTransfer fee leg through closed: got %v, want ErrAccountClosed", err)
	}
	if _, err := l.PostBatch(Batch{ID: "b1", Entries: []JournalEntry{{ID: "b1e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"}}}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostBatch through closed: got %v, want ErrAccountClosed", err)
	}
	if _, err := l.PostSweep(Sweep{ID: "s1", From: []AccountID{"cash"}, To: "equity"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostSweep from closed: got %v, want ErrAccountClosed", err)
	}
	if _, err := l.PostSweep(Sweep{ID: "s2", From: []AccountID{"equity"}, To: "cash"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostSweep to closed: got %v, want ErrAccountClosed", err)
	}
	if _, err := l.PostMerge(Merge{ID: "m1", From: "cash", To: "equity"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("PostMerge from closed: got %v, want ErrAccountClosed", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "cash", AmountCents: 100, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("Hold on closed: got %v, want ErrAccountClosed", err)
	}
	if _, _, err := l.ReverseEntry("rev-me", ""); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("ReverseEntry on closed: got %v, want ErrAccountClosed", err)
	}

	// Hold capture on a closed account rejects, but a hold placed before
	// the close can still be captured... on a live capture target. Put
	// the hold on a live account, then close the capture target.
	l2 := New()
	mustPostClose(t, l2, "seed", "funds", "equity", 200000)
	if _, _, err := l2.Hold(Hold{ID: "h1", Account: "funds", AmountCents: 10000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, _, err := l2.CloseAccount("rent", false); err != nil {
		t.Fatalf("CloseAccount(rent): %v", err)
	}
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h1", To: "rent", AmountCents: 10000}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("Capture to closed: got %v, want ErrAccountClosed", err)
	}
	// Journal rules whose derived leg books through a closed account
	// reject the whole post.
	l3 := New()
	mustPostClose(t, l3, "seed3", "cash3", "equity3", 200000)
	if _, _, err := l3.CloseAccount("fee3", false); err != nil {
		t.Fatalf("CloseAccount(fee3): %v", err)
	}
	if err := l3.SetJournalRule(JournalRule{ID: "r1", Enabled: true, Description: "fee", MatchDebitAccount: "cash3", Legs: []RuleLeg{{DebitAccount: "fee3", CreditAccount: "cash3", FixedCents: 10}}}); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	if _, _, err := l3.Post(JournalEntry{ID: "rl1", DebitAccount: "cash3", CreditAccount: "equity3", AmountCents: 1000, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("Post with rule leg through closed: got %v, want ErrAccountClosed", err)
	}
}

// Reads keep working on closed accounts: balances, snapshots, the hash
// chain, reconcile, and trial balance are unaffected.
func TestCloseAccountReadsUnaffected(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 100000)
	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if got := l.Balance("cash"); got != 100000 {
		t.Fatalf("Balance on closed account = %d, want 100000", got)
	}
	bal, ver := l.Snapshot("cash")
	if bal != 100000 {
		t.Fatalf("Snapshot balance = %d, want 100000", bal)
	}
	_ = ver
	tb := l.TrialBalance("cash")
	if !tb.Closed || tb.Frozen {
		t.Fatalf("TrialBalance = %+v, want Closed=true Frozen=false", tb)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on closed account: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation on closed account: %v", err)
	}
	rep := l.Reconcile(time.Now())
	found := false
	for _, a := range rep.ClosedAccounts {
		if a == "cash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Reconcile missing closed_accounts entry: %+v", rep.ClosedAccounts)
	}
	entries, _, err := l.ListAccountEntries("cash", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().Add(time.Hour), "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListAccountEntries on closed = %d, %v, want 1 entry", len(entries), err)
	}
}

// Cascade closes the whole sub-account subtree; reads on every closed
// member keep working.
func TestCloseAccountCascade(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "merchant", "equity", 100000)
	if err := l.SetParent("sub1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := l.SetParent("sub2", "sub1"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	closure, already, err := l.CloseAccount("merchant", true)
	if err != nil || already {
		t.Fatalf("CloseAccount cascade: already=%v err=%v", already, err)
	}
	if closure.Account != "merchant" {
		t.Fatalf("closure account = %q, want merchant", closure.Account)
	}
	for _, a := range []AccountID{"merchant", "sub1", "sub2"} {
		if !l.IsClosed(a) {
			t.Fatalf("IsClosed(%s) = false after cascade", a)
		}
	}
	// Without cascade only the root closes.
	l2 := New()
	if err := l2.SetParent("c1", "root"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if _, _, err := l2.CloseAccount("root", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if l2.IsClosed("c1") {
		t.Fatalf("non-cascade close closed the child")
	}
}

// Closures survive disaster-recovery snapshots: a restore must not
// silently reopen accounts to new postings.
func TestCloseAccountSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 100000)
	closure, _, err := l.CloseAccount("cash", false)
	if err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !l2.IsClosed("cash") {
		t.Fatalf("restored ledger lost the closure")
	}
	c, already, err := l2.CloseAccount("cash", false)
	if err != nil || !already || c != closure {
		t.Fatalf("restored replay: %+v already=%v err=%v, want original record", c, already, err)
	}
	if _, _, err := l2.Post(JournalEntry{ID: "x", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("restored ledger posted through closed account: %v", err)
	}
}

// Concurrent closes and posts never race: the single mutex serializes
// the closure record and the gate checks.
func TestCloseAccountRace(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 1000000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := AccountID("acct")
			if _, _, err := l.CloseAccount(a, false); err != nil {
				t.Errorf("CloseAccount: %v", err)
				return
			}
			_, _, err := l.Post(JournalEntry{ID: "r" + string(rune('a'+i)), DebitAccount: a, CreditAccount: "cash", AmountCents: 1, Currency: "USD"})
			if !errors.Is(err, ErrAccountClosed) {
				t.Errorf("post through closed account: %v", err)
			}
			if !l.IsClosed(a) {
				t.Errorf("IsClosed false after close")
			}
		}(i)
	}
	wg.Wait()
}

// A closed account is also a closed destination for the dry-run twin:
// a what-if simulation must not approve a posting through it.
func TestCloseAccountDryRunRejected(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 100000)
	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, err := l.DryRunPost(JournalEntry{ID: "d", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("DryRunPost through closed: %v, want ErrAccountClosed", err)
	}
}
