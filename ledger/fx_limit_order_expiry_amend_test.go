package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendLimitOrderExpiryShorten(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	versionBefore := l.Version()
	shorter := now.Add(30 * time.Minute).Truncate(time.Second)
	got, err := l.AmendLimitOrderExpiry("lo-1", shorter)
	if err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
	}
	// The amendment moves the deadline and nothing else.
	if !got.Expiry.Equal(shorter) {
		t.Errorf("expiry = %v, want %v", got.Expiry, shorter)
	}
	if got.LimitNum != 110 || got.LimitDen != 100 || got.AmountCents != 1000 ||
		got.FilledCents != 0 || got.FillCount != 0 || got.Side != LimitOrderSell ||
		got.State != LimitOrderPending {
		t.Errorf("amended order = %+v, want untouched 110/100 1000 pending", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The book carries the new deadline.
	stored, ok := l.LimitOrder("lo-1")
	if !ok || !stored.Expiry.Equal(shorter) || stored.State != LimitOrderPending {
		t.Errorf("stored order = %+v,%v, want pending with expiry %v", stored, ok, shorter)
	}
	// The sweep reads the live deadline: a sweep before the new
	// deadline expires nothing, and one at the new deadline
	// expires the order — sooner than the old deadline would have.
	if res := l.MatchLimitOrders(now.Add(20 * time.Minute)); res.Expires != 0 {
		t.Errorf("sweep before new expiry: %+v, want 0 expires", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderPending {
		t.Errorf("state after early sweep = %q, want pending", o.State)
	}
	if res := l.MatchLimitOrders(shorter); res.Expires != 1 {
		t.Errorf("sweep at new expiry: %+v, want 1 expire", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderExpired {
		t.Errorf("state after new-expiry sweep = %q, want expired", o.State)
	}
}

func TestAmendLimitOrderExpiryLengthen(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	longer := now.Add(3 * time.Hour).Truncate(time.Second)
	got, err := l.AmendLimitOrderExpiry("lo-1", longer)
	if err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
	}
	if !got.Expiry.Equal(longer) {
		t.Errorf("expiry = %v, want %v", got.Expiry, longer)
	}
	// A sweep at the old deadline expires nothing now.
	if res := l.MatchLimitOrders(before); res.Expires != 0 {
		t.Errorf("sweep at old expiry: %+v, want 0 expires", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderPending {
		t.Errorf("state after old-expiry sweep = %q, want pending", o.State)
	}
	if res := l.MatchLimitOrders(longer); res.Expires != 1 {
		t.Errorf("sweep at new expiry: %+v, want 1 expire", res)
	}
}

func TestAmendLimitOrderExpiryClearToNeverExpires(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	got, err := l.AmendLimitOrderExpiry("lo-1", time.Time{})
	if err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
	}
	if !got.Expiry.IsZero() {
		t.Errorf("expiry = %v, want zero (never expires)", got.Expiry)
	}
	if got.State != LimitOrderPending {
		t.Errorf("state = %q, want pending", got.State)
	}
	// A sweep far past the old deadline expires nothing: the
	// cleared order never expires.
	if res := l.MatchLimitOrders(now.Add(2 * time.Hour)); res.Expires != 0 {
		t.Errorf("sweep past old expiry: %+v, want 0 expires", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderPending || !o.Expiry.IsZero() {
		t.Errorf("stored order = %+v, want pending with zero expiry", o)
	}
}

func TestAmendLimitOrderExpirySetOnNoExpiryOrder(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	now := time.Now()
	placeSellOrder(t, l, "lo-1", 1000, 110, 100)
	deadline := now.Add(time.Hour).Truncate(time.Second)
	got, err := l.AmendLimitOrderExpiry("lo-1", deadline)
	if err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
	}
	if !got.Expiry.Equal(deadline) {
		t.Errorf("expiry = %v, want %v", got.Expiry, deadline)
	}
	// The newly set deadline governs the next sweep.
	if res := l.MatchLimitOrders(deadline); res.Expires != 1 {
		t.Errorf("sweep at new expiry: %+v, want 1 expire", res)
	}
	if o, _ := l.LimitOrder("lo-1"); o.State != LimitOrderExpired {
		t.Errorf("state after sweep = %q, want expired", o.State)
	}
}

func TestAmendLimitOrderExpiryKeepsFills(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 6000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // bid = ask = 1.08
	placeExpiringSellOrder(t, l, "lo-1", 10000, 108, 100, time.Now().Add(time.Hour))
	// The first sweep fills only what is funded: a 6000 partial
	// fill, leaving 4000 resting.
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("first sweep = %+v, want 1 fill", res)
	}
	o, _ := l.LimitOrder("lo-1")
	if o.FilledCents != 6000 || o.FillCount != 1 || o.State != LimitOrderPending {
		t.Fatalf("after first fill order = %+v, want pending/6000/1 fill", o)
	}
	// Shortening the deadline keeps the settled fill exactly.
	shorter := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	got, err := l.AmendLimitOrderExpiry("lo-1", shorter)
	if err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
	}
	if !got.Expiry.Equal(shorter) || got.FilledCents != 6000 || got.FillCount != 1 ||
		got.AmountCents != 10000 || got.State != LimitOrderPending {
		t.Errorf("amended order = %+v, want pending 6000 filled, expiry %v", got, shorter)
	}
	// The remainder still fills at the live price before the new
	// deadline, without losing the first fill.
	fundPayer(t, l, "seed2", 5000)
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sweep after amend = %+v, want 1 fill", res)
	}
	o, _ = l.LimitOrder("lo-1")
	if o.State != LimitOrderFilled || o.FilledCents != 10000 || o.FillCount != 2 {
		t.Errorf("order = %+v, want filled/10000/2 fills", o)
	}
}

func TestAmendLimitOrderExpiryRejections(t *testing.T) {
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	later := now.Add(3 * time.Hour).Truncate(time.Second)

	// Unknown ID.
	l := New(WithFXAccount("fx-pnl"))
	if _, err := l.AmendLimitOrderExpiry("nope", later); !errors.Is(err, ErrLimitOrderNotFound) {
		t.Errorf("unknown: err = %v, want ErrLimitOrderNotFound", err)
	}

	// A past new deadline fails, and the original deadline is
	// untouched. Zero is the only non-future value allowed.
	for _, tc := range []struct {
		name string
		next time.Time
	}{
		{"past", now.Add(-time.Hour)},
		{"just past", now.Add(-time.Second)},
	} {
		ll := New(WithFXAccount("fx-pnl"))
		placeExpiringSellOrder(t, ll, "lo-1", 1000, 110, 100, before)
		if _, err := ll.AmendLimitOrderExpiry("lo-1", tc.next); !errors.Is(err, ErrLimitOrderExpiryAmendInvalid) {
			t.Errorf("%s: err = %v, want ErrLimitOrderExpiryAmendInvalid", tc.name, err)
		}
		if o, _ := ll.LimitOrder("lo-1"); !o.Expiry.Equal(before) {
			t.Errorf("%s: expiry moved to %v after rejected amendment", tc.name, o.Expiry)
		}
	}

	// Cancelled, filled, and swept-expired orders are terminal.
	lc := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, lc, "lo-1", 1000, 110, 100, before)
	if err := lc.CancelLimitOrder("lo-1"); err != nil {
		t.Fatalf("CancelLimitOrder: %v", err)
	}
	if _, err := lc.AmendLimitOrderExpiry("lo-1", later); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancelled: err = %v, want ErrLimitOrderNotPending", err)
	}

	lf := New(WithFXAccount("fx-pnl"))
	fundPayer(t, lf, "seed", 100000)
	mustSetRate(t, lf, "USD", "EUR", 120, 100)
	placeExpiringSellOrder(t, lf, "lo-1", 1000, 110, 100, before)
	if res := lf.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("fill sweep: %+v, want 1 fill", res)
	}
	if _, err := lf.AmendLimitOrderExpiry("lo-1", later); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("filled: err = %v, want ErrLimitOrderNotPending", err)
	}

	le := New(WithFXAccount("fx-pnl"))
	placeExpiringSellOrder(t, le, "lo-1", 1000, 110, 100, before)
	if res := le.MatchLimitOrders(now.Add(2 * time.Hour)); res.Expires != 1 {
		t.Fatalf("expire sweep: %+v, want 1 expire", res)
	}
	if _, err := le.AmendLimitOrderExpiry("lo-1", later); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("expired: err = %v, want ErrLimitOrderNotPending", err)
	}
}

func TestAmendLimitOrderExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithFXAccount("fx-pnl"), WithAuditLog(al))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	later := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	ctx := ContextWithActor(context.Background(), "ops-bob")
	if _, err := l.AmendLimitOrderExpiryCtx(ctx, "lo-1", later); err != nil {
		t.Fatalf("AmendLimitOrderExpiryCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_limit_order_expiry_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-bob" {
		t.Errorf("event actor = %q, want ops-bob", events[0].Actor)
	}
	d := events[0].Details
	if d["order_id"] != "lo-1" {
		t.Errorf("event details = %+v, want order lo-1", d)
	}
	if d["previous_expiry"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expiry = %v, want %v", d["previous_expiry"], before.UTC().Format(time.RFC3339))
	}
	if d["expiry"] != later.UTC().Format(time.RFC3339) {
		t.Errorf("expiry = %v, want %v", d["expiry"], later.UTC().Format(time.RFC3339))
	}
}

func TestAmendLimitOrderExpirySnapshotRoundTrip(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	placeExpiringSellOrder(t, l, "lo-1", 1000, 110, 100, before)
	later := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := l.AmendLimitOrderExpiry("lo-1", later); err != nil {
		t.Fatalf("AmendLimitOrderExpiry: %v", err)
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
	if !ok || !restored.Expiry.Equal(later) || restored.LimitNum != 110 || restored.State != LimitOrderPending {
		t.Errorf("restored order = %+v,%v, want pending at 110/100 expiring %v", restored, ok, later)
	}
}
