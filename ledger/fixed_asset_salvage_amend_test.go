package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendFixedAssetSalvageValueValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendFixedAssetSalvageValue("nope", 1000); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Errorf("unknown asset: err = %v, want ErrFixedAssetNotFound", err)
	}
	mustRegister(t, l, testAsset())
	orig, _ := l.GetFixedAsset("srv-001")

	// The new salvage reuses the registration gate: 0 <= salvage
	// < cost (120000 here).
	for _, bad := range []int64{-1, -60000, 120000, 120001} {
		if _, err := l.AmendFixedAssetSalvageValue("srv-001", bad); !errors.Is(err, ErrInvalidSalvage) {
			t.Errorf("salvage %d: err = %v, want ErrInvalidSalvage", bad, err)
		}
		got, _ := l.GetFixedAsset("srv-001")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("asset after salvage %d rejection = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// Amending to the current salvage is a no-op, not a change.
	if got, err := l.AmendFixedAssetSalvageValue("srv-001", 0); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-salvage amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
}

func TestAmendFixedAssetSalvageValueFullyDepreciated(t *testing.T) {
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
	for _, salvage := range []int64{60000, 0} {
		if _, err := l.AmendFixedAssetSalvageValue("srv-001", salvage); !errors.Is(err, ErrFixedAssetFullyDepreciated) {
			t.Errorf("salvage %d on fully depreciated asset: err = %v, want ErrFixedAssetFullyDepreciated", salvage, err)
		}
	}
	got, _ := l.GetFixedAsset("srv-001")
	if got.SalvageCents != 0 {
		t.Errorf("salvage after rejection = %d, want 0 (untouched)", got.SalvageCents)
	}
}

func TestAmendFixedAssetSalvageValueRemainingBaseBoundary(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// Book 9 months at 10000 each: 90000 booked, 30000 remaining
	// under the original salvage of 0.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 9); total != 90000 {
		t.Fatalf("booked = %d, want 90000", total)
	}

	// The gate is evaluated against the NEW salvage: raising it to
	// 30000 leaves cost - 30000 - 90000 = 0 remaining base, so the
	// revision is rejected and the salvage is untouched.
	if _, err := l.AmendFixedAssetSalvageValue("srv-001", 30000); !errors.Is(err, ErrFixedAssetFullyDepreciated) {
		t.Errorf("salvage 30000: err = %v, want ErrFixedAssetFullyDepreciated", err)
	}
	if got, _ := l.GetFixedAsset("srv-001"); got.SalvageCents != 0 {
		t.Errorf("salvage after rejection = %d, want 0 (untouched)", got.SalvageCents)
	}

	// One cent lower leaves exactly 1 cent of future depreciation,
	// so the revision is allowed.
	amended, err := l.AmendFixedAssetSalvageValue("srv-001", 29999)
	if err != nil {
		t.Fatalf("AmendFixedAssetSalvageValue(29999): %v", err)
	}
	if amended.SalvageCents != 29999 {
		t.Errorf("amended salvage = %d, want 29999", amended.SalvageCents)
	}
}

func TestAmendFixedAssetSalvageValueStraightLine(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustRegister(t, l, testAsset())

	// Month 1 books 10000 at the original salvage of 0.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 10000 {
		t.Fatalf("month 1 = %d, want 10000", total)
	}
	orig, _ := l.GetFixedAsset("srv-001")

	amended, err := l.AmendFixedAssetSalvageValue("srv-001", 60000)
	if err != nil {
		t.Fatalf("AmendFixedAssetSalvageValue: %v", err)
	}
	if amended.SalvageCents != 60000 {
		t.Errorf("amended salvage = %d, want 60000", amended.SalvageCents)
	}
	// Only the salvage moves: cost, life, method, currency, and the
	// booked month are untouched.
	amended.SalvageCents = orig.SalvageCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended asset = %+v, want %+v except salvage", amended, orig)
	}

	// The new salvage governs from the next Depreciate:
	// (120000-60000)/12 = 5000/month now, and the already-booked
	// March row stays 10000.
	got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || dup {
		t.Fatalf("Depreciate after amendment = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 5000 {
		t.Errorf("month 2 = %d, want 5000 (new base 60000 over 12 months)", got.AmountCents)
	}
	// The amendment itself posts nothing.
	if n := len(l.Entries()); n != 2 {
		t.Errorf("entries after amendment = %d, want 2 (amendment posts nothing)", n)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fixed_asset_salvage_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["asset_id"] != "srv-001" || d["previous_salvage_cents"] != float64(0) || d["salvage_cents"] != float64(60000) {
		t.Errorf("event details = %+v, want srv-001 0->60000", d)
	}
}

func TestAmendFixedAssetSalvageValueReopensFullyDepreciated(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.SalvageCents = 60000
	mustRegister(t, l, asset)
	// Base 60000 over 12 months: 5000/month; twelve months books
	// the whole base — fully depreciated under salvage 60000.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 12); total != 60000 {
		t.Fatalf("booked total = %d, want 60000", total)
	}

	// Raising the salvage further leaves a negative remaining base
	// under the new salvage, so it is rejected.
	if _, err := l.AmendFixedAssetSalvageValue("srv-001", 90000); !errors.Is(err, ErrFixedAssetFullyDepreciated) {
		t.Errorf("salvage 90000: err = %v, want ErrFixedAssetFullyDepreciated", err)
	}

	// Lowering the salvage to 0 reopens the base: cost - 0 - 60000
	// = 60000 of future depreciation, so the revision is allowed —
	// the gate is evaluated against the post-amendment base.
	if _, err := l.AmendFixedAssetSalvageValue("srv-001", 0); err != nil {
		t.Fatalf("AmendFixedAssetSalvageValue(0): %v", err)
	}
	// The next month books 10000 again: (120000-0)/12, with the
	// already-booked 60000 untouched.
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate after reopening: %v", err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("month after reopening = %d, want 10000 (new base 120000 over 12 months)", got.AmountCents)
	}
	if acc, err := l.AccumulatedDepreciation("srv-001"); err != nil || acc != 70000 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 70000, nil", acc, err)
	}
}

func TestAmendFixedAssetSalvageValueConvergesToNewSalvage(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// Book 3 months at 10000 each: 30000 booked, 90000 remaining
	// under the original salvage of 0.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 3); total != 30000 {
		t.Fatalf("booked = %d, want 30000", total)
	}

	// Raise the salvage to 60000: the remaining base shrinks to
	// 30000. Straight-line now charges (120000-60000)/12 = 5000
	// per month, so six more months converge exactly to the new
	// salvage instead of over-depreciating.
	if _, err := l.AmendFixedAssetSalvageValue("srv-001", 60000); err != nil {
		t.Fatalf("AmendFixedAssetSalvageValue: %v", err)
	}
	if total := depreciateMonths(t, l, "srv-001", 2026, time.April, 6); total != 30000 {
		t.Fatalf("months 4-9 = %d, want 30000 (6 x 5000)", total)
	}
	if acc, err := l.AccumulatedDepreciation("srv-001"); err != nil || acc != 60000 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 60000, nil", acc, err)
	}
	// Fully depreciated at the new salvage now: a further month
	// books nothing.
	empty, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || empty.AmountCents != 0 {
		t.Fatalf("Depreciate after full = %+v, %v, want empty entry, nil", empty, err)
	}
}

func TestAmendFixedAssetSalvageValueDoubleDeclining(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.Method = DepreciationDoubleDeclining
	mustRegister(t, l, asset)
	// Month 1 at the original salvage of 0: 120000*2/12 = 20000.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 20000 {
		t.Fatalf("month 1 = %d, want 20000", total)
	}

	if _, err := l.AmendFixedAssetSalvageValue("srv-001", 60000); err != nil {
		t.Fatalf("AmendFixedAssetSalvageValue: %v", err)
	}
	// Month 2 reads the live salvage: book value 100000 * 2 / 12 =
	// 16666, still under the new remaining base (60000-20000).
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 2: %v", err)
	}
	if got.AmountCents != 16666 {
		t.Fatalf("month 2 = %d, want 16666 (bookValue 100000 * 2 / 12)", got.AmountCents)
	}
	if got.Memo != "depreciation:double-declining" {
		t.Fatalf("memo = %q, want depreciation:double-declining", got.Memo)
	}
}
