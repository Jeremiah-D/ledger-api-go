package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// seedTieredTermDepositLedger builds a ledger with a funded account and
// the term-deposit interest-expense account configured, like
// seedTermDepositLedger but with a larger balance so multi-band tier
// tests can lock meaningful principals.
func seedTieredTermDepositLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 5_000_000)
	return l
}

func TestOpenTermDepositTiered_Validation(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	good := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	cases := []struct {
		name string
		td   TermDeposit
		want error
	}{
		{"both flat and tiers", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateBps: 500, RateTiers: good}, ErrTermDepositRateTiersConflict},
		{"tier not starting at zero", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateTiers: []InterestTier{{MinCents: 100, RateBps: 500}}}, ErrInvalidInterestTier},
		{"non-ascending minima", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateTiers: []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 0, RateBps: 300}}}, ErrInvalidInterestTier},
		{"tier rate out of range", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateTiers: []InterestTier{{MinCents: 0, RateBps: 10001}}}, ErrInvalidInterestTier},
		{"tier zero rate", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1, RateTiers: []InterestTier{{MinCents: 0, RateBps: 0}}}, ErrInvalidInterestTier},
		{"zero flat rate without tiers", TermDeposit{ID: "x", Account: "saver", AmountCents: 100, TermDays: 1}, ErrInvalidTermDepositRate},
	}
	for _, c := range cases {
		if _, _, err := l.OpenTermDeposit(c.td); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}

	// A valid ladder opens and is stored.
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-ok", Account: "saver", AmountCents: 1_500_000,
		TermDays: 30, RateTiers: good,
	})
	if err != nil {
		t.Fatalf("valid tiered open: %v", err)
	}
	if td.RateBps != 0 {
		t.Errorf("RateBps = %d, want 0 for tiered deposit", td.RateBps)
	}
	if !tiersEqual(td.RateTiers, good) {
		t.Errorf("RateTiers = %+v, want %+v", td.RateTiers, good)
	}
	got, ok := l.GetTermDeposit("td-tier-ok")
	if !ok || !tiersEqual(got.RateTiers, good) {
		t.Errorf("stored RateTiers = %+v, want %+v", got.RateTiers, good)
	}
	// The caller's slice must not alias the stored record.
	good[0].RateBps = 999
	got, _ = l.GetTermDeposit("td-tier-ok")
	if got.RateTiers[0].RateBps == 999 {
		t.Error("stored ladder aliases caller's slice")
	}
}

func TestAccrueTermInterest_Tiered(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-acc", Account: "saver", AmountCents: 1_500_000,
		TermDays: 30, RateTiers: tiers,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	asof := td.OpenedAt.Truncate(24 * time.Hour)
	e, dup, err := l.AccrueTermInterest("td-tier-acc", asof)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false on first accrual")
	}
	// Hand-computed: band [0,1M) at 500bps -> floor(1_000_000*500/10000/365)
	// = floor(50000/365) = 136; band [1M,1.5M) at 300bps ->
	// floor(500_000*300/10000/365) = floor(15000/365) = 41. Total 177.
	if e.AmountCents != 177 {
		t.Errorf("amount = %d, want 177", e.AmountCents)
	}
	// The deterministic entry ID is unchanged by the rate mode.
	wantID := "term-interest/td-tier-acc/" + asof.UTC().Format("2006-01-02")
	if e.ID != wantID {
		t.Errorf("entry ID = %q, want %q", e.ID, wantID)
	}
	if e.DebitAccount != "saver" || e.CreditAccount != "td-interest-expense" {
		t.Errorf("legs = %s/%s, want saver/td-interest-expense", e.DebitAccount, e.CreditAccount)
	}
	// Same-day re-accrual replays the original row.
	e2, dup2, err := l.AccrueTermInterest("td-tier-acc", asof)
	if err != nil {
		t.Fatalf("replay accrue: %v", err)
	}
	if !dup2 || e2.ID != e.ID {
		t.Errorf("replay = (%s, dup=%v), want (%s, dup=true)", e2.ID, dup2, e.ID)
	}
}

func TestAccrueTermInterest_TieredPerBandFloor(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	// Per-band flooring, not floor-of-sum: band1 [0,50k) at 500bps ->
	// floor(floor(50_000*500/10000)/365) = floor(2500/365) = 6;
	// band2 [50k,100k) at 10000bps -> floor(floor(50_000*10000/10000)/365)
	// = floor(50000/365) = 136. The ladder books 6+136 = 142; flooring
	// the combined annual figure would book floor(52500/365) = 143.
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-floor", Account: "saver", AmountCents: 100_000,
		TermDays: 30,
		RateTiers: []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 50_000, RateBps: 10000}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	e, _, err := l.AccrueTermInterest("td-tier-floor", td.OpenedAt)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if e.AmountCents != 142 {
		t.Errorf("amount = %d, want 142 (per-band floor)", e.AmountCents)
	}
}

func TestTermDepositRollover_PreservesTiers(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-roll", Account: "saver", AmountCents: 1_500_000,
		TermDays: 30, RateTiers: tiers,
		Rollover: &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if matured, _ := l.SweepTermDeposits(td.MaturesAt); matured != 1 {
		t.Fatalf("matured = %d, want 1", matured)
	}
	rolled, ok := l.GetTermDeposit("td-tier-roll/roll/1")
	if !ok {
		t.Fatal("rollover deposit not found")
	}
	if !tiersEqual(rolled.RateTiers, tiers) {
		t.Errorf("rolled RateTiers = %+v, want %+v", rolled.RateTiers, tiers)
	}
	if rolled.RateBps != 0 {
		t.Errorf("rolled RateBps = %d, want 0", rolled.RateBps)
	}
	// The rolled deposit accrues tiered interest too.
	if _, _, err := l.AccrueTermInterest(rolled.ID, rolled.OpenedAt); err != nil {
		t.Fatalf("accrue on rolled deposit: %v", err)
	}
	if got := l.TermDepositInterestAccrued(rolled.ID); got != 177 {
		t.Errorf("rolled accrued = %d, want 177", got)
	}
}

func TestReconcile_DisclosesRateTiers(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-rep", Account: "saver", AmountCents: 100_000,
		TermDays: 30, RateTiers: tiers,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-flat-rep", Account: "saver", AmountCents: 100_000,
		TermDays: 30, RateBps: 500,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	report := l.Reconcile(time.Now())
	byID := map[string]TermDepositReport{}
	for _, r := range report.TermDeposits {
		byID[r.ID] = r
	}
	tiered, ok := byID["td-tier-rep"]
	if !ok {
		t.Fatal("tiered deposit missing from Reconcile")
	}
	if !tiersEqual(tiered.RateTiers, tiers) {
		t.Errorf("report RateTiers = %+v, want %+v", tiered.RateTiers, tiers)
	}
	if flat, ok := byID["td-flat-rep"]; !ok || len(flat.RateTiers) != 0 {
		t.Errorf("flat deposit report RateTiers = %+v, want empty", flat.RateTiers)
	}
}

func TestTieredDeposit_SnapshotRoundTrip(t *testing.T) {
	l := seedTieredTermDepositLedger(t)
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-tier-snap", Account: "saver", AmountCents: 1_500_000,
		TermDays: 30, RateTiers: tiers, IdempotencyKey: "td-tier-snap-key",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	l2, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, ok := l2.GetTermDeposit("td-tier-snap")
	if !ok || !tiersEqual(got.RateTiers, tiers) {
		t.Errorf("restored RateTiers = %+v, want %+v", got.RateTiers, tiers)
	}
	// The idempotency key index survives the round trip too.
	td, dup, err := l2.OpenTermDeposit(TermDeposit{
		ID: "td-tier-snap-2", Account: "saver", AmountCents: 100,
		TermDays: 30, RateBps: 500, IdempotencyKey: "td-tier-snap-key",
	})
	if err != nil {
		t.Fatalf("replay open: %v", err)
	}
	if !dup || td.ID != "td-tier-snap" {
		t.Errorf("replay = (%s, dup=%v), want (td-tier-snap, dup=true)", td.ID, dup)
	}
}
