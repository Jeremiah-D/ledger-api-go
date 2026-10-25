package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// seedReceivable posts a sale that leaves the receivable account with
// the given USD balance: debit receivable, credit revenue.
func seedReceivable(t *testing.T, l *Ledger, account AccountID, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            "sale-" + string(account),
		DebitAccount:  account,
		CreditAccount: "revenue",
		AmountCents:   cents,
		Currency:      "USD",
	}); err != nil {
		t.Fatalf("seedReceivable: %v", err)
	}
}

func TestWriteOffBooksExpenseAgainstReceivable(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)

	posted, dup, err := l.WriteOff("receivable-acme", 40000, "USD", "invoice INV-9 uncollectible after 180 days")
	if err != nil {
		t.Fatalf("WriteOff: %v", err)
	}
	if dup {
		t.Fatal("WriteOff: first write-off reported duplicate")
	}
	if posted.DebitAccount != "bad-debt-expense" || posted.CreditAccount != "receivable-acme" {
		t.Fatalf("WriteOff legs: debit %q credit %q", posted.DebitAccount, posted.CreditAccount)
	}
	if posted.AmountCents != 40000 || posted.Currency != "USD" {
		t.Fatalf("WriteOff amount/currency: %d %q", posted.AmountCents, posted.Currency)
	}
	if posted.Memo != "invoice INV-9 uncollectible after 180 days" {
		t.Fatalf("WriteOff memo carries the reason: %q", posted.Memo)
	}
	if posted.IdempotencyKey == "" || posted.IdempotencyKey != posted.ID {
		t.Fatalf("WriteOff idempotency key must equal the entry ID: %q", posted.IdempotencyKey)
	}
	// The receivable balance drops by exactly the written-off amount.
	if got := l.BalanceIn("receivable-acme", "USD"); got != 60000 {
		t.Fatalf("receivable balance after write-off: got %d, want 60000", got)
	}
	// The expense account picks up exactly the written-off amount.
	if got := l.BalanceIn("bad-debt-expense", "USD"); got != 40000 {
		t.Fatalf("expense balance after write-off: got %d, want 40000", got)
	}
	// Accounting identity per account, and the hash chain still verifies.
	for _, a := range []AccountID{"receivable-acme", "bad-debt-expense"} {
		tb := l.TrialBalance(a)
		if tb.TotalDebits-tb.TotalCredits != tb.NetBalance {
			t.Fatalf("trial balance identity broken for %q: %+v", a, tb)
		}
		if tb.NetBalance != l.BalanceIn(a, "USD") {
			t.Fatalf("trial balance net != balance for %q: %d vs %d", a, tb.NetBalance, l.BalanceIn(a, "USD"))
		}
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after write-off: %v", err)
	}
}

func TestWriteOffPartialThenRemainder(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 50000)

	if _, _, err := l.WriteOff("receivable-acme", 20000, "USD", "partial: customer paid 30000, rest disputed"); err != nil {
		t.Fatalf("partial WriteOff: %v", err)
	}
	if got := l.BalanceIn("receivable-acme", "USD"); got != 30000 {
		t.Fatalf("balance after partial: got %d, want 30000", got)
	}
	// The remainder is a second write-off with its own reason.
	if _, _, err := l.WriteOff("receivable-acme", 30000, "USD", "remainder: dispute lost, uncollectible"); err != nil {
		t.Fatalf("remainder WriteOff: %v", err)
	}
	if got := l.BalanceIn("receivable-acme", "USD"); got != 0 {
		t.Fatalf("balance after remainder: got %d, want 0", got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestWriteOffReplayIsIdempotent(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)

	first, _, err := l.WriteOff("receivable-acme", 40000, "USD", "invoice INV-9 uncollectible")
	if err != nil {
		t.Fatalf("first WriteOff: %v", err)
	}
	versionAfterFirst := l.Version()
	second, dup, err := l.WriteOff("receivable-acme", 40000, "USD", "invoice INV-9 uncollectible")
	if err != nil {
		t.Fatalf("replay WriteOff: %v", err)
	}
	if !dup {
		t.Fatal("replay WriteOff: duplicate not reported")
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned a different entry: %q vs %q", second.ID, first.ID)
	}
	if l.Version() != versionAfterFirst {
		t.Fatal("replay bumped the journal version")
	}
	if got := l.BalanceIn("receivable-acme", "USD"); got != 60000 {
		t.Fatalf("replay double-expensed: balance %d, want 60000", got)
	}
}

func TestWriteOffReplaySurvivesBalanceDrop(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)

	if _, _, err := l.WriteOff("receivable-acme", 60000, "USD", "invoice INV-9 uncollectible"); err != nil {
		t.Fatalf("first WriteOff: %v", err)
	}
	// A later payment drops the balance below the original amount;
	// the replay must still return the original entry, not fail.
	if _, _, err := l.Post(JournalEntry{
		ID:            "payment-1",
		DebitAccount:  "cash",
		CreditAccount: "receivable-acme",
		AmountCents:   30000,
		Currency:      "USD",
	}); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if got := l.BalanceIn("receivable-acme", "USD"); got != 10000 {
		t.Fatalf("balance before replay: got %d, want 10000", got)
	}
	_, dup, err := l.WriteOff("receivable-acme", 60000, "USD", "invoice INV-9 uncollectible")
	if err != nil {
		t.Fatalf("replay after balance drop: %v", err)
	}
	if !dup {
		t.Fatal("replay after balance drop: duplicate not reported")
	}
}

func TestWriteOffValidation(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)

	cases := []struct {
		name     string
		account  AccountID
		amount   int64
		currency string
		reason   string
		wantErr  error
	}{
		{"empty account", "", 1000, "USD", "reason", ErrInvalidWriteOffAccount},
		{"account with separator", "a/b", 1000, "USD", "reason", ErrInvalidWriteOffAccount},
		{"zero amount", "receivable-acme", 0, "USD", "reason", ErrInvalidWriteOffAmount},
		{"negative amount", "receivable-acme", -1, "USD", "reason", ErrInvalidWriteOffAmount},
		{"bad currency", "receivable-acme", 1000, "usd", "reason", ErrInvalidCurrency},
		{"empty reason", "receivable-acme", 1000, "USD", "", ErrInvalidWriteOffReason},
		{"overlong reason", "receivable-acme", 1000, "USD", strings.Repeat("x", 256), ErrInvalidWriteOffReason},
		{"exceeds balance", "receivable-acme", 100001, "USD", "reason", ErrWriteOffExceedsReceivable},
		{"zero balance account", "receivable-nothing", 1000, "USD", "reason", ErrWriteOffExceedsReceivable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := l.WriteOff(tc.account, tc.amount, tc.currency, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteOff error: got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestWriteOffRequiresExpenseAccount(t *testing.T) {
	l := New()
	seedReceivable(t, l, "receivable-acme", 100000)
	_, _, err := l.WriteOff("receivable-acme", 1000, "USD", "uncollectible")
	if !errors.Is(err, ErrBadDebtExpenseNotConfigured) {
		t.Fatalf("WriteOff without expense account: got %v, want %v", err, ErrBadDebtExpenseNotConfigured)
	}
	// Configuring it afterwards unblocks the same write-off.
	l.SetBadDebtExpenseAccount("bad-debt-expense")
	if _, _, err := l.WriteOff("receivable-acme", 1000, "USD", "uncollectible"); err != nil {
		t.Fatalf("WriteOff after configuring: %v", err)
	}
	if got := l.BadDebtExpenseAccount(); got != "bad-debt-expense" {
		t.Fatalf("BadDebtExpenseAccount: got %q", got)
	}
}

func TestWriteOffRejectsSelfExpense(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("receivable-acme"))
	seedReceivable(t, l, "receivable-acme", 100000)
	_, _, err := l.WriteOff("receivable-acme", 1000, "USD", "uncollectible")
	if !errors.Is(err, ErrWriteOffSameAccount) {
		t.Fatalf("WriteOff into itself: got %v, want %v", err, ErrWriteOffSameAccount)
	}
}

func TestWriteOffDeterministicKeyDiffersByReason(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)

	first, _, err := l.WriteOff("receivable-acme", 10000, "USD", "invoice INV-9")
	if err != nil {
		t.Fatalf("first WriteOff: %v", err)
	}
	// Same amount, different reason: a genuinely different write-off.
	second, dup, err := l.WriteOff("receivable-acme", 10000, "USD", "invoice INV-10")
	if err != nil {
		t.Fatalf("second WriteOff: %v", err)
	}
	if dup {
		t.Fatal("different reason reported as duplicate")
	}
	if second.ID == first.ID {
		t.Fatal("different reasons produced the same entry ID")
	}
	if got := l.BalanceIn("receivable-acme", "USD"); got != 80000 {
		t.Fatalf("balance: got %d, want 80000", got)
	}
}

func TestWriteOffSnapshotRoundTrip(t *testing.T) {
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)
	if _, _, err := l.WriteOff("receivable-acme", 25000, "USD", "uncollectible"); err != nil {
		t.Fatalf("WriteOff: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.BadDebtExpenseAccount(); got != "bad-debt-expense" {
		t.Fatalf("restored expense account: got %q", got)
	}
	// The restored ledger still refuses a replay-turned-double-spend:
	// identical inputs replay the restored entry.
	_, dup, err := restored.WriteOff("receivable-acme", 25000, "USD", "uncollectible")
	if err != nil {
		t.Fatalf("restored WriteOff replay: %v", err)
	}
	if !dup {
		t.Fatal("restored WriteOff replay: duplicate not reported")
	}
	if got := restored.BalanceIn("receivable-acme", "USD"); got != 75000 {
		t.Fatalf("restored balance: got %d, want 75000", got)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on restored: %v", err)
	}
}
