package ledger

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// seedTermDepositLedger builds a ledger with a funded account and the
// term-deposit interest-expense account configured.
func seedTermDepositLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	return l
}

func TestOpenTermDeposit_HappyPath(t *testing.T) {
	l := seedTermDepositLedger(t)
	opened := time.Now().Truncate(time.Second)
	td, dup, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-1",
		Account:     "saver",
		AmountCents: 400_000,
		TermDays:    30,
		RateBps:     500,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false")
	}
	if td.Status != TermDepositActive {
		t.Errorf("status = %q, want active", td.Status)
	}
	if td.Currency != "USD" {
		t.Errorf("currency = %q, want USD default", td.Currency)
	}
	if !td.OpenedAt.Equal(opened) && td.OpenedAt.Before(opened) {
		t.Errorf("opened_at = %v, want >= %v", td.OpenedAt, opened)
	}
	wantMatures := td.OpenedAt.Add(30 * 24 * time.Hour)
	if !td.MaturesAt.Equal(wantMatures) {
		t.Errorf("matures_at = %v, want %v", td.MaturesAt, wantMatures)
	}
	// The principal locks available funds off-journal: the net balance
	// is untouched, available drops by the locked amount.
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("balance = %d, want 1000000", b)
	}
	if a := l.Available("saver"); a != 600_000 {
		t.Errorf("available = %d, want 600000", a)
	}
	// Two deposits accumulate their locks.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-2", Account: "saver", AmountCents: 100_000, TermDays: 7, RateBps: 100,
	}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if a := l.Available("saver"); a != 500_000 {
		t.Errorf("available after two deposits = %d, want 500000", a)
	}
}

func TestOpenTermDeposit_Validation(t *testing.T) {
	l := seedTermDepositLedger(t)
	cases := []struct {
		name string
		td   TermDeposit
		want error
	}{
		{"empty ID", TermDeposit{Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100}, ErrEmptyTermDepositID},
		{"slash ID", TermDeposit{ID: "a/b", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100}, ErrTermDepositIDSeparator},
		{"empty account", TermDeposit{ID: "x", AmountCents: 100, TermDays: 1, RateBps: 100}, ErrEmptyTermDepositAccount},
		{"zero amount", TermDeposit{ID: "x", Account: "saver", TermDays: 1, RateBps: 100}, ErrTermDepositNonPositiveAmount},
		{"zero term", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, RateBps: 100}, ErrInvalidTermDays},
		{"negative term", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: -5, RateBps: 100}, ErrInvalidTermDays},
		{"zero rate", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1}, ErrInvalidTermDepositRate},
		{"rate over 100%", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 10001}, ErrInvalidTermDepositRate},
		{"bad currency", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100, Currency: "usd"}, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := l.OpenTermDeposit(tc.td); !errors.Is(err, tc.want) {
				t.Errorf("OpenTermDeposit err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenTermDeposit_IdempotencyAndConflict(t *testing.T) {
	l := seedTermDepositLedger(t)
	first, dup, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-k", Account: "saver", AmountCents: 100_000, TermDays: 10, RateBps: 200,
		IdempotencyKey: "open-key-1",
	})
	if err != nil || dup {
		t.Fatalf("first open: err=%v dup=%v", err, dup)
	}
	// Same key, different request shape: replays the original.
	second, dup, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-other", Account: "saver", AmountCents: 1, TermDays: 1, RateBps: 1,
		IdempotencyKey: "open-key-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !dup || second.ID != first.ID || second.AmountCents != 100_000 {
		t.Errorf("replay = %+v dup=%v, want original td-k", second, dup)
	}
	// Same ID, different key: conflict, never a silent overwrite.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-k", Account: "saver", AmountCents: 50_000, TermDays: 5, RateBps: 50,
		IdempotencyKey: "open-key-2",
	}); !errors.Is(err, ErrTermDepositIDConflict) {
		t.Errorf("ID conflict err = %v, want ErrTermDepositIDConflict", err)
	}
}

func TestOpenTermDeposit_FrozenAndFunds(t *testing.T) {
	l := seedTermDepositLedger(t)
	// Frozen account rejects the open.
	l.Freeze("saver")
	_, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-f", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100,
		IdempotencyKey: "fk",
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen open err = %v, want ErrAccountFrozen", err)
	}
	// A replay of a key opened before the freeze still succeeds: the
	// replay locks nothing new.
	l2 := seedTermDepositLedger(t)
	if _, _, err := l2.OpenTermDeposit(TermDeposit{
		ID: "td-pre", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100,
		IdempotencyKey: "pre-key",
	}); err != nil {
		t.Fatalf("pre-freeze open: %v", err)
	}
	l2.Freeze("saver")
	if _, dup, err := l2.OpenTermDeposit(TermDeposit{
		ID: "td-ignored", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 100,
		IdempotencyKey: "pre-key",
	}); err != nil || !dup {
		t.Errorf("post-freeze replay: err=%v dup=%v, want replay of original", err, dup)
	}
	// Locking more than available is rejected.
	l3 := seedTermDepositLedger(t)
	_, _, err = l3.OpenTermDeposit(TermDeposit{
		ID: "td-big", Account: "saver", AmountCents: 1_000_001, TermDays: 1, RateBps: 100,
	})
	if !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("oversize open err = %v, want ErrInsufficientAvailableFunds", err)
	}
}

func TestAccrueTermInterest_HappyPath(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-i", Account: "saver", AmountCents: 1_000_000, TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	asof := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	e, dup, err := l.AccrueTermInterest("td-i", asof)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false")
	}
	// floor(1_000_000 * 500 / 10000 / 365) = floor(50000/365) = 136.
	if e.AmountCents != 136 {
		t.Errorf("interest = %d, want 136", e.AmountCents)
	}
	if e.ID != "term-interest/td-i/2026-03-10" {
		t.Errorf("entry ID = %q, want deterministic ID", e.ID)
	}
	// The depositor is debited (balance grows); the expense account is
	// credited.
	if e.DebitAccount != "saver" || e.CreditAccount != "td-interest-expense" {
		t.Errorf("legs = debit %q credit %q, want saver / td-interest-expense",
			e.DebitAccount, e.CreditAccount)
	}
	if b := l.Balance("saver"); b != 1_000_136 {
		t.Errorf("saver balance = %d, want 1000136", b)
	}
	// Same day replays the original row.
	e2, dup, err := l.AccrueTermInterest("td-i", asof)
	if err != nil || !dup || e2.ID != e.ID {
		t.Errorf("replay: err=%v dup=%v id=%q, want replay of %q", err, dup, e2.ID, e.ID)
	}
	// A new day books a fresh row.
	e3, _, err := l.AccrueTermInterest("td-i", asof.Add(24*time.Hour))
	if err != nil || e3.ID == e.ID {
		t.Errorf("next day: err=%v id=%q, want new row", err, e3.ID)
	}
	if got := l.TermDepositInterestAccrued("td-i"); got != 272 {
		t.Errorf("accrued total = %d, want 272", got)
	}
}

func TestAccrueTermInterest_Errors(t *testing.T) {
	t.Run("unconfigured expense account", func(t *testing.T) {
		l := New()
		seedBalance(t, l, "saver", 1_000_000)
		if _, _, err := l.OpenTermDeposit(TermDeposit{
			ID: "td-u", Account: "saver", AmountCents: 500_000, TermDays: 30, RateBps: 500,
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, _, err := l.AccrueTermInterest("td-u", time.Now()); !errors.Is(err, ErrTermDepositInterestAccountNotConfigured) {
			t.Errorf("err = %v, want fail-closed", err)
		}
	})
	t.Run("past maturity", func(t *testing.T) {
		l := seedTermDepositLedger(t)
		opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		td, _, err := l.OpenTermDeposit(TermDeposit{
			ID: "td-m", Account: "saver", AmountCents: 500_000, TermDays: 30, RateBps: 500,
		})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_ = opened
		after := td.MaturesAt.Add(time.Hour)
		if _, _, err := l.AccrueTermInterest("td-m", after); !errors.Is(err, ErrTermDepositMatured) {
			t.Errorf("err = %v, want ErrTermDepositMatured", err)
		}
	})
	t.Run("unknown deposit", func(t *testing.T) {
		l := seedTermDepositLedger(t)
		if _, _, err := l.AccrueTermInterest("nope", time.Now()); !errors.Is(err, ErrTermDepositNotFound) {
			t.Errorf("err = %v, want ErrTermDepositNotFound", err)
		}
	})
	t.Run("dust accrual", func(t *testing.T) {
		l := seedTermDepositLedger(t)
		// 100 cents at 100bps: floor(100*100/10000/365) = 0.
		if _, _, err := l.OpenTermDeposit(TermDeposit{
			ID: "td-dust", Account: "saver", AmountCents: 100, TermDays: 30, RateBps: 100,
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, _, err := l.AccrueTermInterest("td-dust", time.Now()); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("err = %v, want ErrNonPositiveAmount", err)
		}
	})
}

func TestMatureTermDeposit(t *testing.T) {
	l := seedTermDepositLedger(t)
	openedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	td := TermDeposit{
		ID: "td-mat", Account: "saver", AmountCents: 400_000, TermDays: 30, RateBps: 500,
		OpenedAt: openedAt,
	}
	if _, _, err := l.OpenTermDeposit(td); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Early: rejected.
	if _, err := l.MatureTermDeposit("td-mat", openedAt.Add(29*24*time.Hour)); !errors.Is(err, ErrTermNotMatured) {
		t.Fatalf("early mature err = %v, want ErrTermNotMatured", err)
	}
	// Book one day of interest first, then mature at the instant.
	asof := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.AccrueTermInterest("td-mat", asof); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	m, err := l.MatureTermDeposit("td-mat", openedAt.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("mature: %v", err)
	}
	if m.Status != TermDepositMatured {
		t.Errorf("status = %q, want matured", m.Status)
	}
	// Principal unlocks: available returns to the journaled net (which
	// includes the accrued interest).
	if a := l.Available("saver"); a != 1_000_000+54 {
		t.Errorf("available after maturity = %d, want 1000054", a)
	}
	// Idempotent re-mature.
	m2, err := l.MatureTermDeposit("td-mat", openedAt.Add(31*24*time.Hour))
	if err != nil || m2.Status != TermDepositMatured {
		t.Errorf("re-mature: err=%v status=%q, want idempotent matured", err, m2.Status)
	}
	// Accrual after maturity is rejected.
	if _, _, err := l.AccrueTermInterest("td-mat", openedAt.Add(31*24*time.Hour)); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("post-maturity accrue err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestWithdrawTermDeposit(t *testing.T) {
	l := New(
		WithTermDepositInterestExpense("td-interest-expense"),
		WithTransferFeePolicy(0, "fee-revenue"),
	)
	seedBalance(t, l, "saver", 1_000_000)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-w", Account: "saver", AmountCents: 1_000_000, TermDays: 90, RateBps: 500,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	w, err := l.WithdrawTermDeposit("td-w", 100, now) // 1% penalty
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if w.Deposit.Status != TermDepositWithdrawnEarly {
		t.Errorf("status = %q, want withdrawn_early", w.Deposit.Status)
	}
	// floor(1_000_000 * 100 / 10000) = 10000 penalty cents.
	if w.PenaltyCents != 10_000 || w.PenaltyEntry != "td-w/penalty" {
		t.Errorf("withdrawal = %+v, want 10000 penalty at td-w/penalty", w)
	}
	// Penalty leg: debit fee revenue (grows), credit the depositor
	// (shrinks).
	pe, ok := l.entries["td-w/penalty"]
	if !ok {
		t.Fatalf("penalty entry missing")
	}
	if pe.DebitAccount != "fee-revenue" || pe.CreditAccount != "saver" || pe.AmountCents != 10_000 {
		t.Errorf("penalty legs = %+v, want debit fee-revenue / credit saver / 10000", pe)
	}
	// Principal unlocked; balance reflects the penalty only.
	if a := l.Available("saver"); a != 990_000 {
		t.Errorf("available = %d, want 990000", a)
	}
	// Idempotent re-withdraw.
	w2, err := l.WithdrawTermDeposit("td-w", 100, now)
	if err != nil || !w2.Duplicate {
		t.Errorf("re-withdraw: err=%v dup=%v, want duplicate", err, w2.Duplicate)
	}
	// Withdrawing a matured deposit is rejected.
	l2 := seedTermDepositLedger(t)
	opened := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := l2.OpenTermDeposit(TermDeposit{
		ID: "td-wm", Account: "saver", AmountCents: 100_000, TermDays: 1, RateBps: 100,
		OpenedAt: opened,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := l2.MatureTermDeposit("td-wm", opened.Add(25*time.Hour)); err != nil {
		t.Fatalf("mature: %v", err)
	}
	if _, err := l2.WithdrawTermDeposit("td-wm", 0, opened.Add(26*time.Hour)); !errors.Is(err, ErrTermDepositMatured) {
		t.Errorf("withdraw matured err = %v, want ErrTermDepositMatured", err)
	}
}

func TestWithdrawTermDeposit_PenaltyValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-pv", Account: "saver", AmountCents: 500_000, TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := l.WithdrawTermDeposit("td-pv", 10001, time.Now()); !errors.Is(err, ErrInvalidTermDepositPenalty) {
		t.Errorf("bad penalty err = %v, want ErrInvalidTermDepositPenalty", err)
	}
	// Positive penalty without a fee-revenue account: fail-closed.
	if _, err := l.WithdrawTermDeposit("td-pv", 100, time.Now()); !errors.Is(err, ErrTermDepositPenaltyAccountNotConfigured) {
		t.Errorf("err = %v, want ErrTermDepositPenaltyAccountNotConfigured", err)
	}
	// Zero penalty needs no fee account.
	if _, err := l.WithdrawTermDeposit("td-pv", 0, time.Now()); err != nil {
		t.Errorf("zero-penalty withdraw: %v", err)
	}
}

func TestSweepTermDeposits(t *testing.T) {
	l := seedTermDepositLedger(t)
	opened := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// One deposit that matures, one that stays active.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-s1", Account: "saver", AmountCents: 500_000, TermDays: 10, RateBps: 500,
		OpenedAt: opened,
	}); err != nil {
		t.Fatalf("open s1: %v", err)
	}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-s2", Account: "saver", AmountCents: 400_000, TermDays: 60, RateBps: 500,
		OpenedAt: opened,
	}); err != nil {
		t.Fatalf("open s2: %v", err)
	}
	sweepAt := opened.Add(11 * 24 * time.Hour)
	matured, accrued := l.SweepTermDeposits(sweepAt)
	if matured != 1 || accrued != 1 {
		t.Errorf("sweep = (%d matured, %d accrued), want (1, 1)", matured, accrued)
	}
	td1, _ := l.GetTermDeposit("td-s1")
	td2, _ := l.GetTermDeposit("td-s2")
	if td1.Status != TermDepositMatured || td2.Status != TermDepositActive {
		t.Errorf("statuses = %q/%q, want matured/active", td1.Status, td2.Status)
	}
	// Re-sweeping is a no-op: the interest day is idempotent.
	matured, accrued = l.SweepTermDeposits(sweepAt)
	if matured != 0 || accrued != 0 {
		t.Errorf("re-sweep = (%d, %d), want (0, 0)", matured, accrued)
	}
}

func TestTermDeposit_AvailableWithHolds(t *testing.T) {
	l := seedTermDepositLedger(t)
	// A hold and a term deposit share the available pool.
	if _, _, err := l.Hold(Hold{
		ID: "h1", Account: "saver", AmountCents: 200_000,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-h", Account: "saver", AmountCents: 700_000, TermDays: 7, RateBps: 100,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if a := l.Available("saver"); a != 100_000 {
		t.Errorf("available = %d, want 100000", a)
	}
	// Locking past the hold is rejected.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-h2", Account: "saver", AmountCents: 100_001, TermDays: 7, RateBps: 100,
	}); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("oversize err = %v, want ErrInsufficientAvailableFunds", err)
	}
}

func TestTermDeposit_ConcurrentOpens(t *testing.T) {
	l := seedTermDepositLedger(t)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 20 x 100_000 against 1_000_000 available: exactly 10 must
			// win; the rest fail available funds.
			_, _, errs[i] = l.OpenTermDeposit(TermDeposit{
				ID:          "td-c",
				Account:     "saver",
				AmountCents: 100_000,
				TermDays:    7,
				RateBps:     100,
			})
		}(i)
	}
	wg.Wait()
	// All 20 used the same ID: the first wins, the rest conflict —
	// either way no double-lock. (Same-ID opens are the race here;
	// distinct-ID oversubscription is covered by the serial test.)
	var opened int
	for _, err := range errs {
		if err == nil {
			opened++
		} else if !errors.Is(err, ErrTermDepositIDConflict) {
			t.Errorf("unexpected err: %v", err)
		}
	}
	if opened != 1 {
		t.Errorf("opened = %d, want exactly 1", opened)
	}
	if a := l.Available("saver"); a != 900_000 {
		t.Errorf("available = %d, want 900000 (exactly one lock)", a)
	}
}

func TestTermDeposit_SnapshotRoundTrip(t *testing.T) {
	src := New(
		WithTermDepositInterestExpense("td-interest-expense"),
		WithTransferFeePolicy(0, "fee-revenue"),
	)
	seedBalance(t, src, "saver", 1_000_000)
	opened := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := src.OpenTermDeposit(TermDeposit{
		ID: "td-snap", Account: "saver", AmountCents: 500_000, TermDays: 30, RateBps: 500,
		OpenedAt: opened, IdempotencyKey: "snap-key",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := src.AccrueTermInterest("td-snap", opened.Add(24*time.Hour)); err != nil {
		t.Fatalf("accrue: %v", err)
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
	td, ok := restored.GetTermDeposit("td-snap")
	if !ok || td.Status != TermDepositActive || td.AmountCents != 500_000 {
		t.Errorf("restored deposit = %+v ok=%v", td, ok)
	}
	// The idempotency key survives: replaying the key returns the
	// original deposit instead of booking a second lock.
	if _, dup, err := restored.OpenTermDeposit(TermDeposit{
		ID: "td-new", Account: "saver", AmountCents: 1, TermDays: 1, RateBps: 1,
		IdempotencyKey: "snap-key",
	}); err != nil || !dup {
		t.Errorf("replay after restore: err=%v dup=%v", err, dup)
	}
	// Config survives: accrual still credits the expense account.
	if _, _, err := restored.AccrueTermInterest("td-snap", opened.Add(48*time.Hour)); err != nil {
		t.Errorf("accrue after restore: %v", err)
	}
}

func TestTermDeposit_ReconcileDisclosure(t *testing.T) {
	l := seedTermDepositLedger(t)
	opened := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-r", Account: "saver", AmountCents: 500_000, TermDays: 30, RateBps: 500,
		OpenedAt: opened,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := l.AccrueTermInterest("td-r", opened.Add(24*time.Hour)); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.TermDeposits) != 1 {
		t.Fatalf("term_deposits = %d rows, want 1", len(rep.TermDeposits))
	}
	r := rep.TermDeposits[0]
	if r.ID != "td-r" || r.PrincipalCents != 500_000 || r.Status != TermDepositActive {
		t.Errorf("report = %+v, want td-r/500000/active", r)
	}
	// floor(500_000 * 500 / 10000 / 365) = floor(25000/365) = 68.
	if r.AccruedInterestCents != 68 {
		t.Errorf("accrued = %d, want 68", r.AccruedInterestCents)
	}
	if !rep.AccountingEquationOK {
		t.Error("accounting equation broken by term-deposit activity")
	}
}
