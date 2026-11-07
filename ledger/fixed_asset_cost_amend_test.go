package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendFixedAssetCostValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendFixedAssetCost("nope", 240000); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Errorf("unknown asset: err = %v, want ErrFixedAssetNotFound", err)
	}
	mustRegister(t, l, testAsset())
	orig, _ := l.GetFixedAsset("srv-001")

	// The new cost reuses the registration gate: it must be
	// positive (120000 here, salvage 0).
	for _, bad := range []int64{0, -1, -120000} {
		if _, err := l.AmendFixedAssetCost("srv-001", bad); !errors.Is(err, ErrInvalidAssetCost) {
			t.Errorf("cost %d: err = %v, want ErrInvalidAssetCost", bad, err)
		}
		got, _ := l.GetFixedAsset("srv-001")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("asset after cost %d rejection = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// The salvage must stay below the new cost: with salvage 60000
	// on a 120000 asset, a cost at or below the salvage is refused.
	asset := testAsset()
	asset.ID = "srv-002"
	asset.SalvageCents = 60000
	mustRegister(t, l, asset)
	orig2, _ := l.GetFixedAsset("srv-002")
	for _, bad := range []int64{60000, 59999, 1} {
		if _, err := l.AmendFixedAssetCost("srv-002", bad); !errors.Is(err, ErrInvalidSalvage) {
			t.Errorf("cost %d with salvage 60000: err = %v, want ErrInvalidSalvage", bad, err)
		}
		got, _ := l.GetFixedAsset("srv-002")
		if !reflect.DeepEqual(got, orig2) {
			t.Errorf("asset after cost %d rejection = %+v, want %+v (untouched)", bad, got, orig2)
		}
	}
	// One cent above the salvage is the smallest legal cost.
	if got, err := l.AmendFixedAssetCost("srv-002", 60001); err != nil || got.CostCents != 60001 {
		t.Errorf("AmendFixedAssetCost(60001) = %+v, %v, want cost 60001, nil", got, err)
	}

	// Amending to the current cost is a no-op, not a change.
	if got, err := l.AmendFixedAssetCost("srv-001", 120000); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-cost amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
}

func TestAmendFixedAssetCostPostedRejects(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// Book one month at 10000: the cost is now locked.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 1); total != 10000 {
		t.Fatalf("booked = %d, want 10000", total)
	}

	// Any revision fails closed, and the fail-closed gate runs
	// before the no-op check, so even a same-value amendment is
	// rejected.
	for _, cost := range []int64{240000, 120000, 60000} {
		if _, err := l.AmendFixedAssetCost("srv-001", cost); !errors.Is(err, ErrFixedAssetDepreciationPosted) {
			t.Errorf("cost %d on posted asset: err = %v, want ErrFixedAssetDepreciationPosted", cost, err)
		}
	}
	got, _ := l.GetFixedAsset("srv-001")
	if got.CostCents != 120000 {
		t.Errorf("cost after rejection = %d, want 120000 (untouched)", got.CostCents)
	}
}

func TestAmendFixedAssetCostReversedMonthStillRejects(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	first, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate: %v", err)
	}
	if _, _, err := l.ReverseEntry(first.ID, "rev-key-1"); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	// The reversal nets the accumulated total back to zero, but the
	// month was posted: the cost stays locked, following the
	// installment PostedPeriods precedent (posted periods count,
	// they are never un-posted).
	if acc, err := l.AccumulatedDepreciation("srv-001"); err != nil || acc != 0 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 0, nil", acc, err)
	}
	if _, err := l.AmendFixedAssetCost("srv-001", 240000); !errors.Is(err, ErrFixedAssetDepreciationPosted) {
		t.Errorf("cost amendment after reversed month: err = %v, want ErrFixedAssetDepreciationPosted", err)
	}
	got, _ := l.GetFixedAsset("srv-001")
	if got.CostCents != 120000 {
		t.Errorf("cost after rejection = %d, want 120000 (untouched)", got.CostCents)
	}
}

func TestAmendFixedAssetCostStraightLine(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustRegister(t, l, testAsset())
	orig, _ := l.GetFixedAsset("srv-001")

	amended, err := l.AmendFixedAssetCost("srv-001", 240000)
	if err != nil {
		t.Fatalf("AmendFixedAssetCost: %v", err)
	}
	if amended.CostCents != 240000 {
		t.Errorf("amended cost = %d, want 240000", amended.CostCents)
	}
	// Only the cost moves: salvage, life, method, currency, and the
	// (nonexistent) booked months are untouched.
	amended.CostCents = orig.CostCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended asset = %+v, want %+v except cost", amended, orig)
	}

	// The new cost governs from the first Depreciate:
	// (240000-0)/12 = 20000/month.
	got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || dup {
		t.Fatalf("Depreciate after amendment = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 20000 {
		t.Errorf("month 1 = %d, want 20000 (new base 240000 over 12 months)", got.AmountCents)
	}
	// The amendment itself posts nothing.
	if n := len(l.Entries()); n != 1 {
		t.Errorf("entries after amendment = %d, want 1 (amendment posts nothing)", n)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fixed_asset_cost_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["asset_id"] != "srv-001" || d["previous_cost_cents"] != float64(120000) || d["cost_cents"] != float64(240000) {
		t.Errorf("event details = %+v, want srv-001 120000->240000", d)
	}
}

func TestAmendFixedAssetCostDoubleDeclining(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.Method = DepreciationDoubleDeclining
	mustRegister(t, l, asset)

	if _, err := l.AmendFixedAssetCost("srv-001", 60000); err != nil {
		t.Fatalf("AmendFixedAssetCost: %v", err)
	}
	// Month 1 reads the live cost: book value 60000 * 2 / 12 =
	// 10000, against the new base of 60000.
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 1: %v", err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("month 1 = %d, want 10000 (bookValue 60000 * 2 / 12)", got.AmountCents)
	}
	if got.Memo != "depreciation:double-declining" {
		t.Fatalf("memo = %q, want depreciation:double-declining", got.Memo)
	}
}
