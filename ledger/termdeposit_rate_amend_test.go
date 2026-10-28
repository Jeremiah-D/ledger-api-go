package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// openFlatTermDeposit opens one active flat-rate deposit for the
// rate-amendment tests: saver locks 365000 cents for 30 days at
// 500 bps, opened on Monday 2026-10-26, so one day's interest is
// exactly floor(365000*500/10000/365) = 50 cents and a repriced
// 1000 bps day is exactly 100 cents.
func openFlatTermDeposit(t *testing.T, l *Ledger, id string) TermDeposit {
	t.Helper()
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          id,
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    30,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit %s: %v", id, err)
	}
	return td
}

func TestAmendTermDepositRateValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositRate("td-unknown", 600); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openFlatTermDeposit(t, l, "td-1")
	for _, rate := range []int64{0, -100, 10001, 20000} {
		if _, err := l.AmendTermDepositRate("td-1", rate); !errors.Is(err, ErrInvalidTermDepositRate) {
			t.Errorf("rate %d: err = %v, want ErrInvalidTermDepositRate", rate, err)
		}
	}
	// A tiered deposit fails closed: repricing it must neither
	// install a flat rate nor flatten the ladder.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-tiered",
		Account:     "saver",
		AmountCents: 100_000,
		TermDays:    30,
		RateTiers:   []InterestTier{{MinCents: 0, RateBps: 500}},
		OpenedAt:    time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("open tiered: %v", err)
	}
	if _, err := l.AmendTermDepositRate("td-tiered", 600); !errors.Is(err, ErrTermDepositRateTiersConflict) {
		t.Errorf("tiered deposit: err = %v, want ErrTermDepositRateTiersConflict", err)
	}
	if got, ok := l.GetTermDeposit("td-tiered"); !ok || got.RateBps != 0 || len(got.RateTiers) != 1 {
		t.Errorf("tiered deposit after rejected amendment = %+v ok=%v, want flat rate 0 with ladder intact", got, ok)
	}
	// Every rejected amendment leaves the flat deposit exactly as
	// it was.
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Amending to the current rate is a no-op, not a change.
	if got, err := l.AmendTermDepositRate("td-1", 500); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-rate amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	// A legal amendment changes the rate and nothing else.
	amended, err := l.AmendTermDepositRate("td-1", 1000)
	if err != nil {
		t.Fatalf("AmendTermDepositRate: %v", err)
	}
	if amended.RateBps != 1000 {
		t.Errorf("amended rate = %d, want 1000", amended.RateBps)
	}
	amended.RateBps = orig.RateBps
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except the rate", amended, orig)
	}
}

func TestAmendTermDepositRateTerminalStates(t *testing.T) {
	// Matured.
	l := seedTermDepositLedger(t)
	openFlatTermDeposit(t, l, "td-1")
	if _, err := l.MatureTermDeposit("td-1", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.AmendTermDepositRate("td-1", 600); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l2 := seedTermDepositLedger(t)
	openFlatTermDeposit(t, l2, "td-1")
	if _, err := l2.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositRate("td-1", 600); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestAmendTermDepositRateEffectiveNextAccrual(t *testing.T) {
	l := seedTermDepositLedger(t)
	openFlatTermDeposit(t, l, "td-1")
	day1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	// Day 1 books at the old rate.
	e1, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || dup {
		t.Fatalf("day 1 accrual: entry=%+v dup=%v err=%v", e1, dup, err)
	}
	if e1.AmountCents != 50 {
		t.Errorf("day 1 interest = %d, want 50 at 500 bps", e1.AmountCents)
	}
	// Reprice, then day 2 books at the new rate.
	if _, err := l.AmendTermDepositRate("td-1", 1000); err != nil {
		t.Fatalf("AmendTermDepositRate: %v", err)
	}
	e2, dup, err := l.AccrueTermInterest("td-1", day2)
	if err != nil || dup {
		t.Fatalf("day 2 accrual: entry=%+v dup=%v err=%v", e2, dup, err)
	}
	if e2.AmountCents != 100 {
		t.Errorf("day 2 interest = %d, want 100 at 1000 bps", e2.AmountCents)
	}
	// Re-running day 1 replays the original row at the old rate —
	// the amendment never reprices a booked day.
	e1r, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || !dup || e1r.AmountCents != 50 || e1r.ID != e1.ID {
		t.Errorf("day 1 replay = %+v dup=%v err=%v, want original 50-cent row", e1r, dup, err)
	}
	if total := l.TermDepositInterestAccrued("td-1"); total != 150 {
		t.Errorf("accrued total = %d, want 150", total)
	}
}

func TestAmendTermDepositRateRolloverInheritance(t *testing.T) {
	// A child generated before an amendment keeps the rate it was
	// born with: the amendment writes only the parent's row, and a
	// matured parent cannot be amended at all, so the failed amend
	// cannot touch the child either.
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
	l.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child, ok := l.GetTermDeposit("td-roll/roll/1")
	if !ok {
		t.Fatal("rollover child missing after sweep")
	}
	if child.RateBps != 500 {
		t.Errorf("child rate at birth = %d, want 500", child.RateBps)
	}
	if _, err := l.AmendTermDepositRate("td-roll", 800); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured parent amend: err = %v, want ErrTermDepositNotActive", err)
	}
	if child, ok := l.GetTermDeposit("td-roll/roll/1"); !ok || child.RateBps != 500 {
		t.Errorf("child after failed parent amend = %+v ok=%v, want rate 500", child, ok)
	}

	// Inheritance is generation-time: a parent repriced while still
	// active (its maturity date has passed but the sweep has not run
	// yet, so it is still active) rolls at the amended rate — the
	// child born after the amendment is a new term at the new rate,
	// not a rewrite of an existing row.
	l2 := seedTermDepositLedger(t)
	if _, _, err := l2.OpenTermDeposit(TermDeposit{
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
	if _, err := l2.AmendTermDepositRate("td-roll", 800); err != nil {
		t.Fatalf("amend active parent past maturity: %v", err)
	}
	l2.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if child, ok := l2.GetTermDeposit("td-roll/roll/1"); !ok || child.RateBps != 800 {
		t.Errorf("child after parent reprice = %+v ok=%v, want rate 800", child, ok)
	}
}

func TestAmendTermDepositRateAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openFlatTermDeposit(t, l, "td-1")
	// The no-op reprice emits nothing.
	if _, err := l.AmendTermDepositRate("td-1", 500); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTermDepositRateCtx(ctx, "td-1", 1000); err != nil {
		t.Fatalf("AmendTermDepositRateCtx: %v", err)
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
	if after.RateBps != 1000 {
		t.Errorf("restored rate = %d, want 1000", after.RateBps)
	}
	after.RateBps = before.RateBps
	if after.ID != before.ID || after.Account != before.Account ||
		after.AmountCents != before.AmountCents || after.Currency != before.Currency ||
		after.TermDays != before.TermDays || after.Status != before.Status ||
		after.BusinessDayMaturity != before.BusinessDayMaturity ||
		after.RolloversUsed != before.RolloversUsed || after.RolledFrom != before.RolledFrom ||
		!reflect.DeepEqual(after.RateTiers, before.RateTiers) ||
		!reflect.DeepEqual(after.Rollover, before.Rollover) {
		t.Errorf("restored deposit = %+v, want %+v except the rate", after, before)
	}
	if after.OpenedAt.UnixNano() != before.OpenedAt.UnixNano() ||
		after.MaturesAt.UnixNano() != before.MaturesAt.UnixNano() {
		t.Errorf("restored deposit times = %v/%v, want %v/%v",
			after.OpenedAt, after.MaturesAt, before.OpenedAt, before.MaturesAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_rate_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" || d["previous_rate_bps"] != float64(500) ||
		d["rate_bps"] != float64(1000) || d["amount_cents"] != float64(365000) ||
		d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 500->1000 principal 365000 USD", d)
	}
}
