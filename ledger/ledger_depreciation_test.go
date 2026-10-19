package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var deprMonth = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func testAsset() FixedAsset {
	return FixedAsset{
		ID:               "srv-001",
		Account:          "fixed-assets:servers",
		CostCents:        120000,
		Currency:         "USD",
		SalvageCents:     0,
		UsefulLifeMonths: 12,
		Method:           DepreciationStraightLine,
	}
}

func mustRegister(t *testing.T, l *Ledger, asset FixedAsset) {
	t.Helper()
	if err := l.RegisterFixedAsset(asset); err != nil {
		t.Fatalf("RegisterFixedAsset: %v", err)
	}
}

func TestRegisterFixedAssetValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*FixedAsset)
		err    error
	}{
		{"empty ID", func(a *FixedAsset) { a.ID = "" }, ErrInvalidAssetID},
		{"ID with slash", func(a *FixedAsset) { a.ID = "a/b" }, ErrInvalidAssetID},
		{"empty account", func(a *FixedAsset) { a.Account = "" }, ErrInvalidAssetAccount},
		{"zero cost", func(a *FixedAsset) { a.CostCents = 0 }, ErrInvalidAssetCost},
		{"negative cost", func(a *FixedAsset) { a.CostCents = -1 }, ErrInvalidAssetCost},
		{"negative salvage", func(a *FixedAsset) { a.SalvageCents = -1 }, ErrInvalidSalvage},
		{"salvage equals cost", func(a *FixedAsset) { a.SalvageCents = a.CostCents }, ErrInvalidSalvage},
		{"salvage above cost", func(a *FixedAsset) { a.SalvageCents = a.CostCents + 1 }, ErrInvalidSalvage},
		{"zero life", func(a *FixedAsset) { a.UsefulLifeMonths = 0 }, ErrInvalidUsefulLife},
		{"negative life", func(a *FixedAsset) { a.UsefulLifeMonths = -3 }, ErrInvalidUsefulLife},
		{"bad method", func(a *FixedAsset) { a.Method = "sum-of-years" }, ErrInvalidDepreciationMethod},
		{"bad currency", func(a *FixedAsset) { a.Currency = "usd" }, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			asset := testAsset()
			tc.mutate(&asset)
			if err := l.RegisterFixedAsset(asset); !errors.Is(err, tc.err) {
				t.Fatalf("RegisterFixedAsset = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestRegisterFixedAssetDuplicate(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	if err := l.RegisterFixedAsset(testAsset()); !errors.Is(err, ErrFixedAssetExists) {
		t.Fatalf("duplicate register = %v, want ErrFixedAssetExists", err)
	}
}

func TestGetFixedAsset(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	got, err := l.GetFixedAsset("srv-001")
	if err != nil {
		t.Fatalf("GetFixedAsset: %v", err)
	}
	if got.CostCents != 120000 || got.Method != DepreciationStraightLine {
		t.Fatalf("GetFixedAsset = %+v, want test asset", got)
	}
	if _, err := l.GetFixedAsset("nope"); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Fatalf("GetFixedAsset(missing) = %v, want ErrFixedAssetNotFound", err)
	}
}

func TestDepreciateStraightLine(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// 120000 / 12 = 10000/month exactly.
	got, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth)
	if err != nil || dup {
		t.Fatalf("Depreciate = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 10000 {
		t.Fatalf("depreciation = %d, want 10000", got.AmountCents)
	}
	if got.DebitAccount != "depr-expense" || got.CreditAccount != "accum-depr" {
		t.Fatalf("legs = %s/%s, want depr-expense/accum-depr", got.DebitAccount, got.CreditAccount)
	}
	if got.ID != "depr/srv-001/2026-03" {
		t.Fatalf("ID = %q, want depr/srv-001/2026-03", got.ID)
	}
	if got.Memo != "depreciation:straight-line" {
		t.Fatalf("memo = %q, want depreciation:straight-line", got.Memo)
	}
	if got.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", got.Currency)
	}
	// Balances moved like an ordinary posting: expense debited,
	// accumulated depreciation credited (contra-asset, negative balance).
	if b := l.Balance("depr-expense"); b != 10000 {
		t.Fatalf("expense balance = %d, want 10000", b)
	}
	if b := l.Balance("accum-depr"); b != -10000 {
		t.Fatalf("accumulated balance = %d, want -10000", b)
	}
}

func TestDepreciateIdempotentPerMonth(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	first, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth)
	if err != nil || dup {
		t.Fatalf("first = dup=%v err=%v", dup, err)
	}
	ver := l.Version()
	second, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %q, want original %q", second.ID, first.ID)
	}
	if l.Version() != ver {
		t.Fatalf("replay bumped version %d -> %d", ver, l.Version())
	}
	// A different calendar month books a fresh entry.
	next := deprMonth.AddDate(0, 1, 0)
	third, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", next)
	if err != nil || dup {
		t.Fatalf("next month = dup=%v err=%v", dup, err)
	}
	if third.ID == first.ID {
		t.Fatal("different month must produce a different entry ID")
	}
}

func TestDepreciateStraightLineTrueUp(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.ID = "srv-002"
	asset.CostCents = 100000 // 100000/12 = 8333 remainder 4
	mustRegister(t, l, asset)
	var total int64
	month := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		e, dup, err := l.Depreciate("srv-002", "depr-expense", "accum-depr", month)
		if err != nil || dup {
			t.Fatalf("month %d: dup=%v err=%v", i, dup, err)
		}
		total += e.AmountCents
		month = month.AddDate(0, 1, 0)
	}
	// 12 x floor(100000/12) = 99996; the 4-cent remainder trues up next.
	if total != 99996 {
		t.Fatalf("12-month straight-line total = %d, want 99996", total)
	}
	// Month 13 trues up the 4-cent remainder.
	e, dup, err := l.Depreciate("srv-002", "depr-expense", "accum-depr", month)
	if err != nil || dup {
		t.Fatalf("month 13: dup=%v err=%v", dup, err)
	}
	if e.AmountCents != 4 {
		t.Fatalf("true-up month = %d, want 4", e.AmountCents)
	}
	// Fully depreciated: books nothing.
	empty, dup, err := l.Depreciate("srv-002", "depr-expense", "accum-depr", month.AddDate(0, 1, 0))
	if err != nil || dup || empty.ID != "" {
		t.Fatalf("fully depreciated = id=%q dup=%v err=%v, want empty entry", empty.ID, dup, err)
	}
}

func TestDepreciateDoubleDeclining(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.ID = "srv-ddb"
	asset.Method = DepreciationDoubleDeclining
	mustRegister(t, l, asset)
	// Month 1: floor(120000*2/12) = 20000.
	first, _, err := l.Depreciate("srv-ddb", "depr-expense", "accum-depr", deprMonth)
	if err != nil {
		t.Fatalf("month 1: %v", err)
	}
	if first.AmountCents != 20000 {
		t.Fatalf("DDB month 1 = %d, want 20000", first.AmountCents)
	}
	if first.Memo != "depreciation:double-declining" {
		t.Fatalf("memo = %q, want depreciation:double-declining", first.Memo)
	}
	// Month 2: book value 100000 -> floor(100000*2/12) = 16666.
	second, _, err := l.Depreciate("srv-ddb", "depr-expense", "accum-depr", deprMonth.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("month 2: %v", err)
	}
	if second.AmountCents != 16666 {
		t.Fatalf("DDB month 2 = %d, want 16666", second.AmountCents)
	}
	// Declining: month 3 < month 2.
	third, _, err := l.Depreciate("srv-ddb", "depr-expense", "accum-depr", deprMonth.AddDate(0, 2, 0))
	if err != nil {
		t.Fatalf("month 3: %v", err)
	}
	if third.AmountCents >= second.AmountCents {
		t.Fatalf("DDB must decline: month3=%d month2=%d", third.AmountCents, second.AmountCents)
	}
}

func TestDepreciateSalvageCap(t *testing.T) {
	l := New()
	asset := testAsset()
	asset.ID = "srv-salv"
	asset.SalvageCents = 20000 // depreciable base = 100000
	mustRegister(t, l, asset)
	var total int64
	month := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	// Run well past the useful life: total must cap at the base.
	for i := 0; i < 24; i++ {
		e, _, err := l.Depreciate("srv-salv", "depr-expense", "accum-depr", month)
		if err != nil {
			t.Fatalf("month %d: %v", i, err)
		}
		total += e.AmountCents
		month = month.AddDate(0, 1, 0)
	}
	if total != 100000 {
		t.Fatalf("capped total = %d, want 100000 (cost - salvage)", total)
	}
	accum, err := l.AccumulatedDepreciation("srv-salv")
	if err != nil {
		t.Fatalf("AccumulatedDepreciation: %v", err)
	}
	if accum != 100000 {
		t.Fatalf("accumulated = %d, want 100000", accum)
	}
}

func TestDepreciateSkipsJournalRules(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	rule := JournalRule{
		ID:                "tax-withholding",
		Enabled:           true,
		MatchDebitAccount: "depr-expense",
		Legs: []RuleLeg{
			{DebitAccount: "depr-expense", CreditAccount: "tax-payable", FixedCents: 10, Label: "withholding"},
		},
	}
	if err := l.SetJournalRule(rule); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	before := l.RuleLegsCommitted()
	if _, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth); err != nil {
		t.Fatalf("Depreciate: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != before {
		t.Fatalf("rule legs committed = %d, want %d (depreciation must not fire rules)", got, before)
	}
	for _, e := range l.Entries() {
		if strings.Contains(e.ID, "/rule/") {
			t.Fatalf("depreciation produced derived rule leg %q", e.ID)
		}
	}
}

func TestAccumulatedDepreciationNetsReversals(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	first, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth)
	if err != nil {
		t.Fatalf("Depreciate: %v", err)
	}
	if acc, _ := l.AccumulatedDepreciation("srv-001"); acc != 10000 {
		t.Fatalf("accumulated = %d, want 10000", acc)
	}
	// Reverse the month: the reversal nets against it.
	if _, _, err := l.ReverseEntry(first.ID, "rev-key-1"); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if acc, _ := l.AccumulatedDepreciation("srv-001"); acc != 0 {
		t.Fatalf("accumulated after reversal = %d, want 0", acc)
	}
	// The month can be re-run after reversal.
	rerun, dup, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth)
	if err != nil {
		t.Fatalf("re-run after reversal: %v", err)
	}
	// Idempotency replays the ORIGINAL month entry (same key), not a new
	// one — the reversal is a separate entry, the month key still maps
	// to the original.
	if !dup || rerun.ID != first.ID {
		t.Fatalf("re-run = id=%q dup=%v, want original %q with dup=true", rerun.ID, dup, first.ID)
	}
}

func TestDepreciateErrors(t *testing.T) {
	l := New()
	if _, _, err := l.Depreciate("ghost", "e", "a", deprMonth); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Fatalf("Depreciate(missing) = %v, want ErrFixedAssetNotFound", err)
	}
	if _, err := l.AccumulatedDepreciation("ghost"); !errors.Is(err, ErrFixedAssetNotFound) {
		t.Fatalf("AccumulatedDepreciation(missing) = %v, want ErrFixedAssetNotFound", err)
	}
	mustRegister(t, l, testAsset())
	for _, legs := range [][2]AccountID{{"", "a"}, {"e", ""}, {"e/x", "a"}, {"e", "a/y"}} {
		if _, _, err := l.Depreciate("srv-001", legs[0], legs[1], deprMonth); !errors.Is(err, ErrInvalidDepreciationAccount) {
			t.Fatalf("Depreciate(legs=%v) = %v, want ErrInvalidDepreciationAccount", legs, err)
		}
	}
}

func TestDepreciateRunsFullGateOrder(t *testing.T) {
	l := New()
	mustRegister(t, l, testAsset())
	// Frozen expense account rejects the depreciation like any posting.
	l.Freeze("depr-expense")
	if _, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth); err == nil {
		t.Fatal("Depreciate through frozen account must fail")
	}
	l.Unfreeze("depr-expense")
	if _, _, err := l.Depreciate("srv-001", "depr-expense", "accum-depr", deprMonth); err != nil {
		t.Fatalf("Depreciate after unfreeze: %v", err)
	}
}
