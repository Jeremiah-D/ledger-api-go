package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTermDepositAmountTopUpAccruesOnNewPrincipal(t *testing.T) {
	l := seedTermDepositLedger(t)
	orig := openFlatTermDeposit(t, l, "td-1")
	day1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	// Day 1 books at the old principal: floor(365000*500/10000/365) = 50.
	e1, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || dup {
		t.Fatalf("day 1 accrual: entry=%+v dup=%v err=%v", e1, dup, err)
	}
	if e1.AmountCents != 50 {
		t.Errorf("day 1 interest = %d, want 50", e1.AmountCents)
	}
	versionBefore := l.Version()
	// Top up to exactly double: 365000 -> 730000. The saver holds
	// 1,000,000 plus the 50 cents of day-1 interest just booked, and
	// 365,000 is locked, so 635,050 is free and the 365,000 increase
	// fits.
	amended, err := l.AmendTermDepositAmount("td-1", 730_000)
	if err != nil {
		t.Fatalf("AmendTermDepositAmount: %v", err)
	}
	if amended.AmountCents != 730_000 {
		t.Errorf("amended principal = %d, want 730000", amended.AmountCents)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The lock follows the new principal immediately.
	if got := l.AvailableIn("saver", "USD"); got != 270_050 {
		t.Errorf("available after top-up = %d, want 270050", got)
	}
	// Day 2 books at the new principal: floor(730000*500/10000/365) = 100.
	e2, dup, err := l.AccrueTermInterest("td-1", day2)
	if err != nil || dup {
		t.Fatalf("day 2 accrual: entry=%+v dup=%v err=%v", e2, dup, err)
	}
	if e2.AmountCents != 100 {
		t.Errorf("day 2 interest = %d, want 100 at the topped-up principal", e2.AmountCents)
	}
	// Only the principal moved.
	amended.AmountCents = orig.AmountCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except the principal", amended, orig)
	}
}

func TestAmendTermDepositAmountDrawDownReleasesFunds(t *testing.T) {
	l := seedTermDepositLedger(t)
	openFlatTermDeposit(t, l, "td-1")
	day1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	if _, _, err := l.AccrueTermInterest("td-1", day1); err != nil {
		t.Fatalf("day 1 accrual: %v", err)
	}
	// Day 1 booked 50 cents of interest to the saver, so the
	// available funds are 1,000,000 + 50 - 365,000 = 635,050.
	if got := l.AvailableIn("saver", "USD"); got != 635_050 {
		t.Fatalf("available before draw-down = %d, want 635050", got)
	}
	// Draw down to half: 365000 -> 182500. No funds gate applies —
	// releasing liquidity cannot fail — and no penalty is booked.
	amended, err := l.AmendTermDepositAmount("td-1", 182_500)
	if err != nil {
		t.Fatalf("AmendTermDepositAmount: %v", err)
	}
	if amended.AmountCents != 182_500 || amended.Status != TermDepositActive {
		t.Errorf("amended deposit = %+v, want 182500 active", amended)
	}
	if got := l.AvailableIn("saver", "USD"); got != 817_550 {
		t.Errorf("available after draw-down = %d, want 817550", got)
	}
	// Day 2 books at the reduced principal:
	// floor(182500*500/10000/365) = 25.
	e2, dup, err := l.AccrueTermInterest("td-1", day2)
	if err != nil || dup {
		t.Fatalf("day 2 accrual: entry=%+v dup=%v err=%v", e2, dup, err)
	}
	if e2.AmountCents != 25 {
		t.Errorf("day 2 interest = %d, want 25 at the drawn-down principal", e2.AmountCents)
	}
	// History is untouched: day 1 still books the original 50.
	if total := l.TermDepositInterestAccrued("td-1"); total != 75 {
		t.Errorf("accrued total = %d, want 75 (50 + 25)", total)
	}
}

func TestAmendTermDepositAmountValidation(t *testing.T) {
	// Unknown deposit.
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositAmount("td-unknown", 400_000); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openFlatTermDeposit(t, l, "td-1")
	// Zero and negative principals are rejected, live untouched.
	for _, amount := range []int64{0, -1, -365_000} {
		if _, err := l.AmendTermDepositAmount("td-1", amount); !errors.Is(err, ErrTermDepositNonPositiveAmount) {
			t.Errorf("amount %d: err = %v, want ErrTermDepositNonPositiveAmount", amount, err)
		}
	}
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// A top-up past the available funds is rejected, live untouched:
	// the saver holds 1,000,000, so exactly 1,000,000 fits and one
	// cent more does not.
	if _, err := l.AmendTermDepositAmount("td-1", 1_000_001); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("over-funds top-up: err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if got, ok := l.GetTermDeposit("td-1"); !ok || got.AmountCents != 365_000 {
		t.Errorf("deposit after rejected top-up = %+v ok=%v, want 365000", got, ok)
	}
	if got, err := l.AmendTermDepositAmount("td-1", 1_000_000); err != nil || got.AmountCents != 1_000_000 {
		t.Errorf("boundary top-up to 1000000 = %+v, %v, want 1000000, nil", got, err)
	}
}

func TestAmendTermDepositAmountTerminalStates(t *testing.T) {
	// Matured.
	l := seedTermDepositLedger(t)
	orig := openFlatTermDeposit(t, l, "td-1")
	if _, err := l.MatureTermDeposit("td-1", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.AmendTermDepositAmount("td-1", 400_000); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l2 := seedTermDepositLedger(t)
	openFlatTermDeposit(t, l2, "td-1")
	if _, err := l2.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositAmount("td-1", 400_000); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	_ = orig
}

func TestAmendTermDepositAmountAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openFlatTermDeposit(t, l, "td-1")
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositAmount("td-1", 365_000); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTermDepositAmountCtx(ctx, "td-1", 500_000); err != nil {
		t.Fatalf("AmendTermDepositAmountCtx: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTermDeposit("td-1")
	if !ok {
		t.Fatal("restored deposit missing")
	}
	if after.AmountCents != 500_000 {
		t.Errorf("restored principal = %d, want 500000", after.AmountCents)
	}
	after.AmountCents = before.AmountCents
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored deposit = %+v, want %+v except the principal", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" || d["previous_amount_cents"] != float64(365000) ||
		d["amount_cents"] != float64(500000) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 365000->500000 USD", d)
	}
}

func TestAmendTermDepositAmountRolloverInheritsNewPrincipal(t *testing.T) {
	// A child generated after the amendment inherits the amended
	// principal: the rollover copies the live row at generation
	// time, and the amendment writes only the parent's row.
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-roll",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rollover:    &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3},
	}); err != nil {
		t.Fatalf("open rollover parent: %v", err)
	}
	if _, err := l.AmendTermDepositAmount("td-roll", 400_000); err != nil {
		t.Fatalf("AmendTermDepositAmount: %v", err)
	}
	l.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child, ok := l.GetTermDeposit("td-roll/roll/1")
	if !ok {
		t.Fatal("rollover child missing after sweep")
	}
	if child.AmountCents != 400_000 {
		t.Errorf("child principal = %d, want 400000 (amended parent)", child.AmountCents)
	}
}
