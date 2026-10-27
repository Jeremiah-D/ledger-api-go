package ledger

import (
	"errors"
	"testing"
	"time"
)

// placeExpiringSellOrder parks a sell order with an expiry: the
// placeSellOrder helper carries no expiry, so expiring orders need
// their own placement.
func placeExpiringSellOrder(t *testing.T, l *Ledger, id string, amount int64, limitNum, limitDen int64, expiry time.Time) {
	t.Helper()
	err := l.PlaceLimitOrder(LimitOrder{
		ID: id, FromAccount: "payer", ToAccount: "payee",
		FromCurrency: "USD", ToCurrency: "EUR",
		Side: LimitOrderSell, LimitNum: limitNum, LimitDen: limitDen,
		AmountCents: amount, Expiry: expiry,
	})
	if err != nil {
		t.Fatalf("PlaceLimitOrder(%s): %v", id, err)
	}
}

func TestExtendLimitOrderExpirySuccess(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	versionBefore := l.Version()
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	got, err := l.ExtendLimitOrderExpiry("lo-1", extended)
	if err != nil {
		t.Fatalf("ExtendLimitOrderExpiry: %v", err)
	}
	// The extension moves the deadline and nothing else.
	if !got.Expiry.Equal(extended) {
		t.Errorf("expiry = %v, want %v", got.Expiry, extended)
	}
	if got.LimitNum != 110 || got.LimitDen != 100 || got.AmountCents != 1000 ||
		got.FilledCents != 0 || got.FillCount != 0 || got.Side != LimitOrderSell ||
		got.State != LimitOrderPending {
		t.Errorf("extended order = %+v, want untouched 110/100 1000 pending", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: extension is off-journal")
	}
	// The book carries the new deadline.
	stored, ok := l.LimitOrder("lo-1")
	if !ok || !stored.Expiry.Equal(extended) || stored.State != LimitOrderPending {
		t.Errorf("stored order = %+v,%v, want pending with expiry %v", stored, ok, extended)
	}
	// The sweep reads the live deadline: a sweep at the old
	// deadline expires nothing, and one at the new deadline
	// expires the order.
	if res := l.MatchLimitOrders(before); res.Expires != 0 {
		t.Errorf("sweep at old expiry: %+v, want 0 expires", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderPending {
		t.Errorf("state after old-expiry sweep = %q, want pending", o.State)
	}
	if res := l.MatchLimitOrders(extended); res.Expires != 1 {
		t.Errorf("sweep at new expiry: %+v, want 1 expire", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderExpired {
		t.Errorf("state after new-expiry sweep = %q, want expired", o.State)
	}
}

func TestExtendLimitOrderExpiryRejections(t *testing.T) {
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	newer := now.Add(3 * time.Hour).Truncate(time.Second)

	// Unknown ID.
	l := New(WithFXAccount("fx-pnl"))
	if _, err := l.ExtendLimitOrderExpiry("nope", newer); !errors.Is(err, ErrLimitOrderNotFound) {
		t.Errorf("unknown: err = %v, want ErrLimitOrderNotFound", err)
	}

	// No-expiry orders never expire, so there is nothing to move.
	placeSellOrder(t, l, "lo-noexpiry", 1000, 110, 100)
	if _, err := l.ExtendLimitOrderExpiry("lo-noexpiry", newer); !errors.Is(err, ErrLimitOrderExpiryNotSet) {
		t.Errorf("no expiry: err = %v, want ErrLimitOrderExpiryNotSet", err)
	}
	if o, _ := l.LimitOrder("lo-noexpiry"); !o.Expiry.IsZero() {
		t.Errorf("no-expiry order gained an expiry: %v", o.Expiry)
	}

	// Equal, earlier, and past new deadlines all fail, and the
	// original deadline is untouched.
	for _, tc := range []struct {
		name string
		next time.Time
	}{
		{"equal", before},
		{"earlier", now.Add(30 * time.Minute)},
		{"past", now.Add(-time.Hour)},
	} {
		ll := New(WithFXAccount("fx-pnl"))
		placeExpiringSellOrder(t, ll, "lo-1", 1000, 110, 100, before)
		if _, err := ll.ExtendLimitOrderExpiry("lo-1", tc.next); !errors.Is(err, ErrLimitOrderExpiryExtensionNotLater) {
			t.Errorf("%s: err = %v, want ErrLimitOrderExpiryExtensionNotLater", tc.name, err)
		}
		if o, _ := ll.LimitOrder("lo-1"); !o.Expiry.Equal(before) {
			t.Errorf("%s: expiry moved to %v after rejected extension", tc.name, o.Expiry)
		}
	}

	// Cancelled, filled, and swept-expired orders are terminal.
	lc := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, lc, "lo-1", 1000, 110, 100, before)
	if err := lc.CancelLimitOrder("lo-1"); err != nil {
		t.Fatalf("CancelLimitOrder: %v", err)
	}
	if _, err := lc.ExtendLimitOrderExpiry("lo-1", newer); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancelled: err = %v, want ErrLimitOrderNotPending", err)
	}

	lf := New(WithFXAccount("fx-pnl"))
	fundPayer(t, lf, "seed", 100000)
	if err := lf.SetFXRate("USD", "EUR", 120, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	placeExpiringSellOrder(t, lf, "lo-1", 1000, 110, 100, before)
	if res := lf.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("fill sweep: %+v, want 1 fill", res)
	}
	if _, err := lf.ExtendLimitOrderExpiry("lo-1", newer); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("filled: err = %v, want ErrLimitOrderNotPending", err)
	}

	le := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, le, "lo-1", 1000, 110, 100, before)
	if res := le.MatchLimitOrders(now.Add(2 * time.Hour)); res.Expires != 1 {
		t.Fatalf("expire sweep: %+v, want 1 expire", res)
	}
	if _, err := le.ExtendLimitOrderExpiry("lo-1", newer); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("expired: err = %v, want ErrLimitOrderNotPending", err)
	}
}

func TestExtendLimitOrderExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithFXAccount("fx-pnl"), WithAuditLog(al))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := l.ExtendLimitOrderExpiry("lo-1", extended); err != nil {
		t.Fatalf("ExtendLimitOrderExpiry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_limit_order_expiry_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["order_id"] != "lo-1" {
		t.Errorf("event details = %+v, want order lo-1", d)
	}
	if d["previous_expiry"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expiry = %v, want %v", d["previous_expiry"], before.UTC().Format(time.RFC3339))
	}
	if d["expiry"] != extended.UTC().Format(time.RFC3339) {
		t.Errorf("expiry = %v, want %v", d["expiry"], extended.UTC().Format(time.RFC3339))
	}
}
