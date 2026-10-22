package ledger

import (
	"bytes"
	"errors"
	"testing"
)

// pledgeFixture pledges 10 units of "bond-1" @ 10000c with a 25%
// haircut for alice (effective 75000c) and sets a 125% coverage
// requirement in USD. It returns the ledger.
func pledgeFixture(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500); err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	if _, err := l.SetCollateralRequirement("alice", "USD", 12500); err != nil {
		t.Fatalf("SetCollateralRequirement: %v", err)
	}
	return l
}

func TestPledgeCollateralValidation(t *testing.T) {
	l := New()
	for _, tc := range []struct {
		name                                  string
		account                               AccountID
		asset                                 string
		quantity, value, haircut              int64
		want                                  error
	}{
		{"empty account", "", "bond-1", 10, 10000, 0, ErrEmptyCollateralAccount},
		{"empty asset", "alice", "  ", 10, 10000, 0, ErrEmptyCollateralAsset},
		{"zero quantity", "alice", "bond-1", 0, 10000, 0, ErrCollateralNonPositiveQuantity},
		{"negative quantity", "alice", "bond-1", -5, 10000, 0, ErrCollateralNonPositiveQuantity},
		{"zero value", "alice", "bond-1", 10, 0, 0, ErrCollateralNonPositiveValue},
		{"negative haircut", "alice", "bond-1", 10, 10000, -1, ErrInvalidCollateralHaircut},
		{"haircut over 100%", "alice", "bond-1", 10, 10000, 10001, ErrInvalidCollateralHaircut},
	} {
		if _, err := l.PledgeCollateral(tc.account, tc.asset, tc.quantity, tc.value, tc.haircut); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v, want %v", tc.name, err, tc.want)
		}
	}
	p, err := l.PledgeCollateral("alice", "bond-1", 10, 10000, 2500)
	if err != nil {
		t.Fatalf("PledgeCollateral: %v", err)
	}
	if p.Account != "alice" || p.AssetID != "bond-1" || p.Quantity != 10 || p.UnitValueCents != 10000 || p.HaircutBps != 2500 {
		t.Errorf("pledge = %+v, want alice/bond-1/10/10000/2500", p)
	}
	if p.PledgedAt.IsZero() {
		t.Error("PledgedAt not set")
	}
	// A repeat pledge accumulates the quantity and marks to market.
	p2, err := l.PledgeCollateral("alice", "bond-1", 5, 12000, 0)
	if err != nil {
		t.Fatalf("top-up PledgeCollateral: %v", err)
	}
	if p2.Quantity != 15 || p2.UnitValueCents != 12000 || p2.HaircutBps != 0 {
		t.Errorf("top-up pledge = %+v, want quantity 15 marked to 12000c/0 haircut", p2)
	}
	if !p2.PledgedAt.Equal(p.PledgedAt) {
		t.Error("top-up must keep the original PledgedAt")
	}
}

func TestReleaseCollateral(t *testing.T) {
	l := pledgeFixture(t)
	if _, err := l.ReleaseCollateral("alice", "no-such-asset", 1); !errors.Is(err, ErrCollateralNotFound) {
		t.Errorf("unknown asset: err=%v, want ErrCollateralNotFound", err)
	}
	if _, err := l.ReleaseCollateral("alice", "bond-1", 11); !errors.Is(err, ErrCollateralReleaseExceedsPledge) {
		t.Errorf("over-release: err=%v, want ErrCollateralReleaseExceedsPledge", err)
	}
	if _, err := l.ReleaseCollateral("alice", "bond-1", 0); !errors.Is(err, ErrCollateralNonPositiveQuantity) {
		t.Errorf("zero release: err=%v, want ErrCollateralNonPositiveQuantity", err)
	}
	if _, err := l.ReleaseCollateral("alice", "bond-1", 4); err != nil {
		t.Fatalf("ReleaseCollateral: %v", err)
	}
	p, ok := l.GetCollateral("alice", "bond-1")
	if !ok || p.Quantity != 6 {
		t.Errorf("after partial release: %+v, ok=%v, want quantity 6", p, ok)
	}
	// Releasing the whole position deletes the row.
	if _, err := l.ReleaseCollateral("alice", "bond-1", 6); err != nil {
		t.Fatalf("full ReleaseCollateral: %v", err)
	}
	if _, ok := l.GetCollateral("alice", "bond-1"); ok {
		t.Error("fully released position must be deleted")
	}
}

func TestCollateralHaircutMath(t *testing.T) {
	// 10 units @ 10000c, 2500bps haircut -> 75000c effective.
	if got := collateralEffectiveValue(10, 10000, 2500); got != 75000 {
		t.Errorf("collateralEffectiveValue = %d, want 75000", got)
	}
	// Zero haircut: full value.
	if got := collateralEffectiveValue(10, 10000, 0); got != 100000 {
		t.Errorf("zero haircut = %d, want 100000", got)
	}
	// Full haircut: zero.
	if got := collateralEffectiveValue(10, 10000, 10000); got != 0 {
		t.Errorf("full haircut = %d, want 0", got)
	}
	// Coverage: 75000 covers 50000 at 125% (needs 62500), not 100000
	// (needs 125000).
	if !collateralCovers(75000, 50000, 12500) {
		t.Error("75000 should cover 50000 at 125%")
	}
	if collateralCovers(75000, 100000, 12500) {
		t.Error("75000 should not cover 100000 at 125%")
	}
	// Coverage ratio rendering: 75000/50000 = 150%.
	if got := collateralCoverageBps(75000, 50000); got != 15000 {
		t.Errorf("collateralCoverageBps = %d, want 15000", got)
	}
	if got := collateralCoverageBps(75000, 0); got != -1 {
		t.Errorf("zero drawn coverage = %d, want -1", got)
	}
}

func TestCollateralGatePost(t *testing.T) {
	l := pledgeFixture(t)
	// A draw of 50000 needs 62500 coverage: covered.
	if _, _, err := l.Post(JournalEntry{ID: "c-draw-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Fatalf("covered draw: %v", err)
	}
	// A draw of 100000 needs 125000 coverage: rejected, nothing
	// recorded.
	_, versionBefore := l.Snapshot("nobody")
	if _, _, err := l.Post(JournalEntry{ID: "c-draw-2", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 100000, Currency: "USD"}); !errors.Is(err, ErrInsufficientCollateral) {
		t.Fatalf("uncovered draw: err=%v, want ErrInsufficientCollateral", err)
	}
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Errorf("version = %d, want unchanged %d", v, versionBefore)
	}
	// An inflow (no draw) is never gated.
	if _, _, err := l.Post(JournalEntry{ID: "c-in-1", DebitAccount: "alice", CreditAccount: "funding", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("inflow: %v", err)
	}
	// An account without a requirement is never gated.
	if _, _, err := l.Post(JournalEntry{ID: "c-draw-3", DebitAccount: "expense", CreditAccount: "mallory", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("unrequired account: %v", err)
	}
}

func TestCollateralGateTransfer(t *testing.T) {
	l := pledgeFixture(t)
	seedBalance(t, l, "alice", 200000)
	// 250000 outflow leaves -50000 drawn: needs 62500 <= 75000,
	// covered.
	if _, err := l.PostTransfer(Transfer{ID: "c-tx-1", From: "alice", To: "bob", AmountCents: 250000, Currency: "USD"}); err != nil {
		t.Fatalf("covered transfer: %v", err)
	}
	// Another 100000 leaves -150000 drawn: needs 187500 > 75000,
	// rejected.
	if _, err := l.PostTransfer(Transfer{ID: "c-tx-2", From: "alice", To: "bob", AmountCents: 100000, Currency: "USD"}); !errors.Is(err, ErrInsufficientCollateral) {
		t.Fatalf("uncovered transfer: err=%v, want ErrInsufficientCollateral", err)
	}
}

func TestCollateralBatchSimulation(t *testing.T) {
	l := pledgeFixture(t)
	// Each leg alone is covered (50000 needs 62500), but the batch's
	// cumulative draw of 100000 needs 125000: the batch-order
	// simulation must reject the split draw.
	_, err := l.PostBatch(Batch{
		ID: "c-batch-1",
		Entries: []JournalEntry{
			{ID: "c-b-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"},
			{ID: "c-b-2", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"},
		},
	})
	if !errors.Is(err, ErrInsufficientCollateral) {
		t.Fatalf("split draw: err=%v, want ErrInsufficientCollateral", err)
	}
	// A single covered leg still books.
	if _, err := l.PostBatch(Batch{
		ID: "c-batch-2",
		Entries: []JournalEntry{
			{ID: "c-b-3", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("covered batch: %v", err)
	}
}

func TestMarginCallOnRevalue(t *testing.T) {
	l := pledgeFixture(t)
	// Draw 50000 (needs 62500): covered by 75000.
	if _, _, err := l.Post(JournalEntry{ID: "c-mc-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	// Revalue down to 8000c/unit: effective 60000 < 62500 -> margin
	// call.
	calls, err := l.RevalueCollateral("alice", "bond-1", 8000)
	if err != nil {
		t.Fatalf("RevalueCollateral: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("margin calls = %d, want 1", len(calls))
	}
	mc := calls[0]
	if mc.Account != "alice" || mc.Currency != "USD" || mc.EffectiveCents != 60000 || mc.DrawnCents != 50000 || mc.RequiredBps != 12500 {
		t.Errorf("margin call = %+v, want alice/USD/60000/50000/12500", mc)
	}
	if mc.CoverageBps != 12000 {
		t.Errorf("margin call coverage = %d, want 12000", mc.CoverageBps)
	}
	// The query side agrees while the breach stands.
	if q := l.CheckMarginCall("alice", "USD"); q == nil {
		t.Error("CheckMarginCall = nil, want the margin call")
	}
	// Revalue back up: the call clears.
	if calls, err := l.RevalueCollateral("alice", "bond-1", 10000); err != nil || len(calls) != 0 {
		t.Errorf("revalue up: calls=%v, err=%v, want no calls", calls, err)
	}
	if q := l.CheckMarginCall("alice", "USD"); q != nil {
		t.Errorf("CheckMarginCall after recovery = %+v, want nil", q)
	}
	// Revaluing an unknown asset is fail-closed.
	if _, err := l.RevalueCollateral("alice", "no-such-asset", 8000); !errors.Is(err, ErrCollateralNotFound) {
		t.Errorf("unknown asset revalue: err=%v, want ErrCollateralNotFound", err)
	}
}

func TestSetCollateralRequirement(t *testing.T) {
	l := New()
	if _, err := l.SetCollateralRequirement("alice", "USD", -1); !errors.Is(err, ErrInvalidCoverageRatio) {
		t.Errorf("negative ratio: err=%v, want ErrInvalidCoverageRatio", err)
	}
	if _, err := l.SetCollateralRequirement("", "USD", 12500); !errors.Is(err, ErrEmptyCollateralAccount) {
		t.Errorf("empty account: err=%v, want ErrEmptyCollateralAccount", err)
	}
	if _, err := l.SetCollateralRequirement("alice", "USD", 12500); err != nil {
		t.Fatalf("SetCollateralRequirement: %v", err)
	}
	req, ok := l.GetCollateralRequirement("alice", "USD")
	if !ok || req.MinCoverageBps != 12500 {
		t.Errorf("requirement = %+v, ok=%v, want 12500", req, ok)
	}
	// 0 clears the requirement.
	if _, err := l.SetCollateralRequirement("alice", "USD", 0); err != nil {
		t.Fatalf("clear requirement: %v", err)
	}
	if _, ok := l.GetCollateralRequirement("alice", "USD"); ok {
		t.Error("cleared requirement must be gone")
	}
	// Raising the ratio past coverage triggers a margin call.
	l2 := pledgeFixture(t)
	if _, _, err := l2.Post(JournalEntry{ID: "c-req-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	calls, err := l2.SetCollateralRequirement("alice", "USD", 20000)
	if err != nil {
		t.Fatalf("raise requirement: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("raised requirement: calls=%d, want 1", len(calls))
	}
}

func TestCollateralCoverageQuery(t *testing.T) {
	l := pledgeFixture(t)
	effective, drawn, coverage, ok := l.CollateralCoverage("alice", "USD")
	if !ok || effective != 75000 || drawn != 0 || coverage != -1 {
		t.Errorf("coverage = %d/%d/%d ok=%v, want 75000/0/-1 true", effective, drawn, coverage, ok)
	}
	if _, _, _, ok := l.CollateralCoverage("alice", "EUR"); ok {
		t.Error("EUR coverage ok=true, want false (no requirement)")
	}
	if _, _, _, ok := l.CollateralCoverage("mallory", "USD"); ok {
		t.Error("mallory coverage ok=true, want false (no requirement)")
	}
}

func TestCollateralSnapshotRoundTrip(t *testing.T) {
	l := pledgeFixture(t)
	if _, _, err := l.Post(JournalEntry{ID: "c-snap-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	p, ok := restored.GetCollateral("alice", "bond-1")
	if !ok || p.Quantity != 10 || p.UnitValueCents != 10000 || p.HaircutBps != 2500 {
		t.Errorf("restored pledge = %+v, ok=%v, want 10/10000/2500", p, ok)
	}
	req, ok := restored.GetCollateralRequirement("alice", "USD")
	if !ok || req.MinCoverageBps != 12500 {
		t.Errorf("restored requirement = %+v, ok=%v, want 12500", req, ok)
	}
	// The gate survives the round trip: an uncovered draw is still
	// rejected on the restored ledger.
	if _, _, err := restored.Post(JournalEntry{ID: "c-snap-2", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 100000, Currency: "USD"}); !errors.Is(err, ErrInsufficientCollateral) {
		t.Errorf("restored gate: err=%v, want ErrInsufficientCollateral", err)
	}
}

func TestCollateralDryRunSeesGate(t *testing.T) {
	l := pledgeFixture(t)
	// The dry-run twin must simulate against the same coverage the
	// real Post applies: an uncovered draw dry-runs as a rejection.
	if _, err := l.DryRunPost(JournalEntry{ID: "c-dr-1", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 100000, Currency: "USD"}); !errors.Is(err, ErrInsufficientCollateral) {
		t.Errorf("dry-run uncovered: err=%v, want ErrInsufficientCollateral", err)
	}
	if _, err := l.DryRunPost(JournalEntry{ID: "c-dr-2", DebitAccount: "expense", CreditAccount: "alice", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Errorf("dry-run covered: %v", err)
	}
}
