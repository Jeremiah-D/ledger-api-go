package ledger

import (
	"errors"
	"strings"
	"testing"
)

func TestCashPoolConstructorValidation(t *testing.T) {
	_, err := NewCashPool("", "header", []AccountID{"p1"}, "USD")
	if !errors.Is(err, ErrCashPoolEmptyID) {
		t.Fatalf("empty ID: got %v, want %v", err, ErrCashPoolEmptyID)
	}
	_, err = NewCashPool("pool", "", []AccountID{"p1"}, "USD")
	if !errors.Is(err, ErrCashPoolEmptyHeader) {
		t.Fatalf("empty header: got %v, want %v", err, ErrCashPoolEmptyHeader)
	}
	_, err = NewCashPool("pool", "header", nil, "USD")
	if !errors.Is(err, ErrCashPoolNoParticipants) {
		t.Fatalf("no participants: got %v, want %v", err, ErrCashPoolNoParticipants)
	}
	_, err = NewCashPool("pool", "header", []AccountID{""}, "USD")
	if !errors.Is(err, ErrCashPoolEmptyParticipant) {
		t.Fatalf("empty participant: got %v, want %v", err, ErrCashPoolEmptyParticipant)
	}
	_, err = NewCashPool("pool", "header", []AccountID{"p1", "p1"}, "USD")
	if !errors.Is(err, ErrCashPoolDuplicateParticipant) {
		t.Fatalf("duplicate participant: got %v, want %v", err, ErrCashPoolDuplicateParticipant)
	}
	_, err = NewCashPool("pool", "header", []AccountID{"header"}, "USD")
	if !errors.Is(err, ErrCashPoolHeaderParticipant) {
		t.Fatalf("header as participant: got %v, want %v", err, ErrCashPoolHeaderParticipant)
	}
	_, err = NewCashPool("pool", "header", []AccountID{"p1"}, "usd")
	if !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency: got %v, want %v", err, ErrInvalidCurrency)
	}
}

func TestCashPoolRunHappyPath(t *testing.T) {
	l := New()
	seedBalance(t, l, "p1", 1000)
	seedBalance(t, l, "p2", 2500)
	// p3 holds nothing: already at zero, listed not swept.
	pool, err := NewCashPool("pool-1", "header", []AccountID{"p1", "p2", "p3"}, "USD")
	if err != nil {
		t.Fatalf("NewCashPool: %v", err)
	}

	rep, err := pool.Run(l)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.TotalCents != 3500 || len(rep.Legs) != 2 {
		t.Fatalf("rep = %+v, want total 3500 over 2 legs", rep)
	}
	if len(rep.AlreadyZero) != 1 || rep.AlreadyZero[0] != "p3" {
		t.Fatalf("already zero = %v, want [p3]", rep.AlreadyZero)
	}
	if rep.HeaderBalanceBefore != 0 || rep.HeaderBalanceAfter != 3500 {
		t.Fatalf("header %d -> %d, want 0 -> 3500", rep.HeaderBalanceBefore, rep.HeaderBalanceAfter)
	}
	if rep.BatchID == "" || len(rep.EntryIDs) != 2 {
		t.Fatalf("rep = %+v, want batch ID and 2 entry IDs", rep)
	}
	for acct, want := range map[AccountID]int64{"header": 3500, "p1": 0, "p2": 0, "p3": 0} {
		if got := l.BalanceIn(acct, "USD"); got != want {
			t.Fatalf("%s = %d, want %d", acct, got, want)
		}
	}
	// The accounting equation still holds after concentration.
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestCashPoolRunNegativeFailsClosed(t *testing.T) {
	// p2 in debit: the run cannot leave every participant at zero by
	// sweeping, so it refuses and books nothing — no partial
	// concentration of p1 alone.
	l := New()
	seedBalance(t, l, "p1", 1000)
	if _, _, err := l.Post(JournalEntry{
		ID:            "debit-p2",
		DebitAccount:  "funding",
		CreditAccount: "p2",
		AmountCents:   500,
	}); err != nil {
		t.Fatalf("seed debit: %v", err)
	}
	pool, err := NewCashPool("pool-1", "header", []AccountID{"p1", "p2"}, "USD")
	if err != nil {
		t.Fatalf("NewCashPool: %v", err)
	}

	if _, err := pool.Run(l); !errors.Is(err, ErrCashPoolNegativeParticipant) {
		t.Fatalf("Run: got %v, want %v", err, ErrCashPoolNegativeParticipant)
	}
	for acct, want := range map[AccountID]int64{"header": 0, "p1": 1000, "p2": -500} {
		if got := l.BalanceIn(acct, "USD"); got != want {
			t.Fatalf("%s = %d, want %d", acct, got, want)
		}
	}
}

func TestCashPoolRunRepeatAndRefill(t *testing.T) {
	// A second run over already-zero participants books nothing; a
	// refill afterwards sweeps under fresh entry IDs — never a
	// collision with the first run's journal rows.
	l := New()
	seedBalance(t, l, "p1", 1000)
	pool, err := NewCashPool("pool-1", "header", []AccountID{"p1"}, "USD")
	if err != nil {
		t.Fatalf("NewCashPool: %v", err)
	}
	first, err := pool.Run(l)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := pool.Run(l)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.Legs) != 0 || second.TotalCents != 0 || second.BatchID != "" {
		t.Fatalf("second rep = %+v, want empty legs, no batch", second)
	}
	if second.HeaderBalanceAfter != 1000 {
		t.Fatalf("header after second run = %d, want 1000", second.HeaderBalanceAfter)
	}

	// seedBalanceIn's entry ID carries the currency suffix, so it
	// cannot collide with the earlier seedBalance row for p1.
	seedBalanceIn(t, l, "p1", 700, "USD")
	third, err := pool.Run(l)
	if err != nil {
		t.Fatalf("third Run: %v", err)
	}
	if third.TotalCents != 700 || len(third.EntryIDs) != 1 {
		t.Fatalf("third rep = %+v, want 700 over 1 leg", third)
	}
	if third.EntryIDs[0] == first.EntryIDs[0] {
		t.Fatalf("entry ID reused across runs: %q", third.EntryIDs[0])
	}
	if !strings.Contains(third.EntryIDs[0], ":3:") {
		t.Fatalf("third entry ID %q does not carry run 3", third.EntryIDs[0])
	}
	if got := l.BalanceIn("header", "USD"); got != 1700 {
		t.Fatalf("header = %d, want 1700", got)
	}
}

func TestCashPoolRunFrozenFailsAtomically(t *testing.T) {
	// One frozen participant rejects the whole run: the clear
	// participant is NOT swept alone. Atomicity, not best effort.
	l := New()
	seedBalance(t, l, "p1", 1000)
	seedBalance(t, l, "p2", 2500)
	l.Freeze("p1")
	pool, err := NewCashPool("pool-1", "header", []AccountID{"p1", "p2"}, "USD")
	if err != nil {
		t.Fatalf("NewCashPool: %v", err)
	}

	if _, err := pool.Run(l); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Run: got %v, want %v", err, ErrAccountFrozen)
	}
	for acct, want := range map[AccountID]int64{"header": 0, "p1": 1000, "p2": 2500} {
		if got := l.BalanceIn(acct, "USD"); got != want {
			t.Fatalf("%s = %d, want %d", acct, got, want)
		}
	}
}

func TestCashPoolCurrencyIsolation(t *testing.T) {
	l := New()
	seedBalance(t, l, "p1", 1000)
	seedBalanceIn(t, l, "p1", 400, "EUR")
	pool, err := NewCashPool("pool-1", "header", []AccountID{"p1"}, "USD")
	if err != nil {
		t.Fatalf("NewCashPool: %v", err)
	}

	rep, err := pool.Run(l)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.TotalCents != 1000 {
		t.Fatalf("total = %d, want 1000", rep.TotalCents)
	}
	if got := l.BalanceIn("p1", "EUR"); got != 400 {
		t.Fatalf("p1 EUR = %d, want 400", got)
	}
	if got := l.BalanceIn("header", "EUR"); got != 0 {
		t.Fatalf("header EUR = %d, want 0", got)
	}
}
