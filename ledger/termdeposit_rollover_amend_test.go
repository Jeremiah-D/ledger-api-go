package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// openRolloverTermDeposit opens one active deposit for the
// rollover-amendment tests: saver locks 365000 cents for 10 days at
// 500 bps, opened 2026-10-01, with a principal rollover policy of
// 30-day terms capped at 3 rolls. One day's interest is exactly
// floor(365000*500/10000/365) = 50 cents.
func openRolloverTermDeposit(t *testing.T, l *Ledger, id string) TermDeposit {
	t.Helper()
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          id,
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rollover:    &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3},
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit %s: %v", id, err)
	}
	return td
}

func TestAmendTermDepositRolloverValidation(t *testing.T) {
	l := seedTermDepositLedger(t)
	if _, err := l.AmendTermDepositRollover("td-unknown", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3}); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openRolloverTermDeposit(t, l, "td-1")
	// A deposit with no policy fails closed: a policy cannot be
	// installed after opening — and neither can a mode-"none"
	// policy, which open normalizes to nil anyway.
	openFlatTermDeposit(t, l, "td-plain")
	if _, err := l.AmendTermDepositRollover("td-plain", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3}); !errors.Is(err, ErrTermDepositRolloverNotConfigured) {
		t.Errorf("unconfigured deposit: err = %v, want ErrTermDepositRolloverNotConfigured", err)
	}
	// Clearing the policy is rejected: nil and mode "none" are not
	// amendments, and there is no clear-policy path.
	for _, policy := range []*RolloverPolicy{
		nil,
		{Mode: RolloverNone, TermDays: 30, MaxRollovers: 3},
		{Mode: "", TermDays: 30, MaxRollovers: 3},
	} {
		if _, err := l.AmendTermDepositRollover("td-1", policy); !errors.Is(err, ErrInvalidRolloverMode) {
			t.Errorf("clear policy %+v: err = %v, want ErrInvalidRolloverMode", policy, err)
		}
	}
	// Malformed replacement policies fail closed.
	badPolicies := []struct {
		policy *RolloverPolicy
		want   error
	}{
		{&RolloverPolicy{Mode: "bogus", TermDays: 30, MaxRollovers: 3}, ErrInvalidRolloverMode},
		{&RolloverPolicy{Mode: RolloverPrincipal, TermDays: 0, MaxRollovers: 3}, ErrInvalidRolloverTermDays},
		{&RolloverPolicy{Mode: RolloverPrincipal, TermDays: 36501, MaxRollovers: 3}, ErrInvalidRolloverTermDays},
		{&RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 0}, ErrInvalidRolloverMaxRollovers},
	}
	for _, tc := range badPolicies {
		if _, err := l.AmendTermDepositRollover("td-1", tc.policy); !errors.Is(err, tc.want) {
			t.Errorf("policy %+v: err = %v, want %v", tc.policy, err, tc.want)
		}
	}
	// Every rejected amendment leaves the deposit exactly as it was.
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected amendments = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Amending to the current policy is a no-op, not a change.
	if got, err := l.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3}); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-policy amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	// A legal amendment replaces the whole policy and nothing else.
	amended, err := l.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 45, MaxRollovers: 2})
	if err != nil {
		t.Fatalf("AmendTermDepositRollover: %v", err)
	}
	if amended.Rollover == nil || *amended.Rollover != (RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 45, MaxRollovers: 2}) {
		t.Errorf("amended policy = %+v, want principal_plus_interest/45/2", amended.Rollover)
	}
	amended.Rollover = orig.Rollover
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except the policy", amended, orig)
	}
}

func TestAmendTermDepositRolloverTerminalStates(t *testing.T) {
	// Matured.
	l := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l, "td-1")
	if _, err := l.MatureTermDeposit("td-1", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 45, MaxRollovers: 3}); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l2 := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l2, "td-1")
	if _, err := l2.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 45, MaxRollovers: 3}); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestAmendTermDepositRolloverCapBelowUsed(t *testing.T) {
	// Roll twice so the second-generation child has RolloversUsed 2,
	// then a cap of 1 must fail: it would retroactively invalidate
	// a renewal that already happened.
	l := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l, "td-1")
	l.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child1, ok := l.GetTermDeposit("td-1/roll/1")
	if !ok {
		t.Fatal("first rollover child missing after sweep")
	}
	if child1.RolloversUsed != 1 {
		t.Fatalf("child1 rolls used = %d, want 1", child1.RolloversUsed)
	}
	// Child1 opened 2026-10-12 for 30 days, so it matures 2026-11-11.
	l.SweepTermDeposits(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
	child2, ok := l.GetTermDeposit("td-1/roll/1/roll/2")
	if !ok {
		t.Fatal("second rollover child missing after sweep")
	}
	if child2.RolloversUsed != 2 {
		t.Fatalf("child2 rolls used = %d, want 2", child2.RolloversUsed)
	}
	orig := child2
	if _, err := l.AmendTermDepositRollover("td-1/roll/1/roll/2", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 1}); !errors.Is(err, ErrInvalidRolloverMaxRollovers) {
		t.Errorf("cap below used: err = %v, want ErrInvalidRolloverMaxRollovers", err)
	}
	if got, ok := l.GetTermDeposit("td-1/roll/1/roll/2"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("child after rejected cap = %+v ok=%v, want %+v", got, ok, orig)
	}
	// A cap exactly equal to the rolls used is legal: it simply ends
	// the chain at the next maturity.
	if amended, err := l.AmendTermDepositRollover("td-1/roll/1/roll/2", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 2}); err != nil || amended.Rollover == nil || amended.Rollover.MaxRollovers != 2 {
		t.Errorf("cap equal to used = %+v, %v, want policy with cap 2", amended.Rollover, err)
	}
}

func TestAmendTermDepositRolloverEffectiveNextMaturity(t *testing.T) {
	// Term change: the child born after the amendment takes the new
	// tenor, not the original one.
	l := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l, "td-1")
	if _, err := l.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 45, MaxRollovers: 3}); err != nil {
		t.Fatalf("AmendTermDepositRollover: %v", err)
	}
	l.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child, ok := l.GetTermDeposit("td-1/roll/1")
	if !ok {
		t.Fatal("rollover child missing after sweep")
	}
	if child.TermDays != 45 || child.Rollover == nil || child.Rollover.TermDays != 45 {
		t.Errorf("child after term amendment = term %d policy %+v, want 45/45", child.TermDays, child.Rollover)
	}
	if child.Rollover.MaxRollovers != 3 || child.Rollover.Mode != RolloverPrincipal {
		t.Errorf("child policy after term amendment = %+v, want principal/45/3", child.Rollover)
	}

	// Mode change: one day of interest (50 cents) is booked, then
	// the mode switches to principal_plus_interest, so the child
	// re-locks principal + journaled interest.
	l2 := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l2, "td-1")
	if _, _, err := l2.AccrueTermInterest("td-1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("accrue day: %v", err)
	}
	if _, err := l2.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 30, MaxRollovers: 3}); err != nil {
		t.Fatalf("AmendTermDepositRollover: %v", err)
	}
	l2.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child2, ok := l2.GetTermDeposit("td-1/roll/1")
	if !ok {
		t.Fatal("rollover child missing after mode amendment sweep")
	}
	if child2.AmountCents != 365_050 {
		t.Errorf("child principal after mode amendment = %d, want 365050 (principal + 50 interest)", child2.AmountCents)
	}

	// Child independence: amending a child's policy never rewrites
	// its parent's row — each rollover row holds its own snapshot.
	l3 := seedTermDepositLedger(t)
	openRolloverTermDeposit(t, l3, "td-1")
	l3.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if _, err := l3.AmendTermDepositRollover("td-1/roll/1", &RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 60, MaxRollovers: 5}); err != nil {
		t.Fatalf("amend child policy: %v", err)
	}
	if parent, ok := l3.GetTermDeposit("td-1"); !ok || parent.Rollover == nil || *parent.Rollover != (RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3}) {
		t.Errorf("parent after child amend = %+v ok=%v, want original principal/30/3", parent.Rollover, ok)
	}
}

func TestAmendTermDepositRolloverAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	before := openRolloverTermDeposit(t, l, "td-1")
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositRollover("td-1", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3}); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	newPolicy := &RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 45, MaxRollovers: 2}
	if _, err := l.AmendTermDepositRolloverCtx(ctx, "td-1", newPolicy); err != nil {
		t.Fatalf("AmendTermDepositRolloverCtx: %v", err)
	}
	// Mutating the caller's struct afterwards must not rewrite the
	// stored policy: the amendment copies defensively.
	newPolicy.TermDays = 999
	if got, ok := l.GetTermDeposit("td-1"); !ok || got.Rollover == nil || got.Rollover.TermDays != 45 {
		t.Errorf("stored policy after caller mutation = %+v ok=%v, want term 45 intact", got.Rollover, ok)
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
	if after.Rollover == nil || *after.Rollover != (RolloverPolicy{Mode: RolloverPrincipalPlusInterest, TermDays: 45, MaxRollovers: 2}) {
		t.Errorf("restored policy = %+v, want principal_plus_interest/45/2", after.Rollover)
	}
	after.Rollover = before.Rollover
	if after.ID != before.ID || after.Account != before.Account ||
		after.AmountCents != before.AmountCents || after.Currency != before.Currency ||
		after.RateBps != before.RateBps || after.TermDays != before.TermDays ||
		after.Status != before.Status ||
		after.BusinessDayMaturity != before.BusinessDayMaturity ||
		after.RolloversUsed != before.RolloversUsed || after.RolledFrom != before.RolledFrom ||
		!reflect.DeepEqual(after.RateTiers, before.RateTiers) {
		t.Errorf("restored deposit = %+v, want %+v except the policy", after, before)
	}
	if after.OpenedAt.UnixNano() != before.OpenedAt.UnixNano() ||
		after.MaturesAt.UnixNano() != before.MaturesAt.UnixNano() {
		t.Errorf("restored deposit times = %v/%v, want %v/%v",
			after.OpenedAt, after.MaturesAt, before.OpenedAt, before.MaturesAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_rollover_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" ||
		d["previous_rollover_mode"] != "principal" ||
		d["previous_rollover_term_days"] != float64(30) ||
		d["previous_rollover_max"] != float64(3) ||
		d["rollover_mode"] != "principal_plus_interest" ||
		d["rollover_term_days"] != float64(45) ||
		d["rollover_max"] != float64(2) ||
		d["rollovers_used"] != float64(0) ||
		d["amount_cents"] != float64(365000) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want td-1 principal->principal_plus_interest 30/3->45/2 used 0 principal 365000 USD", d)
	}
}
