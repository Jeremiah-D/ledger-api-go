package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSetInterestTiersValidation(t *testing.T) {
	l := New()
	// First band must start at zero.
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 100, RateBps: 500}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("first band not at 0: err=%v, want ErrInvalidInterestTier", err)
	}
	// Minima must be strictly ascending.
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 0, RateBps: 300}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("duplicate minima: err=%v, want ErrInvalidInterestTier", err)
	}
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 500, RateBps: 300}, {MinCents: 400, RateBps: 200}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("descending minima: err=%v, want ErrInvalidInterestTier", err)
	}
	// Negative minima rejected.
	if err := l.SetInterestTiers([]InterestTier{{MinCents: -1, RateBps: 500}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("negative minimum: err=%v, want ErrInvalidInterestTier", err)
	}
	// Rates outside 1..10000 rejected.
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 0}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("zero rate: err=%v, want ErrInvalidInterestTier", err)
	}
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 10001}}); !errors.Is(err, ErrInvalidInterestTier) {
		t.Errorf("rate > 10000: err=%v, want ErrInvalidInterestTier", err)
	}
	// A valid table installs and reads back.
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	if err := l.SetInterestTiers(tiers); err != nil {
		t.Fatalf("valid table: %v", err)
	}
	got := l.InterestTiers()
	if len(got) != 2 || got[0] != tiers[0] || got[1] != tiers[1] {
		t.Errorf("InterestTiers = %+v, want %+v", got, tiers)
	}
	// Mutating the returned slice must not affect the ledger's copy.
	got[0].RateBps = 999
	if l.InterestTiers()[0].RateBps != 500 {
		t.Errorf("InterestTiers accessor leaked the internal slice")
	}
	// An empty table clears the ladder back to unconfigured.
	if err := l.SetInterestTiers(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if l.InterestTiers() != nil {
		t.Errorf("after clear InterestTiers = %+v, want nil", l.InterestTiers())
	}
}

func TestTieredInterestCentsMath(t *testing.T) {
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}
	// Principal 1.5M: band 1 = floor(1_000_000*500/10000/365) = 136,
	// band 2 = floor(500_000*300/10000/365) = 41, total 177.
	got, err := tieredInterestCents(1_500_000, tiers)
	if err != nil {
		t.Fatalf("tieredInterestCents: %v", err)
	}
	if got != 177 {
		t.Errorf("tieredInterestCents(1.5M) = %d, want 177", got)
	}
	// Principal exactly at a band boundary: only the lower band fires.
	got, err = tieredInterestCents(1_000_000, tiers)
	if err != nil {
		t.Fatalf("tieredInterestCents: %v", err)
	}
	if got != 136 {
		t.Errorf("tieredInterestCents(1M) = %d, want 136", got)
	}
	// Single band: floor(3_650_000*1000/10000/365) = 1000 exactly.
	got, err = tieredInterestCents(3_650_000, []InterestTier{{MinCents: 0, RateBps: 1000}})
	if err != nil {
		t.Fatalf("tieredInterestCents: %v", err)
	}
	if got != 1000 {
		t.Errorf("tieredInterestCents single band = %d, want 1000", got)
	}
	// Open-ended top band: the remainder all prices at the top rate.
	got, err = tieredInterestCents(3_000_000, tiers)
	if err != nil {
		t.Fatalf("tieredInterestCents: %v", err)
	}
	// band1 136 + band2 floor(2_000_000*300/10000/365)=floor(60000/365)=164 → 300.
	if got != 300 {
		t.Errorf("tieredInterestCents(3M) = %d, want 300", got)
	}
	// Dust principal rounds to zero.
	got, err = tieredInterestCents(1, tiers)
	if err != nil {
		t.Fatalf("tieredInterestCents: %v", err)
	}
	if got != 0 {
		t.Errorf("tieredInterestCents(1) = %d, want 0", got)
	}
}

func TestAccrueInterestTiered(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// No table installed: tiered accrual is rejected.
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 1_500_000, asof, 0); !errors.Is(err, ErrInterestTiersNotConfigured) {
		t.Fatalf("no table: err=%v, want ErrInterestTiersNotConfigured", err)
	}
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}); err != nil {
		t.Fatalf("set tiers: %v", err)
	}
	// Negative principals are overdraft territory, not deposit bands.
	if _, _, err := l.AccrueInterestTiered("exp", "pay", -100, asof, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("negative principal: err=%v, want ErrNonPositiveAmount", err)
	}
	// Dust books nothing.
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 1, asof, 0); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("dust: err=%v, want ErrNonPositiveAmount", err)
	}
	e, dup, err := l.AccrueInterestTiered("exp", "pay", 1_500_000, asof, 0)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if dup {
		t.Errorf("first accrual: duplicate=true, want false")
	}
	if e.AmountCents != 177 {
		t.Errorf("amount = %d, want 177", e.AmountCents)
	}
	if e.Memo != "accrual:interest-tier" {
		t.Errorf("memo = %q, want accrual:interest-tier", e.Memo)
	}
	if !strings.HasPrefix(e.ID, "accrual/interest-tier/exp/pay/2026-10-17") {
		t.Errorf("entry ID = %q, want accrual/interest-tier/exp/pay/2026-10-17 prefix", e.ID)
	}
	// Idempotent per calendar day.
	e2, dup, err := l.AccrueInterestTiered("exp", "pay", 1_500_000, asof, 0)
	if err != nil || !dup || e2.ID != e.ID {
		t.Errorf("replay: err=%v dup=%v id=%q, want replay of %q", err, dup, e2.ID, e.ID)
	}
	// A new day gets a fresh entry.
	next := asof.Add(24 * time.Hour)
	e3, dup, err := l.AccrueInterestTiered("exp", "pay", 1_500_000, next, 0)
	if err != nil || dup || e3.ID == e.ID {
		t.Errorf("next day: err=%v dup=%v id=%q, want a fresh entry", err, dup, e3.ID)
	}
	// The tiered kind is queryable and separate from the flat kind.
	total, err := l.AccruedTotal("exp", "pay", AccrualKindInterestTiered, time.Time{}, next.Add(time.Hour))
	if err != nil || total != 354 {
		t.Errorf("AccruedTotal(interest-tier) = %d, %v; want 354", total, err)
	}
	flat, err := l.AccruedTotal("exp", "pay", AccrualKindInterest, time.Time{}, next.Add(time.Hour))
	if err != nil || flat != 0 {
		t.Errorf("AccruedTotal(interest) = %d, %v; want 0", flat, err)
	}
	// Reversible through the shared path.
	rev, dup, err := l.ReverseAccrual(e.ID, next)
	if err != nil || dup {
		t.Errorf("ReverseAccrual: err=%v dup=%v", err, dup)
	}
	if rev.AmountCents != 177 || rev.Memo != "accrual:reversal" {
		t.Errorf("reversal = %+v, want mirror of 177", rev)
	}
}

func TestAccrueInterestTieredCap(t *testing.T) {
	l := New()
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 500}}); err != nil {
		t.Fatalf("set tiers: %v", err)
	}
	asof := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// floor(3_650_000*500/10000/365) = 500.
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 3_650_000, asof, 499); !errors.Is(err, ErrAccrualCapExceeded) {
		t.Errorf("over cap: err=%v, want ErrAccrualCapExceeded", err)
	}
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 3_650_000, asof, 500); err != nil {
		t.Errorf("at cap: %v", err)
	}
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 3_650_000, asof.Add(24*time.Hour), 500); !errors.Is(err, ErrAccrualCapExceeded) {
		t.Errorf("second day over cumulative cap: err=%v, want ErrAccrualCapExceeded", err)
	}
}

func TestInterestTiersAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	tiers := []InterestTier{{MinCents: 0, RateBps: 500}}
	if err := l.SetInterestTiers(tiers); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Re-installing the identical table is a silent no-op.
	if err := l.SetInterestTiers(tiers); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := l.SetInterestTiers(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	found := map[string]int{}
	for _, e := range events {
		found[e.Op]++
	}
	if found["interest_tiers_set"] != 1 {
		t.Errorf("interest_tiers_set events = %d, want 1", found["interest_tiers_set"])
	}
	if found["interest_tiers_cleared"] != 1 {
		t.Errorf("interest_tiers_cleared events = %d, want 1", found["interest_tiers_cleared"])
	}
}

func TestInterestTiersSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetInterestTiers([]InterestTier{{MinCents: 0, RateBps: 500}, {MinCents: 1_000_000, RateBps: 300}}); err != nil {
		t.Fatalf("set tiers: %v", err)
	}
	asof := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.AccrueInterestTiered("exp", "pay", 1_500_000, asof, 0); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !tiersEqual(l.InterestTiers(), l2.InterestTiers()) {
		t.Errorf("restored tiers = %+v, want %+v", l2.InterestTiers(), l.InterestTiers())
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Errorf("restored ledger not equal to the original")
	}
	// The restored ledger prices bands identically.
	e, _, err := l2.AccrueInterestTiered("exp", "pay", 1_500_000, asof.Add(24*time.Hour), 0)
	if err != nil {
		t.Fatalf("accrue on restored ledger: %v", err)
	}
	if e.AmountCents != 177 {
		t.Errorf("restored accrual amount = %d, want 177", e.AmountCents)
	}
}
