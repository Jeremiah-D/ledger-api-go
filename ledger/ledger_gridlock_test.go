package ledger

import (
	"errors"
	"testing"
)

// enqueueGridlock is the test helper: enqueue or fail the test.
func enqueueGridlock(t *testing.T, q *GridlockQueue, p QueuedPayment) {
	t.Helper()
	if err := q.Enqueue(p); err != nil {
		t.Fatalf("Enqueue(%v): %v", p, err)
	}
}

func TestGridlockEnqueueValidation(t *testing.T) {
	q := NewGridlockQueue()
	cases := []struct {
		name string
		p    QueuedPayment
		want error
	}{
		{"empty ID", QueuedPayment{From: "a", To: "b", AmountCents: 100}, ErrGridlockEmptyPaymentID},
		{"empty payer", QueuedPayment{ID: "p1", To: "b", AmountCents: 100}, ErrGridlockEmptyAccount},
		{"empty payee", QueuedPayment{ID: "p1", From: "a", AmountCents: 100}, ErrGridlockEmptyAccount},
		{"same account", QueuedPayment{ID: "p1", From: "a", To: "a", AmountCents: 100}, ErrGridlockSameAccount},
		{"zero amount", QueuedPayment{ID: "p1", From: "a", To: "b", AmountCents: 0}, ErrGridlockNonPositiveAmount},
		{"negative amount", QueuedPayment{ID: "p1", From: "a", To: "b", AmountCents: -50}, ErrGridlockNonPositiveAmount},
		{"bad currency", QueuedPayment{ID: "p1", From: "a", To: "b", AmountCents: 100, Currency: "usd"}, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := q.Enqueue(tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("Enqueue: got %v, want %v", err, tc.want)
			}
		})
	}

	// Duplicate ID, including after settlement, is rejected.
	enqueueGridlock(t, q, QueuedPayment{ID: "dup", From: "a", To: "b", AmountCents: 100})
	if err := q.Enqueue(QueuedPayment{ID: "dup", From: "a", To: "b", AmountCents: 100}); !errors.Is(err, ErrGridlockDuplicatePayment) {
		t.Fatalf("re-enqueue: got %v, want %v", err, ErrGridlockDuplicatePayment)
	}
}

func TestGridlockResolveNothingSettleable(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100)
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "g1", From: "alice", To: "bob", AmountCents: 150})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 0 || res.BatchID != "" {
		t.Fatalf("settled %d with batch %q, want none", len(res.Settled), res.BatchID)
	}
	if len(res.Remaining) != 1 || res.Remaining[0].ID != "g1" {
		t.Fatalf("remaining = %v, want [g1]", res.Remaining)
	}
	// Nothing booked: no partial settlement, no silent drop.
	if got := l.BalanceIn("alice", "USD"); got != 100 {
		t.Fatalf("alice = %d, want 100", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Fatalf("bob = %d, want 0", got)
	}
}

func TestGridlockResolveSimple(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100)
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "g1", From: "alice", To: "bob", AmountCents: 60})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 1 || res.SettledTotal != 60 || res.BatchID == "" {
		t.Fatalf("res = %+v, want 1 settled of 60 with a batch", res)
	}
	if len(q.Queued("USD")) != 0 {
		t.Fatalf("queue not drained: %v", q.Queued("USD"))
	}
	if got := l.BalanceIn("alice", "USD"); got != 40 {
		t.Fatalf("alice = %d, want 40", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 60 {
		t.Fatalf("bob = %d, want 60", got)
	}
	// Settled IDs are single-use: re-enqueue is rejected.
	if err := q.Enqueue(QueuedPayment{ID: "g1", From: "alice", To: "bob", AmountCents: 60}); !errors.Is(err, ErrGridlockDuplicatePayment) {
		t.Fatalf("re-enqueue settled ID: got %v, want %v", err, ErrGridlockDuplicatePayment)
	}
}

func TestGridlockResolveChainUnlocked(t *testing.T) {
	// A funds the chain; C's payment to A stays stuck because C only
	// ever receives 60 of the 100 it owes. The settled set is maximal
	// and the remainder is genuinely stuck, not merely unordered.
	l := New()
	seedBalance(t, l, "alice", 100)
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "ab", From: "alice", To: "bob", AmountCents: 60})
	enqueueGridlock(t, q, QueuedPayment{ID: "bc", From: "bob", To: "carol", AmountCents: 60})
	enqueueGridlock(t, q, QueuedPayment{ID: "ca", From: "carol", To: "alice", AmountCents: 100})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 2 || res.SettledTotal != 120 {
		t.Fatalf("settled = %v total %d, want 2 payments of 120", res.Settled, res.SettledTotal)
	}
	if len(res.Remaining) != 1 || res.Remaining[0].ID != "ca" {
		t.Fatalf("remaining = %v, want [ca]", res.Remaining)
	}
	for acct, want := range map[AccountID]int64{"alice": 40, "bob": 0, "carol": 60} {
		if got := l.BalanceIn(acct, "USD"); got != want {
			t.Fatalf("%s = %d, want %d", acct, got, want)
		}
	}
}

func TestGridlockResolveCyclePrimed(t *testing.T) {
	// The primed cycle: alice's funding lets her leg go first, which
	// funds bob's return leg. Both settle atomically; neither could
	// settle alone in the other order.
	l := New()
	seedBalance(t, l, "alice", 100)
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "ab", From: "alice", To: "bob", AmountCents: 100})
	enqueueGridlock(t, q, QueuedPayment{ID: "ba", From: "bob", To: "alice", AmountCents: 100})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 2 || len(res.Remaining) != 0 {
		t.Fatalf("res = %+v, want both settled", res)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100 {
		t.Fatalf("alice = %d, want 100", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Fatalf("bob = %d, want 0", got)
	}
}

func TestGridlockZeroBalanceCycleStaysQueued(t *testing.T) {
	// The honest limit, documented on the queue: a zero-balance cycle
	// nets to zero but no sequential leg order clears it under a zero
	// floor, so it stays queued for a funding decision — never netted
	// by pretending, never partially settled.
	l := New()
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "ab", From: "alice", To: "bob", AmountCents: 100})
	enqueueGridlock(t, q, QueuedPayment{ID: "ba", From: "bob", To: "alice", AmountCents: 100})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 0 || len(res.Remaining) != 2 {
		t.Fatalf("res = %+v, want nothing settled", res)
	}
}

func TestGridlockCurrencyIsolation(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100)
	seedBalanceIn(t, l, "alice", 100, "EUR")
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "usd1", From: "alice", To: "bob", AmountCents: 60, Currency: "USD"})
	enqueueGridlock(t, q, QueuedPayment{ID: "eur1", From: "alice", To: "bob", AmountCents: 60, Currency: "EUR"})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve USD: %v", err)
	}
	if len(res.Settled) != 1 || res.Settled[0].ID != "usd1" {
		t.Fatalf("USD settled = %v, want [usd1]", res.Settled)
	}
	if got := q.Queued("EUR"); len(got) != 1 || got[0].ID != "eur1" {
		t.Fatalf("EUR queued = %v, want [eur1]", got)
	}
	if got := l.BalanceIn("alice", "EUR"); got != 100 {
		t.Fatalf("alice EUR = %d, want 100", got)
	}
}

func TestGridlockFrozenPaymentSkipped(t *testing.T) {
	// One blocked account must not stall the rest of the queue: the
	// frozen leg stays queued, the clear leg settles.
	l := New()
	seedBalance(t, l, "alice", 100)
	l.Freeze("bob")
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "ab", From: "alice", To: "bob", AmountCents: 60})
	enqueueGridlock(t, q, QueuedPayment{ID: "ac", From: "alice", To: "carol", AmountCents: 40})

	res, err := q.Resolve(l, "USD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Settled) != 1 || res.Settled[0].ID != "ac" {
		t.Fatalf("settled = %v, want [ac]", res.Settled)
	}
	if len(res.Remaining) != 1 || res.Remaining[0].ID != "ab" {
		t.Fatalf("remaining = %v, want [ab]", res.Remaining)
	}
	if got := l.BalanceIn("alice", "USD"); got != 60 {
		t.Fatalf("alice = %d, want 60", got)
	}
}

func TestGridlockBatchFailureLeavesQueue(t *testing.T) {
	// A gate the pre-filter cannot see — here an entry-ID collision
	// with an already-journaled row — fails the whole run closed:
	// the payment stays queued and no balance moves.
	l := New()
	seedBalance(t, l, "alice", 100)
	if _, _, err := l.Post(JournalEntry{
		ID:            "gridlock:conflict",
		DebitAccount:  "funding",
		CreditAccount: "test-funding",
		AmountCents:   1,
	}); err != nil {
		t.Fatalf("seed conflicting entry: %v", err)
	}
	q := NewGridlockQueue()
	enqueueGridlock(t, q, QueuedPayment{ID: "conflict", From: "alice", To: "bob", AmountCents: 60})

	if _, err := q.Resolve(l, "USD"); !errors.Is(err, ErrBatchEntryIDConflict) {
		t.Fatalf("Resolve: got %v, want %v", err, ErrBatchEntryIDConflict)
	}
	if got := q.Queued("USD"); len(got) != 1 || got[0].ID != "conflict" {
		t.Fatalf("queued = %v, want [conflict]", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100 {
		t.Fatalf("alice = %d, want 100", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Fatalf("bob = %d, want 0", got)
	}
}
