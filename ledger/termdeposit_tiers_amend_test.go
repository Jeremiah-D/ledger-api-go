package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// openTieredTermDeposit opens one active tiered deposit for the
// ladder-amendment tests: saver locks 365000 cents for 30 days,
// opened on Monday 2026-10-26, on the ladder [{0, 500}, {100000,
// 1000}]. One day's interest is floor(100000*500/10000/365) +
// floor(265000*1000/10000/365) = 13 + 72 = 85 cents; repricing to
// [{0, 1000}, {100000, 500}] books 27 + 36 = 63 cents a day.
func openTieredTermDeposit(t *testing.T, l *Ledger, id string) TermDeposit {
	t.Helper()
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          id,
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    30,
		RateTiers:   []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}},
		OpenedAt:    time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit %s: %v", id, err)
	}
	return td
}

func TestAmendTermDepositTiersValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositTiers("td-unknown", []InterestTier{{MinCents: 0, RateBps: 500}}); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openTieredTermDeposit(t, l, "td-1")
	// A flat deposit fails closed: a ladder amendment must neither
	// install a ladder nor touch the flat rate.
	openFlatTermDeposit(t, l, "td-flat")
	if _, err := l.AmendTermDepositTiers("td-flat", []InterestTier{{MinCents: 0, RateBps: 500}}); !errors.Is(err, ErrTermDepositRateTiersConflict) {
		t.Errorf("flat deposit: err = %v, want ErrTermDepositRateTiersConflict", err)
	}
	if got, ok := l.GetTermDeposit("td-flat"); !ok || got.RateBps != 500 || len(got.RateTiers) != 0 {
		t.Errorf("flat deposit after rejected amendment = %+v ok=%v, want flat rate 500 with no ladder", got, ok)
	}
	// Malformed ladders fail closed, including the empty ladder:
	// there is no clear-ladder path for a live deposit.
	badLadders := [][]InterestTier{
		nil,
		{},
		{{MinCents: 100, RateBps: 500}},
		{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 500}, {MinCents: 100_000, RateBps: 600}},
		{{MinCents: 0, RateBps: 0}},
		{{MinCents: 0, RateBps: 10001}},
		{{MinCents: -1, RateBps: 500}},
	}
	for _, ladder := range badLadders {
		if _, err := l.AmendTermDepositTiers("td-1", ladder); !errors.Is(err, ErrInvalidInterestTier) {
			t.Errorf("ladder %+v: err = %v, want ErrInvalidInterestTier", ladder, err)
		}
	}
	// Every rejected amendment leaves the tiered deposit exactly as
	// it was.
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Amending to the current ladder is a no-op, not a change.
	if got, err := l.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}}); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-ladder amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	// A legal amendment replaces the whole ladder — a one-band
	// ladder replaces the two-band original, it does not patch it.
	amended, err := l.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 800}})
	if err != nil {
		t.Fatalf("AmendTermDepositTiers: %v", err)
	}
	if !reflect.DeepEqual(amended.RateTiers, []InterestTier{{MinCents: 0, RateBps: 800}}) {
		t.Errorf("amended ladder = %+v, want single 800 bps band", amended.RateTiers)
	}
	amended.RateTiers = orig.RateTiers
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except the ladder", amended, orig)
	}
}

func TestAmendTermDepositTiersTerminalStates(t *testing.T) {
	// Matured.
	l := seedTermDepositLedger(t)
	openTieredTermDeposit(t, l, "td-1")
	if _, err := l.MatureTermDeposit("td-1", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 600}}); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l2 := seedTermDepositLedger(t)
	openTieredTermDeposit(t, l2, "td-1")
	if _, err := l2.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 600}}); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestAmendTermDepositTiersEffectiveNextAccrual(t *testing.T) {
	l := seedTermDepositLedger(t)
	openTieredTermDeposit(t, l, "td-1")
	day1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	// Day 1 books at the old ladder.
	e1, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || dup {
		t.Fatalf("day 1 accrual: entry=%+v dup=%v err=%v", e1, dup, err)
	}
	if e1.AmountCents != 85 {
		t.Errorf("day 1 interest = %d, want 85 at the original ladder", e1.AmountCents)
	}
	// Reprice the ladder, then day 2 books at the new ladder.
	if _, err := l.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 1000}, {MinCents: 100_000, RateBps: 500}}); err != nil {
		t.Fatalf("AmendTermDepositTiers: %v", err)
	}
	e2, dup, err := l.AccrueTermInterest("td-1", day2)
	if err != nil || dup {
		t.Fatalf("day 2 accrual: entry=%+v dup=%v err=%v", e2, dup, err)
	}
	if e2.AmountCents != 63 {
		t.Errorf("day 2 interest = %d, want 63 at the amended ladder", e2.AmountCents)
	}
	// Re-running day 1 replays the original row at the old ladder —
	// the amendment never reprices a booked day.
	e1r, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || !dup || e1r.AmountCents != 85 || e1r.ID != e1.ID {
		t.Errorf("day 1 replay = %+v dup=%v err=%v, want original 85-cent row", e1r, dup, err)
	}
	if total := l.TermDepositInterestAccrued("td-1"); total != 148 {
		t.Errorf("accrued total = %d, want 148", total)
	}
}

func TestAmendTermDepositTiersRolloverInheritance(t *testing.T) {
	// A child generated before an amendment keeps the ladder it was
	// born with: the amendment writes only the parent's row.
	l := seedTermDepositLedger(t)
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-roll",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateTiers:   []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}},
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
	wantChild := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}}
	if !reflect.DeepEqual(child.RateTiers, wantChild) {
		t.Errorf("child ladder at birth = %+v, want %+v", child.RateTiers, wantChild)
	}
	// The matured parent cannot be amended at all, so the failed
	// amend cannot touch the child either.
	if _, err := l.AmendTermDepositTiers("td-roll", []InterestTier{{MinCents: 0, RateBps: 800}}); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured parent amend: err = %v, want ErrTermDepositNotActive", err)
	}
	if child, ok := l.GetTermDeposit("td-roll/roll/1"); !ok || !reflect.DeepEqual(child.RateTiers, wantChild) {
		t.Errorf("child after failed parent amend = %+v ok=%v, want %+v", child.RateTiers, ok, wantChild)
	}
	// Amending the child itself does not rewrite the parent's row:
	// each rollover row holds its own ladder snapshot.
	if _, err := l.AmendTermDepositTiers("td-roll/roll/1", []InterestTier{{MinCents: 0, RateBps: 800}}); err != nil {
		t.Fatalf("amend child ladder: %v", err)
	}
	if parent, ok := l.GetTermDeposit("td-roll"); !ok || !reflect.DeepEqual(parent.RateTiers, wantChild) {
		t.Errorf("parent after child amend = %+v ok=%v, want %+v", parent.RateTiers, ok, wantChild)
	}

	// Inheritance is generation-time: a parent repriced while still
	// active (its maturity date has passed but the sweep has not run
	// yet, so it is still active) rolls at the amended ladder.
	l2 := seedTermDepositLedger(t)
	if _, _, err := l2.OpenTermDeposit(TermDeposit{
		ID:          "td-roll",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateTiers:   []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}},
		OpenedAt:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rollover:    &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3},
	}); err != nil {
		t.Fatalf("open rollover parent: %v", err)
	}
	if _, err := l2.AmendTermDepositTiers("td-roll", []InterestTier{{MinCents: 0, RateBps: 800}}); err != nil {
		t.Fatalf("amend active parent past maturity: %v", err)
	}
	l2.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if child, ok := l2.GetTermDeposit("td-roll/roll/1"); !ok || !reflect.DeepEqual(child.RateTiers, []InterestTier{{MinCents: 0, RateBps: 800}}) {
		t.Errorf("child after parent reprice = %+v ok=%v, want single 800 bps band", child.RateTiers, ok)
	}
}

func TestAmendTermDepositTiersAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openTieredTermDeposit(t, l, "td-1")
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositTiers("td-1", []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 100_000, RateBps: 1000}}); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	newLadder := []InterestTier{{MinCents: 0, RateBps: 1000}, {MinCents: 100_000, RateBps: 500}}
	if _, err := l.AmendTermDepositTiersCtx(ctx, "td-1", newLadder); err != nil {
		t.Fatalf("AmendTermDepositTiersCtx: %v", err)
	}
	// Mutating the caller's slice afterwards must not rewrite the
	// stored ladder: the amendment copies defensively.
	newLadder[0].RateBps = 9999
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got.RateTiers, []InterestTier{{MinCents: 0, RateBps: 1000}, {MinCents: 100_000, RateBps: 500}}) {
		t.Errorf("stored ladder after caller mutation = %+v ok=%v, want amended ladder intact", got.RateTiers, ok)
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
	if !reflect.DeepEqual(after.RateTiers, []InterestTier{{MinCents: 0, RateBps: 1000}, {MinCents: 100_000, RateBps: 500}}) {
		t.Errorf("restored ladder = %+v, want amended ladder", after.RateTiers)
	}
	after.RateTiers = before.RateTiers
	if after.ID != before.ID || after.Account != before.Account ||
		after.AmountCents != before.AmountCents || after.Currency != before.Currency ||
		after.RateBps != before.RateBps || after.TermDays != before.TermDays ||
		after.Status != before.Status ||
		after.BusinessDayMaturity != before.BusinessDayMaturity ||
		after.RolloversUsed != before.RolloversUsed || after.RolledFrom != before.RolledFrom ||
		!reflect.DeepEqual(after.Rollover, before.Rollover) {
		t.Errorf("restored deposit = %+v, want %+v except the ladder", after, before)
	}
	if after.OpenedAt.UnixNano() != before.OpenedAt.UnixNano() ||
		after.MaturesAt.UnixNano() != before.MaturesAt.UnixNano() {
		t.Errorf("restored deposit times = %v/%v, want %v/%v",
			after.OpenedAt, after.MaturesAt, before.OpenedAt, before.MaturesAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_tiers_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" || d["amount_cents"] != float64(365000) ||
		d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 principal 365000 USD", d)
	}
	tiers, ok := d["rate_tiers"].([]any)
	if !ok || len(tiers) != 2 {
		t.Fatalf("event rate_tiers = %#v, want 2 bands", d["rate_tiers"])
	}
	first, ok := tiers[0].(map[string]any)
	if !ok || first["min_cents"] != float64(0) || first["rate_bps"] != float64(1000) {
		t.Errorf("event first band = %#v, want min 0 rate 1000", tiers[0])
	}
	prev, ok := d["previous_rate_tiers"].([]any)
	if !ok || len(prev) != 2 {
		t.Fatalf("event previous_rate_tiers = %#v, want 2 bands", d["previous_rate_tiers"])
	}
}
