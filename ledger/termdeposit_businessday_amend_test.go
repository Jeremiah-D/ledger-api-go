package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// openBusinessDayTermDeposit opens one active deposit for the
// business-day-maturity amendment tests: saver locks 365000 cents for
// 10 days at 500 bps, opened Thursday 2026-10-01, so the raw maturity
// is Sunday 2026-10-11 — a non-business day under the default
// Saturday/Sunday calendar — and the business-day maturity is Monday
// 2026-10-12. One day's interest is exactly
// floor(365000*500/10000/365) = 50 cents.
func openBusinessDayTermDeposit(t *testing.T, l *Ledger, id string, businessDay bool) TermDeposit {
	t.Helper()
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:                  id,
		Account:             "saver",
		AmountCents:         365_000,
		TermDays:            10,
		RateBps:             500,
		OpenedAt:            time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		BusinessDayMaturity: businessDay,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit %s: %v", id, err)
	}
	return td
}

func TestAmendTermDepositBusinessDayMaturityValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositBusinessDayMaturity("td-unknown", true); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openBusinessDayTermDeposit(t, l, "td-1", false)
	if orig.MaturesAt != time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("raw maturity = %v, want 2026-10-11 (Sunday)", orig.MaturesAt)
	}
	// Matured.
	l2 := seedTermDepositLedger(t)
	openBusinessDayTermDeposit(t, l2, "td-1", false)
	if _, err := l2.MatureTermDeposit("td-1", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositBusinessDayMaturity("td-1", true); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l3 := seedTermDepositLedger(t)
	openBusinessDayTermDeposit(t, l3, "td-1", false)
	if _, err := l3.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l3.AmendTermDepositBusinessDayMaturity("td-1", true); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Every rejected amendment leaves the deposit exactly as it was.
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Amending to the current flag is a no-op, not a change.
	if got, err := l.AmendTermDepositBusinessDayMaturity("td-1", false); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-flag amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
}

func TestAmendTermDepositBusinessDayMaturityRecompute(t *testing.T) {
	// Flag off -> on: the Sunday raw date moves to Monday.
	l := seedTermDepositLedger(t)
	orig := openBusinessDayTermDeposit(t, l, "td-1", false)
	amended, err := l.AmendTermDepositBusinessDayMaturity("td-1", true)
	if err != nil {
		t.Fatalf("AmendTermDepositBusinessDayMaturity: %v", err)
	}
	if !amended.BusinessDayMaturity || amended.MaturesAt != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) {
		t.Errorf("amended = flag %v matures %v, want true 2026-10-12", amended.BusinessDayMaturity, amended.MaturesAt)
	}
	amended.BusinessDayMaturity = orig.BusinessDayMaturity
	amended.MaturesAt = orig.MaturesAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except flag and maturity", amended, orig)
	}

	// Flag on -> off: the Monday adjusted date restores the Sunday
	// raw date.
	l2 := seedTermDepositLedger(t)
	orig2 := openBusinessDayTermDeposit(t, l2, "td-1", true)
	if orig2.MaturesAt != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("adjusted maturity = %v, want 2026-10-12 (Monday)", orig2.MaturesAt)
	}
	amended2, err := l2.AmendTermDepositBusinessDayMaturity("td-1", false)
	if err != nil {
		t.Fatalf("AmendTermDepositBusinessDayMaturity: %v", err)
	}
	if amended2.BusinessDayMaturity || amended2.MaturesAt != time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) {
		t.Errorf("amended = flag %v matures %v, want false 2026-10-11", amended2.BusinessDayMaturity, amended2.MaturesAt)
	}
	amended2.BusinessDayMaturity = orig2.BusinessDayMaturity
	amended2.MaturesAt = orig2.MaturesAt
	if !reflect.DeepEqual(amended2, orig2) {
		t.Errorf("amended deposit = %+v, want %+v except flag and maturity", amended2, orig2)
	}

	// Flag differs but the recomputed date is equal: opened Monday
	// 2026-10-26 for 4 days, raw maturity Friday 2026-10-30 is a
	// business day, so turning the flag on moves nothing — yet the
	// amendment is still real: the stored flag changed.
	l3 := seedTermDepositLedger(t)
	td, _, err := l3.OpenTermDeposit(TermDeposit{
		ID:          "td-1",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    4,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	amended3, err := l3.AmendTermDepositBusinessDayMaturity("td-1", true)
	if err != nil {
		t.Fatalf("AmendTermDepositBusinessDayMaturity: %v", err)
	}
	if !amended3.BusinessDayMaturity || !amended3.MaturesAt.Equal(td.MaturesAt) {
		t.Errorf("equal-date amendment = flag %v matures %v, want true %v", amended3.BusinessDayMaturity, amended3.MaturesAt, td.MaturesAt)
	}
}

func TestAmendTermDepositBusinessDayMaturityAccrualAndSweep(t *testing.T) {
	// Booked rows are never repriced: one day books 50 cents, the
	// amendment moves the maturity, and re-running the same day
	// replays the original row.
	l := seedTermDepositLedger(t)
	openBusinessDayTermDeposit(t, l, "td-1", true)
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("accrue day: %v", err)
	}
	if _, err := l.AmendTermDepositBusinessDayMaturity("td-1", false); err != nil {
		t.Fatalf("AmendTermDepositBusinessDayMaturity: %v", err)
	}
	if _, dup, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil || !dup {
		t.Errorf("re-accrue booked day = dup %v, %v, want true, nil", dup, err)
	}
	if got := l.TermDepositInterestAccrued("td-1"); got != 50 {
		t.Errorf("accrued after re-run = %d, want 50 (no reprice)", got)
	}
	// Future accrual reads the live maturity: the new maturity is
	// Sunday 2026-10-11, so accrual on that day still books, but
	// Monday 2026-10-12 — the old maturity — is now past it.
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("accrue at new maturity: %v, want nil", err)
	}
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrTermDepositMatured) {
		t.Errorf("accrue past new maturity: err = %v, want ErrTermDepositMatured", err)
	}

	// A recomputed maturity already in the past books no payout at
	// amendment time: the deposit stays active until the sweep
	// matures it.
	l2 := seedTermDepositLedger(t)
	openBusinessDayTermDeposit(t, l2, "td-1", true)
	if amended, err := l2.AmendTermDepositBusinessDayMaturity("td-1", false); err != nil || amended.Status != TermDepositActive {
		t.Errorf("past-maturity amendment = %+v, %v, want active, nil", amended.Status, err)
	}
	if got := l2.TermDepositInterestAccrued("td-1"); got != 0 {
		t.Errorf("accrued after past-maturity amendment = %d, want 0 (no payout booked)", got)
	}
	matured, _ := l2.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if matured != 1 {
		t.Errorf("sweep matured = %d, want 1", matured)
	}
	if got, ok := l2.GetTermDeposit("td-1"); !ok || got.Status != TermDepositMatured {
		t.Errorf("deposit after sweep = %+v ok=%v, want matured", got.Status, ok)
	}
}

func TestAmendTermDepositBusinessDayMaturityAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openBusinessDayTermDeposit(t, l, "td-1", false)
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositBusinessDayMaturity("td-1", false); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	// A flag change whose recomputed date equals the stored date
	// still audits: opened Monday 2026-10-26 for 4 days, raw maturity
	// Friday 2026-10-30 is a business day, so turning the flag on
	// moves nothing — yet the row changed, so an event fires.
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTermDepositBusinessDayMaturityCtx(ctx, "td-1", true); err != nil {
		t.Fatalf("AmendTermDepositBusinessDayMaturityCtx: %v", err)
	}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-2",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    4,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("OpenTermDeposit td-2: %v", err)
	}
	if _, err := l.AmendTermDepositBusinessDayMaturityCtx(ctx, "td-2", true); err != nil {
		t.Fatalf("equal-date AmendTermDepositBusinessDayMaturityCtx: %v", err)
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
	if !after.BusinessDayMaturity || after.MaturesAt != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) {
		t.Errorf("restored = flag %v matures %v, want true 2026-10-12", after.BusinessDayMaturity, after.MaturesAt)
	}
	after.BusinessDayMaturity = before.BusinessDayMaturity
	after.MaturesAt = before.MaturesAt
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored deposit = %+v, want %+v except flag and maturity", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_business_day_maturity_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (no-op must not audit, equal-date must)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" ||
		d["previous_business_day_maturity"] != false ||
		d["business_day_maturity"] != true ||
		d["previous_matures_at"] != "2026-10-11T00:00:00Z" ||
		d["matures_at"] != "2026-10-12T00:00:00Z" ||
		d["term_days"] != float64(10) ||
		d["amount_cents"] != float64(365000) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 false->true 2026-10-11->2026-10-12 term 10 principal 365000 USD", d)
	}
	// The equal-date amendment audits with identical dates.
	d2 := events[1].Details
	if d2["term_deposit_id"] != "td-2" ||
		d2["previous_business_day_maturity"] != false ||
		d2["business_day_maturity"] != true ||
		d2["previous_matures_at"] != "2026-10-30T00:00:00Z" ||
		d2["matures_at"] != "2026-10-30T00:00:00Z" {
		t.Errorf("equal-date event details = %+v, want td-2 false->true 2026-10-30->2026-10-30", d2)
	}
}
