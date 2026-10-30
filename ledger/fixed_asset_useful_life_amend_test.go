package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// depreciateMonths books count consecutive months of depreciation
// starting at year/month, returning the total booked.
func depreciateMonths(t *testing.T, l *Ledger, assetID string, year int, startMonth time.Month, count int) int64 {
	t.Helper()
	var total int64
	for i := 0; i < count; i++ {
		asof := time.Date(year, startMonth+time.Month(i), 15, 12, 0, 0, 0, time.UTC)
		got, dup, err := l.Depreciate(assetID, "depr-expense", "accum-depr", asof)
		if err != nil || dup {
			t.Fatalf("Depreciate month %d = dup=%v err=%v", i+1, dup, err)
		}
		total += got.AmountCents
	}
	return total
}

func TestAmendFixedAssetUsefulLifeValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendFixedAssetUsefulLife("nope", 24); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Errorf("unknown asset: err = %v, want ErrFixedAssetNotFound", err)
	}
	mustRegister(t, l, testAsset())
	orig, _ := l.GetFixedAsset("srv-001")

	// The new life reuses the registration gate: it must be positive.
	for _, bad := range []int64{0, -1, -12} {
		if _, err := l.AmendFixedAssetUsefulLife("srv-001", bad); !errors.Is(err, ErrInvalidUsefulLife) {
			t.Errorf("months %d: err = %v, want ErrInvalidUsefulLife", bad, err)
		}
		got, _ := l.GetFixedAsset("srv-001")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("asset after months %d rejection = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// Amending to the current life is a no-op, not a change.
	if got, err := l.AmendFixedAssetUsefulLife("srv-001", 12); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-life amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
}

func TestAmendFixedAssetUsefulLifeFullyDepreciated(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// 120000 / 12 = 10000/month; twelve months books the whole base.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 12); total != 120000 {
		t.Fatalf("booked total = %d, want 120000", total)
	}
	if got, err := l.AccumulatedDepreciation("srv-001"); err != nil || got != 120000 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 120000, nil", got, err)
	}

	// No future depreciation left to govern: any revision fails
	// closed, and the fail-closed gate runs before the no-op check,
	// so even a same-value amendment is rejected.
	for _, months := range []int64{24, 12} {
		if _, err := l.AmendFixedAssetUsefulLife("srv-001", months); !errors.Is(err, ErrFixedAssetFullyDepreciated) {
			t.Errorf("months %d on fully depreciated asset: err = %v, want ErrFixedAssetFullyDepreciated", months, err)
		}
	}
	got, _ := l.GetFixedAsset("srv-001")
	if got.UsefulLifeMonths != 12 {
		t.Errorf("life after rejection = %d, want 12 (untouched)", got.UsefulLifeMonths)
	}
}

func TestAmendFixedAssetUsefulLifeStraightLine(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustRegister(t, l, testAsset())

	// Month 1 books 10000 at the original 12-month life.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 10000 {
		t.Fatalf("month 1 = %d, want 10000", total)
	}
	orig, _ := l.GetFixedAsset("srv-001")

	amended, err := l.AmendFixedAssetUsefulLife("srv-001", 24)
	if err != nil {
		t.Fatalf("AmendFixedAssetUsefulLife: %v", err)
	}
	if amended.UsefulLifeMonths != 24 {
		t.Errorf("amended life = %d, want 24", amended.UsefulLifeMonths)
	}
	// Only the life moves: cost, salvage, method, currency, and the
	// booked month are untouched.
	amended.UsefulLifeMonths = orig.UsefulLifeMonths
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended asset = %+v, want %+v except life", amended, orig)
	}

	// The new life governs from the next Depreciate: 120000/24 =
	// 5000/month now, and the already-booked March row stays 10000.
	got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || dup {
		t.Fatalf("Depreciate after amendment = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 5000 {
		t.Errorf("month 2 = %d, want 5000 (new 24-month life)", got.AmountCents)
	}
	// The amendment itself posts nothing.
	if n := len(l.Entries()); n != 2 {
		t.Errorf("entries after amendment = %d, want 2 (amendment posts nothing)", n)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fixed_asset_useful_life_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["asset_id"] != "srv-001" || d["previous_useful_life_months"] != float64(12) || d["useful_life_months"] != float64(24) {
		t.Errorf("event details = %+v, want srv-001 12->24", d)
	}
}

func TestAmendFixedAssetUsefulLifeShortenedBelowBooked(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// Book 3 months at 10000 each: 30000 booked, 90000 remaining.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 3); total != 30000 {
		t.Fatalf("booked = %d, want 30000", total)
	}

	// Shorten the life to 2 months — below the months already
	// booked. Straight-line now charges 120000/2 = 60000/month,
	// still capped at the remaining base, so the asset converges
	// exactly to salvage instead of over-depreciating.
	if _, err := l.AmendFixedAssetUsefulLife("srv-001", 2); err != nil {
		t.Fatalf("AmendFixedAssetUsefulLife: %v", err)
	}
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 4: %v", err)
	}
	if got.AmountCents != 60000 {
		t.Fatalf("month 4 = %d, want 60000 (capped by remaining base)", got.AmountCents)
	}
	// Remaining base is now 30000; the next month trues up exactly.
	got, _, err = l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 5: %v", err)
	}
	if got.AmountCents != 30000 {
		t.Fatalf("month 5 = %d, want 30000 (dust true-up to salvage)", got.AmountCents)
	}
	if acc, err := l.AccumulatedDepreciation("srv-001"); err != nil || acc != 120000 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 120000, nil", acc, err)
	}
	// Fully depreciated now: a further month books nothing.
	empty, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || empty.AmountCents != 0 {
		t.Fatalf("Depreciate after full = %+v, %v, want empty entry, nil", empty, err)
	}
}

func TestAmendFixedAssetUsefulLifeDoubleDeclining(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.Method = DepreciationDoubleDeclining
	mustRegister(t, l, asset)
	// Month 1 at the original 12-month life: 120000*2/12 = 20000.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 20000 {
		t.Fatalf("month 1 = %d, want 20000", total)
	}

	if _, err := l.AmendFixedAssetUsefulLife("srv-001", 6); err != nil {
		t.Fatalf("AmendFixedAssetUsefulLife: %v", err)
	}
	// Month 2 reads the live life: book value 100000 * 2 / 6 = 33333.
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 2: %v", err)
	}
	if got.AmountCents != 33333 {
		t.Fatalf("month 2 = %d, want 33333 (bookValue 100000 * 2 / 6)", got.AmountCents)
	}
	if got.Memo != "depreciation:double-declining" {
		t.Fatalf("memo = %q, want depreciation:double-declining", got.Memo)
	}
}
