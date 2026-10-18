package ledger

import (
	"errors"
	"testing"
	"time"
)

func mustPostPartialFixture(t *testing.T, l *Ledger) JournalEntry {
	t.Helper()
	e, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	return e
}

func TestReverseEntryPartialBasic(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	r, duplicate, err := l.ReverseEntryPartial(orig.ID, 300, "")
	if err != nil {
		t.Fatalf("ReverseEntryPartial: %v", err)
	}
	if duplicate {
		t.Fatal("duplicate = true on first partial reversal")
	}
	if r.ID != "e1/reversal/1" {
		t.Errorf("ID = %q, want e1/reversal/1", r.ID)
	}
	if r.DebitAccount != "equity" || r.CreditAccount != "cash" {
		t.Errorf("legs = %q/%q, want equity/cash (swapped)", r.DebitAccount, r.CreditAccount)
	}
	if r.AmountCents != 300 {
		t.Errorf("AmountCents = %d, want 300", r.AmountCents)
	}
	if r.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", r.Currency)
	}
	if r.Memo != "reversal_of:e1/partial/1" {
		t.Errorf("Memo = %q, want reversal_of:e1/partial/1", r.Memo)
	}
	// The original entry is untouched; the partial nets 300 off cash.
	if got := l.Balance("cash"); got != 700 {
		t.Errorf("Balance(cash) = %d, want 700 after 300 partial reversal", got)
	}
	if got := l.Balance("equity"); got != -700 {
		t.Errorf("Balance(equity) = %d, want -700", got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	// The original entry row is unmodified in the journal.
	if l.entries["e1"].AmountCents != 1000 {
		t.Errorf("original entry mutated: %+v", l.entries["e1"])
	}
}

func TestReverseEntryPartialCumulativeIndexes(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	for i, amt := range []int64{300, 400, 300} {
		r, _, err := l.ReverseEntryPartial(orig.ID, amt, "")
		if err != nil {
			t.Fatalf("partial %d: %v", i+1, err)
		}
		wantID := "e1/reversal/" + string(rune('1'+i))
		if r.ID != wantID {
			t.Errorf("partial %d ID = %q, want %q", i+1, r.ID, wantID)
		}
	}
	// 300+400+300 = 1000: the entry is fully netted through partials.
	if got := l.Balance("cash"); got != 0 {
		t.Errorf("Balance(cash) = %d, want 0 after full cumulative partial reversal", got)
	}
	// One cent more would exceed the original amount.
	if _, _, err := l.ReverseEntryPartial(orig.ID, 1, ""); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("over-cap partial: got %v, want ErrAlreadyReversed", err)
	}
}

func TestReverseEntryPartialOverCap(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	// A single partial larger than the original is over the cap.
	if _, _, err := l.ReverseEntryPartial(orig.ID, 1001, ""); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("single over-cap partial: got %v, want ErrAlreadyReversed", err)
	}
	// Cumulative over the cap across two partials.
	if _, _, err := l.ReverseEntryPartial(orig.ID, 600, ""); err != nil {
		t.Fatalf("first partial: %v", err)
	}
	if _, _, err := l.ReverseEntryPartial(orig.ID, 500, ""); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("cumulative over-cap partial: got %v, want ErrAlreadyReversed", err)
	}
	// The failed partial booked nothing.
	if got := l.Balance("cash"); got != 400 {
		t.Errorf("Balance(cash) = %d, want 400 (only the 600 partial booked)", got)
	}
}

func TestReverseEntryPartialIdempotent(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	r1, _, err := l.ReverseEntryPartial(orig.ID, 250, "pk1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, duplicate, err := l.ReverseEntryPartial(orig.ID, 250, "pk1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !duplicate || r2.ID != r1.ID {
		t.Errorf("replay = (%q, dup=%v), want (%q, dup=true)", r2.ID, duplicate, r1.ID)
	}
	// Keyless calls always book the next partial index: the
	// deterministic ID is per-index, so a keyless call cannot identify
	// which partial to replay — pass an idempotency key for retry
	// safety.
	r3, duplicate, err := l.ReverseEntryPartial(orig.ID, 100, "")
	if err != nil {
		t.Fatalf("keyless second partial: %v", err)
	}
	if duplicate || r3.ID != "e1/reversal/2" {
		t.Errorf("keyless = (%q, dup=%v), want (e1/reversal/2, dup=false)", r3.ID, duplicate)
	}
	// Two partials were booked: 250 + 100.
	if got := l.Balance("cash"); got != 650 {
		t.Errorf("Balance(cash) = %d, want 650 (250 + 100 partials)", got)
	}
}

func TestReverseEntryPartialValidation(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	for _, amt := range []int64{0, -100} {
		if _, _, err := l.ReverseEntryPartial(orig.ID, amt, ""); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("amount %d: got %v, want ErrNonPositiveAmount", amt, err)
		}
	}
	if _, _, err := l.ReverseEntryPartial("no-such-entry", 100, ""); !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("unknown entry: got %v, want ErrEntryNotFound", err)
	}
}

func TestReverseEntryPartialAfterFullReversal(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	if _, _, err := l.ReverseEntry(orig.ID, ""); err != nil {
		t.Fatalf("full reversal: %v", err)
	}
	// The full reversal already netted the whole entry: any partial on
	// top would double-correct.
	if _, _, err := l.ReverseEntryPartial(orig.ID, 100, ""); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("partial after full: got %v, want ErrAlreadyReversed", err)
	}
}

func TestFullReversalAfterPartialReversal(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	if _, _, err := l.ReverseEntryPartial(orig.ID, 300, ""); err != nil {
		t.Fatalf("partial: %v", err)
	}
	// A full reversal would over-correct the 300 already reversed.
	if _, _, err := l.ReverseEntry(orig.ID, "new-key"); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("full after partial: got %v, want ErrAlreadyReversed", err)
	}
	// Correcting the partial via reversal-of-reversal re-opens the
	// full reversal path: the books net back first.
	if _, _, err := l.ReverseEntry("e1/reversal/1", ""); err != nil {
		t.Fatalf("reversal of partial: %v", err)
	}
	if got := l.Balance("cash"); got != 1000 {
		t.Fatalf("Balance(cash) = %d, want 1000 after correcting the partial", got)
	}
	if _, _, err := l.ReverseEntry(orig.ID, "full-after-fix"); err != nil {
		t.Fatalf("full reversal after correcting partial: %v", err)
	}
	if got := l.Balance("cash"); got != 0 {
		t.Errorf("Balance(cash) = %d, want 0 after full reversal", got)
	}
}

func TestReverseEntryPartialRiskGates(t *testing.T) {
	// Frozen leg.
	l := New()
	orig := mustPostPartialFixture(t, l)
	l.Freeze("cash")
	if _, _, err := l.ReverseEntryPartial(orig.ID, 100, ""); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen: got %v, want ErrAccountFrozen", err)
	}

	// Suspended leg.
	l2 := New()
	orig2 := mustPostPartialFixture(t, l2)
	if _, _, err := l2.SuspendAccount("cash"); err != nil {
		t.Fatalf("SuspendAccount: %v", err)
	}
	if _, _, err := l2.ReverseEntryPartial(orig2.ID, 100, ""); !errors.Is(err, ErrAccountSuspended) {
		t.Errorf("suspended: got %v, want ErrAccountSuspended", err)
	}

	// Closed leg.
	l3 := New()
	orig3 := mustPostPartialFixture(t, l3)
	if _, _, err := l3.CloseAccount("cash", false); err != nil {
		t.Fatalf("CloseAccount: %v", err)
	}
	if _, _, err := l3.ReverseEntryPartial(orig3.ID, 100, ""); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("closed: got %v, want ErrAccountClosed", err)
	}

	// Overdraft on the swapped credit leg: the reversal pays 100 out of
	// cash, so cash is the protected payer. Drain it below the
	// reversal amount first.
	l4 := New()
	orig4 := mustPostPartialFixture(t, l4)
	if _, err := l4.PostTransfer(Transfer{ID: "drain", From: "cash", To: "other", AmountCents: 950, Currency: "USD"}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	l4.EnableOverdraftProtection("cash")
	if _, _, err := l4.ReverseEntryPartial(orig4.ID, 100, ""); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("overdraft: got %v, want ErrAccountOverdraft", err)
	}

	// Closed period: the partial reversal is timestamped now, so
	// closing the current month rejects it.
	l5 := New()
	orig5 := mustPostPartialFixture(t, l5)
	if err := l5.ClosePeriod(time.Now().UTC().Format("2006-01")); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	if _, _, err := l5.ReverseEntryPartial(orig5.ID, 100, ""); !errors.Is(err, ErrPeriodClosed) {
		t.Errorf("closed period: got %v, want ErrPeriodClosed", err)
	}
}

func TestReverseEntryPartialNamespaceConflict(t *testing.T) {
	l := New()
	orig := mustPostPartialFixture(t, l)

	// A hand-posted entry squatting in the deterministic partial
	// namespace blocks the partial reversal instead of being
	// overwritten.
	if _, _, err := l.Post(JournalEntry{ID: "e1/reversal/1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 10, Currency: "USD", Memo: "squatter"}); err != nil {
		t.Fatalf("squatter post: %v", err)
	}
	if _, _, err := l.ReverseEntryPartial(orig.ID, 100, ""); !errors.Is(err, ErrReversalIDConflict) {
		t.Errorf("namespace conflict: got %v, want ErrReversalIDConflict", err)
	}
}

func TestReverseEntryPartialOutbox(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	orig := mustPostPartialFixture(t, l)

	r, _, err := l.ReverseEntryPartial(orig.ID, 400, "")
	if err != nil {
		t.Fatalf("ReverseEntryPartial: %v", err)
	}
	evs := readAllOutbox(t, dir)
	// entry_posted (original) + entry_posted (partial) +
	// entry_reversed (partial envelope).
	if len(evs) != 3 {
		t.Fatalf("outbox events = %v, want 3", eventTypes(evs))
	}
	env := evs[2]
	if env.Type != OutboxEntryReversed || env.EntryID != r.ID {
		t.Errorf("envelope = %v %q, want entry_reversed %q", env.Type, env.EntryID, r.ID)
	}
	if env.Details["partial"] != true || env.Details["reversed_entry_id"] != "e1" {
		t.Errorf("envelope details = %v, want partial=true reversed_entry_id=e1", env.Details)
	}
}

func TestReverseEntryPartialExternalRef(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000, Currency: "USD", ExternalRef: "INV-42"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	r1, _, err := l.ReverseEntryPartial("e1", 400, "")
	if err != nil {
		t.Fatalf("partial 1: %v", err)
	}
	r2, _, err := l.ReverseEntryPartial("e1", 400, "")
	if err != nil {
		t.Fatalf("partial 2: %v", err)
	}
	// The inherited references are distinct per partial index: no
	// reference-index collision between the two partials.
	if r1.ExternalRef != "reversal_of:INV-42/partial/1" {
		t.Errorf("partial 1 ref = %q", r1.ExternalRef)
	}
	if r2.ExternalRef != "reversal_of:INV-42/partial/2" {
		t.Errorf("partial 2 ref = %q", r2.ExternalRef)
	}
}
