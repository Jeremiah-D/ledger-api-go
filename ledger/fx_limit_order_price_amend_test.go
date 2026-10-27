package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendLimitOrderPriceSuccess(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	versionBefore := l.Version()
	got, err := l.AmendLimitOrderPrice("lo-1", 105, 100)
	if err != nil {
		t.Fatalf("AmendLimitOrderPrice: %v", err)
	}
	// The amendment moves the limit price and nothing else.
	if got.LimitNum != 105 || got.LimitDen != 100 {
		t.Errorf("limit = %d/%d, want 105/100", got.LimitNum, got.LimitDen)
	}
	if got.FromCurrency != "USD" || got.ToCurrency != "EUR" || got.Side != LimitOrderSell ||
		got.AmountCents != 1000 || got.FilledCents != 0 || got.FillCount != 0 ||
		got.State != LimitOrderPending || !got.Expiry.Equal(before) {
		t.Errorf("amended order = %+v, want untouched 1000 pending expiring at %v", got, before)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The book carries the new price.
	stored, ok := l.LimitOrder("lo-1")
	if !ok || stored.LimitNum != 105 || stored.LimitDen != 100 || stored.State != LimitOrderPending {
		t.Errorf("stored order = %+v,%v, want pending at 105/100", stored, ok)
	}
}

func TestAmendLimitOrderPriceSweepReadsLivePrice(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // bid = ask = 1.08
	// A sell at 110/100 does not touch a 1.08 bid.
	placeSellOrder(t, l, "lo-1", 10000, 110, 100)
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 0 {
		t.Fatalf("sweep before amend = %+v, want 0 fills", res)
	}
	// Lowering the trigger to 105/100 puts the resting order
	// in the money: the next sweep fills it at the live price,
	// without cancelling, replacing, or losing the order.
	if _, err := l.AmendLimitOrderPrice("lo-1", 105, 100); err != nil {
		t.Fatalf("AmendLimitOrderPrice: %v", err)
	}
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sweep after amend = %+v, want 1 fill", res)
	}
	o, _ := l.LimitOrder("lo-1")
	if o.State != LimitOrderFilled || o.FilledCents != 10000 || o.FillCount != 1 {
		t.Errorf("order = %+v, want filled/10000/1 fill", o)
	}
	if o.LimitNum != 105 || o.LimitDen != 100 {
		t.Errorf("filled order limit = %d/%d, want amended 105/100", o.LimitNum, o.LimitDen)
	}
}

func TestAmendLimitOrderPriceRejections(t *testing.T) {
	// Unknown ID.
	l := New(WithFXAccount("fx-pnl"))
	if _, err := l.AmendLimitOrderPrice("nope", 105, 100); !errors.Is(err, ErrLimitOrderNotFound) {
		t.Errorf("unknown: err = %v, want ErrLimitOrderNotFound", err)
	}

	// Non-positive ratios fail, and the original limit is
	// untouched.
	for _, tc := range []struct {
		name string
		num  int64
		den  int64
	}{
		{"zero num", 0, 100},
		{"negative num", -5, 100},
		{"zero den", 105, 0},
		{"negative den", 105, -100},
	} {
		ll := New(WithFXAccount("fx-pnl"))
		placeSellOrder(t, ll, "lo-1", 1000, 110, 100)
		if _, err := ll.AmendLimitOrderPrice("lo-1", tc.num, tc.den); !errors.Is(err, ErrLimitOrderPriceAmendNotPositive) {
			t.Errorf("%s: err = %v, want ErrLimitOrderPriceAmendNotPositive", tc.name, err)
		}
		stored, _ := ll.LimitOrder("lo-1")
		if stored.LimitNum != 110 || stored.LimitDen != 100 {
			t.Errorf("%s: limit moved to %d/%d after rejected amendment, want 110/100", tc.name, stored.LimitNum, stored.LimitDen)
		}
	}

	// Cancelled, filled, and expired orders are terminal.
	lc := New(WithFXAccount("fx-pnl"))
	placeSellOrder(t, lc, "lo-1", 1000, 110, 100)
	if err := lc.CancelLimitOrder("lo-1"); err != nil {
		t.Fatalf("CancelLimitOrder: %v", err)
	}
	if _, err := lc.AmendLimitOrderPrice("lo-1", 105, 100); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancelled: err = %v, want ErrLimitOrderNotPending", err)
	}
	lf := New(WithFXAccount("fx-pnl"))
	fundPayer(t, lf, "seed", 100000)
	mustSetRate(t, lf, "USD", "EUR", 108, 100)
	placeSellOrder(t, lf, "lo-1", 1000, 108, 100)
	if res := lf.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("fill sweep = %+v, want 1 fill", res)
	}
	if _, err := lf.AmendLimitOrderPrice("lo-1", 105, 100); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("filled: err = %v, want ErrLimitOrderNotPending", err)
	}
	le := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, le, "lo-1", 1000, 110, 100, time.Now().Add(time.Hour))
	if res := le.MatchLimitOrders(time.Now().Add(2 * time.Hour)); res.Expires != 1 {
		t.Fatalf("expire sweep = %+v, want 1 expire", res)
	}
	if _, err := le.AmendLimitOrderPrice("lo-1", 105, 100); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("expired: err = %v, want ErrLimitOrderNotPending", err)
	}
}

func TestAmendLimitOrderPriceAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithFXAccount("fx-pnl"), WithAuditLog(al))
	placeSellOrder(t, l, "lo-1", 1000, 110, 100)
	ctx := ContextWithActor(context.Background(), "ops-bob")
	if _, err := l.AmendLimitOrderPriceCtx(ctx, "lo-1", 105, 100); err != nil {
		t.Fatalf("AmendLimitOrderPriceCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_limit_order_price_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-bob" {
		t.Errorf("event actor = %q, want ops-bob", events[0].Actor)
	}
	d := events[0].Details
	if d["order_id"] != "lo-1" || d["previous_limit_num"] != float64(110) ||
		d["previous_limit_den"] != float64(100) || d["limit_num"] != float64(105) ||
		d["limit_den"] != float64(100) {
		t.Errorf("event details = %+v, want lo-1 110/100 -> 105/100", d)
	}
}
