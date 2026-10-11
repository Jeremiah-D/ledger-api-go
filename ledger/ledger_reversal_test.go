package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReverseEntryBasic(t *testing.T) {
	l := New()
	orig, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	rev, dup, err := l.ReverseEntry("e1", "")
	if err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first reversal")
	}
	if rev.ID != "e1/reversal" {
		t.Fatalf("reversal ID = %q, want e1/reversal", rev.ID)
	}
	if rev.DebitAccount != "bank" || rev.CreditAccount != "alice" {
		t.Fatalf("reversal legs = %s/%s, want bank/alice (swapped)", rev.DebitAccount, rev.CreditAccount)
	}
	if rev.AmountCents != 1000 || rev.Currency != "USD" {
		t.Fatalf("reversal amount/currency = %d %s, want 1000 USD", rev.AmountCents, rev.Currency)
	}
	if rev.Memo != "reversal_of:e1" {
		t.Fatalf("reversal memo = %q, want reversal_of:e1", rev.Memo)
	}
	// Net effect is zero: alice's balance is back where the funding left it.
	if got := l.BalanceIn("alice", "USD"); got != 0 {
		t.Fatalf("alice balance after reversal = %d, want 0", got)
	}
	if got := l.BalanceIn("bank", "USD"); got != 0 {
		t.Fatalf("bank balance after reversal = %d, want 0", got)
	}
	// The original entry is untouched.
	if again, ok := l.entries["e1"]; !ok || again.AmountCents != orig.AmountCents {
		t.Fatalf("original entry mutated: %+v", again)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestReverseEntryIdempotentDeterministicID(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 250}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	first, _, err := l.ReverseEntry("e1", "")
	if err != nil {
		t.Fatalf("first ReverseEntry: %v", err)
	}
	// Keyless repeat: the deterministic ID makes it idempotent.
	second, dup, err := l.ReverseEntry("e1", "")
	if err != nil {
		t.Fatalf("repeat ReverseEntry: %v", err)
	}
	if !dup {
		t.Fatal("duplicate = false on repeat reversal")
	}
	if second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("repeat returned a different entry: %+v vs %+v", second, first)
	}
	if n := len(l.entries); n != 2 {
		t.Fatalf("entries = %d, want 2 (original + one reversal)", n)
	}
}

func TestReverseEntryIdempotencyKeyReplay(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 250}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	first, _, err := l.ReverseEntry("e1", "rev-key-1")
	if err != nil {
		t.Fatalf("first ReverseEntry: %v", err)
	}
	// Same key: explicit replay.
	second, dup, err := l.ReverseEntry("e1", "rev-key-1")
	if err != nil {
		t.Fatalf("replay ReverseEntry: %v", err)
	}
	if !dup || second.ID != first.ID {
		t.Fatalf("replay = (%+v, dup=%v), want original reversal dup=true", second, dup)
	}
	// A *new* key for an already-reversed entry is a distinct intent: 409.
	if _, _, err := l.ReverseEntry("e1", "rev-key-2"); !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("new-key re-reverse err = %v, want ErrAlreadyReversed", err)
	}
	if n := len(l.entries); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
}

func TestReverseEntryNotFound(t *testing.T) {
	l := New()
	if _, _, err := l.ReverseEntry("nope", ""); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("err = %v, want ErrEntryNotFound", err)
	}
}

func TestReverseEntryReversesTransferLegsByEntryID(t *testing.T) {
	l := New(WithTransferFeePolicyForTest())
	// A transfer with a fee leg: reverse the principal and the fee leg
	// individually, each by its journal entry ID.
	rcpt, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", IdempotencyKey: "k-t1"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(rcpt.Entries) != 2 {
		t.Fatalf("transfer entries = %d, want 2 (principal + fee)", len(rcpt.Entries))
	}
	if _, _, err := l.ReverseEntry("t1", ""); err != nil {
		t.Fatalf("reverse principal: %v", err)
	}
	if _, _, err := l.ReverseEntry("t1/fee", ""); err != nil {
		t.Fatalf("reverse fee leg: %v", err)
	}
	if got := l.BalanceIn("payer", "USD"); got != 0 {
		t.Fatalf("payer balance = %d, want 0 after full reversal", got)
	}
	if got := l.BalanceIn("payee", "USD"); got != 0 {
		t.Fatalf("payee balance = %d, want 0 after full reversal", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestReverseEntryFrozenRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	l.Freeze("a")
	if _, _, err := l.ReverseEntry("e1", ""); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}
	if _, ok := l.entries["e1/reversal"]; ok {
		t.Fatal("reversal was recorded despite frozen rejection")
	}
}

func TestReverseEntryOverdraftRejected(t *testing.T) {
	// p is overdraft-protected. Post e1 (debit p / credit bank 100), then
	// drain p to zero via e2. Reversing e1 books debit bank / credit p
	// 100, which would take p's balance to -100: the swapped credit leg
	// is guarded exactly like a fresh posting's payer leg.
	l := New(WithOverdraftProtection("p"))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "p", CreditAccount: "bank", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "x", CreditAccount: "p", AmountCents: 100}); err != nil {
		t.Fatalf("Post e2: %v", err)
	}
	if _, _, err := l.ReverseEntry("e1", ""); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("err = %v, want ErrAccountOverdraft", err)
	}
	if _, ok := l.entries["e1/reversal"]; ok {
		t.Fatal("reversal was recorded despite overdraft rejection")
	}
}

func TestReverseEntryPeriodClosedRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	period := time.Now().UTC().Format("2006-01")
	if err := l.ClosePeriod(period); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	// The reversal is timestamped now, which falls in the closed period.
	if _, _, err := l.ReverseEntry("e1", ""); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("err = %v, want ErrPeriodClosed", err)
	}
}

func TestReverseEntryPendingReviewRejected(t *testing.T) {
	l := New(WithReviewThreshold(1)) // every transfer goes to review
	rcpt, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 5000, Currency: "USD"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.ReviewStatus != "pending_review" {
		t.Fatalf("review status = %q, want pending_review", rcpt.ReviewStatus)
	}
	if _, _, err := l.ReverseEntry("t1", ""); !errors.Is(err, ErrReversalOfPendingReview) {
		t.Fatalf("err = %v, want ErrReversalOfPendingReview", err)
	}
}

func TestReverseEntryIDConflict(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Somebody hand-posts into the reversal ID namespace.
	if _, _, err := l.Post(JournalEntry{ID: "e1/reversal", DebitAccount: "x", CreditAccount: "y", AmountCents: 5}); err != nil {
		t.Fatalf("hand post: %v", err)
	}
	if _, _, err := l.ReverseEntry("e1", ""); !errors.Is(err, ErrReversalIDConflict) {
		t.Fatalf("err = %v, want ErrReversalIDConflict", err)
	}
}

func TestReverseEntryReversingAReversal(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.ReverseEntry("e1", ""); err != nil {
		t.Fatalf("reverse e1: %v", err)
	}
	// Reversing the reversal re-books the original movement as its own
	// audited entry: net effect returns to the posted state.
	rev2, _, err := l.ReverseEntry("e1/reversal", "")
	if err != nil {
		t.Fatalf("reverse the reversal: %v", err)
	}
	if rev2.ID != "e1/reversal/reversal" {
		t.Fatalf("ID = %q, want e1/reversal/reversal", rev2.ID)
	}
	if got := l.BalanceIn("a", "USD"); got != 100 {
		t.Fatalf("a balance = %d, want 100 (original movement restored)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestReverseEntryMemoSearchable(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.ReverseEntry("e1", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	page, _, err := l.ListEntriesFiltered(time.Time{}, time.Now().Add(time.Hour), "", 10, "reversal_of:e1")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "e1/reversal" {
		t.Fatalf("memo search = %+v, want the reversal entry", page)
	}
}

func TestReverseEntryConcurrent(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	dups := make([]bool, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, dups[i], errs[i] = l.ReverseEntry("e1", "")
		}(i)
	}
	wg.Wait()
	created, replayed := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if dups[i] {
			replayed++
		} else {
			created++
		}
	}
	if created != 1 || replayed != 9 {
		t.Fatalf("created=%d replayed=%d, want exactly 1 creation", created, replayed)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestReverseEntryAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, _, err := l.ReverseEntry("e1", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
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
	var found bool
	for _, ev := range events {
		if ev.Op == "reverse" {
			found = true
			if ev.TraceID != "e1/reversal" {
				t.Fatalf("audit trace = %q", ev.TraceID)
			}
		}
	}
	if !found {
		t.Fatal("no reverse audit event found")
	}
}

// WithTransferFeePolicyForTest installs a flat 1% fee policy for tests
// that need a transfer fee leg without repeating the tier boilerplate.
func WithTransferFeePolicyForTest() Option {
	return WithTransferFeePolicy(100, "fee-revenue")
}
