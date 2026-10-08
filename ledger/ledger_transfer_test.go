package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// seedBalance funds account a with cents via a plain Post (debit a, credit
// a funding account), so transfer tests start from a known balance.
func seedBalance(t *testing.T, l *Ledger, a AccountID, cents int64) {
	t.Helper()
	_, _, err := l.Post(JournalEntry{
		ID:            fmt.Sprintf("seed-%s-%s", t.Name(), a),
		DebitAccount:  a,
		CreditAccount: "test-funding",
		AmountCents:   cents,
	})
	if err != nil {
		t.Fatalf("seedBalance(%s, %d): %v", a, cents, err)
	}
}

// assertLedgerUntouched verifies that a rejected transfer recorded
// nothing: no version bump, no new journal rows, no balance movement, and
// an intact audit chain.
func assertLedgerUntouched(t *testing.T, l *Ledger, versionBefore uint64, entriesBefore int) {
	t.Helper()
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Errorf("version = %d, want unchanged %d", v, versionBefore)
	}
	if n := len(l.Entries()); n != entriesBefore {
		t.Errorf("journal entries = %d, want unchanged %d", n, entriesBefore)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after rejected transfer: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation after rejected transfer: %v", err)
	}
}

func TestPostTransferHappyPath(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)

	before := len(l.Entries())
	receipt, err := l.PostTransfer(Transfer{
		ID:          "tx-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 2500,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.TransferID != "tx-1" {
		t.Errorf("receipt.TransferID = %q, want tx-1", receipt.TransferID)
	}
	if receipt.Duplicate {
		t.Errorf("receipt.Duplicate = true, want false")
	}
	if len(receipt.Entries) != 1 {
		t.Fatalf("receipt.Entries has %d entries, want 1", len(receipt.Entries))
	}
	e := receipt.Entries[0]
	if e.ID != "tx-1" {
		t.Errorf("entry.ID = %q, want the transfer ID", e.ID)
	}
	// Sign convention: the payee is debited (balance grows), the payer is
	// credited (balance shrinks).
	if e.DebitAccount != "bob" || e.CreditAccount != "alice" {
		t.Errorf("entry legs = debit %q credit %q, want debit bob credit alice",
			e.DebitAccount, e.CreditAccount)
	}
	if e.AmountCents != 2500 {
		t.Errorf("entry amount = %d, want 2500", e.AmountCents)
	}
	if e.CreatedAt.IsZero() {
		t.Errorf("entry CreatedAt was not filled")
	}

	if got := l.Balance("alice"); got != 7500 {
		t.Errorf("alice balance = %d, want 7500", got)
	}
	if got := l.Balance("bob"); got != 2500 {
		t.Errorf("bob balance = %d, want 2500", got)
	}
	if _, v := l.Snapshot("alice"); v != uint64(before)+1 {
		t.Errorf("version = %d, want %d (one bump)", v, before+1)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostTransferValidation(t *testing.T) {
	cases := []struct {
		name string
		tr   Transfer
		want error
	}{
		{"empty ID", Transfer{From: "a", To: "b", AmountCents: 1}, ErrEmptyTransferID},
		{"empty from", Transfer{ID: "t", To: "b", AmountCents: 1}, ErrEmptyFromAccount},
		{"empty to", Transfer{ID: "t", From: "a", AmountCents: 1}, ErrEmptyToAccount},
		{"same account", Transfer{ID: "t", From: "a", To: "a", AmountCents: 1}, ErrTransferSameAccount},
		{"zero amount", Transfer{ID: "t", From: "a", To: "b", AmountCents: 0}, ErrTransferNonPositiveAmount},
		{"negative amount", Transfer{ID: "t", From: "a", To: "b", AmountCents: -5}, ErrTransferNonPositiveAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			_, err := l.PostTransfer(tc.tr)
			if !errors.Is(err, tc.want) {
				t.Fatalf("PostTransfer err = %v, want %v", err, tc.want)
			}
			assertLedgerUntouched(t, l, 0, 0)
		})
	}
}

func TestPostTransferFrozenLegRollsBack(t *testing.T) {
	for _, frozen := range []AccountID{"alice", "bob"} {
		t.Run("frozen "+string(frozen), func(t *testing.T) {
			l := New()
			seedBalance(t, l, "alice", 10000)
			l.Freeze(frozen)

			_, vBefore := l.Snapshot("alice")
			nBefore := len(l.Entries())
			_, err := l.PostTransfer(Transfer{ID: "tx-f", From: "alice", To: "bob", AmountCents: 100})
			if !errors.Is(err, ErrAccountFrozen) {
				t.Fatalf("PostTransfer err = %v, want ErrAccountFrozen", err)
			}
			assertLedgerUntouched(t, l, vBefore, nBefore)
			if got := l.Balance("alice"); got != 10000 {
				t.Errorf("alice balance moved to %d after frozen rejection", got)
			}
		})
	}
}

func TestPostTransferOverdraftRollsBack(t *testing.T) {
	l := New(WithOverdraftProtection("alice"))
	seedBalance(t, l, "alice", 100)

	_, vBefore := l.Snapshot("alice")
	nBefore := len(l.Entries())
	_, err := l.PostTransfer(Transfer{ID: "tx-o", From: "alice", To: "bob", AmountCents: 500})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("PostTransfer err = %v, want ErrAccountOverdraft", err)
	}
	assertLedgerUntouched(t, l, vBefore, nBefore)
}

func TestPostTransferIdempotentReplay(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)

	first, err := l.PostTransfer(Transfer{
		ID: "tx-r", From: "alice", To: "bob", AmountCents: 300, IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("first PostTransfer: %v", err)
	}
	_, vAfterFirst := l.Snapshot("alice")

	// Same key, different transfer ID: the key identifies the operation,
	// so this is a replay of the first transfer.
	second, err := l.PostTransfer(Transfer{
		ID: "tx-r-retry", From: "alice", To: "bob", AmountCents: 300, IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
	if len(second.Entries) != 1 || second.Entries[0].ID != first.Entries[0].ID {
		t.Errorf("replay returned different entries: %+v", second.Entries)
	}
	if _, v := l.Snapshot("alice"); v != vAfterFirst {
		t.Errorf("version moved on replay: %d -> %d", vAfterFirst, v)
	}
	if got := l.Balance("alice"); got != 9700 {
		t.Errorf("alice balance = %d, want 9700 (applied once)", got)
	}
}

func TestPostTransferReplaySurvivesFreeze(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-fz", From: "alice", To: "bob", AmountCents: 100, IdempotencyKey: "key-fz",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// Freeze after posting: the replay books nothing new, so it must not
	// fail — mirroring Post's idempotency contract.
	l.Freeze("alice")
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-fz-2", From: "alice", To: "bob", AmountCents: 100, IdempotencyKey: "key-fz",
	})
	if err != nil {
		t.Fatalf("replay after freeze: %v", err)
	}
	if !receipt.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
}

func TestPostTransferReplaysRawPostKey(t *testing.T) {
	l := New()
	orig, _, err := l.Post(JournalEntry{
		ID: "raw-1", DebitAccount: "bob", CreditAccount: "alice",
		AmountCents: 50, IdempotencyKey: "shared-key",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	// The idempotency namespace is shared with Post: a transfer reusing a
	// raw-posted key replays the original entry instead of double-booking.
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-x", From: "alice", To: "bob", AmountCents: 50, IdempotencyKey: "shared-key",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if !receipt.Duplicate || len(receipt.Entries) != 1 || receipt.Entries[0].ID != orig.ID {
		t.Errorf("cross-API replay = %+v, want duplicate of %q", receipt, orig.ID)
	}
	if got := l.Balance("bob"); got != 50 {
		t.Errorf("bob balance = %d, want 50 (booked once)", got)
	}
}

func TestPostTransferIDConflict(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "taken", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 10,
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	_, vBefore := l.Snapshot("alice")
	_, err := l.PostTransfer(Transfer{ID: "taken", From: "alice", To: "bob", AmountCents: 10})
	if !errors.Is(err, ErrTransferIDConflict) {
		t.Fatalf("PostTransfer err = %v, want ErrTransferIDConflict", err)
	}
	assertLedgerUntouched(t, l, vBefore, 1)
}

func TestPostTransferConcurrent(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.PostTransfer(Transfer{
				ID:          fmt.Sprintf("ctx-%d", i),
				From:        "alice",
				To:          "bob",
				AmountCents: 100,
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := l.Balance("alice"); got != 1_000_000-50*100 {
		t.Errorf("alice balance = %d, want %d", got, 1_000_000-50*100)
	}
	if got := l.Balance("bob"); got != 50*100 {
		t.Errorf("bob balance = %d, want %d", got, 50*100)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostTransferPreservesCreatedAt(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1000)
	pinned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-ts", From: "alice", To: "bob", AmountCents: 10, CreatedAt: pinned,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if !receipt.Entries[0].CreatedAt.Equal(pinned) {
		t.Errorf("CreatedAt = %v, want pinned %v", receipt.Entries[0].CreatedAt, pinned)
	}
}
