package ledger

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// tieredLedger builds a ledger with the canonical three-band schedule used
// across these tests: fee-free dust below $100, 2.5% for $100..$10k, and
// 1% above $10k (the cheap top band acts as an effective cap).
func tieredLedger() *Ledger {
	return New(WithTransferFeeSchedule([]FeeTier{
		{MinAmountCents: 0, RateBps: 0},
		{MinAmountCents: 10000, RateBps: 250},
		{MinAmountCents: 1000000, RateBps: 100},
	}, "revenue"))
}

func TestTransferFeeScheduleTierSelection(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 200_000_000)

	cases := []struct {
		amount      int64
		wantFee     int64
		wantTier    int
		wantRate    int64
		wantEntries int
	}{
		{9999, 0, 0, 0, 1},          // dust: fee-free tier, no leg
		{10000, 250, 1, 250, 2},     // tier boundary: inclusive lower bound
		{101, 0, 0, 0, 1},           // dust under the 0 bps tier posts no leg
		{999999, 24999, 1, 250, 2},  // floor(999999*250/10000) = 24999
		{1000000, 10000, 2, 100, 2}, // top tier boundary
		{5000000, 50000, 2, 100, 2}, // top tier: floor(5e6*100/10000)
	}
	for _, tc := range cases {
		txID := fmt.Sprintf("tx-tier-sel-%d", tc.amount)
		receipt, err := l.PostTransfer(Transfer{
			ID: txID, From: "alice", To: "bob",
			AmountCents: tc.amount, IdempotencyKey: fmt.Sprintf("k-tier-sel-%d", tc.amount),
		})
		if err != nil {
			t.Fatalf("amount %d: PostTransfer: %v", tc.amount, err)
		}
		if receipt.FeeCents != tc.wantFee {
			t.Errorf("amount %d: fee = %d, want %d", tc.amount, receipt.FeeCents, tc.wantFee)
		}
		if receipt.FeeTierIndex != tc.wantTier {
			t.Errorf("amount %d: tier index = %d, want %d", tc.amount, receipt.FeeTierIndex, tc.wantTier)
		}
		if receipt.FeeRateBps != tc.wantRate {
			t.Errorf("amount %d: rate = %d bps, want %d", tc.amount, receipt.FeeRateBps, tc.wantRate)
		}
		if len(receipt.Entries) != tc.wantEntries {
			t.Errorf("amount %d: entries = %d, want %d", tc.amount, len(receipt.Entries), tc.wantEntries)
		}
		if tc.wantFee > 0 {
			fee := receipt.Entries[1]
			if fee.ID != txID+"/fee" || fee.DebitAccount != "revenue" || fee.CreditAccount != "alice" {
				t.Errorf("amount %d: fee entry = %+v, want id %s/fee debit revenue credit alice", tc.amount, fee, txID)
			}
		}
	}
}

func TestTransferFeeScheduleFloorRoundingPerTier(t *testing.T) {
	// Each tier floors independently: floor(amount*rate/10000).
	l := New(WithTransferFeeSchedule([]FeeTier{
		{MinAmountCents: 0, RateBps: 250},
		{MinAmountCents: 10000, RateBps: 333},
	}, "revenue"))
	seedBalance(t, l, "alice", 10_000_000)

	// Tier 0: 101 * 250 / 10000 = 2.525 -> 2.
	r, err := l.PostTransfer(Transfer{ID: "t-f1", From: "alice", To: "bob", AmountCents: 101})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 2 || r.FeeTierIndex != 0 || r.FeeRateBps != 250 {
		t.Errorf("tier-0 receipt = %+v, want fee 2 tier 0 rate 250", r)
	}

	// Tier 1: 10001 * 333 / 10000 = 333.0333 -> 333.
	r, err = l.PostTransfer(Transfer{ID: "t-f2", From: "alice", To: "bob", AmountCents: 10001})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 333 || r.FeeTierIndex != 1 || r.FeeRateBps != 333 {
		t.Errorf("tier-1 receipt = %+v, want fee 333 tier 1 rate 333", r)
	}
}

func TestTransferFeeScheduleOverflowSafe(t *testing.T) {
	// The per-tier fee is floor(amount*rate/10000) computed with the same
	// overflow-safe decomposition as the flat policy: no intermediate
	// product may overflow int64 for any amount and rate <= 10000.
	l := New(WithTransferFeeSchedule([]FeeTier{
		{MinAmountCents: 0, RateBps: 9999},
		{MinAmountCents: math.MaxInt64 / 2, RateBps: 0},
	}, "revenue"))
	seedBalance(t, l, "alice", 100)

	for _, amount := range []int64{math.MaxInt64 / 10000, math.MaxInt64 / 2, math.MaxInt64} {
		idx, rate := l.feeTierFor(amount)
		// Reference via big-free decomposition: amount = 10000*q + r.
		q, rr := amount/10000, amount%10000
		want := q*rate + rr*rate/10000
		if got := policyFeeCents(amount, rate); got != want {
			t.Errorf("tier %d: policyFeeCents(%d, %d) = %d, want %d", idx, amount, rate, got, want)
		}
	}

	// MaxInt64 in the 0 bps top tier: fee 0, no leg, tier still disclosed.
	r, err := l.PostTransfer(Transfer{ID: "t-big", From: "alice", To: "bob", AmountCents: math.MaxInt64})
	if err != nil {
		t.Fatalf("PostTransfer(MaxInt64): %v", err)
	}
	if r.FeeCents != 0 || len(r.Entries) != 1 {
		t.Errorf("receipt = %+v, want no fee leg", r)
	}
	if r.FeeTierIndex != 1 || r.FeeRateBps != 0 {
		t.Errorf("tier disclosure = (%d, %d bps), want (1, 0)", r.FeeTierIndex, r.FeeRateBps)
	}

	// A non-zero fee on MaxInt64 still trips the amount+fee overflow guard.
	l2 := New(WithTransferFeePolicy(10000, "revenue")) // 100%
	_, err = l2.PostTransfer(Transfer{ID: "t-big2", From: "alice", To: "bob", AmountCents: math.MaxInt64})
	if err == nil {
		t.Fatal("PostTransfer(MaxInt64, 100% fee): want amount+fee overflow rejection, got nil")
	}
}

func TestTransferFeeScheduleEffectiveCap(t *testing.T) {
	// A cheaper top band caps the fee curve: at $100k the 1% band charges
	// far less than the 2.5% middle band would.
	l := tieredLedger()
	seedBalance(t, l, "alice", 50_000_000)

	r, err := l.PostTransfer(Transfer{ID: "t-cap", From: "alice", To: "bob", AmountCents: 10_000_000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// floor(10_000_000 * 100 / 10000) = 100_000, not the 250_000 the
	// middle band would charge.
	if r.FeeCents != 100_000 || r.FeeTierIndex != 2 || r.FeeRateBps != 100 {
		t.Errorf("receipt = %+v, want fee 100000 tier 2 rate 100", r)
	}
	if got := l.Balance("revenue"); got != 100_000 {
		t.Errorf("revenue balance = %d, want 100000", got)
	}
}

func TestWithTransferFeeScheduleInvalidPanics(t *testing.T) {
	cases := []struct {
		name  string
		tiers []FeeTier
	}{
		{"negative minimum", []FeeTier{{MinAmountCents: -1, RateBps: 100}, {MinAmountCents: 100, RateBps: 100}}},
		{"unsorted minimums", []FeeTier{{MinAmountCents: 0, RateBps: 100}, {MinAmountCents: 50, RateBps: 100}, {MinAmountCents: 40, RateBps: 100}}},
		{"duplicate minimums", []FeeTier{{MinAmountCents: 0, RateBps: 100}, {MinAmountCents: 0, RateBps: 200}}},
		{"first tier not zero", []FeeTier{{MinAmountCents: 500, RateBps: 100}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("WithTransferFeeSchedule(%v): want panic, got none", tc.tiers)
				}
			}()
			_ = New(WithTransferFeeSchedule(tc.tiers, "revenue"))
		})
	}
}

func TestWithTransferFeeScheduleRateNormalization(t *testing.T) {
	// The Go option follows the flat-policy convention: rates normalize
	// into [0, 10000] instead of failing.
	l := New(WithTransferFeeSchedule([]FeeTier{
		{MinAmountCents: 0, RateBps: -50},
		{MinAmountCents: 100, RateBps: 20000},
	}, "revenue"))
	seedBalance(t, l, "alice", 100_000)

	r, err := l.PostTransfer(Transfer{ID: "t-n1", From: "alice", To: "bob", AmountCents: 50})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 0 || r.FeeRateBps != 0 {
		t.Errorf("negative rate tier: fee = %d rate = %d, want 0/0", r.FeeCents, r.FeeRateBps)
	}

	r, err = l.PostTransfer(Transfer{ID: "t-n2", From: "alice", To: "bob", AmountCents: 100})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 100 || r.FeeRateBps != 10000 {
		t.Errorf("clamped rate tier: fee = %d rate = %d, want 100/10000", r.FeeCents, r.FeeRateBps)
	}
}

func TestWithTransferFeeScheduleDisabled(t *testing.T) {
	// Empty tiers or an empty revenue account disables the policy, like
	// the flat policy before it.
	for _, opt := range []Option{
		WithTransferFeeSchedule(nil, "revenue"),
		WithTransferFeeSchedule([]FeeTier{{MinAmountCents: 0, RateBps: 250}}, ""),
	} {
		l := New(opt)
		seedBalance(t, l, "alice", 10_000)
		r, err := l.PostTransfer(Transfer{ID: "t-d", From: "alice", To: "bob", AmountCents: 5000})
		if err != nil {
			t.Fatalf("PostTransfer: %v", err)
		}
		if r.FeeCents != 0 || len(r.Entries) != 1 || r.FeeTierIndex != -1 || r.FeeRateBps != 0 {
			t.Errorf("disabled receipt = %+v, want no fee and tier -1", r)
		}
	}
}

func TestTransferReceiptTierDisclosure(t *testing.T) {
	// The flat policy discloses as the single tier 0.
	l := New(WithTransferFeePolicy(250, "revenue"))
	seedBalance(t, l, "alice", 100_000)

	r, err := l.PostTransfer(Transfer{ID: "t-flat", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 250 || r.FeeTierIndex != 0 || r.FeeRateBps != 250 {
		t.Errorf("flat policy receipt = %+v, want fee 250 tier 0 rate 250", r)
	}

	// An explicit fee suppresses the policy and discloses no tier.
	r, err = l.PostTransfer(Transfer{
		ID: "t-exp", From: "alice", To: "bob",
		AmountCents: 10000, FeeCents: 99, FeeAccount: "custom-fees",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 99 || r.FeeTierIndex != -1 || r.FeeRateBps != 0 {
		t.Errorf("explicit fee receipt = %+v, want fee 99 tier -1 rate 0", r)
	}

	// SkipFee discloses no tier either.
	r, err = l.PostTransfer(Transfer{ID: "t-skip", From: "alice", To: "bob", AmountCents: 10000, SkipFee: true})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeTierIndex != -1 || r.FeeRateBps != 0 {
		t.Errorf("skip-fee receipt = %+v, want tier -1 rate 0", r)
	}

	// No policy at all: no tier.
	l2 := New()
	seedBalance(t, l2, "alice", 100_000)
	r, err = l2.PostTransfer(Transfer{ID: "t-np", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeTierIndex != -1 || r.FeeRateBps != 0 {
		t.Errorf("no-policy receipt = %+v, want tier -1 rate 0", r)
	}
}

func TestTransferFeeScheduleReplayKeepsTierDisclosure(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 10_000_000)

	first, err := l.PostTransfer(Transfer{
		ID: "tx-tr", From: "alice", To: "bob",
		AmountCents: 50000, IdempotencyKey: "tier-key-1",
	})
	if err != nil {
		t.Fatalf("first PostTransfer: %v", err)
	}
	// 50000 falls in tier 1: floor(50000*250/10000) = 1250.
	if first.FeeCents != 1250 || first.FeeTierIndex != 1 || first.FeeRateBps != 250 {
		t.Fatalf("first receipt = %+v, want fee 1250 tier 1 rate 250", first)
	}

	second, err := l.PostTransfer(Transfer{
		ID: "tx-tr-retry", From: "alice", To: "bob",
		AmountCents: 50000, IdempotencyKey: "tier-key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
	if second.FeeCents != first.FeeCents || second.FeeTierIndex != first.FeeTierIndex || second.FeeRateBps != first.FeeRateBps {
		t.Errorf("replay disclosure = (%d, tier %d, %d bps), want (%d, tier %d, %d bps)",
			second.FeeCents, second.FeeTierIndex, second.FeeRateBps,
			first.FeeCents, first.FeeTierIndex, first.FeeRateBps)
	}
	if got := l.Balance("revenue"); got != 1250 {
		t.Errorf("revenue balance = %d, want 1250 (applied once)", got)
	}
}

func TestTransferExplicitFeeReplayDisclosesNoTier(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 100_000)

	first, err := l.PostTransfer(Transfer{
		ID: "tx-er", From: "alice", To: "bob",
		AmountCents: 50000, FeeCents: 77, FeeAccount: "custom-fees",
		IdempotencyKey: "exp-key-1",
	})
	if err != nil {
		t.Fatalf("first PostTransfer: %v", err)
	}
	if first.FeeTierIndex != -1 || first.FeeRateBps != 0 {
		t.Fatalf("explicit fee receipt = %+v, want tier -1 rate 0", first)
	}

	second, err := l.PostTransfer(Transfer{
		ID: "tx-er-retry", From: "alice", To: "bob",
		AmountCents: 50000, FeeCents: 77, FeeAccount: "custom-fees",
		IdempotencyKey: "exp-key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate || second.FeeCents != 77 {
		t.Errorf("replay = %+v, want duplicate with fee 77", second)
	}
	if second.FeeTierIndex != -1 || second.FeeRateBps != 0 {
		t.Errorf("explicit fee replay disclosure = (%d, %d bps), want (-1, 0)",
			second.FeeTierIndex, second.FeeRateBps)
	}
}

func TestParseFeeScheduleFlat(t *testing.T) {
	tiers, account, err := ParseFeeSchedule("250:fee-revenue")
	if err != nil {
		t.Fatalf("ParseFeeSchedule: %v", err)
	}
	if account != "fee-revenue" {
		t.Errorf("account = %q, want fee-revenue", account)
	}
	if want := []FeeTier{{MinAmountCents: 0, RateBps: 250}}; !reflect.DeepEqual(tiers, want) {
		t.Errorf("tiers = %+v, want %+v", tiers, want)
	}

	// The parsed flat form behaves exactly like WithTransferFeePolicy.
	l := New(WithTransferFeeSchedule(tiers, account))
	seedBalance(t, l, "alice", 100_000)
	r, err := l.PostTransfer(Transfer{ID: "t-pf", From: "alice", To: "bob", AmountCents: 101})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 2 || r.FeeTierIndex != 0 || r.FeeRateBps != 250 {
		t.Errorf("receipt = %+v, want fee 2 tier 0 rate 250", r)
	}
}

func TestParseFeeScheduleTiered(t *testing.T) {
	tiers, account, err := ParseFeeSchedule("0:0,10000:250,1000000:100@fee-revenue")
	if err != nil {
		t.Fatalf("ParseFeeSchedule: %v", err)
	}
	if account != "fee-revenue" {
		t.Errorf("account = %q, want fee-revenue", account)
	}
	want := []FeeTier{
		{MinAmountCents: 0, RateBps: 0},
		{MinAmountCents: 10000, RateBps: 250},
		{MinAmountCents: 1000000, RateBps: 100},
	}
	if !reflect.DeepEqual(tiers, want) {
		t.Errorf("tiers = %+v, want %+v", tiers, want)
	}

	// A revenue account containing "@" still parses: everything after the
	// last "@" is the account.
	_, account, err = ParseFeeSchedule("0:100@rev@enue")
	if err != nil {
		t.Fatalf("ParseFeeSchedule: %v", err)
	}
	if account != "rev@enue" {
		t.Errorf("account = %q, want rev@enue", account)
	}
}

func TestParseFeeScheduleInvalid(t *testing.T) {
	for _, raw := range []string{
		"250",                                // no revenue account
		"250:",                               // empty revenue account
		":fee-revenue",                       // empty rate
		"abc:fee-revenue",                    // non-integer rate
		"-1:fee-revenue",                     // negative rate: fail fast, not normalized
		"10001:fee-revenue",                  // rate above 10000: fail fast, not clamped
		"0:0,10000:250",                      // tiered without @
		"@fee-revenue",                       // empty tier list
		"0:250@",                             // empty revenue account
		"10000:250@fee-revenue",              // first tier does not start at 0
		"0:250,0:100@fee-revenue",            // duplicate minimums
		"0:250,5000:100,4000:50@fee-revenue", // unsorted minimums
		"0:250,5000:10001@fee-revenue",       // tier rate above 10000
		"0:250,5000:-5@fee-revenue",          // negative tier rate
		"0:250,x:100@fee-revenue",            // non-integer minimum
		"0:250,5000@fee-revenue",             // segment without rate
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := ParseFeeSchedule(raw); err == nil {
				t.Errorf("ParseFeeSchedule(%q): want error, got nil", raw)
			}
		})
	}
}

func TestTransferFeeScheduleSnapshotRoundTrip(t *testing.T) {
	// The tiered policy survives an export/import round trip, and the
	// restored ledger charges the same tiers.
	l := tieredLedger()
	seedBalance(t, l, "alice", 10_000_000)
	if _, err := l.PostTransfer(Transfer{ID: "t-s1", From: "alice", To: "bob", AmountCents: 50000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}

	r, err := restored.PostTransfer(Transfer{ID: "t-s2", From: "alice", To: "bob", AmountCents: 50000})
	if err != nil {
		t.Fatalf("restored PostTransfer: %v", err)
	}
	if r.FeeCents != 1250 || r.FeeTierIndex != 1 || r.FeeRateBps != 250 {
		t.Errorf("restored receipt = %+v, want fee 1250 tier 1 rate 250", r)
	}
	if got := restored.Balance("revenue"); got != 2500 {
		t.Errorf("restored revenue balance = %d, want 2500 (1250 imported + 1250 new)", got)
	}
}
