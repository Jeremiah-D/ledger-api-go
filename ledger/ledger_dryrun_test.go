package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDryRunPostSuccess(t *testing.T) {
	l := New()
	e := JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 500, Currency: "USD"}
	res, err := l.DryRunPost(e)
	if err != nil {
		t.Fatalf("DryRunPost = %v", err)
	}
	if !res.WouldSucceed || res.Duplicate {
		t.Fatalf("result = %+v, want WouldSucceed=true Duplicate=false", res)
	}
	if res.VersionBefore != 0 || res.VersionAfter != 1 {
		t.Fatalf("version bracket = %d->%d, want 0->1", res.VersionBefore, res.VersionAfter)
	}
	if len(res.Legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(res.Legs))
	}
	leg := res.Legs[0]
	if leg.DebitBalanceBefore != 0 || leg.DebitBalanceAfter != 500 {
		t.Errorf("debit leg %d->%d, want 0->500", leg.DebitBalanceBefore, leg.DebitBalanceAfter)
	}
	if leg.CreditBalanceBefore != 0 || leg.CreditBalanceAfter != -500 {
		t.Errorf("credit leg %d->%d, want 0->-500", leg.CreditBalanceBefore, leg.CreditBalanceAfter)
	}
	// Zero side effects: nothing was recorded.
	if n := len(l.Entries()); n != 0 {
		t.Errorf("entries after dry run = %d, want 0", n)
	}
	if got := l.Balance("cash"); got != 0 {
		t.Errorf("cash balance after dry run = %d, want 0", got)
	}
	if _, v := l.Snapshot("cash"); v != 0 {
		t.Errorf("version after dry run = %d, want 0", v)
	}
}

func TestDryRunPostChainedLegs(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A dry run against existing state must report the live balances as
	// the legs' starting point.
	res, err := l.DryRunPost(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 300})
	if err != nil {
		t.Fatalf("DryRunPost = %v", err)
	}
	leg := res.Legs[0]
	if leg.DebitBalanceBefore != 1000 || leg.DebitBalanceAfter != 1300 {
		t.Errorf("debit leg %d->%d, want 1000->1300", leg.DebitBalanceBefore, leg.DebitBalanceAfter)
	}
	if res.VersionBefore != 1 || res.VersionAfter != 2 {
		t.Errorf("version bracket = %d->%d, want 1->2", res.VersionBefore, res.VersionAfter)
	}
}

func TestDryRunPostRejections(t *testing.T) {
	l := New()
	frozen := AccountID("frozen-acct")
	l.Freeze(frozen)
	l.EnableOverdraftProtection("guarded")
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}

	cases := []struct {
		name string
		e    JournalEntry
		want error
	}{
		{"validation", JournalEntry{}, ErrEmptyID},
		{"frozen", JournalEntry{ID: "e1", DebitAccount: frozen, CreditAccount: "equity", AmountCents: 10}, ErrAccountFrozen},
		{"overdraft", JournalEntry{ID: "e2", DebitAccount: "equity", CreditAccount: "guarded", AmountCents: 10}, ErrAccountOverdraft},
		{"closed period", JournalEntry{ID: "e3", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, CreatedAt: may}, ErrPeriodClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := l.DryRunPost(tc.e)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DryRunPost = %v, want %v", err, tc.want)
			}
			if res.WouldSucceed {
				t.Fatal("WouldSucceed = true on a would-be rejection")
			}
			if n := len(l.Entries()); n != 0 {
				t.Fatalf("entries after rejected dry run = %d, want 0", n)
			}
		})
	}
}

func TestDryRunPostDuplicate(t *testing.T) {
	l := New()
	e := JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 500, IdempotencyKey: "k1"}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("post: %v", err)
	}
	res, err := l.DryRunPost(e)
	if err != nil {
		t.Fatalf("DryRunPost = %v", err)
	}
	if !res.WouldSucceed || !res.Duplicate {
		t.Fatalf("result = %+v, want WouldSucceed=true Duplicate=true", res)
	}
	// A replay books nothing: no version movement, no balance deltas.
	if res.VersionBefore != 1 || res.VersionAfter != 1 {
		t.Errorf("version bracket = %d->%d, want 1->1", res.VersionBefore, res.VersionAfter)
	}
	if len(res.Legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(res.Legs))
	}
	leg := res.Legs[0]
	if leg.DebitBalanceBefore != 500 || leg.DebitBalanceAfter != 500 {
		t.Errorf("replay leg moved balance %d->%d, want no movement", leg.DebitBalanceBefore, leg.DebitBalanceAfter)
	}
}

func TestDryRunTransferWithFee(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "fee-revenue"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "equity", AmountCents: 10000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := l.DryRunTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 4000})
	if err != nil {
		t.Fatalf("DryRunTransfer = %v", err)
	}
	// Principal + fee leg, fee = floor(4000 * 250 / 10000) = 100.
	if len(res.Legs) != 2 {
		t.Fatalf("legs = %d, want 2 (principal + fee)", len(res.Legs))
	}
	fee := res.Legs[1]
	if fee.AmountCents != 100 || fee.DebitAccount != "fee-revenue" || fee.CreditAccount != "payer" {
		t.Errorf("fee leg = %+v, want 100 cents payer->fee-revenue", fee)
	}
	// The payer's legs chain: principal drains 4000, fee drains 100.
	if res.Legs[0].CreditBalanceBefore != 10000 || res.Legs[0].CreditBalanceAfter != 6000 {
		t.Errorf("principal credit leg %d->%d, want 10000->6000",
			res.Legs[0].CreditBalanceBefore, res.Legs[0].CreditBalanceAfter)
	}
	if fee.CreditBalanceBefore != 6000 || fee.CreditBalanceAfter != 5900 {
		t.Errorf("fee credit leg %d->%d, want 6000->5900", fee.CreditBalanceBefore, fee.CreditBalanceAfter)
	}
	if res.VersionBefore != 1 || res.VersionAfter != 3 {
		t.Errorf("version bracket = %d->%d, want 1->3", res.VersionBefore, res.VersionAfter)
	}
	// Zero side effects.
	if got := l.Balance("payer"); got != 10000 {
		t.Errorf("payer balance after dry run = %d, want 10000", got)
	}
	if n := len(l.Entries()); n != 1 {
		t.Errorf("entries after dry run = %d, want 1 (the seed)", n)
	}
}

func TestDryRunTransferDuplicateAndRejection(t *testing.T) {
	l := New()
	seed := func() {
		if _, _, err := l.Post(JournalEntry{ID: "seed1", DebitAccount: "payer", CreditAccount: "equity", AmountCents: 1000}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed()
	tr := Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 200, IdempotencyKey: "tk1"}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("post transfer: %v", err)
	}
	res, err := l.DryRunTransfer(tr)
	if err != nil || !res.Duplicate {
		t.Fatalf("dry-run replay = (%+v, %v), want Duplicate=true", res, err)
	}

	l.Freeze("payee")
	if _, err := l.DryRunTransfer(Transfer{ID: "t2", From: "payer", To: "payee", AmountCents: 200}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("dry-run through frozen account = %v, want ErrAccountFrozen", err)
	}
}

func TestDryRunSweep(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "s1", DebitAccount: "sub1", CreditAccount: "equity", AmountCents: 300, Currency: "USD"}); err != nil {
		t.Fatalf("seed1: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "s2", DebitAccount: "sub2", CreditAccount: "equity", AmountCents: 700, Currency: "USD"}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	res, err := l.DryRunSweep(Sweep{ID: "sw1", From: []AccountID{"sub1", "sub2"}, To: "treasury"})
	if err != nil {
		t.Fatalf("DryRunSweep = %v", err)
	}
	if len(res.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(res.Legs))
	}
	// Treasury receives 300 then 700, chaining within the dry run.
	if res.Legs[1].DebitBalanceBefore != 300 || res.Legs[1].DebitBalanceAfter != 1000 {
		t.Errorf("treasury leg %d->%d, want 300->1000",
			res.Legs[1].DebitBalanceBefore, res.Legs[1].DebitBalanceAfter)
	}
	if res.Legs[0].CreditBalanceBefore != 300 || res.Legs[0].CreditBalanceAfter != 0 {
		t.Errorf("sub1 leg %d->%d, want 300->0",
			res.Legs[0].CreditBalanceBefore, res.Legs[0].CreditBalanceAfter)
	}
	if res.VersionBefore != 2 || res.VersionAfter != 4 {
		t.Errorf("version bracket = %d->%d, want 2->4", res.VersionBefore, res.VersionAfter)
	}
	// Zero side effects: balances and the sweep key index are untouched.
	if got := l.Balance("treasury"); got != 0 {
		t.Errorf("treasury after dry run = %d, want 0", got)
	}
	if _, err := l.DryRunSweep(Sweep{ID: "sw1", From: []AccountID{"sub1"}, To: "treasury", IdempotencyKey: "dup"}); err != nil {
		t.Fatalf("second dry run: %v", err)
	}
}

func TestDryRunEmitsNoAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.DryRunPost(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 10}); err != nil {
		t.Fatalf("dry run post: %v", err)
	}
	if _, err := l.DryRunTransfer(Transfer{ID: "t1", From: "a", To: "b", AmountCents: 10}); err != nil {
		t.Fatalf("dry run transfer: %v", err)
	}
	if _, err := l.DryRunSweep(Sweep{ID: "sw1", From: []AccountID{"a"}, To: "b"}); err != nil {
		t.Fatalf("dry run sweep: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("close audit log: %v", err)
	}
	written, _, _ := l.AuditStats()
	if written != 0 {
		t.Fatalf("audit events written by dry runs = %d, want 0", written)
	}
}

func TestDryRunConcurrentWithPosts(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("e-conc-%d", i)
			_, _, _ = l.Post(JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1})
			_, _ = l.DryRunPost(JournalEntry{ID: "dry-" + id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1})
		}(i)
	}
	wg.Wait()
	// The dry runs must not have booked anything: only the 8 real posts
	// land (unique IDs make every Post succeed exactly once).
	if n := len(l.Entries()); n != 8 {
		t.Fatalf("entries = %d, want 8 (dry runs must book nothing)", n)
	}
}
