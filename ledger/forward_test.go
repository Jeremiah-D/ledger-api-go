package ledger

import (
	"bytes"
	"errors"
	"math/big"
	"testing"
	"time"
)

// forwardTestLedger builds a ledger with an FX clearing account, ready
// for forward settlement (the settlement legs need the configured
// counterparty).
func forwardTestLedger() *Ledger {
	return New(WithFXAccount("fx"))
}

// mustBookForwardLocked books through the lock-held core with a pinned
// booking time, so settle_at/expiry arithmetic stays deterministic.
func mustBookForwardLocked(t *testing.T, l *Ledger, b ForwardBooking, now time.Time) *FXForward {
	t.Helper()
	c, err := l.bookForwardLocked(b, now)
	if err != nil {
		t.Fatalf("bookForwardLocked: %v", err)
	}
	return c
}

// mustFundForwardPayer seeds the payer's source-currency balance.
func mustFundForwardPayer(t *testing.T, l *Ledger, cents int64, currency string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-payer-" + currency, DebitAccount: "payer",
		CreditAccount: "bank", AmountCents: cents, Currency: currency,
	}); err != nil {
		t.Fatalf("seed payer: %v", err)
	}
}

func forwardBooking(id string, settleAt time.Time) ForwardBooking {
	return ForwardBooking{
		ID:            id,
		From:          "payer",
		To:            "payee",
		FromCurrency:  "USD",
		ToCurrency:    "EUR",
		Rate:          big.NewRat(11, 10),
		NotionalCents: 10000,
		SettleAt:      settleAt,
	}
}

func TestForwardBookValidation(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	bad := []struct {
		name   string
		mutate func(*ForwardBooking)
		want   error
	}{
		{"empty ID", func(b *ForwardBooking) { b.ID = "" }, ErrForwardInvalid},
		{"bad from currency", func(b *ForwardBooking) { b.FromCurrency = "usd" }, ErrForwardInvalid},
		{"bad to currency", func(b *ForwardBooking) { b.ToCurrency = "EURO" }, ErrForwardInvalid},
		{"empty from currency", func(b *ForwardBooking) { b.FromCurrency = "" }, ErrForwardInvalid},
		{"empty to currency", func(b *ForwardBooking) { b.ToCurrency = "" }, ErrForwardInvalid},
		{"same currency", func(b *ForwardBooking) { b.ToCurrency = "USD" }, ErrForwardInvalid},
		{"empty from account", func(b *ForwardBooking) { b.From = "" }, ErrForwardInvalid},
		{"empty to account", func(b *ForwardBooking) { b.To = "" }, ErrForwardInvalid},
		{"same account", func(b *ForwardBooking) { b.To = "payer" }, ErrForwardInvalid},
		{"nil rate", func(b *ForwardBooking) { b.Rate = nil }, ErrForwardInvalid},
		{"zero rate", func(b *ForwardBooking) { b.Rate = big.NewRat(0, 1) }, ErrForwardInvalid},
		{"negative rate", func(b *ForwardBooking) { b.Rate = big.NewRat(-11, 10) }, ErrForwardInvalid},
		{"zero notional", func(b *ForwardBooking) { b.NotionalCents = 0 }, ErrForwardInvalid},
		{"negative notional", func(b *ForwardBooking) { b.NotionalCents = -1 }, ErrForwardInvalid},
		{"negative cap", func(b *ForwardBooking) { b.NotionalCapCents = -1 }, ErrForwardInvalid},
		{"notional over cap", func(b *ForwardBooking) { b.NotionalCapCents = 9999 }, ErrForwardCapExceeded},
		{"past settle_at", func(b *ForwardBooking) { b.SettleAt = now.Add(-time.Hour) }, ErrForwardInvalid},
		{"zero settle_at", func(b *ForwardBooking) { b.SettleAt = time.Time{} }, ErrForwardInvalid},
		{"settle_at is now", func(b *ForwardBooking) { b.SettleAt = now }, ErrForwardInvalid},
		{"bad fx side", func(b *ForwardBooking) { b.FXSide = "sideways" }, ErrForwardInvalid},
	}
	for i, tc := range bad {
		b := forwardBooking("fwd-bad", future)
		tc.mutate(&b)
		if _, err := l.bookForwardLocked(b, now); !errors.Is(err, tc.want) {
			t.Errorf("%s (case %d): want %v, got %v", tc.name, i, tc.want, err)
		}
	}
	// A rejected booking records nothing: the ID stays free.
	if _, ok := l.forwards["fwd-bad"]; ok {
		t.Error("rejected booking must not register the forward")
	}
}

func TestForwardBookIdempotentReplay(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	b := forwardBooking("fwd-replay", now.Add(time.Hour))
	first := mustBookForwardLocked(t, l, b, now)
	// Replay with different parameters returns the recorded contract.
	b2 := b
	b2.Rate = big.NewRat(99, 1)
	b2.NotionalCents = 1
	second, err := l.bookForwardLocked(b2, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.RateNum != first.RateNum || second.RateDen != first.RateDen ||
		second.NotionalCents != first.NotionalCents || !second.BookedAt.Equal(first.BookedAt) {
		t.Errorf("replay returned a different contract: %+v vs %+v", second, first)
	}
	if len(l.forwards) != 1 {
		t.Errorf("replay must not register a second contract, have %d", len(l.forwards))
	}
}

// TestForwardRateFrozenAcrossMarketMove is the core LG-110 guarantee:
// the contract locks the agreed rate, and a later SetFXRate cannot
// re-price it — settlement converts at the locked price even when the
// market moved.
func TestForwardRateFrozenAcrossMarketMove(t *testing.T) {
	l := forwardTestLedger()
	mustSetRate(t, l, "USD", "EUR", 108, 100) // market at booking: 1.08
	now := time.Now().UTC()
	bookAt := now
	c := mustBookForwardLocked(t, l, forwardBooking("fwd-freeze", now.Add(time.Hour)), bookAt)
	if c.RateSnapshotVersion != 1 {
		t.Errorf("RateSnapshotVersion = %d, want 1 (the table version at booking)", c.RateSnapshotVersion)
	}
	mustSetRate(t, l, "USD", "EUR", 20, 10) // market moves to 2.0
	mustFundForwardPayer(t, l, 100000, "USD")
	receipt, err := l.settleForwardLocked("fwd-freeze", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if receipt.FX == nil {
		t.Fatal("settlement receipt has no FX conversion")
	}
	fx := receipt.FX
	if fx.RateNum != 11 || fx.RateDen != 10 {
		t.Errorf("settled at %d/%d, want locked 11/10", fx.RateNum, fx.RateDen)
	}
	if want := int64(11000); fx.ConvertedCents != want { // floor(10000 * 11/10)
		t.Errorf("converted = %d, want %d (locked rate, not the 2.0 market)", fx.ConvertedCents, want)
	}
	if fx.RateSnapshotVersion != 1 {
		t.Errorf("receipt RateSnapshotVersion = %d, want 1 (booking-time pin)", fx.RateSnapshotVersion)
	}
	if got, ok := l.Forward("fwd-freeze"); !ok || got.Status != ForwardSettled {
		t.Errorf("contract status = %v, want settled", got)
	}
}

// TestForwardSlippageZero pins the backlog's disclosure contract:
// slippage_bps=0 with admitted == committed == locked rate, even when
// the live market price is far from the locked one.
func TestForwardSlippageZero(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-slip", now.Add(time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 500, 100) // market far from the 1.1 lock
	mustFundForwardPayer(t, l, 100000, "USD")
	receipt, err := l.settleForwardLocked("fwd-slip", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	fx := receipt.FX
	if fx == nil {
		t.Fatal("no FX conversion on receipt")
	}
	if fx.SlippageBps != 0 {
		t.Errorf("SlippageBps = %d, want 0 (by construction)", fx.SlippageBps)
	}
	if fx.AdmittedRateNum != 11 || fx.AdmittedRateDen != 10 {
		t.Errorf("admitted = %d/%d, want locked 11/10", fx.AdmittedRateNum, fx.AdmittedRateDen)
	}
	if fx.RateNum != 11 || fx.RateDen != 10 {
		t.Errorf("committed = %d/%d, want locked 11/10", fx.RateNum, fx.RateDen)
	}
}

// TestForwardSettlesWithoutConfiguredRate: the locked price is the
// bilateral agreement — settlement must not require a market rate for
// the pair at all.
func TestForwardSettlesWithoutConfiguredRate(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-norate", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	receipt, err := l.settleForwardLocked("fwd-norate", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle without a configured rate: %v", err)
	}
	if receipt.FX == nil || receipt.FX.ConvertedCents != 11000 {
		t.Errorf("unexpected conversion: %+v", receipt.FX)
	}
}

func TestForwardSettleBeforeSettleAt(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-early", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	entriesBefore := len(l.entries)
	if _, err := l.settleForwardLocked("fwd-early", now.Add(30*time.Minute)); !errors.Is(err, ErrForwardNotSettleable) {
		t.Errorf("want ErrForwardNotSettleable, got %v", err)
	}
	if len(l.entries) != entriesBefore {
		t.Error("rejected settlement must record nothing")
	}
	if got, _ := l.Forward("fwd-early"); got.Status != ForwardBooked {
		t.Errorf("contract status = %s, want booked", got.Status)
	}
}

func TestForwardExpirySweep(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-exp", now.Add(time.Hour)), now)
	lapseAt := now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second)
	if n := l.ExpireForwards(lapseAt); n != 1 {
		t.Fatalf("ExpireForwards = %d, want 1", n)
	}
	c, ok := l.Forward("fwd-exp")
	if !ok {
		t.Fatal("contract vanished")
	}
	if c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		t.Errorf("status = %s/%s, want cancelled/expired", c.Status, c.CancelReason)
	}
	// The sweep is idempotent.
	if n := l.ExpireForwards(lapseAt.Add(time.Hour)); n != 0 {
		t.Errorf("second sweep expired %d, want 0", n)
	}
	// A lapsed contract can never settle.
	if _, err := l.settleForwardLocked("fwd-exp", lapseAt); !errors.Is(err, ErrForwardExpired) {
		t.Errorf("want ErrForwardExpired, got %v", err)
	}
	// Expiry records no journal entries: the books are untouched.
	if len(l.entries) != 0 {
		t.Errorf("expiry recorded %d journal entries, want 0", len(l.entries))
	}
}

// TestForwardLazyExpiryOnSettle: without any explicit sweep, settling a
// lapsed contract reports expired (the lazy-expiry check runs first).
func TestForwardLazyExpiryOnSettle(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-lazy", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	lapseAt := now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second)
	if _, err := l.settleForwardLocked("fwd-lazy", lapseAt); !errors.Is(err, ErrForwardExpired) {
		t.Errorf("want ErrForwardExpired, got %v", err)
	}
	c, _ := l.Forward("fwd-lazy")
	if c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		t.Errorf("status = %s/%s, want cancelled/expired", c.Status, c.CancelReason)
	}
}

// TestForwardGetterAppliesLazyExpiry: the Forward getter runs the
// lazy expiry first, so a lapsed contract reads as cancelled (reason
// "expired") even before an explicit ExpireForwards sweep — the
// transition is persisted, exactly as the sweep would persist it.
func TestForwardGetterAppliesLazyExpiry(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-getexp", now.Add(time.Hour)), now)
	lapseAt := now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second)
	// The getter uses the wall clock, which cannot be fast-forwarded —
	// drive its locked core directly at the lapsed instant, which is
	// exactly what the getter does under its write lock.
	l.mu.Lock()
	n := l.expireForwardsLocked(lapseAt)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("lazy expiry = %d, want 1", n)
	}
	c, ok := l.Forward("fwd-getexp")
	if !ok || c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		t.Errorf("getter after lapse: %+v, %v", c, ok)
	}
}

func TestForwardCancel(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-cancel", now.Add(time.Hour)), now)
	entriesBefore := len(l.entries)
	c, err := l.cancelForwardLocked("fwd-cancel", now)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if c.Status != ForwardCancelled || c.CancelReason != ForwardCancelManual {
		t.Errorf("status = %s/%s, want cancelled/manual", c.Status, c.CancelReason)
	}
	if len(l.entries) != entriesBefore {
		t.Error("cancel must record no journal entries")
	}
	// Cancelling twice, or settling after cancel, is rejected.
	if _, err := l.cancelForwardLocked("fwd-cancel", now); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("second cancel: want ErrForwardNotBooked, got %v", err)
	}
	if _, err := l.settleForwardLocked("fwd-cancel", now.Add(2*time.Hour)); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("settle after cancel: want ErrForwardNotBooked, got %v", err)
	}
	// Cancelling a lapsed contract reports expired, not "cancelled".
	mustBookForwardLocked(t, l, forwardBooking("fwd-cancel-lapsed", now.Add(time.Hour)), now)
	lapseAt := now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second)
	if _, err := l.cancelForwardLocked("fwd-cancel-lapsed", lapseAt); !errors.Is(err, ErrForwardExpired) {
		t.Errorf("cancel after lapse: want ErrForwardExpired, got %v", err)
	}
}

func TestForwardCapEnforced(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	b := forwardBooking("fwd-cap", now.Add(time.Hour))
	b.NotionalCapCents = 20000
	mustBookForwardLocked(t, l, b, now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-cap", now.Add(2*time.Hour)); err != nil {
		t.Errorf("settle within cap: %v", err)
	}
	if got, _ := l.Forward("fwd-cap"); got.Status != ForwardSettled {
		t.Errorf("status = %s, want settled", got.Status)
	}
}

func TestForwardSettlementFrozenPayer(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-frozen", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	l.Freeze("payer")
	if _, err := l.settleForwardLocked("fwd-frozen", now.Add(2*time.Hour)); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("want ErrAccountFrozen, got %v", err)
	}
	// The frozen rejection records nothing and the contract stays booked.
	if got, _ := l.Forward("fwd-frozen"); got.Status != ForwardBooked {
		t.Errorf("status = %s, want booked", got.Status)
	}
}

func TestForwardSettlementOverdraft(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-od", now.Add(time.Hour)), now)
	l.EnableOverdraftProtection("payer")
	// No funding: the payer's 0 balance cannot cover the 10000 outflow.
	if _, err := l.settleForwardLocked("fwd-od", now.Add(2*time.Hour)); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("want ErrAccountOverdraft, got %v", err)
	}
	if got, _ := l.Forward("fwd-od"); got.Status != ForwardBooked {
		t.Errorf("status = %s, want booked", got.Status)
	}
}

func TestForwardSettlementReviewRouted(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-review", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if err := l.SetReviewThreshold("payer", 5000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	entriesBefore := len(l.entries)
	if _, err := l.settleForwardLocked("fwd-review", now.Add(2*time.Hour)); !errors.Is(err, ErrForwardReviewRouted) {
		t.Errorf("want ErrForwardReviewRouted, got %v", err)
	}
	if len(l.entries) != entriesBefore {
		t.Error("review-routed settlement must record nothing")
	}
	if got, _ := l.Forward("fwd-review"); got.Status != ForwardBooked {
		t.Errorf("status = %s, want booked", got.Status)
	}
}

func TestForwardAuditEvents(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-audit", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-audit", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.cancelForwardLocked("fwd-audit", now); !errors.Is(err, ErrForwardNotBooked) {
		t.Fatalf("cancel after settle: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-audit-cancel", now.Add(time.Hour)), now)
	if _, err := l.cancelForwardLocked("fwd-audit-cancel", now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	seen := map[string]bool{}
	var transferDetails map[string]any
	for _, ev := range providerAuditEvents(t, al) {
		seen[ev.Op] = true
		if ev.Op == "transfer" && ev.TraceID == "fwd-audit" {
			transferDetails = ev.Details
		}
		if ev.Op == "fx_forward_booked" && ev.TraceID == "fwd-audit" {
			d := ev.Details
			if d["locked_rate"] != "11/10" {
				t.Errorf("fx_forward_booked locked_rate = %v, want 11/10", d["locked_rate"])
			}
			if _, ok := d["rate_snapshot_version"]; !ok {
				t.Error("fx_forward_booked missing rate_snapshot_version")
			}
		}
		if ev.Op == "fx_forward_settled" && ev.TraceID == "fwd-audit" {
			d := ev.Details
			if d["slippage_bps"] != float64(0) {
				t.Errorf("fx_forward_settled slippage_bps = %v, want 0", d["slippage_bps"])
			}
		}
	}
	for _, op := range []string{"fx_forward_booked", "fx_forward_settled", "fx_forward_cancelled", "transfer"} {
		if !seen[op] {
			t.Errorf("missing audit event %q", op)
		}
	}
	if transferDetails == nil {
		t.Fatal("no transfer audit event for the settlement")
	}
	// The settlement transfer's own audit event discloses the locked
	// price three ways, all equal.
	admitted, _ := transferDetails["admitted_rate"].(map[string]any)
	committed, _ := transferDetails["committed_rate"].(map[string]any)
	if admitted["num"] != float64(11) || admitted["den"] != float64(10) {
		t.Errorf("admitted_rate = %v, want 11/10", admitted)
	}
	if committed["num"] != float64(11) || committed["den"] != float64(10) {
		t.Errorf("committed_rate = %v, want 11/10", committed)
	}
	if transferDetails["slippage_bps"] != float64(0) {
		t.Errorf("transfer slippage_bps = %v, want 0", transferDetails["slippage_bps"])
	}
	if transferDetails["forward_id"] != "fwd-audit" {
		t.Errorf("transfer forward_id = %v, want fwd-audit", transferDetails["forward_id"])
	}
}

func TestForwardReconcileBook(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	// One booked (far-future value date), one settled, one manually
	// cancelled, one expired.
	mustBookForwardLocked(t, l, forwardBooking("fwd-r-booked", now.Add(48*time.Hour)), now)
	mustBookForwardLocked(t, l, forwardBooking("fwd-r-settled", now.Add(time.Hour)), now)
	mustBookForwardLocked(t, l, forwardBooking("fwd-r-cancelled", now.Add(time.Hour)), now)
	mustBookForwardLocked(t, l, forwardBooking("fwd-r-expired", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-r-settled", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.cancelForwardLocked("fwd-r-cancelled", now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	lapseAt := now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second)
	if n := l.ExpireForwards(lapseAt); n != 1 {
		t.Fatalf("ExpireForwards = %d, want 1", n)
	}
	rep := l.Reconcile(lapseAt)
	fb := rep.ForwardBook
	if fb.Total != 4 || fb.Booked != 1 || fb.Settled != 1 || fb.Expired != 1 || fb.Cancelled != 1 {
		t.Errorf("forward_book = %+v, want total/booked/settled/expired/cancelled = 4/1/1/1/1", fb)
	}
	if len(fb.ByPair) != 1 || fb.ByPair[0].FromCurrency != "USD" || fb.ByPair[0].ToCurrency != "EUR" ||
		fb.ByPair[0].Booked != 1 || fb.ByPair[0].Settled != 1 {
		t.Errorf("by_pair = %+v, want one USD->EUR row with booked=1 settled=1", fb.ByPair)
	}
}

func TestForwardReconcileLazyExpiry(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-r-lazy", now.Add(time.Hour)), now)
	// No sweep: the report still counts the lapsed contract as expired
	// (lazy expiry, like the hold totals).
	rep := l.Reconcile(now.Add(time.Hour).Add(ForwardSettlementGracePeriod).Add(time.Second))
	if rep.ForwardBook.Expired != 1 || rep.ForwardBook.Booked != 0 {
		t.Errorf("forward_book = %+v, want expired=1 booked=0", rep.ForwardBook)
	}
}

func TestForwardSnapshotRoundTrip(t *testing.T) {
	l := forwardTestLedger()
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-s-booked", now.Add(time.Hour)), now)
	mustBookForwardLocked(t, l, forwardBooking("fwd-s-settled", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-s-settled", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("snapshot round-trip diverged on forwards")
	}
	c, ok := restored.Forward("fwd-s-settled")
	if !ok || c.Status != ForwardSettled || c.SettledTransferID != "fwd-s-settled" {
		t.Errorf("restored settled contract: %+v, %v", c, ok)
	}
	c, ok = restored.Forward("fwd-s-booked")
	if !ok || c.Status != ForwardBooked || c.RateNum != 11 || c.RateDen != 10 {
		t.Errorf("restored booked contract: %+v, %v", c, ok)
	}
	// The restored ledger settles the booked forward at the locked
	// rate too — the agreement survived the round trip.
	if _, _, err := restored.Post(JournalEntry{ID: "seed2", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund restored: %v", err)
	}
	receipt, err := restored.settleForwardLocked("fwd-s-booked", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle on restored ledger: %v", err)
	}
	if receipt.FX == nil || receipt.FX.RateNum != 11 || receipt.FX.RateDen != 10 || receipt.FX.ConvertedCents != 11000 {
		t.Errorf("restored settlement not at locked rate: %+v", receipt.FX)
	}
}
