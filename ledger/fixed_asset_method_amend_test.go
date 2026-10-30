package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendFixedAssetDepreciationMethodValidation(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.AmendFixedAssetDepreciationMethod("nope", DepreciationDoubleDeclining); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Errorf("unknown asset: err = %v, want ErrFixedAssetNotFound", err)
	}
	mustRegister(t, l, testAsset())
	orig, _ := l.GetFixedAsset("srv-001")

	// The new method reuses the registration gate: only
	// "straight-line" and "double-declining" are accepted.
	for _, bad := range []string{"", "declining", "Straight-Line", "sum-of-years", "double_declining"} {
		if _, err := l.AmendFixedAssetDepreciationMethod("srv-001", bad); !errors.Is(err, ErrInvalidDepreciationMethod) {
			t.Errorf("method %q: err = %v, want ErrInvalidDepreciationMethod", bad, err)
		}
		got, _ := l.GetFixedAsset("srv-001")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("asset after method %q rejection = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// Amending to the current method is a no-op, not a change.
	if got, err := l.AmendFixedAssetDepreciationMethod("srv-001", DepreciationStraightLine); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-method amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op and the rejections write no audit events.
	if events := auditEventsByOp(t, dir)["fixed_asset_method_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op and rejections are silent)", len(events))
	}
}

func TestAmendFixedAssetDepreciationMethodFullyDepreciated(t *testing.T) {
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
	for _, method := range []string{DepreciationDoubleDeclining, DepreciationStraightLine} {
		if _, err := l.AmendFixedAssetDepreciationMethod("srv-001", method); !errors.Is(err, ErrFixedAssetFullyDepreciated) {
			t.Errorf("method %q on fully depreciated asset: err = %v, want ErrFixedAssetFullyDepreciated", method, err)
		}
	}
	got, _ := l.GetFixedAsset("srv-001")
	if got.Method != DepreciationStraightLine {
		t.Errorf("method after rejection = %q, want straight-line (untouched)", got.Method)
	}
}

func TestAmendFixedAssetDepreciationMethodStraightLineToDoubleDeclining(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	mustRegister(t, l, testAsset())

	// Month 1 books 10000 at the original straight-line method.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 10000 {
		t.Fatalf("month 1 = %d, want 10000", total)
	}
	orig, _ := l.GetFixedAsset("srv-001")

	amended, err := l.AmendFixedAssetDepreciationMethod("srv-001", DepreciationDoubleDeclining)
	if err != nil {
		t.Fatalf("AmendFixedAssetDepreciationMethod: %v", err)
	}
	if amended.Method != DepreciationDoubleDeclining {
		t.Errorf("amended method = %q, want double-declining", amended.Method)
	}
	// Only the method moves: cost, salvage, life, currency, and the
	// booked month are untouched.
	amended.Method = orig.Method
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended asset = %+v, want %+v except method", amended, orig)
	}

	// The new method governs from the next Depreciate: book value
	// 110000 * 2 / 12 = 18333, and the already-booked March row
	// stays 10000.
	got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil || dup {
		t.Fatalf("Depreciate after amendment = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 18333 {
		t.Errorf("month 2 = %d, want 18333 (bookValue 110000 * 2 / 12)", got.AmountCents)
	}
	if got.Memo != "depreciation:double-declining" {
		t.Fatalf("memo = %q, want depreciation:double-declining", got.Memo)
	}
	// History is not restated: the March row still books 10000.
	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries after amendment = %d, want 2 (amendment posts nothing)", len(entries))
	}
	var march int64
	for _, e := range entries {
		if e.ID == "depr/srv-001/2026-03" {
			march = e.AmountCents
		}
	}
	if march != 10000 {
		t.Errorf("March row = %d, want 10000 (old-method amount kept)", march)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fixed_asset_method_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["asset_id"] != "srv-001" || d["previous_method"] != "straight-line" || d["method"] != "double-declining" {
		t.Errorf("event details = %+v, want srv-001 straight-line->double-declining", d)
	}
}

func TestAmendFixedAssetDepreciationMethodDoubleDecliningToStraightLine(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.Method = DepreciationDoubleDeclining
	mustRegister(t, l, asset)
	// Month 1 at the original double-declining method:
	// 120000*2/12 = 20000.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.March, 1); total != 20000 {
		t.Fatalf("month 1 = %d, want 20000", total)
	}

	if _, err := l.AmendFixedAssetDepreciationMethod("srv-001", DepreciationStraightLine); err != nil {
		t.Fatalf("AmendFixedAssetDepreciationMethod: %v", err)
	}
	// Month 2 reads the live method: straight-line re-evens the
	// spread — (120000 - 0) / 12 = 10000.
	got, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Depreciate month 2: %v", err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("month 2 = %d, want 10000 (base 120000 / 12)", got.AmountCents)
	}
	if got.Memo != "depreciation:straight-line" {
		t.Fatalf("memo = %q, want depreciation:straight-line", got.Memo)
	}
	// History is not restated: the March row still books 20000.
	var march int64
	for _, e := range l.Entries() {
		if e.ID == "depr/srv-001/2026-03" {
			march = e.AmountCents
		}
	}
	if march != 20000 {
		t.Errorf("March row = %d, want 20000 (old-method amount kept)", march)
	}
}

func TestAmendFixedAssetDepreciationMethodConvergesToSalvage(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// One straight-line month (10000), then switch to
	// double-declining and run the asset out: the declining charges
	// plus the remaining-base cap and dust true-up must converge
	// exactly to salvage, never over-depreciating.
	if total := depreciateMonths(t, l, "srv-001", 2026, time.January, 1); total != 10000 {
		t.Fatalf("month 1 = %d, want 10000", total)
	}
	if _, err := l.AmendFixedAssetDepreciationMethod("srv-001", DepreciationDoubleDeclining); err != nil {
		t.Fatalf("AmendFixedAssetDepreciationMethod: %v", err)
	}
	var total int64 = 10000
	months := 0
	for y, m := 2026, time.February; months < 120; m++ {
		if m > time.December {
			y++
			m = time.January
		}
		got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", time.Date(y, m, 15, 12, 0, 0, 0, time.UTC))
		if err != nil || dup {
			t.Fatalf("Depreciate %d-%02d = dup=%v err=%v", y, m, dup, err)
		}
		if got.AmountCents == 0 {
			break
		}
		total += got.AmountCents
		months++
	}
	if total != 120000 {
		t.Fatalf("lifetime total = %d, want 120000 (exact convergence to salvage)", total)
	}
	if acc, err := l.AccumulatedDepreciation("srv-001"); err != nil || acc != 120000 {
		t.Fatalf("AccumulatedDepreciation = %d, %v, want 120000, nil", acc, err)
	}
}
