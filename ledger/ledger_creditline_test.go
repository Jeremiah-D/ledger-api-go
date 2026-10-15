package ledger

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGrantCreditLine_HappyPath(t *testing.T) {
	l := New()
	cl, err := l.GrantCreditLine("trader", 50_000, "USD", 800)
	if err != nil {
		t.Fatalf("GrantCreditLine: %v", err)
	}
	if cl.Account != "trader" || cl.Currency != "USD" || cl.LimitCents != 50_000 || cl.RateBps != 800 {
		t.Errorf("credit line = %+v, want trader/USD/50000/800", cl)
	}
	if cl.GrantedAt.IsZero() {
		t.Error("GrantedAt is zero")
	}
	got, ok := l.GetCreditLine("trader", "USD")
	if !ok || got.LimitCents != 50_000 {
		t.Errorf("GetCreditLine = %+v ok=%v", got, ok)
	}
	// Empty currency normalizes to the default.
	cl2, err := l.GrantCreditLine("trader2", 10_000, "", 100)
	if err != nil || cl2.Currency != "USD" {
		t.Errorf("empty currency: %+v err=%v, want USD", cl2, err)
	}
}

func TestGrantCreditLine_Validation(t *testing.T) {
	l := New()
	cases := []struct {
		name            string
		account         AccountID
		limit, rate     int64
		currency        string
		want            error
	}{
		{"empty account", "", 1000, 100, "USD", ErrEmptyCreditLineAccount},
		{"slash account", "a/b", 1000, 100, "USD", ErrCreditLineAccountSeparator},
		{"zero limit", "x", 0, 100, "USD", ErrCreditLineNonPositiveLimit},
		{"negative limit", "x", -5, 100, "USD", ErrCreditLineNonPositiveLimit},
		{"zero rate", "x", 1000, 0, "USD", ErrInvalidCreditLineRate},
		{"rate over 100%", "x", 1000, 10001, "USD", ErrInvalidCreditLineRate},
		{"bad currency", "x", 1000, 100, "usd", ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.GrantCreditLine(tc.account, tc.limit, tc.currency, tc.rate); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// Re-granting is create-only.
	if _, err := l.GrantCreditLine("dup", 1000, "USD", 100); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if _, err := l.GrantCreditLine("dup", 2000, "USD", 200); !errors.Is(err, ErrCreditLineExists) {
		t.Errorf("re-grant err = %v, want ErrCreditLineExists", err)
	}
}

func TestReduceCreditLine(t *testing.T) {
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Draw 30_000 of the line: trader pays out 130_000, balance -30_000.
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 130_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	// Shrinking below the 30_000 utilization is rejected.
	if _, err := l.ReduceCreditLine("trader", "USD", 20_000); !errors.Is(err, ErrCreditLineBelowUtilization) {
		t.Errorf("shrink-below-use err = %v, want ErrCreditLineBelowUtilization", err)
	}
	// Shrinking to exactly the utilization is allowed.
	cl, err := l.ReduceCreditLine("trader", "USD", 30_000)
	if err != nil || cl.LimitCents != 30_000 {
		t.Errorf("reduce to utilization: %+v err=%v", cl, err)
	}
	// Unknown line.
	if _, err := l.ReduceCreditLine("nobody", "USD", 1000); !errors.Is(err, ErrCreditLineNotFound) {
		t.Errorf("unknown reduce err = %v, want ErrCreditLineNotFound", err)
	}
	// Repay, then revoke by reducing to zero.
	if _, _, err := l.Post(JournalEntry{
		ID: "repay", DebitAccount: "trader", CreditAccount: "vendor", AmountCents: 30_000,
	}); err != nil {
		t.Fatalf("repay: %v", err)
	}
	if _, err := l.ReduceCreditLine("trader", "USD", 0); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := l.GetCreditLine("trader", "USD"); ok {
		t.Error("line still present after revoke")
	}
}

func TestCreditLineUtilization(t *testing.T) {
	l := New()
	seedBalance(t, l, "trader", 20_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	used, avail, limit, ok := l.CreditLineUtilization("trader", "")
	if !ok || used != 0 || avail != 50_000 || limit != 50_000 {
		t.Errorf("utilization = (%d, %d, %d, %v), want (0, 50000, 50000, true)", used, avail, limit, ok)
	}
	// Draw into the line.
	if _, _, err := l.Post(JournalEntry{
		ID: "d1", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 35_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	used, avail, _, ok = l.CreditLineUtilization("trader", "USD")
	if !ok || used != 15_000 || avail != 35_000 {
		t.Errorf("utilization = (%d, %d), want (15000, 35000)", used, avail)
	}
	// No line: ok=false.
	if _, _, _, ok := l.CreditLineUtilization("nobody", "USD"); ok {
		t.Error("utilization ok for unknown line")
	}
}

func TestCreditLine_OverdraftInterplay(t *testing.T) {
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Within the line: the -limit floor replaces the zero floor.
	if _, _, err := l.Post(JournalEntry{
		ID: "w1", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Fatalf("within-line post: %v", err)
	}
	if b := l.Balance("trader"); b != -40_000 {
		t.Errorf("balance = %d, want -40000", b)
	}
	// Breaching the floor: ErrCreditLimitExceeded, not ErrAccountOverdraft.
	_, _, err := l.Post(JournalEntry{
		ID: "w2", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 10_001,
	})
	if !errors.Is(err, ErrCreditLimitExceeded) {
		t.Errorf("breach err = %v, want ErrCreditLimitExceeded", err)
	}
	if errors.Is(err, ErrAccountOverdraft) {
		t.Error("breach surfaced as ErrAccountOverdraft, want the credit-line error")
	}
	// Exactly at the floor is allowed: balance - amount == floor passes
	// the strict < comparison.
	if _, _, err := l.Post(JournalEntry{
		ID: "w3", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 10_000,
	}); err != nil {
		t.Errorf("at-floor post: %v", err)
	}
	if b := l.Balance("trader"); b != -50_000 {
		t.Errorf("balance = %d, want -50000", b)
	}
	// A protected account without a line keeps the zero floor.
	l2 := New(WithOverdraftProtection("plain"))
	seedBalance(t, l2, "plain", 100_000)
	_, _, err = l2.Post(JournalEntry{
		ID: "w4", DebitAccount: "vendor", CreditAccount: "plain", AmountCents: 100_001,
	})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("no-line breach err = %v, want ErrAccountOverdraft", err)
	}
	// The floor is per-currency: a USD line does not cover EUR.
	l3 := New(WithOverdraftProtection("fx"))
	seedBalance(t, l3, "fx", 100_000)
	if _, err := l3.GrantCreditLine("fx", 50_000, "USD", 100); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, _, err = l3.Post(JournalEntry{
		ID: "w5", DebitAccount: "vendor", CreditAccount: "fx", AmountCents: 1, Currency: "EUR",
	})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("EUR post err = %v, want ErrAccountOverdraft (no EUR line)", err)
	}
}

func TestCreditLine_FrozenStillBlocks(t *testing.T) {
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	l.Freeze("trader")
	// Frozen still fully blocks: the credit line never un-freezes an
	// account.
	_, _, err := l.Post(JournalEntry{
		ID: "f1", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 10_000,
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen post err = %v, want ErrAccountFrozen", err)
	}
}

func TestAccrueCreditInterest_HappyPath(t *testing.T) {
	l := New(WithCreditInterestIncome("interest-income"), WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Draw 40_000: balance -40_000, used = 40_000.
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	e, dup, err := l.AccrueCreditInterest("trader", "USD", asof)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false")
	}
	// floor(40_000 * 800 / 10000 / 365) = floor(3200/365) = 8.
	if e.AmountCents != 8 {
		t.Errorf("interest = %d, want 8", e.AmountCents)
	}
	if e.ID != "credit-interest/trader/USD/2026-04-02" {
		t.Errorf("entry ID = %q, want deterministic ID", e.ID)
	}
	// The income account is debited (grows); the borrower is credited
	// (owes more).
	if e.DebitAccount != "interest-income" || e.CreditAccount != "trader" {
		t.Errorf("legs = debit %q credit %q, want interest-income / trader",
			e.DebitAccount, e.CreditAccount)
	}
	if b := l.Balance("trader"); b != -40_008 {
		t.Errorf("trader balance = %d, want -40008", b)
	}
	// Same day replays.
	if _, dup, err := l.AccrueCreditInterest("trader", "USD", asof); err != nil || !dup {
		t.Errorf("replay: err=%v dup=%v", err, dup)
	}
}

func TestAccrueCreditInterest_Errors(t *testing.T) {
	t.Run("unconfigured income account", func(t *testing.T) {
		l := New(WithOverdraftProtection("trader"))
		seedBalance(t, l, "trader", 100_000)
		if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if _, _, err := l.Post(JournalEntry{
			ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
		}); err != nil {
			t.Fatalf("draw: %v", err)
		}
		if _, _, err := l.AccrueCreditInterest("trader", "USD", time.Now()); !errors.Is(err, ErrCreditInterestIncomeNotConfigured) {
			t.Errorf("err = %v, want fail-closed", err)
		}
	})
	t.Run("no utilization", func(t *testing.T) {
		l := New(WithCreditInterestIncome("interest-income"))
		seedBalance(t, l, "trader", 100_000)
		if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if _, _, err := l.AccrueCreditInterest("trader", "USD", time.Now()); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("err = %v, want ErrNonPositiveAmount", err)
		}
	})
	t.Run("unknown line", func(t *testing.T) {
		l := New(WithCreditInterestIncome("interest-income"))
		if _, _, err := l.AccrueCreditInterest("nobody", "USD", time.Now()); !errors.Is(err, ErrCreditLineNotFound) {
			t.Errorf("err = %v, want ErrCreditLineNotFound", err)
		}
	})
	t.Run("interest breaching the floor", func(t *testing.T) {
		l := New(WithCreditInterestIncome("interest-income"), WithOverdraftProtection("trader"))
		seedBalance(t, l, "trader", 100_000)
		// Line of 1000 at 100% annual: draw to exactly -1000, then the
		// interest charge itself would breach the floor, so it is
		// rejected with ErrCreditLimitExceeded instead of silently
		// pushing past the line.
		if _, err := l.GrantCreditLine("trader", 1000, "USD", 10000); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if _, _, err := l.Post(JournalEntry{
			ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 101_000,
		}); err != nil {
			t.Fatalf("draw: %v", err)
		}
		// used = 1000, interest = floor(1000*10000/10000/365) = 2;
		// balance -1000, floor -1000: -1000 < 2 + (-1000) = -998.
		if _, _, err := l.AccrueCreditInterest("trader", "USD", time.Now()); !errors.Is(err, ErrCreditLimitExceeded) {
			t.Errorf("err = %v, want ErrCreditLimitExceeded", err)
		}
	})
}

func TestCreditLine_ConcurrentGrants(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.GrantCreditLine("racer", 10_000, "USD", 100)
		}(i)
	}
	wg.Wait()
	var granted int
	for _, err := range errs {
		if err == nil {
			granted++
		} else if !errors.Is(err, ErrCreditLineExists) {
			t.Errorf("unexpected err: %v", err)
		}
	}
	if granted != 1 {
		t.Errorf("granted = %d, want exactly 1", granted)
	}
}

func TestCreditLine_SnapshotRoundTrip(t *testing.T) {
	src := New(
		WithCreditInterestIncome("interest-income"),
		WithOverdraftProtection("trader"),
	)
	seedBalance(t, src, "trader", 100_000)
	if _, err := src.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("snapshotLedgersEqual = false after round trip")
	}
	cl, ok := restored.GetCreditLine("trader", "USD")
	if !ok || cl.LimitCents != 50_000 || cl.RateBps != 800 {
		t.Errorf("restored line = %+v ok=%v", cl, ok)
	}
	// The floor survives the restore: within-line post still clears.
	if _, _, err := restored.Post(JournalEntry{
		ID: "post-restore", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Errorf("post-restore within-line post: %v", err)
	}
}

func TestCreditLine_ReconcileDisclosure(t *testing.T) {
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 130_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.CreditLines) != 1 {
		t.Fatalf("credit_lines = %d rows, want 1", len(rep.CreditLines))
	}
	c := rep.CreditLines[0]
	if c.Account != "trader" || c.LimitCents != 50_000 || c.UsedCents != 30_000 || c.AvailableCents != 20_000 {
		t.Errorf("report = %+v, want limit 50000 / used 30000 / available 20000", c)
	}
	if !rep.AccountingEquationOK {
		t.Error("accounting equation broken by credit-line activity")
	}
}
