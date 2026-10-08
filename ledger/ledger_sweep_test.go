package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// seedBalanceIn funds account a with cents in the given currency via a
// plain Post, so sweep tests start from known multi-currency balances.
func seedBalanceIn(t *testing.T, l *Ledger, a AccountID, cents int64, currency string) {
	t.Helper()
	_, _, err := l.Post(JournalEntry{
		ID:            fmt.Sprintf("seed-%s-%s-%s", t.Name(), a, currency),
		DebitAccount:  a,
		CreditAccount: "test-funding",
		AmountCents:   cents,
		Currency:      currency,
	})
	if err != nil {
		t.Fatalf("seedBalanceIn(%s, %d, %s): %v", a, cents, currency, err)
	}
}

func TestPostSweepHappyPath(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 10000)
	seedBalance(t, l, "sub-b", 2500)

	receipt, err := l.PostSweep(Sweep{
		ID:   "sweep-1",
		From: []AccountID{"sub-a", "sub-b"},
		To:   "treasury",
	})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if receipt.SweepID != "sweep-1" {
		t.Errorf("receipt.SweepID = %q, want sweep-1", receipt.SweepID)
	}
	if receipt.Duplicate {
		t.Error("receipt.Duplicate = true, want false")
	}
	if len(receipt.Legs) != 2 || len(receipt.Entries) != 2 {
		t.Fatalf("legs = %d, entries = %d, want 2 and 2", len(receipt.Legs), len(receipt.Entries))
	}
	// Commit order: source order, currencies sorted within each source.
	wantLegs := []SweepLeg{
		{From: "sub-a", Currency: DefaultCurrency, AmountCents: 10000, EntryID: "sweep-1/sub-a/" + DefaultCurrency},
		{From: "sub-b", Currency: DefaultCurrency, AmountCents: 2500, EntryID: "sweep-1/sub-b/" + DefaultCurrency},
	}
	for i, want := range wantLegs {
		if receipt.Legs[i] != want {
			t.Errorf("leg %d = %+v, want %+v", i, receipt.Legs[i], want)
		}
		if receipt.Entries[i].ID != want.EntryID {
			t.Errorf("entry %d ID = %q, want %q", i, receipt.Entries[i].ID, want.EntryID)
		}
		if receipt.Entries[i].DebitAccount != "treasury" || receipt.Entries[i].CreditAccount != want.From {
			t.Errorf("entry %d legs = debit %q credit %q, want treasury/%s",
				i, receipt.Entries[i].DebitAccount, receipt.Entries[i].CreditAccount, want.From)
		}
	}
	// Sources are zeroed, the target collected everything.
	if got := l.Balance("sub-a"); got != 0 {
		t.Errorf("Balance(sub-a) = %d, want 0", got)
	}
	if got := l.Balance("sub-b"); got != 0 {
		t.Errorf("Balance(sub-b) = %d, want 0", got)
	}
	if got := l.Balance("treasury"); got != 12500 {
		t.Errorf("Balance(treasury) = %d, want 12500", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostSweepMultiCurrency(t *testing.T) {
	l := New()
	seedBalanceIn(t, l, "sub-a", 10000, "USD")
	seedBalanceIn(t, l, "sub-a", 5000, "EUR")
	seedBalanceIn(t, l, "sub-b", 3000, "JPY")

	receipt, err := l.PostSweep(Sweep{ID: "sweep-mc", From: []AccountID{"sub-a", "sub-b"}, To: "treasury"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Legs) != 3 {
		t.Fatalf("legs = %d, want 3", len(receipt.Legs))
	}
	// Currencies sorted within each source: EUR before USD.
	want := []SweepLeg{
		{From: "sub-a", Currency: "EUR", AmountCents: 5000, EntryID: "sweep-mc/sub-a/EUR"},
		{From: "sub-a", Currency: "USD", AmountCents: 10000, EntryID: "sweep-mc/sub-a/USD"},
		{From: "sub-b", Currency: "JPY", AmountCents: 3000, EntryID: "sweep-mc/sub-b/JPY"},
	}
	for i := range want {
		if receipt.Legs[i] != want[i] {
			t.Errorf("leg %d = %+v, want %+v", i, receipt.Legs[i], want[i])
		}
	}
	if got := l.BalanceIn("treasury", "EUR"); got != 5000 {
		t.Errorf("treasury EUR = %d, want 5000", got)
	}
	if got := l.BalanceIn("treasury", "JPY"); got != 3000 {
		t.Errorf("treasury JPY = %d, want 3000", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestPostSweepSkipsZeroAndNegativeBalances(t *testing.T) {
	l := New()
	seedBalance(t, l, "rich", 7000)
	// "poor" has a negative balance: debit funding, credit poor.
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-poor", DebitAccount: "test-funding", CreditAccount: "poor", AmountCents: 1500,
	}); err != nil {
		t.Fatalf("seed negative: %v", err)
	}

	receipt, err := l.PostSweep(Sweep{
		ID:   "sweep-skip",
		From: []AccountID{"rich", "empty", "poor"},
		To:   "treasury",
	})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Legs) != 1 {
		t.Fatalf("legs = %d, want 1 (only the positive balance moves)", len(receipt.Legs))
	}
	if receipt.Legs[0].From != "rich" || receipt.Legs[0].AmountCents != 7000 {
		t.Errorf("leg = %+v, want rich/7000", receipt.Legs[0])
	}
	// The negative balance stays: sweeping must never move debt.
	if got := l.Balance("poor"); got != -1500 {
		t.Errorf("Balance(poor) = %d, want -1500 (untouched)", got)
	}

	// All sources non-positive: an empty sweep succeeds with no legs.
	empty, err := l.PostSweep(Sweep{ID: "sweep-empty", From: []AccountID{"empty"}, To: "treasury"})
	if err != nil {
		t.Fatalf("PostSweep empty: %v", err)
	}
	if len(empty.Legs) != 0 || len(empty.Entries) != 0 {
		t.Errorf("empty sweep legs/entries = %d/%d, want 0/0", len(empty.Legs), len(empty.Entries))
	}
}

func TestPostSweepValidation(t *testing.T) {
	l := New()
	cases := []struct {
		name string
		s    Sweep
		want error
	}{
		{"empty ID", Sweep{From: []AccountID{"a"}, To: "t"}, ErrEmptySweepID},
		{"no sources", Sweep{ID: "s", To: "t"}, ErrEmptySweepSources},
		{"empty source", Sweep{ID: "s", From: []AccountID{""}, To: "t"}, ErrEmptySweepSource},
		{"empty target", Sweep{ID: "s", From: []AccountID{"a"}}, ErrEmptySweepTarget},
		{"target is source", Sweep{ID: "s", From: []AccountID{"a", "t"}, To: "t"}, ErrSweepTargetIsSource},
		{"duplicate source", Sweep{ID: "s", From: []AccountID{"a", "a"}, To: "t"}, ErrDuplicateSweepSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, versionBefore := l.Snapshot("nobody")
			entriesBefore := len(l.Entries())
			if _, err := l.PostSweep(tc.s); !errors.Is(err, tc.want) {
				t.Errorf("PostSweep err = %v, want %v", err, tc.want)
			}
			assertLedgerUntouched(t, l, versionBefore, entriesBefore)
		})
	}
}

func TestPostSweepIDConflict(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 1000)
	// Occupy the sweep ID as a journal entry ID.
	if _, _, err := l.Post(JournalEntry{
		ID: "sweep-1", DebitAccount: "x", CreditAccount: "y", AmountCents: 1,
	}); err != nil {
		t.Fatalf("seed conflicting entry: %v", err)
	}
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())
	if _, err := l.PostSweep(Sweep{ID: "sweep-1", From: []AccountID{"sub-a"}, To: "t"}); !errors.Is(err, ErrSweepIDConflict) {
		t.Errorf("PostSweep err = %v, want ErrSweepIDConflict", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
}

func TestPostSweepFrozen(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 1000)
	seedBalance(t, l, "sub-b", 2000)

	l.Freeze("sub-b")
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())
	if _, err := l.PostSweep(Sweep{ID: "s", From: []AccountID{"sub-a", "sub-b"}, To: "t"}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("PostSweep err = %v, want ErrAccountFrozen", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	// The unfrozen source was not swept: atomicity.
	if got := l.Balance("sub-a"); got != 1000 {
		t.Errorf("Balance(sub-a) = %d, want 1000 (sweep rolled back nothing, committed nothing)", got)
	}

	l.Freeze("treasury")
	if _, err := l.PostSweep(Sweep{ID: "s2", From: []AccountID{"sub-a"}, To: "treasury"}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("PostSweep to frozen target err = %v, want ErrAccountFrozen", err)
	}
}

func TestPostSweepIdempotentReplay(t *testing.T) {
	l := New()
	seedBalance(t, l, "sub-a", 4000)

	first, err := l.PostSweep(Sweep{
		ID: "sweep-1", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	entriesBefore := len(l.Entries())
	_, versionBefore := l.Snapshot("nobody")

	second, err := l.PostSweep(Sweep{
		ID: "sweep-1", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("PostSweep replay: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("replay booked %d new entries, want 0", len(l.Entries())-entriesBefore)
	}
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Error("replay bumped the version, want no change")
	}
	if len(second.Legs) != len(first.Legs) || len(second.Entries) != len(first.Entries) {
		t.Errorf("replay legs/entries = %d/%d, want %d/%d",
			len(second.Legs), len(second.Entries), len(first.Legs), len(first.Entries))
	}
	for i := range first.Legs {
		if second.Legs[i] != first.Legs[i] {
			t.Errorf("replay leg %d = %+v, want %+v", i, second.Legs[i], first.Legs[i])
		}
	}

	// A replay returns the original receipt even when the source was
	// frozen after the original sweep: the replay books nothing new.
	l.Freeze("sub-a")
	third, err := l.PostSweep(Sweep{
		ID: "sweep-1", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("PostSweep replay after freeze: %v", err)
	}
	if !third.Duplicate {
		t.Error("replay-after-freeze Duplicate = false, want true")
	}
}

func TestPostSweepNeverChargesFee(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "fee-revenue")) // 2.5% — must not apply
	seedBalance(t, l, "sub-a", 10000)

	receipt, err := l.PostSweep(Sweep{ID: "sweep-fee", From: []AccountID{"sub-a"}, To: "treasury"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Entries) != 1 {
		t.Fatalf("entries = %d, want exactly 1 (no fee leg)", len(receipt.Entries))
	}
	// The target receives the full balance: the fee policy is ignored.
	if got := l.Balance("treasury"); got != 10000 {
		t.Errorf("Balance(treasury) = %d, want 10000 (no fee taken)", got)
	}
	if got := l.Balance("fee-revenue"); got != 0 {
		t.Errorf("Balance(fee-revenue) = %d, want 0", got)
	}
}

func TestPostSweepIdempotencyKeyExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(50 * time.Millisecond))
	seedBalance(t, l, "sub-a", 1000)

	if _, err := l.PostSweep(Sweep{ID: "s1", From: []AccountID{"sub-a"}, To: "t1", IdempotencyKey: "k"}); err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	// After the TTL the key is forgotten: reposting books a brand-new
	// sweep instead of replaying the old receipt.
	time.Sleep(60 * time.Millisecond)
	if removed := l.ExpireIdempotencyKeys(); removed != 1 {
		t.Fatalf("ExpireIdempotencyKeys removed %d keys, want 1", removed)
	}
	seedBalance(t, l, "sub-a", 2000)
	receipt, err := l.PostSweep(Sweep{ID: "s2", From: []AccountID{"sub-a"}, To: "t2", IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("PostSweep after TTL: %v", err)
	}
	if receipt.Duplicate {
		t.Error("replay after TTL Duplicate = true, want false (new sweep)")
	}
	if len(receipt.Legs) != 1 || receipt.Legs[0].AmountCents != 2000 {
		t.Errorf("new sweep legs = %+v, want one 2000-cent leg", receipt.Legs)
	}
}

func TestPostSweepConcurrent(t *testing.T) {
	l := New()
	const sources = 20
	for i := 0; i < sources; i++ {
		seedBalance(t, l, AccountID(fmt.Sprintf("sub-%d", i)), 1000)
	}

	var wg sync.WaitGroup
	errs := make([]error, sources)
	for i := 0; i < sources; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.PostSweep(Sweep{
				ID:   fmt.Sprintf("sweep-%d", i),
				From: []AccountID{AccountID(fmt.Sprintf("sub-%d", i))},
				To:   "treasury",
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("sweep %d: %v", i, err)
		}
	}
	// Every source zeroed exactly once; the treasury collected everything.
	for i := 0; i < sources; i++ {
		if got := l.Balance(AccountID(fmt.Sprintf("sub-%d", i))); got != 0 {
			t.Errorf("Balance(sub-%d) = %d, want 0", i, got)
		}
	}
	if got := l.Balance("treasury"); got != int64(sources)*1000 {
		t.Errorf("Balance(treasury) = %d, want %d", got, sources*1000)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}
