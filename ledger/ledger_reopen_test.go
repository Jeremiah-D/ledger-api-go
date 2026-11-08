package ledger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// Reopening a closed account restores its write paths at once: posts
// that rejected with ErrAccountClosed while closed succeed again, the
// version is untouched by the close/reopen pair, and the returned
// record is the original closure.
func TestReopenAccountRestoresPost(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 10000)
	ver := l.Version()

	closure, already, err := l.CloseAccount("cash", false)
	if err != nil || already {
		t.Fatalf("CloseAccount: already=%v err=%v", already, err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("Post through closed account: %v, want ErrAccountClosed", err)
	}

	reopened, wasClosed, err := l.ReopenAccount("cash", false)
	if err != nil || !wasClosed {
		t.Fatalf("ReopenAccount: wasClosed=%v err=%v", wasClosed, err)
	}
	if reopened != closure {
		t.Fatalf("ReopenAccount returned %+v, want original closure %+v", reopened, closure)
	}
	if l.IsClosed("cash") {
		t.Fatal("IsClosed still true after reopen")
	}
	if got := l.Version(); got != ver {
		t.Fatalf("close/reopen bumped the version: %d -> %d", ver, got)
	}
	mustPostClose(t, l, "p2", "cash", "equity", 100)
	if got := l.Balance("cash"); got != 10100 {
		t.Fatalf("Balance after reopened post = %d, want 10100", got)
	}
}

// Reopening a never-closed account is an idempotent no-op: it returns
// wasClosed == false and audits nothing.
func TestReopenAccountNeverClosedNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	closure, wasClosed, err := l.ReopenAccount("cash", false)
	if err != nil || wasClosed {
		t.Fatalf("ReopenAccount(live): wasClosed=%v err=%v", wasClosed, err)
	}
	if closure != (AccountClosure{}) {
		t.Fatalf("ReopenAccount(live) returned %+v, want zero record", closure)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := auditEventsByOp(t, dir)["account_reopen"]; len(got) != 0 {
		t.Fatalf("no-op reopen audited %d account_reopen events", len(got))
	}
}

// Reopening an unknown account is a no-op too: there is nothing to
// restore, and the account stays open when it appears later.
func TestReopenAccountUnknownNoop(t *testing.T) {
	l := New()
	closure, wasClosed, err := l.ReopenAccount("ghost", true)
	if err != nil || wasClosed {
		t.Fatalf("ReopenAccount(unknown): wasClosed=%v err=%v", wasClosed, err)
	}
	if closure != (AccountClosure{}) {
		t.Fatalf("ReopenAccount(unknown) returned %+v, want zero record", closure)
	}
	if l.IsClosed("ghost") {
		t.Fatal("unknown account reports closed after no-op reopen")
	}
}

func TestReopenAccountEmptyID(t *testing.T) {
	l := New()
	if _, _, err := l.ReopenAccount("", false); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("ReopenAccount(empty): got %v, want ErrEmptyAccountID", err)
	}
}

// Cascade reopen restores the whole subtree closed by the cascade
// close, returning the root's original closure record.
func TestReopenAccountCascade(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "merchant", "equity", 100000)
	if err := l.SetParent("sub1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := l.SetParent("sub2", "sub1"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	root, _, err := l.CloseAccount("merchant", true)
	if err != nil {
		t.Fatalf("CloseAccount cascade: %v", err)
	}
	reopened, wasClosed, err := l.ReopenAccount("merchant", true)
	if err != nil || !wasClosed {
		t.Fatalf("ReopenAccount cascade: wasClosed=%v err=%v", wasClosed, err)
	}
	if reopened != root {
		t.Fatalf("ReopenAccount returned %+v, want root closure %+v", reopened, root)
	}
	for _, a := range []AccountID{"merchant", "sub1", "sub2"} {
		if l.IsClosed(a) {
			t.Fatalf("IsClosed(%s) still true after cascade reopen", a)
		}
	}
	mustPostClose(t, l, "after", "sub2", "equity", 100)
}

// Without cascade only the root reopens: descendants closed by the
// original cascade stay closed.
func TestReopenAccountNoCascadeKeepsDescendantsClosed(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "merchant", "equity", 100000)
	if err := l.SetParent("sub1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := l.SetParent("sub2", "sub1"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if _, _, err := l.CloseAccount("merchant", true); err != nil {
		t.Fatalf("CloseAccount cascade: %v", err)
	}
	if _, wasClosed, err := l.ReopenAccount("merchant", false); err != nil || !wasClosed {
		t.Fatalf("ReopenAccount: wasClosed=%v err=%v", wasClosed, err)
	}
	if l.IsClosed("merchant") {
		t.Fatal("root still closed after reopen")
	}
	for _, a := range []AccountID{"sub1", "sub2"} {
		if !l.IsClosed(a) {
			t.Fatalf("IsClosed(%s) = false, want descendant to stay closed", a)
		}
	}
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "sub1", CreditAccount: "equity", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("Post through still-closed descendant: %v, want ErrAccountClosed", err)
	}
	// A descendant closed independently stays closed even under a
	// cascade reopen of the root.
	if _, wasClosed, err := l.ReopenAccount("sub1", false); err != nil || !wasClosed {
		t.Fatalf("ReopenAccount(sub1): wasClosed=%v err=%v", wasClosed, err)
	}
	if !l.IsClosed("sub2") {
		t.Fatal("sub2 reopened by its parent's non-cascade reopen")
	}
}

// Freeze is orthogonal to the lifecycle close: reopening a frozen
// account leaves it frozen, so postings still reject — with
// ErrAccountFrozen, not ErrAccountClosed.
func TestReopenAccountKeepsFrozen(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 10000)
	l.Freeze("cash")
	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, wasClosed, err := l.ReopenAccount("cash", false); err != nil || !wasClosed {
		t.Fatalf("ReopenAccount: wasClosed=%v err=%v", wasClosed, err)
	}
	if !l.IsFrozen("cash") {
		t.Fatal("reopen cleared the freeze")
	}
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post on reopened-but-frozen account: %v, want ErrAccountFrozen", err)
	}
}

// The reopen is audited as account_reopen with the operator carried by
// ctx, and emits one account_reopened outbox event per reopened
// account — a cascade reopen of three accounts emits three.
func TestReopenAccountAuditAndOutbox(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	outDir := t.TempDir()
	ob, err := NewOutbox(outDir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	l := New(WithAuditLog(al), WithOutbox(ob))
	mustPostClose(t, l, "seed", "merchant", "equity", 100000)
	if err := l.SetParent("sub1", "merchant"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := l.SetParent("sub2", "sub1"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if _, _, err := l.CloseAccount("merchant", true); err != nil {
		t.Fatalf("CloseAccount cascade: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, _, err := l.ReopenAccountCtx(ctx, "merchant", true); err != nil {
		t.Fatalf("ReopenAccountCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, auditDir)
	evs := byOp["account_reopen"]
	if len(evs) != 1 {
		t.Fatalf("account_reopen events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Actor != "ops-alan" {
		t.Fatalf("account_reopen actor = %q, want ops-alan", ev.Actor)
	}
	if len(ev.Accounts) != 3 {
		t.Fatalf("account_reopen accounts = %v, want all 3 reopened", ev.Accounts)
	}
	if ev.Details["cascade"] != true || ev.Details["reopened_at"] == nil || ev.Details["closed_at"] == nil {
		t.Fatalf("account_reopen details = %+v, want cascade/reopened_at/closed_at", ev.Details)
	}
	if len(byOp["account_close"]) != 1 {
		t.Fatalf("account_close events = %d, want 1", len(byOp["account_close"]))
	}
	outs, _, corrupt, err := ReadOutboxEvents(outDir, OutboxQuery{Types: []string{OutboxAccountReopened}, Limit: 100})
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadOutboxEvents: %v corrupt=%d", err, corrupt)
	}
	if len(outs) != 3 {
		t.Fatalf("account_reopened outbox events = %d, want 3", len(outs))
	}
	seen := map[AccountID]bool{}
	for _, o := range outs {
		seen[o.Account] = true
	}
	for _, a := range []AccountID{"merchant", "sub1", "sub2"} {
		if !seen[a] {
			t.Fatalf("no account_reopened event for %s: %+v", a, outs)
		}
	}
	if err := ob.Close(); err != nil {
		t.Fatalf("Outbox Close: %v", err)
	}
}

// Close -> reopen -> close -> reopen round-trips: each close is a fresh
// record (alreadyClosed == false) and each reopen restores the account.
func TestReopenAccountRoundTrip(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 10000)
	for i := 0; i < 2; i++ {
		if _, already, err := l.CloseAccount("cash", false); err != nil || already {
			t.Fatalf("round %d close: already=%v err=%v", i, already, err)
		}
		if !l.IsClosed("cash") {
			t.Fatalf("round %d: not closed after close", i)
		}
		if _, wasClosed, err := l.ReopenAccount("cash", false); err != nil || !wasClosed {
			t.Fatalf("round %d reopen: wasClosed=%v err=%v", i, wasClosed, err)
		}
		if l.IsClosed("cash") {
			t.Fatalf("round %d: still closed after reopen", i)
		}
	}
	mustPostClose(t, l, "final", "cash", "equity", 100)
}

// Reopening survives disaster-recovery snapshots, and a closure
// restored from a snapshot can still be reopened.
func TestReopenAccountSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 100000)
	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, _, err := l.ReopenAccount("cash", false); err != nil {
		t.Fatalf("ReopenAccount: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if l2.IsClosed("cash") {
		t.Fatal("restored ledger reports a reopened account as closed")
	}
	mustPostClose(t, l2, "after", "cash", "equity", 100)

	// The other direction: a closure that round-trips through a
	// snapshot is still reopenable on the restored ledger.
	l3 := New()
	mustPostClose(t, l3, "seed", "cash", "equity", 100000)
	closure, _, err := l3.CloseAccount("cash", false)
	if err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	buf.Reset()
	if err := l3.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l4, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	reopened, wasClosed, err := l4.ReopenAccount("cash", false)
	if err != nil || !wasClosed {
		t.Fatalf("ReopenAccount on restored ledger: wasClosed=%v err=%v", wasClosed, err)
	}
	if reopened != closure {
		t.Fatalf("restored reopen returned %+v, want original %+v", reopened, closure)
	}
}

// Postings rejected while the account was closed never come back: the
// rejection booked nothing, and the reopen does not replay them.
func TestReopenAccountRejectedPostingsStayRejected(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 10000)
	ver := l.Version()
	if _, _, err := l.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "lost", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 500, Currency: "USD"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("Post while closed: %v, want ErrAccountClosed", err)
	}
	if _, _, err := l.ReopenAccount("cash", false); err != nil {
		t.Fatalf("ReopenAccount: %v", err)
	}
	if got := l.Version(); got != ver {
		t.Fatalf("rejected post bumped the version: %d -> %d", ver, got)
	}
	if got := l.Balance("cash"); got != 10000 {
		t.Fatalf("Balance = %d, want 10000: the rejected post must not resurrect", got)
	}
	if _, ok := l.entries["lost"]; ok {
		t.Fatal("rejected entry 'lost' exists after reopen")
	}
}

// Concurrent closes and reopens never race: the single mutex serializes
// the closure record and the gate checks.
func TestReopenAccountRace(t *testing.T) {
	l := New()
	mustPostClose(t, l, "seed", "cash", "equity", 1000000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := AccountID("acct")
			if i%2 == 0 {
				if _, _, err := l.CloseAccount(a, false); err != nil {
					t.Errorf("CloseAccount: %v", err)
					return
				}
			} else {
				if _, _, err := l.ReopenAccount(a, false); err != nil {
					t.Errorf("ReopenAccount: %v", err)
					return
				}
			}
			_, _, err := l.Post(JournalEntry{ID: "r" + string(rune('a'+i)), DebitAccount: a, CreditAccount: "cash", AmountCents: 1, Currency: "USD"})
			if err != nil && !errors.Is(err, ErrAccountClosed) {
				t.Errorf("post: %v, want nil or ErrAccountClosed", err)
			}
		}(i)
	}
	wg.Wait()
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after race: %v", err)
	}
}
