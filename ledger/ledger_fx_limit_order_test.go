package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// fundPayer credits the payer with USD; the caller picks a unique entry
// ID per funding because Post treats a repeated ID as a duplicate replay.
func fundPayer(t *testing.T, l *Ledger, id string, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "payer", CreditAccount: "bank", AmountCents: cents, Currency: "USD"}); err != nil {
		t.Fatalf("fund payer: %v", err)
	}
}

func placeSellOrder(t *testing.T, l *Ledger, id string, amount int64, limitNum, limitDen int64) {
	t.Helper()
	err := l.PlaceLimitOrder(LimitOrder{
		ID: id, FromAccount: "payer", ToAccount: "payee",
		FromCurrency: "USD", ToCurrency: "EUR",
		Side: LimitOrderSell, LimitNum: limitNum, LimitDen: limitDen,
		AmountCents: amount,
	})
	if err != nil {
		t.Fatalf("PlaceLimitOrder(%s): %v", id, err)
	}
}

func TestLimitOrderPlacementValidation(t *testing.T) {
	fresh := func() *Ledger { return New(WithFXAccount("fx-pnl")) }
	now := time.Now()
	bad := []struct {
		name  string
		order LimitOrder
		want  error
	}{
		{"empty ID", LimitOrder{FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"empty from currency", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"lowercase currency", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "usd", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"same currency", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "USD", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"bad side", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "hold", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"zero limit num", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 0, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"zero limit den", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 0, AmountCents: 1}, ErrInvalidLimitOrder},
		{"zero amount", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 0}, ErrInvalidLimitOrder},
		{"negative amount", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: -5}, ErrInvalidLimitOrder},
		{"empty accounts", LimitOrder{ID: "x", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"same account", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "a", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1}, ErrInvalidLimitOrder},
		{"past expiry", LimitOrder{ID: "x", FromAccount: "a", ToAccount: "b", FromCurrency: "USD", ToCurrency: "EUR", Side: "sell", LimitNum: 1, LimitDen: 1, AmountCents: 1, Expiry: now.Add(-time.Hour)}, ErrInvalidLimitOrder},
	}
	for _, tc := range bad {
		if err := fresh().PlaceLimitOrder(tc.order); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, err)
		}
	}
	// A rejected order is not in the book.
	l := fresh()
	if err := l.PlaceLimitOrder(LimitOrder{ID: "", FromCurrency: "USD", ToCurrency: "EUR"}); err == nil {
		t.Fatal("empty order must be rejected")
	}
	if _, ok := l.LimitOrder(""); ok {
		t.Error("rejected order must not be in the book")
	}

	// Duplicate ID.
	l2 := fresh()
	placeSellOrder(t, l2, "dup", 100, 108, 100)
	err := l2.PlaceLimitOrder(LimitOrder{
		ID: "dup", FromAccount: "payer", ToAccount: "payee",
		FromCurrency: "USD", ToCurrency: "EUR",
		Side: LimitOrderSell, LimitNum: 108, LimitDen: 100, AmountCents: 100,
	})
	if !errors.Is(err, ErrDuplicateLimitOrder) {
		t.Errorf("duplicate ID: want ErrDuplicateLimitOrder, got %v", err)
	}

	// Side is canonicalized case-insensitively; empty side defaults to sell.
	l3 := fresh()
	if err := l3.PlaceLimitOrder(LimitOrder{
		ID: "buy1", FromAccount: "a", ToAccount: "b",
		FromCurrency: "USD", ToCurrency: "EUR",
		Side: "BUY", LimitNum: 1, LimitDen: 1, AmountCents: 1,
	}); err != nil {
		t.Fatalf("uppercase BUY: %v", err)
	}
	if o, _ := l3.LimitOrder("buy1"); o.Side != LimitOrderBuy {
		t.Errorf("side = %q, want %q", o.Side, LimitOrderBuy)
	}
	if err := l3.PlaceLimitOrder(LimitOrder{
		ID: "sell1", FromAccount: "a", ToAccount: "b",
		FromCurrency: "USD", ToCurrency: "EUR",
		LimitNum: 1, LimitDen: 1, AmountCents: 1,
	}); err != nil {
		t.Fatalf("empty side: %v", err)
	}
	if o, _ := l3.LimitOrder("sell1"); o.Side != LimitOrderSell {
		t.Errorf("side = %q, want %q", o.Side, LimitOrderSell)
	}
}

func TestLimitOrderSellTouchFills(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // 1 USD = 1.08 EUR (mid-only: bid = ask = mid)

	placeSellOrder(t, l, "lo-1", 10000, 108, 100)
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 1 || res.Expires != 0 {
		t.Fatalf("sweep = %+v, want {Fills:1 Expires:0}", res)
	}
	o, ok := l.LimitOrder("lo-1")
	if !ok {
		t.Fatal("order vanished from the book")
	}
	if o.State != LimitOrderFilled || o.FilledCents != 10000 || o.FillCount != 1 {
		t.Errorf("order = %+v, want filled/10000/1 fill", o)
	}
	if o.LastError != "" {
		t.Errorf("LastError = %q, want empty", o.LastError)
	}

	// The fill is an ordinary FX transfer: same legs as a market order.
	entry, ok := l.GetByIdempotencyKey("lo-1/fill/1")
	if !ok {
		t.Fatal("fill transfer not idempotent-indexed under lo-1/fill/1")
	}
	if entry.DebitAccount != "payee" || entry.AmountCents != 10800 || entry.Currency != "EUR" {
		t.Errorf("fill principal = %+v, want payee debit 10800 EUR", entry)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 10800 {
		t.Errorf("payee EUR = %d, want 10800", got)
	}
	if got := l.BalanceIn("payer", "USD"); got != 90000 {
		t.Errorf("payer USD = %d, want 90000", got)
	}
	if got := l.LimitOrderFills(); got != 1 {
		t.Errorf("LimitOrderFills = %d, want 1", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken by limit fill: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("chain broken by limit fill: %v", err)
	}

	// A filled order never fills twice: the next sweep is a no-op.
	res = l.MatchLimitOrders(time.Now())
	if res.Fills != 0 {
		t.Errorf("second sweep fills = %d, want 0", res.Fills)
	}
}

func TestLimitOrderNoTouchNoFill(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	// Sell at 120: the market only bids 108 — untouched.
	placeSellOrder(t, l, "lo-nt", 10000, 120, 100)
	v0 := l.Version()
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 0 || res.Expires != 0 {
		t.Fatalf("sweep = %+v, want zero", res)
	}
	o, _ := l.LimitOrder("lo-nt")
	if o.State != LimitOrderPending || o.FilledCents != 0 {
		t.Errorf("order = %+v, want pending/0 filled", o)
	}
	if l.Version() != v0 {
		t.Errorf("no-touch sweep bumped version: %d -> %d", v0, l.Version())
	}
}

func TestLimitOrderPartialFillsAccumulate(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 300) // only 300 of the 1000 order is funded
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-part", 1000, 108, 100)
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 1 {
		t.Fatalf("first sweep fills = %d, want 1", res.Fills)
	}
	o, _ := l.LimitOrder("lo-part")
	if o.State != LimitOrderPending {
		t.Errorf("state = %q, want pending after partial fill", o.State)
	}
	if o.FilledCents != 300 || o.RemainingCents() != 700 || o.FillCount != 1 {
		t.Errorf("after partial fill: filled=%d remaining=%d fills=%d; want 300/700/1",
			o.FilledCents, o.RemainingCents(), o.FillCount)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 324 { // floor(300 * 108/100)
		t.Errorf("payee EUR = %d, want 324", got)
	}

	// Fund the rest: the next sweep fills the remainder and closes the order.
	fundPayer(t, l, "seed2", 700)
	res = l.MatchLimitOrders(time.Now())
	if res.Fills != 1 {
		t.Fatalf("second sweep fills = %d, want 1", res.Fills)
	}
	o, _ = l.LimitOrder("lo-part")
	if o.State != LimitOrderFilled || o.FilledCents != 1000 || o.FillCount != 2 {
		t.Errorf("order = %+v, want filled/1000/2 fills", o)
	}
	// The two fills have distinct idempotency keys: no double booking.
	for _, key := range []string{"lo-part/fill/1", "lo-part/fill/2"} {
		if _, ok := l.GetByIdempotencyKey(key); !ok {
			t.Errorf("fill %s not indexed", key)
		}
	}
	if got := l.LimitOrderFills(); got != 2 {
		t.Errorf("LimitOrderFills = %d, want 2", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken by partial fills: %v", err)
	}
}

func TestLimitOrderUnfundedTouchStaysPending(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	// The payer has no USD at all: the market touches, but nothing is funded.
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-unfunded", 1000, 108, 100)
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 0 {
		t.Fatalf("unfunded sweep fills = %d, want 0", res.Fills)
	}
	o, _ := l.LimitOrder("lo-unfunded")
	if o.State != LimitOrderPending || o.FilledCents != 0 {
		t.Errorf("order = %+v, want pending/0 filled", o)
	}
	if o.LastError == "" {
		t.Error("LastError must record why the touched order did not fill")
	}

	// Fund and fill: the order was patient, not dead.
	fundPayer(t, l, "seed", 1000)
	res = l.MatchLimitOrders(time.Now())
	if res.Fills != 1 {
		t.Fatalf("funded sweep fills = %d, want 1", res.Fills)
	}
	o, _ = l.LimitOrder("lo-unfunded")
	if o.State != LimitOrderFilled {
		t.Errorf("state = %q, want filled", o.State)
	}
}

func TestLimitOrderExpiryAutoCancel(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // the market touches the limit...

	now := time.Now()
	if err := l.PlaceLimitOrder(LimitOrder{
		ID: "lo-exp", FromAccount: "payer", ToAccount: "payee",
		FromCurrency: "USD", ToCurrency: "EUR",
		Side: LimitOrderSell, LimitNum: 108, LimitDen: 100,
		AmountCents: 10000, Expiry: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// ...but the sweep runs past expiry: expiry wins over the touch.
	res := l.MatchLimitOrders(now.Add(2 * time.Hour))
	if res.Expires != 1 || res.Fills != 0 {
		t.Fatalf("sweep = %+v, want {Fills:0 Expires:1}", res)
	}
	o, _ := l.LimitOrder("lo-exp")
	if o.State != LimitOrderExpired || o.FilledCents != 0 {
		t.Errorf("order = %+v, want expired/0 filled", o)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 0 {
		t.Errorf("expired order filled anyway: payee EUR = %d", got)
	}
	if got := l.LimitOrderCancels(); got != 1 {
		t.Errorf("LimitOrderCancels = %d, want 1", got)
	}
}

func TestLimitOrderZeroExpiryNeverExpires(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-immortal", 1000, 108, 100)
	// A sweep in the far future must still fill: no expiry configured.
	res := l.MatchLimitOrders(time.Now().Add(100 * 365 * 24 * time.Hour))
	if res.Fills != 1 {
		t.Fatalf("far-future sweep fills = %d, want 1", res.Fills)
	}
}

func TestLimitOrderCancelOnlyPending(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-cancel", 1000, 120, 100) // untouched: stays pending
	if err := l.CancelLimitOrder("lo-cancel"); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	o, _ := l.LimitOrder("lo-cancel")
	if o.State != LimitOrderCancelled {
		t.Errorf("state = %q, want cancelled", o.State)
	}
	if err := l.CancelLimitOrder("lo-cancel"); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancel cancelled: want ErrLimitOrderNotPending, got %v", err)
	}
	if err := l.CancelLimitOrder("nope"); !errors.Is(err, ErrLimitOrderNotFound) {
		t.Errorf("cancel unknown: want ErrLimitOrderNotFound, got %v", err)
	}
	// A cancelled order never fills, even when the market touches.
	if err := l.SetFXRate("USD", "EUR", 120, 100); err != nil {
		t.Fatal(err)
	}
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 0 {
		t.Errorf("cancelled order filled: %+v", res)
	}
	if got := l.LimitOrderCancels(); got != 1 {
		t.Errorf("LimitOrderCancels = %d, want 1", got)
	}

	// Filled and expired orders reject cancel too.
	placeSellOrder(t, l, "lo-filled", 1000, 120, 100)
	l.MatchLimitOrders(time.Now())
	if err := l.CancelLimitOrder("lo-filled"); !errors.Is(err, ErrLimitOrderNotPending) {
		t.Errorf("cancel filled: want ErrLimitOrderNotPending, got %v", err)
	}
}

func TestLimitOrderBuySideSemantics(t *testing.T) {
	newTwoSided := func(t *testing.T) *Ledger {
		t.Helper()
		// Two-sided rates book a spread leg, which needs the FX income
		// account (the fail-closed ErrFXIncomeAccountNotConfigured rule).
		l := New(WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-income"))
		fundPayer(t, l, "seed", 100000)
		mustSetRate(t, l, "USD", "EUR", 108, 100) // mid 1.08
		if err := l.SetFXBidAskRate("USD", "EUR", 104, 100, 112, 100); err != nil {
			t.Fatalf("SetFXBidAskRate: %v", err)
		}
		return l
	}
	placeBuy := func(t *testing.T, l *Ledger, id string, amount int64, limitNum, limitDen int64) {
		t.Helper()
		if err := l.PlaceLimitOrder(LimitOrder{
			ID: id, FromAccount: "payer", ToAccount: "payee",
			FromCurrency: "USD", ToCurrency: "EUR",
			Side: LimitOrderBuy, LimitNum: limitNum, LimitDen: limitDen,
			AmountCents: amount,
		}); err != nil {
			t.Fatalf("PlaceLimitOrder(%s): %v", id, err)
		}
	}

	// A buy triggers on the ASK at or below the limit: ask 1.12 vs limit 1.10 — no touch,
	// even though the mid (1.08) is inside the limit. The touch test uses the dealing
	// price, never the mid.
	l := newTwoSided(t)
	placeBuy(t, l, "buy-1", 10000, 110, 100)
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 0 {
		t.Fatalf("buy with ask above limit filled: %+v", res)
	}
	// The market moves the ask down to the limit: now it touches and settles at the ask.
	if err := l.SetFXBidAskRate("USD", "EUR", 100, 100, 110, 100); err != nil {
		t.Fatal(err)
	}
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("buy at the ask did not fill: %+v", res)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 11000 { // settles at ask: 10000 * 110/100
		t.Errorf("payee EUR = %d, want 11000 (ask settlement)", got)
	}

	// A sell triggers on the BID at or above the limit and settles at the bid.
	l2 := newTwoSided(t)
	placeSellOrder(t, l2, "sell-1", 10000, 104, 100) // bid 1.04 >= limit 1.04
	if res := l2.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sell at the bid did not fill: %+v", res)
	}
	if got := l2.BalanceIn("payee", "EUR"); got != 10400 { // settles at bid: 10000 * 104/100
		t.Errorf("payee EUR = %d, want 10400 (bid settlement)", got)
	}

	// A sell whose limit sits between bid and mid does not trigger on the mid.
	l3 := newTwoSided(t)
	placeSellOrder(t, l3, "sell-2", 10000, 106, 100) // bid 1.04 < limit 1.06
	if res := l3.MatchLimitOrders(time.Now()); res.Fills != 0 {
		t.Fatalf("sell with bid below limit filled on the mid: %+v", res)
	}
}

func TestLimitOrderSweepIsInsertionOrdered(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 1000) // funds exactly one of the two 1000-cent orders
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-first", 1000, 108, 100)
	placeSellOrder(t, l, "lo-second", 1000, 108, 100)
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 1 {
		t.Fatalf("sweep fills = %d, want 1", res.Fills)
	}
	first, _ := l.LimitOrder("lo-first")
	second, _ := l.LimitOrder("lo-second")
	if first.State != LimitOrderFilled || second.State != LimitOrderPending {
		t.Errorf("insertion order not respected: first=%s second=%s", first.State, second.State)
	}
	orders := l.LimitOrders()
	if len(orders) != 2 || orders[0].ID != "lo-first" || orders[1].ID != "lo-second" {
		t.Errorf("LimitOrders not in placement order: %+v", orders)
	}
}

func TestLimitOrderSnapshotRoundTrip(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	placeSellOrder(t, l, "lo-rt-fill", 1000, 108, 100) // will fill
	placeSellOrder(t, l, "lo-rt-rest", 1000, 120, 100) // rests pending
	placeSellOrder(t, l, "lo-rt-cancel", 1000, 120, 100)
	if err := l.CancelLimitOrder("lo-rt-cancel"); err != nil {
		t.Fatal(err)
	}
	l.MatchLimitOrders(time.Now())

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger differs: the limit-order book must round-trip")
	}
	// The restored book is live: the pending order still fills.
	if res := restored.MatchLimitOrders(time.Now().Add(time.Hour)); res.Fills != 0 {
		t.Errorf("restored pending order with limit 120 filled at 108: %+v", res)
	}
	if err := restored.SetFXRate("USD", "EUR", 120, 100); err != nil {
		t.Fatal(err)
	}
	if res := restored.MatchLimitOrders(time.Now().Add(time.Hour)); res.Fills != 1 {
		t.Errorf("restored pending order did not fill after the market moved: %+v", res)
	}
}

func TestLimitOrderMissingRateStaysPending(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	fundPayer(t, l, "seed", 100000)
	// No rate for USD->EUR: the sweep must not fail, the order waits.
	placeSellOrder(t, l, "lo-norate", 1000, 108, 100)
	res := l.MatchLimitOrders(time.Now())
	if res.Fills != 0 || res.Expires != 0 {
		t.Fatalf("sweep = %+v, want zero", res)
	}
	o, _ := l.LimitOrder("lo-norate")
	if o.State != LimitOrderPending || o.LastError == "" {
		t.Errorf("order = %+v, want pending with LastError", o)
	}
	// Installing the rate later unblocks it.
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if res := l.MatchLimitOrders(time.Now()); res.Fills != 1 {
		t.Fatalf("sweep after SetFXRate = %+v, want 1 fill", res)
	}
}
