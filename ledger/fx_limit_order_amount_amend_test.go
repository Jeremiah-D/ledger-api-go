package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendLimitOrderAmountSuccess(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	versionBefore := l.Version()
	got, err := l.AmendLimitOrderAmount("lo-1", 1500)
	if err != nil {
		t.Fatalf("AmendLimitOrderAmount: %v", err)
	}
	// The amendment moves the size and nothing else.
	if got.AmountCents != 1500 {
		t.Errorf("amount = %d, want 1500", got.AmountCents)
	}
	if got.FromCurrency != "USD" || got.ToCurrency != "EUR" || got.Side != LimitOrderSell ||
		got.LimitNum != 110 || got.LimitDen != 100 || got.FilledCents != 0 || got.FillCount != 0 ||
		got.State != LimitOrderPending || !got.Expiry.Equal(before) {
		t.Errorf("amended order = %+v, want untouched 110/100 pending expiring at %v", got, before)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The book carries the new size.
	stored, ok := l.LimitOrder("lo-1")
	if !ok || stored.AmountCents != 1500 || stored.State != LimitOrderPending {
		t.Errorf("stored order = %+v,%v, want pending at 1500", stored, ok)
	}
}

func TestAmendLimitOrderAmountSweepReadsLiveAmount(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 6000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // bid = ask = 1.08
	placeSellOrder(t, l, "lo-1", 10000, 108, 100)
	// The first sweep fills only what is funded: a 6000 partial
	// fill, leaving 4000 resting.
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("first sweep = %+v, want 1 fill", res)
	}
	o, _ := l.LimitOrder("lo-1")
	if o.FilledCents != 6000 || o.State != LimitOrderPending {
		t.Fatalf("after first fill order = %+v, want pending/6000 filled", o)
	}
	// Growing the order to 8000 re-opens 2000 of remainder; the
	// next sweep fills exactly that, at the live price, without
	// cancelling, replacing, or losing the order's first fill.
	if _, err := l.AmendLimitOrderAmount("lo-1", 8000); err != nil {
		t.Fatalf("AmendLimitOrderAmount: %v", err)
	}
	fundPayer(t, l, "seed2", 5000)
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sweep after amend = %+v, want 1 fill", res)
	}
	o, _ = l.LimitOrder("lo-1")
	if o.State != LimitOrderFilled || o.FilledCents != 8000 || o.FillCount != 2 {
		t.Errorf("order = %+v, want filled/8000/2 fills", o)
	}
	if o.AmountCents != 8000 {
		t.Errorf("filled order amount = %d, want amended 8000", o.AmountCents)
	}
}

func TestAmendLimitOrderAmountShrinkCapsNextFill(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	placeSellOrder(t, l, "lo-1", 10000, 108, 100)
	// Shrinking before any fill caps the fill at the new size:
	// the sweep fills 4000, not the original 10000, and the order
	// is filled — the remainder is gone, not deferred.
	if _, err := l.AmendLimitOrderAmount("lo-1", 4000); err != nil {
		t.Fatalf("AmendLimitOrderAmount: %v", err)
	}
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sweep after shrink = %+v, want 1 fill", res)
	}
	o, _ := l.LimitOrder("lo-1")
	if o.State != LimitOrderFilled || o.FilledCents != 4000 || o.AmountCents != 4000 {
		t.Errorf("order = %+v, want filled/4000/4000", o)
	}
}

func TestAmendLimitOrderAmountRejections(t *testing.T) {
	// Unknown ID.
	l := New(WithFXAccount("fx-pnl"))
	if _, err := l.AmendLimitOrderAmount("nope", 1500); !errors.Is(err, ErrLimitOrderNotFound) {
		t.Errorf("unknown: err = %v, want ErrLimitOrderNotFound", err)
	}

	// Non-positive amounts fail, and the original amount is
	// untouched.
	for _, tc := range []struct {
		name   string
		amount int64
	}{
		{"zero", 0},
		{"negative", -5},
	} {
		ll := New(WithFXAccount("fx-pnl"))
		placeSellOrder(t, ll, "lo-1", 1000, 110, 100)
		if _, err := ll.AmendLimitOrderAmount("lo-1", tc.amount); !errors.Is(err, ErrLimitOrderAmountAmendInvalid) {
			t.Errorf("%s: err = %v, want ErrLimitOrderAmountAmendInvalid", tc.name, err)
		}
		stored, _ := ll.LimitOrder("lo-1")
		if stored.AmountCents != 1000 {
			t.Errorf("%s: amount moved to %d after rejected amendment, want 1000", tc.name, stored.AmountCents)
		}
	}

	// Below the filled amount fails: settled fills are facts.
	lf := New(WithFXAccount("fx-pnl"))
	fundPayer(t, lf, "seed", 6000)
	mustSetRate(t, lf, "USD", "EUR", 108, 100)
	placeSellOrder(t, lf, "lo-1", 10000, 108, 100)
	if res := lf.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("partial fill sweep = %+v, want 1 fill", res)
	}
	if _, err := lf.AmendLimitOrderAmount("lo-1", 5000); !errors.Is(err, ErrLimitOrderAmountAmendInvalid) {
		t.Errorf("below filled: err = %v, want ErrLimitOrderAmountAmendInvalid", err)
	}
	stored, _ := lf.LimitOrder("lo-1")
	if stored.AmountCents != 10000 || stored.FilledCents != 6000 {
		t.Errorf("below filled: order = %+v, want amount 10000 filled 6000", stored)
	}
	// Exactly the filled amount is legal: it closes the remainder
	// without moving the state machine.
	if got, err := lf.AmendLimitOrderAmount("lo-1", 6000); err != nil {
		t.Errorf("at filled amount: err = %v, want nil", err)
	} else if got.AmountCents != 6000 || got.State != LimitOrderPending {
		t.Errorf("at filled amount: order = %+v, want pending/6000", got)
	}

	// Cancelled, filled, and expired orders are terminal.
	lc := New(WithFXAccount("fx-pnl"))
	placeSellOrder(t, lc, "lo-1", 1000, 110, 100)
	if err := lc.CancelLimitOrder("lo-1"); err != nil {
		t.Fatalf("CancelLimitOrder: %v", err)
	}
	if _, err := lc.AmendLimitOrderAmount("lo-1", 1500); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancelled: err = %v, want ErrLimitOrderNotPending", err)
	}
	lfill := New(WithFXAccount("fx-pnl"))
	fundPayer(t, lfill, "seed", 100000)
	mustSetRate(t, lfill, "USD", "EUR", 108, 100)
	placeSellOrder(t, lfill, "lo-1", 1000, 108, 100)
	if res := lfill.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("fill sweep = %+v, want 1 fill", res)
	}
	if _, err := lfill.AmendLimitOrderAmount("lo-1", 1500); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("filled: err = %v, want ErrLimitOrderNotPending", err)
	}
	le := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, le, "lo-1", 1000, 110, 100, time.Now().Add(time.Hour))
	if res := le.MatchLimitOrders(time.Now().Add(2 * time.Hour)); res.Expires != 1 {
		t.Fatalf("expire sweep = %+v, want 1 expire", res)
	}
	if _, err := le.AmendLimitOrderAmount("lo-1", 1500); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("expired: err = %v, want ErrLimitOrderNotPending", err)
	}
}

func TestAmendLimitOrderAmountAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithFXAccount("fx-pnl"), WithAuditLog(al))
	placeSellOrder(t, l, "lo-1", 1000, 110, 100)
	ctx := ContextWithActor(context.Background(), "ops-bob")
	if _, err := l.AmendLimitOrderAmountCtx(ctx, "lo-1", 1500); err != nil {
		t.Fatalf("AmendLimitOrderAmountCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_limit_order_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-bob" {
		t.Errorf("event actor = %q, want ops-bob", events[0].Actor)
	}
	d := events[0].Details
	if d["order_id"] != "lo-1" || d["previous_amount_cents"] != float64(1000) ||
		d["amount_cents"] != float64(1500) || d["filled_cents"] != float64(0) {
		t.Errorf("event details = %+v, want lo-1 1000 -> 1500 filled 0", d)
	}
}

func TestAmendLimitOrderAmountSnapshotRoundTrip(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	placeSellOrder(t, l, "lo-1", 1000, 110, 100)
	if _, err := l.AmendLimitOrderAmount("lo-1", 1500); err != nil {
		t.Fatalf("AmendLimitOrderAmount: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	restored, ok := l2.LimitOrder("lo-1")
	if !ok || restored.AmountCents != 1500 || restored.LimitNum != 110 || restored.State != LimitOrderPending {
		t.Errorf("restored order = %+v,%v, want pending 1500 at 110/100", restored, ok)
	}
}
