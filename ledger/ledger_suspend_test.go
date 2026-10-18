package ledger

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSuspendRejectsPostsOnEitherLeg(t *testing.T) {
	for _, tc := range []struct {
		name      string
		suspended AccountID
	}{
		{"suspended debit leg", "cash"},
		{"suspended credit leg", "equity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			if _, _, err := l.SuspendAccount(tc.suspended); err != nil {
				t.Fatalf("SuspendAccount: %v", err)
			}
			if !l.IsSuspended(tc.suspended) {
				t.Fatalf("IsSuspended(%q) = false after SuspendAccount", tc.suspended)
			}

			_, _, err := l.Post(validEntry())
			if !errors.Is(err, ErrAccountSuspended) {
				t.Fatalf("Post through suspended account: got %v, want ErrAccountSuspended", err)
			}

			// The rejection booked nothing: balances untouched, no
			// journal row, version unbumped, chain unextended.
			if got := l.Balance("cash"); got != 0 {
				t.Errorf("Balance(cash) = %d, want 0 after suspended rejection", got)
			}
			if n := len(l.Entries()); n != 0 {
				t.Errorf("Entries() = %d rows, want 0 after suspended rejection", n)
			}
			if _, v := l.Snapshot("cash"); v != 0 {
				t.Errorf("version = %d, want 0 (rejected posts do not bump)", v)
			}
			if err := l.VerifyChain(); err != nil {
				t.Fatalf("VerifyChain on empty ledger: %v", err)
			}
		})
	}
}

func TestSuspendAndResumeAreIdempotent(t *testing.T) {
	l := New()
	s1, already, err := l.SuspendAccount("cash")
	if err != nil || already {
		t.Fatalf("first SuspendAccount: %v, already=%v", err, already)
	}
	s2, already, err := l.SuspendAccount("cash")
	if err != nil || !already {
		t.Fatalf("second SuspendAccount: %v, already=%v, want already=true", err, already)
	}
	if s1 != s2 {
		t.Errorf("idempotent suspend returned different records: %+v vs %+v", s1, s2)
	}
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("Post after double suspend: got %v, want ErrAccountSuspended", err)
	}

	if !l.ResumeAccount("cash") {
		t.Fatal("ResumeAccount returned false for a suspended account")
	}
	if l.ResumeAccount("cash") {
		t.Fatal("ResumeAccount returned true for a non-suspended account")
	}
	if l.IsSuspended("cash") {
		t.Fatal("IsSuspended(cash) = true after ResumeAccount")
	}
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post after ResumeAccount: unexpected error %v", err)
	}
}

func TestSuspendEmptyAccountRejected(t *testing.T) {
	l := New()
	if _, _, err := l.SuspendAccount(""); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("SuspendAccount(\"\"): got %v, want ErrEmptyAccountID", err)
	}
}

func TestSuspendReplayAfterSuspend(t *testing.T) {
	l := New()
	orig, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	// A replay books nothing new, so it must not fail on an account
	// suspended after the original posting — same contract as frozen.
	replayed, duplicate, err := l.Post(JournalEntry{ID: "e1b", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay after suspend: %v", err)
	}
	if !duplicate || replayed.ID != orig.ID {
		t.Errorf("replay = (%q, dup=%v), want (%q, dup=true)", replayed.ID, duplicate, orig.ID)
	}
}

func TestSuspendReadsUnaffected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	before := func() uint64 { _, v := l.Snapshot("cash"); return v }()
	if _, _, err := l.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, _, err := l.SuspendAccount("unseen-account"); err != nil {
		t.Fatalf("SuspendAccount unknown: %v", err)
	}

	// Reads keep working on the suspended account.
	if got := l.Balance("cash"); got != 1000 {
		t.Errorf("Balance(cash) = %d, want 1000", got)
	}
	if _, v := l.Snapshot("cash"); v != before {
		t.Errorf("version moved from %d to %d across suspend", before, v)
	}
	_ = l.TrialBalance("cash")
	if n := len(l.Entries()); n != 1 {
		t.Errorf("Entries() = %d, want 1", n)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	report := l.Reconcile(time.Now())
	found := false
	for _, a := range report.SuspendedAccounts {
		if a == "cash" {
			found = true
		}
	}
	if !found {
		t.Errorf("Reconcile.SuspendedAccounts = %v, want it to contain cash", report.SuspendedAccounts)
	}
}

func TestSuspendStacksWithFrozenAndClosed(t *testing.T) {
	l := New()
	l.Freeze("cash")
	if _, _, err := l.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	// The frozen stop is the older risk control and wins on a
	// double-set account: pre-suspension observable behavior is
	// preserved.
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post on frozen+suspended: got %v, want ErrAccountFrozen", err)
	}

	l2 := New()
	if _, _, err := l2.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, _, err := l2.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	// The terminal close still rejects first.
	if _, _, err := l2.Post(validEntry()); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("Post on closed+suspended: got %v, want ErrAccountClosed", err)
	}
}

func TestSuspendGatesOtherWritePaths(t *testing.T) {
	// Transfer.
	l := New()
	if _, _, err := l.SuspendAccount("alice"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tr1", From: "alice", To: "bob", AmountCents: 100, Currency: "USD"}); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("PostTransfer through suspended payer: got %v, want ErrAccountSuspended", err)
	}

	// Reversal of an entry touching the suspended account.
	l2 := New()
	orig, _, err := l2.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l2.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, _, err := l2.ReverseEntry(orig.ID, ""); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("ReverseEntry through suspended account: got %v, want ErrAccountSuspended", err)
	}

	// Batch.
	l3 := New()
	if _, _, err := l3.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, err := l3.PostBatch(Batch{ID: "b1", Entries: []JournalEntry{{ID: "be1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 10, Currency: "USD"}}}); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("PostBatch through suspended account: got %v, want ErrAccountSuspended", err)
	}

	// Hold creation.
	l4 := New()
	if _, _, err := l4.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, _, err := l4.Hold(Hold{ID: "h1", Account: "cash", AmountCents: 50, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("Hold on suspended account: got %v, want ErrAccountSuspended", err)
	}

	// System posting path (postLockedGated): an accrual through a
	// suspended account is rejected too.
	l5 := New()
	if _, _, err := l5.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, _, err := l5.postSystemLocked(JournalEntry{ID: "sys1", DebitAccount: "cash", CreditAccount: "interest", AmountCents: 5, Currency: "USD"}, false); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("system posting through suspended account: got %v, want ErrAccountSuspended", err)
	}
}

func TestSuspendSnapshotRoundTrip(t *testing.T) {
	l := New()
	s1, _, err := l.SuspendAccount("cash")
	if err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !l2.IsSuspended("cash") {
		t.Fatal("restored ledger: IsSuspended(cash) = false, want true")
	}
	// The restored record is verbatim, including the original
	// timestamp — a restore must not move the pause timestamp.
	s2, already, err := l2.SuspendAccount("cash")
	if err != nil || !already {
		t.Fatalf("idempotent suspend on restored ledger: %v, already=%v", err, already)
	}
	if !s2.SuspendedAt.Equal(s1.SuspendedAt) {
		t.Errorf("restored SuspendedAt = %v, want %v", s2.SuspendedAt, s1.SuspendedAt)
	}
	if _, _, err := l2.Post(validEntry()); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("Post on restored suspended ledger: got %v, want ErrAccountSuspended", err)
	}
	if !l2.ResumeAccount("cash") {
		t.Fatal("ResumeAccount on restored ledger returned false")
	}
	if _, _, err := l2.Post(validEntry()); err != nil {
		t.Fatalf("Post after resume on restored ledger: %v", err)
	}
}

func TestSuspendAuditAndOutbox(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l, _, dir := newOutboxLedger(t, WithAuditLog(al))

	if _, _, err := l.SuspendAccount("mallory"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if !l.ResumeAccount("mallory") {
		t.Fatalf("ResumeAccount: returned false")
	}
	// Idempotent replay emits nothing new.
	if _, _, err := l.SuspendAccount("mallory"); err != nil {
		t.Fatalf("re-suspend: %v", err)
	}
	if _, already, err := l.SuspendAccount("mallory"); err != nil || !already {
		t.Fatalf("idempotent suspend: %v, already=%v", err, already)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var ops []string
	for _, e := range events {
		// The audit log emits an "audit_signing_disabled" system
		// event on open when signing is off; it is not part of the
		// suspension lifecycle under test.
		if e.Op == "audit_signing_disabled" {
			continue
		}
		ops = append(ops, e.Op)
	}
	want := []string{"suspend", "resume", "suspend"}
	if len(ops) != len(want) {
		t.Fatalf("audit ops = %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("audit ops = %v, want %v", ops, want)
		}
	}

	evs := readAllOutbox(t, dir)
	if got, want := eventTypes(evs), []string{"account_suspended", "account_resumed", "account_suspended"}; len(got) != len(want) {
		t.Fatalf("outbox types = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("outbox types = %v, want %v", got, want)
			}
		}
	}
	if evs[0].Account != "mallory" {
		t.Errorf("outbox account = %q, want mallory", evs[0].Account)
	}
}

func TestSuspendConcurrent(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := AccountID("acct")
			if i%2 == 0 {
				_, _, _ = l.SuspendAccount(a)
			} else {
				_ = l.ResumeAccount(a)
			}
			_ = l.IsSuspended(a)
			_, _, _ = l.Post(JournalEntry{ID: "e", DebitAccount: a, CreditAccount: "equity", AmountCents: 1, Currency: "USD"})
		}(i)
	}
	wg.Wait()
}
