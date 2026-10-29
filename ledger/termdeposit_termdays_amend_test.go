package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// openTermDaysDeposit opens one active deposit for the term-days
// amendment tests: saver locks 365000 cents for 10 days at 500 bps,
// opened Thursday 2026-10-01, so the raw maturity is Sunday
// 2026-10-11 — a non-business day under the default Saturday/Sunday
// calendar — and the business-day maturity is Monday 2026-10-12.
// One day's interest is exactly floor(365000*500/10000/365) = 50
// cents.
func openTermDaysDeposit(t *testing.T, l *Ledger, id string, businessDay bool) TermDeposit {
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

func TestAmendTermDepositTermDaysValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositTermDays("td-unknown", 20); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openTermDaysDeposit(t, l, "td-1", false)
	if orig.MaturesAt != time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("raw maturity = %v, want 2026-10-11 (Sunday)", orig.MaturesAt)
	}
	// Matured.
	l2 := seedTermDepositLedger(t)
	openTermDaysDeposit(t, l2, "td-1", false)
	if _, err := l2.MatureTermDeposit("td-1", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositTermDays("td-1", 20); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l3 := seedTermDepositLedger(t)
	openTermDaysDeposit(t, l3, "td-1", false)
	if _, err := l3.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l3.AmendTermDepositTermDays("td-1", 20); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Out-of-bounds terms reuse the open path's ErrInvalidTermDays,
	// and every rejected amendment leaves the deposit exactly as it
	// was.
	for _, bad := range []int64{0, -5, 36501} {
		if _, err := l.AmendTermDepositTermDays("td-1", bad); !errors.Is(err, ErrInvalidTermDays) {
			t.Errorf("term %d: err = %v, want ErrInvalidTermDays", bad, err)
		}
	}
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Amending to the current term is a no-op, not a change.
	if got, err := l.AmendTermDepositTermDays("td-1", 10); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-term amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	// Boundary terms are accepted: 1 day and 36500 days.
	if got, err := l.AmendTermDepositTermDays("td-1", 1); err != nil || got.TermDays != 1 || got.MaturesAt != time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC) {
		t.Errorf("term 1 amendment = %+v, %v, want term 1 matures 2026-10-02", got, err)
	}
	if got, err := l.AmendTermDepositTermDays("td-1", 36500); err != nil || got.TermDays != 36500 {
		t.Errorf("term 36500 amendment = %+v, %v, want term 36500, nil", got, err)
	}
}

func TestAmendTermDepositTermDaysRecompute(t *testing.T) {
	// Lengthen, flag off: 10 -> 20 days moves Sunday 2026-10-11 to
	// Wednesday 2026-10-21, and only term and maturity change.
	l := seedTermDepositLedger(t)
	orig := openTermDaysDeposit(t, l, "td-1", false)
	amended, err := l.AmendTermDepositTermDays("td-1", 20)
	if err != nil {
		t.Fatalf("AmendTermDepositTermDays: %v", err)
	}
	if amended.TermDays != 20 || amended.MaturesAt != time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) {
		t.Errorf("amended = term %d matures %v, want 20 2026-10-21", amended.TermDays, amended.MaturesAt)
	}
	amended.TermDays = orig.TermDays
	amended.MaturesAt = orig.MaturesAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except term and maturity", amended, orig)
	}

	// Shorten, flag off: 10 -> 5 days moves to Tuesday 2026-10-06.
	l2 := seedTermDepositLedger(t)
	openTermDaysDeposit(t, l2, "td-1", false)
	amended2, err := l2.AmendTermDepositTermDays("td-1", 5)
	if err != nil {
		t.Fatalf("AmendTermDepositTermDays: %v", err)
	}
	if amended2.TermDays != 5 || amended2.MaturesAt != time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC) {
		t.Errorf("amended = term %d matures %v, want 5 2026-10-06", amended2.TermDays, amended2.MaturesAt)
	}

	// Stored flag applied on recompute: flag on, 10 -> 9 days — the
	// raw date is Saturday 2026-10-10, so the maturity moves to
	// Monday 2026-10-12, exactly as a fresh open would derive it.
	l3 := seedTermDepositLedger(t)
	orig3 := openTermDaysDeposit(t, l3, "td-1", true)
	if orig3.MaturesAt != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("adjusted maturity = %v, want 2026-10-12 (Monday)", orig3.MaturesAt)
	}
	amended3, err := l3.AmendTermDepositTermDays("td-1", 9)
	if err != nil {
		t.Fatalf("AmendTermDepositTermDays: %v", err)
	}
	if amended3.TermDays != 9 || amended3.MaturesAt != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) {
		t.Errorf("amended = term %d matures %v, want 9 2026-10-12", amended3.TermDays, amended3.MaturesAt)
	}
	amended3.TermDays = orig3.TermDays
	amended3.MaturesAt = orig3.MaturesAt
	if !reflect.DeepEqual(amended3, orig3) {
		t.Errorf("amended deposit = %+v, want %+v except term and maturity", amended3, orig3)
	}
}

func TestAmendTermDepositTermDaysAccrualAndSweep(t *testing.T) {
	// Booked rows are never repriced: one day books 50 cents, the
	// amendment lengthens the term, and re-running the same day
	// replays the original row.
	l := seedTermDepositLedger(t)
	openTermDaysDeposit(t, l, "td-1", false)
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("accrue day: %v", err)
	}
	if _, err := l.AmendTermDepositTermDays("td-1", 20); err != nil {
		t.Fatalf("AmendTermDepositTermDays: %v", err)
	}
	if _, dup, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil || !dup {
		t.Errorf("re-accrue booked day = dup %v, %v, want true, nil", dup, err)
	}
	if got := l.TermDepositInterestAccrued("td-1"); got != 50 {
		t.Errorf("accrued after re-run = %d, want 50 (no reprice)", got)
	}
	// Future accrual reads the live maturity: the new maturity is
	// Wednesday 2026-10-21, so accrual on that day still books, but
	// Thursday 2026-10-22 is now past it.
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("accrue at new maturity: %v, want nil", err)
	}
	if _, _, err := l.AccrueTermInterest("td-1", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrTermDepositMatured) {
		t.Errorf("accrue past new maturity: err = %v, want ErrTermDepositMatured", err)
	}

	// A recomputed maturity already in the past books no payout at
	// amendment time: the deposit stays active until the sweep
	// matures it.
	l2 := seedTermDepositLedger(t)
	openTermDaysDeposit(t, l2, "td-1", false)
	if amended, err := l2.AmendTermDepositTermDays("td-1", 1); err != nil || amended.Status != TermDepositActive {
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

func TestAmendTermDepositTermDaysRolloverChildIndependent(t *testing.T) {
	// The sweep rolls the parent into td-1/roll/1 with its own term
	// snapshot (the policy's 30 days). Amending the child's term
	// must leave the parent's row exactly as it was.
	l := seedTermDepositLedger(t)
	parent, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-1",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rollover:    &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 2},
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	l.SweepTermDeposits(parent.MaturesAt)
	child, ok := l.GetTermDeposit("td-1/roll/1")
	if !ok {
		t.Fatal("rollover child td-1/roll/1 missing after sweep")
	}
	if child.TermDays != 30 {
		t.Fatalf("child term = %d, want 30 (policy snapshot)", child.TermDays)
	}
	amendedChild, err := l.AmendTermDepositTermDays("td-1/roll/1", 60)
	if err != nil {
		t.Fatalf("AmendTermDepositTermDays child: %v", err)
	}
	if amendedChild.TermDays != 60 || !amendedChild.MaturesAt.Equal(child.OpenedAt.Add(60*24*time.Hour)) {
		t.Errorf("amended child = term %d matures %v, want 60 %v", amendedChild.TermDays, amendedChild.MaturesAt, child.OpenedAt.Add(60*24*time.Hour))
	}
	after, ok := l.GetTermDeposit("td-1")
	if !ok {
		t.Fatal("parent missing after child amendment")
	}
	if after.TermDays != parent.TermDays || !after.MaturesAt.Equal(parent.MaturesAt) || after.Status != TermDepositMatured {
		t.Errorf("parent after child amendment = term %d matures %v status %q, want %d %v matured", after.TermDays, after.MaturesAt, after.Status, parent.TermDays, parent.MaturesAt)
	}
}

func TestAmendTermDepositTermDaysAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openTermDaysDeposit(t, l, "td-1", false)
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositTermDays("td-1", 10); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTermDepositTermDaysCtx(ctx, "td-1", 20); err != nil {
		t.Fatalf("AmendTermDepositTermDaysCtx: %v", err)
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
	if after.TermDays != 20 || after.MaturesAt != time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) {
		t.Errorf("restored = term %d matures %v, want 20 2026-10-21", after.TermDays, after.MaturesAt)
	}
	after.TermDays = before.TermDays
	after.MaturesAt = before.MaturesAt
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored deposit = %+v, want %+v except term and maturity", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_term_days_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" ||
		d["previous_term_days"] != float64(10) ||
		d["term_days"] != float64(20) ||
		d["previous_matures_at"] != "2026-10-11T00:00:00Z" ||
		d["matures_at"] != "2026-10-21T00:00:00Z" ||
		d["business_day_maturity"] != false ||
		d["amount_cents"] != float64(365000) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 10->20 2026-10-11->2026-10-21 flag false principal 365000 USD", d)
	}
}
