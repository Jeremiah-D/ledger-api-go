package ledger

import (
	"bytes"
	"errors"
	"testing"
)

func seedEstimateLedger(t *testing.T) *Ledger {
	t.Helper()
	return New(
		WithTransferFeeSchedule([]FeeTier{
			{MinAmountCents: 0, RateBps: 0},       // dust: free
			{MinAmountCents: 10000, RateBps: 250},  // $100+: 2.5%
			{MinAmountCents: 1000000, RateBps: 100}, // $10k+: 1%
		}, "fee-revenue"),
	)
}

func TestEstimateTransferFeesSameCurrencyPolicy(t *testing.T) {
	l := seedEstimateLedger(t)
	est, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("EstimateTransferFees: %v", err)
	}
	// 20000 falls in the 2.5% tier: floor(20000*250/10000) = 500.
	if est.FeeCents != 500 {
		t.Errorf("FeeCents = %d, want 500", est.FeeCents)
	}
	if est.FeeTierIndex != 1 || est.FeeRateBps != 250 {
		t.Errorf("tier = (%d, %d bps), want (1, 250)", est.FeeTierIndex, est.FeeRateBps)
	}
	if est.FeeAccount != "fee-revenue" {
		t.Errorf("FeeAccount = %q, want fee-revenue", est.FeeAccount)
	}
	if est.FXSpreadCents != 0 || est.CorrespondentCents != 0 {
		t.Errorf("FX/correspondent must be zero on same-currency, got %+v", est)
	}
	if est.TotalCents != 500 {
		t.Errorf("TotalCents = %d, want 500", est.TotalCents)
	}
	if est.Currency != "USD" {
		t.Errorf("Currency = %q, want USD (default)", est.Currency)
	}
}

func TestEstimateTransferFeesExplicitFeeAndSkip(t *testing.T) {
	l := seedEstimateLedger(t)
	est, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000,
		FeeCents: 100, FeeAccount: "explicit-fees",
	})
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if est.FeeCents != 100 || est.FeeAccount != "explicit-fees" {
		t.Errorf("explicit fee = (%d -> %q), want (100 -> explicit-fees)", est.FeeCents, est.FeeAccount)
	}
	if est.FeeTierIndex != -1 {
		t.Errorf("explicit fee tier index = %d, want -1", est.FeeTierIndex)
	}
	est2, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000, SkipFee: true,
	})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if est2.FeeCents != 0 || est2.TotalCents != 0 {
		t.Errorf("skipped fee = (%d, total %d), want (0, 0)", est2.FeeCents, est2.TotalCents)
	}
	// Dust tier: zero policy fee but the tier is still disclosed.
	est3, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 100,
	})
	if err != nil {
		t.Fatalf("dust: %v", err)
	}
	if est3.FeeCents != 0 || est3.FeeTierIndex != 0 {
		t.Errorf("dust = (fee %d, tier %d), want (0, 0)", est3.FeeCents, est3.FeeTierIndex)
	}
}

func TestEstimateTransferFeesCrossCurrencyMidOnly(t *testing.T) {
	l := seedEstimateLedger(t)
	if err := l.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	if err := l.SetCorrespondentFee("USD", "EUR", 1500); err != nil {
		t.Fatalf("SetCorrespondentFee: %v", err)
	}
	est, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000,
		ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("EstimateTransferFees: %v", err)
	}
	// floor(20000 * 108/100) = 21600 at the mid (sell default deals at bid
	// = mid on a mid-only rate).
	if est.FXConvertedCents != 21600 {
		t.Errorf("FXConvertedCents = %d, want 21600", est.FXConvertedCents)
	}
	if est.FXSpreadCents != 0 {
		t.Errorf("FXSpreadCents = %d, want 0 (mid-only)", est.FXSpreadCents)
	}
	if est.FXSide != FXSideSell {
		t.Errorf("FXSide = %q, want sell (default)", est.FXSide)
	}
	if est.CorrespondentCents != 1500 {
		t.Errorf("CorrespondentCents = %d, want 1500", est.CorrespondentCents)
	}
	// Total: fee 500 (2.5% tier) + correspondent 1500 + zero spread.
	if est.TotalCents != 2000 {
		t.Errorf("TotalCents = %d, want 2000", est.TotalCents)
	}
	if est.ToCurrency != "EUR" {
		t.Errorf("ToCurrency = %q, want EUR", est.ToCurrency)
	}
}

func TestEstimateTransferFeesCrossCurrencyWithSpread(t *testing.T) {
	l := seedEstimateLedger(t)
	// Mid 108/100 with a dealing spread: bid 107.5/100, ask 108.5/100.
	if err := l.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	if err := l.SetFXBidAskRate("USD", "EUR", 1075, 1000, 1085, 1000); err != nil {
		t.Fatalf("SetFXBidAskRate: %v", err)
	}
	est, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000,
		ToCurrency: "EUR", FXSide: "sell",
	})
	if err != nil {
		t.Fatalf("EstimateTransferFees: %v", err)
	}
	// Sell side deals at bid: floor(20000 * 1075/1000) = 21500.
	if est.FXConvertedCents != 21500 {
		t.Errorf("FXConvertedCents = %d, want 21500", est.FXConvertedCents)
	}
	// Spread = mid − bid = 21600 − 21500 = 100 EUR cents.
	if est.FXSpreadCents != 100 {
		t.Errorf("FXSpreadCents = %d, want 100", est.FXSpreadCents)
	}
	if est.FXSpreadBps != 92 {
		t.Errorf("FXSpreadBps = %d, want 92 (full bid-ask spread (1085-1075)/1080)", est.FXSpreadBps)
	}
	// Total: fee 500 + spread converted back at mid floor(100*100/108)=92.
	if want := int64(500 + 92); est.TotalCents != want {
		t.Errorf("TotalCents = %d, want %d", est.TotalCents, want)
	}
	// Buy side deals at ask: floor(20000*1085/1000) = 21700, spread 100.
	est2, err := l.EstimateTransferFees(FeeEstimateRequest{
		From: "alice", To: "bob", AmountCents: 20000,
		ToCurrency: "EUR", FXSide: "buy",
	})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if est2.FXConvertedCents != 21700 || est2.FXSpreadCents != 100 || est2.FXSide != FXSideBuy {
		t.Errorf("buy = (converted %d, spread %d, side %q), want (21700, 100, buy)",
			est2.FXConvertedCents, est2.FXSpreadCents, est2.FXSide)
	}
}

func TestEstimateTransferFeesRateErrors(t *testing.T) {
	l := seedEstimateLedger(t)
	req := FeeEstimateRequest{From: "alice", To: "bob", AmountCents: 1000, ToCurrency: "EUR"}
	if _, err := l.EstimateTransferFees(req); !errors.Is(err, ErrFXRateMissing) {
		t.Errorf("missing rate err = %v, want ErrFXRateMissing", err)
	}
	// An estimate is read-only: no version bump, no journal, no keys.
	if v := l.Version(); v != 0 {
		t.Errorf("Version after estimate = %d, want 0", v)
	}
}

func TestEstimateTransferFeesValidation(t *testing.T) {
	l := seedEstimateLedger(t)
	cases := []struct {
		name string
		req  FeeEstimateRequest
		want error
	}{
		{"empty from", FeeEstimateRequest{To: "bob", AmountCents: 100}, ErrEmptyFromAccount},
		{"empty to", FeeEstimateRequest{From: "alice", AmountCents: 100}, ErrEmptyToAccount},
		{"same account", FeeEstimateRequest{From: "alice", To: "alice", AmountCents: 100}, ErrTransferSameAccount},
		{"zero amount", FeeEstimateRequest{From: "alice", To: "bob"}, ErrTransferNonPositiveAmount},
		{"bad currency", FeeEstimateRequest{From: "alice", To: "bob", AmountCents: 100, Currency: "US"}, ErrInvalidCurrency},
		{"self FX pair", FeeEstimateRequest{From: "alice", To: "bob", AmountCents: 100, ToCurrency: "USD"}, ErrInvalidFXRate},
		{"bad fx side", FeeEstimateRequest{From: "alice", To: "bob", AmountCents: 100, FXSide: "sideways"}, ErrInvalidFXSide},
		{"negative fee", FeeEstimateRequest{From: "alice", To: "bob", AmountCents: 100, FeeCents: -5}, ErrInvalidFee},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.EstimateTransferFees(tc.req); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSetCorrespondentFeeValidation(t *testing.T) {
	l := New()
	for _, tc := range []struct {
		name     string
		from, to string
		cents    int64
	}{
		{"negative fee", "USD", "EUR", -1},
		{"self pair", "USD", "USD", 10},
		{"bad code", "US", "EUR", 10},
		{"empty", "", "EUR", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := l.SetCorrespondentFee(tc.from, tc.to, tc.cents); !errors.Is(err, ErrInvalidCorrespondentFee) {
				t.Errorf("err = %v, want ErrInvalidCorrespondentFee", err)
			}
		})
	}
	if err := l.SetCorrespondentFee("USD", "EUR", 1500); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := l.CorrespondentFees(); len(got) != 1 || got[0].Cents != 1500 {
		t.Errorf("CorrespondentFees = %+v, want one 1500 row", got)
	}
	// Zero removes the corridor.
	if err := l.SetCorrespondentFee("USD", "EUR", 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := l.CorrespondentFees(); len(got) != 0 {
		t.Errorf("after clear, CorrespondentFees = %+v, want empty", got)
	}
	// The returned slice is a copy: mutating it does not touch the ledger.
	if err := l.SetCorrespondentFee("USD", "EUR", 1500); err != nil {
		t.Fatalf("set: %v", err)
	}
	got := l.CorrespondentFees()
	got[0].Cents = 1
	if again := l.CorrespondentFees(); again[0].Cents != 1500 {
		t.Errorf("copy aliasing: ledger fee = %d, want 1500", again[0].Cents)
	}
}

func TestCorrespondentFeesSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetCorrespondentFee("USD", "EUR", 1500); err != nil {
		t.Fatalf("set USD/EUR: %v", err)
	}
	if err := l.SetCorrespondentFee("USD", "CNY", 2000); err != nil {
		t.Fatalf("set USD/CNY: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("correspondent_fees")) {
		t.Fatalf("snapshot has no correspondent_fees record")
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got := l2.CorrespondentFees()
	if len(got) != 2 || got[0] != (CorrespondentFee{FromCurrency: "USD", ToCurrency: "CNY", Cents: 2000}) ||
		got[1] != (CorrespondentFee{FromCurrency: "USD", ToCurrency: "EUR", Cents: 1500}) {
		t.Errorf("restored = %+v, want [(USD,CNY,2000) (USD,EUR,1500)] sorted", got)
	}
	// The estimate on the restored ledger prices the corridor fee.
	if err := l2.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatalf("SetFXRate on restored: %v", err)
	}
	est, err := l2.EstimateTransferFees(FeeEstimateRequest{
		From: "a", To: "b", AmountCents: 100, ToCurrency: "EUR", SkipFee: true,
	})
	if err != nil {
		t.Fatalf("estimate on restored: %v", err)
	}
	if est.CorrespondentCents != 1500 || est.TotalCents != 1500 {
		t.Errorf("restored estimate = (corr %d, total %d), want (1500, 1500)",
			est.CorrespondentCents, est.TotalCents)
	}
}

// An estimate must never mutate the ledger: balances, version, journal
// and idempotency state are bit-identical before and after.
func TestEstimateTransferFeesReadOnly(t *testing.T) {
	l := seedEstimateLedger(t)
	seedBalance(t, l, "alice", 100000)
	if err := l.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	beforeVersion := l.Version()
	beforeEntries := len(l.Entries())
	beforeBalance := l.Balance("alice")
	beforeFeeRows := len(l.CorrespondentFees())
	for i := 0; i < 25; i++ {
		if _, err := l.EstimateTransferFees(FeeEstimateRequest{
			From: "alice", To: "bob", AmountCents: 20000, ToCurrency: "EUR",
		}); err != nil {
			t.Fatalf("estimate %d: %v", i, err)
		}
	}
	if got := l.Version(); got != beforeVersion {
		t.Errorf("Version moved %d -> %d under read-only estimates", beforeVersion, got)
	}
	if got := len(l.Entries()); got != beforeEntries {
		t.Errorf("journal grew %d -> %d under read-only estimates", beforeEntries, got)
	}
	if got := l.Balance("alice"); got != beforeBalance {
		t.Errorf("alice balance moved %d -> %d under read-only estimates", beforeBalance, got)
	}
	if got := len(l.CorrespondentFees()); got != beforeFeeRows {
		t.Errorf("correspondent table changed %d -> %d rows", beforeFeeRows, got)
	}
}
