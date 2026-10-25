package ledger

import (
	"errors"
	"testing"
	"time"
)

func seedFXBreakerLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithFXAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	mustSetRate(t, l, "USD", "EUR", 100, 100)
	return l
}

func postFXBreakerTransfer(t *testing.T, l *Ledger, id string) (TransferReceipt, error) {
	t.Helper()
	return l.PostTransfer(Transfer{
		ID: id, From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
}

func TestFXRateBreaker_SetValidation(t *testing.T) {
	l := New()
	// No live rate to anchor to.
	if _, err := l.SetFXRateBreaker("USD", "EUR", 500); !errors.Is(err, ErrFXRateBreakerNoRate) {
		t.Errorf("no-rate err = %v, want ErrFXRateBreakerNoRate", err)
	}
	mustSetRate(t, l, "USD", "EUR", 100, 100)
	for _, tc := range []struct {
		name     string
		from, to string
		bps      int64
	}{
		{"bad from", "us", "EUR", 500},
		{"bad to", "USD", "EURO", 500},
		{"same currency", "USD", "USD", 500},
		{"zero threshold", "USD", "EUR", 0},
		{"negative threshold", "USD", "EUR", -1},
	} {
		if _, err := l.SetFXRateBreaker(tc.from, tc.to, tc.bps); !errors.Is(err, ErrInvalidFXRateBreaker) {
			t.Errorf("%s: err = %v, want ErrInvalidFXRateBreaker", tc.name, err)
		}
	}
	b, err := l.SetFXRateBreaker("USD", "EUR", 500)
	if err != nil {
		t.Fatalf("SetFXRateBreaker: %v", err)
	}
	if b.AnchorNum != 100 || b.AnchorDen != 100 || b.Tripped {
		t.Errorf("breaker = %+v, want anchor 100/100 untripped", b)
	}
	if _, ok := l.FXRateBreaker("USD", "EUR"); !ok {
		t.Error("breaker not retrievable after set")
	}
	if _, ok := l.FXRateBreaker("USD", "GBP"); ok {
		t.Error("unset pair must report no breaker")
	}
	if _, err := l.ResetFXRateBreaker("USD", "GBP"); !errors.Is(err, ErrFXRateBreakerNotConfigured) {
		t.Errorf("reset unset err = %v, want ErrFXRateBreakerNotConfigured", err)
	}
}

func TestFXRateBreaker_TripsAboveThresholdAndLatches(t *testing.T) {
	l := seedFXBreakerLedger(t)
	if _, err := l.SetFXRateBreaker("USD", "EUR", 500); err != nil {
		t.Fatalf("SetFXRateBreaker: %v", err)
	}
	// 4% move: inside the 5% budget, settles.
	mustSetRate(t, l, "USD", "EUR", 104, 100)
	if _, err := postFXBreakerTransfer(t, l, "fx-br-1"); err != nil {
		t.Fatalf("4%% move transfer: %v", err)
	}
	// Exactly at the 5% threshold: strict-greater, so it still settles.
	mustSetRate(t, l, "USD", "EUR", 105, 100)
	if _, err := postFXBreakerTransfer(t, l, "fx-br-2"); err != nil {
		t.Fatalf("at-threshold transfer: %v", err)
	}
	// 6% move: trips.
	mustSetRate(t, l, "USD", "EUR", 106, 100)
	beforeVersion := l.Version()
	beforeBalance := l.Balance("payer")
	if _, err := postFXBreakerTransfer(t, l, "fx-br-3"); !errors.Is(err, ErrFXRateBreakerTripped) {
		t.Fatalf("over-threshold err = %v, want ErrFXRateBreakerTripped", err)
	}
	if v := l.Version(); v != beforeVersion {
		t.Errorf("version = %d, want %d (tripped settlement must book nothing)", v, beforeVersion)
	}
	if b := l.Balance("payer"); b != beforeBalance {
		t.Errorf("payer balance = %d, want %d", b, beforeBalance)
	}
	b, ok := l.FXRateBreaker("USD", "EUR")
	if !ok || !b.Tripped || b.TrippedRateNum != 106 || b.TrippedRateDen != 100 || b.TrippedDeviationBps != 600 {
		t.Errorf("breaker after trip = %+v, want tripped at 106/100 deviation 600", b)
	}
	// Latching: the rate falls back to the anchor, but the breaker
	// stays tripped until an operator resets it.
	mustSetRate(t, l, "USD", "EUR", 100, 100)
	if _, err := postFXBreakerTransfer(t, l, "fx-br-4"); !errors.Is(err, ErrFXRateBreakerTripped) {
		t.Errorf("latched err = %v, want ErrFXRateBreakerTripped", err)
	}
	// Reset re-anchors to the current rate and clears the trip.
	reset, err := l.ResetFXRateBreaker("USD", "EUR")
	if err != nil {
		t.Fatalf("ResetFXRateBreaker: %v", err)
	}
	if reset.Tripped || reset.AnchorNum != 100 || reset.MaxDeviationBps != 500 {
		t.Errorf("reset = %+v, want untripped anchor 100/100 threshold 500", reset)
	}
	if _, err := postFXBreakerTransfer(t, l, "fx-br-5"); err != nil {
		t.Fatalf("post-reset transfer: %v", err)
	}
}

func TestFXRateBreaker_SetReplacesAndClearsTrip(t *testing.T) {
	l := seedFXBreakerLedger(t)
	if _, err := l.SetFXRateBreaker("USD", "EUR", 500); err != nil {
		t.Fatalf("SetFXRateBreaker: %v", err)
	}
	mustSetRate(t, l, "USD", "EUR", 120, 100)
	if _, err := postFXBreakerTransfer(t, l, "fx-br-x"); !errors.Is(err, ErrFXRateBreakerTripped) {
		t.Fatalf("trip err = %v, want ErrFXRateBreakerTripped", err)
	}
	// Re-setting re-anchors to the moved rate (120) and clears the trip.
	b, err := l.SetFXRateBreaker("USD", "EUR", 500)
	if err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if b.Tripped || b.AnchorNum != 120 {
		t.Errorf("re-set = %+v, want untripped anchor 120/100", b)
	}
	if _, err := postFXBreakerTransfer(t, l, "fx-br-y"); err != nil {
		t.Fatalf("post re-set transfer: %v", err)
	}
}

func TestFXRateBreaker_NoBreakerMeansNoChange(t *testing.T) {
	l := seedFXBreakerLedger(t)
	// No breaker configured: a 50% move settles exactly as before.
	mustSetRate(t, l, "USD", "EUR", 150, 100)
	rcpt, err := postFXBreakerTransfer(t, l, "fx-br-free")
	if err != nil {
		t.Fatalf("no-breaker transfer: %v", err)
	}
	if rcpt.FX == nil || rcpt.FX.ConvertedCents != 15000 {
		t.Errorf("conversion = %+v, want 15000", rcpt.FX)
	}
}

func TestFXRateBreaker_ForwardSettlementExempt(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustSetRate(t, l, "USD", "EUR", 100, 100)
	mustBookForwardLocked(t, l, forwardBooking("fwd-br", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	// Anchor the breaker at 100, then move the live market 100% —
	// a spot settlement would trip, but the forward settles at its
	// locked 110/100 price regardless.
	if _, err := l.SetFXRateBreaker("USD", "EUR", 500); err != nil {
		t.Fatalf("SetFXRateBreaker: %v", err)
	}
	mustSetRate(t, l, "USD", "EUR", 200, 100)
	receipt, err := l.settleForwardLocked("fwd-br", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("forward settle despite live jump: %v", err)
	}
	if receipt.FX == nil || receipt.FX.ConvertedCents != 11000 {
		t.Errorf("forward conversion = %+v, want 11000 at locked rate", receipt.FX)
	}
	// The forward did not trip the breaker, but the live jump still
	// trips the next spot settlement — the guard is live, just not
	// applied to locked contracts.
	if _, err := postFXBreakerTransfer(t, l, "fx-br-after-fwd"); !errors.Is(err, ErrFXRateBreakerTripped) {
		t.Errorf("spot after forward err = %v, want ErrFXRateBreakerTripped", err)
	}
}

func TestFXRateBreaker_DownwardMoveTrips(t *testing.T) {
	l := seedFXBreakerLedger(t)
	if _, err := l.SetFXRateBreaker("USD", "EUR", 500); err != nil {
		t.Fatalf("SetFXRateBreaker: %v", err)
	}
	// A 10% fall is a move, not just a rise.
	mustSetRate(t, l, "USD", "EUR", 90, 100)
	if _, err := postFXBreakerTransfer(t, l, "fx-br-down"); !errors.Is(err, ErrFXRateBreakerTripped) {
		t.Errorf("downward err = %v, want ErrFXRateBreakerTripped", err)
	}
	if b, _ := l.FXRateBreaker("USD", "EUR"); b.TrippedDeviationBps != 1000 {
		t.Errorf("deviation = %d, want 1000", b.TrippedDeviationBps)
	}
}
