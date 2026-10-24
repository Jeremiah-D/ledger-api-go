package ledger

import (
	"errors"
	"testing"
	"time"
)

func TestSetRebateTiersValidation(t *testing.T) {
	l := New()
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 100, RebateBps: 50}}); !errors.Is(err, ErrInvalidRebateTier) {
		t.Errorf("first band not at 0: err=%v, want ErrInvalidRebateTier", err)
	}
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 0, RebateBps: 0}, {MinVolumeCents: 0, RebateBps: 50}}); !errors.Is(err, ErrInvalidRebateTier) {
		t.Errorf("duplicate minima: err=%v, want ErrInvalidRebateTier", err)
	}
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 0, RebateBps: 0}, {MinVolumeCents: 500, RebateBps: 50}, {MinVolumeCents: 400, RebateBps: 100}}); !errors.Is(err, ErrInvalidRebateTier) {
		t.Errorf("descending minima: err=%v, want ErrInvalidRebateTier", err)
	}
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 0, RebateBps: 10001}}); !errors.Is(err, ErrInvalidRebateTier) {
		t.Errorf("rate > 10000: err=%v, want ErrInvalidRebateTier", err)
	}
	// A 0 bps bottom band is valid: small makers earn nothing.
	tiers := []RebateTier{{MinVolumeCents: 0, RebateBps: 0}, {MinVolumeCents: 1_000_000, RebateBps: 50}, {MinVolumeCents: 10_000_000, RebateBps: 100}}
	if err := l.SetRebateTiers(tiers); err != nil {
		t.Fatalf("valid table: %v", err)
	}
	got := l.RebateTiers()
	if len(got) != 3 || got[2] != tiers[2] {
		t.Errorf("RebateTiers = %+v, want %+v", got, tiers)
	}
	got[0].RebateBps = 999
	if l.RebateTiers()[0].RebateBps != 0 {
		t.Errorf("RebateTiers accessor leaked the internal slice")
	}
	if err := l.SetRebateTiers(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if l.RebateTiers() != nil {
		t.Errorf("after clear RebateTiers = %+v, want nil", l.RebateTiers())
	}
}

func TestRebateQuoteAndWashExclusion(t *testing.T) {
	l := New()
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 0, RebateBps: 0}, {MinVolumeCents: 1_000_000, RebateBps: 50}}); err != nil {
		t.Fatalf("SetRebateTiers: %v", err)
	}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	// Below the threshold: 0 bps, no rebate.
	if counted, err := l.RecordMakerTrade("acct-maker", "acct-taker", 500_000, at); err != nil || !counted {
		t.Fatalf("RecordMakerTrade: counted=%v err=%v", counted, err)
	}
	// Wash trade: same account both sides — excluded, not an error.
	if counted, err := l.RecordMakerTrade("acct-maker", "acct-maker", 9_000_000, at); err != nil || counted {
		t.Fatalf("wash: counted=%v err=%v, want counted=false err=nil", counted, err)
	}
	q, err := l.RebateQuoteFor("acct-maker", "2026-10")
	if err != nil {
		t.Fatalf("RebateQuoteFor: %v", err)
	}
	if q.VolumeCents != 500_000 || q.TierIndex != 0 || q.RebateBps != 0 || q.RebateCents != 0 {
		t.Errorf("below-threshold quote = %+v, want volume 500000 tier 0 bps 0 rebate 0", q)
	}
	if q.WashExcludedCents != 9_000_000 || q.WashExcludedTrades != 1 {
		t.Errorf("wash disclosure = %d cents / %d trades, want 9000000 / 1", q.WashExcludedCents, q.WashExcludedTrades)
	}
	// Cross the threshold: floor(1_500_000 * 50 / 10000) = 7500.
	if _, err := l.RecordMakerTrade("acct-maker", "acct-taker", 1_000_000, at); err != nil {
		t.Fatalf("RecordMakerTrade: %v", err)
	}
	q, err = l.RebateQuoteFor("acct-maker", "2026-10")
	if err != nil {
		t.Fatalf("RebateQuoteFor: %v", err)
	}
	if q.VolumeCents != 1_500_000 || q.TierIndex != 1 || q.RebateBps != 50 || q.RebateCents != 7500 {
		t.Errorf("above-threshold quote = %+v, want volume 1500000 tier 1 bps 50 rebate 7500", q)
	}
	// Other months are independent.
	vol, err := l.MakerVolume("acct-maker", "2026-11")
	if err != nil || vol != 0 {
		t.Errorf("MakerVolume 2026-11 = %d err=%v, want 0 nil", vol, err)
	}
	if _, err := l.RebateQuoteFor("acct-maker", "2026-13"); !errors.Is(err, ErrInvalidRebateMonth) {
		t.Errorf("bad month: err=%v, want ErrInvalidRebateMonth", err)
	}
	if _, err := l.RecordMakerTrade("", "acct-taker", 100, at); !errors.Is(err, ErrInvalidMakerTrade) {
		t.Errorf("empty maker: err=%v, want ErrInvalidMakerTrade", err)
	}
	if _, err := l.RecordMakerTrade("acct-maker", "acct-taker", 0, at); !errors.Is(err, ErrInvalidMakerTrade) {
		t.Errorf("zero amount: err=%v, want ErrInvalidMakerTrade", err)
	}
}

func TestSettleMakerRebatesIdempotentAndClosesMonth(t *testing.T) {
	l := New()
	if _, err := l.SettleMakerRebates("2026-10"); !errors.Is(err, ErrRebateTiersNotConfigured) {
		t.Fatalf("settle without tiers: err=%v, want ErrRebateTiersNotConfigured", err)
	}
	if err := l.SetRebateTiers([]RebateTier{{MinVolumeCents: 0, RebateBps: 100}}); err != nil {
		t.Fatalf("SetRebateTiers: %v", err)
	}
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	if _, err := l.RecordMakerTrade("acct-b", "acct-taker", 200_000, at); err != nil {
		t.Fatalf("RecordMakerTrade b: %v", err)
	}
	if _, err := l.RecordMakerTrade("acct-a", "acct-taker", 100_000, at); err != nil {
		t.Fatalf("RecordMakerTrade a: %v", err)
	}
	// Wash-only account still appears in the schedule (disclosed).
	if _, err := l.RecordMakerTrade("acct-w", "acct-w", 50_000, at); err != nil {
		t.Fatalf("RecordMakerTrade wash: %v", err)
	}
	schedule, err := l.SettleMakerRebates("2026-10")
	if err != nil {
		t.Fatalf("SettleMakerRebates: %v", err)
	}
	if len(schedule) != 3 || schedule[0].Account != "acct-a" || schedule[1].Account != "acct-b" || schedule[2].Account != "acct-w" {
		t.Fatalf("schedule order = %+v, want a, b, w sorted", schedule)
	}
	if schedule[0].RebateCents != 1000 || schedule[1].RebateCents != 2000 || schedule[2].RebateCents != 0 {
		t.Errorf("schedule rebates = %d/%d/%d, want 1000/2000/0", schedule[0].RebateCents, schedule[1].RebateCents, schedule[2].RebateCents)
	}
	// Month is closed: further fills rejected, second settle replays.
	if _, err := l.RecordMakerTrade("acct-a", "acct-taker", 100, at); !errors.Is(err, ErrRebateMonthSettled) {
		t.Errorf("post-settlement fill: err=%v, want ErrRebateMonthSettled", err)
	}
	again, err := l.SettleMakerRebates("2026-10")
	if err != nil || len(again) != 3 || again[0].RebateCents != 1000 {
		t.Errorf("second settle = %+v err=%v, want stored schedule", again, err)
	}
	stored, ok, err := l.MakerRebateSettlement("2026-10")
	if err != nil || !ok || len(stored) != 3 {
		t.Errorf("MakerRebateSettlement = %+v ok=%v err=%v", stored, ok, err)
	}
	if _, ok, err := l.MakerRebateSettlement("2026-11"); err != nil || ok {
		t.Errorf("unsettled month: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestRebateQuoteWithoutTiers(t *testing.T) {
	l := New()
	if _, err := l.RebateQuoteFor("acct-a", "2026-10"); !errors.Is(err, ErrRebateTiersNotConfigured) {
		t.Errorf("quote without tiers: err=%v, want ErrRebateTiersNotConfigured", err)
	}
}
