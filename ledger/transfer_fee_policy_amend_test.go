package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSetTransferFeeScheduleAppliesToFutureTransfersOnly(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 10_000_000)

	// Old schedule: 20000 falls in tier 1 at 2.5% -> 500 to "revenue".
	first, err := l.PostTransfer(Transfer{
		ID: "tx-fee-old", From: "alice", To: "bob",
		AmountCents: 20000, IdempotencyKey: "fee-amend-key",
	})
	if err != nil {
		t.Fatalf("PostTransfer (old schedule): %v", err)
	}
	if first.FeeCents != 500 || first.FeeTierIndex != 1 || first.FeeRateBps != 250 {
		t.Fatalf("old receipt = %+v, want fee 500 tier 1 rate 250", first)
	}

	tiers := []FeeTier{{MinAmountCents: 0, RateBps: 100}, {MinAmountCents: 10000, RateBps: 50}}
	if err := l.SetTransferFeeSchedule(tiers, "new-revenue"); err != nil {
		t.Fatalf("SetTransferFeeSchedule: %v", err)
	}
	// Mutating the caller's slice after the call must not reprice.
	tiers[1].RateBps = 9999

	// New schedule: 20000 falls in tier 1 at 0.5% -> 100 to "new-revenue".
	second, err := l.PostTransfer(Transfer{ID: "tx-fee-new", From: "alice", To: "bob", AmountCents: 20000})
	if err != nil {
		t.Fatalf("PostTransfer (new schedule): %v", err)
	}
	if second.FeeCents != 100 || second.FeeTierIndex != 1 || second.FeeRateBps != 50 {
		t.Errorf("new receipt = %+v, want fee 100 tier 1 rate 50", second)
	}
	if len(second.Entries) != 2 || second.Entries[1].DebitAccount != "new-revenue" {
		t.Errorf("new fee leg = %+v, want debit new-revenue", second.Entries)
	}

	// Replaying the pre-amendment key returns the original fee leg,
	// not a repriced one, and books nothing new. The tier
	// disclosure is exempt: it re-derives under the live schedule
	// (see the module doc), so after a repricing amendment it
	// may fall back to -1 while the journaled leg stays intact.
	replay, err := l.PostTransfer(Transfer{
		ID: "tx-fee-old-retry", From: "alice", To: "bob",
		AmountCents: 20000, IdempotencyKey: "fee-amend-key",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !replay.Duplicate || replay.FeeCents != 500 || !reflect.DeepEqual(replay.Entries, first.Entries) {
		t.Errorf("replay = %+v, want duplicate with original 500-cent fee leg", replay)
	}

	// Explicit fee and SkipFee still win over the new default.
	explicit, err := l.PostTransfer(Transfer{
		ID: "tx-fee-exp", From: "alice", To: "bob",
		AmountCents: 20000, FeeCents: 77, FeeAccount: "custom-fees",
	})
	if err != nil {
		t.Fatalf("explicit PostTransfer: %v", err)
	}
	if explicit.FeeCents != 77 || explicit.FeeTierIndex != -1 {
		t.Errorf("explicit receipt = %+v, want fee 77 tier -1", explicit)
	}
	skipped, err := l.PostTransfer(Transfer{ID: "tx-fee-skip", From: "alice", To: "bob", AmountCents: 20000, SkipFee: true})
	if err != nil {
		t.Fatalf("skip PostTransfer: %v", err)
	}
	if skipped.FeeCents != 0 || skipped.FeeTierIndex != -1 {
		t.Errorf("skip receipt = %+v, want fee 0 tier -1", skipped)
	}

	if got := l.Balance("revenue"); got != 500 {
		t.Errorf("old revenue balance = %d, want 500 (old leg only)", got)
	}
	if got := l.Balance("new-revenue"); got != 100 {
		t.Errorf("new revenue balance = %d, want 100 (new leg only)", got)
	}
}

func TestSetTransferFeeScheduleRejectsInvalidPreservesPolicy(t *testing.T) {
	cases := []struct {
		name    string
		tiers   []FeeTier
		revenue AccountID
	}{
		{"nil tiers", nil, "new-revenue"},
		{"empty tiers", []FeeTier{}, "new-revenue"},
		{"empty revenue", []FeeTier{{MinAmountCents: 0, RateBps: 100}}, ""},
		{"negative rate", []FeeTier{{MinAmountCents: 0, RateBps: -1}}, "new-revenue"},
		{"rate above 10000", []FeeTier{{MinAmountCents: 0, RateBps: 10001}}, "new-revenue"},
		{"first tier not zero", []FeeTier{{MinAmountCents: 500, RateBps: 100}}, "new-revenue"},
		{"negative minimum", []FeeTier{{MinAmountCents: -1, RateBps: 100}, {MinAmountCents: 100, RateBps: 100}}, "new-revenue"},
		{"duplicate minimums", []FeeTier{{MinAmountCents: 0, RateBps: 100}, {MinAmountCents: 100, RateBps: 100}, {MinAmountCents: 100, RateBps: 200}}, "new-revenue"},
		{"decreasing minimums", []FeeTier{{MinAmountCents: 0, RateBps: 100}, {MinAmountCents: 10000, RateBps: 100}, {MinAmountCents: 5000, RateBps: 100}}, "new-revenue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := tieredLedger()
			seedBalance(t, l, "alice", 10_000_000)
			if err := l.SetTransferFeeSchedule(tc.tiers, tc.revenue); !errors.Is(err, ErrInvalidTransferFeeSchedule) {
				t.Fatalf("SetTransferFeeSchedule: err = %v, want ErrInvalidTransferFeeSchedule", err)
			}
			// The previous schedule is exactly as it was: 20000
			// still costs 500 at tier 1 to the old revenue account.
			r, err := l.PostTransfer(Transfer{ID: "tx-fee-preserved", From: "alice", To: "bob", AmountCents: 20000})
			if err != nil {
				t.Fatalf("PostTransfer after rejection: %v", err)
			}
			if r.FeeCents != 500 || r.FeeTierIndex != 1 || r.FeeRateBps != 250 {
				t.Errorf("receipt after rejection = %+v, want fee 500 tier 1 rate 250", r)
			}
			if got := l.Balance("revenue"); got != 500 {
				t.Errorf("revenue balance = %d, want 500", got)
			}
			if got := l.Balance("new-revenue"); got != 0 {
				t.Errorf("new-revenue balance = %d, want 0 (rejected schedule never charged)", got)
			}
		})
	}
}

func TestSetTransferFeePolicyFlat(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10_000_000)

	if err := l.SetTransferFeePolicy(250, "revenue"); err != nil {
		t.Fatalf("SetTransferFeePolicy(250): %v", err)
	}
	r, err := l.PostTransfer(Transfer{ID: "tx-flat-1", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.FeeCents != 250 || r.FeeTierIndex != 0 || r.FeeRateBps != 250 {
		t.Errorf("flat receipt = %+v, want fee 250 tier 0 rate 250", r)
	}

	// Negative and above-10000 flat rates are rejected, not
	// normalized, and the installed 250 bps policy survives.
	for _, rate := range []int64{-1, 10001} {
		if err := l.SetTransferFeePolicy(rate, "new-revenue"); !errors.Is(err, ErrInvalidTransferFeeSchedule) {
			t.Errorf("SetTransferFeePolicy(%d): err = %v, want ErrInvalidTransferFeeSchedule", rate, err)
		}
	}
	r, err = l.PostTransfer(Transfer{ID: "tx-flat-2", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer after rejections: %v", err)
	}
	if r.FeeCents != 250 || r.FeeRateBps != 250 {
		t.Errorf("receipt after rejections = %+v, want fee 250 rate 250", r)
	}

	// A 0 rate via the Ctx form is legal: fee-free, tier disclosed.
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetTransferFeePolicyCtx(ctx, 0, "revenue"); err != nil {
		t.Fatalf("SetTransferFeePolicyCtx(0): %v", err)
	}
	r, err = l.PostTransfer(Transfer{ID: "tx-flat-3", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer (0 rate): %v", err)
	}
	if r.FeeCents != 0 || r.FeeTierIndex != 0 || r.FeeRateBps != 0 {
		t.Errorf("zero-rate receipt = %+v, want fee 0 tier 0 rate 0", r)
	}
}

func TestSetTransferFeeScheduleAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTransferFeeSchedule([]FeeTier{
		{MinAmountCents: 0, RateBps: 0},
		{MinAmountCents: 10000, RateBps: 250},
	}, "revenue"))
	seedBalance(t, l, "alice", 10_000_000)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetTransferFeeScheduleCtx(ctx, []FeeTier{
		{MinAmountCents: 0, RateBps: 100},
		{MinAmountCents: 10000, RateBps: 50},
	}, "new-revenue"); err != nil {
		t.Fatalf("SetTransferFeeScheduleCtx: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"fee_tiers"`) || !strings.Contains(buf.String(), "new-revenue") {
		t.Errorf("snapshot config line lacks amended fee_tiers/new-revenue")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	r, err := restored.PostTransfer(Transfer{ID: "tx-fee-restored", From: "alice", To: "bob", AmountCents: 20000})
	if err != nil {
		t.Fatalf("restored PostTransfer: %v", err)
	}
	if r.FeeCents != 100 || r.FeeTierIndex != 1 || r.FeeRateBps != 50 {
		t.Errorf("restored receipt = %+v, want fee 100 tier 1 rate 50", r)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_fee_schedule_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	if events[0].Details["revenue_account"] != "new-revenue" || events[0].Details["previous_revenue_account"] != "revenue" {
		t.Errorf("event details = %+v, want revenue -> new-revenue", events[0].Details)
	}
}
