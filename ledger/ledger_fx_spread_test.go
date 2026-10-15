package ledger

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// Two-sided FX dealing prices and spread P&L (LG-93): bid/ask quotes,
// side-correct pricing, the "<transfer ID>/spread" double-entry leg,
// fail-closed income-account handling, receipt/audit disclosure, and the
// Reconcile spread-P&L section.

func mustSetBidAsk(t *testing.T, l *Ledger, from, to string, bidNum, bidDen, askNum, askDen int64) {
	t.Helper()
	if err := l.SetFXBidAskRate(from, to, bidNum, bidDen, askNum, askDen); err != nil {
		t.Fatalf("SetFXBidAskRate(%s,%s,%d/%d,%d/%d): %v", from, to, bidNum, bidDen, askNum, askDen, err)
	}
}

// spreadTestLedger returns a ledger with USD->EUR mid 110/100, bid
// 108/100, ask 112/100, an FX clearing account and an FX income
// account, and a funded payer.
func spreadTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-inc"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 110, 100)
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
	return l
}

func TestSetFXBidAskRateValidation(t *testing.T) {
	l := New()
	for _, tc := range []struct {
		name           string
		from, to       string
		bidNum, bidDen int64
		askNum, askDen int64
	}{
		{"bid exceeds ask", "USD", "EUR", 112, 100, 108, 100},
		{"zero bid num", "USD", "EUR", 0, 100, 112, 100},
		{"zero bid den", "USD", "EUR", 108, 0, 112, 100},
		{"zero ask num", "USD", "EUR", 108, 100, 0, 100},
		{"negative ask den", "USD", "EUR", 108, 100, 112, -100},
		{"same currency", "USD", "USD", 108, 100, 112, 100},
		{"bad from code", "us", "EUR", 108, 100, 112, 100},
		{"empty to", "USD", "", 108, 100, 112, 100},
	} {
		if err := l.SetFXBidAskRate(tc.from, tc.to, tc.bidNum, tc.bidDen, tc.askNum, tc.askDen); !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("%s: want ErrInvalidFXRate, got %v", tc.name, err)
		}
	}
	if _, ok := l.FXRate("USD", "EUR"); ok {
		t.Error("rejected two-sided rate must not be installed")
	}
}

func TestSetFXBidAskRateBracketsMid(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 110, 100)

	// A spread that does not bracket the mid is rejected: the desk must
	// not buy above (or sell below) its own reference price.
	for _, tc := range []struct {
		name           string
		bidNum, bidDen int64
		askNum, askDen int64
	}{
		{"bid above mid", 111, 100, 113, 100},
		{"ask below mid", 107, 100, 109, 100},
	} {
		if err := l.SetFXBidAskRate("USD", "EUR", tc.bidNum, tc.bidDen, tc.askNum, tc.askDen); !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("%s: want ErrInvalidFXRate, got %v", tc.name, err)
		}
	}
	// The rejected spreads left the rate mid-only.
	if r, _ := l.FXRate("USD", "EUR"); r.BidNum != 0 || r.AskNum != 0 {
		t.Errorf("rate after rejections: %+v", r)
	}
	// A zero-width spread exactly at the mid is a degenerate but valid
	// quote (bid = ask = mid: no spread to earn).
	if err := l.SetFXBidAskRate("USD", "EUR", 110, 100, 110, 100); err != nil {
		t.Errorf("zero-width spread at mid: unexpected %v", err)
	}

	// A bracketing spread installs bid/ask and keeps the mid.
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
	r, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("two-sided rate missing")
	}
	if r.Num != 110 || r.Den != 100 || r.BidNum != 108 || r.BidDen != 100 || r.AskNum != 112 || r.AskDen != 100 {
		t.Errorf("bad two-sided rate: %+v", r)
	}
}

func TestSetFXBidAskRateDerivesMid(t *testing.T) {
	l := New()
	// No pre-existing rate: the mid is the exact midpoint of the quote.
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
	r, _ := l.FXRate("USD", "EUR")
	// (108/100 + 112/100)/2 = 11/10 exactly (big.Rat reduces).
	if r.Num != 11 || r.Den != 10 {
		t.Errorf("derived mid = %d/%d, want 11/10", r.Num, r.Den)
	}
	// Odd midpoints stay exact: (1/3 + 1/2)/2 = 5/12.
	l2 := New()
	mustSetBidAsk(t, l2, "USD", "JPY", 1, 3, 1, 2)
	r2, _ := l2.FXRate("USD", "JPY")
	if r2.Num != 5 || r2.Den != 12 {
		t.Errorf("derived mid = %d/%d, want 5/12", r2.Num, r2.Den)
	}
}

func TestTwoSidedFromMidSpread(t *testing.T) {
	// 100 bps full spread on an 11/10 mid: bid = 11/10·(1−50/10000),
	// ask = 11/10·(1+50/10000) — exact rationals, no float.
	bn, bd, an, ad, err := TwoSidedFromMidSpread(11, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	// big.Rat reduces: 10945/10000 -> 2189/2000, 11055/10000 -> 2211/2000.
	if bn != 2189 || bd != 2000 || an != 2211 || ad != 2000 {
		t.Errorf("got bid %d/%d ask %d/%d, want 2189/2000 and 2211/2000", bn, bd, an, ad)
	}
	// The derived quote is symmetric: ask − bid is exactly mid·s/10000.
	if got := fxSpreadBps(bn, bd, an, ad, 11, 10); got != 100 {
		t.Errorf("spread bps = %d, want 100", got)
	}
	// Zero spread collapses to the mid.
	bn, bd, an, ad, err = TwoSidedFromMidSpread(11, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bn != 11 || bd != 10 || an != 11 || ad != 10 {
		t.Errorf("zero spread: got %d/%d %d/%d, want 11/10 11/10", bn, bd, an, ad)
	}
	for _, s := range []int64{-1, 20000, 30000} {
		if _, _, _, _, err := TwoSidedFromMidSpread(11, 10, s); !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("spread %d: want ErrInvalidFXRate, got %v", s, err)
		}
	}
	if _, _, _, _, err := TwoSidedFromMidSpread(0, 10, 100); !errors.Is(err, ErrInvalidFXRate) {
		t.Errorf("zero mid: want ErrInvalidFXRate, got %v", err)
	}
	// The derived quote installs cleanly and brackets by construction.
	l := New()
	mustSetRate(t, l, "USD", "EUR", 11, 10)
	bn, bd, an, ad, err = TwoSidedFromMidSpread(11, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetFXBidAskRate("USD", "EUR", bn, bd, an, ad); err != nil {
		t.Errorf("derived quote rejected: %v", err)
	}
}

func TestFXSellPricesAtBid(t *testing.T) {
	l := spreadTestLedger(t)
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxs1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", // FXSide "" defaults to sell
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FX == nil {
		t.Fatal("receipt must disclose FX conversion")
	}
	fx := rcpt.FX
	// Sell prices at bid: floor(10000·108/100) = 10800.
	if fx.ConvertedCents != 10800 || fx.RateNum != 108 || fx.RateDen != 100 {
		t.Errorf("sell conversion = %d @ %d/%d, want 10800 @ 108/100", fx.ConvertedCents, fx.RateNum, fx.RateDen)
	}
	if fx.Direction != "sell" {
		t.Errorf("direction = %q, want sell", fx.Direction)
	}
	// Spread: floor(10000·110/100) − floor(10000·108/100) = 200.
	if fx.SpreadCents != 200 {
		t.Errorf("spread = %d, want 200", fx.SpreadCents)
	}
	// Spread bps: floor((112/100−108/100)/(110/100)·10000) = floor(363.63…) = 363.
	if fx.SpreadBps != 363 {
		t.Errorf("spread bps = %d, want 363", fx.SpreadBps)
	}
	if fx.BidNum != 108 || fx.BidDen != 100 || fx.AskNum != 112 || fx.AskDen != 100 ||
		fx.MidNum != 110 || fx.MidDen != 100 {
		t.Errorf("bad quote disclosure: %+v", fx)
	}
	if fx.FXIncomeAccount != "fx-inc" {
		t.Errorf("income account = %q, want fx-inc", fx.FXIncomeAccount)
	}

	// Three legs: principal, /fx, /spread — each single-currency.
	if len(rcpt.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(rcpt.Entries))
	}
	spread := rcpt.Entries[2]
	if spread.ID != "fxs1/spread" || spread.DebitAccount != "fx-pnl" || spread.CreditAccount != "fx-inc" ||
		spread.AmountCents != 200 || spread.Currency != "EUR" {
		t.Errorf("bad spread leg: %+v", spread)
	}

	// Balances: the desk's 200 EUR spread lands in the income account,
	// not hidden in the clearing account.
	if got := l.BalanceIn("fx-inc", "EUR"); got != -200 {
		t.Errorf("fx-inc EUR = %d, want -200 (credit balance)", got)
	}
	if got := l.BalanceIn("fx-pnl", "EUR"); got != -(10800 - 200) {
		t.Errorf("fx-pnl EUR = %d, want -10600", got)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 10800 {
		t.Errorf("payee EUR = %d, want 10800", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken by spread leg: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("audit chain broken: %v", err)
	}
	// Operational counters.
	if n := l.FXSpreadBookingCount(); n != 1 {
		t.Errorf("spread bookings = %d, want 1", n)
	}
	if c := l.FXSpreadCentsTotal(); c != 200 {
		t.Errorf("spread cents total = %d, want 200", c)
	}
}

func TestFXBuyPricesAtAsk(t *testing.T) {
	l := spreadTestLedger(t)
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxb1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXSide: "buy",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	fx := rcpt.FX
	// Buy prices at ask: floor(10000·112/100) = 11200.
	if fx.ConvertedCents != 11200 || fx.RateNum != 112 || fx.RateDen != 100 {
		t.Errorf("buy conversion = %d @ %d/%d, want 11200 @ 112/100", fx.ConvertedCents, fx.RateNum, fx.RateDen)
	}
	if fx.Direction != "buy" {
		t.Errorf("direction = %q, want buy", fx.Direction)
	}
	// Spread: floor(10000·112/100) − floor(10000·110/100) = 200.
	if fx.SpreadCents != 200 {
		t.Errorf("spread = %d, want 200", fx.SpreadCents)
	}
	if got := l.BalanceIn("fx-inc", "EUR"); got != -200 {
		t.Errorf("fx-inc EUR = %d, want -200 (credit balance)", got)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 11200 {
		t.Errorf("payee EUR = %d, want 11200", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken: %v", err)
	}
}

func TestFXSideValidation(t *testing.T) {
	l := spreadTestLedger(t)
	if _, err := l.PostTransfer(Transfer{
		ID: "fxv1", From: "payer", To: "payee", AmountCents: 100,
		Currency: "USD", ToCurrency: "EUR", FXSide: "sideways",
	}); !errors.Is(err, ErrInvalidFXSide) {
		t.Errorf("garbage fx_side: want ErrInvalidFXSide, got %v", err)
	}
	// Case-insensitive: "BUY" prices at ask.
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxv2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXSide: "BUY",
	})
	if err != nil {
		t.Fatalf("PostTransfer BUY: %v", err)
	}
	if rcpt.FX.Direction != "buy" || rcpt.FX.ConvertedCents != 11200 {
		t.Errorf("BUY side: %+v", rcpt.FX)
	}
	// Same-currency transfers ignore the side entirely.
	if _, err := l.PostTransfer(Transfer{
		ID: "same1", From: "payer", To: "payee", AmountCents: 100, Currency: "USD", FXSide: "buy",
	}); err != nil {
		t.Errorf("same-currency with fx_side: %v", err)
	}
}

func TestFXSpreadFailClosed(t *testing.T) {
	newLedger := func() *Ledger {
		l := New(WithFXAccount("fx-pnl"))
		if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
			t.Fatal(err)
		}
		mustSetRate(t, l, "USD", "EUR", 110, 100)
		mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)
		return l
	}
	post := func(l *Ledger, id string) (TransferReceipt, error) {
		return l.PostTransfer(Transfer{
			ID: id, From: "payer", To: "payee", AmountCents: 10000,
			Currency: "USD", ToCurrency: "EUR",
		})
	}

	// No income account anywhere: fail-closed, records nothing.
	l := newLedger()
	v := l.Version()
	if _, err := post(l, "fxf1"); !errors.Is(err, ErrFXIncomeAccountNotConfigured) {
		t.Errorf("want ErrFXIncomeAccountNotConfigured, got %v", err)
	}
	if l.Version() != v {
		t.Error("rejected transfer bumped the version")
	}
	if _, exists := l.entries["fxf1"]; exists {
		t.Error("rejected transfer journaled entries")
	}

	// Income account == FX account would book a self-leg: fail-closed.
	l = newLedger()
	l.fxIncomeAccount = "fx-pnl"
	if _, err := post(l, "fxf2"); !errors.Is(err, ErrFXIncomeAccountNotConfigured) {
		t.Errorf("income == fx account: want ErrFXIncomeAccountNotConfigured, got %v", err)
	}

	// The transfer-level override fixes it.
	l = newLedger()
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxf3", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXIncomeAccount: "desk-inc",
	})
	if err != nil {
		t.Fatalf("transfer-level income account: %v", err)
	}
	if rcpt.FX.FXIncomeAccount != "desk-inc" || rcpt.Entries[2].CreditAccount != "desk-inc" {
		t.Errorf("override not honored: %+v", rcpt.FX)
	}
}

func TestFXMidOnlyRateUnchanged(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-inc"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100) // mid-only: no bid/ask

	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxm1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// Converts at mid, exactly like before LG-93.
	if rcpt.FX.ConvertedCents != 10800 || rcpt.FX.RateNum != 108 {
		t.Errorf("mid-only conversion: %+v", rcpt.FX)
	}
	if len(rcpt.Entries) != 2 {
		t.Fatalf("mid-only transfer must book 2 legs, got %d", len(rcpt.Entries))
	}
	if rcpt.FX.SpreadCents != 0 || rcpt.FX.SpreadBps != 0 {
		t.Errorf("mid-only spread: %+v", rcpt.FX)
	}
	// bid = ask = mid in the disclosure.
	if rcpt.FX.BidNum != 108 || rcpt.FX.AskNum != 108 {
		t.Errorf("mid-only quote disclosure: %+v", rcpt.FX)
	}
	if rcpt.FX.FXIncomeAccount != "" {
		t.Errorf("no spread leg booked, income account must be empty: %q", rcpt.FX.FXIncomeAccount)
	}
	if n := l.FXSpreadBookingCount(); n != 0 {
		t.Errorf("spread bookings = %d, want 0", n)
	}
}

func TestFXSpreadDustBooksNoLeg(t *testing.T) {
	l := spreadTestLedger(t)
	// 1 cent inside a 363 bps spread: the floored spread is zero.
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fxd1", From: "payer", To: "payee", AmountCents: 1,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FX.SpreadCents != 0 {
		t.Errorf("dust spread = %d, want 0", rcpt.FX.SpreadCents)
	}
	if len(rcpt.Entries) != 2 {
		t.Errorf("dust spread must book no leg: %d entries", len(rcpt.Entries))
	}
}

func TestFXSpreadFrozenIncomeAccount(t *testing.T) {
	l := spreadTestLedger(t)
	l.Freeze("fx-inc")
	if _, err := l.PostTransfer(Transfer{
		ID: "fxz1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen income account: want ErrAccountFrozen, got %v", err)
	}
	l.Unfreeze("fx-inc")
	if _, _, err := l.CloseAccount("fx-inc", false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "fxz2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("closed income account: want ErrAccountClosed, got %v", err)
	}
}

func TestFXSpreadIdempotentReplay(t *testing.T) {
	l := spreadTestLedger(t)
	orig, err := l.PostTransfer(Transfer{
		ID: "fxr1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "sprk1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(orig.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(orig.Entries))
	}
	v := l.Version()
	replay, err := l.PostTransfer(Transfer{
		ID: "fxr2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "sprk1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate {
		t.Error("replay must be a duplicate")
	}
	if l.Version() != v {
		t.Error("replay bumped the version")
	}
	// The replay carries the spread leg's ID — the journaled row is the
	// authoritative record.
	if len(replay.Entries) != 3 || replay.Entries[2].ID != "fxr1/spread" {
		t.Errorf("replay entries: %v", replay.Entries)
	}
	if replay.FX != nil {
		t.Error("replay must omit the FX disclosure")
	}
	// Balances unchanged by the replay.
	if got := l.BalanceIn("fx-inc", "EUR"); got != -200 {
		t.Errorf("fx-inc EUR = %d, want -200", got)
	}
}

func TestFXSpreadConservationAcrossSides(t *testing.T) {
	l := spreadTestLedger(t)
	sells := []string{"c1", "c2"}
	for _, id := range sells {
		if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.PostTransfer(Transfer{ID: "c3", From: "payer", To: "payee", AmountCents: 5000, Currency: "USD", ToCurrency: "EUR", FXSide: "buy"}); err != nil {
		t.Fatal(err)
	}
	// 200 + 200 + 100 (buy side: floor(5000·112/100) − floor(5000·110/100)),
	// as a credit balance.
	if got := l.BalanceIn("fx-inc", "EUR"); got != -500 {
		t.Errorf("fx-inc EUR = %d, want -500", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("audit chain broken: %v", err)
	}
	if n := l.FXSpreadBookingCount(); n != 3 {
		t.Errorf("spread bookings = %d, want 3", n)
	}
	if c := l.FXSpreadCentsTotal(); c != 500 {
		t.Errorf("spread cents total = %d, want 500", c)
	}
}

func TestFXSpreadReconcilePNL(t *testing.T) {
	l := spreadTestLedger(t)
	mustSetRate(t, l, "USD", "JPY", 150, 1)
	bn, bd, an, ad, err := TwoSidedFromMidSpread(150, 1, 200) // 200 bps on 150
	if err != nil {
		t.Fatal(err)
	}
	mustSetBidAsk(t, l, "USD", "JPY", bn, bd, an, ad)
	if _, _, err := l.Post(JournalEntry{ID: "seed2", DebitAccount: "payer2", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "p1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "p2", From: "payer2", To: "payee2", AmountCents: 10000, Currency: "USD", ToCurrency: "JPY", FXSide: "buy"}); err != nil {
		t.Fatal(err)
	}

	rep := l.Reconcile(time.Now())
	if len(rep.FXSpreadPNL) != 2 {
		t.Fatalf("FXSpreadPNL rows = %d, want 2: %+v", len(rep.FXSpreadPNL), rep.FXSpreadPNL)
	}
	// Sorted by currency: EUR before JPY.
	eur, jpy := rep.FXSpreadPNL[0], rep.FXSpreadPNL[1]
	if eur.Currency != "EUR" || jpy.Currency != "JPY" {
		t.Fatalf("rows not sorted by currency: %+v", rep.FXSpreadPNL)
	}
	if eur.SpreadBookings != 1 || eur.SpreadCentsTotal != 200 {
		t.Errorf("EUR row = %+v, want {1 200}", eur)
	}
	// Buy side JPY: floor(10000·ask) − floor(10000·150).
	askC, _ := fxConvertCents(10000, an, ad)
	midC, _ := fxConvertCents(10000, 150, 1)
	if jpy.SpreadBookings != 1 || jpy.SpreadCentsTotal != askC-midC {
		t.Errorf("JPY row = %+v, want {1 %d}", jpy, askC-midC)
	}
	if !rep.AccountingEquationOK {
		t.Errorf("accounting equation not OK: %s", rep.AccountingError)
	}
}

func TestFXSpreadReversalNetsPNL(t *testing.T) {
	l := spreadTestLedger(t)
	if _, err := l.PostTransfer(Transfer{ID: "v1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ReverseEntry("v1/spread", "rev1"); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.FXSpreadPNL) != 1 {
		t.Fatalf("FXSpreadPNL rows = %d, want 1", len(rep.FXSpreadPNL))
	}
	// The direct reversal un-books the P&L: net zero, still disclosed.
	if got := rep.FXSpreadPNL[0].SpreadCentsTotal; got != 0 {
		t.Errorf("netted spread = %d, want 0", got)
	}
}

func TestFXBidAskSnapshotRoundTrip(t *testing.T) {
	l := spreadTestLedger(t)
	if _, err := l.PostTransfer(Transfer{ID: "s1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs from original")
	}
	// The restored ledger deals at the restored bid/ask, with the
	// restored income account.
	r, ok := l2.FXRate("USD", "EUR")
	if !ok || r.BidNum != 108 || r.AskNum != 112 || r.Num != 110 {
		t.Errorf("restored two-sided rate: %+v, %v", r, ok)
	}
	rcpt, err := l2.PostTransfer(Transfer{ID: "s2", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"})
	if err != nil {
		t.Fatalf("FX on restored ledger: %v", err)
	}
	if rcpt.FX.SpreadCents != 200 || rcpt.FX.FXIncomeAccount != "fx-inc" {
		t.Errorf("restored spread: %+v", rcpt.FX)
	}
}

func TestFXBidAskAdmissionQuoteIsSideCorrect(t *testing.T) {
	l := spreadTestLedger(t)
	sell := l.admitFXRateQuote(Transfer{Currency: "USD", ToCurrency: "EUR"})
	if sell.Num != 108 || sell.Den != 100 {
		t.Errorf("sell quote = %d/%d, want bid 108/100", sell.Num, sell.Den)
	}
	buy := l.admitFXRateQuote(Transfer{Currency: "USD", ToCurrency: "EUR", FXSide: "buy"})
	if buy.Num != 112 || buy.Den != 100 {
		t.Errorf("buy quote = %d/%d, want ask 112/100", buy.Num, buy.Den)
	}
	// The quote pins the full two-sided picture.
	if sell.BidNum != 108 || sell.AskNum != 112 || sell.MidNum != 110 {
		t.Errorf("sell quote dealing prices: %+v", sell)
	}
}

func TestFXSpreadSlippageGateOnSidePrice(t *testing.T) {
	l := spreadTestLedger(t)
	// Admit once at the current ask (112), then move only the ask (112
	// -> 120) between admission and commit: the slippage gate measures
	// drift on the side price, not the mid.
	// Drift: |120/100 − 112/100| / (112/100) = 8/112 = 714 bps (floor).
	tr := Transfer{ID: "g1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXSide: "buy", MaxSlippageBps: 100}
	quote := l.admitFXRateQuote(tr)
	if quote.Num != 112 || quote.Den != 100 {
		t.Fatalf("admitted ask = %d/%d, want 112/100", quote.Num, quote.Den)
	}
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 120, 100) // the concurrent move
	l.mu.Lock()
	_, err := l.postTransferLocked(tr, quote)
	l.mu.Unlock()
	if !errors.Is(err, ErrFXSlippageExceeded) {
		t.Errorf("want ErrFXSlippageExceeded, got %v", err)
	}

	// A generous cap settles at the ADMITTED ask (112), with the spread
	// measured against the admitted mid (110) — the rate move after
	// admission changes disclosure, never the booked legs. The
	// rejection above recorded nothing, so the same quote is reusable.
	tr2 := Transfer{ID: "g2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", FXSide: "buy", MaxSlippageBps: 10000}
	l.mu.Lock()
	rcpt, err := l.postTransferLocked(tr2, quote)
	l.mu.Unlock()
	if err != nil {
		t.Fatalf("postTransferLocked: %v", err)
	}
	if rcpt.FX.ConvertedCents != 11200 || rcpt.FX.AdmittedRateNum != 112 {
		t.Errorf("must settle at admitted ask: %+v", rcpt.FX)
	}
	if rcpt.FX.SpreadCents != 200 { // floor(10000·112/100) − floor(10000·110/100)
		t.Errorf("admitted spread = %d, want 200", rcpt.FX.SpreadCents)
	}
	if rcpt.FX.SlippageBps != 714 {
		t.Errorf("slippage = %d, want 714", rcpt.FX.SlippageBps)
	}
}

func TestFXSpreadOverrideCollapsesSpread(t *testing.T) {
	l := spreadTestLedger(t)
	// An approved manual override is a single price: bid = ask = mid =
	// the override, so no spread leg books.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(115, 100), time.Hour, "test", "op1"); err != nil {
		t.Fatal(err)
	}
	if err := l.ApproveRateOverride("USD", "EUR", "op2"); err != nil {
		t.Fatal(err)
	}
	rcpt, err := l.PostTransfer(Transfer{
		ID: "o1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FX.ConvertedCents != 11500 || !rcpt.FX.OverrideApplied {
		t.Errorf("override conversion: %+v", rcpt.FX)
	}
	if rcpt.FX.SpreadCents != 0 || len(rcpt.Entries) != 2 {
		t.Errorf("override must book no spread leg: %+v (%d entries)", rcpt.FX, len(rcpt.Entries))
	}
}

func TestFXSpreadSplitTransfer(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"), WithFXIncomeAccount("fx-inc"), WithMaxTransferChunkCents(4000))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 110, 100)
	mustSetBidAsk(t, l, "USD", "EUR", 108, 100, 112, 100)

	rcpt, err := l.PostTransferSplit(Transfer{
		ID: "sp1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if rcpt.FX == nil || rcpt.FX.Direction != "sell" {
		t.Fatalf("split FX disclosure: %+v", rcpt.FX)
	}
	// Chunks of 4000/4000/2000 at bid: 4320 + 4320 + 2160 = 10800.
	if rcpt.FX.ConvertedCents != 10800 {
		t.Errorf("split converted = %d, want 10800", rcpt.FX.ConvertedCents)
	}
	// Per-chunk spread: 2×(4400−4320) + (2200−2160) = 200.
	if rcpt.FX.SpreadCents != 200 {
		t.Errorf("split spread = %d, want 200", rcpt.FX.SpreadCents)
	}
	// The aggregated "<id>/spread" leg is journaled.
	found := false
	for _, e := range rcpt.Entries {
		if e.ID == "sp1/spread" {
			found = true
			if e.DebitAccount != "fx-pnl" || e.CreditAccount != "fx-inc" ||
				e.AmountCents != 200 || e.Currency != "EUR" {
				t.Errorf("bad split spread leg: %+v", e)
			}
		}
	}
	if !found {
		t.Error("split spread leg missing from entries")
	}
	if got := l.BalanceIn("fx-inc", "EUR"); got != -200 {
		t.Errorf("fx-inc EUR = %d, want -200", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.FXSpreadPNL) != 1 || rep.FXSpreadPNL[0].SpreadCentsTotal != 200 {
		t.Errorf("reconcile P&L: %+v", rep.FXSpreadPNL)
	}
}

func TestFXSpreadDryRun(t *testing.T) {
	l := spreadTestLedger(t)
	// A dry run simulates the spread leg (would-book) and records nothing.
	res, err := l.DryRunTransfer(Transfer{
		ID: "dry1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("DryRunTransfer: %v", err)
	}
	if !res.WouldSucceed {
		t.Error("dry run should succeed")
	}
	if n := l.FXSpreadBookingCount(); n != 0 {
		t.Errorf("dry run booked spread: count = %d", n)
	}
	// Fail-closed surfaces on the dry run too.
	l2 := New(WithFXAccount("fx-pnl"))
	if _, _, err := l2.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l2, "USD", "EUR", 110, 100)
	mustSetBidAsk(t, l2, "USD", "EUR", 108, 100, 112, 100)
	if _, err := l2.DryRunTransfer(Transfer{
		ID: "dry2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); !errors.Is(err, ErrFXIncomeAccountNotConfigured) {
		t.Errorf("dry run without income account: want ErrFXIncomeAccountNotConfigured, got %v", err)
	}
}

func TestFXIncomeAccountSnapshotRoundTrip(t *testing.T) {
	l := spreadTestLedger(t)
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs (income account must round-trip)")
	}
}
